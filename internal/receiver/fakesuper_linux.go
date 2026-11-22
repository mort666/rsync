package receiver

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsyncxattr"
)

// --fake-super on the receiving side (xattrs.c, syscall.c): devices,
// specials and symlinks become regular files, owners and modes stay as
// this user can have them, and what the sender had goes into the
// user.rsync.%stat xattr, which this file reads back and writes.

// maxLinkTarget bounds what a symlink's placeholder is read for
// (PATH_MAX), so that a large regular file is not read whole.
const maxLinkTarget = 4096

// lstat stats name at the destination, as --fake-super sees it (x_lstat):
// with %stat's mode, owner and device number applied.
func (rt *Transfer) lstat(name string) (fs.FileInfo, error) {
	st, err := rt.DestRoot.Lstat(name)
	if err != nil || !rt.Opts.FakeSuper {
		return st, err
	}
	dir, err := rt.DestRoot.Open(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	s, ok, err := rsyncxattr.ReadStat(dir, filepath.Base(name), st)
	if err != nil {
		return nil, err
	}
	if !ok {
		return st, nil
	}
	return rsyncxattr.FakeInfo(st, s), nil
}

// readlink reads a symlink's target, or under --fake-super what its
// placeholder holds (do_readlink). A real symlink there reads as no
// target, so that it is replaced with a placeholder.
func (rt *Transfer) readlink(name string) (string, error) {
	if !rt.Opts.FakeSuper {
		return rt.DestRoot.Readlink(name)
	}
	f, err := rt.DestRoot.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxLinkTarget))
	return string(b), err
}

// placeholder replaces name with a regular file of mode 0600 holding
// content, without following a symlink there (do_symlink_at, do_mknod_at
// under --fake-super).
func (rt *Transfer) placeholder(name string, content string) error {
	if err := rt.DestRoot.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := rt.DestRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fakeDevice makes f's placeholder unless one of its type is there
// already; setPerms then records the device number.
func (rt *Transfer) fakeDevice(f *File, st fs.FileInfo) error {
	if st != nil && st.Mode().Type() == f.FileMode().Type() {
		if real, err := rt.DestRoot.Lstat(f.Name); err == nil && real.Mode().IsRegular() {
			return nil
		}
	}
	return rt.placeholder(f.Name, "")
}

// setFakeStat is set_stat_xattr: it gives the real file the permissions
// this user needs and none that only root may set, and records in %stat
// what differs from the real file: the sender's mode with its setuid,
// setgid and sticky bits, the device number, and the owner.
func (rt *Transfer) setFakeStat(f *File, perm fs.FileMode, st fs.FileInfo) error {
	if rt.Opts.DryRun {
		return nil
	}
	stt, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	dir, err := rt.DestRoot.Open(filepath.Dir(f.Name))
	if err != nil {
		return err
	}
	defer dir.Close()
	base := filepath.Base(f.Name)
	old, had, err := rsyncxattr.ReadStat(dir, base, st)
	if err != nil {
		return err
	}

	bits := uint32(perm.Perm())
	if rt.Opts.PreservePerms {
		bits = uint32(f.Mode) & 0o7777
	} else if had {
		bits = bits | old.Mode&0o7000
	}
	want := rsyncxattr.Stat{Mode: uint32(f.Mode)&rsync.S_IFMT | bits, Uid: stt.Uid, Gid: stt.Gid}
	if had {
		want.Uid, want.Gid = old.Uid, old.Gid
	}
	if rt.Opts.PreserveUid {
		want.Uid = uint32(f.Uid)
	}
	if rt.Opts.PreserveGid {
		want.Gid = uint32(f.Gid)
	}
	if t := f.Mode & rsync.S_IFMT; t == rsync.S_IFCHR || t == rsync.S_IFBLK {
		want.Major, want.Minor = splitRdev(f.Rdev)
	}

	// The real file: the permissions, without the special bits, and
	// always enough for this user to read and change it.
	owner := fs.FileMode(0o600)
	if st.IsDir() {
		owner = 0o700
	}
	realPerm := fs.FileMode(bits&0o777) | owner
	if st.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != realPerm {
		if err := rt.DestRoot.Chmod(f.Name, realPerm); err != nil {
			return err
		}
	}
	real := rsyncxattr.Stat{Mode: stt.Mode&syscall.S_IFMT | uint32(realPerm), Uid: stt.Uid, Gid: stt.Gid}

	switch {
	case want == real:
		if had {
			return rsyncxattr.WriteStat(dir, base, nil)
		}
	case !had || old != want:
		return rsyncxattr.WriteStat(dir, base, &want)
	}
	return nil
}
