package rsyncwire

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncos"
)

type nopCloseWriter struct{ io.Writer }

func (nopCloseWriter) Close() error { return nil }

func TestServerCompatFlags(t *testing.T) {
	for _, tt := range []struct {
		clientInfo string
		want       uint32
	}{
		{"", 0},
		{ClientCapabilities, CfSafeFlist | CfChksumSeedFix},
		// rsync 3.2+: everything it offers but f and C stays off,
		// including v (name negotiation) and i (incremental recursion).
		{".iLsfxCIvu", CfSafeFlist | CfChksumSeedFix},
		// rsync 3.0.9 offers no C: block sums keep the old seed order.
		{".iLsf", CfSafeFlist},
	} {
		if got := ServerCompatFlags(tt.clientInfo); got != tt.want {
			t.Errorf("ServerCompatFlags(%q) = %#x, want %#x", tt.clientInfo, got, tt.want)
		}
	}
}

func TestReadCompatFlags(t *testing.T) {
	for _, tt := range []struct {
		flags   int32
		wantErr string
	}{
		{CfSafeFlist | CfChksumSeedFix, ""},
		// rsync's server sets these whatever the client offered.
		{CfSafeFlist | CfSymlinkTimes | CfSymlinkIconv, ""},
		{CfIncRecurse, "not offered"},
		{CfVarintFlistFlags, "not offered"},
	} {
		var buf bytes.Buffer
		buf.Write(AppendVarint(nil, tt.flags))
		c := &Conn{Reader: io.NopCloser(&buf), Session: Session{Protocol: 30}}
		err := c.ReadCompatFlags()
		if tt.wantErr == "" && (err != nil || c.Session.CompatFlags != uint32(tt.flags)) {
			t.Errorf("flags %#x: %v, CompatFlags %#x", tt.flags, err, c.Session.CompatFlags)
		}
		if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("flags %#x: error %v, want %q", tt.flags, err, tt.wantErr)
		}
	}
	// Below 30 nothing is on the wire.
	c := &Conn{Reader: io.NopCloser(strings.NewReader("")), Session: Session{Protocol: 29}}
	if err := c.ReadCompatFlags(); err != nil {
		t.Errorf("protocol 29: %v", err)
	}
}

func TestGreeting(t *testing.T) {
	if got := Greeting(29); got != "@RSYNCD: 29\n" {
		t.Errorf("Greeting(29) = %q", got)
	}
	if got := Greeting(30); got != "@RSYNCD: 30.0\n" {
		t.Errorf("Greeting(30) = %q", got)
	}
}

// A payload longer than one frame goes as several, and concurrent writers
// never interleave inside a frame.
func TestMultiplexWriterFrames(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := &MultiplexWriter{Writer: nopCloseWriter{writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})}}
	big := bytes.Repeat([]byte{'x'}, maxFramePayload*2+10)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == 0 {
				w.Write(big)
				return
			}
			for range 100 {
				w.WriteMsg(MsgInfo, []byte("hello\n"))
			}
		}()
	}
	wg.Wait()

	r := &MultiplexReader{Env: &rsyncos.Env{}, Reader: &buf}
	var data, infos int
	for {
		tag, p, err := r.ReadMsg()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch tag {
		case MsgData:
			if len(p) > maxFramePayload {
				t.Fatalf("data frame of %d bytes, more than %d", len(p), maxFramePayload)
			}
			data += len(p)
		case MsgInfo:
			if string(p) != "hello\n" {
				t.Fatalf("info frame %q, want hello", p)
			}
			infos++
		}
	}
	if data != len(big) || infos != 700 {
		t.Errorf("read %d data bytes and %d info frames, want %d and 700", data, infos, len(big))
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestMsgIOError(t *testing.T) {
	var buf bytes.Buffer
	w := &MultiplexWriter{Writer: nopCloseWriter{&buf}}
	c := &Conn{Mux: w}
	if err := c.WriteMsgInt(MsgIOError, 2); err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("x"))
	env := &rsyncos.Env{}
	env.IOErrors.Store(1)
	r := &MultiplexReader{Env: env, Reader: &buf}
	p := make([]byte, 10)
	n, err := io.ReadAtLeast(r, p, 1)
	if err != nil || string(p[:n]) != "x" {
		t.Fatalf("read %q, %v", p[:n], err)
	}
	if got := env.IOErrors.Load(); got != 3 {
		t.Errorf("IOErrors = %d, want 3 (1 | 2)", got)
	}
	if err := (&Conn{}).WriteMsg(MsgIOError, nil); err == nil {
		t.Errorf("WriteMsg without a multiplexed output did not fail")
	}
}
