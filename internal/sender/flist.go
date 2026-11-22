package sender

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

type file struct {
	source  FileSource
	path    string
	Wpath   string
	regular bool
	isDir   bool

	// fields below are used by the receiver (TODO: unify)
	Name       string
	Length     int64
	ModTime    time.Time
	Mode       int32
	Uid        int32
	Gid        int32
	LinkTarget string
	Rdev       int32

	// xattrs is the index of the file's xattr list (-X), or -1.
	xattrs int
}

type fileList struct {
	TotalSize int64
	BuildTime time.Duration

	// hlinkFirst maps a hard-linked file's device and inode to the index,
	// in the list as sent, of the first file with them (protocol 30+).
	hlinkFirst map[[2]int64]int32
	Files      []file
	Sources    []FileSource
}

// A fileList must not be used after calling Close().
func (fl *fileList) Close() {
	for _, source := range fl.Sources {
		source.Close()
	}
	fl.Sources = nil
}

// readBufSize is how much we read from the local source per syscall while
// sending; larger than rsync.ChunkSize for I/O efficiency. The data is still
// split into wire tokens of at most rsync.ChunkSize (rsync's CHUNK_SIZE): a
// receiver rejects longer literal tokens ("invalid uncompressed token
// length"), so the token size is a wire-protocol constant.
const readBufSize = 256 * 1024

// readBufs recycles readBufSize buffers across files, so a peer that asks
// for many files does not cost fresh 256 KiB buffers per request.
var readBufs = sync.Pool{New: func() any { return new([readBufSize]byte) }}

var (
	lookupOnce      sync.Once
	lookupGroupOnce sync.Once
)

// getStrip operates on wire paths (slash space).
func getStrip(requested string) string {
	if requested == "/" {
		return ""
	}
	if strings.HasSuffix(requested, "/") {
		return strings.TrimPrefix(path.Clean(requested), "/") + "/"
	}
	return ""
}

type scopedWalker struct {
	st        *Transfer
	ioError   func(err error)
	conn      *rsyncwire.Conn
	fec       *rsyncwire.Buffer
	excl      *filterRuleList
	uidMap    map[int32]string
	gidMap    map[int32]string
	fileList  *fileList
	source    FileSource
	localDir  string
	requested string
	strip     string
	subdir    string
	prefix    string
	only      *onlySet // nil: everything goes
}

func (s *scopedWalker) walk() error {
	if s.source == nil {
		fi, err := os.Lstat(s.localDir)
		if err != nil {
			s.st.Logger.Printf("  Lstat(localDir=%q): %v", s.localDir, err)
			return fmt.Errorf("i/o error: requested module path is not accessible")
		}
		if fi.IsDir() {
			root, err := os.OpenRoot(s.localDir)
			if err != nil {
				s.st.Logger.Printf("  OpenRoot(localDir=%q): %v", s.localDir, err)
				return fmt.Errorf("i/o error: requested module path is not accessible")
			}
			s.source = newOSRootSource(root)
		} else {
			s.source = newSingleFileSource(s.localDir, fi)
		}
		s.fileList.Sources = append(s.fileList.Sources, s.source)
	}
	if s.subdir != "." {
		sub, err := newSubSource(s.source, s.subdir)
		if err != nil {
			s.st.Logger.Printf("  newSubSource(subdir=%q): %v", s.subdir, err)
			return fmt.Errorf("i/o error: requested module path is not accessible")
		}
		s.source = sub
	}

	rootname := s.requested
	// fs.WalkDir(root.FS(), …) does not accept absolute paths,
	// so make them relative by prepending a .
	if strings.HasPrefix(rootname, "/") {
		rootname = "." + rootname
	}
	root := path.Clean(rootname)
	if s.strip == "" && root != "." {
		// The requested path names one entry, not a folder's contents.
		// rsync looks at it with link_stat (lstat, flist.c
		// send_file_name): with -l a symbolic link there is sent as the
		// link, where fs.WalkDir stats its root and follows it. A path
		// that is not there is told to the client, as rsync does, not
		// only counted in io_error: the client could not tell "not
		// there" from "could not read".
		info, err := fs.Lstat(s.source.FS(), root)
		if err != nil {
			s.st.tellClient(rsyncwire.MsgErrorXfer, fmt.Sprintf("rsync: link_stat \"%s\" failed: %v\n", path.Join(s.subdir, root), errors.Unwrap(err)))
			s.ioError(err)
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 && s.st.Opts.PreserveLinks() {
			return s.walkFn(root, fs.FileInfoToDirEntry(info), nil)
		}
	}
	if err := fs.WalkDir(s.source.FS(), root, s.walkFn); err != nil {
		return err
	}
	return nil
}

func (s *scopedWalker) walkFn(path string, d fs.DirEntry, err error) error {
	logger := s.st.Logger // for convenience
	opts := s.st.Opts     // for convenience
	if opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
		logger.Printf("filepath.WalkFn(path=%s)", path)
	}
	var info fs.FileInfo
	if err == nil {
		info, err = d.Info()
	}
	fakeLink := false // a symlink kept as a regular file by --fake-super
	if err == nil && opts.FakeSuper() {
		info, fakeLink, err = s.st.fakeStat(s.source, path, info)
	}
	if err != nil {
		// set the I/O error flag, but keep walking
		s.ioError(err)
		return nil
	}

	if opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
		logger.Printf("isDir=%v, xferDirs=%v", info.Mode().IsDir(), opts.XferDirs())
	}
	if info.Mode().IsDir() && opts.XferDirs() == 0 {
		logger.Printf("skipping directory %s", path)
		return filepath.SkipDir
	}

	// Only ever transmit long names, like openrsync
	flags := byte(rsync.XMIT_LONG_NAME)

	name := path
	if s.strip != "" {
		if path+"/" == s.strip {
			// Transmit the top directory as ., like tridge rsync.
			name = "."
		} else {
			name = strings.TrimPrefix(name, s.strip)
		}
	}
	if s.prefix != "" {
		if path == "." {
			name = s.prefix
		} else {
			name = s.prefix + "/" + path
		}
	}
	if opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
		logger.Printf("Trim(path=%q) = %q", path, name)
	}
	if path == "." && s.prefix == "" {
		flags |= rsync.XMIT_TOP_DIR
	}
	// st.logger.Printf("flags for %q: %v", name, flags)

	if send, prune := s.only.allows(name, info.Mode().IsDir()); !send {
		if prune {
			return filepath.SkipDir
		}
		return nil // SkipDir on a file would skip the rest of its folder
	}

	if s.excl.matches(name) {
		return filepath.SkipDir
	}

	s.fileList.Files = append(s.fileList.Files, file{
		source:  s.source,
		path:    path,
		regular: info.Mode().IsRegular(),
		isDir:   info.IsDir(),
		Wpath:   name,
		Length:  info.Size(),
		xattrs:  -1,
	})

	s.fec.Reset()

	proto := s.conn.Session.Protocol
	xflags := uint16(flags)

	mode := int32(info.Mode() & os.ModePerm)
	// The bits Go keeps apart: rsync sends them in the mode, and a
	// receiver as root, or with --fake-super, keeps them.
	for _, b := range []struct {
		goBit   fs.FileMode
		unixBit int32
	}{{fs.ModeSetuid, 0o4000}, {fs.ModeSetgid, 0o2000}, {fs.ModeSticky, 0o1000}} {
		if info.Mode()&b.goBit != 0 {
			mode |= b.unixBit
		}
	}
	isDev := false
	isSpecial := false
	if info.Mode().IsDir() {
		mode |= rsync.S_IFDIR
	} else if info.Mode().IsRegular() {
		mode |= rsync.S_IFREG
	} else if info.Mode().Type()&os.ModeSymlink != 0 {
		mode |= rsync.S_IFLNK
		if runtime.GOOS == "windows" {
			// Go gives a symlink the file's permission bits on Windows
			// (0666). A Unix symlink is always 0777, and a receiver that
			// sees other bits tries to change a link's permissions, which
			// rsync for Windows (MSYS2) cannot do.
			mode = rsync.S_IFLNK | 0o777
		}
		// TODO: skip symlink if PreserveSymlinks is not set
	}

	if info.Mode().Type()&os.ModeCharDevice != 0 {
		mode |= rsync.S_IFCHR
		isDev = true
	} else if info.Mode().Type()&os.ModeDevice != 0 {
		mode |= rsync.S_IFBLK
		isDev = true
	}

	if info.Mode().Type()&os.ModeNamedPipe != 0 {
		mode |= rsync.S_IFIFO
		isSpecial = true
	}

	if info.Mode().Type()&os.ModeSocket != 0 {
		mode |= rsync.S_IFSOCK
		isSpecial = true
	}

	// From protocol 28 a device number goes as major and minor, the minor
	// in one byte when it fits.
	// Specials (FIFOs, sockets) carry a dummy one below protocol 31 only
	// (flist.c:send_file_entry).
	sendRdev := (opts.PreserveDevices() && isDev) || (opts.PreserveSpecials() && isSpecial && proto < 31)
	var rdevMajor, rdevMinor uint32
	if sendRdev && proto >= 28 {
		if isDev {
			rdevMajor, rdevMinor = rdevMajorMinor(info)
		}
		// From protocol 30 the minor is a varint, and bit 11 means
		// XMIT_GROUP_NAME_FOLLOWS.
		if proto < 30 && rdevMinor <= 0xff {
			xflags |= rsync.XMIT_RDEV_MINOR_8_pre30
		}
	}

	// Hard-link identity, as rsync's make_file picks it: below protocol 28
	// for every regular file, from 28 for anything but a folder with more
	// than one link. Only a server sends it: its client asked for -H and
	// expects it. A gokrazy client does not pass -H on, so its server does
	// not expect it.
	var sendHlink bool
	var hlinkDev, hlinkIno int64
	if opts.PreserveHardLinks() && opts.Server() {
		dev, ino, nlink, ok := hardLinkID(info)
		if !ok {
			// No inode numbers here. Give each file its own, so that a
			// receiver links nothing together.
			dev, ino, nlink = 0, int64(len(s.fileList.Files)), 1
		}
		if proto < 28 {
			sendHlink = info.Mode().IsRegular()
		} else {
			sendHlink = !info.IsDir() && nlink > 1
		}
		hlinkDev, hlinkIno = dev, ino
		if sendHlink && proto >= 28 {
			xflags |= rsync.XMIT_HLINKED
		}
	}
	// From protocol 30 the first file of a hard-link group goes in full,
	// marked XMIT_HLINK_FIRST; a later one names the first by its index in
	// the list as sent and leaves out the rest (flist.c:send_file_entry).
	firstHlinkNdx := int32(-1)
	if sendHlink && proto >= 30 {
		key := [2]int64{hlinkDev, hlinkIno}
		if s.fileList.hlinkFirst == nil {
			s.fileList.hlinkFirst = make(map[[2]int64]int32)
		}
		if first, ok := s.fileList.hlinkFirst[key]; ok {
			firstHlinkNdx = first
		} else {
			s.fileList.hlinkFirst[key] = int32(len(s.fileList.Files) - 1)
			xflags |= rsync.XMIT_HLINK_FIRST
		}
	}

	// From protocol 31 an mtime's nanoseconds follow it, when there are
	// any (flist.c:send_file_entry, XMIT_MOD_NSEC).
	var mtimeNsec int32
	if t := info.ModTime(); proto >= 31 && !t.IsZero() && t.Nanosecond() != 0 {
		mtimeNsec = int32(t.Nanosecond())
		xflags |= rsync.XMIT_MOD_NSEC
	}

	// 1.   status byte, from protocol 28 two bytes when any of the upper
	//      flags are set, or a varint with CF_VARINT_FLIST_FLAGS
	if s.conn.Session.CompatFlags&rsyncwire.CfVarintFlistFlags != 0 {
		s.fec.WriteVarint(int32(xflags))
	} else if proto >= 28 && xflags&0xff00 != 0 {
		xflags |= rsync.XMIT_EXTENDED_FLAGS
		s.fec.WriteByte(byte(xflags))
		s.fec.WriteByte(byte(xflags >> 8))
	} else {
		s.fec.WriteByte(byte(xflags))
	}

	// 2.   inherited filename length (optional, byte)
	// 3.   filename length (integer or byte, from protocol 30 a varint)
	s.fec.WriteVarint30(proto, int32(len(name)))

	// 4.   file (byte array)
	s.fec.WriteString(name)

	if firstHlinkNdx >= 0 {
		// A later file of a hard-link group: the receiver takes
		// everything else from the first.
		s.fec.WriteVarint(firstHlinkNdx)
		s.conn.WriteString(s.fec.String())
		return nil
	}

	// 5.   file length (long)
	size := info.Size()
	if info.Mode().IsDir() {
		// tmpfs returns non-4K sizes for directories. Override with
		// 4096 to make the tests succeed regardless of the /tmp file
		// system type.
		size = 4096
	}
	s.fec.WriteVarlong30(proto, size, 3)

	s.fileList.TotalSize += size

	// 6.   file modification time (optional, integer; from protocol 30 a
	//      varlong, which does not run out in 2038)
	// A file system without times (an fs.FS such as fstest.MapFS) gives
	// Go's zero time, the year 1, which receivers cannot set (Windows
	// refuses it). Send the Unix epoch instead.
	var mtime int64
	if t := info.ModTime(); !t.IsZero() {
		mtime = t.Unix()
	}
	if proto >= 30 {
		s.fec.WriteVarlong(mtime, 4)
	} else {
		s.fec.WriteInt32(int32(mtime))
	}
	if xflags&rsync.XMIT_MOD_NSEC != 0 {
		s.fec.WriteVarint(mtimeNsec)
	}

	// 7.   file mode (optional, mode_t, integer)
	s.fec.WriteInt32(mode)

	if opts.PreserveUid() {
		uid, ok := uidFromFileInfo(info)
		if ok {
			if _, ok := s.uidMap[uid]; !ok && uid != 0 {
				u, err := user.LookupId(strconv.Itoa(int(uid)))
				if err != nil {
					lookupOnce.Do(func() {
						logger.Printf("lookup(%d) = %v", uid, err)
					})
				} else {
					s.uidMap[uid] = u.Username
				}
			}
		}
		// 8.   if -o, the user id (integer; from protocol 30 a varint)
		s.fec.WriteVarint30(proto, uid)
	}

	if opts.PreserveGid() {
		gid, ok := gidFromFileInfo(info)
		if ok {
			if _, ok := s.gidMap[gid]; !ok && gid != 0 {
				g, err := user.LookupGroupId(strconv.Itoa(int(gid)))
				if err != nil {
					lookupGroupOnce.Do(func() {
						logger.Printf("lookupgroup(%d) = %v", gid, err)
					})
				} else {
					s.gidMap[gid] = g.Name
				}
			}
		}
		// 9.   if -g, the group id (integer; from protocol 30 a varint)
		s.fec.WriteVarint30(proto, gid)
	}

	if sendRdev {
		// 10.  if a special file and -D, the device “rdev” type: one
		//      integer below protocol 28, then major and minor
		if proto < 28 {
			rdev, _ := rdevFromFileInfo(info)
			s.fec.WriteInt32(rdev)
		} else {
			s.fec.WriteVarint30(proto, int32(rdevMajor))
			switch {
			case proto >= 30:
				s.fec.WriteVarint(int32(rdevMinor))
			case xflags&rsync.XMIT_RDEV_MINOR_8_pre30 != 0:
				s.fec.WriteByte(byte(rdevMinor))
			default:
				s.fec.WriteInt32(int32(rdevMinor))
			}
		}
	}

	if opts.PreserveLinks() && info.Mode().Type()&os.ModeSymlink != 0 {
		// 11.  if a symbolic link and -l, the link target's length (integer)
		// 12.  if a symbolic link and -l, the link target (byte array)

		readlink := s.source.Readlink
		if fakeLink {
			readlink = func(p string) (string, error) { return readPlaceholder(s.source, p) }
		}
		target, err := readlink(path)
		if err != nil {
			return err // TODO
		}
		s.fec.WriteVarint30(proto, int32(len(target)))
		s.fec.WriteString(target)
	}

	if sendHlink && proto < 30 {
		// 13.  if -H, the device (1-incremented, so never 0) and inode
		s.fec.WriteInt64(hlinkDev + 1)
		s.fec.WriteInt64(hlinkIno)
	}

	// Before protocol 28, every entry carried a checksum, all zeros for
	// anything but a regular file.
	if opts.AlwaysChecksum() && (info.Mode().IsRegular() || proto < 28) {
		checksum := make([]byte, s.st.strong().FileLen())
		if info.Mode().IsRegular() {
			f, err := s.source.Open(path)
			if err != nil {
				return err
			}
			checksum, err = s.st.strong().FileChecksum(f)
			f.Close()
			if err != nil {
				return err
			}
		} else {
			// send empty md4 checksum
		}
		s.fec.WriteString(string(checksum))
	}

	// With -X every entry carries its xattr list (flist.c:send_file_list,
	// send_xattr). One that cannot be read goes empty, with io_error set.
	if s.st.xattrs() {
		list, err := s.st.readXattrs(s.source, path, info.Mode().IsRegular() || info.IsDir())
		if err != nil {
			s.st.Logger.Printf("reading xattrs of %s: %v", name, err)
			s.st.noteIOError(ioErrGeneral)
			list = nil
		}
		b, ndx := s.st.xattrCache.AppendSend(nil, list)
		s.fec.WriteString(string(b))
		s.fileList.Files[len(s.fileList.Files)-1].xattrs = ndx
	}

	s.conn.WriteString(s.fec.String())

	// The status byte may consist of the following bits and determines which of the optional fields are transmitted.

	// 0x01    A top-level directory.  (Only applies to directory files.)  If specified, the matching local directory is for deletions.
	// 0x02    Do not send the file mode: it is a repeat of the last file's mode.
	// 0x08    Like 0x02, but for the user id.
	// 0x10    Like 0x02, but for the group id.
	// 0x20    Inherit some of the prior file name.  Enables the inherited filename length transmission.
	// 0x40    Use full integer length for file name.  Otherwise, use only the byte length.
	// 0x80    Do not send the file modification time: it is a repeat of the last file's.

	// If the status byte is zero, the file-list has terminated.

	// --dirs (-d) without -r: the folder a request names with a trailing
	// slash, or ".", goes with its own entries, one level deep; it is the
	// only entry sent as ".". Any other folder goes as an entry only.
	// (rsync flist.c send_file_list: SLASH_ENDING_NAME, DOTDIR_NAME.)
	if info.Mode().IsDir() && !opts.Recurse() && name != "." {
		return filepath.SkipDir
	}

	return nil
}

// rsync/flist.c:send_file_list
func (st *Transfer) SendFileList(localDir string, paths []string, excl *filterRuleList) (_ *fileList, err error) {
	var fileList fileList
	// The caller closes the list's sources (open module folders) once it
	// has sent the files; when building or sending the list fails, close
	// them here. Windows cannot remove a folder that is still open.
	defer func() {
		if err != nil {
			fileList.Close()
		}
	}()
	fec := &rsyncwire.Buffer{}

	uidMap := make(map[int32]string)
	gidMap := make(map[int32]string)

	// TODO: flush in between to keep the pipes filled when traversal takes long

	// TODO: handle info == nil case (permission denied?): should set an i/o
	// error flag, but traversal should continue

	st.Logger.Printf("building file list")
	if st.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
		st.Logger.Printf("sendFileList()")
	}
	ioErrors := int32(0)
	only := newOnlySet(st.Only)

	ioError := func(err error) {
		if os.IsNotExist(err) {
			st.Logger.Printf("file vanished: %v", err)
		} else {
			st.Logger.Printf("lstat: %v", err)
		}
		ioErrors = 1
		st.noteIOError(ioErrGeneral)
	}

	for _, requested := range paths {
		subdir := "."
		prefix := ""
		local := localDir
		if local == rsync.FileSystemRoot {
			// Implicit module (/) and absolute requested path (/tmp/foo/),
			// turn the path into the local directory and request /.
			local = filepath.Clean(requested)
			if strings.HasSuffix(requested, "/") {

			} else {
				prefix = filepath.Base(requested)
			}
			requested = "/"
		} else if !strings.HasSuffix(requested, "/") {
			st.Logger.Printf("  handling requested=%q", requested)
			clean := path.Clean(strings.TrimPrefix(requested, "/"))
			subdir = path.Dir(clean)
			requested = path.Base(clean)
			st.Logger.Printf("  -> subdir=%q, requested=%q", subdir, requested)
		}

		if st.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
			st.Logger.Printf("  path %q (local dir %q)", requested, local)
		}
		// st.Logger.Printf("getRootStrip(requested=%q, localDir=%q", requested, localDir)
		strip := getStrip(requested)
		// st.Logger.Printf("root=%q, strip=%q", root, strip)
		if st.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
			st.Logger.Printf("  fs.Walk(%q, %q), strip=%q", local, requested, strip)
		}

		sw := &scopedWalker{
			st:        st,
			conn:      st.Conn,
			fec:       fec,
			excl:      excl,
			uidMap:    uidMap,
			gidMap:    gidMap,
			fileList:  &fileList,
			source:    st.Source,
			ioError:   ioError,
			localDir:  local,
			requested: requested,
			strip:     strip,
			subdir:    subdir,
			prefix:    prefix,
			only:      only,
		}
		if err := sw.walk(); err != nil {
			return nil, err
		}
	}
	// A listed path no walk came to is not there. rsync's --files-from
	// says so (link_stat failed) and sets io_error: the run must not look
	// complete.
	for _, name := range only.missing() {
		st.tellClient(rsyncwire.MsgErrorXfer, fmt.Sprintf("rsync: link_stat \"%s\" failed: %v\n", name, fs.ErrNotExist))
		ioError(&fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist})
	}

	if st.Opts.InfoGTE(rsyncopts.INFO_PROGRESS, 1) {
		st.Logger.Printf("%d files to consider", len(fileList.Files))
	}

	fec.Reset()

	// End of the list (flist.c:write_end_of_flist). From protocol 30 with
	// a safe file list, an I/O error ends it instead, carrying the error.
	proto := st.Conn.Session.Protocol
	errInTrailer := proto >= 30 && ioErrors != 0 && st.Conn.Session.SafeFlist()
	if st.Conn.Session.CompatFlags&rsyncwire.CfVarintFlistFlags != 0 {
		// A 0 and then the I/O error, or 0.
		fec.WriteVarint(0)
		if errInTrailer {
			fec.WriteVarint(ioErrors)
		} else {
			fec.WriteVarint(0)
		}
	} else if errInTrailer {
		fec.WriteByte(rsync.XMIT_EXTENDED_FLAGS)
		fec.WriteByte(rsync.XMIT_IO_ERROR_ENDLIST >> 8)
		fec.WriteVarint(ioErrors)
	} else {
		const endOfFileList = 0
		fec.WriteByte(endOfFileList)
	}

	const endOfSet = 0
	if st.Opts.PreserveUid() {
		for uid, name := range uidMap {
			fec.WriteVarint30(proto, uid)
			fec.WriteByte(byte(len(name)))
			fec.WriteString(name)
		}
		fec.WriteVarint30(proto, endOfSet)
	}
	if st.Opts.PreserveGid() {
		for gid, name := range gidMap {
			fec.WriteVarint30(proto, gid)
			fec.WriteByte(byte(len(name)))
			fec.WriteString(name)
		}
		fec.WriteVarint30(proto, endOfSet)
	}

	// The io_error flag: below protocol 30 always, after the id lists.
	// From 30, a message when the trailer could not carry it.
	if proto < 30 {
		fec.WriteInt32(ioErrors)
	}

	if err := st.Conn.WriteString(fec.String()); err != nil {
		return nil, err
	}
	if proto >= 30 && ioErrors != 0 && !errInTrailer {
		if err := st.Conn.WriteMsgInt(rsyncwire.MsgIOError, ioErrors); err != nil {
			return nil, err
		}
	}

	return &fileList, nil
}
