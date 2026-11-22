package receiver

import (
	"math/rand/v2"

	"testing"
)

// trackRef is rsync's fileio.c:track_block_touches as written, with the
// last block touched (inclusive, -1 for none).
func trackRef(touched, last *int64, offset, n int64) {
	if n <= 0 {
		return
	}
	start := offset / 4096
	end := start + ((offset%4096)+n-1)/4096
	var add int64
	if start > *last {
		add = end - start + 1
	} else if end > *last {
		add = end - *last
	}
	*touched += add
	if end > *last {
		*last = end
	}
}

func TestBlockTracker(t *testing.T) {
	for _, tt := range []struct {
		name   string
		writes [][2]int64 // offset, length
		want   int64
	}{
		{"one byte", [][2]int64{{0, 1}}, 1},
		{"one block", [][2]int64{{0, 4096}}, 1},
		{"one block and a byte", [][2]int64{{0, 4097}}, 2},
		{"sequential small writes", [][2]int64{{0, 1000}, {1000, 1000}, {2000, 1000}, {3000, 1000}, {4000, 1000}}, 2},
		{"straddling", [][2]int64{{4095, 2}}, 2},
		{"a gap", [][2]int64{{0, 10}, {3 * 4096, 10}}, 2},
		{"empty write", [][2]int64{{0, 0}}, 0},
		{"rewrite earlier", [][2]int64{{8192, 10}, {0, 10}}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var b blockTracker
			for _, w := range tt.writes {
				b.track(w[0], w[1])
			}
			if b.touched != tt.want {
				t.Errorf("touched = %d, want %d", b.touched, tt.want)
			}
		})
	}
}

// The tracker agrees with rsync's on any series of writes.
func TestBlockTrackerMatchesRsync(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 1000 {
		var b blockTracker
		var touched, last int64 = 0, -1
		var off int64
		for range r.IntN(20) {
			n := r.Int64N(20000)
			if r.IntN(4) == 0 {
				off = r.Int64N(100000) // jump anywhere, even backwards
			}
			b.track(off, n)
			trackRef(&touched, &last, off, n)
			off += n
		}
		if b.touched != touched {
			t.Fatalf("touched = %d, rsync's = %d", b.touched, touched)
		}
	}
}

// Writes count their blocks; in place, skipped bytes count none, and a
// sparse file's holes count none either.
func TestPendingFileBlocks(t *testing.T) {
	root, _ := openRoot(t)
	p, err := newPendingFile(root, "f", outputMode{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	p.Write(make([]byte, 5000))
	p.Write([]byte("x"))
	if p.blocks.touched != 2 {
		t.Errorf("plain: touched = %d, want 2", p.blocks.touched)
	}

	if err := root.WriteFile("g", make([]byte, 3*4096), 0o644); err != nil {
		t.Fatal(err)
	}
	q, err := newPendingFile(root, "g", outputMode{Inplace: true})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Cleanup()
	if err := q.Skip(4096); err != nil {
		t.Fatal(err)
	}
	q.Write([]byte("y"))
	if err := q.Skip(4095 + 4096); err != nil {
		t.Fatal(err)
	}
	if q.blocks.touched != 1 {
		t.Errorf("inplace: touched = %d, want 1", q.blocks.touched)
	}
	if err := q.CloseAtomicallyReplace(); err != nil {
		t.Fatal(err)
	}
	if b, _ := root.ReadFile("g"); len(b) != 3*4096 || b[4096] != 'y' {
		t.Errorf("inplace: file is %d bytes, byte 4096 %q", len(b), b[4096])
	}

	s, err := newPendingFile(root, "s", outputMode{Sparse: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cleanup()
	data := make([]byte, 5*4096)
	data[0], data[len(data)-1] = 1, 1
	s.Write(data)
	if s.blocks.touched != 2 {
		t.Errorf("sparse: touched = %d, want 2", s.blocks.touched)
	}
}

func TestPendingFileSkipNeedsInplace(t *testing.T) {
	root, _ := openRoot(t)
	p, err := newPendingFile(root, "f", outputMode{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	if err := p.Skip(1); err == nil {
		t.Error("Skip in a temporary file succeeded")
	}
}
