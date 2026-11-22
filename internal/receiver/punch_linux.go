package receiver

import (
	"os"

	"golang.org/x/sys/unix"
)

// punchHole makes n bytes of f at off a hole, keeping f's size, or zeros
// them where the file system cannot (rsync/syscall.c:do_punch_hole).
func punchHole(f *os.File, off, n int64) error {
	fd := int(f.Fd())
	if unix.Fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n) == nil {
		return nil
	}
	if unix.Fallocate(fd, unix.FALLOC_FL_ZERO_RANGE, off, n) == nil {
		return nil
	}
	return writeZeros(f, off, n)
}
