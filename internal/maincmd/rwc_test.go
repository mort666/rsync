package maincmd

import (
	"io"
	"testing"
)

type closeCounter struct{ n *int }

func (c closeCounter) Read([]byte) (int, error)  { return 0, io.EOF }
func (c closeCounter) Write([]byte) (int, error) { return 0, nil }
func (c closeCounter) Close() error              { *c.n++; return nil }

// A connection built from a reader and a writer alone closes both.
func TestReadWriteCloserWithoutCloser(t *testing.T) {
	var n int
	rwc := &readWriteCloser{r: closeCounter{&n}, w: closeCounter{&n}}
	if err := rwc.Close(); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("closed %d ends, want 2", n)
	}
}
