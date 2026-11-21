package rsync

import (
	"bytes"
	"io"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

type nopWC struct{ io.Writer }

func (nopWC) Close() error { return nil }

// rsyncwire keeps its own copy of the item flags it reads and writes, as it
// cannot import this package. Write each through it and check the bytes.
func TestItemFlagsMatchRsyncwire(t *testing.T) {
	for _, tt := range []struct {
		attrs rsyncwire.ItemAttrs
		want  []byte
	}{
		{rsyncwire.ItemAttrs{Iflags: ITEM_TRANSFER | ITEM_IS_NEW, BasisType: FNAMECMP_FNAME}, []byte{0x00, 0xa0}},
		{rsyncwire.ItemAttrs{Iflags: ITEM_TRANSFER | ITEM_BASIS_TYPE_FOLLOWS, BasisType: FNAMECMP_PARTIAL_DIR}, []byte{0x00, 0x88, 0x81}},
		{rsyncwire.ItemAttrs{Iflags: ITEM_XNAME_FOLLOWS, BasisType: FNAMECMP_FNAME, Xname: "ab"}, []byte{0x00, 0x10, 2, 'a', 'b'}},
	} {
		var buf bytes.Buffer
		c := &rsyncwire.Conn{Writer: nopWC{&buf}, Reader: io.NopCloser(&buf), Session: rsyncwire.Session{Protocol: 29}}
		if err := c.WriteNdxAndAttrs(0, tt.attrs); err != nil {
			t.Fatal(err)
		}
		// Index 0 is a one-byte delta below 30: a 4-byte int.
		if got := buf.Bytes()[4:]; !bytes.Equal(got, tt.want) {
			t.Errorf("attrs %+v: wrote %x, want %x", tt.attrs, got, tt.want)
		}
		ndx, got, err := c.ReadNdxAndAttrs(1)
		if err != nil || ndx != 0 || got != tt.attrs {
			t.Errorf("read back %d, %+v, %v; want 0, %+v", ndx, got, err, tt.attrs)
		}
	}
	if (rsyncwire.ItemAttrs{Iflags: ITEM_TRANSFER}).Transfer() != true {
		t.Errorf("ItemAttrs.Transfer does not see ITEM_TRANSFER")
	}
}
