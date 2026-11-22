//go:build linux

package rsyncxattr

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// Supported reports whether this platform has extended attributes for -X.
// rsync also has them on macOS and the BSDs, with names mapped into and
// out of the user namespace; gokrazy/rsync does Linux only so far.
const Supported = true

// ops reach one file's extended attributes: through a descriptor, or by
// name in a folder for what cannot be opened (symlinks, devices, FIFOs).
type ops struct {
	what   string
	list   func(buf []byte) (int, error)
	get    func(name string, buf []byte) (int, error)
	set    func(name string, v []byte) error
	remove func(name string) error
}

func fdOps(f *os.File) ops {
	fd := int(f.Fd())
	return ops{
		what:   f.Name(),
		list:   func(buf []byte) (int, error) { return unix.Flistxattr(fd, buf) },
		get:    func(name string, buf []byte) (int, error) { return unix.Fgetxattr(fd, name, buf) },
		set:    func(name string, v []byte) error { return unix.Fsetxattr(fd, name, v, 0) },
		remove: func(name string) error { return unix.Fremovexattr(fd, name) },
	}
}

// atOps reach name in dir by its path under /proc/self/fd, with the
// calls that do not follow a symlink at the end (rsync's l*xattr). The
// folder is the one dir opened, so a symlink swapped in above name cannot
// send them elsewhere.
func atOps(dir *os.File, name string) ops {
	p := "/proc/self/fd/" + strconv.Itoa(int(dir.Fd())) + "/" + name
	return ops{
		what:   dir.Name() + "/" + name,
		list:   func(buf []byte) (int, error) { return unix.Llistxattr(p, buf) },
		get:    func(attr string, buf []byte) (int, error) { return unix.Lgetxattr(p, attr, buf) },
		set:    func(attr string, v []byte) error { return unix.Lsetxattr(p, attr, v, 0) },
		remove: func(attr string) error { return unix.Lremovexattr(p, attr) },
	}
}

// ProcFDAvailable reports whether /proc/self/fd is there to reach files by
// name (ReadAt, SetAt). A sandbox may hide it.
var ProcFDAvailable = sync.OnceValue(func() bool {
	_, err := os.Stat("/proc/self/fd")
	return err == nil
})

// Read returns f's extended attributes that keep takes, by name. A
// filesystem without them has none.
func Read(f *os.File, keep func(name string) bool) (map[string][]byte, error) {
	return fdOps(f).read(keep)
}

// ReadAt is Read for name in folder dir, without following a symlink:
// for symlinks, devices and specials, which cannot be opened.
func ReadAt(dir *os.File, name string, keep func(name string) bool) (map[string][]byte, error) {
	return atOps(dir, name).read(keep)
}

// Get reads one of f's extended attributes.
func Get(f *os.File, name string) ([]byte, error) { return fdOps(f).getValue(name) }

// GetAt is Get for name in folder dir, without following a symlink.
func GetAt(dir *os.File, name, attr string) ([]byte, error) { return atOps(dir, name).getValue(attr) }

// Set makes f's extended attributes that keep covers exactly values:
// it sets those that differ from current (what Read returned) and removes
// the others (rsync_xal_set).
func Set(f *os.File, values, current map[string][]byte, keep func(name string) bool) error {
	return fdOps(f).setAll(values, current, keep)
}

// SetAt is Set for name in folder dir, without following a symlink.
func SetAt(dir *os.File, name string, values, current map[string][]byte, keep func(name string) bool) error {
	return atOps(dir, name).setAll(values, current, keep)
}

func (o ops) read(keep func(name string) bool) (map[string][]byte, error) {
	buf := make([]byte, 1024)
	var n int
	for {
		var err error
		n, err = o.list(buf)
		if err == nil {
			break
		}
		if errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		if !errors.Is(err, unix.ERANGE) {
			return nil, &os.PathError{Op: "listxattr", Path: o.what, Err: err}
		}
		size, err := o.list(nil)
		if err != nil {
			return nil, &os.PathError{Op: "listxattr", Path: o.what, Err: err}
		}
		buf = make([]byte, size+1024)
	}
	values := make(map[string][]byte)
	for _, name := range bytes.Split(buf[:n], []byte{0}) {
		if len(name) == 0 || !keep(string(name)) {
			continue
		}
		v, err := o.getValue(string(name))
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				continue // removed since it was listed
			}
			return nil, err
		}
		values[string(name)] = v
	}
	return values, nil
}

func (o ops) getValue(name string) ([]byte, error) {
	for {
		size, err := o.get(name, nil)
		if err != nil {
			return nil, &os.PathError{Op: "getxattr " + name, Path: o.what, Err: err}
		}
		buf := make([]byte, size)
		n, err := o.get(name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue // it grew
		}
		if err != nil {
			return nil, &os.PathError{Op: "getxattr " + name, Path: o.what, Err: err}
		}
		return buf[:n], nil
	}
}

func (o ops) setAll(values, current map[string][]byte, keep func(name string) bool) error {
	var errs []error
	for name, v := range values {
		if cur, ok := current[name]; ok && bytes.Equal(cur, v) {
			continue
		}
		if err := o.set(name, v); err != nil {
			errs = append(errs, &os.PathError{Op: "setxattr " + name, Path: o.what, Err: err})
		}
	}
	for name := range current {
		if _, ok := values[name]; ok || !keep(name) {
			continue
		}
		if err := o.remove(name); err != nil && !errors.Is(err, unix.ENODATA) {
			errs = append(errs, &os.PathError{Op: "removexattr " + name, Path: o.what, Err: err})
		}
	}
	return errors.Join(errs...)
}
