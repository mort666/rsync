package rsyncchecksum

import (
	"crypto/md5"
	"encoding/binary"
	"hash"
	"io"
	"os"
	"slices"

	"github.com/cespare/xxhash/v2"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/mmcloughlin/md4"
	"github.com/zeebo/xxh3"
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

// The strong checksums, by the names rsync negotiates them with, in rsync's
// table order (checksum.c valid_checksums_items). Both sides pick the name
// from the other's list they rank highest themselves, so they only agree
// if they rank names in the same order: this list may leave names out but
// must never reorder them. rsync also has "xxhash" (an alias of xxh64),
// "sha1" (with OpenSSL) and "none".
var Names = []string{"xxh128", "xxh3", "xxh64", "md5", "md4"}

// md4Old is the MD4 of protocols 27 to 29, which seeds the whole-file sum
// (CSUM_MD4_OLD); a negotiated "md4" does not.
const md4Old = "md4-old"

// DefaultName is the checksum without negotiation (checksum.c
// parse_csum_name): MD5 from protocol 30, the old MD4 before.
func DefaultName(proto int32) string {
	if proto >= 30 {
		return "md5"
	}
	return md4Old
}

// Strong is the strong checksum of a session.
type Strong struct {
	name     string
	fileName string // for --checksum
	// seedFirst puts the seed before the data in an MD5 block sum
	// (CF_CHKSUM_SEED_FIX, proper_seed_order).
	seedFirst bool
	seed      int32
}

// NewStrong returns the strong checksum for a session: the negotiated one,
// or the default for its protocol.
func NewStrong(s rsyncwire.Session, seed int32) Strong {
	name := s.Checksum
	if name == "" {
		name = DefaultName(s.Protocol)
	}
	fileName := s.FileChecksum
	if fileName == "" {
		fileName = name
	}
	// The MD5 seed order follows CF_CHKSUM_SEED_FIX only for the implied
	// default. An "md5" chosen by name (negotiated, or --checksum-choice)
	// goes through OpenSSL in rsync builds that have it, which is the usual
	// build and puts the seed first at any protocol (checksum.c
	// get_checksum2, xfer_sum_evp_md). rsync built without OpenSSL follows
	// the flag there too, and does not interoperate with its OpenSSL builds
	// for a named md5 below protocol 30.
	seedFirst := s.CompatFlags&rsyncwire.CfChksumSeedFix != 0 || s.Checksum == "md5"
	return Strong{
		name:      name,
		fileName:  fileName,
		seedFirst: seedFirst,
		seed:      seed,
	}
}

// Name is the checksum's name, as rsync's --debug=nstr shows it.
func (s Strong) Name() string {
	if s.name == md4Old {
		return "md4"
	}
	return s.name
}

// Supported reports whether name is a checksum this package implements.
func Supported(name string) bool {
	return slices.Contains(Names, name)
}

// Len is the length of a sum (checksum.c:csum_len_for_type).
func (s Strong) Len() int {
	switch s.name {
	case "xxh64", "xxh3":
		return 8
	}
	return 16
}

func (s Strong) seedBytes() []byte {
	return binary.LittleEndian.AppendUint32(nil, uint32(s.seed))
}

// xxhSeed is the seed as rsync passes it to xxHash: the int, sign-extended
// to 64 bits.
func (s Strong) xxhSeed() uint64 { return uint64(int64(s.seed)) }

// Block is the strong sum of one block (checksum.c:get_checksum2). A zero
// seed is left out of MD4 and MD5, as rsync does.
func (s Strong) Block(buf []byte) []byte {
	switch s.name {
	case "xxh64":
		d := xxhash.NewWithSeed(s.xxhSeed())
		d.Write(buf)
		return binary.LittleEndian.AppendUint64(nil, d.Sum64())
	case "xxh3":
		return binary.LittleEndian.AppendUint64(nil, xxh3.HashSeed(buf, s.xxhSeed()))
	case "xxh128":
		return appendUint128(nil, xxh3.Hash128Seed(buf, s.xxhSeed()))
	case "md5":
		h := md5.New()
		if s.seed != 0 && s.seedFirst {
			h.Write(s.seedBytes())
		}
		h.Write(buf)
		if s.seed != 0 && !s.seedFirst {
			h.Write(s.seedBytes())
		}
		return h.Sum(nil)
	default: // md4, md4-old
		h := md4.New()
		h.Write(buf)
		if s.seed != 0 {
			h.Write(s.seedBytes())
		}
		return h.Sum(nil)
	}
}

// appendUint128 is how rsync writes an XXH128 sum: the low half, then the
// high half, each little-endian.
func appendUint128(b []byte, v xxh3.Uint128) []byte {
	b = binary.LittleEndian.AppendUint64(b, v.Lo)
	return binary.LittleEndian.AppendUint64(b, v.Hi)
}

// NewTransferHash is the running sum of a file's data that the sender
// sends after it and the receiver checks (checksum.c:sum_init). Only the
// old MD4 of protocols 27 to 29 is seeded.
func (s Strong) NewTransferHash() hash.Hash {
	if s.name == md4Old {
		h := md4.New()
		h.Write(s.seedBytes())
		return h
	}
	return newHash(s.name)
}

// FileLen is the length of a --checksum file sum.
func (s Strong) FileLen() int { return Strong{name: s.fileName}.Len() }

// NewFileHash is the unseeded whole-file sum --checksum compares
// (checksum.c:file_checksum).
func (s Strong) NewFileHash() hash.Hash { return newHash(s.fileName) }

func newHash(name string) hash.Hash {
	switch name {
	case "xxh64":
		return leHash64{xxhash.New()}
	case "xxh3":
		return leHash64{xxh3.New()}
	case "xxh128":
		return &xxh128Hash{xxh3.New()}
	case "md5":
		return md5.New()
	default:
		return md4.New()
	}
}

type hash64 interface {
	hash.Hash
	Sum64() uint64
}

// leHash64 sums as rsync writes a 64-bit xxHash: little-endian
// (SIVAL64), where the libraries append it big-endian.
type leHash64 struct{ hash64 }

func (h leHash64) Sum(b []byte) []byte { return binary.LittleEndian.AppendUint64(b, h.Sum64()) }

type xxh128Hash struct{ *xxh3.Hasher }

func (h *xxh128Hash) Size() int           { return 16 }
func (h *xxh128Hash) Sum(b []byte) []byte { return appendUint128(b, h.Sum128()) }

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
