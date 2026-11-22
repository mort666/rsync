package sender

import (
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/gokrazy/rsync/internal/rsyncxattr"
)

// --fake-super on the sending side (x_lstat, do_readlink): a regular file
// or folder with a user.rsync.%stat xattr goes in the file list as that
// record says, with its mode, owner and device number, and a symlink's
// placeholder with the target it holds.

// maxLinkTarget bounds what a symlink's placeholder is read for
// (PATH_MAX).
const maxLinkTarget = 4096

// statDir opens the folder that holds path in source, for reading its
// %stat by name, or returns nil for a source not on disk.
func statDir(source FileSource, p string) (*os.File, string, error) {
	switch s := source.(type) {
	case *osRootSource:
		dir, err := s.root.Open(filepath.Dir(p))
		return dir, filepath.Base(p), err
	case *singleFileSource:
		dir, err := os.Open(filepath.Dir(s.path))
		return dir, filepath.Base(s.path), err
	case *subSource:
		return statDir(s.underlying, path.Join(s.subdir, p))
	}
	return nil, "", nil
}

// fakeStat applies path's %stat record to info, and reports whether it
// makes a symlink of a placeholder.
func (st *Transfer) fakeStat(source FileSource, p string, info fs.FileInfo) (fs.FileInfo, bool, error) {
	if !info.Mode().IsRegular() && !info.IsDir() {
		return info, false, nil
	}
	dir, base, err := statDir(source, p)
	if err != nil || dir == nil {
		return info, false, err
	}
	defer dir.Close()
	s, ok, err := rsyncxattr.ReadStat(dir, base, info)
	if err != nil || !ok {
		return info, false, err
	}
	fake := rsyncxattr.FakeInfo(info, s)
	return fake, info.Mode().IsRegular() && fake.Mode().Type() == fs.ModeSymlink, nil
}

// readPlaceholder reads the target a symlink's placeholder holds.
func readPlaceholder(source FileSource, p string) (string, error) {
	f, err := source.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxLinkTarget))
	return string(b), err
}
