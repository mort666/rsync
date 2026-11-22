//go:build !linux

package receiver

import (
	"errors"
	"io/fs"
)

// --fake-super needs xattrs, which gokrazy/rsync has on Linux only; the
// options refuse it elsewhere.

var errNoFakeSuper = errors.New("--fake-super is not supported on this platform")

func (rt *Transfer) lstat(name string) (fs.FileInfo, error) { return rt.DestRoot.Lstat(name) }

func (rt *Transfer) readlink(name string) (string, error) { return rt.DestRoot.Readlink(name) }

func (rt *Transfer) placeholder(string, string) error { return errNoFakeSuper }

func (rt *Transfer) fakeDevice(*File, fs.FileInfo) error { return errNoFakeSuper }

func (rt *Transfer) setFakeStat(*File, fs.FileMode, fs.FileInfo) error { return errNoFakeSuper }
