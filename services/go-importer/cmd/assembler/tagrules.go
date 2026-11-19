package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"flag"
	"go-importer/internal/pkg/db"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Tag rules: "tag = regex" lines in a text file, edited on the Settings page. Every new flow whose
// client data (or, with "@out", server data) matches a rule gets that tag, like flag-in / flag-out.
// The file is re-read when it changes, so new rules apply without restarting anything.

var tagRulesPath = flag.String("tag-rules", "", "Tag rules file ('tag = regex' per line), default /petal/rules/tag-rules.conf")

type tagRule struct {
	tag string
	out bool // match server -> client data instead of client -> server
	bin bool // match raw bytes: \x90 is the byte 0x90 (data is read as Latin-1, not UTF-8)
	re  *regexp.Regexp
}

var tagRuleLine = regexp.MustCompile(`^([a-z0-9_-]{1,40})((?:\s+@(?:out|bin))*)\s*=\s*(.+?)\s*$`)

// ParseTagRules returns the valid rules and one message per invalid line (those are skipped).
func ParseTagRules(text string) ([]tagRule, []string) {
	var rules []tagRule
	var errs []string
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := tagRuleLine.FindStringSubmatch(line)
		if m == nil {
			errs = append(errs, "line "+strconv.Itoa(n+1)+": expected 'tag [@out] [@bin] = regex' (tag: a-z 0-9 _ -)")
			continue
		}
		re, err := regexp.Compile(m[3])
		if err != nil {
			errs = append(errs, "line "+strconv.Itoa(n+1)+": "+err.Error())
			continue
		}
		rules = append(rules, tagRule{tag: m[1], out: strings.Contains(m[2], "@out"), bin: strings.Contains(m[2], "@bin"), re: re})
	}
	return rules, errs
}

type tagRuleSet struct {
	mu      sync.Mutex
	path    string
	mtime   time.Time
	size    int64
	checked time.Time
	rules   []tagRule
}

var tagRules = &tagRuleSet{}

// current returns the rules, re-reading the file if it changed (checked at most every 5 seconds).
func (s *tagRuleSet) current() []tagRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" || time.Since(s.checked) < 5*time.Second {
		return s.rules
	}
	s.checked = time.Now()
	st, err := os.Stat(s.path)
	if err != nil {
		if s.rules != nil {
			log.Printf("tag rules: %s not readable (%v), no rule tags", s.path, err)
		}
		s.rules, s.mtime, s.size = nil, time.Time{}, 0
		return nil
	}
	if st.ModTime().Equal(s.mtime) && st.Size() == s.size {
		return s.rules
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s.rules
	}
	rules, errs := ParseTagRules(string(data))
	for _, e := range errs {
		log.Printf("tag rules: skipping %s", e)
	}
	log.Printf("tag rules: loaded %d rule(s) from %s", len(rules), s.path)
	s.rules, s.mtime, s.size = rules, st.ModTime(), st.Size()
	return rules
}

// ApplyTagRules adds the tag of every rule that matches the flow (client data, or server data for @out).
// Each item is matched as sent and in decoded forms (see decodedVariants), so a payload that is
// JSON-escaped, URL-encoded, base64-encoded or in a gzip request body hits the same rule as the plain one.
func ApplyTagRules(flow *db.FlowEntry, rules []tagRule) {
	if len(rules) == 0 {
		return
	}
	needIn, needOut := false, false
	for _, r := range rules {
		if r.out {
			needOut = true
		} else {
			needIn = true
		}
	}
	variants := make([][][]byte, len(flow.Flow))
	for i := range flow.Flow {
		if out := flow.Flow[i].From == "s"; (out && needOut) || (!out && needIn) {
			variants[i] = decodedVariants(flow.Flow[i].Data)
		}
	}
	// Latin-1 views (one rune per byte) for @bin rules, built only when needed
	latin := make([][]string, len(flow.Flow))
	latinOf := func(i int) []string {
		if latin[i] == nil {
			views := []string{latin1(flow.Flow[i].Data)}
			for _, v := range variants[i] {
				views = append(views, latin1(v))
			}
			latin[i] = views
		}
		return latin[i]
	}
	for _, r := range rules {
		if contains(flow.Tags, r.tag) {
			continue
		}
		for i := range flow.Flow {
			item := &flow.Flow[i]
			if (item.From == "s") != r.out {
				continue
			}
			if ruleMatches(r, item.Data, variants[i], func() []string { return latinOf(i) }) {
				flow.Tags = append(flow.Tags, r.tag)
				break
			}
		}
	}
}

func ruleMatches(r tagRule, data []byte, variants [][]byte, latinViews func() []string) bool {
	if r.bin {
		for _, v := range latinViews() {
			if r.re.MatchString(v) {
				return true
			}
		}
		return false
	}
	if r.re.Match(data) {
		return true
	}
	for _, v := range variants {
		if r.re.Match(v) {
			return true
		}
	}
	return false
}

// latin1 maps every byte to the rune with the same value, so a regex can match raw binary data.
func latin1(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

const (
	maxVariantBytes = 8 << 20 // total decoded data kept per flow item
	maxScanBytes    = 1 << 20 // base64 search window per item
	maxGunzipBytes  = 4 << 20 // decompression limit (zip bombs)
	maxB64Tokens    = 64
)

// decodedVariants returns decoded copies of data (not data itself): gunzipped request body, JSON
// unescaped, URL decoded (twice, for double encoding), and base64 values that decode to text, each of
// those again JSON/URL decoded. Only variants that differ from their source are kept.
func decodedVariants(data []byte) [][]byte {
	var out [][]byte
	total := 0
	add := func(b []byte) bool {
		if len(b) == 0 || total+len(b) > maxVariantBytes {
			return false
		}
		out = append(out, b)
		total += len(b)
		return true
	}
	textual := func(b []byte) {
		if j := jsonUnescape(b); j != nil {
			add(j)
		}
		if x := hexEscapes(b); x != nil {
			add(x)
		}
		if u := urlDecode(b); u != nil && add(u) {
			if u2 := urlDecode(u); u2 != nil {
				add(u2)
			}
		}
	}

	sources := [][]byte{data}
	for _, body := range httpBodies(data) {
		if add(body) {
			sources = append(sources, body)
		}
	}
	for _, src := range sources {
		textual(src)
	}
	scan := append([][]byte{}, sources...)
	scan = append(scan, out...)
	for _, b := range scan {
		for _, dec := range base64Texts(b) {
			if add(dec) {
				textual(dec)
			}
		}
		for _, dec := range hexTexts(b) {
			if add(dec) {
				textual(dec)
			}
		}
	}
	return out
}

// jsonUnescape decodes JSON string escapes (\uXXXX incl. surrogate pairs, \n \t \r \b \f \/ \" \\)
// anywhere in data. Returns nil if data has no escape sequences, so callers can skip the second match.
// Invalid escapes are kept as they are.
func jsonUnescape(data []byte) []byte {
	if bytes.IndexByte(data, '\\') < 0 {
		return nil
	}
	out := make([]byte, 0, len(data))
	changed := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c != '\\' || i+1 >= len(data) {
			out = append(out, c)
			continue
		}
		switch e := data[i+1]; e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'u':
			r, n := decodeUnicodeEscape(data[i:])
			if n == 0 {
				out = append(out, c)
				continue
			}
			out = utf8.AppendRune(out, r)
			i += n - 1
			changed = true
			continue
		default:
			out = append(out, c)
			continue
		}
		i++
		changed = true
	}
	if !changed {
		return nil
	}
	return out
}

// decodeUnicodeEscape reads "\uXXXX" (and a following low surrogate "\uXXXX") at the start of b.
// Returns the rune and the number of bytes used, or 0 if b does not start with a valid escape.
func decodeUnicodeEscape(b []byte) (rune, int) {
	hex4 := func(b []byte) (rune, bool) {
		if len(b) < 6 || b[0] != '\\' || b[1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(string(b[2:6]), 16, 32)
		return rune(v), err == nil
	}
	r, ok := hex4(b)
	if !ok {
		return 0, 0
	}
	if utf16.IsSurrogate(r) {
		if r2, ok := hex4(b[6:]); ok {
			if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
				return dec, 12
			}
		}
		return utf8.RuneError, 6
	}
	return r, 6
}

// urlDecode percent-decodes data ("+" becomes a space). Returns nil if nothing was decoded.
// Invalid sequences are kept as they are.
func urlDecode(data []byte) []byte {
	if bytes.IndexByte(data, '%') < 0 && bytes.IndexByte(data, '+') < 0 {
		return nil
	}
	out := make([]byte, 0, len(data))
	changed := false
	for i := 0; i < len(data); i++ {
		switch c := data[i]; {
		case c == '%' && i+2 < len(data) && isHex(data[i+1]) && isHex(data[i+2]):
			out = append(out, unhex(data[i+1])<<4|unhex(data[i+2]))
			i += 2
			changed = true
		case c == '+':
			out = append(out, ' ')
			changed = true
		default:
			out = append(out, c)
		}
	}
	if !changed {
		return nil
	}
	return out
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

var base64Token = regexp.MustCompile(`[A-Za-z0-9+/_-]{8,}={0,2}`)

// base64Texts finds base64 / base64url values in data and returns those that decode to readable text
// (random IDs and binary blobs are dropped, so they cannot cause false matches).
func base64Texts(data []byte) [][]byte {
	if len(data) > maxScanBytes {
		data = data[:maxScanBytes]
	}
	var out [][]byte
	for _, tok := range base64Token.FindAll(data, maxB64Tokens) {
		raw := bytes.TrimRight(tok, "=")
		var dec []byte
		var err error
		if bytes.ContainsAny(raw, "-_") {
			dec, err = base64.RawURLEncoding.DecodeString(string(raw))
		} else {
			dec, err = base64.RawStdEncoding.DecodeString(string(raw))
		}
		if err == nil && len(dec) >= 5 && mostlyText(dec) {
			out = append(out, dec)
		}
	}
	return out
}

func mostlyText(b []byte) bool {
	printable := 0
	for _, c := range b {
		if (c >= 0x20 && c < 0x7f) || c == '\n' || c == '\r' || c == '\t' {
			printable++
		}
	}
	return printable*10 >= len(b)*9
}

// httpBodies returns decoded forms of an HTTP message body: de-chunked (Transfer-Encoding: chunked,
// which can also be used to split a payload across chunks) and decompressed (Content-Encoding gzip /
// deflate, or a body starting with the gzip magic). Returns nil when there is nothing to decode.
func httpBodies(data []byte) [][]byte {
	i := bytes.Index(data, []byte("\r\n\r\n"))
	if i < 0 || len(data) < i+6 {
		return nil
	}
	head, body := bytes.ToLower(data[:i]), data[i+4:]
	var out [][]byte
	if bytes.Contains(head, []byte("transfer-encoding: chunked")) {
		if d := dechunk(body); d != nil {
			out = append(out, d)
			body = d
		}
	}
	if len(body) < 2 {
		return out
	}
	var r io.Reader
	switch {
	case bytes.Contains(head, []byte("content-encoding: gzip")) || (body[0] == 0x1f && body[1] == 0x8b):
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return out
		}
		r = gz
	case bytes.Contains(head, []byte("content-encoding: deflate")):
		if z, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
			r = z
		} else {
			r = flate.NewReader(bytes.NewReader(body))
		}
	default:
		return out
	}
	dec, _ := io.ReadAll(io.LimitReader(r, maxGunzipBytes)) // a truncated stream still gives its data
	if len(dec) > 0 {
		out = append(out, dec)
	}
	return out
}

// dechunk joins the chunks of a chunked HTTP body ("<hex size>[;ext]\r\n<data>\r\n" ... "0\r\n").
// Returns nil if the body is not valid chunked encoding.
func dechunk(body []byte) []byte {
	var out []byte
	for len(body) > 0 {
		nl := bytes.Index(body, []byte("\r\n"))
		if nl < 0 {
			return nil
		}
		sizeField := bytes.TrimSpace(bytes.SplitN(body[:nl], []byte(";"), 2)[0])
		size, err := strconv.ParseUint(string(sizeField), 16, 32)
		if err != nil {
			return nil
		}
		body = body[nl+2:]
		if size == 0 {
			return out
		}
		if uint64(len(body)) < size {
			return append(out, body...) // capture cut mid-chunk: keep what we have
		}
		out = append(out, body[:size]...)
		body = bytes.TrimPrefix(body[size:], []byte("\r\n"))
		if len(out) > maxGunzipBytes {
			return out
		}
	}
	return out
}

var hexToken = regexp.MustCompile(`(?:[0-9a-fA-F]{2}){8,}`)

// hexTexts decodes long hex strings (e.g. ?x=2e2e2f2e2e2f657463) that turn out to be readable text.
func hexTexts(data []byte) [][]byte {
	if len(data) > maxScanBytes {
		data = data[:maxScanBytes]
	}
	var out [][]byte
	for _, tok := range hexToken.FindAll(data, maxB64Tokens) {
		dec := make([]byte, len(tok)/2)
		for i := range dec {
			dec[i] = unhex(tok[2*i])<<4 | unhex(tok[2*i+1])
		}
		if mostlyText(dec) {
			out = append(out, dec)
		}
	}
	return out
}

// hexEscapes decodes "\xNN" escapes written as text (e.g. a Python payload "\x90\x90\x31\xc0") into the
// raw bytes, so @bin rules see the real shellcode. Needs at least 4 escapes; nil otherwise.
func hexEscapes(data []byte) []byte {
	if bytes.Count(data, []byte("\\x")) < 4 {
		return nil
	}
	out := make([]byte, 0, len(data))
	n := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\\' && i+3 < len(data) && (data[i+1] == 'x' || data[i+1] == 'X') && isHex(data[i+2]) && isHex(data[i+3]) {
			out = append(out, unhex(data[i+2])<<4|unhex(data[i+3]))
			i += 3
			n++
			continue
		}
		out = append(out, data[i])
	}
	if n < 4 {
		return nil
	}
	return out
}
