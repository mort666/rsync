package receiver

import (
	"io/fs"
	"path/filepath"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/outformat"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// itemFlags are the item flags for f against st, what was at the
// destination before the generator changed anything, or nil for nothing
// there (generator.c:itemize).
func (rt *Transfer) itemFlags(f *File, st fs.FileInfo, iflags uint32) uint32 {
	if st == nil {
		if rt.genXattr.differs {
			iflags |= rsync.ITEM_REPORT_XATTR
		}
		return iflags | rsync.ITEM_IS_NEW
	}
	mode := f.Mode & rsync.S_IFMT
	keepTime := rt.Opts.PreserveTimes && !(mode == rsync.S_IFDIR && rt.Opts.OmitDirTimes)
	if mode == rsync.S_IFREG && st.Size() != f.Length {
		iflags |= rsync.ITEM_REPORT_SIZE
	}
	mtime := st.ModTime()
	if t, ok := rt.preDeleteTimes[filepath.ToSlash(f.Name)]; ok && mode == rsync.S_IFDIR {
		mtime = t
	}
	if keepTime {
		if !rt.modTimeEqual(mtime, f.ModTime) {
			iflags |= rsync.ITEM_REPORT_TIME
		}
	} else if iflags&(rsync.ITEM_TRANSFER|rsync.ITEM_LOCAL_CHANGE) != 0 {
		iflags |= rsync.ITEM_REPORT_TIME
	}
	if mode != rsync.S_IFLNK && rt.Opts.PreservePerms && goModeBits(st.Mode()) != uint32(f.Mode)&0o7777 {
		iflags |= rsync.ITEM_REPORT_PERMS
	}
	iflags |= rt.ownerFlags(f, st)
	if rt.genXattr.differs {
		iflags |= rsync.ITEM_REPORT_XATTR
	}
	return iflags
}

// itemize reports f to the sender, which echoes it back for the client
// to print, when it is worth a line; below protocol 29 a client prints it
// here (generator.c:itemize). st is as for itemFlags.
func (rt *Transfer) itemize(idx int, f *File, st fs.FileInfo, iflags uint32, xname string) error {
	iflags = rt.itemFlags(f, st, iflags) & 0xffff
	if xname != "" {
		iflags |= rsync.ITEM_XNAME_FOLLOWS
	}
	const significant = 0xffff &^ (rsync.ITEM_BASIS_TYPE_FOLLOWS | rsync.ITEM_XNAME_FOLLOWS | rsync.ITEM_LOCAL_CHANGE)
	if iflags&(significant|rsync.ITEM_REPORT_XATTR) == 0 && !rt.Opts.ItemizeAll && xname == "" {
		return nil
	}
	if rt.Conn.Session.Protocol < 29 {
		if rt.Out.Itemizing() {
			code := outformat.Client
			if rt.Out.LogFileHasI {
				code = outformat.Info
			}
			rt.Out.LogItem(code, f.item(rsyncwire.ItemAttrs{Iflags: iflags, Xname: xname}))
		}
		return nil
	}
	a := rsyncwire.ItemAttrs{Iflags: iflags, BasisType: rsync.FNAMECMP_FNAME, Xname: xname}
	req := rt.addXattrRequest(&a)
	if err := rt.Conn.WriteNdxAndAttrs(int32(idx), a); err != nil {
		return err
	}
	_, err := rt.Conn.Writer.Write(req)
	return err
}
