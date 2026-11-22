//go:build !unix

package receiver

import "time"

// lchtimes leaves a symlink's times alone where Go cannot set them.
func (rt *Transfer) lchtimes(string, time.Time) error { return nil }
