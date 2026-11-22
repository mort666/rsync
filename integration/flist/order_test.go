//go:build linux

package flist_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// awkwardFiles are names whose order differs between protocol 27 and 29,
// where folders compare as if they ended in "/" and sort after files at the
// same depth. Each file holds its own name, so a file that gets another's
// data shows. A wrong order makes both sides mean different files by the
// same index.
var awkwardFiles = []string{
	"foo/bar.txt", "foo/bar-x", "foo/bar/x", "foo.txt", "foo-bar", "foo_bar", "foo bar",
	"foo0", "fooZ", "foo~", "Foo", "a/b.x", "a/b-c", "a/b/c", "a/b.d", "a b", "a-b/z",
	"b", ".hidden", ".dir/x", "~",
}

// changed is the file the second sync must send, and no other.
const changed = "a/b/c"

var oldTime = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

func makeAwkwardTree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	for _, name := range awkwardFiles {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "z"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// changeOne gives the changed file new content of a new length and time.
// It returns the new content's length.
func changeOne(t *testing.T, src string) int {
	t.Helper()
	content := strings.Repeat("changed ", 100)
	p := filepath.Join(src, filepath.FromSlash(changed))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return len(content)
}

func checkAwkward(t *testing.T, dest string, changedLen int) {
	t.Helper()
	for _, name := range awkwardFiles {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if name == changed && changedLen > 0 {
			if len(got) != changedLen {
				t.Errorf("%s: %d bytes, want the changed %d", name, len(got), changedLen)
			}
			continue
		}
		if string(got) != name {
			t.Errorf("%s holds %q, want its own name", name, got)
		}
	}
	if fi, err := os.Stat(filepath.Join(dest, "z")); err != nil || !fi.IsDir() {
		t.Errorf("z: %v, %v; want a folder", fi, err)
	}
}

// inodes records each file's inode. rsync writes a file it transfers to a
// temporary name and renames it, so a file left alone keeps its inode.
func inodes(t *testing.T, dest string) map[string]uint64 {
	t.Helper()
	m := make(map[string]uint64)
	for _, name := range awkwardFiles {
		fi, err := os.Stat(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		m[name] = fi.Sys().(*syscall.Stat_t).Ino
	}
	return m
}

func checkOnlyChangedReplaced(t *testing.T, before, after map[string]uint64) {
	t.Helper()
	for name, ino := range before {
		if replaced := after[name] != ino; replaced != (name == changed) {
			t.Errorf("%s replaced: %v, want %v", name, replaced, name == changed)
		}
	}
}

var literalRe = regexp.MustCompile(`Literal data: ([0-9,]+) bytes`)

// literalData is what stock rsync's --stats calls Literal data: the bytes
// it sent or received that matched nothing at the destination.
func literalData(t *testing.T, out string) int {
	t.Helper()
	m := literalRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no Literal data in rsync output:\n%s", out)
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Stock rsync pulls the tree from a gokrazy daemon (gokrazy sends), then
// pulls again after one file changed: only that file's bytes may move.
func TestAwkwardFromGokrazy(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		src := makeAwkwardTree(t)
		ts := rsynctest.New(t, rsynctest.InteropModule(src))
		dest := t.TempDir()
		pull := func() string {
			out, err := stock(t, fmt.Sprintf("--protocol=%d", proto), "-a", "--stats", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			return out
		}
		pull()
		checkAwkward(t, dest, 0)
		n := changeOne(t, src)
		if got := literalData(t, pull()); got != n {
			t.Errorf("second pull: Literal data %d bytes, want %d (the changed file only)", got, n)
		}
		checkAwkward(t, dest, n)
	})
}

// Stock rsync pushes the tree to a gokrazy daemon (gokrazy generates and
// receives).
func TestAwkwardToGokrazy(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		src := makeAwkwardTree(t)
		dest := t.TempDir()
		ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
		push := func() string {
			out, err := stock(t, fmt.Sprintf("--protocol=%d", proto), "-rt", "--stats", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			return out
		}
		push()
		checkAwkward(t, dest, 0)
		before := inodes(t, dest)
		n := changeOne(t, src)
		if got := literalData(t, push()); got != n {
			t.Errorf("second push: Literal data %d bytes, want %d (the changed file only)", got, n)
		}
		checkAwkward(t, dest, n)
		checkOnlyChangedReplaced(t, before, inodes(t, dest))
	})
}

// A gokrazy client pulls the tree from a stock daemon (stock sends).
func TestAwkwardPullFromStock(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		src := makeAwkwardTree(t)
		addr := startStockDaemon(t, src)
		dest := t.TempDir()
		pull := func() {
			if out, err := gokrazyPull(t, addr, "", dest, fmt.Sprintf("--protocol=%d", proto), "-rt"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}
		pull()
		checkAwkward(t, dest, 0)
		before := inodes(t, dest)
		n := changeOne(t, src)
		pull()
		checkAwkward(t, dest, n)
		checkOnlyChangedReplaced(t, before, inodes(t, dest))
	})
}

// A gokrazy client pushes the tree to a stock daemon (stock generates and
// receives).
func TestAwkwardPushToStock(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		src := makeAwkwardTree(t)
		dest := t.TempDir()
		addr := startStockDaemonModule(t, dest, false)
		push := func() {
			if out, err := gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, fmt.Sprintf("--protocol=%d", proto), "-rt"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}
		push()
		checkAwkward(t, dest, 0)
		before := inodes(t, dest)
		n := changeOne(t, src)
		push()
		checkAwkward(t, dest, n)
		checkOnlyChangedReplaced(t, before, inodes(t, dest))
	})
}

// From protocol 29 the generator tells the sender why it wants each file,
// and a stock sender with -i prints it: "<f+++++++++" for a new file,
// "<f.st......" for one whose size and time changed. (Below 29 the sender
// knows nothing but the index.)
func TestItemizeFromGokrazyGenerator(t *testing.T) {
	if v := rsynctest.RsyncVersion(t); !rsynctest.RsyncVersionAtLeast(t, "3.0.0") {
		t.Skipf("rsync %s prints item flags in an older format", v)
	}
	src := makeAwkwardTree(t)
	dest := t.TempDir()
	ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
	push := func() string {
		out, err := stock(t, "--protocol=29", "-rti", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	if out := push(); !regexp.MustCompile(`(?m)^<f\+{9} foo\.txt$`).MatchString(out) {
		t.Errorf("first push: no new-file line for foo.txt")
	}
	changeOne(t, src)
	out := push()
	if !regexp.MustCompile(`(?m)^<f\.st\.{6} a/b/c$`).MatchString(out) {
		t.Errorf("second push: no size-and-time line for %s", changed)
	}
	if strings.Contains(out, "foo.txt") {
		t.Errorf("second push names foo.txt, which did not change")
	}
}

// --delete with an exclude: the client sends its filter list, and the
// gokrazy daemon receiver keeps what it excludes. (It does not take
// --delete-excluded, after which a client below protocol 29 sends no list.)
func TestDeleteExcludedToGokrazy(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		for _, deleteExcluded := range []bool{false} {
			t.Run(fmt.Sprintf("delete-excluded=%v", deleteExcluded), func(t *testing.T) {
				src := makeAwkwardTree(t)
				dest := t.TempDir()
				for _, name := range []string{"extra.txt", "keep.log"} {
					if err := os.WriteFile(filepath.Join(dest, name), []byte(name), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
				args := []string{fmt.Sprintf("--protocol=%d", proto), "-rt", "--delete", "--exclude=*.log"}
				if deleteExcluded {
					args = append(args, "--delete-excluded")
				}
				if out, err := stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				checkAwkward(t, dest, 0)
				if _, err := os.Stat(filepath.Join(dest, "extra.txt")); !os.IsNotExist(err) {
					t.Errorf("extra.txt: %v, want it deleted", err)
				}
				_, err := os.Stat(filepath.Join(dest, "keep.log"))
				if kept := err == nil; kept == deleteExcluded {
					t.Errorf("keep.log kept: %v, want %v", kept, !deleteExcluded)
				}
			})
		}
	})
}

// From protocol 29 a stock server tells a client that asked for -v which
// files --delete removed (MSG_DELETED). A gokrazy client used to fail on
// the unknown message.
func TestDeleteMessagesFromStock(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		src := makeAwkwardTree(t)
		dest := t.TempDir()
		if err := os.WriteFile(filepath.Join(dest, "extra.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		addr := startStockDaemonModule(t, dest, false)
		out, err := gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, fmt.Sprintf("--protocol=%d", proto), "-rtv", "--delete")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dest, "extra.txt")); !os.IsNotExist(err) {
			t.Errorf("extra.txt: %v, want it deleted", err)
		}
		if proto >= 29 && !strings.Contains(out, "deleting extra.txt") {
			t.Errorf("no \"deleting extra.txt\" in the client's output:\n%s", out)
		}
	})
}
