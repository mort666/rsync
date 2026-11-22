package rsynctoken

import (
	"bufio"
	"fmt"
	"io"

	"github.com/pierrec/lz4/v4"
)

// lz4 (send_compressed_token, recv_compressed_token). Each DEFLATED_DATA
// frame is one LZ4 block on its own, of at most maxDataCount bytes of
// input; nothing carries over from block to block.

// lz4OutSize is the receiver's output buffer:
// MAX(LZ4_compressBound(CHUNK_SIZE), MAX_DATA_COUNT+2).
var lz4OutSize = max(lz4.CompressBlockBound(chunkSize), maxDataCount+2)

type lz4Encoder struct {
	runWriter
	c   lz4.Compressor
	buf []byte
}

func newLZ4Encoder(w io.Writer) *lz4Encoder {
	e := &lz4Encoder{
		runWriter: runWriter{w: bufio.NewWriterSize(w, 64*1024)},
		buf:       make([]byte, lz4OutSize),
	}
	e.reset()
	return e
}

func (e *lz4Encoder) SendToken(token int32, lit, _ []byte) error {
	e.token(token, len(lit))
	// rsync also comes here with no data after a -2, and sends an empty
	// block, which its receiver would take for the end of the file
	// (match.c never does it). LZ4 holds nothing back, so there is nothing
	// to flush: send nothing.
	if len(lit) != 0 {
		for len(lit) > 0 {
			// Like rsync, halve the input until its block fits a frame.
			in := lit[:min(len(lit), maxDataCount)]
			for {
				n, err := e.c.CompressBlock(in, e.buf)
				if err != nil {
					return fmt.Errorf("lz4: %v", err)
				}
				if n <= maxDataCount {
					e.data(e.buf[:n])
					break
				}
				in = in[:len(in)/2]
			}
			lit = lit[len(in):]
		}
	}
	return e.end(token)
}

type lz4Decoder struct {
	runReader
	newFile bool
	cbuf    []byte
	dbuf    []byte
}

func newLZ4Decoder(r io.Reader) *lz4Decoder {
	return &lz4Decoder{
		runReader: runReader{r: asReader(r)},
		newFile:   true,
		cbuf:      make([]byte, maxDataCount),
		dbuf:      make([]byte, lz4OutSize),
	}
}

func (d *lz4Decoder) Next() ([]byte, int32, error) {
	if d.newFile {
		d.newFile = false
		d.runReader.reset()
	}
	for {
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
			n, err := lz4.UncompressBlock(in, d.dbuf)
			if err != nil {
				return nil, -1, fmt.Errorf("lz4: uncompress failed: %v", err)
			}
			if n == 0 {
				continue
			}
			return d.dbuf[:n], -1, nil
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

func (d *lz4Decoder) See(int64) io.Writer { return nil }
