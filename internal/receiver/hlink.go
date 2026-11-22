package receiver

import (
	"fmt"
	"os"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// Hard links (-H) on the receiving side (hlink.c). The first member of a
// group, in the sorted list, is checked and transferred as any file; the
// generator leaves the others alone, and once the transfer is done they
// are linked to it. rsync links each one as soon as the first is in
// place, which needs a message from its receiver process back to its
// generator; here generator and receiver are goroutines over one list, and
// linking at the end leaves the same files.

// matchHardLinks maps each later member of a group to the group's first
// (hlink.c:match_hard_links).
func matchHardLinks(fileList []*File) map[*File]*File {
	first := make(map[int64]*File)
	later := make(map[*File]*File)
	for _, f := range fileList {
		if !f.hlinked {
			continue
		}
		if lead, ok := first[f.hlinkGroup]; ok {
			later[f] = lead
		} else {
			first[f.hlinkGroup] = f
		}
	}
	return later
}

// linkHardLinks links each later member of a group to its first, unless
// it is already (hlink.c:hard_link_one, finish_hard_link).
func (rt *Transfer) linkHardLinks(fileList []*File) {
	if rt.Opts.DryRun {
		return
	}
	for _, f := range fileList {
		lead, ok := rt.hlinkLater[f]
		if !ok {
			continue
		}
		lst, err := rt.DestRoot.Lstat(lead.Name)
		if err != nil {
			rt.hlinkFailed(f, lead, err)
			continue
		}
		if st, err := rt.DestRoot.Lstat(f.Name); err == nil {
			if os.SameFile(st, lst) {
				continue
			}
			if st.IsDir() {
				rt.hlinkFailed(f, lead, fmt.Errorf("a folder is in the way"))
				continue
			}
			if err := rt.DestRoot.Remove(f.Name); err != nil {
				rt.hlinkFailed(f, lead, err)
				continue
			}
		}
		if err := rt.DestRoot.Link(lead.Name, f.Name); err != nil {
			rt.hlinkFailed(f, lead, err)
			continue
		}
		if rt.Opts.Verbose {
			rt.info(fmt.Sprintf("%s => %s\n", f.Name, lead.Name))
		}
	}
}

func (rt *Transfer) hlinkFailed(f, lead *File, err error) {
	msg := fmt.Sprintf("rsync: link %s => %s failed: %v\n", f.Name, lead.Name, err)
	rt.Logger.Printf("%s", msg)
	if rt.TellClient != nil {
		rt.TellClient(rsyncwire.MsgErrorXfer, msg)
		return
	}
	if rt.Env != nil {
		rt.Env.XferErrors.Add(1)
	}
}
