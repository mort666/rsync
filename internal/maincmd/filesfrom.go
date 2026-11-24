package maincmd

import "io"

// writeFilesFrom sends --files-from names to a sender, as rsync's
// io.c:forward_filesfrom_data: each one NUL-ended, and an empty one last.
func writeFilesFrom(w io.Writer, names []string) error {
	var buf []byte
	for _, n := range names {
		buf = append(buf, n...)
		buf = append(buf, 0)
	}
	buf = append(buf, 0)
	_, err := w.Write(buf)
	return err
}
