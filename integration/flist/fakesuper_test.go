//go:build linux

package flist_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
	"github.com/gokrazy/rsync/rsyncd"
	"golang.org/x/sys/unix"
)

// --fake-super, against stock rsync both ways: a receiver that is not
// root keeps devices, specials and symlinks as regular files, and owners
// and special mode bits in the user.rsync.%stat xattr. Both sides run
// with it, so the source may itself be such a backup. What each direction
// leaves on disk must be what stock rsync leaves, byte for byte.

const statXattr = "user.rsync.%stat"

func stockHasFakeSuper(t *testing.T) {
	t.Helper()
	stockHasXattrs(t)
	bin := rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	out, _ := exec.Command(bin, "--help").Output()
	if !strings.Contains(string(out), "--fake-super") {
		t.Skip("stock rsync has no --fake-super")
	}
}

// fakeSuperTree makes a source with what an ordinary user can make (a
// setuid file, a FIFO, a symlink) and what an earlier --fake-super backup
// holds: placeholders with %stat records for a device, a root-owned file
// and a symlink, and a security label kept under user.rsync.
func fakeSuperTree(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	write := func(name, content string, perm fs.FileMode) {
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
	write("file", "plain", 0o644)
	write("suid", "setuid", 0o755|fs.ModeSetuid)
	write("dir/inner", "inner", 0o600)
	if err := os.Chmod(filepath.Join(src, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	setXattrs(t, filepath.Join(src, "file"), map[string][]byte{"user.a": []byte("1")})
	setXattrs(t, filepath.Join(src, "dir"), map[string][]byte{"user.d": bigValue})
	if err := os.Symlink("file", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(src, "fifo"), 0o640); err != nil {
		t.Fatal(err)
	}

	write("faked-dev", "", 0o600)
	setXattrs(t, filepath.Join(src, "faked-dev"), map[string][]byte{statXattr: []byte("20640 1,3 0:0")})
	write("faked-root", "secret", 0o600)
	setXattrs(t, filepath.Join(src, "faked-root"), map[string][]byte{statXattr: []byte("104750 0,0 0:54321")})
	write("faked-link", "dir/inner", 0o600)
	setXattrs(t, filepath.Join(src, "faked-link"), map[string][]byte{statXattr: []byte("120777 0,0 54321:0")})
	write("labelled", "label", 0o644)
	setXattrs(t, filepath.Join(src, "labelled"), map[string][]byte{"user.rsync.security.selinux": []byte("system_u:object_r:etc_t:s0")})
	return src
}

// snapshot describes a tree as it is on disk, one line per entry: type,
// mode, owner, size, mtime (symlinks' too, not for folders, which the receivers touch in
// different orders), content or link target, and user xattrs.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	// rsync sets symlink times from 3.0 (CAN_SET_SYMLINK_TIMES).
	linkTimes := stockMaxProtocol(t) >= 30
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		var b strings.Builder
		fmt.Fprintf(&b, "%v %d:%d size=%d", info.Mode(), st.Uid, st.Gid, info.Size())
		switch {
		case info.Mode().IsRegular():
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " mtime=%d content=%q", info.ModTime().Unix(), content)
		case info.Mode().Type() == fs.ModeSymlink:
			target, _ := os.Readlink(path)
			fmt.Fprintf(&b, " target=%q", target)
			if linkTimes {
				fmt.Fprintf(&b, " mtime=%d", info.ModTime().Unix())
			}
		}
		x := lxattrs(t, path, "user.")
		for _, name := range slices.Sorted(maps.Keys(x)) {
			v := x[name]
			if len(v) > 40 {
				v = append(v[:20:20], "..."...)
			}
			fmt.Fprintf(&b, " %s=%q", name, v)
		}
		snap[rel] = b.String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func compareSnapshots(t *testing.T, got, want map[string]string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if got[name] != want[name] {
			t.Errorf("%s:\n got  %s\n want %s", name, got[name], want[name])
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: %s, not in stock rsync's copy", name, got[name])
		}
	}
}

func fakeSuperArgs() []string {
	if p, err := strconv.Atoi(os.Getenv("RSYNC_MATRIX_PROTOCOL")); err == nil && p < 30 {
		return []string{"-a"} // -X needs protocol 30
	}
	return []string{"-aX"}
}

func TestFakeSuper(t *testing.T) {
	stockHasFakeSuper(t)
	if stockMaxProtocol(t) < 31 {
		// Before 3.1 (protocol 31) rsync kept a symlink as a symlink,
		// without a %stat record, and cannot read the placeholders that
		// later versions, and gokrazy, write.
		t.Skip("stock rsync before 3.1 keeps symlinks differently")
	}
	args := fakeSuperArgs()

	// Stock rsync's own copy, both sides --fake-super, is the reference.
	src := fakeSuperTree(t)
	ref := t.TempDir()
	if out, err := stock(t, append(args, "--fake-super", src+"/", ref+"/")...); err != nil {
		t.Fatalf("stock copy: %v\n%s", err, out)
	}
	want := snapshot(t, ref)
	if !strings.Contains(want["fifo"], statXattr) || !strings.Contains(want["link"], statXattr) {
		t.Fatalf("stock rsync's copy has no %%stat records: %v", want)
	}

	fake := func(dir string) []rsyncd.Module {
		m := rsynctest.WritableInteropModule(dir)
		m[0].FakeSuper = true
		return m
	}
	src2 := func(dir string) []rsyncd.Module {
		m := rsynctest.InteropModule(dir)
		m[0].FakeSuper = true
		return m
	}
	// Without chroot a stock daemon munges symlinks by default, which
	// the reference copy does not.
	const fakeConf = "  fake super = yes\n  munge symlinks = no\n"
	type run func(t *testing.T, src, dest string, args ...string) (string, error)
	for _, dir := range []struct {
		name string
		run  run
	}{
		{"stock to gokrazy", func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, fake(dest))
			return stock(t, append(args, "--fake-super", src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
		}},
		{"gokrazy to stock", func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, src2(src))
			return stock(t, append(args, "--fake-super", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
		}},
		{"gokrazy client pulls", func(t *testing.T, src, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, fakeConf)
			return gokrazyPull(t, addr, "", dest, append(args, "--fake-super")...)
		}},
		{"gokrazy client pushes", func(t *testing.T, src, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, dest, "  read only = no\n"+fakeConf)
			return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, append(args, "--fake-super")...)
		}},
		{"gokrazy to gokrazy", func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, fake(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, append(args, "--fake-super")...)
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			dest := t.TempDir()
			if out, err := dir.run(t, src, dest, args...); err != nil {
				t.Fatalf("first run: %v\n%s", err, out)
			}
			compareSnapshots(t, snapshot(t, dest), want)
			// Again, with nothing to change: the records and
			// placeholders stay.
			if out, err := dir.run(t, src, dest, args...); err != nil {
				t.Fatalf("second run: %v\n%s", err, out)
			}
			compareSnapshots(t, snapshot(t, dest), want)
		})
	}
}

// A --fake-super sender sends what it keeps under user.rsync. in its own
// namespace, and leaves %stat out: an ordinary receiver that is not root
// keeps neither, but keeps the user xattrs.
func TestFakeSuperSendsNamespaces(t *testing.T) {
	stockHasFakeSuper(t)
	if os.Geteuid() == 0 {
		t.Skip("as root, the receiver would keep security names")
	}
	if p, err := strconv.Atoi(os.Getenv("RSYNC_MATRIX_PROTOCOL")); err == nil && p < 30 {
		t.Skip("-X needs protocol 30")
	}
	src := fakeSuperTree(t)
	for _, dir := range []struct {
		name string
		run  func(t *testing.T, dest string) (string, error)
	}{
		{"gokrazy daemon sends", func(t *testing.T, dest string) (string, error) {
			m := rsynctest.InteropModule(src)
			m[0].FakeSuper = true
			ts := rsynctest.New(t, m)
			return stock(t, "-rlX", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
		}},
		{"gokrazy client pushes", func(t *testing.T, dest string) (string, error) {
			addr, _ := startStockDaemonConf(t, dest, "  read only = no\n")
			return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, "-rlX", "--fake-super")
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			dest := t.TempDir()
			if out, err := dir.run(t, dest); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for name, want := range map[string]map[string][]byte{
				"labelled":   {},
				"faked-root": {},
				"file":       {"user.a": []byte("1")},
			} {
				if got := lxattrs(t, filepath.Join(dest, name), "user."); !maps.EqualFunc(got, want, bytes.Equal) {
					t.Errorf("%s: xattrs %s, want %s", name, describe(got), describe(want))
				}
			}
		})
	}
}
