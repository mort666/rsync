package receiver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/rsyncxattr"
)

// Extended attributes (-X) on the receiving side: the generator compares
// the sender's lists with the destination and asks for values the file
// list abbreviated; the receiver reads them with the file and writes the
// lot (xattrs.c). The lists are read-only once the file list is in, as
// generator and receiver share them here where rsync's are two processes.

func (rt *Transfer) xattrs() bool { return rt.Opts.PreserveXattrs > 0 }

func (rt *Transfer) xattrFilter() rsyncxattr.Filter {
	return rsyncxattr.Filter{Level: rt.Opts.PreserveXattrs, Root: os.Geteuid() == 0}
}

// receiveXattrs reads a file-list entry's xattr list (receive_xattr).
func (rt *Transfer) receiveXattrs(f *File) error {
	ndx, err := rt.xattrCache.Receive(rt.Conn.Reader, rt.xattrFilter().KeepReceived)
	if err != nil {
		return fmt.Errorf("%s: %v", f.Name, err)
	}
	f.xattrList = ndx + 1
	return nil
}

func (rt *Transfer) xattrListOf(f *File) rsyncxattr.List {
	return rt.xattrCache.List(f.xattrList - 1)
}

// xattrTarget reports whether f is a type this side reads and writes
// xattrs of: regular files and folders (see sender.readXattrs).
func xattrTarget(f *File) bool {
	mode := f.Mode & rsync.S_IFMT
	return mode == rsync.S_IFREG || mode == rsync.S_IFDIR
}

// localXattrs reads the destination's own xattrs of f, if it is there.
func (rt *Transfer) localXattrs(name string) (map[string][]byte, error) {
	in, err := rt.DestRoot.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer in.Close()
	return rt.readXattrsOf(in)
}

func (rt *Transfer) readXattrsOf(in *os.File) (map[string][]byte, error) {
	filter := rt.xattrFilter()
	return rsyncxattr.Read(in, func(name string) bool { return filter.KeepLocal(name, false) })
}

// xattrState is what the generator found for the file it is on.
type xattrState struct {
	list    int // the file's list index, or -1
	differs bool
	todo    []int // abbreviated values to ask the sender for
	sent    bool  // the request went out with the file's attributes
}

// checkXattrs compares f's list with the destination (xattr_diff with
// find_all), before the generator changes anything there.
func (rt *Transfer) checkXattrs(f *File) xattrState {
	if !rt.xattrs() || rt.Opts.DryRun {
		return xattrState{list: -1}
	}
	st := xattrState{list: f.xattrList - 1}
	if !xattrTarget(f) {
		return st
	}
	local, err := rt.localXattrs(f.Name)
	if err != nil {
		rt.Logger.Printf("reading xattrs of %s: %v", f.Name, err)
	}
	differs, todo := rsyncxattr.Diff(rt.xattrListOf(f), local)
	st.differs = differs
	st.todo = rt.xattrGen.Todo(st.list, todo)
	return st
}

// addXattrRequest marks a, the attributes the generator sends for the file
// it is on, with ITEM_REPORT_XATTR when the xattrs differ, and returns the
// request that follows them (generator.c:itemize, send_xattr_request).
func (rt *Transfer) addXattrRequest(a *rsyncwire.ItemAttrs) []byte {
	// Whatever the item, the list's values are dealt with now: asked for,
	// or the receiver has them and keeps them for later files.
	if rt.genXattr.list >= 0 {
		defer rt.xattrGen.Done(rt.genXattr.list)
	}
	if !rt.genXattr.differs {
		return nil
	}
	a.Iflags |= rsync.ITEM_REPORT_XATTR
	rt.genXattr.sent = true
	s := rt.Conn.Session
	if !rsyncxattr.RequestFollows(a.Iflags, s.Protocol, s.CompatFlags&rsyncwire.CfAvoidXattrOptim != 0) {
		return nil
	}
	return rsyncxattr.AppendRequest(nil, rt.genXattr.todo)
}

// reportXattrs sends the item for a file the generator does not ask the
// sender to transfer but whose xattrs differ: the receiver sets them once
// the sender has answered.
func (rt *Transfer) reportXattrs(idx int) error {
	if !rt.genXattr.differs || rt.genXattr.sent {
		return nil
	}
	a := rsyncwire.ItemAttrs{BasisType: rsync.FNAMECMP_FNAME}
	req := rt.addXattrRequest(&a)
	if err := rt.Conn.WriteNdxAndAttrs(int32(idx), a); err != nil {
		return err
	}
	_, err := rt.Conn.Writer.Write(req)
	return err
}

// readXattrValues reads the values the sender sends with a file's
// attributes, if any (receiver.c:recv_files, recv_xattr_request).
func (rt *Transfer) readXattrValues(f *File, attrs rsyncwire.ItemAttrs) (map[int][]byte, error) {
	s := rt.Conn.Session
	if !rt.xattrs() || rt.Opts.DryRun || !rsyncxattr.RequestFollows(attrs.Iflags, s.Protocol, s.CompatFlags&rsyncwire.CfAvoidXattrOptim != 0) {
		return nil, nil
	}
	values, err := rsyncxattr.ReadValues(rt.Conn.Reader)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", f.Name, err)
	}
	return values, nil
}

// setXattrs gives target f's xattrs (set_xattr, rsync_xal_set): values
// from the list, from fetched, or from basis when the sum matches. basis
// may be target itself, or nil. It reports failures as rsync does, without
// stopping the transfer.
func (rt *Transfer) setXattrs(target *os.File, f *File, fetched map[int][]byte, basis *os.File) {
	if !rt.xattrs() || rt.Opts.DryRun || !xattrTarget(f) {
		return
	}
	current, err := rt.readXattrsOf(target)
	if err != nil {
		rt.xattrFailed(f, err)
		return
	}
	basisValues := current
	if basis != nil && basis != target {
		if basisValues, err = rt.readXattrsOf(basis); err != nil {
			rt.xattrFailed(f, err)
			return
		}
	}
	values, err := rt.xattrRecv.Values(rt.xattrListOf(f), f.xattrList-1, fetched, basisValues)
	if err != nil {
		rt.xattrFailed(f, err) // and set what it has
	}
	filter := rt.xattrFilter()
	if err := rsyncxattr.Set(target, values, current, func(name string) bool { return filter.KeepLocal(name, false) }); err != nil {
		rt.xattrFailed(f, err)
	}
}

// setXattrsInPlace sets f's xattrs on the destination as it is, for a file
// the sender did not transfer.
func (rt *Transfer) setXattrsInPlace(f *File, fetched map[int][]byte) {
	if !rt.xattrs() || rt.Opts.DryRun || !xattrTarget(f) {
		return
	}
	target, err := rt.DestRoot.Open(f.Name)
	if err != nil {
		rt.xattrFailed(f, err)
		return
	}
	defer target.Close()
	rt.setXattrs(target, f, fetched, target)
}

func (rt *Transfer) xattrFailed(f *File, err error) {
	msg := fmt.Sprintf("rsync: set_xattr: %s: %v\n", f.Name, err)
	rt.Logger.Printf("%s", msg)
	if rt.TellClient != nil {
		rt.TellClient(rsyncwire.MsgErrorXfer, msg)
		return
	}
	if rt.Env != nil {
		rt.Env.XferErrors.Add(1)
	}
}
