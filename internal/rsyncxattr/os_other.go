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
