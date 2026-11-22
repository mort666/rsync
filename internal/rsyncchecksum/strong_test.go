package rsyncchecksum

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha3"
	"encoding/binary"
	"hash"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/mmcloughlin/md4"
)

func sum(h interface{ Sum([]byte) []byte }) []byte { return h.Sum(nil) }

// The layouts of checksum.c:get_checksum2 and sum_init per protocol.
func TestStrong(t *testing.T) {
	data := []byte("some block of data")
	const seed = 0x12345678
	seedLE := binary.LittleEndian.AppendUint32(nil, seed)
	md := func(parts ...[]byte) []byte { h := md5.New(); h.Write(bytes.Join(parts, nil)); return sum(h) }
	m4 := func(parts ...[]byte) []byte { h := md4.New(); h.Write(bytes.Join(parts, nil)); return sum(h) }
	with := func(nh func() hash.Hash) func(parts ...[]byte) []byte {
		return func(parts ...[]byte) []byte { h := nh(); h.Write(bytes.Join(parts, nil)); return sum(h) }
	}
	s1 := with(sha1.New)
	s3a := with(func() hash.Hash { return sha3.New256() })
	s3b := with(func() hash.Hash { return sha3.New384() })
	s3c := with(func() hash.Hash { return sha3.New512() })

	for _, tt := range []struct {
		name     string
		sess     rsyncwire.Session
		seed     int32
		block    []byte
		transfer []byte
		file     []byte
	}{
		{"27", rsyncwire.Session{Protocol: 27}, seed, m4(data, seedLE), m4(seedLE, data), m4(data)},
		{"27 zero seed", rsyncwire.Session{Protocol: 27}, 0, m4(data), m4(make([]byte, 4), data), m4(data)},
		{"30", rsyncwire.Session{Protocol: 30}, seed, md(data, seedLE), md(data), md(data)},
		{"30 seed fix", rsyncwire.Session{Protocol: 30, CompatFlags: rsyncwire.CfChksumSeedFix}, seed, md(seedLE, data), md(data), md(data)},
		{"30 zero seed", rsyncwire.Session{Protocol: 30, CompatFlags: rsyncwire.CfChksumSeedFix}, 0, md(data), md(data), md(data)},
		// A named md5 puts the seed first, as rsync's OpenSSL path does.
		{"29 named md5", rsyncwire.Session{Protocol: 29, Checksum: "md5"}, seed, md(seedLE, data), md(data), md(data)},
		{"30 named md5 no fix", rsyncwire.Session{Protocol: 30, Checksum: "md5"}, seed, md(seedLE, data), md(data), md(data)},
		// OpenSSL's digests: the seed first in block sums only, at any
		// protocol and whatever CF_CHKSUM_SEED_FIX says.
		{"sha1", rsyncwire.Session{Protocol: 31, Checksum: "sha1"}, seed, s1(seedLE, data), s1(data), s1(data)},
		{"sha1 zero seed", rsyncwire.Session{Protocol: 31, Checksum: "sha1"}, 0, s1(data), s1(data), s1(data)},
		{"sha1 at 27", rsyncwire.Session{Protocol: 27, Checksum: "sha1"}, seed, s1(seedLE, data), s1(data), s1(data)},
		{"sha3-256", rsyncwire.Session{Protocol: 32, Checksum: "sha3-256"}, seed, s3a(seedLE, data), s3a(data), s3a(data)},
		{"sha3-384", rsyncwire.Session{Protocol: 32, Checksum: "sha3-384"}, seed, s3b(seedLE, data), s3b(data), s3b(data)},
		{"sha3-512", rsyncwire.Session{Protocol: 33, Checksum: "sha3-512"}, seed, s3c(seedLE, data), s3c(data), s3c(data)},
		{"sha3-512 xfer, sha1 file", rsyncwire.Session{Protocol: 33, Checksum: "sha3-512", FileChecksum: "sha1"}, seed, s3c(seedLE, data), s3c(data), s1(data)},
	} {
		s := NewStrong(tt.sess, tt.seed)
		if got := s.Block(data); !bytes.Equal(got, tt.block) {
			t.Errorf("%s: Block = %x, want %x", tt.name, got, tt.block)
		}
		h := s.NewTransferHash()
		h.Write(data)
		if got := sum(h); !bytes.Equal(got, tt.transfer) {
			t.Errorf("%s: transfer sum = %x, want %x", tt.name, got, tt.transfer)
		}
		if got, _ := s.FileChecksum(bytes.NewReader(data)); !bytes.Equal(got, tt.file) {
			t.Errorf("%s: file sum = %x, want %x", tt.name, got, tt.file)
		}
	}
}

// The sums' lengths (csum_len_for_type), and the 16 bytes at most a block
// sum carries.
func TestLen(t *testing.T) {
	for _, tt := range []struct {
		name          string
		len, blockLen int
	}{
		{"xxh64", 8, 8}, {"xxh3", 8, 8}, {"xxh128", 16, 16}, {"md5", 16, 16}, {"md4", 16, 16},
		{"sha1", 20, 16}, {"sha3-256", 32, 16}, {"sha3-384", 48, 16}, {"sha3-512", 64, 16},
	} {
		s := NewStrong(rsyncwire.Session{Protocol: 32, Checksum: tt.name}, 1)
		if s.Len() != tt.len || s.BlockLen() != tt.blockLen || len(s.Block([]byte("x"))) != tt.len {
			t.Errorf("%s: Len %d, BlockLen %d, block sum %d bytes; want %d, %d", tt.name, s.Len(), s.BlockLen(), len(s.Block([]byte("x"))), tt.len, tt.blockLen)
		}
		if s.Len() > MaxLen {
			t.Errorf("%s: Len %d > MaxLen %d", tt.name, s.Len(), MaxLen)
		}
	}
}
