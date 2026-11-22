package receiver

import (
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/flistcmp"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// rsync/flist.c:flist_sort_and_clean. The sender sorts the same way and
// both sides then name files by index, so the order must be rsync's.
func sortFileList(proto int32, fileList []*File) {
	sort.SliceStable(fileList, func(i, j int) bool {
		return flistcmp.Compare(proto, fileList[i].entry(), fileList[j].entry()) < 0
	})
}

// entry is f as the sender sorts it: by the name as sent, with forward
// slashes. f.Name is a local path, which on Windows has backslashes, and
// rsync's order splits names at '/' only.
func (f *File) entry() flistcmp.Entry {
	return flistcmp.Entry{Name: filepath.ToSlash(f.Name), IsDir: f.Mode&rsync.S_IFMT == rsync.S_IFDIR}
}

// maxWireNsec is rsync's MAX_WIRE_NSEC.
const maxWireNsec = 999999999

type File struct {
	Name       string
	Length     int64
	ModTime    time.Time
	Mode       int32
	Uid        int32
	Gid        int32
	LinkTarget string
	Rdev       int32
	Checksum   [rsyncchecksum.Size]byte

	// State that later entries refer back to, carried from entry to entry
	// as rsync's recv_file_entry keeps it in statics: the last device's
	// major number (protocol 28+) and the last hard-linked file's device.
	rdevMajor uint32
	hlinkDev  int64
}

// FileMode converts from the Linux permission bits to Go’s permission bits.
func (f *File) FileMode() fs.FileMode {
	ret := fs.FileMode(f.Mode) & fs.ModePerm

	mode := f.Mode & rsync.S_IFMT
	switch mode {
	case rsync.S_IFCHR:
		ret |= fs.ModeCharDevice
	case rsync.S_IFBLK:
		ret |= fs.ModeDevice
	case rsync.S_IFIFO:
		ret |= fs.ModeNamedPipe
	case rsync.S_IFSOCK:
		ret |= fs.ModeSocket
	case rsync.S_IFLNK:
		ret |= fs.ModeSymlink
	case rsync.S_IFDIR:
		ret |= fs.ModeDir
	}

	return ret
}

// rsync/flist.c:receive_file_entry
func (rt *Transfer) receiveFileEntry(flags uint16, last *File) (*File, error) {
	f := &File{rdevMajor: last.rdevMajor, hlinkDev: last.hlinkDev}
	proto := rt.Conn.Session.Protocol

	var l1 int
	if flags&rsync.XMIT_SAME_NAME != 0 {
		l, err := rt.Conn.ReadByte()
		if err != nil {
			return nil, err
		}
		l1 = int(l)
	}

	var l2 int
	if flags&rsync.XMIT_LONG_NAME != 0 {
		l, err := rt.Conn.ReadVarint30()
		if err != nil {
			return nil, err
		}
		l2 = int(l)
	} else {
		l, err := rt.Conn.ReadByte()
		if err != nil {
			return nil, err
		}
		l2 = int(l)
	}
	// linux/limits.h
	const PATH_MAX = 4096
	if l2 < 0 || l2 >= PATH_MAX-l1 {
		const lastname = ""
		return nil, fmt.Errorf("overflow: flags=0x%x l1=%d l2=%d lastname=%s",
			flags, l1, l2, lastname)
	}
	b := make([]byte, l1+l2)
	readb := b
	if l1 > 0 {
		copy(b, []byte(last.Name))
		readb = b[l1:]
	}
	if _, err := io.ReadFull(rt.Conn.Reader, readb); err != nil {
		return nil, err
	}
	// rsync 3.4 aborts on a name that could leave the destination, and on
	// one the client did not ask for (flist.c:recv_file_entry).
	if name := string(b); name != "" && unsafeName(name) {
		return nil, fmt.Errorf("ABORTING due to unsafe pathname from sender: %s", name)
	} else if name != "" && !rt.Implied.Allows(name) {
		return nil, fmt.Errorf("ERROR: rejecting unrequested file-list name: %s", name)
	}
	f.Name = filepath.Clean(string(b))

	if proto >= 30 && rt.Opts.PreserveHardlinks && flags&rsync.XMIT_HLINKED != 0 {
		// A protocol 30 sender abbreviates the later files of a
		// hard-link group to a reference. Neither gokrazy/rsync receiver
		// takes -H (the client does not pass it on, the daemon refuses
		// it), so this cannot be asked for.
		return nil, fmt.Errorf("hard-link data at protocol %d is not supported", proto)
	}

	length, err := rt.Conn.ReadVarlong30(3)
	if err != nil {
		return nil, err
	}
	if length < 0 {
		return nil, fmt.Errorf("offset underflow: the length of %s is negative", f.Name)
	}
	f.Length = length

	// XMIT_SAME_TIME repeats the seconds only: the nanoseconds come with
	// XMIT_MOD_NSEC (protocol 31+), and are 0 without it.
	var modTime int64
	if flags&rsync.XMIT_SAME_TIME != 0 {
		modTime = last.ModTime.Unix()
	} else if proto >= 30 {
		if modTime, err = rt.Conn.ReadVarlong(4); err != nil {
			return nil, err
		}
	} else {
		t, err := rt.Conn.ReadInt32()
		if err != nil {
			return nil, err
		}
		modTime = int64(t)
	}
	var nsec int32
	if flags&rsync.XMIT_MOD_NSEC != 0 && proto >= 31 {
		if nsec, err = rt.Conn.ReadVarint(); err != nil {
			return nil, err
		}
		if nsec < 0 || nsec > maxWireNsec {
			return nil, fmt.Errorf("invalid mtime nanoseconds %d for %s", nsec, f.Name)
		}
	}
	f.ModTime = time.Unix(modTime, int64(nsec))

	if flags&rsync.XMIT_SAME_MODE != 0 {
		f.Mode = last.Mode
	} else {
		mode, err := rt.Conn.ReadInt32()
		if err != nil {
			return nil, err
		}
		f.Mode = mode
	}

	if rt.Opts.PreserveUid {
		if flags&rsync.XMIT_SAME_UID != 0 {
			f.Uid = last.Uid
		} else {
			uid, err := rt.Conn.ReadVarint30()
			if err != nil {
				return nil, err
			}
			f.Uid = uid
			// With incremental recursion a protocol 30 sender puts the
			// name right after the id, instead of in the list after the
			// file list.
			if proto >= 30 && flags&rsync.XMIT_USER_NAME_FOLLOWS != 0 {
				if err := rt.skipIdName(); err != nil {
					return nil, err
				}
			}
		}
	}

	if rt.Opts.PreserveGid {
		if flags&rsync.XMIT_SAME_GID != 0 {
			f.Gid = last.Gid
		} else {
			gid, err := rt.Conn.ReadVarint30()
			if err != nil {
				return nil, err
			}
			f.Gid = gid
			if proto >= 30 && flags&rsync.XMIT_GROUP_NAME_FOLLOWS != 0 {
				if err := rt.skipIdName(); err != nil {
					return nil, err
				}
			}
		}
	}

	mode := f.Mode & rsync.S_IFMT
	isDev := mode == rsync.S_IFCHR || mode == rsync.S_IFBLK
	isSpecial := mode == rsync.S_IFIFO || mode == rsync.S_IFSOCK
	isLink := mode == rsync.S_IFLNK
	isReg := mode == rsync.S_IFREG

	// Specials carry a dummy device number below protocol 31 only.
	if (rt.Opts.PreserveDevices && isDev) || (rt.Opts.PreserveSpecials && isSpecial && proto < 31) {
		if proto < 28 {
			if flags&rsync.XMIT_SAME_RDEV_pre28 != 0 {
				f.Rdev = last.Rdev
			} else {
				rdev, err := rt.Conn.ReadInt32()
				if err != nil {
					return nil, err
				}
				f.Rdev = rdev
			}
		} else {
			if flags&rsync.XMIT_SAME_RDEV_MAJOR == 0 {
				major, err := rt.Conn.ReadVarint30()
				if err != nil {
					return nil, err
				}
				f.rdevMajor = uint32(major)
			}
			var minor uint32
			if proto >= 30 {
				m, err := rt.Conn.ReadVarint()
				if err != nil {
					return nil, err
				}
				minor = uint32(m)
			} else if flags&rsync.XMIT_RDEV_MINOR_8_pre30 != 0 {
				b, err := rt.Conn.ReadByte()
				if err != nil {
					return nil, err
				}
				minor = uint32(b)
			} else {
				m, err := rt.Conn.ReadInt32()
				if err != nil {
					return nil, err
				}
				minor = uint32(m)
			}
			f.Rdev = makeRdev(f.rdevMajor, minor)
		}
	}

	if rt.Opts.PreserveLinks && isLink {
		length, err := rt.Conn.ReadVarint30()
		if err != nil {
			return nil, err
		}
		// rsync/flist.c:recv_file_entry
		if length <= 0 || length > PATH_MAX {
			return nil, fmt.Errorf("overflow: symlink target length=%d", length)
		}
		b := make([]byte, length)
		if _, err := io.ReadFull(rt.Conn.Reader, b); err != nil {
			return nil, err
		}
		f.LinkTarget = string(b)
	}

	// Hard-link identity, protocols 26 to 29: below 28 for every regular
	// file, from 28 for entries flagged XMIT_HLINKED. It is read to stay in
	// step with the sender; this receiver does not create hard links yet.
	if rt.Opts.PreserveHardlinks && proto < 30 && (proto < 28 && isReg || proto >= 28 && flags&rsync.XMIT_HLINKED != 0) {
		if proto < 28 || flags&rsync.XMIT_SAME_DEV_pre30 == 0 {
			dev, err := rt.Conn.ReadInt64()
			if err != nil {
				return nil, err
			}
			f.hlinkDev = dev - 1 // sent 1-incremented so it is never 0
		}
		if _, err := rt.Conn.ReadInt64(); err != nil { // inode
			return nil, err
		}
	}

	// Before protocol 28, every entry carried a checksum, all zeros for
	// anything but a regular file.
	if rt.Opts.AlwaysChecksum && (isReg || proto < 28) {
		// As long as the file checksum is: 16 bytes for MD4 and MD5, 8 for
		// xxh64 and xxh3.
		if _, err := io.ReadFull(rt.Conn.Reader, f.Checksum[:rsyncchecksum.NewStrong(rt.Conn.Session, 0).FileLen()]); err != nil {
			return nil, err
		}
	}

	switch f.Mode & rsync.S_IFMT {
	case rsync.S_IFREG, rsync.S_IFDIR, rsync.S_IFLNK, rsync.S_IFCHR, rsync.S_IFBLK, rsync.S_IFIFO, rsync.S_IFSOCK:
	default:
		return nil, fmt.Errorf("invalid file mode 0%o for %s", f.Mode, f.Name)
	}
	// "." is the transfer root: as anything but a folder it could replace
	// the destination folder.
	if f.Name == "." && f.Mode&rsync.S_IFMT != rsync.S_IFDIR {
		return nil, fmt.Errorf("ERROR: rejecting non-directory transfer-root entry: %s", f.Name)
	}
	return f, nil
}

// skipIdName reads past a user or group name sent after its id
// (flist.c:recv_user_name, recv_group_name). gokrazy/rsync maps ids by the
// lists after the file list.
func (rt *Transfer) skipIdName() error {
	n, err := rt.Conn.ReadByte()
	if err != nil {
		return err
	}
	_, err = io.CopyN(io.Discard, rt.Conn.Reader, int64(n))
	return err
}

// rsync/flist.c:recv_file_list
func (rt *Transfer) ReceiveFileList() ([]*File, error) {
	if rt.Opts.Progress {
		fmt.Fprintln(rt.Env.Stdout, "receiving file list...")
		fmt.Fprint(rt.Env.Stdout, "0 files to consider")
	}
	lastFileEntry := new(File)
	var fileList []*File
	var endListIOErrors int32
	for {
		// With CF_VARINT_FLIST_FLAGS the flags are a varint, and 0 ends
		// the list, followed by the I/O error. Otherwise one byte, two
		// from protocol 28 with XMIT_EXTENDED_FLAGS.
		var flags uint16
		if rt.Conn.Session.CompatFlags&rsyncwire.CfVarintFlistFlags != 0 {
			v, err := rt.Conn.ReadVarint()
			if err != nil {
				return nil, err
			}
			if v == 0 {
				e, err := rt.Conn.ReadVarint()
				if err != nil {
					return nil, err
				}
				endListIOErrors = e & 7 // IOERR_VALID_MASK
				break
			}
			if v&^0xffff != 0 {
				return nil, fmt.Errorf("unsupported file-list flags %#x", v)
			}
			flags = uint16(v)
		} else {
			b, err := rt.Conn.ReadByte()
			if err != nil {
				return nil, err
			}
			if b == 0 {
				break
			}
			flags = uint16(b)
			if rt.Conn.Session.Protocol >= 28 && flags&rsync.XMIT_EXTENDED_FLAGS != 0 {
				b, err := rt.Conn.ReadByte()
				if err != nil {
					return nil, err
				}
				flags |= uint16(b) << 8
			}
			// From protocol 30 with a safe file list, an I/O error can end
			// the list, followed by the error (flist.c:recv_file_list).
			if rt.Conn.Session.Protocol >= 30 && flags == rsync.XMIT_EXTENDED_FLAGS|rsync.XMIT_IO_ERROR_ENDLIST {
				if !rt.Conn.Session.SafeFlist() {
					return nil, fmt.Errorf("invalid flist flag: %#x", flags)
				}
				v, err := rt.Conn.ReadVarint()
				if err != nil {
					return nil, err
				}
				endListIOErrors = v
				break
			}
		}

		f, err := rt.receiveFileEntry(flags, lastFileEntry)
		if err != nil {
			return nil, err
		}
		lastFileEntry = f
		// TODO: include depth in output?
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
			rt.Logger.Printf("[Receiver] i=%d ? %s mode=%o len=%d uid=%d gid=%d flags=?",
				len(fileList),
				f.Name,
				f.Mode,
				f.Length,
				f.Uid,
				f.Gid)
		}
		fileList = append(fileList, f)
		if rt.Opts.OnFile != nil {
			rt.Opts.OnFile(f)
		}
		if rt.Opts.Progress && len(fileList)%100 == 0 {
			fmt.Fprintf(rt.Env.Stdout, "\r%d files to consider", len(fileList))
		}
	}
	if rt.Opts.Progress {
		fmt.Fprintf(rt.Env.Stdout, "\r%d files to consider\n", len(fileList))
	}

	sortFileList(rt.Conn.Session.Protocol, fileList)

	if rt.Opts.PreserveUid || rt.Opts.PreserveGid {
		// receive the uid/gid list
		users, groups, err := rt.RecvIdList()
		if err != nil {
			return nil, err
		}
		rt.Users = users
		rt.Groups = groups
	}

	// read the i/o error flag: below protocol 30 after the lists. From 30
	// it came at the end of the list, or comes as MSG_IO_ERROR.
	ioErrors := endListIOErrors
	if rt.Conn.Session.Protocol < 30 {
		v, err := rt.Conn.ReadInt32()
		if err != nil {
			return nil, err
		}
		ioErrors = v
	} else if rt.Env != nil {
		ioErrors |= rt.Env.IOErrors.Load()
	}
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 2) {
		rt.Logger.Printf("ioErrors: %v", ioErrors)
	}
	rt.IOErrors = ioErrors
	if rt.Env != nil {
		rt.Env.IOErrors.Or(ioErrors)
	}

	return fileList, nil
}
