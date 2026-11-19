package main

import (
	"go-importer/internal/pkg/db"
	"os"
	"path/filepath"
	"sort"
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
