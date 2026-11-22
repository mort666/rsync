// Package rsynccommon contains functionality that both the sender and the
// receiver implementation need.
package rsynccommon

import "github.com/gokrazy/rsync"

const blockSize = 700 // rsync/rsync.h

// Block length limits (rsync.h): MAX_BLOCK_SIZE from protocol 30,
// OLD_MAX_BLOCK_SIZE before it. A sender refuses a sum head above them.
const (
	maxBlockSize    = 1 << 17
	oldMaxBlockSize = 1 << 29
)

// MaxBlockLength is the longest block a peer accepts at proto.
func MaxBlockLength(proto int32) int32 {
	if proto < 30 {
		return oldMaxBlockSize
	}
	return maxBlockSize
}

// SumSizesSqroot picks the block and checksum sizes for a file of
// contentLen bytes, as rsync/generator.c:sum_sizes_sqroot does: 700-byte
// blocks up to 700² bytes, then the square root rounded down to a multiple
// of 8, at most MaxBlockLength(proto). The strong sums are sent in full,
// up to 16 bytes: sumLen, the length of the session's strong checksum, may
// be shorter (8 for xxh64), and rsync refuses a sum head asking for more.
// fixedBlockLen is --block-size: when above 0 it is the block length for
// every file.
func SumSizesSqroot(proto int32, contentLen int64, sumLen int, fixedBlockLen int32) rsync.SumHead {
	var blockLength int32
	if fixedBlockLen > 0 {
		blockLength = fixedBlockLen // --block-size
	} else if contentLen <= blockSize*blockSize {
		blockLength = blockSize
	} else {
		maxBlength := MaxBlockLength(proto)
		c := int32(1)
		for l := contentLen; ; {
			l >>= 2
			if l == 0 {
				break
			}
			c <<= 1
		}
		if c < 0 || c >= maxBlength {
			blockLength = maxBlength
		} else {
			for {
				blockLength |= c
				if contentLen < int64(blockLength)*int64(blockLength) {
					blockLength &^= c
				}
				c >>= 1
				if c < 8 {
					break
				}
			}
			blockLength = max(blockLength, blockSize)
		}
	}

	checksumLength := int32(min(16, sumLen)) // SUM_LENGTH, or less

	return rsync.SumHead{
		ChecksumCount:   int32((contentLen + (int64(blockLength) - 1)) / int64(blockLength)),
		RemainderLength: int32(contentLen % int64(blockLength)),
		BlockLength:     blockLength,
		ChecksumLength:  checksumLength,
	}
}
