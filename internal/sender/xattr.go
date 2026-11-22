package sender

import (
	"fmt"
	"os"

	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/rsyncxattr"
)

// xattrs reports whether the session carries extended attributes (-X).
func (st *Transfer) xattrs() bool { return st.Opts.PreserveXattrs() > 0 }

func (st *Transfer) xattrFilter() rsyncxattr.Filter {
	return rsyncxattr.Filter{Level: st.Opts.PreserveXattrs(), Root: os.Geteuid() == 0}
}

// readXattrs is the list a file-list entry carries (get_xattr): for regular
// files and folders, which every platform supports. Symlinks, devices and
// specials go with none: opening a FIFO would block, and Linux has no user
// xattrs on symlinks.
func (st *Transfer) readXattrs(source FileSource, path string, regularOrDir bool) (rsyncxattr.List, error) {
	if !regularOrDir {
		return nil, nil
	}
	f, err := source.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	osf, ok := f.(*os.File)
	if !ok {
		return nil, nil // a source without xattrs
	}
	filter := st.xattrFilter()
	values, err := rsyncxattr.Read(osf, func(name string) bool { return filter.KeepLocal(name, true) })
	if err != nil {
		return nil, err
	}
	return rsyncxattr.FromValues(values), nil
}

// readXattrRequest reads the generator's request for a file's abbreviated
// xattr values, if one follows its attributes (sender.c:send_files,
// recv_xattr_request), and keeps it for writeNdxAndAttrs.
func (st *Transfer) readXattrRequest(fl file, attrs rsyncwire.ItemAttrs) error {
	if !st.xattrRequestFollows(attrs) {
		return nil
	}
	if err := st.xattrState.ReadRequest(st.Conn.Reader, st.xattrCache.List(fl.xattrs), fl.xattrs); err != nil {
		return fmt.Errorf("%s: %v", fl.Wpath, err)
	}
	return nil
}

func (st *Transfer) xattrRequestFollows(attrs rsyncwire.ItemAttrs) bool {
	s := st.Conn.Session
	return st.xattrs() && !st.Opts.DryRun() &&
		rsyncxattr.RequestFollows(attrs.Iflags, s.Protocol, s.CompatFlags&rsyncwire.CfAvoidXattrOptim != 0)
}

// writeNdxAndAttrs writes a file's index and attributes, then the xattr
// values the generator asked for (sender.c:write_ndx_and_attrs).
func (st *Transfer) writeNdxAndAttrs(ndx int32, fl file, attrs rsyncwire.ItemAttrs) error {
	if err := st.Conn.WriteNdxAndAttrs(ndx, attrs); err != nil {
		return err
	}
	if !st.xattrRequestFollows(attrs) {
		return nil
	}
	b, err := st.xattrState.AppendValues(nil, st.xattrCache.List(fl.xattrs), fl.xattrs, func(name string) ([]byte, error) {
		f, err := fl.source.Open(fl.path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		osf, ok := f.(*os.File)
		if !ok {
			return nil, fmt.Errorf("no xattrs in this source")
		}
		return rsyncxattr.Get(osf, name)
	})
	if err != nil {
		st.Logger.Printf("%s: %v", fl.Wpath, err)
		st.noteIOError(ioErrGeneral)
	}
	_, err = st.Conn.Writer.Write(b)
	return err
}
