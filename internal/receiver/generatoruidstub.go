//go:build !linux && !darwin

package receiver

import "io/fs"

func (rt *Transfer) setUid(_ *File, st fs.FileInfo) (fs.FileInfo, error) {
	return st, nil
}

func (rt *Transfer) ownerDiffers(*File, fs.FileInfo) bool { return false }

func (rt *Transfer) ownerFlags(*File, fs.FileInfo) uint32 { return 0 }

// amRoot is whether ids can be set: with --super only, here.
func (rt *Transfer) amRoot() bool { return rt.Opts.Super }
