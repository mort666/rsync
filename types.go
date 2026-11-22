package rsync

import (
	"fmt"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// rsync/rsync.h:struct sum_buf
type SumBuf struct {
	Offset int64
	Len    int64
	Index  int32
	Sum1   uint32
	Sum2   [16]byte
}

// TODO: remove connection.go:sumHead in favor of this type
type SumHead struct {
	// “number of blocks” (openrsync)
	// “how many chunks” (rsync)
	ChecksumCount int32

	// “block length in the file” (openrsync)
	// maximum (1 << 29) for older rsync, (1 << 17) for newer
	BlockLength int32

	// “long checksum length” (openrsync)
	ChecksumLength int32

	// “terminal (remainder) block length” (openrsync)
	// RemainderLength is flength % BlockLength
	RemainderLength int32

	Sums []SumBuf
}

// ReadFrom is ReadFromMax with the longest strong checksum, 16 bytes.
func (sh *SumHead) ReadFrom(c *rsyncwire.Conn) error {
	return sh.ReadFromMax(c, 16)
}

// ReadFromMax is io.c:read_sum_head: a sum head, checked as rsync 3.4
// checks it. maxSumLen is the session's strong checksum length
// (xfer_sum_len), which the per-block checksum length may not exceed: 8
// for xxh64 and xxh3, 16 for the others.
func (sh *SumHead) ReadFromMax(c *rsyncwire.Conn, maxSumLen int) error {
	// io.c:read_sum_head: MAX_BLOCK_SIZE from protocol 30,
	// OLD_MAX_BLOCK_SIZE before.
	maxBlockLen := int32(1 << 29)
	if c.Session.Protocol >= 30 {
		maxBlockLen = 1 << 17
	}

	var err error
	sh.ChecksumCount, err = c.ReadInt32()
	if err != nil {
		return err
	}
	if sh.ChecksumCount < 0 {
		return fmt.Errorf("invalid checksum count %d", sh.ChecksumCount)
	}

	sh.BlockLength, err = c.ReadInt32()
	if err != nil {
		return err
	}
	if sh.BlockLength < 0 || sh.BlockLength > maxBlockLen {
		return fmt.Errorf("invalid block length %d", sh.BlockLength)
	}
	if sh.ChecksumCount > 0 && sh.BlockLength == 0 {
		// For ChecksumCount 0, BlockLength is expected to be 0.
		// But for ChecksumCount > 0, BlockLength 0 panics in match.go:
		//
		// hashSearch sets k := int(head.BlockLength) == 0
		// On the last offset, mmore == 0, so we call
		// ms.ptr(offset-backup, k+mmore+backup), which is:
		// ms.ptr(offset-backup, backup),
		// update = update[backup:] // skips backup bytes, i.e. len==0
		// s1 -= rsyncchecksum.SignExtend(update[0]) // now out of bounds
		return fmt.Errorf("invalid block length 0 with non-zero checksum count")
	}

	sh.ChecksumLength, err = c.ReadInt32()
	if err != nil {
		return err
	}
	if sh.ChecksumLength < 0 || int(sh.ChecksumLength) > maxSumLen {
		return fmt.Errorf("invalid checksum length %d", sh.ChecksumLength)
	}

	sh.RemainderLength, err = c.ReadInt32()
	if err != nil {
		return err
	}
	if sh.RemainderLength < 0 || sh.RemainderLength > sh.BlockLength {
		return fmt.Errorf("invalid remainder length %d", sh.RemainderLength)
	}

	return nil
}

func (sh *SumHead) WriteTo(c *rsyncwire.Conn) error {
	var buf rsyncwire.Buffer
	buf.WriteInt32(sh.ChecksumCount)
	buf.WriteInt32(sh.BlockLength)
	buf.WriteInt32(sh.ChecksumLength)
	buf.WriteInt32(sh.RemainderLength)
	return c.WriteString(buf.String())
}
