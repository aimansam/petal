package main

import (
	"flag"
	"go-importer/internal/pkg/db"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
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
func ApplyTagRules(flow *db.FlowEntry, rules []tagRule) {
	for _, r := range rules {
		if contains(flow.Tags, r.tag) {
			continue
		}
		for i := range flow.Flow {
			item := &flow.Flow[i]
			if (item.From == "s") != r.out {
				continue
			}
			if r.re.Match(item.Data) {
				flow.Tags = append(flow.Tags, r.tag)
				break
			}
		}
	}
}
