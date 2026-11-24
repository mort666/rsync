package sender

import (
	"io"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

func buildList(t *testing.T, rules ...string) *filterRuleList {
	t.Helper()
	var l filterRuleList
	for _, r := range rules {
		fr, err := parseFilter(r)
		if err != nil {
			t.Fatalf("parseFilter(%q): %v", r, err)
		}
		l.addRule(fr)
	}
	return &l
}

func TestExcludedSurgicalFileDelete(t *testing.T) {
	l := buildList(t, "+ /foo", "- *")
	cases := []struct {
		name string
		want bool
	}{
		{"foo", false},
		{"bar", true},
		{"baz", true},
		{"subdir/foo", true},
	}
	for _, c := range cases {
		if got := l.Excluded(c.name, false); got != c.want {
			t.Errorf("Excluded(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestExcludedSurgicalDirDelete(t *testing.T) {
	l := buildList(t, "+ /foo", "+ /foo/***", "- *")
	cases := []struct {
		name string
		want bool
	}{
		{"foo", false},
		{"foo/inner", false},
		{"bar", true},
		{"bar/inner", true},
	}
	for _, c := range cases {
		if got := l.Excluded(c.name, false); got != c.want {
			t.Errorf("Excluded(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRecvFilterListAcceptsWildcards reads a wire-encoded filter list with the
// rules a surgical --delete sends. Upstream's RecvFilterList rejects wildcard
// rules ("wildcard filter rules not yet implemented"); this fork matches them,
// so that rejection must not come back with an upstream merge.
func TestRecvFilterListAcceptsWildcards(t *testing.T) {
	var wire rsyncwire.Buffer
	for _, rule := range []string{"+ /foo", "+ /foo/***", "- *"} {
		wire.WriteInt32(int32(len(rule)))
		wire.WriteString(rule)
	}
	wire.WriteInt32(0) // end of list
	l, err := RecvFilterList(&rsyncwire.Conn{
		Reader: io.NopCloser(strings.NewReader(wire.String())),
		Writer: nopClose(io.Discard),
	})
	if err != nil {
		t.Fatalf("RecvFilterList: %v", err)
	}
	cases := []struct {
		name string
		want bool
	}{
		{"foo", false},
		{"foo/inner", false},
		{"bar", true},
		{"bar/inner", true},
	}
	for _, c := range cases {
		if got := l.Excluded(c.name, false); got != c.want {
			t.Errorf("Excluded(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestExcludedEmptyList(t *testing.T) {
	l := buildList(t)
	if l.Excluded("anything", false) {
		t.Errorf("Excluded on empty rule list should be false")
	}
}

func TestMatchesAnchored(t *testing.T) {
	fr, _ := parseFilter("- /foo")
	var l filterRuleList
	l.addRule(fr)
	if !fr.matches("foo", false) {
		t.Errorf("/foo should match top-level foo")
	}
	if fr.matches("sub/foo", false) {
		t.Errorf("/foo should NOT match nested sub/foo (anchored)")
	}
	if fr.matches("foo/sub", false) {
		t.Errorf("/foo should NOT match foo/sub (anchored, no trailing slash)")
	}
}

func TestMatchesUnanchored(t *testing.T) {
	fr, _ := parseFilter("- foo")
	var l filterRuleList
	l.addRule(fr)
	if !fr.matches("foo", false) {
		t.Errorf("foo should match foo")
	}
	if !fr.matches("a/b/foo", false) {
		t.Errorf("unanchored foo should match at any depth (basename)")
	}
	if fr.matches("foobar", false) {
		t.Errorf("unanchored foo should not match foobar")
	}
}

func TestMatchesWildcardStar(t *testing.T) {
	fr, _ := parseFilter("- *")
	var l filterRuleList
	l.addRule(fr)
	if !fr.matches("anything", false) {
		t.Errorf("* should match anything")
	}
	if !fr.matches("a/b/c", false) {
		t.Errorf("* should match basename of nested path")
	}
}

// From protocol 29 rules carry modifiers between the "-" or "+" and the
// space; below it they are old-style.
func TestParseFilterProto(t *testing.T) {
	for _, tt := range []struct {
		proto     int32
		line      string
		pattern   string
		flags     int
		excludes  string // a name the rule excludes on the receiver, "" for none
		protected bool
	}{
		{27, "- foo", "foo", 0, "foo", true},
		{27, "-r foo", "-r foo", 0, "", false}, // old-style: the whole line is a pattern
		{29, "- foo", "foo", 0, "foo", true},
		{29, "+ foo", "foo", filtruleInclude, "foo", false},
		{29, "-r foo", "foo", filtruleReceiverOnly, "foo", true},
		{29, "-s foo", "foo", filtruleSenderOnly, "foo", false},
		{29, "-! foo", "foo", filtruleNegate, "bar", true},
		{29, "-/ /abs", "/abs", 0, "abs", true},
	} {
		fr, err := parseFilterProto(tt.proto, tt.line)
		if err != nil {
			t.Fatal(err)
		}
		if fr.pattern != tt.pattern || fr.flag != tt.flags {
			t.Errorf("parseFilterProto(%d, %q) = %q flags %b, want %q flags %b", tt.proto, tt.line, fr.pattern, fr.flag, tt.pattern, tt.flags)
		}
		if tt.excludes == "" {
			continue
		}
		var l filterRuleList
		l.addRule(fr)
		if got := l.Excluded(tt.excludes, false); got != tt.protected {
			t.Errorf("%q at %d: Excluded(%q) = %v, want %v", tt.line, tt.proto, tt.excludes, got, tt.protected)
		}
	}
}

// TestDirectoryRuleMatchesFolders checks that a rule ending in "/" is for
// folders alone (exclude.c:rule_matches): "+ */" must not include files,
// or "--include=*/ --exclude=*" would send everything.
func TestDirectoryRuleMatchesFolders(t *testing.T) {
	l := buildList(t, "+ b.txt", "+ */", "- *")
	for _, c := range []struct {
		name string
		dir  bool
		want bool
	}{
		{"b.txt", false, false},
		{"d", true, false},
		{"d/b.txt", false, false},
		{"y", false, true},
		{"d/x", false, true},
	} {
		if got := l.matches(c.name, c.dir); got != c.want {
			t.Errorf("matches(%q, dir=%v) = %v, want %v", c.name, c.dir, got, c.want)
		}
	}
	fr, err := parseFilterProto(30, "-! */")
	if err != nil {
		t.Fatal(err)
	}
	var neg filterRuleList
	neg.addRule(fr)
	if !neg.matches("file", false) {
		t.Errorf(`"-! */" should exclude a file`)
	}
	if neg.matches("dir", true) {
		t.Errorf(`"-! */" should not exclude a folder`)
	}
}
