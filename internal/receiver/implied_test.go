package receiver

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

func TestImpliedNames(t *testing.T) {
	for _, tt := range []struct {
		args       []string
		skipModule bool
		allow      []string
		reject     []string
	}{
		{args: []string{"mod/"}, skipModule: true, allow: []string{".", "a", "x/y", ".bashrc"}},
		{args: []string{"mod"}, skipModule: true, allow: []string{".", "anything"}},
		{args: []string{"mod/dir/."}, skipModule: true, allow: []string{"a"}},
		{args: []string{"mod/file.txt"}, skipModule: true, allow: []string{"file.txt"}, reject: []string{".bashrc", "file.txt2", "other/file.txt"}},
		{args: []string{"mod/sub/dir"}, skipModule: true, allow: []string{"dir", "dir/a", "dir/x/y"}, reject: []string{"sub", "dir2", ".ssh/authorized_keys"}},
		{args: []string{"mod/*.txt"}, skipModule: true, allow: []string{"a.txt", "b.txt"}, reject: []string{"a.sh", ".profile"}},
		{args: []string{"mod/a", "mod/b"}, skipModule: true, allow: []string{"a", "b/c"}, reject: []string{"c"}},
		{args: []string{"/home/me/file"}, allow: []string{"file"}, reject: []string{"me", ".bashrc"}},
		{args: []string{"dir/"}, allow: []string{"anything"}},
	} {
		n := NewImpliedNames(tt.args, tt.skipModule)
		for _, name := range tt.allow {
			if !n.Allows(name) {
				t.Errorf("%q: %q rejected, want allowed", tt.args, name)
			}
		}
		for _, name := range tt.reject {
			if n.Allows(name) {
				t.Errorf("%q: %q allowed, want rejected", tt.args, name)
			}
		}
	}
	if !(*ImpliedNames)(nil).Allows(".bashrc") {
		t.Errorf("no ImpliedNames (--trust-sender) rejected a name")
	}
}

// receiveListErr is receiveList that returns the error.
func receiveListErr(proto int32, implied *ImpliedNames, wire []byte) error {
	opts := &TransferOpts{}
	opts.InfoGTE = func(rsyncopts.InfoLevel, uint16) bool { return false }
	opts.DebugGTE = func(rsyncopts.DebugLevel, uint16) bool { return false }
	rt := &Transfer{
		Logger:  log.New(io.Discard),
		Opts:    opts,
		Env:     &rsyncos.Env{},
		Implied: implied,
		Conn: &rsyncwire.Conn{
			Reader:  io.NopCloser(bytes.NewReader(wire)),
			Session: rsyncwire.Session{Protocol: proto},
		},
	}
	_, err := rt.ReceiveFileList()
	return err
}

// A sender's file list is checked as rsync 3.4 checks it.
func TestHostileFileList(t *testing.T) {
	const reg = rsync.S_IFREG | 0o644
	for _, tt := range []struct {
		name    string
		entries func(b *flistBuilder)
		implied *ImpliedNames
		want    string
	}{
		{"dot-dot", func(b *flistBuilder) { b.entry(rsync.XMIT_TOP_DIR, "../escape", reg) }, nil, "unsafe pathname"},
		{"inner dot-dot", func(b *flistBuilder) { b.entry(rsync.XMIT_TOP_DIR, "a/../../b", reg) }, nil, "unsafe pathname"},
		{"absolute", func(b *flistBuilder) { b.entry(rsync.XMIT_TOP_DIR, "/etc/passwd", reg) }, nil, "unsafe pathname"},
		{"unrequested", func(b *flistBuilder) { b.entry(rsync.XMIT_TOP_DIR, ".bashrc", reg) }, NewImpliedNames([]string{"mod/file.txt"}, true), "unrequested file-list name"},
		{"bad mode", func(b *flistBuilder) { b.entry(rsync.XMIT_TOP_DIR, "a", 0o170644) }, nil, "invalid file mode"},
		{"root as file", func(b *flistBuilder) { b.entry(rsync.XMIT_TOP_DIR, ".", reg) }, nil, "non-directory transfer-root"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var b flistBuilder
			tt.entries(&b)
			b.WriteByte(0)  // end of list
			b.WriteInt32(0) // io_error
			err := receiveListErr(28, tt.implied, []byte(b.String()))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ReceiveFileList = %v, want an error about %q", err, tt.want)
			}
		})
	}

	// The requested name itself goes through.
	var b flistBuilder
	b.entry(rsync.XMIT_TOP_DIR, "file.txt", reg)
	b.WriteByte(0)
	b.WriteInt32(0)
	if err := receiveListErr(28, NewImpliedNames([]string{"mod/file.txt"}, true), []byte(b.String())); err != nil {
		t.Errorf("the requested file: %v", err)
	}
}
