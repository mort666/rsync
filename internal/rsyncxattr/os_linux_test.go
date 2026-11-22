//go:build linux

package rsyncxattr

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
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
	want := map[string][]byte{"user.keep": []byte("k"), "user.big": bytes.Repeat([]byte("b"), 5000), "user.empty": {}}
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
