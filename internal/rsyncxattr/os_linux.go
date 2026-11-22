//go:build linux

package rsyncxattr

import (
	"bytes"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Supported reports whether this platform has extended attributes for -X.
// rsync also has them on macOS and the BSDs, with names mapped into and
// out of the user namespace; gokrazy/rsync does Linux only so far.
const Supported = true

// Read returns f's extended attributes that keep takes, by name. A
// filesystem without them has none.
func Read(f *os.File, keep func(name string) bool) (map[string][]byte, error) {
	fd := int(f.Fd())
	buf := make([]byte, 1024)
	var n int
	for {
		var err error
		n, err = unix.Flistxattr(fd, buf)
		if err == nil {
			break
		}
		if errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		if !errors.Is(err, unix.ERANGE) {
			return nil, &os.PathError{Op: "flistxattr", Path: f.Name(), Err: err}
		}
		size, err := unix.Flistxattr(fd, nil)
		if err != nil {
			return nil, &os.PathError{Op: "flistxattr", Path: f.Name(), Err: err}
		}
		buf = make([]byte, size+1024)
	}
	values := make(map[string][]byte)
	for _, name := range bytes.Split(buf[:n], []byte{0}) {
		if len(name) == 0 || !keep(string(name)) {
			continue
		}
		v, err := Get(f, string(name))
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

// Get reads one of f's extended attributes.
func Get(f *os.File, name string) ([]byte, error) {
	fd := int(f.Fd())
	for {
		size, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			return nil, &os.PathError{Op: "fgetxattr " + name, Path: f.Name(), Err: err}
		}
		buf := make([]byte, size)
		n, err := unix.Fgetxattr(fd, name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue // it grew
		}
		if err != nil {
			return nil, &os.PathError{Op: "fgetxattr " + name, Path: f.Name(), Err: err}
		}
		return buf[:n], nil
	}
}

// Set makes f's extended attributes that keep covers exactly values:
// it sets those that differ from current (what Read returned) and removes
// the others (rsync_xal_set).
func Set(f *os.File, values, current map[string][]byte, keep func(name string) bool) error {
	fd := int(f.Fd())
	var errs []error
	for name, v := range values {
		if cur, ok := current[name]; ok && bytes.Equal(cur, v) {
			continue
		}
		if err := unix.Fsetxattr(fd, name, v, 0); err != nil {
			errs = append(errs, &os.PathError{Op: "fsetxattr " + name, Path: f.Name(), Err: err})
		}
	}
	for name := range current {
		if _, ok := values[name]; ok || !keep(name) {
			continue
		}
		if err := unix.Fremovexattr(fd, name); err != nil && !errors.Is(err, unix.ENODATA) {
			errs = append(errs, &os.PathError{Op: "fremovexattr " + name, Path: f.Name(), Err: err})
		}
	}
	return errors.Join(errs...)
}
