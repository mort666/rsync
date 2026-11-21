package receiver

import (
	"bytes"
	"io"
	"testing"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// flistBuilder writes file-list entries the way rsync's send_file_entry
// does below protocol 30, without the name and attribute compression this
// receiver already handles at 27.
type flistBuilder struct{ rsyncwire.Buffer }

func (b *flistBuilder) entry(xflags uint16, name string, mode int32) {
	if xflags&0xff00 != 0 {
		xflags |= rsync.XMIT_EXTENDED_FLAGS
		b.WriteByte(byte(xflags))
		b.WriteByte(byte(xflags >> 8))
	} else {
		b.WriteByte(byte(xflags))
	}
	b.WriteByte(byte(len(name)))
	b.WriteString(name)
	b.WriteInt64(0)          // length
	b.WriteInt32(1700000000) // mtime
	b.WriteInt32(mode)
}

func receiveList(t *testing.T, proto int32, opts *TransferOpts, wire []byte) []*File {
	t.Helper()
	opts.InfoGTE = func(rsyncopts.InfoLevel, uint16) bool { return false }
	opts.DebugGTE = func(rsyncopts.DebugLevel, uint16) bool { return false }
	rt := &Transfer{
		Logger: log.New(io.Discard),
		Opts:   opts,
		Env:    &rsyncos.Env{},
		Conn: &rsyncwire.Conn{
			Reader:  io.NopCloser(bytes.NewReader(wire)),
			Session: rsyncwire.Session{Protocol: proto},
		},
	}
	files, err := rt.ReceiveFileList()
	if err != nil {
		t.Fatalf("ReceiveFileList: %v", err)
	}
	return files
}

func byName(files []*File) map[string]*File {
	m := make(map[string]*File)
	for _, f := range files {
		m[f.Name] = f
	}
	return m
}

// Protocol 28 device numbers and hard-link data, in the shapes rsync's
// sender produces them.
func TestReceiveFileList28Devices(t *testing.T) {
	const (
		chr  = rsync.S_IFCHR | 0o644
		blk  = rsync.S_IFBLK | 0o644
		reg  = rsync.S_IFREG | 0o644
		fifo = rsync.S_IFIFO | 0o644
	)
	var b flistBuilder
	// A minor that does not fit a byte: major and minor as integers.
	b.entry(rsync.XMIT_TOP_DIR, "a", chr)
	b.WriteInt32(300)
	b.WriteInt32(70000)
	// A regular file between devices must not reset the major.
	b.entry(rsync.XMIT_TOP_DIR, "b", reg)
	// Same major as "a" (not sent), minor in one byte.
	b.entry(rsync.XMIT_TOP_DIR|rsync.XMIT_SAME_RDEV_MAJOR|rsync.XMIT_RDEV_MINOR_8_pre30, "c", blk)
	b.WriteByte(5)
	// A FIFO, as rsync sends specials before protocol 31: the last major
	// and a one-byte minor of 0.
	b.entry(rsync.XMIT_TOP_DIR|rsync.XMIT_SAME_RDEV_MAJOR|rsync.XMIT_RDEV_MINOR_8_pre30, "d", fifo)
	b.WriteByte(0)
	// Two hard-linked files: the second shares the first's device.
	b.entry(rsync.XMIT_TOP_DIR|rsync.XMIT_HLINKED, "e", reg)
	b.WriteInt64(42 + 1) // device, 1-incremented
	b.WriteInt64(1000)   // inode
	b.entry(rsync.XMIT_TOP_DIR|rsync.XMIT_HLINKED|rsync.XMIT_SAME_DEV_pre30, "f", reg)
	b.WriteInt64(1000)
	// A file with one link carries no hard-link data.
	b.entry(rsync.XMIT_TOP_DIR, "g", reg)
	b.WriteByte(0)  // end of list
	b.WriteInt32(0) // io_error

	files := byName(receiveList(t, 28, &TransferOpts{PreserveDevices: true, PreserveSpecials: true, PreserveHardlinks: true}, []byte(b.String())))
	if len(files) != 7 {
		t.Fatalf("got %d files, want 7", len(files))
	}
	for name, want := range map[string][2]uint32{"a": {300, 70000}, "c": {300, 5}} {
		if maj, min := splitRdev(files[name].Rdev); maj != want[0] || min != want[1] {
			t.Errorf("%s: device %d,%d, want %d,%d", name, maj, min, want[0], want[1])
		}
	}
	if files["f"].hlinkDev != 42 {
		t.Errorf("f: hard-link device %d, want 42 (carried from e)", files["f"].hlinkDev)
	}
}

// From protocol 28, --checksum sums are sent for regular files only; before
// it, for every entry.
func TestReceiveFileListChecksum(t *testing.T) {
	sum := bytes.Repeat([]byte{0xab}, 16)
	for _, proto := range []int32{27, 28} {
		var b flistBuilder
		b.entry(rsync.XMIT_TOP_DIR, ".", rsync.S_IFDIR|0o755)
		if proto < 28 {
			b.WriteString(string(make([]byte, 16)))
		}
		b.entry(rsync.XMIT_TOP_DIR, "file", rsync.S_IFREG|0o644)
		b.WriteString(string(sum))
		b.WriteByte(0)
		b.WriteInt32(0)
		files := byName(receiveList(t, proto, &TransferOpts{AlwaysChecksum: true}, []byte(b.String())))
		if got := files["file"].Checksum[:]; !bytes.Equal(got, sum) {
			t.Errorf("protocol %d: checksum %x, want %x", proto, got, sum)
		}
	}
}

// From protocol 30 with a safe file list, an I/O error ends the list; no
// io_error int follows the (empty) id lists.
func TestReceiveFileList30IOErrorTrailer(t *testing.T) {
	var b flistBuilder
	b.WriteByte(rsync.XMIT_TOP_DIR | rsync.XMIT_LONG_NAME)
	b.WriteVarint(1)
	b.WriteString("a")
	b.WriteVarlong(5, 3)          // length
	b.WriteVarlong(1700000000, 4) // mtime
	b.WriteInt32(rsync.S_IFREG | 0o644)
	b.WriteByte(rsync.XMIT_EXTENDED_FLAGS)
	b.WriteByte(rsync.XMIT_IO_ERROR_ENDLIST >> 8)
	b.WriteVarint(1)

	opts := &TransferOpts{}
	opts.InfoGTE = func(rsyncopts.InfoLevel, uint16) bool { return false }
	opts.DebugGTE = func(rsyncopts.DebugLevel, uint16) bool { return false }
	rt := &Transfer{
		Logger: log.New(io.Discard),
		Opts:   opts,
		Env:    &rsyncos.Env{},
		Conn: &rsyncwire.Conn{
			Reader:  io.NopCloser(bytes.NewReader([]byte(b.String()))),
			Session: rsyncwire.Session{Protocol: 30, CompatFlags: rsyncwire.CfSafeFlist},
		},
	}
	files, err := rt.ReceiveFileList()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "a" || files[0].Length != 5 {
		t.Errorf("files = %+v", files)
	}
	if rt.IOErrors != 1 || rt.Env.IOErrors.Load() != 1 {
		t.Errorf("IOErrors = %d (env %d), want 1", rt.IOErrors, rt.Env.IOErrors.Load())
	}

	// Without CF_SAFE_FLIST the trailer is a protocol error.
	rt.Conn = &rsyncwire.Conn{
		Reader:  io.NopCloser(bytes.NewReader([]byte(b.String()))),
		Session: rsyncwire.Session{Protocol: 30},
	}
	if _, err := rt.ReceiveFileList(); err == nil {
		t.Errorf("trailer without CF_SAFE_FLIST: no error")
	}
}
