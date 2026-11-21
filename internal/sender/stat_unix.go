//go:build linux || darwin

package sender

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

func uidFromFileInfo(info fs.FileInfo) (int32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int32(st.Uid), true
}

func gidFromFileInfo(info fs.FileInfo) (int32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int32(st.Gid), true
}

func rdevFromFileInfo(info fs.FileInfo) (int32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int32(st.Rdev), true
}

// rdevMajorMinor splits a device file's number, as protocol 28 sends it.
func rdevMajorMinor(info fs.FileInfo) (major, minor uint32) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev))
}

// hardLinkID returns what identifies a file across hard links.
func hardLinkID(info fs.FileInfo) (dev, ino int64, nlink uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return int64(st.Dev), int64(st.Ino), uint64(st.Nlink), true
}
