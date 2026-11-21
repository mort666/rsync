package rsyncwire

import (
	"fmt"
	"io"
	"math"
)

// File-list index values with special meanings (rsync.h).
const (
	NdxDone        = -1
	NdxFlistEOF    = -2
	NdxDelStats    = -3
	NdxFlistOffset = -101
)

// ndxState is the previous positive and negative index one direction sent,
// which protocol 30 encodes the next index against. rsync starts them at -1
// and 1.
type ndxState struct {
	started      bool
	prevPositive int32
	prevNegative int32
}

func (s *ndxState) init() {
	if !s.started {
		s.prevPositive, s.prevNegative, s.started = -1, 1, true
	}
}

// NdxWriter encodes the file-list indexes one side sends from protocol 30
// on (io.c:write_ndx). Each direction of a connection needs its own, and it
// must not be shared between goroutines.
type NdxWriter struct{ s ndxState }

// AppendNdx appends ndx, delta-encoded against the previous index of the same
// sign: one byte for a small step forward, 0xFE and two bytes for a larger
// one, 0xFE and four bytes otherwise. Negative indexes get a 0xFF prefix, and
// NdxDone is a single 0.
func (w *NdxWriter) AppendNdx(b []byte, ndx int32) []byte {
	w.s.init()
	var diff int32
	switch {
	case ndx >= 0:
		diff = ndx - w.s.prevPositive
		w.s.prevPositive = ndx
	case ndx == NdxDone:
		return append(b, 0)
	default:
		b = append(b, 0xFF)
		ndx = -ndx
		diff = ndx - w.s.prevNegative
		w.s.prevNegative = ndx
	}
	switch {
	case diff < 0xFE && diff > 0:
		return append(b, byte(diff))
	case diff < 0 || diff > 0x7FFF:
		return append(b, 0xFE, byte(ndx>>24)|0x80, byte(ndx), byte(ndx>>8), byte(ndx>>16))
	default:
		return append(b, 0xFE, byte(diff>>8), byte(diff))
	}
}

// NdxReader decodes what an NdxWriter on the other side encoded
// (io.c:read_ndx). Each direction of a connection needs its own, and it must
// not be shared between goroutines.
type NdxReader struct{ s ndxState }

func (rd *NdxReader) ReadNdx(r io.Reader) (int32, error) {
	rd.s.init()
	var b [4]byte
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return 0, err
	}
	prev := &rd.s.prevPositive
	switch b[0] {
	case 0xFF:
		if _, err := io.ReadFull(r, b[:1]); err != nil {
			return 0, eofIsUnexpected(err)
		}
		prev = &rd.s.prevNegative
	case 0:
		return NdxDone, nil
	}
	var unum uint32
	if b[0] == 0xFE {
		if _, err := io.ReadFull(r, b[:2]); err != nil {
			return 0, eofIsUnexpected(err)
		}
		if b[0]&0x80 != 0 {
			b[3] = b[0] &^ 0x80
			b[0] = b[1]
			if _, err := io.ReadFull(r, b[1:3]); err != nil {
				return 0, eofIsUnexpected(err)
			}
			unum = uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
		} else {
			unum = uint32(b[0])<<8 + uint32(b[1]) + uint32(*prev)
		}
	} else {
		unum = uint32(b[0]) + uint32(*prev)
	}
	// rsync 3.4 rejects an index that does not fit an int32 rather than
	// trusting every user of it to check.
	if unum > math.MaxInt32 {
		return 0, fmt.Errorf("invalid file index: %d", unum)
	}
	num := int32(unum)
	*prev = num
	if prev == &rd.s.prevNegative {
		num = -num
	}
	return num, nil
}

// WriteNdx writes a file-list index: an int32 below protocol 30, the
// delta encoding from 30 on.
func (c *Conn) WriteNdx(ndx int32) error {
	if c.Session.Protocol < 30 {
		return c.WriteInt32(ndx)
	}
	var buf [6]byte
	_, err := c.Writer.Write(c.ndxOut.AppendNdx(buf[:0], ndx))
	return err
}

// ReadNdx reads a file-list index written by the peer's WriteNdx.
func (c *Conn) ReadNdx() (int32, error) {
	if c.Session.Protocol < 30 {
		return c.ReadInt32()
	}
	return c.ndxIn.ReadNdx(c.Reader)
}
