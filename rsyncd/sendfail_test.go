package rsyncd_test

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/internal/testlogger"
	"github.com/gokrazy/rsync/rsyncclient"
	"github.com/gokrazy/rsync/rsyncd"
)

// A daemon that cannot open a file a client asks for tells the client,
// which counts it as an error, as stock rsync does (rsync/sender.c
// send_files). gorsyncd only logged it, so the client saw a download that
// left the file out as complete.
func TestDaemonTellsClientWhatItCannotSend(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce POSIX permission bits")
	}
	dir := t.TempDir()
	for _, rel := range []string{"ok.txt", "secret.txt"} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(dir, "secret.txt")
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(secret, 0o644) })

	stock := startStockDaemon(t, dir)
	ts := rsynctest.New(t, []rsyncd.Module{{Name: "mod", Path: dir}})
	for name, addr := range map[string]string{
		"stock rsync": stock,
		"gorsyncd":    net.JoinHostPort("localhost", ts.Port),
	} {
		client, err := rsyncclient.New([]string{"-r"}, rsyncclient.WithStdout(io.Discard), rsyncclient.WithStderr(testlogger.New(t)), rsyncclient.DontRestrict())
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		dst := t.TempDir()
		res, err := client.RunDaemon(t.Context(), conn, "mod/", []string{dst + "/"})
		conn.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.XferErrors == 0 {
			t.Errorf("%s: download with secret.txt unreadable: XferErrors = 0", name)
		}
		if _, err := os.Stat(filepath.Join(dst, "ok.txt")); err != nil {
			t.Errorf("%s: ok.txt did not arrive: %v", name, err)
		}
	}
}

// A daemon that cannot delete what a client's --delete asks for tells the
// client, which counts it as an error, as stock rsync does (rsync/delete.c
// delete_item). gorsyncd only logged it, so the delete looked done.
func TestDaemonTellsClientWhatItCannotDelete(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root deletes everything")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce POSIX permission bits")
	}
	for name, tt := range map[string]struct {
		start func(t *testing.T, dir string) string
		args  []string
	}{
		// From protocol 30 --delete means --delete-during, and stock
		// rsync then gets past a read-only folder it owns. Protocol 29
		// keeps the delete failing, which this reference needs.
		"stock rsync": {func(t *testing.T, dir string) string { return startStockDaemonModule(t, dir, false) }, []string{"--protocol=29"}},
		"gorsyncd": {func(t *testing.T, dir string) string {
			ts := rsynctest.New(t, []rsyncd.Module{{Name: "mod", Path: dir, Writable: true}})
			return net.JoinHostPort("localhost", ts.Port)
		}, nil},
	} {
		dir := t.TempDir()
		ro := filepath.Join(dir, "ro")
		if err := os.MkdirAll(ro, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ro, "old.txt"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ro, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(ro, 0o755) })
		addr := tt.start(t, dir)

		client, err := rsyncclient.New(append([]string{"-r", "--delete"}, tt.args...), rsyncclient.WithSender(), rsyncclient.WithStdout(io.Discard), rsyncclient.WithStderr(testlogger.New(t)), rsyncclient.DontRestrict())
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.RunDaemon(t.Context(), conn, "mod/ro/", []string{t.TempDir() + "/"})
		conn.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(ro, "old.txt")); err != nil {
			t.Fatalf("%s: old.txt is gone; the test needs a delete that fails: %v", name, err)
		}
		if res.XferErrors == 0 {
			t.Errorf("%s: --delete of old.txt in a read-only folder failed, but XferErrors = 0", name)
		}
	}
}
