//go:build linux

package rsyncxattr

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Set makes a file's xattrs exactly the given ones, within what keep
// covers, and Read reads them back.
func TestReadSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, "user.old", []byte("o"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("no user xattrs here")
		}
		t.Fatal(err)
	}
	unix.Setxattr(path, "user.keep", []byte("k"), 0)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	userOnly := func(name string) bool { return strings.HasPrefix(name, "user.") }
	current, err := Read(f, userOnly)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"user.keep": []byte("k"), "user.big": bytes.Repeat([]byte("b"), 2000), "user.empty": {}}
	if err := Set(f, want, current, func(name string) bool { return name != "user.keep" }); err != nil {
		t.Fatal(err)
	}
	got, err := Read(f, userOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("after Set: %v, want %v", keys(got), keys(want))
	}
	// keep says Set may not touch user.keep: it stays though not asked for.
	if err := Set(f, map[string][]byte{}, got, func(name string) bool { return name != "user.keep" }); err != nil {
		t.Fatal(err)
	}
	got, _ = Read(f, userOnly)
	if len(got) != 1 || got["user.keep"] == nil {
		t.Errorf("after removing: %v, want user.keep alone", keys(got))
	}
}

func keys(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// ReadAt reaches a symlink itself, not what it points at, and a FIFO
// without opening it (which would block); as root, SetAt sets and removes
// names Linux allows there.
func TestReadSetAt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(target, "user.t", []byte("t"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("no user xattrs here")
		}
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	all := func(string) bool { return true }
	for _, name := range []string{"link", "fifo"} {
		got, err := ReadAt(d, name, all)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: %v, want nothing (not the target's)", name, keys(got))
		}
	}
	if _, err := ReadAt(d, "missing", all); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v, want ErrNotExist", err)
	}
	if os.Geteuid() != 0 {
		t.Skip("setting trusted names needs root")
	}
	for _, name := range []string{"link", "fifo"} {
		want := map[string][]byte{"trusted.a": []byte("a"), "trusted.big": bytes.Repeat([]byte("b"), 100)}
		if err := SetAt(d, name, want, nil, all); err != nil {
			if errors.Is(err, unix.EPERM) {
				// Root in a container without CAP_SYS_ADMIN.
				t.Skipf("%s: %v (trusted names need CAP_SYS_ADMIN)", name, err)
			}
			t.Fatalf("%s: %v", name, err)
		}
		got, err := ReadAt(d, name, func(n string) bool { return strings.HasPrefix(n, "trusted.") })
		if err != nil || !maps.EqualFunc(got, want, bytes.Equal) {
			t.Errorf("%s: %v, %v; want %v", name, keys(got), err, keys(want))
		}
		if err := SetAt(d, name, map[string][]byte{"trusted.a": []byte("a")}, got, all); err != nil {
			t.Fatal(err)
		}
		if got, _ := ReadAt(d, name, func(n string) bool { return strings.HasPrefix(n, "trusted.") }); len(got) != 1 {
			t.Errorf("%s after removing: %v", name, keys(got))
		}
	}
	if got, _ := Read(mustOpen(t, target), all); len(got) != 1 || got["user.t"] == nil {
		t.Errorf("the target changed: %v", keys(got))
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// WriteStat stores and removes the %stat record, ReadStat reads it back,
// and FakeInfo applies it.
func TestStatRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadStat(d, "f", info); ok || err != nil {
		t.Fatalf("ReadStat before any record = %v, %v", ok, err)
	}
	s := Stat{Mode: 0o20640, Major: 1, Minor: 3, Uid: 0, Gid: 6}
	if err := WriteStat(d, "f", &s); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("no user xattrs here")
		}
		t.Fatal(err)
	}
	v := make([]byte, 64)
	n, err := unix.Getxattr(path, StatName, v)
	if err != nil || string(v[:n]) != "20640 1,3 0:6" {
		t.Errorf("stored %q, %v", v[:n], err)
	}
	got, ok, err := ReadStat(d, "f", info)
	if err != nil || !ok || got != s {
		t.Fatalf("ReadStat = %+v, %v, %v", got, ok, err)
	}
	fi := FakeInfo(info, got)
	if sys := fi.Sys().(*syscall.Stat_t); sys.Uid != 0 || sys.Gid != 6 || unix.Major(sys.Rdev) != 1 || unix.Minor(sys.Rdev) != 3 {
		t.Errorf("FakeInfo stat uid %d gid %d rdev %d,%d", sys.Uid, sys.Gid, unix.Major(sys.Rdev), unix.Minor(sys.Rdev))
	}
	if fi.Mode().Type() != os.ModeDevice|os.ModeCharDevice || fi.Mode().Perm() != 0o640 || fi.Size() != 0 {
		t.Errorf("FakeInfo mode %v size %d", fi.Mode(), fi.Size())
	}
	if err := WriteStat(d, "f", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ReadStat(d, "f", info); ok {
		t.Errorf("record still there after removal")
	}
	if err := WriteStat(d, "f", nil); err != nil {
		t.Errorf("removing no record: %v", err)
	}
}
