package sender

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/rsyncxattr"
)

// xattrs reports whether the session carries extended attributes (-X).
func (st *Transfer) xattrs() bool { return st.Opts.PreserveXattrs() > 0 }

func (st *Transfer) xattrFilter() rsyncxattr.Filter {
	return rsyncxattr.Filter{
		Level: st.Opts.PreserveXattrs(),
		Root:  (os.Geteuid() == 0 || st.Opts.Super()) && !st.Opts.FakeSuper(),
		Fake:  st.Opts.FakeSuper(),
	}
}

// xattrFile reaches one source file's xattrs: through a descriptor for
// regular files and folders, or by name in its folder for symlinks,
// devices and specials, which cannot be opened (a FIFO would block).
type xattrFile struct {
	f    *os.File // regular file or folder
	dir  *os.File // the folder of name, for other types
	name string
}

// openXattrs opens path's xattrs in source, or returns nil when there are
// none to read: a source that is not on disk, or other types where
// /proc/self/fd is hidden.
func openXattrs(source FileSource, path string, regularOrDir bool) (*xattrFile, error) {
	if regularOrDir {
		f, err := source.Open(path)
		if err != nil {
			return nil, err
		}
		osf, ok := f.(*os.File)
		if !ok {
			f.Close()
			return nil, nil // a source without xattrs
		}
		return &xattrFile{f: osf}, nil
	}
	rs, ok := source.(*osRootSource)
	if !ok || !rsyncxattr.ProcFDAvailable() {
		return nil, nil
	}
	dir, err := rs.root.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return &xattrFile{dir: dir, name: filepath.Base(path)}, nil
}

func (x *xattrFile) read(keep func(name string) bool) (map[string][]byte, error) {
	if x.f != nil {
		return rsyncxattr.Read(x.f, keep)
	}
	return rsyncxattr.ReadAt(x.dir, x.name, keep)
}

func (x *xattrFile) get(attr string) ([]byte, error) {
	if x.f != nil {
		return rsyncxattr.Get(x.f, attr)
	}
	return rsyncxattr.GetAt(x.dir, x.name, attr)
}

func (x *xattrFile) Close() error {
	if x.f != nil {
		return x.f.Close()
	}
	return x.dir.Close()
}

// readXattrs is the list a file-list entry carries (get_xattr), for any
// type the file list holds, as rsync reads them (lgetxattr for what is not
// a regular file or folder).
func (st *Transfer) readXattrs(source FileSource, path string, regularOrDir bool) (rsyncxattr.List, error) {
	x, err := openXattrs(source, path, regularOrDir)
	if err != nil || x == nil {
		return nil, err
	}
	defer x.Close()
	filter := st.xattrFilter()
	values, err := x.read(func(name string) bool { return filter.KeepLocal(name, true) })
	if err != nil {
		return nil, err
	}
	if filter.Fake {
		// What --fake-super kept under user.rsync. goes back to its own
		// namespace (send_xattr).
		sent := make(map[string][]byte, len(values))
		for name, v := range values {
			sent[rsyncxattr.SendName(name)] = v
		}
		values = sent
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
		x, err := openXattrs(fl.source, fl.path, fl.regular || fl.isDir)
		if err != nil {
			return nil, err
		}
		if x == nil {
			return nil, fmt.Errorf("no xattrs in this source")
		}
		defer x.Close()
		if st.xattrFilter().Fake {
			name = rsyncxattr.LocalName(name)
		}
		return x.get(name)
	})
	if err != nil {
		st.Logger.Printf("%s: %v", fl.Wpath, err)
		st.noteIOError(ioErrGeneral)
	}
	_, err = st.Conn.Writer.Write(b)
	return err
}
