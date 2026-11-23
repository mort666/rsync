package receiver

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsynccommon"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/safelink"
)

// rsync/generator.c:generate_files()
func (rt *Transfer) GenerateFiles(fileList []*File) error {
	phase := 0
	for idx, f := range fileList {
		if _, ok := rt.hlinkLater[f]; ok {
			continue // linked to its group's first at the end
		}
		if !rt.listOnly() {
			rt.genXattr = rt.checkXattrs(f)
		}
		if err := rt.recvGenerator(idx, f); err != nil {
			return err
		}
		if err := rt.reportXattrs(idx); err != nil {
			return err
		}
	}
	phase++
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
		rt.Logger.Printf("generateFiles phase=%d", phase)
	}
	if err := rt.Conn.WriteNdx(rsyncwire.NdxDone); err != nil {
		return err
	}

	// The redo pass: ask again for each file that failed verification,
	// until the receiver reaches phase 1 (generator.c:generate_files,
	// check_for_finished_files). The quick check and the options that
	// leave files alone are off for it, as rsync turns them off.
	if rt.redo != nil {
		rt.genRedoing = true
		for idx := range rt.redo {
			rt.flushRecvMsgs()
			if err := rt.recvGenerator(idx, fileList[idx]); err != nil {
				return err
			}
		}
		rt.genRedoing = false
		rt.flushRecvMsgs()
	}
	phase++
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
		rt.Logger.Printf("generateFiles phase=%d", phase)
	}
	if err := rt.Conn.WriteNdx(rsyncwire.NdxDone); err != nil {
		return err
	}

	// From protocol 29 a third phase is for --delay-updates. Without it,
	// rsync closes the phase right away (EARLY_DELAY_DONE_MSG), and so do
	// we.
	if rt.Conn.Session.Protocol >= 29 {
		phase++
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			rt.Logger.Printf("generateFiles phase=%d", phase)
		}
		if err := rt.Conn.WriteNdx(rsyncwire.NdxDone); err != nil {
			return err
		}
	}

	// NOTE: touchUpDirs is called from [Transfer.Do]
	// so that both goroutines (generator and receiver)
	// have finished before we set final permissions.

	if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
		rt.Logger.Printf("generateFiles finished")
	}
	return nil
}

func (rt *Transfer) touchUpDirs() error {
	for _, d := range rt.retouchDirs {
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_TIME, 2) {
			rt.Logger.Printf("touchUpDirs: %s (%v)", d.f.Name, d.perm)
		}
		if err := rt.setPerms(d.f, d.perm); err != nil {
			return err
		}
	}
	return nil
}

// rsync/generator.c:skip_file
func (rt *Transfer) skipFile(f *File, st os.FileInfo) (bool, error) {
	if rt.genRedoing || st.Size() != f.Length {
		return false, nil
	}

	if rt.Opts.AlwaysChecksum {
		checksum, err := rt.strong().RootFileChecksum(rt.DestRoot, f.Name)
		if err != nil {
			return false, err
		}
		return bytes.Equal(f.Checksum, checksum), nil
	}

	if rt.Opts.SizeOnly {
		return true, nil
	}

	if rt.Opts.IgnoreTimes {
		return false, nil
	}

	return rt.modTimeEqual(st.ModTime(), f.ModTime), nil
}

// modTimeEqual is the quick check's comparison (util1.c:same_time): whole
// seconds by default, within --modify-window seconds of each other, or
// below 0 to the nanosecond. Setting times compares exactly
// (rsync.c:same_mtime).
func (rt *Transfer) modTimeEqual(a, b time.Time) bool {
	w := rt.Opts.ModifyWindow
	switch {
	case w < 0:
		return a.Equal(b)
	case w == 0:
		return a.Unix() == b.Unix()
	}
	d := a.Unix() - b.Unix()
	return -int64(w) <= d && d <= int64(w)
}

// setPerms is set_file_attrs for a file this side did not just write: its
// times are set unless they are the same within --modify-window, whole
// seconds by default (rsync.c:same_mtime).
func (rt *Transfer) setPerms(f *File, perm fs.FileMode) error {
	return rt.setAttrs(f, perm, false)
}

// setPermsAccurate is set_file_attrs with ATTRS_ACCURATE_TIME, for a file
// just written or checked by checksum: its times are set unless they are
// the same to the nanosecond.
func (rt *Transfer) setPermsAccurate(f *File, perm fs.FileMode) error {
	return rt.setAttrs(f, perm, true)
}

// rsync/rsync.c:set_perms
func (rt *Transfer) setAttrs(f *File, perm fs.FileMode, accurate bool) error {
	if rt.Opts.DryRun {
		return nil
	}

	st, err := rt.DestRoot.Lstat(f.Name)
	if err != nil {
		return err
	}

	// Under --fake-super a symlink is a regular file, which takes times.
	isLink := f.Mode&rsync.S_IFMT == rsync.S_IFLNK && !rt.Opts.FakeSuper
	isDir := f.Mode&rsync.S_IFMT == rsync.S_IFDIR
	sameTime := rt.modTimeEqual(st.ModTime(), f.ModTime)
	if accurate {
		sameTime = st.ModTime().Equal(f.ModTime)
	}
	if rt.Opts.PreserveTimes &&
		!(isDir && rt.Opts.OmitDirTimes) &&
		!sameTime {
		set := rt.DestRoot.Chtimes
		// rsync sets times without following a link, so with -K a
		// folder's times go on the link standing for it.
		if isLink || st.Mode()&fs.ModeSymlink != 0 {
			set = func(name string, _, mtime time.Time) error { return rt.lchtimes(name, mtime) }
		}
		if err := set(f.Name, f.ModTime, f.ModTime); err != nil {
			return err
		}
	}

	if rt.Opts.FakeSuper {
		return rt.setFakeStat(f, perm, st)
	}

	_, err = rt.setUid(f, st)
	if err != nil {
		return err
	}

	if !isLink {
		if st.Mode().Perm() != perm { // only call Chmod if the permissions actually differ
			if err := rt.DestRoot.Chmod(f.Name, perm); err != nil {
				return err
			}
		}
	}

	return nil
}

// rsync/generator.c:recv_generator
func (rt *Transfer) recvGenerator(idx int, f *File) error {
	if rt.listOnly() {
		fmt.Fprintf(rt.Env.Stdout, "%s %11.0f %s %s\n",
			f.FileMode().String(),
			float64(f.Length), // TODO: rsync prints decimal separators
			f.ModTime.Format("2006/01/02 15:04:05"),
			f.Name)
		return nil
	}
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
		rt.Logger.Printf("recv_generator(f=%+v)", f)
	}

	local := filepath.Join(rt.Dest, f.Name)
	st, err := rt.lstat(f.Name)

	mode := f.Mode & rsync.S_IFMT
	// --existing and --ignore-existing (generator.c:recv_generator). A
	// folder the destination has is gone into either way.
	if rt.Opts.IgnoreNonExisting && !rt.genRedoing && os.IsNotExist(err) {
		kind := "file"
		if mode == rsync.S_IFDIR {
			kind = "directory"
		}
		rt.skipInfo(fmt.Sprintf("not creating new %s \"%s\"", kind, f.Name))
		return nil
	}
	if rt.Opts.IgnoreExisting && !rt.genRedoing && err == nil && !(mode == rsync.S_IFDIR && st.IsDir()) {
		rt.skipInfo(f.Name + " exists")
		return nil
	}
	if mode == rsync.S_IFDIR && rt.Opts.KeepDirlinks && err == nil && st.Mode()&fs.ModeSymlink != 0 {
		// -K: a symlink to a folder stands for the folder
		// (generator.c:gen_entry_stat with follow_dirlinks).
		if target, terr := rt.DestRoot.Stat(f.Name); terr == nil && target.IsDir() {
			st = target
		}
	}
	if mode == rsync.S_IFDIR {
		// What the item reports on: the folder that was there, if any.
		var itemSt os.FileInfo
		var dirFlags uint32
		if err == nil && st.IsDir() {
			itemSt = st
		} else {
			dirFlags = rsync.ITEM_LOCAL_CHANGE
		}
		if rt.Opts.DryRun {
			return rt.itemize(idx, f, itemSt, dirFlags, "")
		}
		if err == nil && !st.IsDir() {
			// A file (not a directory) with this name exists. Delete it so that
			// we can create a directory instead.
			if err := rt.DestRoot.Remove(f.Name); err != nil {
				return fmt.Errorf("unlinking to make room for directory: %v", err)
			}
			err = fmt.Errorf("file removed")
		}
		var destSt os.FileInfo
		if err == nil {
			destSt = st
		}
		perm := rt.destPerm(f, destSt)
		if err != nil {
			if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
				rt.Logger.Printf("MkdirAll(%s, %v)", f.Name, perm)
			}
			if err := rt.DestRoot.MkdirAll(f.Name, perm); err != nil {
				// TODO: EEXIST is okay
				return err
			}
			// fallthrough to setPerms and return nil
		}
		if perm&syscall.S_IWUSR == 0 {
			// The directory is lacking write permission,
			// so we need to create it writeable as long as
			// we are creating files inside that directory.
			// GenerateFiles will fix permissions afterwards.
			rt.retouchDirs = append(rt.retouchDirs, retouchDir{
				f:    f,
				perm: perm,
			})
			perm |= syscall.S_IWUSR
		}
		if err := rt.setPerms(f, perm); err != nil {
			return err
		}
		return rt.itemize(idx, f, itemSt, dirFlags, "")
	}

	var destSt os.FileInfo
	if err == nil && !st.IsDir() {
		destSt = st
	}
	perm := rt.destPerm(f, destSt)

	if rt.Opts.PreserveLinks && mode == rsync.S_IFLNK {
		if rt.Opts.SafeLinks && safelink.Unsafe(f.LinkTarget, f.Name) {
			if rt.Opts.Verbose {
				rt.info(fmt.Sprintf("ignoring unsafe symlink \"%s\" -> \"%s\"\n", f.Name, f.LinkTarget))
			}
			return nil
		}
		if err == nil {
			// local file exists, verify target matches
			if target, err := rt.readlink(f.Name); err == nil {
				if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
					rt.Logger.Printf("existing target: %q", target)
				}
				if target == f.LinkTarget {
					if err := rt.setPerms(f, perm); err != nil {
						return err
					}
					rt.fileDone(idx)
					return rt.itemize(idx, f, st, 0, "")
				}
				// fallthrough to create or replace the symlink
			}
			// fallthrough to create or replace the symlink
		}
		if err != nil && len(rt.basisRoots) > 0 && rt.tryDestsNon(f) {
			rt.fileDone(idx)
			return nil
		}
		var linkSt os.FileInfo
		if err == nil && st.Mode()&fs.ModeSymlink != 0 {
			linkSt = st
		}
		if rt.Opts.DryRun {
			return rt.itemize(idx, f, linkSt, rsync.ITEM_LOCAL_CHANGE|rsync.ITEM_REPORT_CHANGE, "")
		}
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			rt.Logger.Printf("symlink %s -> %s", f.Name, f.LinkTarget)
		}
		create := symlink
		if rt.Opts.FakeSuper {
			create = func(_ *os.Root, target, name string) error { return rt.placeholder(name, target) }
		}
		if err := create(rt.DestRoot, f.LinkTarget, f.Name); err != nil {
			return err
		}
		if err := rt.setPerms(f, perm); err != nil {
			return err
		}
		rt.fileDone(idx)
		return rt.itemize(idx, f, linkSt, rsync.ITEM_LOCAL_CHANGE|rsync.ITEM_REPORT_CHANGE, "")
	}

	if rt.Opts.PreserveDevices && (mode == rsync.S_IFCHR ||
		mode == rsync.S_IFBLK ||
		mode == rsync.S_IFSOCK ||
		mode == rsync.S_IFIFO) {
		if err != nil && len(rt.basisRoots) > 0 && !rt.Opts.FakeSuper && rt.tryDestsNon(f) {
			return nil
		}
		if (mode == rsync.S_IFCHR || mode == rsync.S_IFBLK) && rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			major, minor := splitRdev(f.Rdev)
			rt.Logger.Printf("mknod(%s, 0%o, [%d,%d])", f.Name, f.Mode, major, minor)
		}
		var devSt os.FileInfo
		if err == nil && sameType(f, st) {
			devSt = st
		}
		if rt.Opts.DryRun {
			if devSt != nil {
				return rt.itemize(idx, f, devSt, 0, "")
			}
			return rt.itemize(idx, f, nil, rsync.ITEM_LOCAL_CHANGE|rsync.ITEM_REPORT_CHANGE, "")
		}
		if rt.Opts.FakeSuper {
			if err := rt.fakeDevice(f, st); err != nil {
				return err
			}
			if err := rt.setPerms(f, perm); err != nil {
				return err
			}
		} else if err := rt.createDevice(f, st, perm); err != nil {
			return err
		}
		if devSt != nil {
			return rt.itemize(idx, f, devSt, 0, "")
		}
		return rt.itemize(idx, f, nil, rsync.ITEM_LOCAL_CHANGE|rsync.ITEM_REPORT_CHANGE, "")
	}

	if !f.FileMode().IsRegular() {
		// None of the Preserve* options is enabled, so just skip over
		// non-regular files.
		return nil
	}
	if rt.Opts.MaxSize >= 0 && !rt.genRedoing && f.Length > rt.Opts.MaxSize {
		rt.skipInfo(f.Name + " is over max-size")
		return nil
	}
	if rt.Opts.MinSize >= 0 && !rt.genRedoing && f.Length < rt.Opts.MinSize {
		rt.skipInfo(f.Name + " is under min-size")
		return nil
	}
	// -u: leave a file that is newer at the destination.
	if rt.Opts.UpdateOnly && !rt.genRedoing && err == nil && st.Mode().IsRegular() && f.ModTime.Unix()-st.ModTime().Unix() < int64(rt.Opts.ModifyWindow) {
		rt.skipInfo(f.Name + " is newer")
		return nil
	}

	requestFullFile := func(iflags uint32) error {
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			rt.Logger.Printf("requesting: %s", f.Name)
		}
		if err := rt.requestFile(idx, iflags, rsync.FNAMECMP_FNAME); err != nil {
			return err
		}
		if rt.Opts.DryRun {
			return nil
		}
		var sh rsync.SumHead
		if err := sh.WriteTo(rt.Conn); err != nil {
			return err
		}
		return nil
	}

	if rt.Opts.KeepPartial {
		sent, err := rt.recvGeneratorPartial(idx, f)
		if err != nil {
			return err
		}
		if sent {
			return nil
		}
		// no partial file? fall through.
	}

	if err == nil && !st.Mode().IsRegular() && len(rt.basisRoots) > 0 {
		// Make room first, so that the alternate folders are asked as for
		// a file that is not there (generator.c:recv_generator).
		if ok, err := rt.makeRoom(f, st); !ok {
			return err
		}
		err = fs.ErrNotExist
	}
	if len(rt.basisRoots) > 0 && (err != nil || rt.Opts.AltDest != rsyncopts.CopyDest) {
		var destSt fs.FileInfo
		if err == nil {
			destSt = st
		}
		done, basis, aerr := rt.tryDestsReg(f, destSt, perm)
		if aerr != nil {
			return aerr
		}
		if done {
			rt.fileDone(idx)
			return nil
		}
		if basis >= 0 {
			return rt.requestFromBasis(idx, f, basis)
		}
	}

	if os.IsNotExist(err) {
		return requestFullFile(rsync.ITEM_IS_NEW)
	}
	if err != nil {
		return err
	}

	if !st.Mode().IsRegular() {
		// A non-regular file with this name exists. Delete it so that we can
		// create our file instead.
		if ok, err := rt.makeRoom(f, st); !ok {
			return err
		}
		return requestFullFile(rsync.ITEM_IS_NEW)
	}

	skip, err := rt.skipFile(f, st)
	if err != nil {
		return err
	}
	if skip {
		if rt.Opts.InfoGTE(rsyncopts.INFO_SKIP, 1) {
			rt.Logger.Printf("skipping %s", local)
		}
		if err := rt.setAttrs(f, perm, rt.Opts.AlwaysChecksum); err != nil {
			return err
		}
		rt.fileDone(idx)
		return rt.itemize(idx, f, st, 0, "")
	}

	// rsync/generator.c: --append only adds to files, so it leaves a
	// destination alone when it is not shorter than the source. No --append
	// sender would read the block sums sent below.
	if rt.Opts.AppendMode > 0 && !rt.genRedoing && st.Size() >= f.Length {
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			rt.Logger.Printf("append: skipping %s, not shorter than the source (%d/%d bytes)", f.Name, st.Size(), f.Length)
		}
		return nil
	}

	changed := rt.changedFlags(f, st)

	if rt.Opts.DryRun {
		return rt.requestFile(idx, changed, rsync.FNAMECMP_FNAME)
	}

	if rt.Opts.AppendMode > 0 && !rt.genRedoing && st.Size() > 0 && st.Size() < f.Length {
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			rt.Logger.Printf("append: sending prefix sum head for %s (%d/%d bytes)", f.Name, st.Size(), f.Length)
		}
		if err := rt.requestFile(idx, changed, rsync.FNAMECMP_FNAME); err != nil {
			return err
		}
		// Match rsync/generator.c: in --append mode, send only the sum head;
		// the sender skips block-match verification and infers the prefix size
		// from ChecksumCount * BlockLength + RemainderLength.
		sh := rsynccommon.SumSizesSqroot(rt.Conn.Session.Protocol, st.Size(), rt.strong().Len(), rt.Opts.BlockSize)
		return sh.WriteTo(rt.Conn)
	}

	if rt.Opts.WholeFile {
		return requestFullFile(changed)
	}

	in, err := rt.DestRoot.Open(f.Name)
	if err != nil {
		rt.Logger.Printf("failed to open %s, continuing: %v", local, err)
		return requestFullFile(changed)
	}
	defer in.Close()

	if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
		rt.Logger.Printf("sending sums for: %s", f.Name)
	}
	if err := rt.requestFile(idx, changed, rsync.FNAMECMP_FNAME); err != nil {
		return err
	}

	return rt.generateAndSendSums(in, st.Size())
}

func (rt *Transfer) recvGeneratorPartial(idx int, f *File) (bool, error) {
	// See if there is a partial file we can resume from.
	partialName := rt.partialName(f.Name)
	if partialName == "" {
		// Partials are kept at the destination name, where the regular delta
		// path already resumes from them.
		return false, nil
	}
	partial, err := rt.DestRoot.Open(partialName)
	if os.IsNotExist(err) {
		// No partial file, request the full file.
		return false, nil
	}
	if err != nil {
		rt.Logger.Printf("failed to open partial file %s, continuing: %v", partialName, err)
		return false, nil
	}
	defer partial.Close()
	st, err := partial.Stat()
	if err != nil {
		rt.Logger.Printf("failed to stat partial file %s, continuing: %v", partialName, err)
		return false, nil
	}

	// The checksums come from the file in --partial-dir, which the
	// generator tells the sender from protocol 29.
	if rt.Opts.DryRun {
		return true, rt.requestFile(idx, 0, rsync.FNAMECMP_PARTIAL_DIR)
	}

	// TODO: if deltas are disabled, request the file in full

	if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
		rt.Logger.Printf("sending sums for: %s", f.Name)
	}
	if err := rt.requestFile(idx, 0, rsync.FNAMECMP_PARTIAL_DIR); err != nil {
		return true, err
	}

	return true, rt.generateAndSendSums(partial, st.Size())
}

// requestFile asks the sender for a file's data: its index, and from
// protocol 29 the item flags (ITEM_TRANSFER and why) and, when the checksums
// that follow are not of the destination file, where they came from.
func (rt *Transfer) requestFile(idx int, iflags uint32, basis byte) error {
	a := rsyncwire.ItemAttrs{Iflags: rsync.ITEM_TRANSFER | iflags, BasisType: rsync.FNAMECMP_FNAME}
	if basis != rsync.FNAMECMP_FNAME {
		a.Iflags |= rsync.ITEM_BASIS_TYPE_FOLLOWS
		a.BasisType = basis
	}
	req := rt.addXattrRequest(&a)
	if err := rt.Conn.WriteNdxAndAttrs(int32(idx), a); err != nil {
		return err
	}
	_, err := rt.Conn.Writer.Write(req)
	return err
}

// changedFlags are the report flags of rsync's itemize for a regular file
// the generator found at the destination: what about it differs. A sender
// that itemizes (-i) prints them.
func (rt *Transfer) changedFlags(f *File, st os.FileInfo) uint32 {
	// The xattr flag goes with the request (addXattrRequest).
	iflags := rt.itemFlags(f, st, rsync.ITEM_TRANSFER) &^ (rsync.ITEM_TRANSFER | rsync.ITEM_REPORT_XATTR)
	if rt.Opts.AlwaysChecksum {
		iflags |= rsync.ITEM_REPORT_CHANGE
	}
	return iflags
}

// rsync/generator.c:generate_and_send_sums
func (rt *Transfer) generateAndSendSums(in *os.File, fileLen int64) error {
	sh := rsynccommon.SumSizesSqroot(rt.Conn.Session.Protocol, fileLen, rt.strong().Len(), rt.Opts.BlockSize)
	if err := sh.WriteTo(rt.Conn); err != nil {
		return err
	}
	buf := make([]byte, int(sh.BlockLength))
	remaining := fileLen
	for i := int32(0); i < sh.ChecksumCount; i++ {
		n1 := min(int64(sh.BlockLength), remaining)
		b := buf[:n1]
		if _, err := io.ReadFull(in, b); err != nil {
			return err
		}

		sum1 := rsyncchecksum.Checksum1(b)
		sum2 := rt.strong().Block(b)
		if err := rt.Conn.WriteInt32(int32(sum1)); err != nil {
			return err
		}
		if _, err := rt.Conn.Writer.Write(sum2[:sh.ChecksumLength]); err != nil {
			return err
		}
		remaining -= n1
	}
	return nil
}

// info tells the client msg, as rsync's FINFO, or logs it on a client.
func (rt *Transfer) info(msg string) {
	if rt.TellClient != nil {
		rt.TellClient(rsyncwire.MsgInfo, msg)
		return
	}
	rt.Logger.Printf("%s", strings.TrimSuffix(msg, "\n"))
}

// requestFromBasis asks for f with the block sums of its copy in the
// alternate folder basis, which the receiver then reads too
// (FNAMECMP_BASIS_DIR_LOW + basis).
func (rt *Transfer) requestFromBasis(idx int, f *File, basis int) error {
	in, err := rt.basisRoots[basis].Open(f.Name)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := rt.requestFile(idx, rt.changedFlags(f, st), byte(rsync.FNAMECMP_BASIS_DIR_LOW+basis)); err != nil {
		return err
	}
	if rt.Opts.DryRun {
		return nil
	}
	rt.altBasis.Store(f, basis)
	return rt.generateAndSendSums(in, st.Size())
}

// skipInfo says why a file is left alone, with --info=skip (-v), as
// rsync's generator does.
func (rt *Transfer) skipInfo(msg string) {
	if rt.Opts.InfoGTE != nil && rt.Opts.InfoGTE(rsyncopts.INFO_SKIP, 1) {
		rt.info(msg + "\n")
	}
}

// makeRoom removes what is at f's name for a file to take its place. A
// folder goes only when empty, or with --force or --delete with what it
// holds; one
// that is not empty is reported and f left alone, as rsync's delete_item
// without DEL_RECURSE does. It reports whether there is room.
func (rt *Transfer) makeRoom(f *File, st fs.FileInfo) (bool, error) {
	if rt.Opts.DryRun {
		return true, nil // a dry run only says what it would do
	}
	// With --delete, rsync removes what is in the way with what it holds
	// (del_opts with DEL_RECURSE), --force or not.
	if st.IsDir() && rt.Opts.Force {
		rt.logDeleteContents(filepath.ToSlash(f.Name))
		return true, rt.DestRoot.RemoveAll(f.Name)
	}
	if st.IsDir() && rt.Opts.DeleteMode {
		saved := rt.delStats
		rt.delStats = rsyncwire.DelStats{}
		rt.countDeleted(filepath.ToSlash(f.Name), fs.FileInfoToDirEntry(st))
		n := rt.delStats.Total()
		rt.delStats = saved
		if rt.mayDelete(n) {
			rt.logDeleteContents(filepath.ToSlash(f.Name))
			return true, rt.DestRoot.RemoveAll(f.Name)
		}
	}
	err := rt.DestRoot.Remove(f.Name)
	if err == nil {
		return true, nil
	}
	if st.IsDir() {
		rt.deleteFailed(f.Name, fmt.Errorf("cannot delete non-empty directory: %s", f.Name))
		return false, nil
	}
	return false, fmt.Errorf("unlinking to make room for regular file: %v", err)
}

// fileDone tells the sender the file at idx is in place, for
// --remove-source-files (generator.c and receiver.c:send_msg_success). It
// needs this side's output multiplexed: always for a server, from
// protocol 30 for a client.
func (rt *Transfer) fileDone(idx int) {
	if !rt.Opts.RemoveSourceFiles || rt.Opts.DryRun || rt.Conn.Mux == nil {
		return
	}
	rt.Conn.Mux.WriteMsg(rsyncwire.MsgSuccess, binary.LittleEndian.AppendUint32(nil, uint32(idx)))
}
