package rsynctoken

import (
	"bufio"
	"bytes"
	"compress/flate"
	"fmt"
	"io"

	kflate "github.com/klauspost/compress/flate"
)

// zlib and zlibx (send_deflated_token, recv_deflated_token,
// see_deflate_token).
//
// rsync keeps one deflate stream per file. For zlib, both sides also put
// each matched block into the deflate history (Z_INSERT_ONLY on the sender,
// a stored block run through inflate on the receiver), so later literal
// data can refer back to it; zlibx leaves matched blocks out. Every literal
// run ends in Z_SYNC_FLUSH, which leaves the stream byte aligned, and the
// stream never sets BFINAL. Go's deflate packages cannot add data to a
// running stream's history, but they can start one with a preset
// dictionary, so both sides here restart at each literal run with the last
// 32 KiB of history. The result is the stream rsync itself produces and
// reads.

const windowSize = 32 * 1024

// defaultDictLimit is how much history the encoder primes each run with.
// Priming costs as much as compressing the bytes, so with frequent restarts
// a short dictionary is much faster for slightly larger output. The stream
// stays valid either way: the receiver's window holds the full history.
const defaultDictLimit = 16 * 1024

// history is the shared deflate window: the last 32 KiB the receiver's
// inflater has seen, in order.
type history struct {
	buf []byte
}

func (h *history) reset() { h.buf = h.buf[:0] }

func (h *history) add(p []byte) {
	if len(p) >= windowSize {
		h.buf = append(h.buf[:0], p[len(p)-windowSize:]...)
		return
	}
	h.buf = append(h.buf, p...)
	if len(h.buf) > 4*windowSize {
		h.buf = append(h.buf[:0], h.buf[len(h.buf)-windowSize:]...)
	}
}

func (h *history) bytes() []byte {
	if len(h.buf) > windowSize {
		return h.buf[len(h.buf)-windowSize:]
	}
	return h.buf
}

// maxPiece is how much of a matched block rsync puts into the history at a
// time.
const maxPiece = 0xffff

// blockWriter puts a matched block of n bytes into the history, written to
// it in pieces of any size, the way rsync does: in pieces of up to 0xffff
// bytes. Below protocol 31 rsync forgets to advance through the block (the
// "data duplicating bug" fixed in 31), so each piece repeats the start of
// the block; only the block's first 0xffff bytes matter then.
type blockWriter struct {
	h     *history
	proto int32
	n     int64
	seen  int64
	first []byte // below protocol 31: the block's first piece
}

func (bw *blockWriter) Write(p []byte) (int, error) {
	if bw.proto >= 31 {
		bw.h.add(p)
		return len(p), nil
	}
	if keep := min(int64(maxPiece), bw.n) - bw.seen; keep > 0 {
		bw.first = append(bw.first, p[:min(int64(len(p)), keep)]...)
	}
	bw.seen += int64(len(p))
	if bw.seen >= bw.n {
		// Only the last two pieces can reach into the last 32 KiB.
		pieces := (bw.n + maxPiece - 1) / maxPiece
		for i := max(0, pieces-2); i < pieces; i++ {
			bw.h.add(bw.first[:min(int64(maxPiece), bw.n-i*maxPiece)])
		}
	}
	return len(p), nil
}

// deflateEncoder is send_deflated_token.
type deflateEncoder struct {
	runWriter
	zlibx bool // matched blocks stay out of the history
	proto int32
	level int

	fw    *kflate.Writer
	inRun bool
	hist  history
	out   bytes.Buffer
	bw    blockWriter

	dictLimit int
}

func newDeflateEncoder(w io.Writer, zlibx bool, proto int32, level int) *deflateEncoder {
	e := &deflateEncoder{
		runWriter: runWriter{w: bufio.NewWriterSize(w, 64*1024)},
		zlibx:     zlibx,
		proto:     proto,
		level:     level,
		dictLimit: defaultDictLimit,
	}
	e.reset()
	return e
}

var syncMarker = []byte{0, 0, 0xff, 0xff}

func (e *deflateEncoder) SendToken(token int32, lit, block []byte) error {
	if e.token(token, len(lit)) {
		e.inRun = false
		e.hist.reset()
	}
	if len(lit) != 0 || e.flushPending {
		if !e.inRun {
			dict := e.hist.bytes()
			if len(dict) > e.dictLimit {
				dict = dict[len(dict)-e.dictLimit:]
			}
			if e.fw == nil {
				fw, err := kflate.NewWriterDict(&e.out, e.level, dict)
				if err != nil {
					return err
				}
				e.fw = fw
			} else {
				e.fw.ResetDict(&e.out, dict)
			}
			e.inRun = true
		}
		if _, err := e.fw.Write(lit); err != nil {
			return err
		}
		e.hist.add(lit)
		if token != -2 {
			if err := e.fw.Flush(); err != nil {
				return err
			}
			b := e.out.Bytes()
			if !bytes.HasSuffix(b, syncMarker) {
				return fmt.Errorf("deflate flush did not end in a sync marker")
			}
			e.out.Truncate(len(b) - len(syncMarker))
			e.inRun = false
		}
		e.data(e.out.Bytes())
		e.out.Reset()
		e.flushPending = token == -2
	}
	if token >= 0 && !e.zlibx {
		e.bw = blockWriter{h: &e.hist, proto: e.proto, n: int64(len(block)), first: e.bw.first[:0]}
		e.bw.Write(block)
	}
	return e.end(token)
}

// inflateInput feeds one literal run to the inflater: the payloads of
// consecutive DEFLATED_DATA frames. When the next flag is anything else, it
// keeps that flag for the token parser, hands out the sync marker the
// sender stripped and an empty final stored block, and then io.EOF, so the
// inflater ends cleanly without reading the next token.
type inflateInput struct {
	d       *deflateDecoder
	first   int // flag byte the decoder already read, or -1
	pending []byte
	done    bool
}

var runTerminator = []byte{0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}

func (in *inflateInput) Read(p []byte) (int, error) {
	for len(in.pending) == 0 {
		if in.done {
			return 0, io.EOF
		}
		var flag byte
		if in.first >= 0 {
			flag = byte(in.first)
			in.first = -1
		} else {
			b, err := in.d.r.ReadByte()
			if err != nil {
				return 0, noEOF(err)
			}
			flag = b
		}
		if flag&0xc0 == deflatedData {
			var err error
			if in.pending, err = in.d.readData(flag, in.d.cbuf); err != nil {
				return 0, err
			}
			continue
		}
		in.d.savedFlag = int(flag) | 0x100
		in.pending = runTerminator
		in.done = true
	}
	n := copy(p, in.pending)
	in.pending = in.pending[n:]
	return n, nil
}

// ReadByte keeps flate from wrapping the input in a bufio.Reader, which
// would read past the end of the run.
func (in *inflateInput) ReadByte() (byte, error) {
	var b [1]byte
	if _, err := in.Read(b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// deflateDecoder is recv_deflated_token and see_deflate_token.
type deflateDecoder struct {
	runReader
	zlibx bool
	proto int32

	fr        io.ReadCloser
	in        inflateInput
	inRun     bool
	newFile   bool
	savedFlag int // flag | 0x100 when set

	hist history
	bw   blockWriter
	cbuf []byte
	dbuf []byte
}

func newDeflateDecoder(r io.Reader, zlibx bool, proto int32) *deflateDecoder {
	return &deflateDecoder{
		runReader: runReader{r: asReader(r)},
		zlibx:     zlibx,
		proto:     proto,
		newFile:   true,
		cbuf:      make([]byte, maxDataCount),
		dbuf:      make([]byte, windowSize),
	}
}

func (d *deflateDecoder) Next() ([]byte, int32, error) {
	if d.newFile {
		d.newFile = false
		d.runReader.reset()
		d.hist.reset()
	}
	for {
		if tok, ok := d.running(); ok {
			return nil, tok, nil
		}
		if d.inRun {
			n, err := d.fr.Read(d.dbuf)
			if n > 0 {
				d.hist.add(d.dbuf[:n])
				return d.dbuf[:n], -1, nil
			}
			if err == io.EOF {
				d.inRun = false
				continue
			}
			if err != nil {
				return nil, -1, fmt.Errorf("inflate: %w", err)
			}
			continue
		}

		var flag byte
		if d.savedFlag != 0 {
			flag = byte(d.savedFlag)
			d.savedFlag = 0
		} else {
			b, err := d.r.ReadByte()
			if err != nil {
				return nil, -1, noEOF(err)
			}
			flag = b
		}

		if flag&0xc0 == deflatedData {
			// The start of a literal run: hand the flag to the run's input
			// and restart the inflater with the shared history.
			d.in = inflateInput{d: d, first: int(flag)}
			if d.fr == nil {
				d.fr = flate.NewReaderDict(&d.in, d.hist.bytes())
			} else if err := d.fr.(flate.Resetter).Reset(&d.in, d.hist.bytes()); err != nil {
				return nil, -1, err
			}
			d.inRun = true
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

func (d *deflateDecoder) See(n int64) io.Writer {
	if d.zlibx {
		return nil
	}
	d.bw = blockWriter{h: &d.hist, proto: d.proto, n: n, first: d.bw.first[:0]}
	return &d.bw
}
