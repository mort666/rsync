package rsyncxattr

import (
	"fmt"
	"io/fs"
	"strings"
)

// --fake-super keeps what only root can set in xattrs (xattrs.c): the
// mode, device number and owner in user.rsync.%stat, and with -A the ACLs
// in user.rsync.%aacl and %dacl. Devices, specials and symlinks become
// regular files; a symlink's file holds its target.
const (
	StatName       = rsyncPercent + "stat"
	accessACLName  = rsyncPercent + "aacl"
	defaultACLName = rsyncPercent + "dacl"
	// rsyncPrefix holds the names of other namespaces under --fake-super:
	// security.selinux is kept as user.rsync.security.selinux.
	rsyncPrefix = "user.rsync."
)

// fakeOwn reports whether name is one of --fake-super's own records,
// which -X neither sends nor removes.
func fakeOwn(name string) bool {
	return name == StatName || name == accessACLName || name == defaultACLName
}

// Stat is a user.rsync.%stat record: "%o %u,%u %u:%u", the mode (file type
// and permissions as rsync sends them), device major and minor, uid and gid.
type Stat struct {
	Mode         uint32
	Major, Minor uint32
	Uid, Gid     uint32
}

// ParseStat reads a %stat value (get_stat_xattr).
func ParseStat(b []byte) (Stat, error) {
	var s Stat
	n, err := fmt.Sscanf(string(b), "%o %d,%d %d:%d", &s.Mode, &s.Major, &s.Minor, &s.Uid, &s.Gid)
	if err != nil || n != 5 {
		return Stat{}, fmt.Errorf("corrupt %s xattr: %q", StatName, b)
	}
	return s, nil
}

// String is the %stat value (set_stat_xattr).
func (s Stat) String() string {
	return fmt.Sprintf("%o %d,%d %d:%d", s.Mode, s.Major, s.Minor, s.Uid, s.Gid)
}

// Unix file types, as rsync sends them.
const (
	sIFMT   = 0o170000
	sIFSOCK = 0o140000
	sIFLNK  = 0o120000
	sIFREG  = 0o100000
	sIFBLK  = 0o060000
	sIFDIR  = 0o040000
	sIFCHR  = 0o020000
	sIFIFO  = 0o010000
)

// GoMode is s's mode as Go has it.
func (s Stat) GoMode() fs.FileMode {
	m := fs.FileMode(s.Mode & 0o777)
	if s.Mode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if s.Mode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if s.Mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	switch s.Mode & sIFMT {
	case sIFDIR:
		m |= fs.ModeDir
	case sIFLNK:
		m |= fs.ModeSymlink
	case sIFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case sIFBLK:
		m |= fs.ModeDevice
	case sIFIFO:
		m |= fs.ModeNamedPipe
	case sIFSOCK:
		m |= fs.ModeSocket
	}
	return m
}

// SendName is the name a --fake-super sender puts in the file list for a
// local one: the namespace a receiver as root keeps it in.
func SendName(local string) string {
	if strings.HasPrefix(local, rsyncPrefix) && !strings.HasPrefix(local, rsyncPercent) {
		return strings.TrimPrefix(local, rsyncPrefix)
	}
	return local
}

// LocalName is the name a --fake-super side keeps sent under: other
// namespaces than user go under user.rsync. (receive_xattr).
func LocalName(sent string) string {
	if strings.HasPrefix(sent, userPrefix) {
		return sent
	}
	return rsyncPrefix + sent
}

// fakeInfo is a file's stat as --fake-super reports it.
type fakeInfo struct {
	fs.FileInfo
	mode fs.FileMode
	sys  any
}

func (fi fakeInfo) Mode() fs.FileMode { return fi.mode }
func (fi fakeInfo) IsDir() bool       { return fi.mode.IsDir() }
func (fi fakeInfo) Sys() any          { return fi.sys }
