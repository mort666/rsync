// Package rsynctoken is rsync's token.c: how the sender describes a file to
// the receiver as literal data and references to the receiver's blocks,
// with or without compression (-z).
//
// Every compressor shares one framing. A flag byte says what follows:
// DEFLATED_DATA (compressed literal data of up to 16,383 bytes), a block
// index relative to the last one (TOKEN_REL) or absolute (TOKEN_LONG),
// either one optionally followed by a count of consecutive blocks after it
// (TOKENRUN_REL, TOKENRUN_LONG), or END_FLAG at the end of a file. Without
// compression the stream is rsync's simple one: a positive int32 length and
// that much literal data, -(i+1) for block i, 0 at the end.
package rsynctoken

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Flag bytes (token.c).
const (
	endFlag      = 0x00 // end of file
	tokenLong    = 0x20 // followed by a 32-bit token number
	tokenRunLong = 0x21 // ditto, then a 16-bit run count
	deflatedData = 0x40 // + 6-bit high length, then the low length byte
	tokenRel     = 0x80 // + 6-bit relative token number
	tokenRunRel  = 0xc0 // ditto, then a 16-bit run count

	maxDataCount = 16383 // the longest DEFLATED_DATA payload

	// chunkSize is rsync's CHUNK_SIZE: the longest literal token of the
	// simple stream.
	chunkSize = 32 * 1024
)

// Encoder is send_token: it sends one file at a time, and keeps whatever
// state the compressor carries from file to file.
type Encoder interface {
	// SendToken sends lit, the literal data before token, then token: the
	// index of a matched block, -2 for literal data alone, or -1 at the end
	// of the file. block is the matched block's content when token >= 0;
	// zlib puts it into the compressor's history.
	SendToken(token int32, lit, block []byte) error
}

// Decoder is recv_token and see_token.
type Decoder interface {
	// Next returns literal data (lit != nil), the index of a matched block
	// (tok >= 0), or io.EOF at the end of the file. A stream that ends
	// before END_FLAG is io.ErrUnexpectedEOF. lit is valid until the next
	// call.
	Next() (lit []byte, tok int32, err error)

	// See returns where the caller writes the n bytes of the block Next
	// just returned, or nil when the compressor does not need them (only
	// zlib does).
	See(n int64) io.Writer
}

// errProtocol is a token stream no rsync sends.
var errProtocol = errors.New("protocol error in compressed token stream")

// noEOF turns a plain io.EOF from the connection into
// io.ErrUnexpectedEOF. io.EOF is reserved for END_FLAG, so a truncated
// stream never looks like the clean end of a file.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// byteReader adds ReadByte to a reader without reading ahead, as decoders
// must stop at the end of a file's tokens: what follows on the connection
// is not theirs.
type byteReader struct {
	io.Reader
	b [1]byte
}

func (r *byteReader) ReadByte() (byte, error) {
	if _, err := io.ReadFull(r.Reader, r.b[:]); err != nil {
		return 0, err
	}
	return r.b[0], nil
}

// reader is the connection as decoders read it.
type reader interface {
	io.Reader
	io.ByteReader
}

func asReader(r io.Reader) reader {
	if br, ok := r.(reader); ok {
		return br
	}
	return &byteReader{Reader: r}
}

// runWriter is the token half of every compressed encoder: it records
// consecutive matched blocks as runs and writes each run out when the next
// one starts (the shared top part of send_deflated_token,
// send_zstd_token and send_compressed_token).
type runWriter struct {
	w            *bufio.Writer
	lastToken    int32
	runStart     int32
	lastRunEnd   int32
	flushPending bool
}

// start of a file
func (rw *runWriter) reset() { rw.lastToken = -1 }

// token is called first by every SendToken. It reports whether the
// file starts here.
func (rw *runWriter) token(token int32, nb int) (first bool) {
	switch {
	case rw.lastToken == -1:
		rw.lastRunEnd = 0
		rw.runStart = token
		rw.flushPending = false
		first = true
	case rw.lastToken == -2:
		rw.runStart = token
	case nb != 0 || token != rw.lastToken+1 || token >= rw.runStart+65536:
		// Output the previous run.
		r := rw.runStart - rw.lastRunEnd
		n := rw.lastToken - rw.runStart
		if r >= 0 && r <= 63 {
			flag := byte(tokenRel)
			if n != 0 {
				flag = tokenRunRel
			}
			rw.w.WriteByte(flag + byte(r))
		} else {
			flag := byte(tokenLong)
			if n != 0 {
				flag = tokenRunLong
			}
			rw.w.WriteByte(flag)
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], uint32(rw.runStart))
			rw.w.Write(b[:])
		}
		if n != 0 {
			rw.w.WriteByte(byte(n))
			rw.w.WriteByte(byte(n >> 8))
		}
		rw.lastRunEnd = rw.lastToken
		rw.runStart = token
	}
	rw.lastToken = token
	return first
}

// data writes p as DEFLATED_DATA frames.
func (rw *runWriter) data(p []byte) {
	for len(p) > 0 {
		n := min(len(p), maxDataCount)
		rw.w.WriteByte(deflatedData + byte(n>>8))
		rw.w.WriteByte(byte(n))
		rw.w.Write(p[:n])
		p = p[n:]
	}
}

// end finishes a call: END_FLAG and a flush at the end of the file.
func (rw *runWriter) end(token int32) error {
	if token == -1 {
		rw.w.WriteByte(endFlag)
		return rw.w.Flush()
	}
	return nil
}

// runReader is the token half of every compressed decoder
// (recv_compressed_token_num and the r_running state).
type runReader struct {
	r       reader
	rxToken int32
	rxRun   int32
}

func (rr *runReader) reset() { rr.rxToken, rr.rxRun = 0, 0 }

// running returns the next block of a run, if one is under way.
func (rr *runReader) running() (int32, bool) {
	if rr.rxRun == 0 {
		return 0, false
	}
	rr.rxRun--
	rr.rxToken++
	return rr.rxToken, true
}

// token reads the rest of a token whose flag is flag.
func (rr *runReader) token(flag byte) (int32, error) {
	if flag&tokenRel != 0 {
		rr.rxToken += int32(flag & 0x3f)
		flag >>= 6
	} else {
		if flag&^1 != tokenLong {
			return 0, fmt.Errorf("%w: flag %#x", errProtocol, flag)
		}
		var b [4]byte
		if _, err := io.ReadFull(rr.r, b[:]); err != nil {
			return 0, noEOF(err)
		}
		t := int32(binary.LittleEndian.Uint32(b[:]))
		if t < 0 {
			return 0, errProtocol
		}
		rr.rxToken = t
	}
	if flag&1 != 0 {
		var b [2]byte
		if _, err := io.ReadFull(rr.r, b[:]); err != nil {
			return 0, noEOF(err)
		}
		rr.rxRun = int32(binary.LittleEndian.Uint16(b[:]))
		if rr.rxRun == 0 {
			return 0, errProtocol
		}
	}
	return rr.rxToken, nil
}

// readData reads a DEFLATED_DATA payload, whose flag is flag, into buf
// (at least maxDataCount bytes).
func (rr *runReader) readData(flag byte, buf []byte) ([]byte, error) {
	lo, err := rr.r.ReadByte()
	if err != nil {
		return nil, noEOF(err)
	}
	n := int(flag&0x3f)<<8 | int(lo)
	if _, err := io.ReadFull(rr.r, buf[:n]); err != nil {
		return nil, noEOF(err)
	}
	return buf[:n], nil
}
