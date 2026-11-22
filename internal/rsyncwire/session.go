package rsyncwire

import (
	"fmt"
	"strconv"
	"strings"
)

// Protocol versions this implementation can speak.
const (
	// MinProtocolVersion is the oldest version gokrazy/rsync speaks. rsync
	// itself goes back to 20, but nothing older than 27 is implemented.
	MinProtocolVersion = 27

	// MaxProtocolVersion is the newest version gokrazy/rsync speaks, and the
	// one it announces unless --protocol asks for less. Each version is added
	// once the interop matrix passes at it. 30 arrived with rsync 3.0.0
	// (2008), 31 with 3.1.0 (2013); openrsync still speaks 27.
	MaxProtocolVersion = 31

	// maxSaneProtocolVersion matches rsync's MAX_PROTOCOL_VERSION. A peer
	// announcing more than this is not speaking rsync at all, usually
	// because a remote shell printed something before rsync started.
	maxSaneProtocolVersion = 40
)

// Session holds what the two sides of one connection agreed on. Later
// protocol versions add the checksum and the compressor here.
type Session struct {
	// Protocol is the negotiated protocol version: the lower of what each
	// side announced.
	Protocol int32

	// CompatFlags are the CF_* flags the server chose from protocol 30
	// (compat.c:setup_protocol).
	CompatFlags uint32

	// Checksum is the negotiated strong checksum for block and transfer
	// sums (see rsyncchecksum.Names), or "" for the protocol's default.
	// FileChecksum is the one for --checksum file sums; the two only
	// differ with --checksum-choice=xfer,file.
	Checksum     string
	FileChecksum string

	// Compress is the compressor for file data (see rsynctoken.Names), or
	// "" for none. CompressLevel is its level.
	Compress      string
	CompressLevel int
}

// Compat flags (compat.c), which a server sends from protocol 30. It picks
// them from the capability letters the client sent in its -e option.
const (
	CfIncRecurse        = 1 << 0 // 'i': incremental recursion
	CfSymlinkTimes      = 1 << 1 // 'L': the receiver can set symlink times
	CfSymlinkIconv      = 1 << 2 // 's'
	CfSafeFlist         = 1 << 3 // 'f': an I/O error can end the file list
	CfAvoidXattrOptim   = 1 << 4 // 'x'
	CfChksumSeedFix     = 1 << 5 // 'C': MD5 block sums put the seed first
	CfInplacePartialDir = 1 << 6 // 'I'
	CfVarintFlistFlags  = 1 << 7 // 'v': varint file-list flags, and name negotiation
	CfID0Names          = 1 << 8 // 'u': names for uid and gid 0
)

// ClientCapabilities is what a gokrazy/rsync client offers in its -e option
// from protocol 30, as rsync's options.c:maybe_add_e_option builds it, but
// with only the letters this implementation handles: 'f' (safe file list),
// 'C' (checksum seed order fix) and 'v' (varint file-list flags, and
// checksum and compression name negotiation). No 'i' (incremental
// recursion), 'L', 's', 'x', 'I' or 'u'.
const ClientCapabilities = "e.fCv"

// ServerCompatFlags picks the compat flags a server sends for the
// capability letters in the client's -e value (shell_cmd), only those this
// implementation handles: 'f', 'C' and 'v'. A client offering more, such
// as rsync's "e.LsfxCIvu", gets the rest turned off, which it accepts.
func ServerCompatFlags(clientInfo string) uint32 {
	var flags uint32
	for _, c := range clientInfo {
		switch c {
		case 'f':
			flags |= CfSafeFlist
		case 'C':
			flags |= CfChksumSeedFix
		case 'v':
			flags |= CfVarintFlistFlags
		}
	}
	return flags
}

// SafeFlist reports whether an I/O error may end the file list
// (use_safe_inc_flist: CF_SAFE_FLIST, or protocol 31+).
func (s Session) SafeFlist() bool {
	return s.CompatFlags&CfSafeFlist != 0 || s.Protocol >= 31
}

// WriteCompatFlags is the server side of the protocol 30 compat flags.
func (c *Conn) WriteCompatFlags(clientInfo string) error {
	if c.Session.Protocol < 30 {
		return nil
	}
	c.Session.CompatFlags = ServerCompatFlags(clientInfo)
	return c.WriteVarint(int32(c.Session.CompatFlags))
}

// ReadCompatFlags is the client side of the protocol 30 compat flags. A
// server that turns on something this client did not offer gets an error,
// so a surprise fails loudly instead of corrupting the transfer.
func (c *Conn) ReadCompatFlags() error {
	if c.Session.Protocol < 30 {
		return nil
	}
	f, err := c.ReadVarint()
	if err != nil {
		return err
	}
	// rsync's server sets the symlink flags whatever the client offered.
	// They only matter for symlink times and --iconv, which gokrazy/rsync
	// does not do.
	allowed := ServerCompatFlags(ClientCapabilities) | CfSymlinkTimes | CfSymlinkIconv
	if unknown := uint32(f) &^ allowed; unknown != 0 {
		return fmt.Errorf("server set compat flags %#x that were not offered", unknown)
	}
	c.Session.CompatFlags = uint32(f)
	return nil
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

// Greeting is the "@RSYNCD:" line a daemon or daemon client starts with for
// protocol version proto, as rsync 3.2.7 and newer send it: version,
// sub-protocol (always 0, as gokrazy/rsync speaks only final protocol
// versions) and the auth digests this side supports. Older peers read the
// version alone.
func Greeting(proto int32, authDigests string) string {
	return fmt.Sprintf("@RSYNCD: %d.0 %s\n", proto, authDigests)
}

// ParseGreeting reads a peer's "@RSYNCD:" line (clientserver.c
// exchange_protocols): the version, and the auth digests it lists, if any.
// From protocol 30 the sub-protocol must be there, and above 31 the digest
// list too.
func ParseGreeting(line string) (proto int32, digests string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "@RSYNCD: ")
	if !ok {
		return 0, "", fmt.Errorf("invalid greeting %q", line)
	}
	version, digests, _ := strings.Cut(rest, " ")
	major, sub, hasSub := strings.Cut(version, ".")
	p, err := strconv.ParseInt(major, 10, 32)
	if err != nil {
		return 0, "", fmt.Errorf("invalid greeting %q", line)
	}
	if hasSub {
		if _, err := strconv.ParseInt(sub, 10, 32); err != nil {
			return 0, "", fmt.Errorf("invalid greeting %q", line)
		}
	} else if p >= 30 {
		return 0, "", fmt.Errorf("the peer omitted the subprotocol value: %q", line)
	}
	if digests == "" && p > 31 {
		return 0, "", fmt.Errorf("the peer omitted the digest name list: %q", line)
	}
	return int32(p), strings.TrimSpace(digests), nil
}
