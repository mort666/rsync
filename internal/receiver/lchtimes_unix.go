//go:build unix

package receiver

import (
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// lchtimes sets the times of the symlink name itself, as rsync does where
// it can (CAN_SET_SYMLINK_TIMES, utimensat with AT_SYMLINK_NOFOLLOW).
func (rt *Transfer) lchtimes(name string, mtime time.Time) error {
	dir, err := rt.DestRoot.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer dir.Close()
	ts := unix.NsecToTimespec(mtime.UnixNano())
	return unix.UtimesNanoAt(int(dir.Fd()), filepath.Base(name), []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}
