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
	if err := st.Conn.WriteInt64(crd.BytesRead); err != nil {
		return err
	}
	// total bytes written (to network connection)
	if err := st.Conn.WriteInt64(cwr.BytesWritten); err != nil {
		return err
	}
	// total size of files
	if err := st.Conn.WriteInt64(fileList.TotalSize); err != nil {
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

	if err := st.SendFiles(fileList); err != nil {
		return nil, err
	}

	if err := st.handleStats(crd, cwr, fileList); err != nil {
		return nil, err
	}

	if st.Opts.DebugGTE(rsyncopts.DEBUG_PROTO, 1) {
		st.Logger.Printf("reading final int32")
	}

	finish, err := st.Conn.ReadInt32()
	if err != nil {
		return nil, err
	}
	if finish != -1 {
		return nil, fmt.Errorf("protocol error: expected final -1, got %d", finish)
	}

	return &rsyncstats.TransferStats{
		Read:    crd.BytesRead,
		Written: cwr.BytesWritten,
		Size:    fileList.TotalSize,
	}, nil
}
