//go:build linux

package flist_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
	"github.com/gokrazy/rsync/rsyncd"
)

// linkTree makes a source with symlinks of every kind -L, -k and
// --copy-unsafe-links tell apart: to a file and to a folder inside it,
// one climbing out of a folder but staying inside, one to nothing, and,
// with outside, ones leading out of it to a file and a folder and one
// with an absolute target.
func linkTree(t *testing.T, outside bool) string {
	t.Helper()
	top := t.TempDir()
	src := filepath.Join(top, "src")
	write := func(name, content string) {
		t.Helper()
		full := filepath.Join(top, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	write("src/file", "file")
	write("src/sub/inner", "inner")
	link("file", "rel")
	link("sub", "dirlink")
	link("../file", "sub/up")
	link("nowhere", "dangling")
	if outside {
		write("outside/o", "outside")
		write("outside/od/x", "outside folder")
		link("../outside/o", "unsafe")
		link("../outside/od", "unsafedir")
		link(filepath.Join(top, "outside", "o"), "abs")
	}
	return src
}

// linkArgs are the option sets compared, each with -rt so that the copies'
// file times agree.
var linkArgs = [][]string{
	{"-rtl"},
	{"-rtL"},
	{"-rtlk"},
	{"-rtk"},
	{"-rtl", "--copy-unsafe-links"},
	{"-rt", "--copy-unsafe-links"},
	{"-rtl", "--safe-links"},
}

// TestCopyLinks compares what -L, -k, --copy-unsafe-links and --safe-links
// leave behind
// with stock rsync's own local copy, in all five directions. A daemon
// keeps a client to its module, so where a daemon sends, the source has
// no links out of it; where a client sends, it has.
func TestCopyLinks(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	// A stock daemon without chroot munges symlinks by default.
	const conf = "  munge symlinks = no\n"
	type run func(t *testing.T, src, dest string, args ...string) (string, error)
	for _, dir := range []struct {
		name    string
		outside bool // the sender is a client: links may lead out
		run     run
	}{
		{"stock to gokrazy", true, func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
		}},
		{"gokrazy to stock", false, func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
		}},
		{"gokrazy client pulls", false, func(t *testing.T, src, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, conf)
			return gokrazyPull(t, addr, "", dest, args...)
		}},
		// A stock daemon without chroot sanitises received link targets
		// against the depth of the last folder named in the file list
		// (flist.c:recv_file_entry, lastdir_depth), so the order of the
		// list decides what "../x" becomes. Stock sends a folder's
		// entries before going into its subfolders, gokrazy goes depth
		// first; both are valid, and the receiver sorts the list. So no
		// links out of the source here; gokrazy to gokrazy has them.
		{"gokrazy client pushes", false, func(t *testing.T, src, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, dest, "  read only = no\n"+conf)
			return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
		{"gokrazy to gokrazy", true, func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			src := linkTree(t, dir.outside)
			for _, args := range linkArgs {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					ref := t.TempDir()
					// Stock rsync reports the dangling link with -L and
					// exits 23; only what it leaves counts.
					stock(t, append(args, src+"/", ref+"/")...)
					dest := t.TempDir()
					dir.run(t, src, dest, args...)
					compareSnapshots(t, snapshot(t, dest), snapshot(t, ref))
				})
			}
		})
	}
}

// A daemon follows links only inside its module, as a chrooted stock
// daemon does: one leading out is an error, and the rest goes across.
func TestCopyLinksConfined(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	src := linkTree(t, true)
	ts := rsynctest.New(t, rsynctest.InteropModule(src))
	dest := t.TempDir()
	out, err := stock(t, "-rL", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
	if err == nil {
		t.Errorf("transfer succeeded, want exit 23 for links out of the module:\n%s", out)
	}
	for _, name := range []string{"unsafe", "unsafedir", "abs"} {
		if _, err := os.Lstat(filepath.Join(dest, name)); err == nil {
			t.Errorf("%s: sent, though it leads out of the module", name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dest, "dirlink", "inner")); err != nil || string(b) != "inner" {
		t.Errorf("dirlink/inner: %q, %v", b, err)
	}
}

// -K: a symlink to a folder at the destination stands for the folder, so
// what the sender has in a folder of that name goes into the folder the
// link points to, and the link stays.
func TestKeepDirlinks(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "linked", "f"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepare := func(t *testing.T) string {
		dest := t.TempDir()
		if err := os.Mkdir(filepath.Join(dest, "real"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(dest, "linked")); err != nil {
			t.Fatal(err)
		}
		return dest
	}
	ref := prepare(t)
	if out, err := stock(t, "-rtK", src+"/", ref+"/"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := snapshot(t, ref)
	if !strings.Contains(want["linked"], "target=") {
		t.Fatalf("stock rsync replaced the link: %v", want)
	}
	for _, dir := range []struct {
		name string
		run  func(t *testing.T, dest string) (string, error)
	}{
		{"stock to gokrazy", func(t *testing.T, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return stock(t, "-rtK", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
		}},
		{"gokrazy client pulls", func(t *testing.T, dest string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, "")
			return gokrazyPull(t, addr, "", dest, "-rtK")
		}},
		{"gokrazy to gokrazy", func(t *testing.T, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, "-rtK")
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			dest := prepare(t)
			if out, err := dir.run(t, dest); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			compareSnapshots(t, snapshot(t, dest), want)
		})
	}
}

// --munge-links on a receiving client, and munge_symlinks on a daemon
// module, which also takes the prefix off what the daemon sends.
func TestMungeLinks(t *testing.T) {
	bin := rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	if out, _ := exec.Command(bin, "--help").Output(); !strings.Contains(string(out), "--munge-links") {
		t.Skip("stock rsync has no --munge-links (new in 3.1)")
	}
	src := linkTree(t, false)
	if err := os.Symlink("/rsyncd-munged/file", filepath.Join(src, "munged")); err != nil {
		t.Fatal(err)
	}
	// A local copy munges on both sides, so a client pulling from a
	// daemon that does not is the reference.
	addr, _ := startStockDaemonConf(t, src, "  munge symlinks = no\n")
	ref := t.TempDir()
	stock(t, "-rtl", "--munge-links", "rsync://"+addr+"/mod/", ref+"/")
	want := snapshot(t, ref)
	if !strings.Contains(want["rel"], "/rsyncd-munged/file") {
		t.Fatalf("stock rsync did not munge: %v", want)
	}
	munged := func(dir string) []rsyncd.Module {
		m := rsynctest.WritableInteropModule(dir)
		m[0].MungeSymlinks = true
		return m
	}
	t.Run("gokrazy client pulls", func(t *testing.T) {
		dest := t.TempDir()
		gokrazyPull(t, addr, "", dest, "-rtl", "--munge-links")
		compareSnapshots(t, snapshot(t, dest), want)
	})
	t.Run("stock to gokrazy daemon", func(t *testing.T) {
		// A stock daemon with munge symlinks is the reference here.
		ref := t.TempDir()
		addr, _ := startStockDaemonConf(t, ref, "  read only = no\n  munge symlinks = yes\n")
		stock(t, "-rtl", src+"/", "rsync://"+addr+"/mod/")
		dest := t.TempDir()
		ts := rsynctest.New(t, munged(dest))
		stock(t, "-rtl", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
		compareSnapshots(t, snapshot(t, dest), snapshot(t, ref))
	})
	t.Run("gokrazy daemon sends", func(t *testing.T) {
		// The munged tree goes back out as it came in.
		ts := rsynctest.New(t, munged(ref))
		dest := t.TempDir()
		stock(t, "-rtl", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
		again := t.TempDir()
		stock(t, "-rtl", src+"/", again+"/")
		compareSnapshots(t, snapshot(t, dest), snapshot(t, again))
	})
}
