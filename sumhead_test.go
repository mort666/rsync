package rsync

import (
	"bytes"
	"io"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

func sumHeadConn(count, blen, s2len, rem int32) *rsyncwire.Conn {
	var b rsyncwire.Buffer
	for _, v := range []int32{count, blen, s2len, rem} {
		b.WriteInt32(v)
	}
	return &rsyncwire.Conn{Reader: io.NopCloser(bytes.NewReader([]byte(b.String()))), Session: rsyncwire.Session{Protocol: 31}}
}

// The per-block checksum length may not exceed the session's strong
// checksum (rsync 3.4, io.c:read_sum_head).
func TestSumHeadChecksumLength(t *testing.T) {
	for _, tt := range []struct {
		s2len, max int32
		ok         bool
	}{
		{8, 8, true},
		{9, 8, false},
		{16, 16, true},
		{17, 16, false},
		{-1, 16, false},
		{0, 8, true},
	} {
		var sh SumHead
		err := sh.ReadFromMax(sumHeadConn(1, 700, tt.s2len, 0), int(tt.max))
		if (err == nil) != tt.ok {
			t.Errorf("s2length %d with a %d-byte checksum: %v, want ok=%v", tt.s2len, tt.max, err, tt.ok)
		}
	}
	var sh SumHead
	if err := sh.ReadFrom(sumHeadConn(1, 700, 16, 0)); err != nil {
		t.Errorf("ReadFrom with 16: %v", err)
	}
	// The other checks: block length, zero block length, remainder.
	for _, c := range []*rsyncwire.Conn{
		sumHeadConn(-1, 700, 16, 0),
		sumHeadConn(1, 1<<17+1, 16, 0),
		sumHeadConn(1, 0, 16, 0),
		sumHeadConn(1, 700, 16, 701),
	} {
		if err := sh.ReadFrom(c); err == nil {
			t.Errorf("a bad sum head was accepted")
		}
	}
}
