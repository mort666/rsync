//go:build !linux

package receiver

import "os"

// punchHole zeros n bytes of f at off: rsync punches holes only through
// Linux's fallocate (rsync/syscall.c:do_punch_hole).
func punchHole(f *os.File, off, n int64) error { return writeZeros(f, off, n) }
