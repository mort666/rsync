package rsyncchecksum

import (
	"crypto/md5"
	"encoding/base64"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/mmcloughlin/md4"
)

// rsync's own tables, in order (checksum.c valid_checksums_items and
// valid_auth_checksums_items, rsync master for 4.0.0, which adds SHA-3 to
// 3.5.2's), aliases and all.
var (
	upstreamChecksums = []string{"xxh128", "xxh3", "xxh64", "xxhash", "sha3-512", "sha3-384", "sha3-256", "md5", "md4", "sha1", "none"}
	upstreamAuth      = []string{"sha512", "sha256", "sha1", "md5", "md4"}
)

// Two sides only agree when they rank names the same, so our lists may
// leave names out of rsync's but never reorder them.
func TestNamesKeepUpstreamOrder(t *testing.T) {
	for _, tt := range []struct {
		ours, upstream []string
	}{
		{Names, upstreamChecksums},
		{DefaultNames, upstreamChecksums},
		{AuthDigests, upstreamAuth},
	} {
		i := 0
		for _, n := range tt.ours {
			for i < len(tt.upstream) && tt.upstream[i] != n {
				i++
			}
			if i == len(tt.upstream) {
				t.Errorf("%v is not in rsync's order %v (at %q)", tt.ours, tt.upstream, n)
				break
			}
		}
	}
}

func TestPickName(t *testing.T) {
	for _, tt := range []struct {
		peer string
		want string
		ok   bool
	}{
		{"xxh128 xxh3 xxh64 md5 md4 sha1", "xxh128", true},
		// Our ranking decides, not the peer's order.
		{"md4 md5", "md5", true},
		{"sha1 none md4", "md4", true},
		{"sha1 none", "sha1", true},
		{"none", "", false},
		// Not offered unless asked for, so never picked from the defaults.
		{"sha3-512 sha3-256", "", false},
		{"", "", false},
	} {
		got, ok := rsyncwire.PickName(DefaultNames, tt.peer)
		if got != tt.want || ok != tt.ok {
			t.Errorf("PickName(%q) = %q, %v; want %q, %v", tt.peer, got, ok, tt.want, tt.ok)
		}
	}
}

func TestOurList(t *testing.T) {
	for _, tt := range []struct {
		env    string
		server bool
		want   string
	}{
		{"", false, "xxh128 xxh3 xxh64 md5 md4 sha1"},
		{"md5 xxh64", false, "md5 xxh64"},
		{"sha1 md4", false, "sha1 md4"},
		{"blake3 md4", false, "md4"},
		{"blake3", false, "xxh128 xxh3 xxh64 md5 md4 sha1"},
		{"md5&xxh3", false, "md5"},
		{"md5&xxh3", true, "xxh3"},
		// The SHA-3 sums only when asked for.
		{"sha3-256 md5", false, "sha3-256 md5"},
		{"md5&sha3-512", true, "sha3-512"},
	} {
		got := rsyncwire.OurList(tt.env, tt.server, Names, DefaultNames)
		if s := join(got); s != tt.want {
			t.Errorf("OurList(%q, server=%v) = %q, want %q", tt.env, tt.server, s, tt.want)
		}
	}
}

func join(l []string) string {
	s := ""
	for i, n := range l {
		if i > 0 {
			s += " "
		}
		s += n
	}
	return s
}

func TestNegotiateAuth(t *testing.T) {
	for _, tt := range []struct {
		proto int32
		peer  string
		want  string
	}{
		{27, "", "md4-old"},
		{29, "", "md4-old"},
		{30, "", "md5"},
		{31, "sha512 sha256 sha1 md5 md4", "sha512"},
		{29, "md5 md4", "md5"},
		{30, "md4", "md4"},
	} {
		if got, err := NegotiateAuth(tt.proto, tt.peer); err != nil || got != tt.want {
			t.Errorf("NegotiateAuth(%d, %q) = %q, %v; want %q", tt.proto, tt.peer, got, err, tt.want)
		}
	}
	if _, err := NegotiateAuth(30, "blake3"); err == nil {
		t.Errorf("NegotiateAuth with nothing in common: no error")
	}
}

func TestAuthResponse(t *testing.T) {
	b64 := base64.RawStdEncoding.EncodeToString
	m4 := md4.New()
	m4.Write([]byte{0, 0, 0, 0})
	m4.Write([]byte("pwchallenge"))
	if got, want := AuthResponse("md4-old", "pw", "challenge"), b64(m4.Sum(nil)); got != want {
		t.Errorf("md4-old: %q, want %q (MD4 of a zero seed, password, challenge)", got, want)
	}
	m5 := md5.Sum([]byte("pwchallenge"))
	if got, want := AuthResponse("md5", "pw", "challenge"), b64(m5[:]); got != want {
		t.Errorf("md5: %q, want %q", got, want)
	}
}

func TestNegotiateChoice(t *testing.T) {
	for _, tt := range []struct {
		choice, xfer, file string
	}{
		{"xxh64", "xxh64", "xxh64"},
		{"MD5", "md5", "md5"},
		{"xxh3,md5", "xxh3", "md5"},
		{"auto", "", ""},
		{"auto,xxh128", "", "xxh128"},
		{"sha1", "sha1", "sha1"},
		{"SHA3-256,sha3-512", "sha3-256", "sha3-512"},
	} {
		c := &rsyncwire.Conn{Session: rsyncwire.Session{Protocol: 30}}
		if err := Negotiate(c, tt.choice, false); err != nil || c.Session.Checksum != tt.xfer || c.Session.FileChecksum != tt.file {
			t.Errorf("Negotiate(%q) = %q, %q, %v; want %q, %q", tt.choice, c.Session.Checksum, c.Session.FileChecksum, err, tt.xfer, tt.file)
		}
	}
	if err := Negotiate(&rsyncwire.Conn{}, "blake3", false); err == nil {
		t.Errorf("Negotiate(blake3): no error")
	}
}
