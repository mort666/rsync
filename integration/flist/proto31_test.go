//go:build linux

package flist_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// Protocol 31: nanosecond mtimes, and delete stats at the end of a run.

// nsecTime has nanoseconds that a filesystem without them would lose.
var nsecTime = time.Date(2021, 3, 4, 5, 6, 7, 123456789, time.UTC)

func nsecTree(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(src, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "dir/b"} {
		full := filepath.Join(src, p)
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, nsecTime, nsecTime); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(filepath.Join(src, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if st.ModTime().Nanosecond() == 0 {
		t.Skip("the filesystem does not keep nanoseconds")
	}
	return src
}

// checkNsec wants the times rsync leaves at proto: all of them from 31,
// whole seconds below, where only the seconds are sent.
func checkNsec(t *testing.T, dest string, proto int) {
	t.Helper()
	want := nsecTime
	if proto < 31 {
		want = nsecTime.Truncate(time.Second)
	}
	for _, p := range []string{"a", "dir/b"} {
		st, err := os.Stat(filepath.Join(dest, p))
		if err != nil {
			t.Error(err)
			continue
		}
		if !st.ModTime().Equal(want) {
			t.Errorf("protocol %d: %s mtime %v, want %v", proto, p, st.ModTime().UTC(), want)
		}
	}
}

func TestNanosecondTimes(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		p := fmt.Sprintf("--protocol=%d", proto)
		t.Run("from gokrazy", func(t *testing.T) {
			src, dest := nsecTree(t), t.TempDir()
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			if _, err := stock(t, p, "-rt", "rsync://localhost:"+ts.Port+"/interop/", dest+"/"); err != nil {
				t.Fatal(err)
			}
			checkNsec(t, dest, proto)
		})
		t.Run("to gokrazy", func(t *testing.T) {
			src, dest := nsecTree(t), t.TempDir()
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			if _, err := stock(t, p, "-rt", src+"/", "rsync://localhost:"+ts.Port+"/interop/"); err != nil {
				t.Fatal(err)
			}
			checkNsec(t, dest, proto)
		})
		t.Run("gokrazy client pulls", func(t *testing.T) {
			src, dest := nsecTree(t), t.TempDir()
			addr, _ := startStockDaemonConf(t, src, "")
			if out, err := gokrazyPull(t, addr, "", dest, p, "-rt"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			checkNsec(t, dest, stockDaemonProtocol(proto))
		})
		t.Run("gokrazy client pushes", func(t *testing.T) {
			src, dest := nsecTree(t), t.TempDir()
			addr := startStockDaemonModule(t, dest, false)
			if out, err := gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, p, "-rt"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			checkNsec(t, dest, stockDaemonProtocol(proto))
		})
	})
}

// deleteTree is a source and a destination with one file and one folder
// (holding one file) that the source does not have.
func deleteTree(t *testing.T) (src, dest string) {
	t.Helper()
	src, dest = filepath.Join(t.TempDir(), "src"), t.TempDir()
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "keep"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "gonedir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"gone", "gonedir/inside"} {
		if err := os.WriteFile(filepath.Join(dest, p), []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src, dest
}

// deletedRe is stock rsync's count, which it prints from protocol 31.
var deletedRe = regexp.MustCompile(`Number of deleted files: (\d+)`)

func checkDeleted(t *testing.T, out, dest string, proto int) {
	t.Helper()
	for _, p := range []string{"gone", "gonedir"} {
		if _, err := os.Lstat(filepath.Join(dest, p)); err == nil {
			t.Errorf("%s was not deleted", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "keep")); err != nil {
		t.Errorf("keep: %v", err)
	}
	if proto < 31 {
		return
	}
	// gone, gonedir and gonedir/inside.
	if m := deletedRe.FindStringSubmatch(out); m == nil || m[1] != "3" {
		t.Errorf("protocol %d: stock rsync's stats say %q, want 3 deleted files:\n%s", proto, m, out)
	}
}

func TestDeleteStats(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		p := fmt.Sprintf("--protocol=%d", proto)
		// The stock client deletes and counts; the gokrazy sender passes
		// its count back to the client.
		t.Run("from gokrazy", func(t *testing.T) {
			src, dest := deleteTree(t)
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			out, err := stock(t, p, "-r", "--delete", "--stats", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
			if err != nil {
				t.Fatal(err)
			}
			checkDeleted(t, out, dest, proto)
		})
		// The gokrazy generator deletes and sends its count.
		t.Run("to gokrazy", func(t *testing.T) {
			src, dest := deleteTree(t)
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			out, err := stock(t, p, "-r", "--delete", "--stats", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
			if err != nil {
				t.Fatal(err)
			}
			checkDeleted(t, out, dest, proto)
		})
	})
}
