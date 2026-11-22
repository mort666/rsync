package sender

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// removeSource removes the source of the file the receiver reports done,
// unless it changed since it was sent (--remove-source-files,
// sender.c:successful_send).
func (st *Transfer) removeSource(fl *fileList, ndx int32) {
	if ndx < 0 || int(ndx) >= len(fl.Files) {
		return
	}
	f := fl.Files[ndx]
	if f.isDir {
		return
	}
	remover, ok := f.source.(interface{ Remove(name string) error })
	if !ok {
		st.tellClientOrLog(rsyncwire.MsgErrorXfer, fmt.Sprintf("rsync: sender failed to remove %s: the source cannot remove files\n", f.Wpath))
		return
	}
	stat := fs.Lstat
	if st.Opts.CopyLinks() {
		stat = fs.Stat
	}
	info, err := stat(f.source.FS(), f.path)
	if err == nil && (info.Size() != f.Length || !info.ModTime().Equal(f.ModTime)) {
		st.tellClientOrLog(rsyncwire.MsgErrorXfer, fmt.Sprintf("ERROR: Skipping sender remove for changed file: %s\n", f.Wpath))
		return
	}
	if err == nil {
		err = remover.Remove(f.path)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		st.tellClientOrLog(rsyncwire.MsgInfo, fmt.Sprintf("sender file already removed: %s\n", f.Wpath))
	case err != nil:
		st.tellClientOrLog(rsyncwire.MsgErrorXfer, fmt.Sprintf("rsync: sender failed to remove %s: %v\n", f.Wpath, errors.Unwrap(err)))
	case st.Opts.InfoGTE(rsyncopts.INFO_REMOVE, 1):
		st.tellClientOrLog(rsyncwire.MsgInfo, fmt.Sprintf("sender removed %s\n", f.Wpath))
	}
}

// tellClientOrLog tells the client, as a server, or logs it, as one.
func (st *Transfer) tellClientOrLog(tag uint8, msg string) {
	if st.TellClient != nil {
		st.TellClient(tag, msg)
		return
	}
	st.Logger.Printf("%s", msg)
	if tag == rsyncwire.MsgErrorXfer && st.Env != nil {
		st.Env.XferErrors.Add(1)
	}
}

func (s *osRootSource) Remove(name string) error { return s.root.Remove(name) }

func (s *subSource) Remove(name string) error {
	r, ok := s.underlying.(interface{ Remove(string) error })
	if !ok {
		return fmt.Errorf("remove %s: not supported", name)
	}
	return r.Remove(path.Join(s.subdir, name))
}
