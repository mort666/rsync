package rsyncchecksum

import (
	"crypto/md5"
	"encoding/binary"
	"hash"
	"io"
	"os"

	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/mmcloughlin/md4"
)

func Tag2(s1, s2 uint16) uint16 {
	return (((s1) + (s2)) & 0xFFFF)
}

func Tag(sum uint32) uint16 {
	return Tag2(uint16(sum&0xFFFF), uint16(sum>>16))
}

// SignExtend mirrors how C converts from (signed char) to uint32, i.e. using
// sign extension. get_checksum1 treats the buffer as (signed char*) instead of
// (unsigned char*), which likely was not a conscious choice, but here we are.
//
// This function is exported for use in the rolling checksum in match.go.
func SignExtend(b byte) uint32 {
	val := uint32(b)
	return uint32(int32(val<<24) >> 24)
}

func Checksum1(buf []byte) uint32 {
	bufLen := len(buf)
	var s1, s2 uint32
	var i int

	if bufLen > 4 {
		for i = 0; i < (bufLen - 4); i += 4 {
			s2 += 4*(s1+SignExtend(buf[i])) +
				3*SignExtend(buf[i+1]) +
				2*SignExtend(buf[i+2]) +
				SignExtend(buf[i+3])
			s1 += SignExtend(buf[i+0]) +
				SignExtend(buf[i+1]) +
				SignExtend(buf[i+2]) +
				SignExtend(buf[i+3])
		}
	}
	for ; i < bufLen; i++ {
		s1 += SignExtend(buf[i])
		s2 += s1
	}
	return (s1 & 0xffff) + (s2 << 16)
}

const Size = md4.Size

// Strong is the strong checksum of a session: MD4 below protocol 30, MD5
// from 30 when nothing else is negotiated (checksum.c).
type Strong struct {
	md5 bool
	// seedFirst puts the seed before the data in an MD5 block sum
	// (CF_CHKSUM_SEED_FIX, proper_seed_order).
	seedFirst bool
	seed      int32
}

// NewStrong returns the strong checksum for a session.
func NewStrong(s rsyncwire.Session, seed int32) Strong {
	return Strong{
		md5:       s.Protocol >= 30,
		seedFirst: s.CompatFlags&rsyncwire.CfChksumSeedFix != 0,
		seed:      seed,
	}
}

func (s Strong) seedBytes() []byte {
	return binary.LittleEndian.AppendUint32(nil, uint32(s.seed))
}

// Block is the strong sum of one block (checksum.c:get_checksum2). A zero
// seed is left out, as rsync does.
func (s Strong) Block(buf []byte) []byte {
	var h hash.Hash
	if s.md5 {
		h = md5.New()
		if s.seed != 0 && s.seedFirst {
			h.Write(s.seedBytes())
		}
		h.Write(buf)
		if s.seed != 0 && !s.seedFirst {
			h.Write(s.seedBytes())
		}
	} else {
		h = md4.New()
		h.Write(buf)
		if s.seed != 0 {
			h.Write(s.seedBytes())
		}
	}
	return h.Sum(nil)
}

// NewTransferHash is the running sum of a file's data that the sender
// sends after it and the receiver checks (checksum.c:sum_init): MD4 with
// the seed first below protocol 30, plain MD5 from 30.
func (s Strong) NewTransferHash() hash.Hash {
	if s.md5 {
		return md5.New()
	}
	h := md4.New()
	h.Write(s.seedBytes())
	return h
}

// NewFileHash is the whole-file sum --checksum compares
// (checksum.c:file_checksum): MD4 below protocol 30, MD5 from 30.
func (s Strong) NewFileHash() hash.Hash {
	if s.md5 {
		return md5.New()
	}
	return md4.New()
}

// FileChecksum sums r with NewFileHash.
func (s Strong) FileChecksum(r io.Reader) ([]byte, error) {
	h := s.NewFileHash()
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// RootFileChecksum sums the file fn in root with NewFileHash.
func (s Strong) RootFileChecksum(root *os.Root, fn string) ([]byte, error) {
	f, err := root.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return s.FileChecksum(f)
}
