package sender

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// onlySet is what a sender limited to a list of paths sends: the listed
// paths and every folder above one, as rsync's --files-from. A listed
// folder goes as a folder; what it holds goes only when that is listed too.
type onlySet struct {
	listed  map[string]bool // by file list name; true once the walk saw it
	parents map[string]bool
	recurse bool // a listed folder goes with all it holds (-r)
}

// newOnlySet returns nil for a nil list: everything goes.
func newOnlySet(paths []string, recurse bool) *onlySet {
	if paths == nil {
		return nil
	}
	o := &onlySet{listed: map[string]bool{}, parents: map[string]bool{}, recurse: recurse}
	for _, p := range paths {
		p = path.Clean(p)
		o.listed[p] = false
		// A folder already there has its own folders above it there too.
		for d := path.Dir(p); d != "." && d != "/" && !o.parents[d]; d = path.Dir(d) {
			o.parents[d] = true
		}
	}
	return o
}

// allows says whether the entry with file list name name goes, and, when it
// does not, whether it is a folder the walk need not go into.
func (o *onlySet) allows(name string, dir bool) (send, prune bool) {
	if o == nil || name == "." {
		return true, false
	}
	if _, ok := o.listed[name]; ok {
		o.listed[name] = true
		return true, false
	}
	if o.recurse {
		for d := path.Dir(name); d != "." && d != "/"; d = path.Dir(d) {
			if _, ok := o.listed[d]; ok {
				return true, false
			}
		}
	}
	if dir {
		if o.parents[name] {
			return true, false
		}
		return false, true
	}
	return false, false
}

// leadsTo says whether the walk must go into folder name, without -r, to
// reach a listed path below it.
func (o *onlySet) leadsTo(name string) bool {
	return o != nil && o.parents[name]
}

// missing are the listed paths the walk did not come to, sorted.
func (o *onlySet) missing() []string {
	if o == nil {
		return nil
	}
	var out []string
	for name, seen := range o.listed {
		if !seen {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// FilesFromNames turns --files-from lines into the names a sender sends:
// comment lines left out unless keepComments (names read from the
// connection keep them, as rsync's read_line without RL_DUMP_COMMENTS),
// and each name kept inside the source folder (flist.c:send_file_list,
// sanitize_path), "/a" and "../a" both as "a".
func FilesFromNames(lines []string, keepComments bool) []string {
	names := []string{}
	for _, l := range lines {
		if l == "" || !keepComments && (l[0] == '#' || l[0] == ';') {
			continue
		}
		n := strings.TrimPrefix(path.Clean("/"+l), "/")
		if n == "" {
			n = "."
		}
		names = append(names, n)
	}
	return names
}

// ReadFilesFromConn reads the --files-from names a receiving client sends
// a server sender: each NUL-ended, an empty one last (io.c:read_line with
// RL_EOL_NULLS). It reads a byte at a time, so nothing after the names is
// taken.
func ReadFilesFromConn(r io.Reader) ([]string, error) {
	var names []string
	var name []byte
	var b [1]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("reading --files-from names: %w", err)
		}
		if b[0] != 0 {
			if len(name) >= 4096 {
				return nil, fmt.Errorf("reading --files-from names: a name is too long")
			}
			name = append(name, b[0])
			continue
		}
		if len(name) == 0 {
			return names, nil
		}
		names = append(names, string(name))
		name = name[:0]
	}
}
