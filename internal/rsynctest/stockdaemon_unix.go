//go:build !windows

package rsynctest

import (
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
)

// StockDaemon serves stock rsync's daemon, bin, on a loopback port and
// returns the address. args are the daemon's options (--config,
// --log-file, ...).
//
// The listener stays open, and each connection goes to an rsync started
// with it as stdin, which serves it as from inetd (clientserver.c:
// daemon_main). Picking a free port, closing it and starting rsync on it
// leaves a gap in which another process can take the port: with CI jobs
// sharing a runner's loopback, that happens.
func StockDaemon(t *testing.T, bin string, args ...string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		closed bool
		procs  = map[*os.Process]bool{}
		wg     sync.WaitGroup
	)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			f, err := c.(*net.TCPConn).File()
			c.Close()
			if err != nil {
				continue
			}
			cmd := exec.Command(bin, append([]string{"--daemon"}, args...)...)
			cmd.Stdin = f
			cmd.Stdout = f
			err = cmd.Start()
			f.Close()
			if err != nil {
				continue
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				cmd.Process.Kill()
				cmd.Wait()
				continue
			}
			procs[cmd.Process] = true
			wg.Add(1)
			mu.Unlock()
			go func() {
				defer wg.Done()
				cmd.Wait()
				mu.Lock()
				delete(procs, cmd.Process)
				mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		closed = true
		for p := range procs {
			p.Kill()
		}
		mu.Unlock()
		wg.Wait()
	})
	return l.Addr().String()
}
