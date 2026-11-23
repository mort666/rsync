//go:build linux

package flist_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/testlogger"
	"github.com/gokrazy/rsync/rsyncclient"
	"github.com/gokrazy/rsync/rsyncd"
	"golang.org/x/sys/unix"
)

// The file list changed at protocol 28: extended flags, device numbers as
// major and minor, hard-link data and --checksum sums. These tests run each
// direction against stock rsync at every protocol from 27 up to
// rsyncwire.MaxProtocolVersion, forcing the version with stock rsync's
// --protocol.

func protocols(t *testing.T, fn func(t *testing.T, proto int)) {
	top := min(rsyncwire.MaxProtocolVersion, stockMaxProtocol(t))
	for p := rsyncwire.MinProtocolVersion; p <= top; p++ {
		t.Run(fmt.Sprintf("protocol%d", p), func(t *testing.T) { fn(t, p) })
	}
}

var protocolVersionRe = regexp.MustCompile(`protocol version (\d+)`)

// stockMaxProtocol is the newest protocol the stock rsync on PATH speaks,
// from its --version line ("rsync  version 2.6.9  protocol version 29").
// stockDaemonProtocol is the protocol a session with a stock daemon runs at
// when its client asks for proto: run-tests.sh -p forces the daemon lower
// (RSYNC_MATRIX_PROTOCOL), and a client's own --protocol cannot raise it.
func stockDaemonProtocol(proto int) int {
	if forced, err := strconv.Atoi(os.Getenv("RSYNC_MATRIX_PROTOCOL")); err == nil {
		return min(proto, forced)
	}
	return proto
}

func stockMaxProtocol(t *testing.T) int {
	t.Helper()
	bin := rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	m := protocolVersionRe.FindSubmatch(out)
	if m == nil {
		t.Fatalf("no protocol version in rsync --version:\n%s", out)
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// stock runs stock rsync and returns its combined output. Failing is up to
// the caller: a non-root mknod fails by design here.
func stock(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bin := rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	var buf bytes.Buffer
	// A file list the two sides read differently tends to hang rather than
	// fail, so do not wait for ever.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	t.Logf("%s %s\n%s", bin, strings.Join(args, " "), buf.String())
	return buf.String(), err
}

func startStockDaemon(t *testing.T, dir string) string {
	t.Helper()
	return startStockDaemonModule(t, dir, true)
}

func startStockDaemonModule(t *testing.T, dir string, readOnly bool) string {
	t.Helper()
	addr, _ := startStockDaemonConf(t, dir, fmt.Sprintf("  read only = %v\n", readOnly))
	return addr
}

// startStockDaemonConf serves dir as module mod with the extra module
// settings in conf (each line indented, ending in a newline). It returns
// the address and the daemon's log file.
func startStockDaemonConf(t *testing.T, dir, conf string) (addr, logFile string) {
	t.Helper()
	bin := rsynctest.TridgeOrGTFO(t, "compares with the stock daemon")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = l.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	l.Close()
	confFile := filepath.Join(t.TempDir(), "rsyncd.conf")
	cfg := fmt.Sprintf("use chroot = no\nport = %s\n", port)
	if os.Getuid() == 0 {
		// A daemon started as root runs transfers as nobody, which cannot
		// write to the test's folders (CI runs the suite as root in
		// containers).
		cfg += "uid = 0\ngid = 0\n"
	}
	cfg += fmt.Sprintf("[mod]\n  path = %s\n%s", dir, conf)
	if !strings.Contains(conf, "read only") {
		cfg += "  read only = yes\n"
	}
	if err := os.WriteFile(confFile, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile = filepath.Join(filepath.Dir(confFile), "d.log")
	cmd := exec.Command(bin, "--daemon", "--no-detach", "--config="+confFile, "--port="+port, "--log-file="+logFile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return addr, logFile
		}
	}
	t.Fatal("stock rsync daemon did not come up")
	return "", ""
}

// gokrazyPull runs a gokrazy client with args against mod/<path> on the
// daemon at addr, into dest, returning what it logged.
func gokrazyPull(t *testing.T, addr, path, dest string, args ...string) (string, error) {
	t.Helper()
	return gokrazyRun(t, addr, "mod/"+path, []string{dest}, nil, args...)
}

// gokrazyRun runs a gokrazy client against remotePath on the daemon at addr.
// paths are the destination when it receives, the sources when it sends.
func gokrazyRun(t *testing.T, addr, remotePath string, paths []string, extra []rsyncclient.Option, args ...string) (string, error) {
	t.Helper()
	_, log, err := gokrazyRunResult(t, addr, remotePath, paths, extra, args...)
	return log, err
}

// gokrazyRunResult is gokrazyRun, also returning the client's result.
func gokrazyRunResult(t *testing.T, addr, remotePath string, paths []string, extra []rsyncclient.Option, args ...string) (*rsyncclient.Result, string, error) {
	t.Helper()
	// The log is stdout and stderr, as a terminal shows them: names and
	// "deleting" lines go to stdout, as rsync's do.
	var log lockedBuffer
	opts := append([]rsyncclient.Option{rsyncclient.WithStdout(&log), rsyncclient.WithStderr(rsynctest.NopClose(io.MultiWriter(&log, testlogger.New(t)))), rsyncclient.DontRestrict()}, extra...)
	client, err := rsyncclient.New(args, opts...)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	res, err := client.RunDaemon(t.Context(), conn, remotePath, paths)
	return res, log.String(), err
}

func devNull(t *testing.T) (major, minor uint32) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat("/dev/null", &st); err != nil {
		t.Skip(err)
	}
	return unix.Major(st.Rdev), unix.Minor(st.Rdev)
}

// mknodLine matches the line rsync's generator (and gokrazy's) logs before
// it creates a device, carrying the numbers it received: from 3.0 on
// "mknod(null, 020666, [1,3])", in 2.6 "mknod(null,020666,0x103)" with the
// raw dev_t. rsync logs it at -vvv (3.1 and newer also at --debug=genr).
func mknodLine(name string, major, minor uint32) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`mknod\(%s, ?0?[0-7]+, ?(\[%d,%d\]|0x%x)\)`, regexp.QuoteMeta(name), major, minor, unix.Mkdev(major, minor)))
}

// A gokrazy sender's device numbers reach stock rsync intact. Creating the
// device needs root; --super makes stock rsync try, and log the numbers,
// anyway. (rsync 2.6.9 and 3.0.9 have --super too.)
func TestDeviceFromGokrazy(t *testing.T) {
	major, minor := devNull(t)
	ts := rsynctest.New(t, []rsyncd.Module{{Name: "dev", Path: "/dev"}})
	protocols(t, func(t *testing.T, proto int) {
		out, _ := stock(t, fmt.Sprintf("--protocol=%d", proto), "--super", "-D", "-vvv",
			"rsync://localhost:"+ts.Port+"/dev/null", t.TempDir()+"/")
		if !mknodLine("null", major, minor).MatchString(out) {
			t.Errorf("stock rsync did not receive /dev/null as %d,%d", major, minor)
		}
	})
}

// A stock sender's device numbers reach the gokrazy receiver intact.
func TestDeviceToGokrazy(t *testing.T) {
	major, minor := devNull(t)
	addr := startStockDaemon(t, "/dev")
	protocols(t, func(t *testing.T, proto int) {
		out, _ := gokrazyPull(t, addr, "null", t.TempDir(), fmt.Sprintf("--protocol=%d", proto), "-D", "--debug=genr")
		if !mknodLine("null", major, minor).MatchString(out) {
			t.Errorf("gokrazy did not receive /dev/null as %d,%d", major, minor)
		}
	})
}

// makeTree writes a folder with regular files, a FIFO, a symbolic link and
// a pair of hard links, the entry types whose file-list encoding differs
// between 27 and 28.
func makeTree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a": "alpha", "sub/b": "bravo", "c": "charlie"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "a-link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("c", filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func checkTree(t *testing.T, dest string, hardLinks bool) {
	t.Helper()
	for name, want := range map[string]string{"a": "alpha", "a-link": "alpha", "sub/b": "bravo", "c": "charlie"} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, want)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dest, "fifo")); err != nil || fi.Mode().Type() != os.ModeNamedPipe {
		t.Errorf("fifo: %v, %v; want a named pipe", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "symlink")); err != nil || target != "c" {
		t.Errorf("symlink: %q, %v; want c", target, err)
	}
	a, errA := os.Stat(filepath.Join(dest, "a"))
	link, errL := os.Stat(filepath.Join(dest, "a-link"))
	c, errC := os.Stat(filepath.Join(dest, "c"))
	if errA != nil || errL != nil || errC != nil {
		return // reported above
	}
	if got := os.SameFile(a, link); got != hardLinks {
		t.Errorf("a and a-link hard-linked: %v, want %v", got, hardLinks)
	}
	if os.SameFile(a, c) {
		t.Errorf("a and c are hard-linked, want separate files")
	}
}

// Stock rsync pulls the tree from a gokrazy daemon, with --checksum and
// with -H. The gokrazy sender sends hard-link data, so stock rsync can
// recreate the link.
func TestTreeFromGokrazy(t *testing.T) {
	src := makeTree(t)
	ts := rsynctest.New(t, []rsyncd.Module{{Name: "mod", Path: src}})
	protocols(t, func(t *testing.T, proto int) {
		for _, tt := range []struct {
			args      []string
			hardLinks bool
		}{
			{[]string{"-aD"}, false},
			{[]string{"-aDc"}, false},
			{[]string{"-aDH"}, true},
			{[]string{"-aDHc"}, true},
		} {
			t.Run(strings.Join(tt.args, ""), func(t *testing.T) {
				dest := t.TempDir()
				args := append([]string{fmt.Sprintf("--protocol=%d", proto)}, tt.args...)
				if out, err := stock(t, append(args, "rsync://localhost:"+ts.Port+"/mod/", dest+"/")...); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				checkTree(t, dest, tt.hardLinks)
			})
		}
	})
}

// A gokrazy client pulls the tree from a stock daemon; with -H the link
// arrives as a link.
func TestTreeToGokrazy(t *testing.T) {
	addr := startStockDaemon(t, makeTree(t))
	protocols(t, func(t *testing.T, proto int) {
		for _, tt := range []struct {
			args      []string
			hardLinks bool
		}{
			{[]string{"-aD"}, false},
			{[]string{"-aDc"}, false},
			{[]string{"-aDH"}, true},
			{[]string{"-aDHc"}, true},
		} {
			t.Run(strings.Join(tt.args, ""), func(t *testing.T) {
				dest := t.TempDir()
				if out, err := gokrazyPull(t, addr, "", dest, append([]string{fmt.Sprintf("--protocol=%d", proto)}, tt.args...)...); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				checkTree(t, dest, tt.hardLinks)
			})
		}
	})
}

// Stock rsync pushes the tree to a gokrazy daemon.
func TestTreePushToGokrazy(t *testing.T) {
	src := makeTree(t)
	protocols(t, func(t *testing.T, proto int) {
		for _, tt := range []struct {
			args      []string
			hardLinks bool
		}{
			{[]string{"-aD"}, false},
			{[]string{"-aDc"}, false},
			{[]string{"-aDH"}, true},
			{[]string{"-aDHc"}, true},
		} {
			args := tt.args
			t.Run(strings.Join(args, ""), func(t *testing.T) {
				dest := t.TempDir()
				ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
				all := append([]string{fmt.Sprintf("--protocol=%d", proto)}, args...)
				if out, err := stock(t, append(all, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				checkTree(t, dest, tt.hardLinks)
			})
		}
	})
}

// lockedBuffer is a bytes.Buffer that two writers can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
