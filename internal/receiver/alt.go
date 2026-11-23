package receiver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncxattr"
)

// --compare-dest, --copy-dest and --link-dest (generator.c:try_dests_reg,
// try_dests_non). For a file the destination lacks, or with --link-dest
// and --compare-dest one it has, the generator looks for it in each
// alternate folder in turn. One that matches exactly is hard-linked
// (--link-dest), left out (--compare-dest) or copied (--copy-dest); one
// with the same content but other attributes is copied; one with other
// content is the basis the sender's delta goes against.

// basisMatch is how well a file in an alternate folder matches
// (generator.c:regular_basis_match).
type basisMatch int

const (
	basisNotFound basisMatch = iota
	basisContentDiffers
	basisAttrsDiffer
	basisMatches
)

// openBasisDirs opens the alternate folders, relative ones from the
// destination. One that is not there matches nothing, as in rsync, which
// warns about it.
func (rt *Transfer) openBasisDirs() {
	open := rt.OpenBasisDir
	if open == nil {
		open = func(dir string) (*os.Root, error) {
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(rt.Dest, dir)
			}
			return os.OpenRoot(dir)
		}
	}
	for _, dir := range rt.Opts.BasisDirs {
		root, err := open(dir)
		if err != nil {
			rt.Logger.Printf("%s arg does not exist: %s (%v)", rsyncopts.AltDestOption(rt.Opts.AltDest), dir, err)
		}
		rt.basisRoots = append(rt.basisRoots, root) // nil keeps the numbering
	}
}

func (rt *Transfer) closeBasisDirs() {
	for _, root := range rt.basisRoots {
		if root != nil {
			root.Close()
		}
	}
}

// matchRegular is match_regular_basis: how well the file at f's name in
// root matches f.
func (rt *Transfer) matchRegular(root *os.Root, f *File) (basisMatch, fs.FileInfo) {
	st, err := root.Lstat(f.Name)
	if err != nil || !st.Mode().IsRegular() {
		return basisNotFound, nil
	}
	if st.Size() != f.Length {
		return basisContentDiffers, st
	}
	switch {
	case rt.genRedoing:
		return basisContentDiffers, st
	case rt.Opts.AlwaysChecksum && !rt.Opts.SizeOnly:
		sum, err := rt.strong().RootFileChecksum(root, f.Name)
		if err != nil || !bytes.Equal(sum, f.Checksum) {
			return basisContentDiffers, st
		}
	case rt.Opts.SizeOnly:
	case rt.Opts.IgnoreTimes || !rt.modTimeEqual(st.ModTime(), f.ModTime):
		return basisContentDiffers, st
	}
	if !rt.unchangedAttrs(root, f, st) {
		return basisAttrsDiffer, st
	}
	return basisMatches, st
}

// unchangedAttrs is generator.c:unchanged_attrs: whether st has the
// attributes this receiver would give f.
func (rt *Transfer) unchangedAttrs(root *os.Root, f *File, st fs.FileInfo) bool {
	isLink := f.Mode&rsync.S_IFMT == rsync.S_IFLNK
	if rt.Opts.PreserveTimes && !rt.modTimeEqual(st.ModTime(), f.ModTime) {
		return false
	}
	if !isLink && rt.Opts.PreservePerms && goModeBits(st.Mode()) != uint32(f.Mode)&0o7777 {
		return false
	}
	if rt.ownerDiffers(f, st) {
		return false
	}
	if rt.xattrs() && !isLink && (st.Mode().IsRegular() || st.IsDir()) {
		in, err := root.Open(f.Name)
		if err != nil {
			return false
		}
		defer in.Close()
		local, err := rt.readXattrsOf(in)
		if err != nil {
			return false
		}
		if differs, _ := rsyncxattr.Diff(rt.xattrListOf(f), local); differs {
			return false
		}
	}
	return true
}

// goModeBits is a Go mode's permission bits as rsync sends them.
func goModeBits(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// tryDestsReg is try_dests_reg for the regular file f. destSt is the
// destination's file, or nil. It reports whether f is dealt with, or else
// the alternate folder to use as the delta basis, or -1.
func (rt *Transfer) tryDestsReg(f *File, destSt fs.FileInfo, perm fs.FileMode) (done bool, basis int, err error) {
	best, level := -1, basisNotFound
	var bestSt fs.FileInfo
	for j, root := range rt.basisRoots {
		if root == nil {
			continue
		}
		m, st := rt.matchRegular(root, f)
		if m > level {
			best, level, bestSt = j, m, st
		}
		if m == basisMatches {
			break
		}
	}
	if level == basisNotFound {
		return false, -1, nil
	}
	root := rt.basisRoots[best]
	kind := rt.Opts.AltDest

	if level == basisMatches && kind != rsyncopts.CopyDest {
		if destSt != nil {
			if kind == rsyncopts.LinkDest && os.SameFile(destSt, bestSt) {
				return false, -1, nil // already linked: the usual check
			}
			if !rt.Opts.DryRun {
				if err := rt.DestRoot.Remove(f.Name); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return false, -1, nil
				}
			}
		}
		if kind == rsyncopts.CompareDest || rt.Opts.DryRun {
			return true, -1, nil
		}
		if err := linkBetween(root, rt.DestRoot, f.Name); err == nil {
			return true, -1, nil
		}
		// Another file system, or no hard links here: copy instead
		// (try_a_copy).
	} else if destSt != nil {
		return false, -1, nil
	}

	if level >= basisAttrsDiffer {
		if rt.Opts.DryRun {
			return true, -1, nil
		}
		if err := rt.copyFromBasis(root, f, perm); err != nil {
			rt.Logger.Printf("copying %s from %s failed: %v", f.Name, rt.Opts.BasisDirs[best], err)
			return false, -1, nil
		}
		return true, -1, nil
	}
	if rt.Opts.Inplace {
		// In place, the destination is the basis.
		return false, -1, nil
	}
	return false, best, nil
}

// copyFromBasis copies f from root into place and gives it f's
// attributes (generator.c:copy_altdest_file).
func (rt *Transfer) copyFromBasis(root *os.Root, f *File, perm fs.FileMode) error {
	in, err := root.Open(f.Name)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := newPendingFile(rt.DestRoot, f.Name, outputMode{
		Inplace: rt.Opts.Inplace,
		Fsync:   rt.Opts.DoFsync,
		Sparse:  rt.Opts.SparseFiles,
	})
	if err != nil {
		return err
	}
	defer out.Cleanup()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.CloseAtomicallyReplace(); err != nil {
		return err
	}
	return rt.setPermsAccurate(f, perm)
}

// tryDestsNon is try_dests_non for a symlink, device or special f the
// destination lacks. It reports whether f is dealt with: linked from an
// alternate folder, or left out with --compare-dest. Otherwise the
// caller creates it, as rsync does with --copy-dest.
func (rt *Transfer) tryDestsNon(f *File) bool {
	for _, root := range rt.basisRoots {
		if root == nil {
			continue
		}
		st, err := root.Lstat(f.Name)
		if err != nil || !sameType(f, st) || !rt.sameNonRegular(root, f, st) || !rt.unchangedAttrs(root, f, st) {
			continue
		}
		switch rt.Opts.AltDest {
		case rsyncopts.CompareDest:
			return true
		case rsyncopts.LinkDest:
			if rt.Opts.DryRun {
				return true
			}
			// A link that cannot be made falls back to creating the
			// entry, as rsync does.
			return linkBetween(root, rt.DestRoot, f.Name) == nil
		}
		return false
	}
	return false
}

func sameType(f *File, st fs.FileInfo) bool {
	m := st.Mode()
	switch f.Mode & rsync.S_IFMT {
	case rsync.S_IFLNK:
		return m&fs.ModeSymlink != 0
	case rsync.S_IFCHR:
		return m&fs.ModeCharDevice != 0
	case rsync.S_IFBLK:
		return m&fs.ModeDevice != 0 && m&fs.ModeCharDevice == 0
	case rsync.S_IFIFO:
		return m&fs.ModeNamedPipe != 0
	case rsync.S_IFSOCK:
		return m&fs.ModeSocket != 0
	}
	return false
}

// sameNonRegular is quick_check_ok for a symlink, device or special.
func (rt *Transfer) sameNonRegular(root *os.Root, f *File, st fs.FileInfo) bool {
	switch f.Mode & rsync.S_IFMT {
	case rsync.S_IFLNK:
		target, err := root.Readlink(f.Name)
		return err == nil && target == f.LinkTarget
	case rsync.S_IFCHR, rsync.S_IFBLK:
		major, minor := splitRdev(f.Rdev)
		amajor, aminor := rdevOf(st)
		return major == amajor && minor == aminor
	}
	return true
}

// linkBetween hard-links name in from to the same name in to, each
// reached through its own root so that neither path can leave it.
func linkBetween(from, to *os.Root, name string) error {
	if from == nil {
		return fmt.Errorf("no alternate folder")
	}
	return linkAcross(from, to, name)
}
