package rsyncwire

import (
	"fmt"
	"strings"
)

// maxNstrLen is rsync's MAX_NSTR_STRLEN: the longest negotiation list.
const maxNstrLen = 256

// PickName is compat.c:parse_negotiate_str. From the peer's
// space-separated list it picks the name that ranks highest in ours,
// whatever the peer's own order. ok is false when no name is in both.
func PickName(ours []string, peerList string) (name string, ok bool) {
	best := len(ours)
	for _, tok := range strings.Fields(peerList) {
		for rank, n := range ours {
			if n == tok && rank < best {
				best, name = rank, n
			}
		}
	}
	return name, best < len(ours)
}

// OurList is the list a side offers: env (rsync's RSYNC_CHECKSUM_LIST or
// RSYNC_COMPRESS_LIST) narrowed to supported names in env's order, or
// defaults when env is empty or names none of them. defaults may leave out
// names a side offers only when asked to. A server uses what
// follows a '&' in env, as rsync does with "client&server".
func OurList(env string, isServer bool, supported, defaults []string) []string {
	if isServer {
		if _, after, ok := strings.Cut(env, "&"); ok {
			env = after
		}
	} else if before, _, ok := strings.Cut(env, "&"); ok {
		env = before
	}
	var list []string
	for _, tok := range strings.Fields(env) {
		for _, n := range supported {
			if n == tok {
				list = append(list, n)
			}
		}
	}
	if len(list) == 0 {
		return defaults
	}
	return list
}

// NegotiateName is one of compat.c:negotiate_the_strings's exchanges: each
// side sends its list and reads the other's, then picks with PickName.
//
// rsync sends before it reads on both sides, which relies on the
// connection buffering a list. An io.Pipe does not (tests, and callers of
// rsyncclient.Run, use one), so here only the client sends first; a server
// reads the client's list before sending its own. Either way round works
// with rsync.
func (c *Conn) NegotiateName(what string, ours []string, isServer bool) (string, error) {
	var theirs string
	var err error
	if isServer {
		if theirs, err = c.ReadVstring(maxNstrLen); err != nil {
			return "", err
		}
	}
	if err := c.WriteVstring(strings.Join(ours, " ")); err != nil {
		return "", err
	}
	if !isServer {
		if theirs, err = c.ReadVstring(maxNstrLen); err != nil {
			return "", err
		}
	}
	name, ok := PickName(ours, theirs)
	if !ok {
		return "", fmt.Errorf("failed to negotiate a %s choice: peer offered %q, we offered %q", what, theirs, strings.Join(ours, " "))
	}
	return name, nil
}
