package rsynctoken

import (
	"encoding/binary"
	"fmt"
	"io"
)

// simpleEncoder is simple_send_token: no compression.
type simpleEncoder struct {
	w io.Writer
}

func (e *simpleEncoder) writeInt32(v int32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	_, err := e.w.Write(b[:])
	return err
}

func (e *simpleEncoder) SendToken(token int32, lit, _ []byte) error {
	for len(lit) > 0 {
		n := min(len(lit), chunkSize)
		if err := e.writeInt32(int32(n)); err != nil {
			return err
		}
		if _, err := e.w.Write(lit[:n]); err != nil {
			return err
		}
		lit = lit[n:]
	}
	if token == -2 {
		return nil
	}
	return e.writeInt32(-(token + 1))
}

// simpleDecoder is simple_recv_token.
type simpleDecoder struct {
	r    io.Reader
	left int // of the current literal token
	buf  []byte
}

func (d *simpleDecoder) Next() ([]byte, int32, error) {
	if d.left == 0 {
		var b [4]byte
		if _, err := io.ReadFull(d.r, b[:]); err != nil {
			return nil, -1, noEOF(err)
		}
		v := int32(binary.LittleEndian.Uint32(b[:]))
		switch {
		case v == 0:
			return nil, -1, io.EOF
		case v < 0:
			return nil, -(v + 1), nil
		case v > chunkSize:
			// rsync's CHUNK_SIZE is the longest literal token; rsync
			// rejects longer ones, and so does this.
			return nil, -1, fmt.Errorf("invalid uncompressed token length %d", v)
		}
		d.left = int(v)
	}
	if d.buf == nil {
		d.buf = make([]byte, chunkSize)
	}
	n, err := d.r.Read(d.buf[:d.left])
	if n == 0 && err != nil {
		return nil, -1, noEOF(err)
	}
	d.left -= n
	return d.buf[:n], -1, nil
}

func (d *simpleDecoder) See(int64) io.Writer { return nil }
