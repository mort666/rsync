//go:build windows

package rsynctest

import (
	"net"
	"os/exec"
	"testing"
	"time"
)

// StockDaemon serves stock rsync's daemon, bin, on a loopback port and
// returns the address. args are the daemon's options (--config,
// --log-file, ...).
//
// A Go connection cannot be handed to a child as stdin here, so rsync
// listens itself, on a port picked free just before. Another process could
// take it in between; GitHub runs each Windows job on a machine of its
// own.
func StockDaemon(t *testing.T, bin string, args ...string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	l.Close()
	cmd := exec.Command(bin, append([]string{"--daemon", "--no-detach", "--port=" + port}, args...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return addr
		}
	}
	t.Fatal("stock rsync daemon did not come up")
	return ""
}
