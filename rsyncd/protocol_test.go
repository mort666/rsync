package rsyncd_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/testlogger"
	"github.com/gokrazy/rsync/rsyncclient"
	"github.com/gokrazy/rsync/rsyncd"
)

func protocolTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runProtocolClient dry-runs a gokrazy client with args against mod/ on the
// daemon at addr.
func runProtocolClient(t *testing.T, addr string, args ...string) error {
	t.Helper()
	client, err := rsyncclient.New(append([]string{"-n", "-r"}, args...), rsyncclient.WithStdout(io.Discard), rsyncclient.WithStderr(testlogger.New(t)), rsyncclient.DontRestrict())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = client.RunDaemon(t.Context(), conn, "mod/", []string{t.TempDir()})
	return err
}

// The gokrazy client's --protocol bounds what it announces, against both a
// gokrazy and a stock daemon.
func TestClientProtocolOption(t *testing.T) {
	dir := protocolTestDir(t)
	ts := rsynctest.New(t, []rsyncd.Module{{Name: "mod", Path: dir}})
	servers := map[string]string{"gokrazy": net.JoinHostPort("localhost", ts.Port)}
	if _, err := exec.LookPath("rsync"); err == nil {
		servers["stock"] = startStockDaemon(t, dir)
	}
	for name, addr := range servers {
		t.Run(name, func(t *testing.T) {
			for v := int32(rsyncwire.MinProtocolVersion); v <= rsyncwire.MaxProtocolVersion; v++ {
				if err := runProtocolClient(t, addr, fmt.Sprintf("--protocol=%d", v)); err != nil {
					t.Errorf("--protocol=%d: %v", v, err)
				}
			}
			for arg, want := range map[string]string{
				"--protocol=26": "--protocol must be at least 27 on the Client",
				fmt.Sprintf("--protocol=%d", rsyncwire.MaxProtocolVersion+1): fmt.Sprintf("--protocol must be no more than %d on the Client", rsyncwire.MaxProtocolVersion),
			} {
				err := runProtocolClient(t, addr, arg)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("%s: got error %v, want %q", arg, err, want)
				}
			}
		})
	}
}

// daemonGreet sends greeting to a gokrazy daemon, asks for mod and returns
// the first line the daemon answers with after its own greeting.
func daemonGreet(t *testing.T, greeting string) string {
	t.Helper()
	ts := rsynctest.New(t, []rsyncd.Module{{Name: "mod", Path: protocolTestDir(t)}})
	conn, err := net.Dial("tcp", net.JoinHostPort("localhost", ts.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rd := bufio.NewReader(conn)
	if line, err := rd.ReadString('\n'); err != nil || !strings.HasPrefix(line, fmt.Sprintf("@RSYNCD: %d.0 ", rsyncwire.MaxProtocolVersion)) {
		t.Fatalf("server greeting: %q, %v", line, err)
	}
	if _, err := io.WriteString(conn, greeting+"mod\n"); err != nil {
		t.Fatal(err)
	}
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func TestDaemonGreeting(t *testing.T) {
	for _, tt := range []struct {
		greeting string
		want     string
	}{
		{"@RSYNCD: 27\n", "@RSYNCD: OK"},
		// rsync 3.0 (protocol 30) adds a sub-protocol, 3.2.7 a digest list.
		{"@RSYNCD: 30.0\n", "@RSYNCD: OK"},
		{"@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n", "@RSYNCD: OK"},
		{"@RSYNCD: 26\n", "@ERROR: peer speaks protocol version 26, older than the oldest gokrazy/rsync supports (27)"},
		{"@RSYNCD: 99.0 md5\n", "@ERROR: protocol version mismatch"},
		// From 30 rsync refuses a greeting without the sub-protocol.
		{"@RSYNCD: 30\n", "@ERROR: your client omitted the subprotocol value"},
		{"@RSYNCD: x\n", "@ERROR: protocol startup error"},
	} {
		if got := daemonGreet(t, tt.greeting); !strings.HasPrefix(got, tt.want) {
			t.Errorf("greeting %q: daemon answered %q, want %q", tt.greeting, got, tt.want)
		}
	}
}

// Stock rsync with --protocol=27 talks to a gokrazy daemon. (It announces 27
// in its greeting instead of its own maximum.)
func TestStockClientProtocol27(t *testing.T) {
	bin := rsynctest.TridgeOrGTFO(t, "runs the stock rsync client")
	dir := protocolTestDir(t)
	ts := rsynctest.New(t, []rsyncd.Module{{Name: "mod", Path: dir}})
	cmd := exec.Command(bin, "--protocol=27", "-n", "-r", "rsync://localhost:"+ts.Port+"/mod/", t.TempDir()+"/")
	cmd.Stdout = testlogger.New(t)
	cmd.Stderr = testlogger.New(t)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}
