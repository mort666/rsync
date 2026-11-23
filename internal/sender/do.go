package sender

import (
	"fmt"
	"sort"
	"time"

	"github.com/gokrazy/rsync/internal/flistcmp"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncstats"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// rsync/main.c:handle_stats
func (st *Transfer) handleStats(crd *rsyncwire.CountingReader, cwr *rsyncwire.CountingWriter, fileList *fileList) error {
	if !st.Opts.Server() || !st.Opts.Sender() {
		return nil
	}

	// send statistics:
	// total bytes read (from network connection)
	if err := st.Conn.WriteVarlong30(crd.BytesRead, 3); err != nil {
		return err
	}
	// total bytes written (to network connection)
	if err := st.Conn.WriteVarlong30(cwr.BytesWritten, 3); err != nil {
		return err
	}
	// total size of files
	if err := st.Conn.WriteVarlong30(fileList.TotalSize, 3); err != nil {
		return err
	}
	// From protocol 29: how long building and sending the file list took,
	// in milliseconds. gokrazy/rsync sends the list as it builds it.
	if st.Conn.Session.Protocol >= 29 {
		if err := st.Conn.WriteVarlong30(fileList.BuildTime.Milliseconds(), 3); err != nil {
			return err
		}
		if err := st.Conn.WriteVarlong30(0, 3); err != nil {
			return err
		}
	}
	return nil
}

// rsync/main.c:client_run am_sender
func (st *Transfer) Do(crd *rsyncwire.CountingReader, cwr *rsyncwire.CountingWriter, modPath string, paths []string, exclusionList *filterRuleList) (*rsyncstats.TransferStats, error) {
	if exclusionList == nil {
		exclusionList = &filterRuleList{}
	}
	if st.Out != nil && st.Out.ChecksumLen == 0 {
		// %C pads to the length of the sum it would show.
		st.Out.ChecksumLen = st.strong().Len()
		if st.Opts.AlwaysChecksum() {
			st.Out.ChecksumLen = st.strong().FileLen()
		}
	}

	// “Update exchange” as per
	// https://github.com/kristapsdz/openrsync/blob/master/rsync.5

	// send file list
	st.Logger.Printf("SendFileList(modPath=%q, paths=%q)", modPath, paths)
	start := time.Now()
	fileList, err := st.SendFileList(modPath, paths, exclusionList)
	if err != nil {
		return nil, err
	}
	fileList.BuildTime = time.Since(start)
	defer fileList.Close()

	if st.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 3) {
		st.Logger.Printf("file list sent")
	}

	// Sort the file list. The client sorts, so we need to sort, too (in the
	// same way!), otherwise our indices do not match what the client will
	// request.
	proto := st.Conn.Session.Protocol
	sort.SliceStable(fileList.Files, func(i, j int) bool {
		a, b := fileList.Files[i], fileList.Files[j]
		return flistcmp.Compare(proto, flistcmp.Entry{Name: a.Wpath, IsDir: a.isDir}, flistcmp.Entry{Name: b.Wpath, IsDir: b.isDir}) < 0
	})

	if st.Opts.RemoveSourceFiles() && st.Env != nil {
		st.Env.SourceDone = func(ndx int32) { st.removeSource(fileList, ndx) }
		defer func() { st.Env.SourceDone = nil }()
	}
	if err := st.SendFiles(fileList); err != nil {
		return nil, err
	}

	if err := st.handleStats(crd, cwr, fileList); err != nil {
		return nil, err
	}

	if st.Opts.DebugGTE(rsyncopts.DEBUG_PROTO, 1) {
		st.Logger.Printf("reading final int32")
	}

	// The final goodbye: NDX_DONE (main.c:read_final_goodbye). From
	// protocol 31 it is the generator's fourth NDX_DONE, which the sender
	// answers before the last; delete stats may come first.
	finish, err := st.readGoodbye(len(fileList.Files))
	if err != nil {
		return nil, err
	}
	if finish == rsyncwire.NdxDone && st.Conn.Session.Protocol >= 31 {
		// A server sender passes delete stats on to its client
		// (rsync.c:read_ndx_and_attrs) once it has read the NDX_DONE
		// after them: sending while the client is still writing would
		// block both over an unbuffered connection.
		if st.Conn.ForwardDelStats && st.Conn.GotDelStats {
			if err := st.Conn.WriteDelStats(st.Conn.DelStats); err != nil {
				return nil, err
			}
		}
		if err := st.Conn.WriteNdx(rsyncwire.NdxDone); err != nil {
			return nil, err
		}
		if finish, err = st.readGoodbye(len(fileList.Files)); err != nil {
			return nil, err
		}
	}
	if finish != rsyncwire.NdxDone {
		return nil, fmt.Errorf("protocol error: expected final -1, got %d", finish)
	}

	stats := &rsyncstats.TransferStats{
		Read:    crd.BytesRead,
		Written: cwr.BytesWritten,
		Size:    fileList.TotalSize,
	}
	if st.Conn.Session.Protocol >= 33 && st.Env != nil {
		// The server receiver's count (MSG_BLOCK_STATS), which has come
		// in by the end of the goodbye.
		stats.TouchedBlocks = st.Env.BlockStats.Load()
	}
	return stats, nil
}

// readGoodbye reads one index of the final goodbye.
func (st *Transfer) readGoodbye(numFiles int) (int32, error) {
	if st.Conn.Session.Protocol < 29 {
		return st.Conn.ReadNdx()
	}
	ndx, _, err := st.Conn.ReadNdxAndAttrs(numFiles)
	return ndx, err
}
