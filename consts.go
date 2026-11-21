package rsync

import "github.com/gokrazy/rsync/internal/rsyncwire"

// File-list transmit flags (rsync.h). From protocol 28, XMIT_EXTENDED_FLAGS
// in the first byte means a second byte follows, holding bits 8 to 15.
const (
	XMIT_TOP_DIR            = (1 << 0)
	XMIT_SAME_MODE          = (1 << 1)
	XMIT_SAME_RDEV_pre28    = (1 << 2) // protocols 20 - 27
	XMIT_EXTENDED_FLAGS     = (1 << 2) // protocols 28 - now
	XMIT_SAME_UID           = (1 << 3)
	XMIT_SAME_GID           = (1 << 4)
	XMIT_SAME_NAME          = (1 << 5)
	XMIT_LONG_NAME          = (1 << 6)
	XMIT_SAME_TIME          = (1 << 7)
	XMIT_SAME_RDEV_MAJOR    = (1 << 8)  // protocols 28 - now (devices only)
	XMIT_HLINKED            = (1 << 9)  // protocols 28 - now (non-dirs)
	XMIT_SAME_DEV_pre30     = (1 << 10) // protocols 28 - 29
	XMIT_RDEV_MINOR_8_pre30 = (1 << 11) // protocols 28 - 29
	XMIT_NO_CONTENT_DIR     = (1 << 8)  // protocols 30 - now (dirs only)
	XMIT_USER_NAME_FOLLOWS  = (1 << 10) // protocols 30 - now
	XMIT_GROUP_NAME_FOLLOWS = (1 << 11) // protocols 30 - now
	XMIT_HLINK_FIRST        = (1 << 12) // protocols 30 - now (HLINKED files only)
	XMIT_IO_ERROR_ENDLIST   = (1 << 12) // protocols 31 - now, and 30 with CF_SAFE_FLIST (with XMIT_EXTENDED_FLAGS)
	XMIT_MOD_NSEC           = (1 << 13) // protocols 31 - now

	// Deprecated: rsync 2.6 names, use XMIT_HLINKED.
	XMIT_HAS_IDEV_DATA = XMIT_HLINKED
	// Deprecated: rsync 2.6 names, use XMIT_SAME_DEV_pre30.
	XMIT_SAME_DEV = XMIT_SAME_DEV_pre30
	// Deprecated: rsync 2.6 names, use XMIT_RDEV_MINOR_8_pre30.
	XMIT_RDEV_MINOR_IS_SMALL = XMIT_RDEV_MINOR_8_pre30
)

// Item flags (rsync.h), sent with each file index from protocol 29. The
// generator says what it wants and why; the sender echoes them back.
const (
	ITEM_REPORT_ATIME       = (1 << 0)
	ITEM_REPORT_CHANGE      = (1 << 1)
	ITEM_REPORT_SIZE        = (1 << 2) // regular files only
	ITEM_REPORT_TIMEFAIL    = (1 << 2) // symlinks only
	ITEM_REPORT_TIME        = (1 << 3)
	ITEM_REPORT_PERMS       = (1 << 4)
	ITEM_REPORT_OWNER       = (1 << 5)
	ITEM_REPORT_GROUP       = (1 << 6)
	ITEM_REPORT_ACL         = (1 << 7)
	ITEM_REPORT_XATTR       = (1 << 8)
	ITEM_REPORT_CRTIME      = (1 << 10)
	ITEM_BASIS_TYPE_FOLLOWS = (1 << 11) // a basis-type byte follows
	ITEM_XNAME_FOLLOWS      = (1 << 12) // an alternate name (vstring) follows
	ITEM_IS_NEW             = (1 << 13)
	ITEM_LOCAL_CHANGE       = (1 << 14)
	ITEM_TRANSFER           = (1 << 15) // the generator wants the file's data
)

// Basis types (rsync.h): which file the generator's checksums came from,
// sent after ITEM_BASIS_TYPE_FOLLOWS. Values below FNAMECMP_FNAME index the
// --compare-dest/--copy-dest/--link-dest directories.
const (
	FNAMECMP_FNAME       = 0x80
	FNAMECMP_PARTIAL_DIR = 0x81
	FNAMECMP_BACKUP      = 0x82
	FNAMECMP_FUZZY       = 0x83
)

// as per /usr/include/bits/stat.h:
const (
	S_IFMT   = 0o0170000 // bits determining the file type
	S_IFDIR  = 0o0040000 // Directory
	S_IFCHR  = 0o0020000 // Character device
	S_IFBLK  = 0o0060000 // Block device
	S_IFREG  = 0o0100000 // Regular file
	S_IFIFO  = 0o0010000 // FIFO
	S_IFLNK  = 0o0120000 // Symbolic link
	S_IFSOCK = 0o0140000 // Socket
)

// ProtocolVersion is the newest rsync protocol version this package speaks.
//
// Deprecated: the version is negotiated per connection, see
// rsyncwire.Session. Code that needs the upper bound should use
// rsyncwire.MaxProtocolVersion.
const ProtocolVersion = rsyncwire.MaxProtocolVersion

// rsync/rsync.h defines chunkSize as 32 * 1024, which we match.
//
// Other parts of rsync have subtle dependencies on the chunkSize.
// For example, (*sender.Transfer).hashSearch declares a window
// of at least 256 KB and then reads from that window, requesting
// 2*chunkSize+alignFudge, which must not exceed the window.
// So raising chunkSize to 128 KB or 256 KB breaks transfers
// with "file has changed mid-transfer" (issue #53).
const ChunkSize = 32 * 1024
