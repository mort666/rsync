package receiver

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsynccommon"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// rsync/generator.c:generate_files()
func (rt *Transfer) GenerateFiles(fileList []*File) error {
	phase := 0
	for idx, f := range fileList {
		if err := rt.recvGenerator(idx, f); err != nil {
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

	// TODO: re-do any files that failed
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
	if st.Size() != f.Length {
		return false, nil
	}

	if rt.Opts.AlwaysChecksum {
		checksum, err := rt.strong().RootFileChecksum(rt.DestRoot, f.Name)
		if err != nil {
			return false, err
		}
		return bytes.Equal(f.Checksum, checksum), nil
	}

	// TODO: size only

	if rt.Opts.IgnoreTimes {
		return false, nil
	}

	return modTimeEqual(st.ModTime(), f.ModTime), nil
}

// modTimeEqual is the quick check's comparison: whole seconds, as rsync
// compares with --modify-window=0 (util1.c:same_time). Setting times
// compares exactly, nanoseconds included (rsync.c:same_mtime).
func modTimeEqual(a, b time.Time) bool {
	a = a.Truncate(time.Second)
	b = b.Truncate(time.Second)
	return a.Equal(b)
}

// rsync/rsync.c:set_perms
func (rt *Transfer) setPerms(f *File, perm fs.FileMode) error {
	if rt.Opts.DryRun {
		return nil
	}

	st, err := rt.DestRoot.Lstat(f.Name)
	if err != nil {
		return err
	}

	isLink := f.Mode&rsync.S_IFMT == rsync.S_IFLNK
	isDir := f.Mode&rsync.S_IFMT == rsync.S_IFDIR
	if rt.Opts.PreserveTimes &&
		!isLink &&
		!(isDir && rt.Opts.OmitDirTimes) &&
		!st.ModTime().Equal(f.ModTime) {
		if err := rt.DestRoot.Chtimes(f.Name, f.ModTime, f.ModTime); err != nil {
			return err
		}
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
	st, err := rt.DestRoot.Lstat(f.Name)

	mode := f.Mode & rsync.S_IFMT
	if mode == rsync.S_IFDIR {
		if rt.Opts.DryRun {
			return nil
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
		return nil
	}

	var destSt os.FileInfo
	if err == nil && !st.IsDir() {
		destSt = st
	}
	perm := rt.destPerm(f, destSt)

	if rt.Opts.PreserveLinks && mode == rsync.S_IFLNK {
		// TODO: safe_symlinks option
		if err == nil {
			// local file exists, verify target matches
			if target, err := rt.DestRoot.Readlink(f.Name); err == nil {
				if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
					rt.Logger.Printf("existing target: %q", target)
				}
				if target == f.LinkTarget {
					if err := rt.setPerms(f, perm); err != nil {
						return err
					}
					return nil // skip
				}
				// fallthrough to create or replace the symlink
			}
			// fallthrough to create or replace the symlink
		}
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			rt.Logger.Printf("symlink %s -> %s", f.Name, f.LinkTarget)
		}
		if err := symlink(rt.DestRoot, f.LinkTarget, f.Name); err != nil {
			return err
		}
		if err := rt.setPerms(f, perm); err != nil {
			return err
		}
		return nil
	}

	if rt.Opts.PreserveDevices && (mode == rsync.S_IFCHR ||
		mode == rsync.S_IFBLK ||
		mode == rsync.S_IFSOCK ||
		mode == rsync.S_IFIFO) {
		if (mode == rsync.S_IFCHR || mode == rsync.S_IFBLK) && rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			major, minor := splitRdev(f.Rdev)
			rt.Logger.Printf("mknod(%s, 0%o, [%d,%d])", f.Name, f.Mode, major, minor)
		}
		if err := rt.createDevice(f, st, perm); err != nil {
			return err
		}
		return nil
	}

	if rt.Opts.PreserveHardlinks {
		// TODO: hard link check
	}

	if !f.FileMode().IsRegular() {
		// None of the Preserve* options is enabled, so just skip over
		// non-regular files.
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

	if os.IsNotExist(err) {
		return requestFullFile(rsync.ITEM_IS_NEW)
	}
	if err != nil {
		return err
	}

	if !st.Mode().IsRegular() {
		// A non-regular file with this name exists. Delete it so that we can
		// create our file instead.
		if err := rt.DestRoot.Remove(f.Name); err != nil {
			return fmt.Errorf("unlinking to make room for regular file: %v", err)
		}
		return requestFullFile(rsync.ITEM_IS_NEW)
	}

	// TODO: update-only check

	skip, err := rt.skipFile(f, st)
	if err != nil {
		return err
	}
	if skip {
		if rt.Opts.InfoGTE(rsyncopts.INFO_SKIP, 1) {
			rt.Logger.Printf("skipping %s", local)
		}
		if err := rt.setPerms(f, perm); err != nil {
			return err
		}
		return nil
	}

	// rsync/generator.c: --append only adds to files, so it leaves a
	// destination alone when it is not shorter than the source. No --append
	// sender would read the block sums sent below.
	if rt.Opts.AppendMode > 0 && st.Size() >= f.Length {
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			rt.Logger.Printf("append: skipping %s, not shorter than the source (%d/%d bytes)", f.Name, st.Size(), f.Length)
		}
		return nil
	}

	changed := rt.changedFlags(f, st)

	if rt.Opts.DryRun {
		return rt.requestFile(idx, changed, rsync.FNAMECMP_FNAME)
	}

	if rt.Opts.AppendMode > 0 && st.Size() > 0 && st.Size() < f.Length {
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
	return rt.Conn.WriteNdxAndAttrs(int32(idx), a)
}

// changedFlags are the report flags of rsync's itemize for a regular file
// the generator found at the destination: what about it differs. A sender
// that itemizes (-i) prints them.
func (rt *Transfer) changedFlags(f *File, st os.FileInfo) uint32 {
	var iflags uint32
	if st.Size() != f.Length {
		iflags |= rsync.ITEM_REPORT_SIZE
	}
	if rt.Opts.PreserveTimes && !modTimeEqual(st.ModTime(), f.ModTime) {
		iflags |= rsync.ITEM_REPORT_TIME
	}
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
