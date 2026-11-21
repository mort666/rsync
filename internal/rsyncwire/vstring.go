package rsyncwire

import (
	"fmt"
	"io"
)

// maxVstringLen is the longest string a vstring can carry: two bytes of
// length with the top bit as the marker (io.c:write_vstring).
const maxVstringLen = 0x7fff

// AppendVstring appends s with rsync's vstring length prefix
// (io.c:write_vstring): one byte for up to 127 bytes, otherwise two, the
// first with its top bit set. It is used from protocol 29 for the alternate
// basis name after a file index, and from 30 for negotiation lists.
func AppendVstring(b []byte, s string) []byte {
	if len(s) > maxVstringLen {
		panic(fmt.Sprintf("rsyncwire: vstring of %d bytes is longer than %d", len(s), maxVstringLen))
	}
	if len(s) > 0x7f {
		b = append(b, byte(len(s)>>8)|0x80, byte(len(s)))
	} else {
		b = append(b, byte(len(s)))
	}
	return append(b, s...)
}

// ReadVstring reads a string written by AppendVstring
// (io.c:read_vstring), refusing one of maxLen bytes or more.
func ReadVstring(r io.Reader, maxLen int) (string, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", err
	}
	n := int(b[0])
	if n&0x80 != 0 {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", eofIsUnexpected(err)
		}
		n = (n&^0x80)<<8 | int(b[0])
	}
	if n >= maxLen {
		return "", fmt.Errorf("over-long vstring received (%d >= %d)", n, maxLen)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", eofIsUnexpected(err)
	}
	return string(buf), nil
}

func (c *Conn) WriteVstring(s string) error {
	_, err := c.Writer.Write(AppendVstring(nil, s))
	return err
}

func (c *Conn) ReadVstring(maxLen int) (string, error) { return ReadVstring(c.Reader, maxLen) }

// WriteShort writes a 16-bit value, little-endian (io.c:write_shortint).
func (c *Conn) WriteShort(x uint16) error {
	_, err := c.Writer.Write([]byte{byte(x), byte(x >> 8)})
	return err
}

// ReadShort reads a value written by WriteShort (io.c:read_shortint).
func (c *Conn) ReadShort() (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(c.Reader, b[:]); err != nil {
		return 0, err
	}
	return uint16(b[0]) | uint16(b[1])<<8, nil
}
