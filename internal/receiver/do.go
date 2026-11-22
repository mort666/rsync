package receiver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/gokrazy/rsync/internal/panicerr"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncstats"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"golang.org/x/sync/errgroup"
)

func isTopDir(f *File) bool {
	// TODO: once we check the f.Flags:
	// if !f.FileMode().IsDir() {
	//    // non-directories can get the top_dir flag set,
	//    // but it must be ignored (only for protocol reasons).
	//   return false
	// }
	// return (f.Flags & TOP_DIR) != 0
	return f.Name == "."
}

func (rt *Transfer) deleteFiles(fileList []*File) error {
	if rt.IOErrors > 0 {
		rt.Logger.Printf("IO error encountered, skipping file deletion")
		return nil
	}

	// fs.WalkDir names paths with forward slashes; f.Name is a local path,
	// with backslashes on Windows, where comparing the two as they are
	// would delete every file below the top folder.
	inList := make(map[string]bool, len(fileList))
	for _, f := range fileList {
		inList[filepath.ToSlash(f.Name)] = true
	}
	for _, f := range fileList {
		if !isTopDir(f) {
			continue
		}
		rt.Logger.Printf("deleting in %s", f.Name)
		// Other rsync implementations generate a local file list and compare it
		// with the remote file list, we re-implement the path→name mapping part
		// of file list generation here. We could change it for consistency.
		err := fs.WalkDir(rt.DestRoot.FS(), ".", func(path string, info fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rt.Logger.Printf("WalkDir(%q)", path)
			if inList[path] {
				return nil
			}
			if rt.Excluded != nil && rt.Excluded(path) {
				if rt.Opts.Verbose {
					rt.Logger.Printf("  protecting %s (filter rule)", path)
				}
				return nil
			}
			if rt.Opts.Verbose {
				rt.Logger.Printf("  deleting %s", path)
			}
			if rt.Opts.DryRun {
				rt.countDeleted(path, info)
				if info.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			counted := rt.delStats
			rt.countDeleted(path, info)
			if err := rt.DestRoot.RemoveAll(path); err != nil {
				rt.delStats = counted
				rt.Logger.Printf("  deleting %s failed: %v", path, err)
				// Keep going, as rsync does; the error fails the run
				// (rsync/delete.c delete_item, FERROR_XFER).
				rt.deleteFailed(path, err)
			}
			if info.IsDir() {
				return fs.SkipDir // skip the just-deleted directory
			}
			return nil
		})
		if err != nil {
			if os.IsNotExist(err) {
				return nil // destination does not exist, nothing to do
			}
			return err
		}
	}
	return nil
}

// countDeleted adds what deleting path removes to the delete stats, which
// protocol 31 sends (NDX_DEL_STATS): each entry by type, a folder's
// contents included, as rsync's delete_item counts them.
func (rt *Transfer) countDeleted(path string, d fs.DirEntry) {
	count := func(t fs.FileMode) {
		switch {
		case t.IsDir():
			rt.delStats.Dirs++
		case t&fs.ModeSymlink != 0:
			rt.delStats.Symlinks++
		case t&fs.ModeDevice != 0:
			rt.delStats.Devices++
		case t&(fs.ModeNamedPipe|fs.ModeSocket) != 0:
			rt.delStats.Specials++
		default:
			rt.delStats.Files++
		}
	}
	if !d.IsDir() {
		count(d.Type())
		return
	}
	fs.WalkDir(rt.DestRoot.FS(), path, func(_ string, e fs.DirEntry, err error) error {
		if err == nil {
			count(e.Type())
		}
		return nil
	})
}

// deleteFailed reports a path that --delete could not remove: a server
// tells the client, a client counts it.
func (rt *Transfer) deleteFailed(path string, err error) {
	if rt.TellClient != nil {
		rt.TellClient(rsyncwire.MsgErrorXfer, fmt.Sprintf("rsync: delete_file: remove \"%s\" failed: %v\n", path, errors.Unwrap(err)))
		return
	}
	if rt.Env != nil {
		rt.Env.XferErrors.Add(1)
	}
}

// rsync/main.c:do_recv
func (rt *Transfer) Do(c *rsyncwire.Conn, fileList []*File, noReport bool) (*rsyncstats.TransferStats, error) {
	rt.defaultPerms = defaultPerms()

	if rt.Opts.DeleteMode {
		if err := rt.deleteFiles(fileList); err != nil {
			return nil, err
		}
	}

	var eg errgroup.Group
	// Close the connection as soon as either goroutine fails, so the other one
	// stops blocking on a read or write that will never complete.
	var closeOnce sync.Once
	closeOnErr := func(err error) error {
		if err != nil {
			closeOnce.Do(func() { c.Close() })
		}
		return err
	}
	// Wait passes on no panic of these goroutines: a panic would end the
	// program. Each one returns its own as an error, which closes the
	// connection too.
	eg.Go(func() error { return closeOnErr(panicerr.Call(func() error { return rt.GenerateFiles(fileList) })) })
	eg.Go(func() error { return closeOnErr(panicerr.Call(func() error { return rt.RecvFiles(fileList) })) })
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	if len(rt.retouchDirs) > 0 /* || rt.retouchDirTimes */ {
		if err := rt.touchUpDirs(); err != nil {
			return nil, err
		}
	}

	var stats *rsyncstats.TransferStats
	if !noReport {
		var err error
		stats, err = rt.report(c)
		if err != nil {
			return nil, err
		}
	}

	// From protocol 31 a fourth phase is for deleting after the transfer
	// (--delete-after, --delete-delay). Deleting happens before the
	// transfer here, so it only carries what --delete removed. rsync's
	// generator sends it right after the third phase
	// (EARLY_DELETE_DONE_MSG); here it waits for the sender's stats, which
	// the sender writes first: over an unbuffered connection (io.Pipe)
	// both would block otherwise. The sender answers it with an NDX_DONE
	// of its own (main.c:read_final_goodbye), after any delete stats a
	// server sender passes on.
	if c.Session.Protocol >= 31 {
		if rt.Opts.DeleteMode {
			if err := c.WriteDelStats(rt.delStats); err != nil {
				return nil, err
			}
		}
		if err := c.WriteNdx(rsyncwire.NdxDone); err != nil {
			return nil, err
		}
		ndx, _, err := c.ReadNdxAndAttrs(len(fileList))
		if err != nil {
			return nil, err
		}
		if ndx != rsyncwire.NdxDone {
			return nil, fmt.Errorf("protocol error: expected the sender's goodbye (%d), got %d", rsyncwire.NdxDone, ndx)
		}
		if stats != nil {
			stats.Deleted = c.DelStats.Total()
		}
	}

	// send final goodbye message
	if err := c.WriteNdx(rsyncwire.NdxDone); err != nil {
		return nil, err
	}

	return stats, nil
}

// rsync/main.c:report
func (rt *Transfer) report(c *rsyncwire.Conn) (*rsyncstats.TransferStats, error) {
	// read statistics:
	// total bytes read (from network connection)
	read, err := c.ReadVarlong30(3)
	if err != nil {
		return nil, err
	}
	// total bytes written (to network connection)
	written, err := c.ReadVarlong30(3)
	if err != nil {
		return nil, err
	}
	// total size of files
	size, err := c.ReadVarlong30(3)
	if err != nil {
		return nil, err
	}
	// From protocol 29: how long the sender took to build and to send the
	// file list, in milliseconds.
	var flistBuild, flistXfer int64
	if c.Session.Protocol >= 29 {
		if flistBuild, err = c.ReadVarlong30(3); err != nil {
			return nil, err
		}
		if flistXfer, err = c.ReadVarlong30(3); err != nil {
			return nil, err
		}
	}
	if rt.Opts.InfoGTE(rsyncopts.INFO_STATS, 1) {
		rt.Logger.Printf("server sent stats: read=%d, written=%d, size=%d, flist build=%dms, xfer=%dms", read, written, size, flistBuild, flistXfer)
	}

	return &rsyncstats.TransferStats{
		Read:    read,
		Written: written,
		Size:    size,
	}, nil
}
