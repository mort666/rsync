package rsyncwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// intByteExtra is rsync's io.c int_byte_extra: indexed by the first byte of
// a varint divided by 4, it gives how many more bytes follow.
var intByteExtra = [64]byte{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // (00 - 3F)/4
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // (40 - 7F)/4
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, // (80 - BF)/4
	2, 2, 2, 2, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 5, 6, // (C0 - FF)/4
}

// AppendVarint appends x in rsync's varint encoding (io.c:write_varint), used
// from protocol 30 on: 1 to 5 bytes, where the high bits of the first byte
// say how many bytes follow.
func AppendVarint(b []byte, x int32) []byte {
	var buf [5]byte
	binary.LittleEndian.PutUint32(buf[1:], uint32(x))
	cnt := 4
	for cnt > 1 && buf[cnt] == 0 {
		cnt--
	}
	bit := byte(1) << (7 - cnt + 1)
	switch {
	case buf[cnt] >= bit:
		cnt++
		buf[0] = ^(bit - 1)
	case cnt > 1:
		buf[0] = buf[cnt] | ^(bit*2 - 1)
	default:
		buf[0] = buf[1]
	}
	return append(b, buf[:cnt]...)
}

// ReadVarint reads a value written by AppendVarint (io.c:read_varint).
func ReadVarint(r io.Reader) (int32, error) {
	var ch [1]byte
	if _, err := io.ReadFull(r, ch[:]); err != nil {
		return 0, err
	}
	var u [5]byte
	extra := int(intByteExtra[ch[0]/4])
	if extra > 0 {
		if extra >= len(u) {
			return 0, fmt.Errorf("overflow in read_varint: first byte %#02x", ch[0])
		}
		if _, err := io.ReadFull(r, u[:extra]); err != nil {
			return 0, eofIsUnexpected(err)
		}
		bit := byte(1) << (8 - extra)
		u[extra] = ch[0] & (bit - 1)
	} else {
		u[0] = ch[0]
	}
	// For a 5-byte varint, u[4] holds bits beyond 32. rsync drops them too.
	return int32(binary.LittleEndian.Uint32(u[:4])), nil
}

// The minBytes values a varlong can use. rsync calls write_varlong with 3
// (file lengths, transfer stats) and 4 (times). Below 3, rsync's own encoder
// writes more bytes for large values than its decoder reads back.
const (
	minVarlongBytes = 3
	maxVarlongBytes = 8
)

func checkMinBytes(minBytes int) {
	if minBytes < minVarlongBytes || minBytes > maxVarlongBytes {
		panic(fmt.Sprintf("rsyncwire: varlong minBytes %d outside %d..%d", minBytes, minVarlongBytes, maxVarlongBytes))
	}
}

// AppendVarlong appends x in rsync's varlong encoding (io.c:write_varlong):
// at least minBytes bytes, at most 9. minBytes must be 3 to 8.
func AppendVarlong(b []byte, x int64, minBytes int) []byte {
	checkMinBytes(minBytes)
	var buf [9]byte
	binary.LittleEndian.PutUint64(buf[1:], uint64(x))
	cnt := 8
	for cnt > minBytes && buf[cnt] == 0 {
		cnt--
	}
	bit := byte(1) << (7 - cnt + minBytes)
	switch {
	case buf[cnt] >= bit:
		cnt++
		buf[0] = ^(bit - 1)
	case cnt > minBytes:
		buf[0] = buf[cnt] | ^(bit*2 - 1)
	default:
		buf[0] = buf[cnt]
	}
	return append(b, buf[:cnt]...)
}

// ReadVarlong reads a value written by AppendVarlong with the same minBytes
// (io.c:read_varlong).
func ReadVarlong(r io.Reader, minBytes int) (int64, error) {
	checkMinBytes(minBytes)
	var b2 [maxVarlongBytes]byte
	if _, err := io.ReadFull(r, b2[:minBytes]); err != nil {
		return 0, err
	}
	var u [9]byte
	copy(u[:], b2[1:minBytes])
	extra := int(intByteExtra[b2[0]/4])
	if extra > 0 {
		if minBytes+extra > len(u) {
			return 0, fmt.Errorf("overflow in read_varlong: first byte %#02x with minBytes %d", b2[0], minBytes)
		}
		if _, err := io.ReadFull(r, u[minBytes-1:minBytes-1+extra]); err != nil {
			return 0, eofIsUnexpected(err)
		}
		bit := byte(1) << (8 - extra)
		u[minBytes+extra-1] = b2[0] & (bit - 1)
	} else {
		u[minBytes-1] = b2[0]
	}
	return int64(binary.LittleEndian.Uint64(u[:8])), nil
}

// A stream that ends inside a value is truncated, not cleanly finished.
func eofIsUnexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (c *Conn) WriteVarint(x int32) error {
	var buf [5]byte
	_, err := c.Writer.Write(AppendVarint(buf[:0], x))
	return err
}

func (c *Conn) ReadVarint() (int32, error) { return ReadVarint(c.Reader) }

func (c *Conn) WriteVarlong(x int64, minBytes int) error {
	var buf [9]byte
	_, err := c.Writer.Write(AppendVarlong(buf[:0], x, minBytes))
	return err
}

func (c *Conn) ReadVarlong(minBytes int) (int64, error) { return ReadVarlong(c.Reader, minBytes) }

// WriteVarint30 writes x as an int32 below protocol 30 and as a varint from 30
// on (io.h:write_varint30).
func (c *Conn) WriteVarint30(x int32) error {
	if c.Session.Protocol < 30 {
		return c.WriteInt32(x)
	}
	return c.WriteVarint(x)
}

func (c *Conn) ReadVarint30() (int32, error) {
	if c.Session.Protocol < 30 {
		return c.ReadInt32()
	}
	return c.ReadVarint()
}

// WriteVarlong30 writes x as rsync's longint below protocol 30 (see
// WriteInt64) and as a varlong from 30 on (io.h:write_varlong30).
func (c *Conn) WriteVarlong30(x int64, minBytes int) error {
	if c.Session.Protocol < 30 {
		return c.WriteInt64(x)
	}
	return c.WriteVarlong(x, minBytes)
}

func (c *Conn) ReadVarlong30(minBytes int) (int64, error) {
	if c.Session.Protocol < 30 {
		return c.ReadInt64()
	}
	return c.ReadVarlong(minBytes)
}

func (b *Buffer) WriteVarint(x int32) {
	var buf [5]byte
	b.buf.Write(AppendVarint(buf[:0], x))
}

func (b *Buffer) WriteVarlong(x int64, minBytes int) {
	var buf [9]byte
	b.buf.Write(AppendVarlong(buf[:0], x, minBytes))
}

// WriteVarint30 is Conn.WriteVarint30 for a buffer, which has no session.
func (b *Buffer) WriteVarint30(proto int32, x int32) {
	if proto < 30 {
		b.WriteInt32(x)
		return
	}
	b.WriteVarint(x)
}

// WriteVarlong30 is Conn.WriteVarlong30 for a buffer.
func (b *Buffer) WriteVarlong30(proto int32, x int64, minBytes int) {
	if proto < 30 {
		b.WriteInt64(x)
		return
	}
	b.WriteVarlong(x, minBytes)
}
