package rsyncwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/gokrazy/rsync/internal/rsyncos"
)

// Multiplex message tags, mirroring rsync.h's MSG_* constants. MSG_ERROR_XFER
// (1) and MSG_ERROR (3) are non-fatal logging messages from the peer in real
// rsync — losing one file's stat is not a reason to abort the whole transfer.
// MSG_ERROR_SOCKET (5) is a peer-side socket-level error and is fatal.
const (
	MsgData        uint8 = 0
	MsgErrorXfer   uint8 = 1
	MsgInfo        uint8 = 2
	MsgError       uint8 = 3
	MsgWarning     uint8 = 4
	MsgErrorSocket uint8 = 5
	MsgLog         uint8 = 6
	MsgBlockStats  uint8 = 11  // the receiver's count of 4 KiB blocks written (protocol 33+)
	MsgIOError     uint8 = 22  // the sending side had an I/O error (protocol 30+)
	MsgIOTimeout   uint8 = 33  // a daemon's --timeout, for its client (protocol 31+)
	MsgNoop        uint8 = 42  // a keep-alive (protocol 30)
	MsgErrorExit   uint8 = 86  // the peer is exiting with an error (protocol 31+)
	MsgSuccess     uint8 = 100 // successfully updated indicated flist index
	MsgDeleted     uint8 = 101 // successfully deleted a file on receiving side (protocol 29+)
	MsgNoSend      uint8 = 102 // the sender failed to open a file the generator wanted (protocol 30+)
)

// ioerrValidMask is rsync's IOERR_VALID_MASK: the io_error bits a peer may
// set (IOERR_GENERAL, IOERR_VANISHED, IOERR_DEL_LIMIT).
const ioerrValidMask = 1 | 2 | 4

const mplexBase = 7

type MultiplexWriter struct {
	Writer io.WriteCloser
	Trace  *Trace // optional

	// mu keeps frames whole: from protocol 30 more than one goroutine
	// writes to a multiplexed stream (data, and messages such as errors).
	mu sync.Mutex
}

func (w *MultiplexWriter) Write(p []byte) (n int, err error) {
	return w.WriteMsg(MsgData, p)
}

// maxFramePayload is the most one frame carries: what this package's
// reader accepts, which is far below the 24-bit length field.
const maxFramePayload = maxMessageSize

func (w *MultiplexWriter) WriteMsg(tag uint8, p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		chunk := p[n:]
		if len(chunk) > maxFramePayload {
			chunk = chunk[:maxFramePayload]
		}
		w.Trace.Frame("send", tag, chunk)
		header := uint32(mplexBase+tag)<<24 | uint32(len(chunk))
		if err := binary.Write(w.Writer, binary.LittleEndian, header); err != nil {
			return n, err
		}
		m, err := w.Writer.Write(chunk)
		n += m
		if err != nil || n == len(p) {
			return n, err
		}
	}
}

func (w *MultiplexWriter) Close() error { return w.Writer.Close() }

type MultiplexReader struct {
	Env    *rsyncos.Env
	Reader io.Reader
	Trace  *Trace // optional

	// Protocol is the session's protocol version, for messages that only
	// exist from some version on.
	Protocol int32
}

// rsync.h defines IO_BUFFER_SIZE as 32 * 1024, but gokr-rsyncd increases it to
// 256K. Since we use this as the maximum message size, too, we need to at least
// match it.
const ioBufferSize = 256 * 1024
const maxMessageSize = ioBufferSize

func (w *MultiplexReader) ReadMsg() (tag uint8, p []byte, err error) {
	var header uint32
	if err := binary.Read(w.Reader, binary.LittleEndian, &header); err != nil {
		return 0, nil, err
	}

	tag = uint8(header>>24) - mplexBase
	length := header & 0x00FFFFFF
	if length > maxMessageSize {
		// NOTE: if you run into this error, one alternative to bumping
		// maxMessageSize is to restructure the program to work with i/o buffer
		// windowing.
		return 0, nil, fmt.Errorf("length %d exceeds max message size (%d)", length, maxMessageSize)
	}
	p = make([]byte, int(length))
	if _, err := io.ReadFull(w.Reader, p); err != nil {
		return 0, nil, err
	}
	w.Trace.Frame("recv", tag, p)
	return tag, p, nil
}

func (w *MultiplexReader) Read(p []byte) (n int, err error) {
	tag, payload, err := w.ReadMsg()
	if err != nil {
		return 0, err
	}
	switch tag {
	case MsgErrorXfer, MsgError:
		w.Env.XferErrors.Add(1)
		fallthrough
	case MsgWarning, MsgLog:
		w.Env.Logf("rsync: %s", strings.TrimRight(string(payload), "\n"))
		// io.ReadFull will call Read again
		return 0, nil
	case MsgInfo:
		w.Env.Logf("info: %s", payload)
		return 0, nil
	case MsgErrorSocket:
		return 0, fmt.Errorf("rsync socket error: %s", strings.TrimRight(string(payload), "\n"))
	case MsgIOError:
		if len(payload) != 4 {
			return 0, fmt.Errorf("invalid MSG_IO_ERROR length %d", len(payload))
		}
		w.Env.IOErrors.Or(int32(binary.LittleEndian.Uint32(payload)) & ioerrValidMask)
		return 0, nil
	case MsgErrorExit:
		// The peer stops here with an error, which its other messages
		// explain, and waits for this side to close (cleanup.c,
		// io.c:read_a_msg). A message without a code answers one of ours,
		// which this side does not send.
		if len(payload) == 4 {
			return 0, &RemoteExitError{Code: int32(binary.LittleEndian.Uint32(payload))}
		}
		if len(payload) != 0 {
			return 0, fmt.Errorf("invalid MSG_ERROR_EXIT length %d", len(payload))
		}
		return 0, &RemoteExitError{}
	case MsgIOTimeout:
		// A daemon's --timeout, which the client may adopt. This side does
		// not time out on its own.
		if len(payload) != 4 {
			return 0, fmt.Errorf("invalid MSG_IO_TIMEOUT length %d", len(payload))
		}
		return 0, nil
	case MsgBlockStats:
		// A server receiver's count of the 4 KiB blocks it wrote, for a
		// sending client's --stats (io.c:read_a_msg).
		if len(payload) != 8 || w.Protocol < 33 {
			return 0, fmt.Errorf("invalid MSG_BLOCK_STATS (length %d, protocol %d)", len(payload), w.Protocol)
		}
		w.Env.BlockStats.Store(int64(binary.LittleEndian.Uint64(payload)))
		return 0, nil
	case MsgNoop:
		if len(payload) != 0 {
			return 0, fmt.Errorf("invalid MSG_NOOP length %d", len(payload))
		}
		return 0, nil
	case MsgNoSend:
		// The sender also reports why, as an error message. Without a redo
		// pass there is nothing more to do about the file.
		if len(payload) != 4 {
			return 0, fmt.Errorf("invalid MSG_NO_SEND length %d", len(payload))
		}
		return 0, nil
	case MsgDeleted:
		// From protocol 29 a server receiver names each file --delete
		// removed, for a client that asked for -v.
		dir := strings.HasSuffix(string(payload), "\x00") // a folder comes with its NUL
		name := strings.TrimRight(string(payload), "\x00")
		if w.Env.Deleted != nil {
			w.Env.Deleted(name, dir)
			return 0, nil
		}
		if dir {
			name += "/"
		}
		w.Env.Logf("deleting %s", name)
		return 0, nil
	case MsgSuccess:
		// The receiver has a file (--remove-source-files). openrsync
		// after 7c31e9525e89515401257621ddd7fb9f8019818d ("Massive
		// up-merge of Apple's openrsync") sends it after every file, so
		// without the option it is ignored.
		if len(payload) != 4 {
			return 0, fmt.Errorf("invalid MSG_SUCCESS length %d", len(payload))
		}
		if w.Env != nil && w.Env.SourceDone != nil {
			w.Env.SourceDone(int32(binary.LittleEndian.Uint32(payload)))
		}
		return 0, nil
	case MsgData:
		// continues below
	default:
		return 0, fmt.Errorf("unexpected tag: got %v, want %v", tag, MsgData)
	}
	if len(p) < len(payload) {
		panic(fmt.Sprintf("not enough buffer space! %d < %d", len(p), len(payload)))
	}
	return copy(p, payload), nil
}

type Buffer struct {
	// buf.Write() never fails, making for a convenient API.
	buf bytes.Buffer
}

func (b *Buffer) WriteByte(data byte) error {
	return binary.Write(&b.buf, binary.LittleEndian, data)
}

func (b *Buffer) WriteInt32(data int32) {
	binary.Write(&b.buf, binary.LittleEndian, data)
}

func (b *Buffer) WriteInt64(data int64) {
	// send as a 32-bit integer if possible
	if data <= 0x7FFFFFFF && data >= 0 {
		b.WriteInt32(int32(data))
		return
	}
	// otherwise, send -1 followed by the 64-bit integer
	b.WriteInt32(-1)
	binary.Write(&b.buf, binary.LittleEndian, data)
}

func (b *Buffer) WriteString(data string) {
	io.WriteString(&b.buf, data)
}

func (b *Buffer) String() string {
	return b.buf.String()
}

func (b *Buffer) Reset() {
	b.buf.Reset()
}

type Conn struct {
	Writer io.WriteCloser
	Reader io.ReadCloser

	// Session is set once the protocol version is negotiated.
	Session Session

	// Mux is this side's multiplexed output, when it has one: a server's
	// from the start, a client's from protocol 30. It carries messages
	// besides the data (see WriteMsg).
	Mux *MultiplexWriter

	// DelStats is what the peer's generator said --delete removed
	// (protocol 31+), once ReadNdxAndAttrs has read it (GotDelStats).
	// ForwardDelStats marks a server sender, which passes it on to its
	// client.
	DelStats        DelStats
	GotDelStats     bool
	ForwardDelStats bool

	// File-list index state for each direction (protocol 30+). The reading
	// and writing goroutines each touch only their own.
	ndxIn  NdxReader
	ndxOut NdxWriter
}

// WriteMsg sends a message other than data, such as MSG_IO_ERROR. It
// fails where this side's output is not multiplexed: a client below
// protocol 30.
func (c *Conn) WriteMsg(tag uint8, p []byte) error {
	if c.Mux == nil {
		return fmt.Errorf("cannot send message %d: output not multiplexed", tag)
	}
	_, err := c.Mux.WriteMsg(tag, p)
	return err
}

// WriteMsgInt sends a message whose payload is one 32-bit integer, as
// io.c:send_msg_int does.
func (c *Conn) WriteMsgInt(tag uint8, v int32) error {
	return c.WriteMsg(tag, binary.LittleEndian.AppendUint32(nil, uint32(v)))
}

func (c *Conn) Close() error {
	wcErr := c.Writer.Close()
	rcErr := c.Reader.Close()
	if wcErr != nil {
		return wcErr
	}
	return rcErr
}

func (c *Conn) WriteByte(data byte) error {
	return binary.Write(c.Writer, binary.LittleEndian, data)
}

func (c *Conn) WriteInt32(data int32) error {
	return binary.Write(c.Writer, binary.LittleEndian, data)
}

func (c *Conn) WriteInt64(data int64) error {
	// send as a 32-bit integer if possible
	if data <= 0x7FFFFFFF && data >= 0 {
		return c.WriteInt32(int32(data))
	}
	// otherwise, send -1 followed by the 64-bit integer
	if err := c.WriteInt32(-1); err != nil {
		return err
	}
	return binary.Write(c.Writer, binary.LittleEndian, data)
}

func (c *Conn) WriteString(data string) error {
	_, err := io.WriteString(c.Writer, data)
	return err
}

func (c *Conn) ReadByte() (byte, error) {
	var buf [1]byte
	if _, err := io.ReadFull(c.Reader, buf[:]); err != nil {
		return 0, err
	}
	return buf[0], nil
}

func (c *Conn) ReadInt32() (int32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(c.Reader, buf[:]); err != nil {
		return 0, err
	}
	return int32(binary.LittleEndian.Uint32(buf[:])), nil
}

func (c *Conn) ReadInt64() (int64, error) {
	{
		data, err := c.ReadInt32()
		if err != nil {
			return 0, err
		}
		if data != -1 {
			// The value was small enough to fit into a 32 bit int, so it was
			// transferred directly.
			return int64(data), nil
		}
		// Otherwise, -1 was transmitted, followed by the int64.
	}
	var data int64
	if err := binary.Read(c.Reader, binary.LittleEndian, &data); err != nil {
		return 0, err
	}
	return data, nil
}

type CountingReader struct {
	R         io.ReadCloser
	BytesRead int64
}

func (r *CountingReader) Read(p []byte) (n int, err error) {
	n, err = r.R.Read(p)
	r.BytesRead += int64(n)
	return n, err
}

func (r *CountingReader) Close() error { return r.R.Close() }

type CountingWriter struct {
	W            io.WriteCloser
	BytesWritten int64
}

func (w *CountingWriter) Write(p []byte) (n int, err error) {
	n, err = w.W.Write(p)
	w.BytesWritten += int64(n)
	return n, err
}

func (w *CountingWriter) Close() error { return w.W.Close() }

func CounterPair(r io.ReadCloser, w io.WriteCloser) (*CountingReader, *CountingWriter) {
	crd := &CountingReader{R: r}
	cwr := &CountingWriter{W: w}
	return crd, cwr
}

// RemoteExitError is the peer telling this side it exits with an error
// (MSG_ERROR_EXIT, protocol 31+). Code is its rsync exit code, such as 23
// for a partial transfer, or 0 when the message carried none.
type RemoteExitError struct {
	Code int32
}

func (e *RemoteExitError) Error() string {
	if e.Code == 0 {
		return "the remote rsync exited with an error"
	}
	return fmt.Sprintf("the remote rsync exited with error code %d", e.Code)
}
