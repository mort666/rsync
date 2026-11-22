//go:build !unix

package receiver

import (
	"errors"
	"io/fs"
	"os"
)

// linkAcross is not done here: a match is copied instead, as rsync does
// when it cannot link.
func linkAcross(_, _ *os.Root, _ string) error { return errors.ErrUnsupported }

func rdevOf(fs.FileInfo) (major, minor uint32) { return 0, 0 }
