//go:build linux || darwin

package receiver

import "golang.org/x/sys/unix"

// makeRdev turns the major and minor device numbers protocol 28 sends into
// this system's dev_t, as mknod wants it.
func makeRdev(major, minor uint32) int32 { return int32(unix.Mkdev(major, minor)) }

func splitRdev(rdev int32) (major, minor uint32) {
	return unix.Major(uint64(uint32(rdev))), unix.Minor(uint64(uint32(rdev)))
}
