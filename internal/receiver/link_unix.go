//go:build unix

package receiver

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// linkAcross hard-links name in from to name in to: linkat between their
// folders, opened through each root.
func linkAcross(from, to *os.Root, name string) error {
	src, err := from.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := to.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer dst.Close()
	base := filepath.Base(name)
	return unix.Linkat(int(src.Fd()), base, int(dst.Fd()), base, 0)
}

// rdevOf is a device's major and minor number.
func rdevOf(st fs.FileInfo) (major, minor uint32) {
	stt, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return unix.Major(uint64(stt.Rdev)), unix.Minor(uint64(stt.Rdev))
}
