package rsync

import "time"

type FileInfo struct {
	Name    string
	Length  int64
	ModTime time.Time
	Mode    int32
	// Checksum is the file's --checksum sum as the sender sent it, as long
	// as the session's file checksum is (8 to 64 bytes), or nil without
	// --checksum.
	Checksum []byte
	// LinkTarget is a symbolic link's target, the text the sender's link
	// holds. The sender sends it only with -l (--links); it is empty
	// without -l, and for anything that is no link.
	LinkTarget string
}
