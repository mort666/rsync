package sender

import (
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

type filterRuleList struct {
	Filters []*filterRule
}

// exclude.c:add_rule
func (l *filterRuleList) addRule(fr *filterRule) {
	if strings.HasSuffix(fr.pattern, "/") {
		fr.flag |= filtruleDirectory
		fr.pattern = strings.TrimSuffix(fr.pattern, "/")
	}
	if strings.ContainsFunc(fr.pattern, func(r rune) bool {
		return r == '*' || r == '[' || r == '?'
	}) {
		fr.flag |= filtruleWild
	}
	l.Filters = append(l.Filters, fr)
}

// exclude.c:check_filter: the first matching rule decides, and an
// include rule means the name goes.
func (l *filterRuleList) matches(name string, dir bool) bool {
	for _, fr := range l.Filters {
		if fr.flag&filtruleReceiverOnly != 0 {
			continue // a rule for the receiver side (e.g. protect)
		}
		if fr.matches(name, dir) {
			return fr.flag&filtruleInclude == 0
		}
	}
	return false
}

// Excluded reports whether name is protected from deletion by the first
// matching rule (i.e. the rule is an exclude). Used by the receiver when
// walking the destination tree under --delete: paths matched by an exclude
// rule must be preserved. An include rule that matches first means the path
// is eligible for deletion per the normal source-fileList check.
func (l *filterRuleList) Excluded(name string, dir bool) bool {
	for _, fr := range l.Filters {
		if fr.flag&filtruleSenderOnly != 0 {
			continue // a rule for the sender side (e.g. hide)
		}
		if !fr.matches(name, dir) {
			continue
		}
		return fr.flag&filtruleInclude == 0
	}
	return false
}

// exclude.c:recv_filter_list
func RecvFilterList(c *rsyncwire.Conn) (*filterRuleList, error) {
	proto := c.Session.Protocol
	var l filterRuleList
	const exclusionListEnd = 0
	const bigpathbuflen = 4096 + 1024 // matches rsync.h
	for {
		length, err := c.ReadInt32()
		if err != nil {
			return nil, err
		}
		if length == exclusionListEnd {
			break
		}
		if length < 0 || length >= bigpathbuflen {
			return nil, fmt.Errorf("overflow: filter rule length=%d", length)
		}
		line := make([]byte, length)
		if _, err := io.ReadFull(c.Reader, line); err != nil {
			return nil, err
		}
		fr, err := parseFilterProto(proto, string(line))
		if err != nil {
			return nil, err
		}
		// Upstream rejects wildcard rules here because its matcher cannot
		// handle them; this fork's matches() does (path.Match), and clients
		// rely on "- *" and "/name/***" for surgical --delete.
		l.addRule(fr)
	}
	return &l, nil
}

const (
	filtruleInclude = 1 << iota
	filtruleClearList
	filtruleDirectory
	filtruleWild
	filtruleNegate       // '!': the rule applies where the pattern does not match
	filtruleSenderOnly   // 's' without 'r'
	filtruleReceiverOnly // 'r' without 's'
)

type filterRule struct {
	flag    int
	pattern string
}

// exclude.c:rule_matches. A rule ending in "/" is for folders: it never
// matches anything else (and a negated one always does).
func (fr *filterRule) matches(name string, dir bool) bool {
	if fr.flag&filtruleDirectory != 0 && !dir {
		return fr.flag&filtruleNegate != 0
	}
	pat := fr.pattern
	anchored := strings.HasPrefix(pat, "/")
	if anchored {
		pat = strings.TrimPrefix(pat, "/")
	}
	candidate := name
	if !anchored && !strings.ContainsRune(pat, '/') {
		candidate = filepath.Base(name)
	}
	var ok bool
	if fr.flag&filtruleWild != 0 {
		ok, _ = path.Match(pat, candidate)
	} else {
		ok = pat == candidate
	}
	return ok != (fr.flag&filtruleNegate != 0)
}

// ruleModifiers are the letters rsync may put between a rule's "-" or "+"
// and the space from protocol 29 (exclude.c:get_rule_prefix).
const ruleModifiers = "/!Cnw+-exsrp"

// parseFilterProto parses one rule as a client sent it. Below protocol 29
// rsync sends old-style rules (XFLG_OLD_PREFIXES): "- pattern",
// "+ pattern" or a bare pattern to exclude. From 29 each rule is "-" or "+",
// then modifiers, then a space and the pattern: "- foo", "-/ foo",
// "-! foo", "+s foo".
func parseFilterProto(proto int32, line string) (*filterRule, error) {
	if proto < 29 || line == "" || (line[0] != '-' && line[0] != '+') {
		return parseFilter(line)
	}
	sp := strings.IndexByte(line, ' ')
	if sp < 1 || strings.Trim(line[1:sp], ruleModifiers) != "" {
		return parseFilter(line)
	}
	rule := new(filterRule)
	if line[0] == '+' {
		rule.flag |= filtruleInclude
	}
	mods := line[1:sp]
	if strings.Contains(mods, "!") {
		rule.flag |= filtruleNegate
	}
	sender, receiver := strings.Contains(mods, "s"), strings.Contains(mods, "r")
	if sender && !receiver {
		rule.flag |= filtruleSenderOnly
	}
	if receiver && !sender {
		rule.flag |= filtruleReceiverOnly
	}
	// '/' (match the absolute path), 'p' (perishable) and the rest only
	// matter for features gokrazy/rsync does not have.
	rule.pattern = line[sp+1:]
	return rule, nil
}

// exclude.c:parse_filter_str / exclude.c:parse_rule_tok
func parseFilter(line string) (*filterRule, error) {
	rule := new(filterRule)

	// We only support what rsync calls XFLG_OLD_PREFIXES
	if strings.HasPrefix(line, "- ") {
		// clear include flag
		rule.flag &= ^filtruleInclude
		line = strings.TrimPrefix(line, "- ")
	} else if strings.HasPrefix(line, "+ ") {
		// set include flag
		rule.flag |= filtruleInclude
		line = strings.TrimPrefix(line, "+ ")
	} else if strings.HasPrefix(line, "!") {
		// set clear_list flag
		rule.flag |= filtruleClearList
	}

	rule.pattern = line

	return rule, nil
}

// ReceiverRules is what a client's own rules protect from --delete when
// it receives: every rule but sender-side ones, or with --delete-excluded
// only receiver-side ones (exclude.c:send_rules, which keeps those
// local).
func ReceiverRules(rules []string, deleteExcluded bool) func(name string, dir bool) bool {
	var l filterRuleList
	for _, r := range rules {
		fr, err := parseFilterProto(30, r)
		if err != nil || deleteExcluded && fr.flag&filtruleReceiverOnly == 0 {
			continue
		}
		l.addRule(fr)
	}
	return l.Excluded
}

// RulesForReceiver are the rules a sending client gives the receiver:
// all of them, or with --delete-excluded only receiver-side ones, so that
// it deletes what is excluded (exclude.c:send_rules).
func RulesForReceiver(rules []string, deleteExcluded bool) []string {
	if !deleteExcluded {
		return rules
	}
	var out []string
	for _, r := range rules {
		if fr, err := parseFilterProto(30, r); err == nil && fr.flag&filtruleReceiverOnly != 0 {
			out = append(out, r)
		}
	}
	return out
}

// SenderRules are a sending client's own rules, for what it leaves out of
// the file list (receiver-side ones are skipped there).
func SenderRules(rules []string) *filterRuleList {
	var l filterRuleList
	for _, r := range rules {
		if fr, err := parseFilterProto(30, r); err == nil {
			l.addRule(fr)
		}
	}
	return &l
}
