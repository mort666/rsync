package rsynctoken

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstd (send_zstd_token, recv_zstd_token).
//
// rsync keeps one zstd stream for the whole session: one frame that never
// ends, with a flush at the end of each literal run, so everything sent
// before a block token or END_FLAG can be decoded in full.

type zstdEncoder struct {
	runWriter
	zw *zstd.Encoder
}

// frameWriter writes what the compressor produces as DEFLATED_DATA frames.
type frameWriter struct{ rw *runWriter }

func (f frameWriter) Write(p []byte) (int, error) {
	f.rw.data(p)
	return len(p), nil
}

func newZstdEncoder(w io.Writer, level int) (*zstdEncoder, error) {
	e := &zstdEncoder{runWriter: runWriter{w: bufio.NewWriterSize(w, 64*1024)}}
	e.reset()
	// One goroutine: the encoder then writes its output during Write and
	// Flush, never behind the token writer's back.
	zw, err := zstd.NewWriter(frameWriter{&e.runWriter},
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		// rsync's decoder takes windows up to 128 MiB
		// (ZSTD_WINDOWLOG_LIMIT_DEFAULT); stay well below.
		zstd.WithWindowSize(8<<20),
	)
	if err != nil {
		return nil, err
	}
	e.zw = zw
	return e, nil
}

func (e *zstdEncoder) SendToken(token int32, lit, _ []byte) error {
	e.token(token, len(lit))
	if len(lit) != 0 || e.flushPending {
		if _, err := e.zw.Write(lit); err != nil {
			return err
		}
		if token != -2 {
			if err := e.zw.Flush(); err != nil {
				return err
			}
		}
		e.flushPending = token == -2
	}
	return e.end(token)
}

// zstdDecoder reads the session's zstd stream. klauspost/compress/zstd
// pulls its input from a reader, and would read on past a flush into the
// tokens that follow, so it runs on a goroutine of its own and gets the
// DEFLATED_DATA payloads one at a time. After each payload, Next returns
// what the decoder makes of it until the decoder asks for more input: run
// synchronously (one goroutine), it asks only once it has handed over all
// it can decode, which after a flush is everything sent so far (the
// r_inflating state of recv_zstd_token).
type zstdDecoder struct {
	runReader
	newFile   bool
	inflating bool
	started   bool
	cbuf      []byte

	feed chan []byte   // a payload for the decoder
	need chan struct{} // the decoder has used all it got
	out  chan []byte   // decoded data
	errc chan error    // the decoder stopped
	done chan struct{} // closed by Close
	once sync.Once
}

func newZstdDecoder(r io.Reader) *zstdDecoder {
	return &zstdDecoder{
		runReader: runReader{r: asReader(r)},
		newFile:   true,
		cbuf:      make([]byte, maxDataCount),
		feed:      make(chan []byte),
		need:      make(chan struct{}),
		out:       make(chan []byte),
		errc:      make(chan error, 1),
		done:      make(chan struct{}),
	}
}

// zstdOutSize is how much the decoder hands over at a time:
// ZSTD_DStreamOutSize() * 2, as rsync's receiver uses.
const zstdOutSize = 2 * (128<<10 + 3)

// zstdMaxWindow is the largest window rsync's decoder takes
// (ZSTD_WINDOWLOG_LIMIT_DEFAULT, 2^27).
const zstdMaxWindow = 128 << 20

func (d *zstdDecoder) start() error {
	zr, err := zstd.NewReader(&zstdFeeder{d: d},
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(zstdMaxWindow),
		zstd.WithDecoderLowmem(true),
	)
	if err != nil {
		return err
	}
	d.started = true
	go d.run(zr)
	return nil
}

// run is the decoder's goroutine. It alternates between two buffers: the
// one Next returned last stays untouched until Next is called again, which
// is before it can receive the other.
func (d *zstdDecoder) run(zr *zstd.Decoder) {
	defer zr.Close()
	bufs := [2][]byte{make([]byte, zstdOutSize), make([]byte, zstdOutSize)}
	for i := 0; ; i ^= 1 {
		n, err := zr.Read(bufs[i])
		if n > 0 {
			select {
			case d.out <- bufs[i][:n]:
			case <-d.done:
				return
			}
		}
		if err != nil {
			d.errc <- err
			return
		}
	}
}

// zstdFeeder is the decoder's input: the payloads Next passes on. Only
// the decoder's goroutine uses it.
type zstdFeeder struct {
	d       *zstdDecoder
	pending []byte // what is left of the payload
}

func (f *zstdFeeder) Read(p []byte) (int, error) {
	d := f.d
	if len(f.pending) == 0 {
		// Next reuses the payload's buffer once it hears this.
		select {
		case d.need <- struct{}{}:
		case <-d.done:
			return 0, errZstdClosed
		}
		select {
		case f.pending = <-d.feed:
		case <-d.done:
			return 0, errZstdClosed
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

var errZstdClosed = errors.New("zstd decoder closed")

func (d *zstdDecoder) Next() ([]byte, int32, error) {
	if d.newFile {
		d.newFile = false
		d.runReader.reset()
	}
	for {
		if d.inflating {
			select {
			case out := <-d.out:
				return out, -1, nil
			case <-d.need:
				d.inflating = false
			case err := <-d.errc:
				return nil, -1, fmt.Errorf("zstd: %v", noEOF(err))
			}
			continue
		}
		if tok, ok := d.running(); ok {
			return nil, tok, nil
		}
		flag, err := d.r.ReadByte()
		if err != nil {
			return nil, -1, noEOF(err)
		}
		if flag&0xc0 == deflatedData {
			in, err := d.readData(flag, d.cbuf)
			if err != nil {
				return nil, -1, err
			}
			if len(in) == 0 {
				continue
			}
			if !d.started {
				if err := d.start(); err != nil {
					return nil, -1, err
				}
				// The decoder's first read.
				select {
				case <-d.need:
				case err := <-d.errc:
					return nil, -1, fmt.Errorf("zstd: %v", err)
				}
			}
			d.feed <- in
			d.inflating = true
			continue
		}
		if flag == endFlag {
			d.newFile = true
			return nil, -1, io.EOF
		}
		tok, err := d.token(flag)
		if err != nil {
			return nil, -1, err
		}
		return nil, tok, nil
	}
}

func (d *zstdDecoder) See(int64) io.Writer { return nil }

// Close stops the decoder's goroutine. The decoder is not usable after.
func (d *zstdDecoder) Close() error {
	d.once.Do(func() { close(d.done) })
	return nil
}
