package main

import (
	"bytes"
	"flag"
	"go-importer/internal/pkg/db"
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
	re  *regexp.Regexp
}

var tagRuleLine = regexp.MustCompile(`^([a-z0-9_-]{1,40})(\s+@out)?\s*=\s*(.+?)\s*$`)

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
			errs = append(errs, "line "+strconv.Itoa(n+1)+": expected 'tag = regex' (tag: a-z 0-9 _ -)")
			continue
		}
		re, err := regexp.Compile(m[3])
		if err != nil {
			errs = append(errs, "line "+strconv.Itoa(n+1)+": "+err.Error())
			continue
		}
		rules = append(rules, tagRule{tag: m[1], out: m[2] != "", re: re})
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
// Each item is matched as sent and, if it contains JSON escapes, also with them decoded, so a payload
// hidden as "127.0.0.1\u003bcat /flag" is caught by the same rule as "127.0.0.1;cat /flag".
func ApplyTagRules(flow *db.FlowEntry, rules []tagRule) {
	if len(rules) == 0 {
		return
	}
	decoded := make([][]byte, len(flow.Flow))
	for i := range flow.Flow {
		decoded[i] = jsonUnescape(flow.Flow[i].Data) // nil when there is nothing to decode
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
			if r.re.Match(item.Data) || (decoded[i] != nil && r.re.Match(decoded[i])) {
				flow.Tags = append(flow.Tags, r.tag)
				break
			}
		}
	}
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
