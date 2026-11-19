package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"go-importer/internal/pkg/db"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func flowWith(client, server string) *db.FlowEntry {
	return &db.FlowEntry{Flow: []db.FlowItem{
		{From: "c", Data: []byte(client)},
		{From: "s", Data: []byte(server)},
	}}
}

func TestParseTagRules(t *testing.T) {
	rules, errs := ParseTagRules("# comment\n\nsqli = (?i)union\\s+select\nleak @out = root:x:0:0\nBad Tag = x\nbroken = (\nnoequals\n")
	if len(rules) != 2 || rules[0].tag != "sqli" || rules[0].out || rules[1].tag != "leak" || !rules[1].out {
		t.Fatalf("unexpected rules: %+v", rules)
	}
	if len(errs) != 3 {
		t.Fatalf("want 3 errors (bad tag, bad regex, no '='), got %v", errs)
	}
}

func TestDefaultRules(t *testing.T) {
	data, err := os.ReadFile("../../../../rules/tag-rules.default.conf")
	if err != nil {
		t.Skip("default rules file not found:", err)
	}
	rules, errs := ParseTagRules(string(data))
	if len(errs) > 0 {
		t.Fatalf("default rules have errors: %v", errs)
	}
	cases := map[string][]string{
		"GET /search?q=1'+UNION+SELECT+password+FROM+users-- HTTP/1.1":   {"sqli"},
		"POST /login\r\n\r\nuser=admin' or '1'='1":                       {"sqli"},
		"GET /static/../../../../etc/passwd HTTP/1.1":                    {"path-traversal"},
		"GET /file?name=..%2f..%2fflag HTTP/1.1":                         {"path-traversal"},
		"POST /ping\r\n\r\nhost=127.0.0.1;cat /flag":                     {"cmd-injection"},
		"GET /hello?name={{7*7}} HTTP/1.1":                               {"template-injection"},
		"POST /ping\r\n\r\nhost=127.0.0.1%3Bcat%20%2Fflag":               {"cmd-injection"},
		"GET /run?c=x%26%26id HTTP/1.1":                                  {"cmd-injection"},
		"GET /hello?name=%7B%7B7*7%7D%7D HTTP/1.1":                       {"template-injection"},
		"GET /?q=%3Cscript%3Ealert(1)%3C/script%3E HTTP/1.1":             {"xss"},
		"GET /?q=<script>alert(1)</script> HTTP/1.1":                     {"xss"},
		"GET /recipes?page=2&sort=name HTTP/1.1\r\nHost: recipe\r\n\r\n": {},
	}
	for req, want := range cases {
		f := flowWith(req, "HTTP/1.1 200 OK\r\n\r\nok")
		ApplyTagRules(f, rules)
		sort.Strings(f.Tags)
		if len(f.Tags) != len(want) || (len(want) > 0 && f.Tags[0] != want[0]) {
			t.Errorf("%q: got tags %v, want %v", req, f.Tags, want)
		}
	}
	f := flowWith("GET /x HTTP/1.1", "HTTP/1.1 200 OK\r\n\r\nroot:x:0:0:root:/root:/bin/bash")
	ApplyTagRules(f, rules)
	if len(f.Tags) != 1 || f.Tags[0] != "leak-passwd" {
		t.Errorf("@out rule: got %v", f.Tags)
	}
}

func TestDirectionAndNoDuplicates(t *testing.T) {
	rules, _ := ParseTagRules("a = secret\nb @out = secret")
	f := flowWith("secret secret", "nothing")
	f.Tags = []string{"a"}
	ApplyTagRules(f, rules)
	if len(f.Tags) != 1 {
		t.Errorf("client-only match must not add the @out tag or duplicate: %v", f.Tags)
	}
}

func TestReloadOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.conf")
	os.WriteFile(path, []byte("one = x\n"), 0o644)
	s := &tagRuleSet{path: path}
	if r := s.current(); len(r) != 1 || r[0].tag != "one" {
		t.Fatalf("first load: %+v", r)
	}
	os.WriteFile(path, []byte("one = x\ntwo = y\n"), 0o644)
	if len(s.current()) != 1 {
		t.Fatal("should not re-read within 5 seconds")
	}
	s.checked = time.Now().Add(-6 * time.Second)
	if r := s.current(); len(r) != 2 {
		t.Fatalf("after change: %+v", r)
	}
	os.Remove(path)
	s.checked = time.Time{}
	if r := s.current(); r != nil {
		t.Fatalf("missing file must give no rules: %+v", r)
	}
}

func TestJSONUnescape(t *testing.T) {
	cases := map[string]string{
		`{"host":"127.0.0.1\u003bcat /flag"}`: `{"host":"127.0.0.1;cat /flag"}`,
		`{"q":"1 \u0055NION SELECT"}`:         `{"q":"1 UNION SELECT"}`,
		`a\"b\\c\/d\ne\tf`:                    "a\"b\\c/d\ne\tf",
		`\ud83d\ude00 smile`:                  "\U0001F600 smile",
		`bad \u00zz and \q stay`:              "",
		`no escapes here`:                     "",
		`trailing \`:                          "",
	}
	for in, want := range cases {
		got := jsonUnescape([]byte(in))
		if want == "" {
			if got != nil {
				t.Errorf("%q: want nil (nothing decoded), got %q", in, got)
			}
			continue
		}
		if string(got) != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestRulesMatchJSONEscapedPayloads(t *testing.T) {
	data, err := os.ReadFile("../../../../rules/tag-rules.default.conf")
	if err != nil {
		t.Skip("default rules file not found:", err)
	}
	rules, _ := ParseTagRules(string(data))
	cases := map[string]string{
		`POST /ping HTTP/1.1` + "\r\n\r\n" + `{"host":"127.0.0.1\u003bcat /flag"}`:  "cmd-injection",
		`POST /api HTTP/1.1` + "\r\n\r\n" + `{"q":"1\u0027 \u0055NION SELECT 1"}`:   "sqli",
		`POST /file HTTP/1.1` + "\r\n\r\n" + `{"path":"..\/..\/etc\/passwd"}`:       "path-traversal",
		`POST /hi HTTP/1.1` + "\r\n\r\n" + `{"name":"\u007b\u007b7*7\u007d\u007d"}`: "template-injection",
	}
	for req, want := range cases {
		f := flowWith(req, "HTTP/1.1 200 OK")
		ApplyTagRules(f, rules)
		if !contains(f.Tags, want) {
			t.Errorf("%q: got %v, want %s", req, f.Tags, want)
		}
	}
}

func TestURLDecode(t *testing.T) {
	if got := string(urlDecode([]byte("a%3Bb+c%2fd"))); got != "a;b c/d" {
		t.Errorf("got %q", got)
	}
	if urlDecode([]byte("100%zz")) != nil || urlDecode([]byte("plain")) != nil {
		t.Error("nothing decodable must give nil")
	}
}

func TestBase64Texts(t *testing.T) {
	payload := "127.0.0.1;cat /flag"
	data := []byte("GET /run?data=" + base64.StdEncoding.EncodeToString([]byte(payload)) + " HTTP/1.1")
	got := base64Texts(data)
	if len(got) != 1 || string(got[0]) != payload {
		t.Fatalf("got %q", got)
	}
	bin := make([]byte, 30)
	for i := range bin {
		bin[i] = byte(i * 37)
	}
	if got := base64Texts([]byte("id=" + base64.StdEncoding.EncodeToString(bin))); len(got) != 0 {
		t.Errorf("binary must be dropped, got %q", got)
	}
}

func gzipped(s string) string {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.String()
}

func TestRulesMatchEncodedPayloads(t *testing.T) {
	data, err := os.ReadFile("../../../../rules/tag-rules.default.conf")
	if err != nil {
		t.Skip("default rules file not found:", err)
	}
	rules, _ := ParseTagRules(string(data))
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	b64url := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	cases := map[string]string{
		"GET /run?data=" + b64("127.0.0.1;cat /flag") + " HTTP/1.1":                                    "cmd-injection",
		"POST /api HTTP/1.1\r\n\r\n{\"p\":\"" + b64url("../../../etc/passwd") + "\"}":                  "path-traversal",
		"GET /f?x=%252e%252e%252f%252e%252e%252fetc%252fpasswd HTTP/1.1":                               "path-traversal",
		"POST /q HTTP/1.1\r\nContent-Encoding: gzip\r\n\r\n" + gzipped("q=1 UNION SELECT flag FROM t"): "sqli",
		"POST /q HTTP/1.1\r\n\r\nq=" + url64(b64("{{7*7}}")):                                           "template-injection",
	}
	for req, want := range cases {
		f := flowWith(req, "HTTP/1.1 200 OK")
		ApplyTagRules(f, rules)
		if !contains(f.Tags, want) {
			t.Errorf("%q: got %v, want %s", req, f.Tags, want)
		}
	}
	clean := []string{
		"GET /static/app.3f9a8b7c6d5e4f3a2b1c0d9e.js HTTP/1.1\r\nHost: x\r\n\r\n",
		"GET /api HTTP/1.1\r\nAuthorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZWFtMSJ9.c2lnbmF0dXJlX2hlcmU\r\n\r\n",
		"GET /x?id=" + b64("hello, this is a normal value") + " HTTP/1.1",
		"GET /x?token=Zx81Qp0LmNvT2bW7yR4kA9sD HTTP/1.1",
	}
	for _, req := range clean {
		f := flowWith(req, "HTTP/1.1 200 OK")
		ApplyTagRules(f, rules)
		if len(f.Tags) != 0 {
			t.Errorf("false positive on %q: %v", req, f.Tags)
		}
	}
}

// url64 percent-encodes "=" and "+" like a browser form would, to also exercise URL-then-base64 decoding.
func url64(s string) string {
	return strings.NewReplacer("=", "%3D", "+", "%2B", "/", "%2F").Replace(s)
}

func rep(b byte, n int) string { return string(bytes.Repeat([]byte{b}, n)) }

func le64(v uint64) string {
	b := make([]byte, 8)
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	return string(b)
}

func TestBinaryRules(t *testing.T) {
	data, err := os.ReadFile("../../../../rules/tag-rules.default.conf")
	if err != nil {
		t.Skip("default rules file not found:", err)
	}
	rules, errs := ParseTagRules(string(data))
	if len(errs) > 0 {
		t.Fatalf("default rules: %v", errs)
	}
	shell64 := string([]byte{0x48, 0x31, 0xf6, 0x56, 0x48, 0xbf, 0x2f, 0x62, 0x69, 0x6e, 0x2f, 0x2f, 0x73, 0x68, 0x57, 0x54, 0x5f, 0x6a, 0x3b, 0x58, 0x99, 0x0f, 0x05})
	bs := string(rune(92))
	escaped := strings.Repeat(bs+"x90", 20) // "\x90\x90..." written as text, e.g. in a python exploit
	rop := "name\n" + rep('A', 40) + le64(0x401236) + le64(0x7ffff7a4f2c3) + le64(0x7ffff7b97e9a) + le64(0x7ffff7a33440) + le64(0x7ffff7a52390)
	cases := map[string][]string{
		rep(0x90, 32) + shell64 + "\n":             {"nop-sled", "shellcode"},
		"payload = b'" + escaped + "'\n":           {"nop-sled"},
		"%p.%p.%p.%p.%p\n":                         {"format-string"},
		"AAAA%7$n\n":                               {"format-string"},
		rep('A', 200) + "\n":                       {"overflow-pattern"},
		"aaaabaaacaaadaaaeaaafaaagaaa\n":           {"overflow-pattern"},
		rop:                                        {"rop-chain"},
		"LOGIN team7 hunter2\nGET flag 13\nQUIT\n": {},
		"GET /search?q=hello%20see%20the%20sun%20set%20x%20now HTTP/1.1\r\n":              {},
		"printf leak %08x.%08x.%016lx.%p\n":                                               {"format-string"},
		string([]byte{0x00, 0x01, 0x02, 0x10, 0x7f, 0xff, 0x00, 0x00, 0x13, 0x37}) + "\n": {},
	}
	for in, want := range cases {
		f := flowWith(in, "OK\n")
		ApplyTagRules(f, rules)
		for _, w := range want {
			if !contains(f.Tags, w) {
				t.Errorf("%q: got %v, want %s", in, f.Tags, w)
			}
		}
		if len(want) == 0 && len(f.Tags) != 0 {
			t.Errorf("false positive on %q: %v", in, f.Tags)
		}
	}
}

func TestBinRuleIsBytewise(t *testing.T) {
	rules, errs := ParseTagRules("hi @bin = " + string(rune(92)) + "xff" + string(rune(92)) + "xfe\nboth @out @bin = " + string(rune(92)) + "x00{4}")
	if len(errs) != 0 || len(rules) != 2 || !rules[0].bin || rules[0].out || !rules[1].bin || !rules[1].out {
		t.Fatalf("parse: %+v %v", rules, errs)
	}
	f := flowWith(string([]byte{'x', 0xff, 0xfe, 'y'}), string([]byte{1, 0, 0, 0, 0, 2}))
	ApplyTagRules(f, rules)
	if !contains(f.Tags, "hi") || !contains(f.Tags, "both") {
		t.Errorf("got %v", f.Tags)
	}
	// without @bin, invalid UTF-8 bytes never match \xff (Go reads the data as UTF-8)
	plain, _ := ParseTagRules("hi = " + string(rune(92)) + "xff")
	g := flowWith(string([]byte{0xff}), "")
	ApplyTagRules(g, plain)
	if len(g.Tags) != 0 {
		t.Errorf("plain rule unexpectedly matched raw byte: %v", g.Tags)
	}
}

func TestChunkedAndHex(t *testing.T) {
	data, _ := os.ReadFile("../../../../rules/tag-rules.default.conf")
	rules, _ := ParseTagRules(string(data))
	chunked := "POST /q HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nq=1 \r\n3\r\nUNI\r\n9\r\nON SELECT\r\n5\r\n flag\r\n0\r\n\r\n"
	gz := gzipped("q=1 UNION SELECT flag")
	chunkedGzip := "POST /q HTTP/1.1\r\nTransfer-Encoding: chunked\r\nContent-Encoding: gzip\r\n\r\n" +
		strconv.FormatInt(int64(len(gz[:10])), 16) + "\r\n" + gz[:10] + "\r\n" +
		strconv.FormatInt(int64(len(gz[10:])), 16) + "\r\n" + gz[10:] + "\r\n0\r\n\r\n"
	hexed := "GET /f?x=2e2e2f2e2e2f2e2e2f6574632f706173737764 HTTP/1.1\r\n\r\n"
	for req, want := range map[string]string{chunked: "sqli", chunkedGzip: "sqli", hexed: "path-traversal"} {
		f := flowWith(req, "HTTP/1.1 200 OK")
		ApplyTagRules(f, rules)
		if !contains(f.Tags, want) {
			t.Errorf("%q: got %v, want %s", req, f.Tags, want)
		}
	}
	if d := dechunk([]byte("zz\r\nnope")); d != nil {
		t.Errorf("invalid chunked body must give nil, got %q", d)
	}
}
