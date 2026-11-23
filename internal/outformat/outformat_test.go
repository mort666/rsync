package outformat

import (
	"testing"
	"time"

	"github.com/gokrazy/rsync"
)

func TestItemize(t *testing.T) {
	c := &Context{Op: "recv", PreserveMTimes: true, ReceiverSymlinkTimes: true}
	for _, tt := range []struct {
		mode   uint32
		iflags uint32
		op     string
		want   string
	}{
		{rsync.S_IFREG, rsync.ITEM_TRANSFER | rsync.ITEM_IS_NEW, "recv", ">f+++++++++"},
		{rsync.S_IFREG, rsync.ITEM_TRANSFER | rsync.ITEM_REPORT_SIZE | rsync.ITEM_REPORT_TIME, "recv", ">f.st......"},
		{rsync.S_IFREG, rsync.ITEM_TRANSFER | rsync.ITEM_REPORT_SIZE, "send", "<f.s......."},
		{rsync.S_IFDIR, rsync.ITEM_LOCAL_CHANGE | rsync.ITEM_IS_NEW, "recv", "cd+++++++++"},
		{rsync.S_IFDIR, rsync.ITEM_REPORT_TIME, "recv", ".d..t......"},
		{rsync.S_IFDIR, 0, "recv", ".d         "},
		{rsync.S_IFLNK, rsync.ITEM_LOCAL_CHANGE | rsync.ITEM_IS_NEW, "recv", "cL+++++++++"},
		{rsync.S_IFLNK, rsync.ITEM_LOCAL_CHANGE | rsync.ITEM_REPORT_CHANGE | rsync.ITEM_REPORT_TIME, "recv", "cLc.t......"},
		{rsync.S_IFREG, rsync.ITEM_LOCAL_CHANGE | rsync.ITEM_XNAME_FOLLOWS, "recv", "hf         "},
		{rsync.S_IFIFO, rsync.ITEM_LOCAL_CHANGE | rsync.ITEM_IS_NEW, "recv", "cS+++++++++"},
		{rsync.S_IFCHR, rsync.ITEM_REPORT_PERMS | rsync.ITEM_REPORT_OWNER | rsync.ITEM_REPORT_GROUP, "recv", ".D...pog..."},
		{rsync.S_IFREG, rsync.ITEM_REPORT_XATTR | rsync.ITEM_REPORT_ATIME | rsync.ITEM_REPORT_CRTIME, "recv", ".f......b.x"},
		{rsync.S_IFREG, rsync.ITEM_DELETED, "del.", "*deleting  "},
	} {
		c.Op = tt.op
		if got := c.itemize(Item{Mode: tt.mode, Iflags: tt.iflags}); got != tt.want {
			t.Errorf("itemize(%o, %#x, %s) = %q, want %q", tt.mode, tt.iflags, tt.op, got, tt.want)
		}
	}
}

func TestFormat(t *testing.T) {
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	c := &Context{Op: "recv", PreserveMTimes: true, PreserveGid: true, ChecksumLen: 16}
	file := Item{Name: "dir/a b", Mode: rsync.S_IFREG | 0o4755, Length: 1234567, ModTime: mod, Uid: 1000, Gid: 100,
		Iflags: rsync.ITEM_TRANSFER | rsync.ITEM_IS_NEW, Bytes: 42}
	dir := Item{Name: "dir", Mode: rsync.S_IFDIR | 0o755}
	link := Item{Name: "l", Mode: rsync.S_IFLNK | 0o777, LinkTarget: "dir/a b"}
	for _, tt := range []struct {
		format string
		it     Item
		want   string
	}{
		{"%n%L", file, "dir/a b"},
		{"%n%L", dir, "dir/"},
		{"%n%L", link, "l -> dir/a b"},
		{"%n%L", Item{Name: "h", Mode: rsync.S_IFREG, Hlink: "dir/a b"}, "h => dir/a b"},
		{"%n%5L|", file, "dir/a b         |"},
		{"%i %n%L", file, ">f+++++++++ dir/a b"},
		{"%l %'l %''l %'''l", file, "1234567 1,234,567 1.23M 1.18Mi"},
		{"[%10n][%-10n]", dir, "[      dir/][dir/      ]"},
		{"%B %M", file, "rwsr-xr-x 2026/01/02-03:04:05"},
		{"%U %G", file, "0 100"},
		{"%o %b %c", file, "recv 0 42"},
		{"%h %a %m %u %P %f", file, "%h %a %m %u %P dir/a b"},
		{"100%% %x", file, "100% %x"},
		{"%C|", file, "                                |"},
		{"%C", Item{Mode: rsync.S_IFREG, Checksum: []byte{0xde, 0xad}}, "dead"},
		{"tail%", file, "tail%"},
	} {
		if got, want := c.Format(tt.format, tt.it), tt.want+"\n"; got != want {
			t.Errorf("Format(%q) = %q, want %q", tt.format, got, want)
		}
	}
}

func TestHas(t *testing.T) {
	for _, tt := range []struct {
		format string
		esc    byte
		want   bool
	}{
		{"%i %n%L", 'i', true},
		{"%n%L", 'i', false},
		{"%-10i", 'i', true},
		{"%'''b", 'b', true},
		{"%%i", 'i', false},
		{"%%%i", 'i', true},
	} {
		if got := Has(tt.format, tt.esc); got != tt.want {
			t.Errorf("Has(%q, %c) = %v, want %v", tt.format, tt.esc, got, tt.want)
		}
	}
}

func TestBigNum(t *testing.T) {
	for _, tt := range []struct {
		n     int64
		human int
		want  string
	}{
		{0, 0, "0"},
		{999, 1, "999"},
		{1000, 1, "1,000"},
		{-1234567, 1, "-1,234,567"},
		{999, 2, "999"},
		{1000, 2, "1.00K"},
		{12345, 2, "12.3K"},
		{123456, 2, "123K"},
		{1024, 3, "1.00Ki"},
		{1 << 30, 3, "1.00Gi"},
	} {
		if got := BigNum(tt.n, tt.human); got != tt.want {
			t.Errorf("BigNum(%d, %d) = %q, want %q", tt.n, tt.human, got, tt.want)
		}
	}
}
