//go:build !linux

package rsyncxattr

import (
	"errors"
	"os"
)

// Supported reports whether this platform has extended attributes for -X.
const Supported = false

var errUnsupported = errors.New("extended attributes are not supported on this platform")

func Read(f *os.File, keep func(name string) bool) (map[string][]byte, error) {
	return nil, errUnsupported
}

func Get(f *os.File, name string) ([]byte, error) { return nil, errUnsupported }

func Set(f *os.File, values, current map[string][]byte, keep func(name string) bool) error {
	return errUnsupported
}

// ProcFDAvailable reports whether files can be reached by name (ReadAt,
// SetAt).
func ProcFDAvailable() bool { return false }

func ReadAt(dir *os.File, name string, keep func(name string) bool) (map[string][]byte, error) {
	return nil, errUnsupported
}

func GetAt(dir *os.File, name, attr string) ([]byte, error) { return nil, errUnsupported }

func SetAt(dir *os.File, name string, values, current map[string][]byte, keep func(name string) bool) error {
	return errUnsupported
}
