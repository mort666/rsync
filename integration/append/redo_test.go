package append_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/internal/testlogger"
	"github.com/gokrazy/rsync/rsyncclient"
)

var redoArgs = []string{"-v", "-t", "--inplace", "--partial", "--append-verify"}

// redoFixture is appendFixture with a destination prefix that differs from
// the source, so --append-verify fails the file in phase 0 and the redo
// pass has to send it again without append.
func redoFixture(t *testing.T) (srcPath, destPath string, full []byte) {
	t.Helper()
	srcPath, destDir, full := appendFixture(t)
	destPath = filepath.Join(destDir, "data.bin")
	old := bytes.Clone(full[:appendPrefix])
	old[appendPrefix/2] ^= 0xff
	if err := os.WriteFile(destPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	return srcPath, destPath, full
}

func checkRedone(t *testing.T, destPath string, full []byte) {
	t.Helper()
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("dest after the redo pass: %d bytes, want the %d-byte source", len(got), len(full))
	}
}

// TestRedoFromSystemRsync has a gokrazy receiver pull from system rsync. It
// used to stop the run at the first file that failed verification; rsync
// asks for it again in the redo pass.
func TestRedoFromSystemRsync(t *testing.T) {
	t.Parallel()

	srcPath, destPath, full := redoFixture(t)
	client, err := rsyncclient.New(redoArgs, rsyncclient.WithStderr(testlogger.New(t)))
	if err != nil {
		t.Fatal(err)
	}
	rsync := exec.Command(rsynctest.TridgeOrGTFO(t, "openrsync has no --append-verify"), client.ServerCommandOptions(srcPath)...)
	stdin, err := rsync.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := rsync.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	rsync.Stderr = testlogger.New(t)
	if err := rsync.Start(); err != nil {
		t.Fatal(err)
	}
	res, err := client.Run(t.Context(), &readWriter{Reader: stdout, Writer: stdin}, []string{filepath.Dir(destPath)})
	if err != nil {
		rsync.Process.Kill()
		rsync.Wait()
		t.Fatalf("client.Run: %v", err)
	}
	_ = stdin.Close()
	if err := rsync.Wait(); err != nil {
		t.Fatalf("rsync.Wait: %v", err)
	}
	if res.XferErrors > 0 {
		t.Fatalf("%d transfer error(s); the redo pass should have fixed the file", res.XferErrors)
	}
	checkRedone(t, destPath, full)
}

// TestRedoGorsync covers the redo pass between gokrazy ends, with the
// receiver as client (download) and as server (upload).
func TestRedoGorsync(t *testing.T) {
	t.Parallel()

	t.Run("download", func(t *testing.T) {
		t.Parallel()
		srcPath, destPath, full := redoFixture(t)
		client, err := rsyncclient.New(redoArgs, rsyncclient.WithStderr(testlogger.New(t)))
		if err != nil {
			t.Fatal(err)
		}
		runAgainstServer(t, client, client.ServerCommandOptions(srcPath), []string{filepath.Dir(destPath)})
		checkRedone(t, destPath, full)
	})

	t.Run("upload", func(t *testing.T) {
		t.Parallel()
		srcPath, destPath, full := redoFixture(t)
		client, err := rsyncclient.New(redoArgs,
			rsyncclient.WithSender(),
			rsyncclient.WithStderr(testlogger.New(t)),
		)
		if err != nil {
			t.Fatal(err)
		}
		runAgainstServer(t, client, client.ServerCommandOptions(filepath.Dir(destPath)), []string{srcPath})
		checkRedone(t, destPath, full)
	})
}
