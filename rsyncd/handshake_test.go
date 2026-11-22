package rsyncd

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// A client that stalls in the handshake is cut off (CVE-2026-70464).
func TestHandshakeDeadline(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	srv, err := NewServer([]Module{{Name: "mod", Path: t.TempDir()}}, WithStderr(nopWriteCloser{io.Discard}), DontRestrict())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)

	for _, stall := range []string{
		"",                      // no greeting at all
		"@RSYNCD: 31.0",         // a greeting without its newline
		"@RSYNCD: 31.0\nmod\n-", // part of the first argument
	} {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(c, stall); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		start := time.Now()
		// Read the server's greeting and whatever follows until it hangs up.
		_, err = io.Copy(io.Discard, bufio.NewReader(c))
		c.Close()
		if err != nil {
			t.Errorf("after %q: the server did not hang up within 5s: %v", stall, err)
		} else if d := time.Since(start); d > 3*time.Second {
			t.Errorf("after %q: the server hung up after %v", stall, d)
		}
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
