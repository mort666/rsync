package receiver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// rsync/receiver.c:recv_files
func (rt *Transfer) RecvFiles(fileList []*File) error {
	maxPhase := 1
	if rt.Conn.Session.Protocol >= 29 {
		maxPhase = 2 // the --delay-updates pass
	}
	phase := 0
	// Closing redo tells the generator the redo pass is complete; it must
	// happen on the way out too, or a generator waiting on it would hang.
	redoClosed := rt.redo == nil
	closeRedo := func() {
		if !redoClosed {
			close(rt.redo)
			redoClosed = true
		}
	}
	defer closeRedo()
	for {
		idx, attrs, err := rt.Conn.ReadNdxAndAttrs(len(fileList))
		if err != nil {
			return err
		}
		if idx == rsyncwire.NdxDone {
			phase++
			if phase > maxPhase {
				break
			}
			if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
				rt.Logger.Printf("recvFiles phase=%d", phase)
			}
			if phase == 1 {
				// What comes now is the redo pass (receiver.c:recv_files
				// redoing); the generator has every file to ask for again.
				rt.recvRedoing = true
				closeRedo()
			}
			continue
		}
		fetched, err := rt.readXattrValues(fileList[idx], attrs)
		if err != nil {
			return err
		}
		if !attrs.Transfer() {
			// An item the generator reported on (protocol 29+), which the
			// sender echoes: no data follows, but xattrs may have changed.
			if attrs.Iflags&rsync.ITEM_REPORT_XATTR != 0 {
				rt.setXattrsInPlace(fileList[idx], fetched)
			}
			continue
		}
		rt.xattrFetched = fetched
		if fileList[idx].Mode&rsync.S_IFMT != rsync.S_IFREG {
			return fmt.Errorf("received request to transfer non-regular file: %d", idx)
		}

		if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
			rt.Logger.Printf("receiving file idx=%d: %+v", idx, fileList[idx])
		}
		if rt.Opts.Progress {
			fmt.Fprintln(rt.Env.Stdout, fileList[idx].Name)
		}
		if err := rt.recvFile1(fileList[idx]); err != nil {
			var verr *verifyError
			if !errors.As(err, &verr) {
				return err
			}
			rt.verifyFailed(int(idx), verr)
			continue
		}
		rt.fileDone(int(idx))
	}
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
		rt.Logger.Printf("recvFiles finished")
	}
	return nil
}

// partialName reports where a previously kept partial file for name lives, or
// "" when partials are not kept in a separate directory (in which case the
// partial was renamed to the destination name and openLocalFile finds it).
// The layout must match pendingFile.Cleanup in output.go.
func (rt *Transfer) partialName(name string) string {
	if rt.Opts.Inplace || !rt.Opts.KeepPartial || rt.Opts.PartialDir == "" {
		return ""
	}
	return filepath.Join(rt.Opts.PartialDir, filepath.Base(name))
}

func (rt *Transfer) recvFile1(f *File) error {
	if rt.Opts.DryRun {
		if !rt.Opts.Server {
			fmt.Fprintln(rt.Env.Stdout, f.Name)
		}
		return nil
	}

	st, err := rt.lstat(f.Name)
	if err != nil || !st.Mode().IsRegular() {
		st = nil
	}
	perm := rt.destPerm(f, st)

	if partialName := rt.partialName(f.Name); partialName != "" {
		// Resume from the kept partial file, if any, regardless of
		// whether the localFile exists.
		partial, err := rt.DestRoot.Open(partialName)
		if err != nil && !os.IsNotExist(err) {
			rt.Logger.Printf("opening partial file failed, continuing: %v", err)
			// fallthrough to local file
		}
		if err == nil {
			// partial file exists; use it.
			defer partial.Close()
			if err := rt.receiveData(f, partial, perm); err != nil {
				return err
			}
			// receiveData called partial.Close()
			if err := rt.DestRoot.Remove(partialName); err != nil {
				return err
			}
			return nil
		}
		// fallthrough to local file
	}

	open := rt.openLocalFile
	if basis, ok := rt.altBasis.Load(f); ok {
		open = func(f *File) (*os.File, error) { return rt.basisRoots[basis.(int)].Open(f.Name) }
	}
	localFile, err := open(f)
	if err != nil && !os.IsNotExist(err) {
		rt.Logger.Printf("opening local file failed, continuing: %v", err)
	}
	defer localFile.Close()
	if err := rt.receiveData(f, localFile, perm); err != nil {
		return err
	}
	return nil
}

func (rt *Transfer) openLocalFile(f *File) (*os.File, error) {
	in, err := rt.DestRoot.Open(f.Name)
	if err != nil {
		return nil, err
	}

	st, err := in.Stat()
	if err != nil {
		return nil, err
	}

	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", filepath.Join(rt.Dest, f.Name))
	}

	if !st.Mode().IsRegular() {
		return nil, nil
	}

	return in, nil
}

// rsync/receiver.c:receive_data
func (rt *Transfer) receiveData(f *File, localFile *os.File, perm fs.FileMode) error {
	rt.Progress.Reset(uint64(f.Length))
	var sh rsync.SumHead
	if err := sh.ReadFromMax(rt.Conn, rt.strong().BlockLen()); err != nil {
		return err
	}

	if rt.Opts.DebugGTE(rsyncopts.DEBUG_DELTASUM, 1) {
		local := filepath.Join(rt.Dest, f.Name)
		rt.Logger.Printf("creating %s", local)
	}
	out, err := newPendingFile(rt.DestRoot, f.Name, outputMode{
		Inplace:     rt.Opts.Inplace,
		KeepPartial: rt.Opts.KeepPartial,
		PartialDir:  rt.Opts.PartialDir,
		Fsync:       rt.Opts.DoFsync,
		Sparse:      rt.Opts.SparseFiles,
	})
	if err != nil {
		return err
	}
	defer out.Cleanup()
	defer func() { rt.touchedBlocks += out.blocks.touched }()

	h := rt.strong().NewTransferHash()

	wr := io.MultiWriter(out, h)

	var buf [rsync.ChunkSize]byte
	var offset int64
	// The redo pass sends files whole or against their basis, never
	// appended (receiver.c:recv_files turns append_mode off).
	appended := rt.Opts.AppendMode > 0 && !rt.recvRedoing && sh.ChecksumCount > 0
	if appended {
		// Match rsync/receiver.c:receive_data — seek past the existing prefix
		// and append the incoming literals after it. At this protocol level
		// append always verifies: the existing prefix is folded into the sum
		// so the sender's whole-file MD4 checks it, unless the caller asked
		// for an unverified append.
		prefix := int64(sh.ChecksumCount) * int64(sh.BlockLength)
		if sh.RemainderLength != 0 {
			prefix -= int64(sh.BlockLength) - int64(sh.RemainderLength)
		}
		if !rt.Opts.UnverifiedAppend {
			if localFile == nil {
				return fmt.Errorf("append-verify: %s has no existing data to verify", f.Name)
			}
			if _, err := io.Copy(h, io.NewSectionReader(localFile, 0, prefix)); err != nil {
				return fmt.Errorf("append-verify: reading prefix of %s: %w", f.Name, err)
			}
		}
		if err := out.SeekToAppendOffset(prefix); err != nil {
			return fmt.Errorf("append: seek to %d: %w", prefix, err)
		}
		offset = prefix
	}
	dec, err := rt.decoder()
	if err != nil {
		return err
	}
	for {
		lit, token, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if rt.Opts.Progress && !rt.Opts.Server {
			rt.Progress.MaybeShow(uint64(offset), false)
			if offset == 0 {
				defer func() {
					rt.Progress.MaybeShow(uint64(offset), true)
				}()
			}
		}
		if lit != nil {
			if _, err := wr.Write(lit); err != nil {
				return err
			}
			offset += int64(len(lit))
			continue
		}
		if localFile == nil {
			return fmt.Errorf("BUG: local file %s not open for copying chunk", out.Name())
		}
		if token >= sh.ChecksumCount {
			return fmt.Errorf("invalid block index %d (%d blocks) for %s", token, sh.ChecksumCount, out.Name())
		}
		offset2 := int64(token) * int64(sh.BlockLength)
		dataLen := sh.BlockLength
		if token == sh.ChecksumCount-1 && sh.RemainderLength != 0 {
			dataLen = sh.RemainderLength
		}
		blockWr := wr
		// In place, a block matched at its own offset is already there:
		// only the checksum needs it. (The basis is the destination itself
		// here, as --inplace turns off the partial dir.)
		skip := rt.Opts.Inplace && offset2 == offset
		if skip {
			blockWr = h
		}
		if see := dec.See(int64(dataLen)); see != nil {
			blockWr = io.MultiWriter(blockWr, see)
		}
		dataReader := io.NewSectionReader(localFile, offset2, int64(dataLen))
		n, err := io.CopyBuffer(blockWr, dataReader, buf[:])
		if err != nil {
			return err
		}
		if n != int64(dataLen) {
			// The basis file shrank since the generator summed it.
			return fmt.Errorf("reading block %d of %s: %w", token, out.Name(), io.ErrUnexpectedEOF)
		}
		if skip {
			if err := out.Skip(n); err != nil {
				return err
			}
		}
		offset += n
	}
	localSum := h.Sum(nil)
	remoteSum := make([]byte, len(localSum))
	if _, err := io.ReadFull(rt.Conn.Reader, remoteSum); err != nil {
		return err
	}
	switch {
	case bytes.Equal(localSum, remoteSum):
	case appended && rt.Opts.UnverifiedAppend:
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_DELTASUM, 1) {
			rt.Logger.Printf("append of %s accepted unverified", f.Name)
		}
	default:
		fate := "discarded"
		switch {
		case !rt.Opts.Inplace && rt.Opts.KeepPartial && rt.Opts.PartialDir != "":
			fate = "put into partial-dir"
		case rt.Opts.Inplace || rt.Opts.KeepPartial:
			fate = "retained"
		}
		return &verifyError{name: f.Name, fate: fate}
	}
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_DELTASUM, 1) {
		rt.Logger.Printf("checksum %x matches!", localSum)
	}

	// The xattrs go on before the file takes the destination's place;
	// unchanged abbreviated ones come from the file it replaces.
	rt.setXattrs(out.f, f, rt.xattrFetched, localFile)

	if localFile != nil {
		// Close the file earlier than the calling function’s deferred Close(),
		// so that we can rename files on Windows, which fails as long
		// as there are any open file handles.
		localFile.Close()
	}

	if err := out.CloseAtomicallyReplace(); err != nil {
		return err
	}

	if err := rt.setPermsAccurate(f, perm); err != nil {
		return err
	}

	return nil
}

// verifyError is a file whose whole-file checksum did not match the
// sender's, after which rsync tries it once more (receiver.c:recv_files,
// recv_ok == 0).
type verifyError struct {
	name string
	fate string // what became of the received data
}

func (e *verifyError) Error() string {
	return fmt.Sprintf("%s failed verification -- update %s", e.name, e.fate)
}

// verifyFailed hands a file that failed verification to the generator for
// the redo pass, with a warning at -v, as rsync does. In the redo pass it
// is an error, which fails the run (exit 23) but not the transfer.
func (rt *Transfer) verifyFailed(idx int, e *verifyError) {
	if !rt.recvRedoing && rt.redo != nil {
		if rt.Opts.InfoGTE != nil && rt.Opts.InfoGTE(rsyncopts.INFO_NAME, 1) {
			rt.tell(rsyncwire.MsgWarning, fmt.Sprintf("WARNING: %s failed verification -- update %s (will try again).\n", e.name, e.fate))
		}
		rt.redo <- idx
		return
	}
	rt.tell(rsyncwire.MsgErrorXfer, fmt.Sprintf("ERROR: %s failed verification -- update %s.\n", e.name, e.fate))
	if rt.TellClient == nil && rt.Env != nil {
		rt.Env.XferErrors.Add(1)
	}
}

// tell sends the client msg with tag, or logs it on a client. On a server
// it only queues msg: the receiver writing while a sending client writes
// would block both over an unbuffered connection, so the generator sends
// it, or Do once both are done, as rsync's receiver passes its messages
// to the generator.
func (rt *Transfer) tell(tag uint8, msg string) {
	if rt.TellClient == nil {
		rt.Logger.Printf("%s", strings.TrimSuffix(msg, "\n"))
		return
	}
	rt.recvMsgsMu.Lock()
	defer rt.recvMsgsMu.Unlock()
	rt.recvMsgs = append(rt.recvMsgs, recvMsg{tag, msg})
}

// flushRecvMsgs sends the client the receiver's queued messages.
func (rt *Transfer) flushRecvMsgs() {
	rt.recvMsgsMu.Lock()
	msgs := rt.recvMsgs
	rt.recvMsgs = nil
	rt.recvMsgsMu.Unlock()
	for _, m := range msgs {
		rt.TellClient(m.tag, m.msg)
	}
}

type recvMsg struct {
	tag uint8
	msg string
}
