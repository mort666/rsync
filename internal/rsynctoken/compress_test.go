package rsynctoken

import (
	"io"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// TestNamesKeepUpstreamOrder guards the lists both sides rank by: leaving
// names out is fine, reordering them is not.
func TestNamesKeepUpstreamOrder(t *testing.T) {
	upstream := []string{"zstd", "lz4", "zlibx", "zlib", "none"}
	for _, list := range [][]string{Names} {
		i := 0
		for _, n := range list {
			for i < len(upstream) && upstream[i] != n {
				i++
			}
			if i == len(upstream) {
				t.Errorf("%q is not in upstream order %q", list, upstream)
			}
		}
	}
}

// connPair connects a client and a server Conn with pipes.
func connPair(flags uint32) (client, server *rsyncwire.Conn) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	s := rsyncwire.Session{Protocol: 30, CompatFlags: flags}
	return &rsyncwire.Conn{Reader: cr, Writer: cw, Session: s}, &rsyncwire.Conn{Reader: sr, Writer: sw, Session: s}
}

func TestNegotiate(t *testing.T) {
	for _, tt := range []struct {
		name           string
		flags          uint32
		choice         string
		level          int
		env            string
		want           string
		wantLevel      int
		wantClientFail bool
	}{
		// Two gokrazy/rsyncs agree on zstd, which both send and receive.
		{name: "negotiated", flags: rsyncwire.CfVarintFlistFlags, choice: "auto", level: -1, want: "zstd", wantLevel: 3},
		{name: "no negotiation", choice: "auto", level: -1, want: "zlib", wantLevel: 6},
		{name: "env", flags: rsyncwire.CfVarintFlistFlags, choice: "auto", level: 9, env: "zlibx zlib", want: "zlibx", wantLevel: 9},
		{name: "level 0 turns zlib off", choice: "auto", level: 0, want: ""},
		{name: "choice", flags: rsyncwire.CfVarintFlistFlags, choice: "zlibx", level: 12, want: "zlibx", wantLevel: 9},
		{name: "none", choice: "", level: 5, want: ""},
		{name: "zstd", choice: "zstd", level: 19, want: "zstd", wantLevel: 19},
		{name: "unknown", choice: "brotli", level: -1, wantClientFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RSYNC_COMPRESS_LIST", tt.env)
			client, server := connPair(tt.flags)
			done := make(chan error, 1)
			go func() {
				err := Negotiate(server, tt.choice, tt.level, true)
				if err != nil {
					server.Reader.Close()
				}
				done <- err
			}()
			err := Negotiate(client, tt.choice, tt.level, false)
			if tt.wantClientFail {
				if err == nil {
					t.Fatalf("client: no error")
				}
				return
			}
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("server: %v", err)
			}
			for _, c := range []*rsyncwire.Conn{client, server} {
				if c.Session.Compress != tt.want || c.Session.CompressLevel != tt.wantLevel {
					t.Errorf("negotiated %q level %d, want %q level %d", c.Session.Compress, c.Session.CompressLevel, tt.want, tt.wantLevel)
				}
			}
		})
	}
}

func TestCompressionLevel(t *testing.T) {
	for _, tt := range []struct {
		name        string
		level, want int
	}{
		{"zlib", -1, 6}, {"zlib", 0, -1}, {"zlib", 1, 1}, {"zlibx", 12, 9},
		{"zstd", -1, 3}, {"zstd", 0, 3}, {"zstd", 19, 19}, {"zstd", 30, 22},
		{"lz4", 5, 0},
	} {
		if got := compressionLevel(tt.name, tt.level); got != tt.want {
			t.Errorf("compressionLevel(%s, %d) = %d, want %d", tt.name, tt.level, got, tt.want)
		}
	}
}
