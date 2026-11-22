package rsyncchecksum

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"hash"
	"strings"

	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/mmcloughlin/md4"
)

// AuthDigests are the daemon authentication digests, in rsync's table order
// (checksum.c valid_auth_checksums_items), which a daemon and its clients
// list in their greetings from rsync 3.2.7. As with Names, the order
// decides what two sides agree on and must not change.
var AuthDigests = []string{"sha512", "sha256", "sha1", "md5", "md4"}

// AuthDigestList is AuthDigests as a greeting carries it.
func AuthDigestList() string { return strings.Join(AuthDigests, " ") }

// authMD4Old is the MD4 of a peer that lists no digests below protocol 30.
// It hashes 4 zero bytes (a seed of 0) before the password, a negotiated
// "md4" does not (compat.c negotiate_daemon_auth, CSUM_MD4_OLD).
const authMD4Old = "md4-old"

// NegotiateAuth picks the daemon auth digest (compat.c
// negotiate_daemon_auth). peerDigests is the list from the peer's greeting,
// "" when it sent none: then it is MD5 from protocol 30 and the old MD4
// below.
func NegotiateAuth(proto int32, peerDigests string) (string, error) {
	if peerDigests == "" {
		if proto >= 30 {
			return "md5", nil
		}
		return authMD4Old, nil
	}
	name, ok := rsyncwire.PickName(AuthDigests, peerDigests)
	if !ok {
		return "", fmt.Errorf("no daemon-auth checksum in common: peer offered %q, we offer %q", peerDigests, AuthDigestList())
	}
	return name, nil
}

// AuthResponse is what a client answers a daemon's challenge with, and what
// the daemon expects (authenticate.c generate_hash): the digest of the
// password and the challenge, base64 without padding.
func AuthResponse(digest, password, challenge string) string {
	var h hash.Hash
	switch digest {
	case "sha512":
		h = sha512.New()
	case "sha256":
		h = sha256.New()
	case "sha1":
		h = sha1.New()
	case "md5":
		h = md5.New()
	case "md4":
		h = md4.New()
	default: // md4-old
		h = md4.New()
		h.Write([]byte{0, 0, 0, 0})
	}
	h.Write([]byte(password))
	h.Write([]byte(challenge))
	return base64.RawStdEncoding.EncodeToString(h.Sum(nil))
}
