package receiver

import (
	"path/filepath"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/outformat"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// item is f for an output line, with the item flags it came with.
func (f *File) item(a rsyncwire.ItemAttrs) outformat.Item {
	it := outformat.Item{
		Name:       filepath.ToSlash(f.Name),
		Mode:       uint32(f.Mode),
		Length:     f.Length,
		ModTime:    f.ModTime,
		Uid:        f.Uid,
		Gid:        f.Gid,
		LinkTarget: f.LinkTarget,
		Iflags:     a.Iflags,
		Checksum:   f.Checksum,
	}
	if a.Iflags&rsync.ITEM_XNAME_FOLLOWS != 0 {
		it.Hlink = a.Xname
	}
	return it
}

// bytesRead is what the connection has read so far, for %b.
func (rt *Transfer) bytesRead() int64 {
	if cr, ok := rt.Conn.Reader.(*rsyncwire.CountingReader); ok {
		return cr.BytesRead
	}
	return 0
}
