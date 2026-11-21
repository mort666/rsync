package rsyncwire

import "fmt"

// Protocol versions this implementation can speak.
const (
	// MinProtocolVersion is the oldest version gokrazy/rsync speaks. rsync
	// itself goes back to 20, but nothing older than 27 is implemented.
	MinProtocolVersion = 27

	// MaxProtocolVersion is the newest version gokrazy/rsync speaks, and the
	// one it announces unless --protocol asks for less. Each version is added
	// once the interop matrix passes at it. 29 arrived with rsync 2.6.4
	// (2005); openrsync still speaks 27.
	MaxProtocolVersion = 29

	// maxSaneProtocolVersion matches rsync's MAX_PROTOCOL_VERSION. A peer
	// announcing more than this is not speaking rsync at all, usually
	// because a remote shell printed something before rsync started.
	maxSaneProtocolVersion = 40
)

// Session holds what the two sides of one connection agreed on. Later
// protocol versions add compat flags, the checksum and the compressor here.
type Session struct {
	// Protocol is the negotiated protocol version: the lower of what each
	// side announced.
	Protocol int32
}

// CheckProtocolOption reports whether this side can announce ours, the
// version from --protocol. side is "Client" or "Server", for messages.
func CheckProtocolOption(side string, ours int32) error {
	if ours < MinProtocolVersion {
		return fmt.Errorf("--protocol must be at least %d on the %s", MinProtocolVersion, side)
	}
	if ours > MaxProtocolVersion {
		return fmt.Errorf("--protocol must be no more than %d on the %s", MaxProtocolVersion, side)
	}
	return nil
}

// NegotiateProtocol returns the protocol version for a connection where this
// side announced ours and the peer announced remote, mirroring rsync's
// compat.c:setup_protocol. side is "Client" or "Server", for messages.
func NegotiateProtocol(side string, ours, remote int32) (int32, error) {
	if remote > maxSaneProtocolVersion || remote <= 0 {
		return 0, fmt.Errorf("protocol version mismatch: peer announced %d -- is your shell clean? (see the rsync manpage for an explanation)", remote)
	}
	if err := CheckProtocolOption(side, ours); err != nil {
		return 0, err
	}
	if remote < MinProtocolVersion {
		return 0, fmt.Errorf("peer speaks protocol version %d, older than the oldest gokrazy/rsync supports (%d)", remote, MinProtocolVersion)
	}
	return min(ours, remote), nil
}
