package sender

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/safelink"
)

// -L, -k and --copy-unsafe-links: a symlink goes in the file list as what
// it points to (flist.c:readlink_stat, link_stat). A link to a folder is
// walked as its own root, its entries named below the link's name, as
// fs.WalkDir does not follow links.

// maxLinkDepth is how many followed folder links may nest, as the kernel
// stops resolving a path after 40 links (ELOOP), which is where rsync,
// which does not look for loops, stops.
const maxLinkDepth = 40

// followLink decides what becomes of the symlink at path, named name in
// the file list. It returns the target's info for a link to anything but
// a folder that is to be followed, info itself for a link to keep, or
// nil when the link has been dealt with: walked as a folder, or left out
// with an error.
func (s *scopedWalker) followLink(path, name string, info fs.FileInfo) (fs.FileInfo, error) {
	opts := s.st.Opts
	follow := opts.CopyLinks()
	if !follow && opts.CopyUnsafeLinks() {
		target, err := s.source.Readlink(path)
		if err != nil {
			return nil, err
		}
		if follow = safelink.Unsafe(target, name); follow && opts.InfoGTE(rsyncopts.INFO_SYMSAFE, 1) {
			s.st.Logger.Printf("copying unsafe symlink \"%s\" -> \"%s\"", name, target)
		}
	}
	if !follow && !opts.CopyDirlinks() {
		return info, nil
	}

	st, src, err := s.statLink(path)
	if err != nil {
		if !follow { // -k leaves a link to nothing as a link
			return info, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			msg := fmt.Sprintf("symlink has no referent: %s", name)
			s.st.tellClient(rsyncwire.MsgErrorXfer, msg+"\n")
			err = errors.New(msg)
		}
		s.ioError(err)
		return nil, nil
	}
	if !st.IsDir() {
		if !follow {
			return info, nil
		}
		if src == nil {
			return st, nil // read through the link, as the source resolves it
		}
	}
	if s.linkDepth >= maxLinkDepth || s.isAncestor(st) {
		s.st.tellClient(rsyncwire.MsgErrorXfer, fmt.Sprintf("rsync: [sender] opendir \"%s\" failed: too many levels of symbolic links\n", name))
		s.ioError(fmt.Errorf("%s: too many levels of symbolic links", name))
		return nil, nil
	}

	child := *s
	child.parent = s
	child.linkDepth = s.linkDepth + 1
	child.strip, child.subdir, child.requested = "", ".", "."
	child.prefix = name
	switch {
	case src != nil:
		child.source = src
		child.localDir = src.(interface{ dir() string }).dir()
		s.fileList.Sources = append(s.fileList.Sources, src)
	default:
		sub, err := newSubSource(s.source, path)
		if err != nil {
			s.ioError(err)
			return nil, nil
		}
		child.source = sub
		child.localDir = filepath.Join(s.localDir, s.subdir, path)
	}
	if !st.IsDir() {
		return nil, child.walkFn(".", fs.FileInfoToDirEntry(st), nil)
	}
	return nil, fs.WalkDir(child.source.FS(), ".", child.walkFn)
}

// statLink stats what the link at path points to. Within the source it
// goes through the source, which keeps it there; a client also follows a
// link that leaves it, and gets back a source for the target.
func (s *scopedWalker) statLink(p string) (fs.FileInfo, FileSource, error) {
	st, err := fs.Stat(s.source.FS(), p)
	if err == nil || errors.Is(err, fs.ErrNotExist) || s.st.Confined || s.localDir == "" {
		return st, nil, err
	}
	// Outside the source: os.Root refuses to go there.
	real, err := filepath.EvalSymlinks(filepath.Join(s.localDir, s.subdir, filepath.FromSlash(p)))
	if err != nil {
		return nil, nil, err
	}
	st, err = os.Stat(real)
	if err != nil {
		return nil, nil, err
	}
	if !st.IsDir() {
		return st, &outsideFile{singleFileSource: newSingleFileSource(real, st)}, nil
	}
	root, err := os.OpenRoot(real)
	if err != nil {
		return nil, nil, err
	}
	return st, &outsideDir{osRootSource: osRootSource{root: root}, path: real}, nil
}

// outsideFile and outsideDir are a client's sources for what a followed
// link leads to outside the source folder.
type outsideFile struct{ *singleFileSource }

func (o *outsideFile) dir() string { return filepath.Dir(o.path) }

type outsideDir struct {
	osRootSource
	path string
}

func (o *outsideDir) dir() string { return o.path }

// isAncestor reports whether the folder st is one this walk is inside,
// which a link to it would loop back into.
func (s *scopedWalker) isAncestor(st fs.FileInfo) bool {
	dev, ino, _, ok := hardLinkID(st)
	if !ok {
		return false // the depth limit stops the loop
	}
	// Every folder from each walker's root down to the link in it: a
	// parent walker is still on the link that started its child.
	for w := s; w != nil; w = w.parent {
		for d := path.Dir(w.current); ; d = path.Dir(d) {
			info, err := fs.Stat(w.source.FS(), d)
			if err == nil {
				if adev, aino, _, ok := hardLinkID(info); ok && adev == dev && aino == ino {
					return true
				}
			}
			if d == "." || d == "/" {
				break
			}
		}
	}
	return false
}

// mungedTarget takes the --munge-links prefix off a target being sent.
func mungedTarget(opts *rsyncopts.Options, target string) string {
	if opts.MungeLinks() && len(target) > len(rsyncopts.SymlinkPrefix) {
		return strings.TrimPrefix(target, rsyncopts.SymlinkPrefix)
	}
	return target
}
