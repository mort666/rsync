//go:build linux

package flist_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// TestDryRun checks that -n leaves the destination as it was when a gokrazy
// receiver gets symlinks and devices, which it used to create.
func TestDryRun(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "runs stock rsync")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-rlD", "-n"}
	for _, dir := range []struct {
		name string
		run  func(t *testing.T, dest string) (string, error)
	}{
		{"stock to gokrazy", func(t *testing.T, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
		}},
		{"gokrazy client pulls", func(t *testing.T, dest string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, "")
			return gokrazyPull(t, addr, "", dest, args...)
		}},
		{"gokrazy to gokrazy", func(t *testing.T, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			dest := t.TempDir()
			if err := os.WriteFile(filepath.Join(dest, "keep"), []byte("kept"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, dest)
			if out, err := dir.run(t, dest); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			compareSnapshots(t, snapshot(t, dest), before)
		})
	}
}
