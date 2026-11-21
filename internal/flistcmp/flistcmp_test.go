package flistcmp

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// awkward holds names where byte order, "folder before its contents" and
// "folders after files" disagree. Entries ending in "/" are folders.
var awkward = []string{
	"foo/", "foo/bar.txt", "foo/bar-x", "foo/bar/", "foo/bar/x", "foo.txt", "foo-bar", "foo_bar",
	"foo bar", "foo0", "fooZ", "foo~", "Foo", "a/", "a/b.x", "a/b-c", "a/b/", "a/b/c",
	"a/b.d", "a b", "a-b/", "a-b/z", "b", ".hidden", ".dir/", ".dir/x", "z/", "~",
}

// stockOrder lists dir with stock rsync and returns its entries in the
// order rsync's generator walks them, which is the sorted file list.
func stockOrder(t *testing.T, proto int, dir string) []Entry {
	t.Helper()
	bin, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync not installed")
	}
	out, err := exec.Command(bin, fmt.Sprintf("--protocol=%d", proto), "-r", "--list-only", dir+"/").Output()
	if err != nil {
		t.Fatalf("rsync --list-only: %v", err)
	}
	var got []Entry
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		// "drwxr-xr-x          4,096 2026/11/21 12:00:00 foo/bar"
		f := strings.SplitN(sc.Text(), " ", 2)
		if len(f) < 2 {
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		// The name is everything after the time, and may hold spaces.
		idx := strings.Index(sc.Text(), fields[3]) + len(fields[3]) + 1
		got = append(got, Entry{Name: sc.Text()[idx:], IsDir: sc.Text()[0] == 'd'})
	}
	return got
}

func TestCompareMatchesRsync(t *testing.T) {
	dir := t.TempDir()
	for _, n := range awkward {
		p := filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(n, "/")))
		if strings.HasSuffix(n, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if filepath.Base(p) == "." {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, proto := range []int{27, 28, 29} {
		t.Run(fmt.Sprintf("protocol%d", proto), func(t *testing.T) {
			want := stockOrder(t, proto, dir)
			if len(want) < len(awkward)-1 {
				t.Fatalf("rsync listed %d entries, want at least %d: %v", len(want), len(awkward)-1, want)
			}
			got := slices.Clone(want)
			// Start from a different order, so the sort has work to do.
			slices.Reverse(got)
			slices.SortStableFunc(got, func(a, b Entry) int { return Compare(int32(proto), a, b) })
			if !slices.Equal(got, want) {
				t.Errorf("order differs from rsync's\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestCompareCases(t *testing.T) {
	for _, tt := range []struct {
		proto int32
		a, b  Entry
		want  int // sign
	}{
		{27, Entry{"foo", true}, Entry{"foo.txt", false}, -1},
		// From 29 a folder compares as "foo/", after "foo.txt" ('/' > '.').
		{29, Entry{"foo", true}, Entry{"foo.txt", false}, 1},
		// ... and after any file at the same depth.
		{29, Entry{"a", true}, Entry{"b", false}, 1},
		{27, Entry{"a", true}, Entry{"b", false}, -1},
		// "." first.
		{29, Entry{".", true}, Entry{"+", false}, -1},
		{29, Entry{"x", false}, Entry{".", true}, 1},
		// A folder right before its contents.
		{29, Entry{"foo", true}, Entry{"foo/a", false}, -1},
		{29, Entry{"foo/a", false}, Entry{"foo-a", false}, 1},
		{27, Entry{"foo/a", false}, Entry{"foo-a", false}, 1},
		// A folder and a file of the same name are not equal from 29.
		{29, Entry{"same", true}, Entry{"same", false}, 1},
		{27, Entry{"same", true}, Entry{"same", false}, 0},
		{29, Entry{"same/x", false}, Entry{"same/x", false}, 0},
	} {
		got := Compare(tt.proto, tt.a, tt.b)
		if sign(got) != tt.want {
			t.Errorf("Compare(%d, %v, %v) = %d, want sign %d", tt.proto, tt.a, tt.b, got, tt.want)
		}
		if sign(Compare(tt.proto, tt.b, tt.a)) != -tt.want {
			t.Errorf("Compare(%d, %v, %v) is not the reverse of Compare(%v, %v)", tt.proto, tt.b, tt.a, tt.a, tt.b)
		}
	}
}

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

// Below protocol 29 the order is the byte order of the full paths, which is
// what gokrazy/rsync sorted by before this package existed.
func FuzzCompare27IsByteOrder(f *testing.F) {
	f.Add("a/b", "a-b", true, false)
	f.Add("foo", "foo/bar", false, true)
	f.Fuzz(func(t *testing.T, a, b string, dirA, dirB bool) {
		if !validName(a) || !validName(b) {
			t.Skip()
		}
		if got, want := sign(Compare(27, Entry{a, dirA}, Entry{b, dirB})), sign(strings.Compare(a, b)); got != want {
			t.Fatalf("Compare(27, %q, %q) = %d, strings.Compare %d", a, b, got, want)
		}
	})
}

// validName is what a file list can hold: a relative path of non-empty
// components without NUL.
func validName(s string) bool {
	if s == "" || strings.ContainsRune(s, 0) || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return false
	}
	return !strings.Contains(s, "//")
}
