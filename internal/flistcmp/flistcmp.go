// Package flistcmp orders file-list entries as rsync does. Both sides sort
// the file list and then refer to entries by index, so the order has to
// match the peer's exactly, or the wrong file's data is sent.
package flistcmp

import "strings"

// Entry is what the order depends on: the path as sent in the file list
// and whether it is a folder.
type Entry struct {
	Name  string
	IsDir bool
}

type fncState int

const (
	sDir fncState = iota
	sSlash
	sBase
	sTrailing
)

type fncType int

const (
	tPath fncType = iota
	tItem
)

// cursor walks one name the way f_name_cmp walks its dirname, an implied
// "/", the basename and, for a folder from protocol 29, a trailing "/".
type cursor struct {
	s     string // what is being walked now
	state fncState
	typ   fncType
}

func (c *cursor) empty() bool { return c.s == "" }

// Compare is rsync's flist.c:f_name_cmp. It returns a negative number when a
// sorts before b, 0 when they are equal, positive otherwise.
//
// Below protocol 29 this is the byte order of the full paths. From 29, a
// folder compares as if its name ended in "/", so it sorts right before its
// contents; a folder sorts after every non-folder at the same depth; and "."
// sorts before everything.
func Compare(proto int32, a, b Entry) int {
	tPathFor := tItem
	if proto >= 29 {
		tPathFor = tPath
	}
	dirA, baseA := split(a.Name)
	dirB, baseB := split(b.Name)

	start := func(e Entry, dir, base string, hasDir bool) cursor {
		if hasDir {
			return cursor{s: dir, state: sDir, typ: tPathFor}
		}
		return baseCursor(e, base, tPathFor)
	}
	// rsync shares one dirname string between the entries of a folder and
	// treats equal pointers as "no dirname". Equal strings come to the same
	// result.
	hasDirA, hasDirB := dirA != "", dirB != ""
	if hasDirA && hasDirB && dirA == dirB {
		hasDirA, hasDirB = false, false
	}
	c1 := start(a, dirA, baseA, hasDirA)
	c2 := start(b, dirB, baseB, hasDirB)

	if c1.typ != c2.typ {
		if c1.typ == tPath {
			return 1
		}
		return -1
	}

	for {
		if c1.empty() {
			switch c1.state {
			case sDir:
				c1.state = sSlash
				c1.s = "/"
			case sSlash:
				c1 = baseCursor(a, baseA, tPathFor)
			case sBase:
				c1.state = sTrailing
				if c1.typ == tPath {
					c1.s = "/"
					break
				}
				c1.typ = tItem
			case sTrailing:
				c1.typ = tItem
			}
			if !c2.empty() && c1.typ != c2.typ {
				if c1.typ == tPath {
					return 1
				}
				return -1
			}
		}
		if c2.empty() {
			switch c2.state {
			case sDir:
				c2.state = sSlash
				c2.s = "/"
			case sSlash:
				c2 = baseCursor(b, baseB, tPathFor)
			case sBase:
				c2.state = sTrailing
				if c2.typ == tPath {
					c2.s = "/"
					break
				}
				if c1.empty() {
					return 0
				}
				c2.typ = tItem
			case sTrailing:
				if c1.empty() {
					return 0
				}
				c2.typ = tItem
			}
			if c1.typ != c2.typ {
				if c1.typ == tPath {
					return 1
				}
				return -1
			}
		}
		// Compare one byte each; an exhausted name reads as NUL, as in C.
		var ch1, ch2 int
		if !c1.empty() {
			ch1 = int(c1.s[0])
			c1.s = c1.s[1:]
		}
		if !c2.empty() {
			ch2 = int(c2.s[0])
			c2.s = c2.s[1:]
		}
		if d := ch1 - ch2; d != 0 {
			return d
		}
		if ch1 == 0 {
			// Both ran out at once without the states above deciding:
			// rsync would read past the strings here, which its state
			// machine never lets happen for well-formed entries.
			return 0
		}
	}
}

func baseCursor(e Entry, base string, tPathFor fncType) cursor {
	typ := tItem
	if e.IsDir {
		typ = tPathFor
	}
	if typ == tPath && base == "." {
		return cursor{s: "", state: sTrailing, typ: tItem}
	}
	return cursor{s: base, state: sBase, typ: typ}
}

func split(name string) (dir, base string) {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}
