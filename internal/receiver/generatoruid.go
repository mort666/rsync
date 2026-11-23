//go:build linux || darwin

package receiver

import (
	"github.com/gokrazy/rsync"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

var isRoot = os.Getuid() == 0

// amRoot reports whether this side acts as root: it is, or --super.
func (rt *Transfer) amRoot() bool { return isRoot || rt.Opts.Super }

var inGroup = func() map[uint32]bool {
	m := make(map[uint32]bool)
	u, err := user.Current()
	if err != nil {
		return m
	}
	gids, err := u.GroupIds()
	if err != nil {
		return m
	}
	for _, gidString := range gids {
		gid64, err := strconv.ParseInt(gidString, 0, 64)
		if err != nil {
			return m
		}
		m[uint32(gid64)] = true
	}
	return m
}()

func (rt *Transfer) setUid(f *File, st fs.FileInfo) (fs.FileInfo, error) {
	stt := st.Sys().(*syscall.Stat_t)

	changeUid := rt.Opts.PreserveUid &&
		rt.amRoot() &&
		stt.Uid != uint32(f.Uid)

	changeGid := rt.Opts.PreserveGid &&
		(rt.amRoot() || inGroup[uint32(f.Gid)]) &&
		stt.Gid != uint32(f.Gid)

	if !changeUid && !changeGid {
		return st, nil
	}

	uid := stt.Uid
	if changeUid {
		uid = uint32(f.Uid)
	}
	gid := stt.Gid
	if changeGid {
		gid = uint32(f.Gid)
	}
	if err := rt.DestRoot.Lchown(f.Name, int(uid), int(gid)); err != nil {
		return nil, err
	}
	return rt.DestRoot.Lstat(f.Name)
}

// ownerDiffers reports whether f's owner or group differ from st's, in a
// way this receiver would change (generator.c:ownership_differs).
func (rt *Transfer) ownerDiffers(f *File, st fs.FileInfo) bool {
	stt, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if rt.Opts.PreserveUid && rt.amRoot() && stt.Uid != uint32(f.Uid) {
		return true
	}
	return rt.Opts.PreserveGid && (rt.amRoot() || inGroup[uint32(f.Gid)]) && stt.Gid != uint32(f.Gid)
}

// ownerFlags are ITEM_REPORT_OWNER and ITEM_REPORT_GROUP for what this
// receiver would change of st's owner and group (generator.c:itemize).
func (rt *Transfer) ownerFlags(f *File, st fs.FileInfo) uint32 {
	stt, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	var iflags uint32
	if rt.Opts.PreserveUid && rt.amRoot() && stt.Uid != uint32(f.Uid) {
		iflags |= rsync.ITEM_REPORT_OWNER
	}
	if rt.Opts.PreserveGid && (rt.amRoot() || inGroup[uint32(f.Gid)]) && stt.Gid != uint32(f.Gid) {
		iflags |= rsync.ITEM_REPORT_GROUP
	}
	return iflags
}
