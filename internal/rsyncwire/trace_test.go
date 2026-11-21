package rsyncwire_test

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// roundTrip sends a data frame and an info frame through a traced writer and
// reads them back through a traced reader, returning the trace lines.
func roundTrip(t *testing.T, level int) []string {
	t.Helper()
	var lines []string
	logf := func(format string, v ...any) { lines = append(lines, fmt.Sprintf(format, v...)) }
	trace := rsyncwire.NewTrace(logf, "test", level)

	var buf bytes.Buffer
	w := &rsyncwire.MultiplexWriter{Writer: trace.WrapWriter(nopWriteCloser{&buf}), Trace: trace}
	payload := bytes.Repeat([]byte{0xab}, 40)
	if _, err := w.WriteMsg(rsyncwire.MsgData, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteMsg(rsyncwire.MsgInfo, []byte("hi")); err != nil {
		t.Fatal(err)
	}

	r := &rsyncwire.MultiplexReader{Reader: trace.WrapReader(io.NopCloser(&buf)), Trace: trace}
	for range 2 {
		if _, _, err := r.ReadMsg(); err != nil {
			t.Fatal(err)
		}
	}
	return lines
}

func TestTraceLevels(t *testing.T) {
	if got := roundTrip(t, 0); len(got) != 0 {
		t.Errorf("level 0 logged %q, want nothing", got)
	}

	want := []string{
		"[test] send msg=DATA len=40",
		"[test] send msg=INFO len=2",
		"[test] recv msg=DATA len=40",
		"[test] recv msg=INFO len=2",
	}
	if got := roundTrip(t, rsyncwire.TraceFrames); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("level 1:\n got %q\nwant %q", got, want)
	}

	got := roundTrip(t, rsyncwire.TracePayload)
	if len(got) != 4 {
		t.Fatalf("level 2 logged %d lines, want 4: %q", len(got), got)
	}
	// 40 bytes of payload show as a 32-byte prefix.
	if want := "[test] send msg=DATA len=40: " + strings.Repeat("ab ", 31) + "ab ..."; got[0] != want {
		t.Errorf("level 2 data frame:\n got %q\nwant %q", got[0], want)
	}
	if want := "[test] recv msg=INFO len=2: 68 69"; got[3] != want {
		t.Errorf("level 2 info frame:\n got %q\nwant %q", got[3], want)
	}

	got = roundTrip(t, rsyncwire.TraceRaw)
	var raw int
	for _, l := range got {
		if strings.HasPrefix(l, "[test] raw ") {
			raw++
		}
	}
	// Each WriteMsg is a header write and a payload write.
	if raw < 4 {
		t.Errorf("level 4 logged %d raw lines, want at least 4: %q", raw, got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "[test] raw write 4 bytes: 28 00 00 07") {
		t.Errorf("level 4 did not log the first frame header (40 bytes, tag DATA): %q", got)
	}
}

func TestNilTrace(t *testing.T) {
	var trace *rsyncwire.Trace
	trace.Frame("send", rsyncwire.MsgData, []byte("x"))
	w := nopWriteCloser{io.Discard}
	if got := trace.WrapWriter(w); got != w {
		t.Errorf("nil trace wrapped the writer")
	}
}
