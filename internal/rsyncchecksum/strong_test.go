package rsyncchecksum

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
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
