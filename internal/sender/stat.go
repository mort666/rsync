//go:build !linux && !darwin

package sender

import "io/fs"

func uidFromFileInfo(fs.FileInfo) (int32, bool) {
	return 0, false
}

func gidFromFileInfo(fs.FileInfo) (int32, bool) {
	return 0, false
}

func rdevFromFileInfo(fs.FileInfo) (int32, bool) {
	return 0, false
}

func rdevMajorMinor(fs.FileInfo) (major, minor uint32) {
	return 0, 0
}

func hardLinkID(fs.FileInfo) (dev, ino int64, nlink uint64, ok bool) {
	return 0, 0, 0, false
}
