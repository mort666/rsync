//go:build linux

package flist_test

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// linkGroups names, for each file in root, the files it shares an inode
// with, itself included.
func linkGroups(t *testing.T, root string) map[string]string {
	t.Helper()
	byIno := map[uint64][]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		ino := info.Sys().(*syscall.Stat_t).Ino
		byIno[ino] = append(byIno[ino], rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]string{}
	for _, names := range byIno {
		slices.Sort(names)
		for _, n := range names {
			groups[n] = strings.Join(names, " ")
		}
	}
	return groups
}

func compareLinkGroups(t *testing.T, got, want map[string]string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if got[name] != want[name] {
			t.Errorf("%s: linked with %q, want %q", name, got[name], want[name])
		}
	}
}

// TestHardLinks compares what -H leaves behind, content and which names
// share an inode, with stock rsync's own copy, in all five directions.
// The second run adds a member to a group, and the destination has a
// member that was copied rather than linked.
func TestHardLinks(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	top := t.TempDir()
	src := filepath.Join(top, "src")
	write := func(name, content string) {
		t.Helper()
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(old, new string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, new)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(src, old), filepath.Join(src, new)); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "three names")
	link("a", "b")
	link("a", "sub/c")
	write("d", "two names")
	link("d", "z/e")
	write("f", "one name")
	args := []string{"-rtH"}

	type run func(t *testing.T, src, dest string) (string, error)
	for _, dir := range []struct {
		name string
		run  run
	}{
		{"stock to gokrazy", func(t *testing.T, src, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
		}},
		{"gokrazy to stock", func(t *testing.T, src, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
		}},
		{"gokrazy client pulls", func(t *testing.T, src, dest string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, "")
			return gokrazyPull(t, addr, "", dest, args...)
		}},
		{"gokrazy client pushes", func(t *testing.T, src, dest string) (string, error) {
			addr, _ := startStockDaemonConf(t, dest, "  read only = no\n")
			return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
		{"gokrazy to gokrazy", func(t *testing.T, src, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			// A fresh source per direction, as the second run changes it.
			src := filepath.Join(t.TempDir(), "src")
			if out, err := stock(t, "-aH", filepath.Join(top, "src")+"/", src+"/"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			dest, ref := t.TempDir(), t.TempDir()
			check := func(t *testing.T, step string) {
				t.Helper()
				if out, err := stock(t, append(args, src+"/", ref+"/")...); err != nil {
					t.Fatalf("stock copy: %v\n%s", err, out)
				}
				if out, err := dir.run(t, src, dest); err != nil {
					t.Fatalf("%s: %v\n%s", step, err, out)
				}
				compareSnapshots(t, snapshot(t, dest), snapshot(t, ref))
				compareLinkGroups(t, linkGroups(t, dest), linkGroups(t, ref))
			}
			check(t, "first run")
			if g := linkGroups(t, ref); g["a"] != "a b sub/c" {
				t.Fatalf("stock rsync's copy is not linked: %v", g)
			}

			// Add a member to a group, and break one in each copy: it is
			// a separate file with the same content and time.
			if err := os.Link(filepath.Join(src, "d"), filepath.Join(src, "g")); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{dest, ref} {
				b := filepath.Join(d, "b")
				info, err := os.Stat(b)
				if err != nil {
					t.Fatal(err)
				}
				os.Remove(b)
				os.WriteFile(b, []byte("three names"), 0o644)
				os.Chtimes(b, info.ModTime(), info.ModTime())
			}
			check(t, "second run")
		})
	}
}
