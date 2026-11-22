// Package safelink decides whether a symlink stays inside the transfer, as
// rsync's --safe-links and --copy-unsafe-links do.
package safelink

import "strings"

// Unsafe reports whether a symlink at src, named from the top of the
// transfer, with target dest, may lead outside it: an absolute or empty
// target, one with ".." anywhere but at its start or ending in "/..", or
// one that climbs above the transfer's top.
//
// rsync/util1.c:unsafe_symlink
func Unsafe(dest, src string) bool {
	if dest == "" || dest[0] == '/' {
		return true
	}
	// Leading ".." components may be followed by more slashes.
	rest := dest
	for strings.HasPrefix(rest, "../") {
		rest = strings.TrimLeft(rest[3:], "/")
	}
	if strings.Contains(rest, "/../") {
		return true
	}
	if len(dest) > 3 && strings.HasSuffix(dest, "/..") {
		return true
	}

	// How deep the link sits: its folders, a ".." starting the count over.
	depth := 0
	comps := strings.Split(src, "/")
	for _, c := range comps[:len(comps)-1] {
		switch c {
		case "", ".":
		case "..":
			depth = 0
		default:
			depth++
		}
	}
	if comps[len(comps)-1] == ".." {
		depth = 0
	}

	comps = strings.Split(dest, "/")
	for _, c := range comps[:len(comps)-1] {
		switch c {
		case "", ".":
		case "..":
			if depth--; depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	if comps[len(comps)-1] == ".." {
		depth--
	}
	return depth < 0
}
