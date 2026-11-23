package sender

import (
	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/outformat"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// item is fl for an output line, with the item flags the generator sent.
func (st *Transfer) item(fl file, a rsyncwire.ItemAttrs) outformat.Item {
	it := outformat.Item{
		Name:       fl.Wpath,
		Mode:       uint32(fl.Mode),
		Length:     fl.Length,
		ModTime:    fl.ModTime,
		Uid:        fl.Uid,
		Gid:        fl.Gid,
		LinkTarget: fl.LinkTarget,
		Iflags:     a.Iflags,
	}
	if a.Iflags&rsync.ITEM_XNAME_FOLLOWS != 0 {
		it.Hlink = a.Xname
	}
	return it
}

// bytesWritten is what the connection has written so far, for %b.
func (st *Transfer) bytesWritten() int64 {
	if cw, ok := st.Conn.Writer.(*rsyncwire.CountingWriter); ok {
		return cw.BytesWritten
	}
	return 0
}
