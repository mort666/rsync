package rsyncwire

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncos"
)

// MSG_ERROR_EXIT ends a read with the peer's exit code; MSG_IO_TIMEOUT is
// skipped.
func TestMsgErrorExitAndIOTimeout(t *testing.T) {
	var buf bytes.Buffer
	w := &MultiplexWriter{Writer: nopCloseWriter{&buf}}
	c := &Conn{Mux: w}
	if err := c.WriteMsgInt(MsgIOTimeout, 60); err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("x"))
	if err := c.WriteMsgInt(MsgErrorExit, 23); err != nil {
		t.Fatal(err)
	}
	r := &MultiplexReader{Env: &rsyncos.Env{}, Reader: &buf}
	p := make([]byte, 10)
	if n, err := io.ReadAtLeast(r, p, 1); err != nil || string(p[:n]) != "x" {
		t.Fatalf("read %q, %v; want the data after MSG_IO_TIMEOUT", p[:n], err)
	}
	_, err := io.ReadAtLeast(r, p, 1)
	var exit *RemoteExitError
	if !errors.As(err, &exit) || exit.Code != 23 {
		t.Errorf("read = %v, want a RemoteExitError with code 23", err)
	}
}

// Delete stats between indices are read, kept, passed on when asked, and
// skipped by ReadNdxAndAttrs.
func TestDelStats(t *testing.T) {
	for _, forward := range []bool{false, true} {
		var in, out bytes.Buffer
		s := Session{Protocol: 31}
		wc := &Conn{Writer: nopCloseWriter{&in}, Session: s}
		want := DelStats{Files: 3, Dirs: 2, Symlinks: 1, Devices: 0, Specials: 70000}
		if err := wc.WriteDelStats(want); err != nil {
			t.Fatal(err)
		}
		if err := wc.WriteNdx(NdxDone); err != nil {
			t.Fatal(err)
		}
		rc := &Conn{Reader: io.NopCloser(&in), Writer: nopCloseWriter{&out}, Session: s, ForwardDelStats: forward}
		ndx, _, err := rc.ReadNdxAndAttrs(5)
		if err != nil || ndx != NdxDone {
			t.Fatalf("ReadNdxAndAttrs = %d, %v; want NdxDone", ndx, err)
		}
		if !rc.GotDelStats || rc.DelStats != want || rc.DelStats.Total() != 70006 {
			t.Errorf("DelStats = %+v (%v), want %+v", rc.DelStats, rc.GotDelStats, want)
		}
		// Reading never writes: the sender forwards later.
		if out.Len() != 0 {
			t.Errorf("ReadNdxAndAttrs wrote %d bytes", out.Len())
		}
		if !forward {
			continue
		}
		if err := rc.WriteDelStats(rc.DelStats); err != nil {
			t.Fatal(err)
		}
		// What is forwarded reads back the same.
		fc := &Conn{Reader: io.NopCloser(&out), Session: s}
		if ndx, err := fc.ReadNdx(); err != nil || ndx != NdxDelStats {
			t.Fatalf("forwarded ndx = %d, %v", ndx, err)
		}
		if got, err := fc.readDelStats(); err != nil || got != want {
			t.Errorf("forwarded stats = %+v, %v; want %+v", got, err, want)
		}
	}
}

// MSG_BLOCK_STATS carries a count from protocol 33, and is an error below
// it or with the wrong length.
func TestMsgBlockStats(t *testing.T) {
	for _, tt := range []struct {
		proto   int32
		payload []byte
		wantErr bool
	}{
		{33, []byte{0x39, 0x30, 0, 0, 1, 0, 0, 0}, false},
		{32, make([]byte, 8), true},
		{33, make([]byte, 4), true},
	} {
		var buf bytes.Buffer
		w := &MultiplexWriter{Writer: nopCloseWriter{&buf}}
		w.WriteMsg(MsgBlockStats, tt.payload)
		w.Write([]byte("x"))
		env := &rsyncos.Env{}
		r := &MultiplexReader{Env: env, Reader: &buf, Protocol: tt.proto}
		p := make([]byte, 10)
		n, err := io.ReadAtLeast(r, p, 1)
		if tt.wantErr {
			if err == nil {
				t.Errorf("protocol %d, %d bytes: read succeeded", tt.proto, len(tt.payload))
			}
			continue
		}
		if err != nil || string(p[:n]) != "x" {
			t.Fatalf("read %q, %v; want the data after MSG_BLOCK_STATS", p[:n], err)
		}
		if got, want := env.BlockStats.Load(), int64(1<<32+12345); got != want {
			t.Errorf("BlockStats = %d, want %d", got, want)
		}
	}
}
