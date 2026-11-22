package receiver

import (
	"strings"

	"github.com/gokrazy/rsync/internal/wildmatch"
)

// ImpliedNames is what a receiving client asked for, as rsync's
// add_implied_include (exclude.c) works it out from the source arguments
// (without -R): the last path element of each, a wildcard pattern when it
// has wildcard characters. A server that sends a name outside of them is
// sending what nobody asked for (CVE-2022-29154), which Allows rejects.
// A trailing slash or a final "." asks for everything below.
type ImpliedNames struct {
	all      bool
	patterns []string
}

// NewImpliedNames works out the names args (remote paths, as the client
// sent them to the server) allow. With skipModule, each arg starts with a
// daemon module, which is not part of the names.
func NewImpliedNames(args []string, skipModule bool) *ImpliedNames {
	n := &ImpliedNames{}
	for _, arg := range args {
		if skipModule {
			if _, rest, ok := strings.Cut(arg, "/"); ok {
				arg = rest
			} else {
				arg = ""
			}
		}
		if i := strings.LastIndexByte(arg, '/'); i >= 0 {
			arg = arg[i+1:]
		}
		if arg == "" || arg == "." {
			n.all = true
			continue
		}
		n.patterns = append(n.patterns, arg)
	}
	return n
}

// Allows reports whether name, as the sender named it (forward slashes),
// is below what was asked for.
func (n *ImpliedNames) Allows(name string) bool {
	if n == nil || n.all || name == "." {
		return true
	}
	first, _, _ := strings.Cut(name, "/")
	for _, p := range n.patterns {
		if strings.ContainsAny(p, "*?[") {
			if wildmatch.Match(p, first) {
				return true
			}
		} else if p == first {
			return true
		}
	}
	return false
}

// unsafeName reports a name no sender may send: absolute, or with a ".."
// element (flist.c:recv_file_entry, clean_fname with
// CFN_REFUSE_DOT_DOT_DIRS).
func unsafeName(name string) bool {
	if strings.HasPrefix(name, "/") {
		return true
	}
	for elem := range strings.SplitSeq(name, "/") {
		if elem == ".." {
			return true
		}
	}
	return false
}
