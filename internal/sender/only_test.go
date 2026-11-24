package sender

import (
	"slices"
	"strings"
	"testing"
)

// TestFilesFromNames checks how --files-from lines become names: kept
// inside the source folder as rsync's sanitize_path does, and comment lines
// dropped only for a file read on the sending side.
func TestFilesFromNames(t *testing.T) {
	lines := []string{"a.txt", "/abs/b", "../up", "d/../../c", "# note", "; note", "d/", "./e", "."}
	got := FilesFromNames(lines, false)
	want := []string{"a.txt", "abs/b", "up", "c", "d", "e", "."}
	if !slices.Equal(got, want) {
		t.Errorf("FilesFromNames(local) = %q, want %q", got, want)
	}
	got = FilesFromNames(lines, true)
	if !slices.Contains(got, "# note") || !slices.Contains(got, "; note") {
		t.Errorf("FilesFromNames(from the connection) = %q, want the comment lines kept", got)
	}
	if got := FilesFromNames(nil, false); got == nil {
		t.Errorf("FilesFromNames(nil) = nil: an empty list must send nothing, not everything")
	}
}

// TestReadFilesFromConn reads names as a receiving client sends them, and
// stops at the empty one without reading on.
func TestReadFilesFromConn(t *testing.T) {
	r := strings.NewReader("a\x00d/e\x00\x00after")
	got, err := ReadFilesFromConn(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "d/e"}; !slices.Equal(got, want) {
		t.Errorf("ReadFilesFromConn = %q, want %q", got, want)
	}
	if r.Len() != len("after") {
		t.Errorf("ReadFilesFromConn read past the end marker: %d bytes left", r.Len())
	}
}
