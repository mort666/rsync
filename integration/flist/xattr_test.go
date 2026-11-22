//go:build linux

package flist_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
	"golang.org/x/sys/unix"
)

// Extended attributes (-X), against stock rsync both ways. Values over 32
// bytes travel abbreviated, as their MD5 sum, and the generator asks for
// them; shorter ones go in the file list.

// bigValue is long enough to be abbreviated.
var bigValue = bytes.Repeat([]byte("0123456789"), 100)

// xattrFiles are the source's user xattrs: two files with the same list,
// so the second refers back to the first, and one file without any.
var xattrFiles = map[string]map[string][]byte{
	"small":   {"user.small": []byte("v")},
	"big":     {"user.big": bigValue, "user.tag": []byte("t")},
	"same1":   {"user.shared": []byte("s"), "user.long": bigValue},
	"same2":   {"user.shared": []byte("s"), "user.long": bigValue},
	"plain":   {},
	"dir":     {"user.dirattr": []byte("d")},
	"dir/sub": {"user.inner": bigValue},
}

func stockHasXattrs(t *testing.T) {
	t.Helper()
	bin := rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "xattrs") || strings.Contains(string(out), "no xattrs") {
		t.Skip("stock rsync has no xattr support")
	}
}

func setXattrs(t *testing.T, path string, values map[string][]byte) {
	t.Helper()
	for name, v := range values {
		if err := unix.Setxattr(path, name, v, 0); err != nil {
			if errors.Is(err, unix.ENOTSUP) {
				t.Skipf("no user xattrs on this filesystem: %v", err)
			}
			t.Fatalf("setxattr %s %s: %v", path, name, err)
		}
	}
}

func userXattrs(t *testing.T, path string) map[string][]byte {
	t.Helper()
	buf := make([]byte, 64*1024)
	n, err := unix.Listxattr(path, buf)
	if err != nil {
		t.Fatalf("listxattr %s: %v", path, err)
	}
	got := map[string][]byte{}
	for _, name := range strings.Split(string(buf[:n]), "\x00") {
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		v := make([]byte, 64*1024)
		m, err := unix.Getxattr(path, name, v)
		if err != nil {
			t.Fatalf("getxattr %s %s: %v", path, name, err)
		}
		got[name] = v[:m]
	}
	return got
}

// xattrTree makes the source tree with its xattrs.
func xattrTree(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	for _, name := range []string{"dir/sub", "small", "big", "same1", "same2", "plain"} {
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, values := range xattrFiles {
		setXattrs(t, filepath.Join(src, name), values)
	}
	return src
}

// checkXattrs wants dest's user xattrs to be src's.
func checkXattrs(t *testing.T, src, dest string) {
	t.Helper()
	for name := range xattrFiles {
		want := userXattrs(t, filepath.Join(src, name))
		got := userXattrs(t, filepath.Join(dest, name))
		if !maps.EqualFunc(got, want, bytes.Equal) {
			t.Errorf("%s: xattrs %s, want %s", name, describe(got), describe(want))
		}
	}
}

func describe(m map[string][]byte) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%d bytes", k, len(v)))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// changeXattrs changes the source after a first run, and dest's copy of
// one file: a changed abbreviated value (requested), a removed one, a new
// one, an xattr dest has but the source never had (removed), and a file
// whose data changes but whose abbreviated value does not (taken from the
// file it replaces).
func changeXattrs(t *testing.T, src, dest string) {
	t.Helper()
	changed := bytes.Clone(bigValue)
	changed[500] = 'X'
	setXattrs(t, filepath.Join(src, "big"), map[string][]byte{"user.big": changed})
	if err := unix.Removexattr(filepath.Join(src, "big"), "user.tag"); err != nil {
		t.Fatal(err)
	}
	setXattrs(t, filepath.Join(src, "plain"), map[string][]byte{"user.new": []byte("n")})
	setXattrs(t, filepath.Join(dest, "small"), map[string][]byte{"user.extra": []byte("x")})
	if err := os.WriteFile(filepath.Join(src, "same1"), []byte("new content, same xattrs"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestXattrs(t *testing.T) {
	stockHasXattrs(t)
	if p, err := strconv.Atoi(os.Getenv("RSYNC_MATRIX_PROTOCOL")); err == nil && p < 30 {
		t.Skip("-X needs protocol 30 (TestXattrsNeedProtocol30)")
	}
	type run func(t *testing.T, src, dest string, args ...string) (string, error)
	for _, dir := range []struct {
		name string
		run  run
	}{
		{"stock to gokrazy", func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
		}},
		{"gokrazy to stock", func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
		}},
		{"gokrazy client pulls", func(t *testing.T, src, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, "")
			return gokrazyPull(t, addr, "", dest, args...)
		}},
		{"gokrazy client pushes", func(t *testing.T, src, dest string, args ...string) (string, error) {
			addr := startStockDaemonModule(t, dest, false)
			return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
		{"gokrazy to gokrazy", func(t *testing.T, src, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			src, dest := xattrTree(t), t.TempDir()
			args := []string{"-rtX"}
			if out, err := dir.run(t, src, dest, args...); err != nil {
				t.Fatalf("first run: %v\n%s", err, out)
			}
			checkXattrs(t, src, dest)

			changeXattrs(t, src, dest)
			if out, err := dir.run(t, src, dest, args...); err != nil {
				t.Fatalf("second run: %v\n%s", err, out)
			}
			checkXattrs(t, src, dest)
			if got, _ := os.ReadFile(filepath.Join(dest, "same1")); string(got) != "new content, same xattrs" {
				t.Errorf("same1 = %q, want the new content", got)
			}
		})
	}
}

// Without protocol 30 there are no xattrs: both sides refuse -X.
func TestXattrsNeedProtocol30(t *testing.T) {
	stockHasXattrs(t)
	src, dest := xattrTree(t), t.TempDir()
	ts := rsynctest.New(t, rsynctest.InteropModule(src))
	out, err := stock(t, "--protocol=29", "-rX", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
	if err == nil {
		t.Fatalf("-X at protocol 29 succeeded:\n%s", out)
	}
	if !strings.Contains(out, "requires protocol 30") {
		t.Errorf("want an error that -X requires protocol 30:\n%s", out)
	}
}
