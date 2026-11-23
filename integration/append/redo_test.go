package append_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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

// killOnClose is a connection to a local rsync process that kills the
// process when closed.
type killOnClose struct {
	readWriter
	cmd *exec.Cmd
}

func (k *killOnClose) Close() error { return k.cmd.Process.Kill() }

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
	// Stock rsync runs in the source's folder and gets the bare name: on
	// Windows (msys2, Cygwin) it splits names at "/" only, so a path with
	// backslashes goes into the file list whole, and it reads "C:/..." as
	// relative.
	rsync := exec.Command(rsynctest.TridgeOrGTFO(t, "openrsync has no --append-verify"), client.ServerCommandOptions(filepath.Base(srcPath))...)
	rsync.Dir = filepath.Dir(srcPath)
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
	// Closing the connection stops rsync, so a client that fails, or a
	// run past the deadline, ends the test instead of hanging on a read
	// that will never return.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	conn := &killOnClose{readWriter: readWriter{Reader: stdout, Writer: stdin}, cmd: rsync}
	res, err := client.Run(ctx, conn, []string{filepath.Dir(destPath)})
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
