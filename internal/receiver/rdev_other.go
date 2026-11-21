//go:build !linux && !darwin

package receiver

// makeRdev turns the major and minor device numbers protocol 28 sends into
// a dev_t. Device files are not created on this system, so the encoding
// only has to be stable.
func makeRdev(major, minor uint32) int32 { return int32(major<<8 | minor&0xff | (minor&^0xff)<<12) }

func splitRdev(rdev int32) (major, minor uint32) {
	r := uint32(rdev)
	return r >> 8 & 0xfff, r&0xff | r>>12&^0xff
}
