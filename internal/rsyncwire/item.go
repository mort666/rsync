package rsyncwire

import "fmt"

// The item flags this package needs to read and write a file index's
// attributes. They are rsync.ITEM_* and rsync.FNAMECMP_FNAME, which this
// package cannot import (package rsync imports it).
const (
	itemBasisTypeFollows = 1 << 11
	itemXnameFollows     = 1 << 12
	itemIsNew            = 1 << 13
	itemTransfer         = 1 << 15
	itemMissingData      = 1 << 16 // never on the wire

	fnamecmpFname = 0x80
)

// maxPathLen is rsync's MAXPATHLEN, the limit for an alternate name.
const maxPathLen = 4096

// ItemAttrs is what follows a file index from protocol 29: the item flags,
// and the basis type and alternate name when the flags say they follow.
type ItemAttrs struct {
	Iflags    uint32
	BasisType byte   // rsync.FNAMECMP_FNAME unless ITEM_BASIS_TYPE_FOLLOWS
	Xname     string // only with ITEM_XNAME_FOLLOWS
}

// Transfer reports whether the generator wants the file's data
// (ITEM_TRANSFER).
func (a ItemAttrs) Transfer() bool { return a.Iflags&itemTransfer != 0 }

// WriteNdxAndAttrs writes a file index, and from protocol 29 its attributes
// (rsync/sender.c:write_ndx_and_attrs, which the generator mirrors).
func (c *Conn) WriteNdxAndAttrs(ndx int32, a ItemAttrs) error {
	if err := c.WriteNdx(ndx); err != nil {
		return err
	}
	if c.Session.Protocol < 29 {
		return nil
	}
	if err := c.WriteShort(uint16(a.Iflags)); err != nil {
		return err
	}
	if a.Iflags&itemBasisTypeFollows != 0 {
		if _, err := c.Writer.Write([]byte{a.BasisType}); err != nil {
			return err
		}
	}
	if a.Iflags&itemXnameFollows != 0 {
		return c.WriteVstring(a.Xname)
	}
	return nil
}

// ReadNdxAndAttrs reads what WriteNdxAndAttrs wrote
// (rsync/rsync.c:read_ndx_and_attrs, without incremental recursion). It
// returns NdxDone as it is. Below protocol 29 there are no attributes, and
// every index is a transfer. numFiles is the length of the file list: a
// protocol 29 keep-alive names the index one past its end.
func (c *Conn) ReadNdxAndAttrs(numFiles int) (int32, ItemAttrs, error) {
	for {
		ndx, err := c.ReadNdx()
		if err != nil {
			return 0, ItemAttrs{}, err
		}
		if ndx == NdxDone {
			return ndx, ItemAttrs{}, nil
		}
		if ndx == NdxDelStats && c.Session.Protocol >= 31 {
			// What --delete removed (protocol 31+).
			d, err := c.readDelStats()
			if err != nil {
				return 0, ItemAttrs{}, err
			}
			c.DelStats, c.GotDelStats = d, true
			continue
		}
		if ndx < 0 {
			return 0, ItemAttrs{}, fmt.Errorf("invalid file index: %d (%d - %d)", ndx, NdxDone, numFiles-1)
		}
		a := ItemAttrs{Iflags: itemTransfer | itemMissingData, BasisType: fnamecmpFname}
		if c.Session.Protocol >= 29 {
			iflags, err := c.ReadShort()
			if err != nil {
				return 0, ItemAttrs{}, err
			}
			a.Iflags = uint32(iflags)
		}
		// The protocol 29 keep-alive: nothing to do.
		if c.Session.Protocol < 30 && int(ndx) == numFiles && a.Iflags == itemIsNew {
			continue
		}
		if int(ndx) >= numFiles {
			return 0, ItemAttrs{}, fmt.Errorf("invalid file index: %d (%d - %d)", ndx, NdxDone, numFiles-1)
		}
		if a.Iflags&itemBasisTypeFollows != 0 {
			b, err := c.ReadByte()
			if err != nil {
				return 0, ItemAttrs{}, err
			}
			a.BasisType = b
		}
		if a.Iflags&itemXnameFollows != 0 {
			if a.Xname, err = c.ReadVstring(maxPathLen); err != nil {
				return 0, ItemAttrs{}, err
			}
		}
		return ndx, a, nil
	}
}
