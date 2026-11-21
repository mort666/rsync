package rsyncwire

import (
	"fmt"
	"io"
)

// Trace levels, as set by --debug=ioN.
const (
	TraceFrames  = 1 // multiplex frame headers: direction, tag, length
	TracePayload = 2 // and a hex prefix of each frame's payload
	TraceRaw     = 4 // and every raw read and write on the connection
)

// tracePrefix is how many bytes of a payload or raw buffer a trace line shows.
const tracePrefix = 32

// Trace logs what crosses a connection, for comparing a gokrazy/rsync session
// with upstream's --debug=io4,proto output. A nil *Trace logs nothing, so
// callers can use one unconditionally.
type Trace struct {
	Logf  func(format string, v ...any)
	Name  string // shown in brackets, e.g. "client" or "server"
	Level int
}

// NewTrace returns nil when level is 0, so that tracing costs a nil check.
func NewTrace(logf func(string, ...any), name string, level int) *Trace {
	if level <= 0 || logf == nil {
		return nil
	}
	return &Trace{Logf: logf, Name: name, Level: level}
}

func (t *Trace) on(level int) bool { return t != nil && t.Level >= level }

var tagNames = map[uint8]string{
	MsgData:        "DATA",
	MsgErrorXfer:   "ERROR_XFER",
	MsgInfo:        "INFO",
	MsgError:       "ERROR",
	MsgWarning:     "WARNING",
	MsgErrorSocket: "ERROR_SOCKET",
	MsgLog:         "LOG",
	MsgSuccess:     "SUCCESS",
	MsgDeleted:     "DELETED",
	MsgIOError:     "IO_ERROR",
	MsgNoop:        "NOOP",
	MsgNoSend:      "NO_SEND",
}

func tagName(tag uint8) string {
	if n, ok := tagNames[tag]; ok {
		return n
	}
	return fmt.Sprintf("UNKNOWN(%d)", tag)
}

func hexPrefix(p []byte) string {
	if len(p) <= tracePrefix {
		return fmt.Sprintf("% x", p)
	}
	return fmt.Sprintf("% x ...", p[:tracePrefix])
}

// Frame logs one multiplex frame. dir is "send" or "recv".
func (t *Trace) Frame(dir string, tag uint8, p []byte) {
	if !t.on(TraceFrames) {
		return
	}
	if t.on(TracePayload) && len(p) > 0 {
		t.Logf("[%s] %s msg=%s len=%d: %s", t.Name, dir, tagName(tag), len(p), hexPrefix(p))
		return
	}
	t.Logf("[%s] %s msg=%s len=%d", t.Name, dir, tagName(tag), len(p))
}

func (t *Trace) raw(dir string, p []byte, err error) {
	if err != nil {
		t.Logf("[%s] raw %s %d bytes, err=%v: %s", t.Name, dir, len(p), err, hexPrefix(p))
		return
	}
	t.Logf("[%s] raw %s %d bytes: %s", t.Name, dir, len(p), hexPrefix(p))
}

type traceReader struct {
	r io.Reader
	t *Trace
}

func (r *traceReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 || err != nil {
		r.t.raw("read", p[:n], err)
	}
	return n, err
}

type traceWriter struct {
	w io.Writer
	t *Trace
}

func (w *traceWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.t.raw("write", p[:n], err)
	return n, err
}

// WrapReader logs every read from r at TraceRaw, and returns r unchanged below
// that level.
func (t *Trace) WrapReader(r io.ReadCloser) io.ReadCloser {
	if !t.on(TraceRaw) {
		return r
	}
	return struct {
		io.Reader
		io.Closer
	}{&traceReader{r: r, t: t}, r}
}

// WrapWriter logs every write to w at TraceRaw, and returns w unchanged below
// that level.
func (t *Trace) WrapWriter(w io.WriteCloser) io.WriteCloser {
	if !t.on(TraceRaw) {
		return w
	}
	return struct {
		io.Writer
		io.Closer
	}{&traceWriter{w: w, t: t}, w}
}

// Wrap logs every read from and write to rwc at TraceRaw.
func (t *Trace) Wrap(rwc io.ReadWriteCloser) io.ReadWriteCloser {
	if !t.on(TraceRaw) {
		return rwc
	}
	return struct {
		io.Reader
		io.Writer
		io.Closer
	}{&traceReader{r: rwc, t: t}, &traceWriter{w: rwc, t: t}, rwc}
}

// WrapReadWriter is Wrap for a connection that is not closed through rw.
func (t *Trace) WrapReadWriter(rw io.ReadWriter) io.ReadWriter {
	if !t.on(TraceRaw) {
		return rw
	}
	return struct {
		io.Reader
		io.Writer
	}{&traceReader{r: rw, t: t}, &traceWriter{w: rw, t: t}}
}
