//go:build linux

package flist_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// sharesInode names the files under dest that are the same inode as the
// file of that name under prev: what --link-dest linked.
func sharesInode(t *testing.T, dest, prev string) map[string]bool {
	t.Helper()
	shared := map[string]bool{}
	filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dest, p)
		old, err := os.Lstat(filepath.Join(prev, rel))
		if err == nil {
			shared[rel] = old.Sys().(*syscall.Stat_t).Ino == info.Sys().(*syscall.Stat_t).Ino
		}
		return nil
	})
	return shared
}

// TestAltDest backs a tree up, changes it, and backs it up again with
// --link-dest, --copy-dest and --compare-dest against the first backup,
// in all five directions. What each leaves, and which files it shares with
// the first backup, must be what stock rsync's own run leaves.
func TestAltDest(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	src := filepath.Join(t.TempDir(), "src")
	write := func(name, content string, perm os.FileMode) {
		t.Helper()
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, perm); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("block of data that stays the same\n", 4000)
	write("same", "unchanged", 0o644)
	write("grown", long, 0o644)
	write("chmod", "same content", 0o644)
	write("sub/deep", "deep", 0o600)
	if err := os.Symlink("same", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	// The first backup, made by stock rsync for every run alike.
	first := t.TempDir()
	if out, err := stock(t, "-a", src+"/", first+"/"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// What changed since: more data in one file, other permissions on
	// another, and a new one.
	f, err := os.OpenFile(filepath.Join(src, "grown"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("and a new line at the end\n")
	f.Close()
	if err := os.Chmod(filepath.Join(src, "chmod"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("new", "new file", 0o644)

	// Each run backs up into top/cur, against top/prev, a copy of the
	// first backup that keeps its inodes for comparing.
	layout := func(t *testing.T) (top, cur, prev string) {
		top = t.TempDir()
		prev = filepath.Join(top, "prev")
		if out, err := stock(t, "-aH", first+"/", prev+"/"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return top, filepath.Join(top, "cur"), prev
	}

	type run func(t *testing.T, top string, args ...string) (string, error)
	for _, kind := range []string{"--link-dest", "--copy-dest", "--compare-dest"} {
		t.Run(kind, func(t *testing.T) {
			args := []string{"-a", kind + "=../prev"}
			_, ref, refPrev := layout(t)
			if out, err := stock(t, append(args, src+"/", ref+"/")...); err != nil {
				t.Fatalf("stock run: %v\n%s", err, out)
			}
			want := snapshot(t, ref)
			wantShared := sharesInode(t, ref, refPrev)
			if kind == "--link-dest" && !wantShared["same"] {
				t.Fatalf("stock rsync linked nothing: %v", wantShared)
			}
			for _, dir := range []struct {
				name string
				run  run
			}{
				{"stock to gokrazy", func(t *testing.T, top string, args ...string) (string, error) {
					ts := rsynctest.New(t, rsynctest.WritableInteropModule(top))
					return stock(t, append(args, "--stats", src+"/", "rsync://localhost:"+ts.Port+"/interop/cur/")...)
				}},
				{"gokrazy to stock", func(t *testing.T, top string, args ...string) (string, error) {
					ts := rsynctest.New(t, rsynctest.InteropModule(src))
					return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", filepath.Join(top, "cur")+"/")...)
				}},
				{"gokrazy client pulls", func(t *testing.T, top string, args ...string) (string, error) {
					addr, _ := startStockDaemonConf(t, src, "")
					return gokrazyPull(t, addr, "", filepath.Join(top, "cur"), args...)
				}},
				{"gokrazy client pushes", func(t *testing.T, top string, args ...string) (string, error) {
					addr, _ := startStockDaemonConf(t, top, "  read only = no\n  munge symlinks = no\n")
					return gokrazyRun(t, addr, "mod/cur/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
				}},
				{"gokrazy to gokrazy", func(t *testing.T, top string, args ...string) (string, error) {
					ts := rsynctest.New(t, rsynctest.WritableInteropModule(top))
					return gokrazyRun(t, "localhost:"+ts.Port, "interop/cur/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
				}},
			} {
				t.Run(dir.name, func(t *testing.T) {
					top, cur, prev := layout(t)
					out, err := dir.run(t, top, args...)
					if err != nil {
						t.Fatalf("%v\n%s", err, out)
					}
					compareSnapshots(t, snapshot(t, cur), want)
					got := sharesInode(t, cur, prev)
					for name, w := range wantShared {
						if got[name] != w {
							t.Errorf("%s: shares the earlier backup's inode: %v, want %v", name, got[name], w)
						}
					}
					// The grown file goes as a delta against its earlier
					// copy, where the receiver keeps it.
					if strings.Contains(out, "Matched data: 0 bytes") {
						t.Errorf("nothing matched: the earlier copy was not the basis\n%s", out)
					}
				})
			}
		})
	}
}
