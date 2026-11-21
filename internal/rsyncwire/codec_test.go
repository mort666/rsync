package rsyncwire

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The golden file holds commands for rsync's own codecs and what they
// returned, in the codec-harness format ("wvarint 128" -> "8080"). See
// testdata/README.md for how to regenerate it.
const goldenFile = "testdata/codecs.golden"

var update = flag.Bool("update", false, "regenerate "+goldenFile+" with the harness in $RSYNC_CODEC_HARNESS")

// codecs runs harness commands against the Go implementation, keeping the
// same ndx state the harness keeps.
type codecs struct {
	w NdxWriter
	r NdxReader
}

func decodeHex(s string) []byte {
	if s == "-" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func encodeHex(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	return hex.EncodeToString(b)
}

func readResult[T int32 | int64](v T, err error, r *bytes.Reader, total int) string {
	if err != nil {
		return "ERR"
	}
	return fmt.Sprintf("%d %d", v, total-r.Len())
}

func (c *codecs) run(cmd string) string {
	f := strings.Fields(cmd)
	num := func(s string) int64 {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			panic(err)
		}
		return v
	}
	switch f[0] {
	case "reset":
		*c = codecs{}
		return "ok"
	case "wvarint":
		return encodeHex(AppendVarint(nil, int32(num(f[1]))))
	case "wvarlong":
		return encodeHex(AppendVarlong(nil, num(f[2]), int(num(f[1]))))
	case "wndx":
		return encodeHex(c.w.AppendNdx(nil, int32(num(f[1]))))
	case "rvarint":
		b := decodeHex(f[1])
		r := bytes.NewReader(b)
		v, err := ReadVarint(r)
		return readResult(v, err, r, len(b))
	case "rvarlong":
		b := decodeHex(f[2])
		r := bytes.NewReader(b)
		v, err := ReadVarlong(r, int(num(f[1])))
		return readResult(v, err, r, len(b))
	case "rndx":
		b := decodeHex(f[1])
		r := bytes.NewReader(b)
		v, err := c.r.ReadNdx(r)
		return readResult(v, err, r, len(b))
	}
	panic("unknown command " + cmd)
}

// harness drives rsync's own codecs, built by codec-harness.
type harness struct {
	in  io.WriteCloser
	out *bufio.Scanner
}

func startHarness(t testing.TB) *harness {
	t.Helper()
	path := os.Getenv("RSYNC_CODEC_HARNESS")
	if path == "" {
		t.Skip("RSYNC_CODEC_HARNESS not set (see testdata/README.md)")
	}
	cmd := exec.Command(path)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Wait() })
	return &harness{in: in, out: bufio.NewScanner(out)}
}

func (h *harness) run(cmd string) string {
	fmt.Fprintln(h.in, cmd)
	if !h.out.Scan() {
		panic("harness exited")
	}
	res := h.out.Text()
	// The error messages differ; that a read failed is what must match.
	if strings.HasPrefix(res, "ERR") {
		return "ERR"
	}
	return res
}

var boundaries32 = func() []int64 {
	v := []int64{0, -1, 1, 0x7f, 0x80, 0xff, 0x100, 0x3fff, 0x4000, 0x1fffff, 0x200000,
		0xffffff, 0x1000000, 0xfffffff, 0x10000000, math.MaxInt32, math.MinInt32, -128, -129}
	for i := range 31 {
		v = append(v, 1<<i-1, 1<<i, 1<<i+1, -(1 << i))
	}
	return v
}()

var boundaries64 = func() []int64 {
	v := append([]int64{math.MaxInt64, math.MinInt64}, boundaries32...)
	for i := 31; i < 63; i++ {
		v = append(v, 1<<i-1, 1<<i, 1<<i+1, -(1 << i))
	}
	return v
}()

// goldenCommands is what -update asks the harness about.
func goldenCommands() []string {
	var cmds []string
	for _, x := range boundaries32 {
		cmds = append(cmds, fmt.Sprintf("wvarint %d", x))
	}
	for min := minVarlongBytes; min <= maxVarlongBytes; min++ {
		for _, x := range boundaries64 {
			cmds = append(cmds, fmt.Sprintf("wvarlong %d %d", min, x))
		}
	}
	// Steps around each encoding's limits, forwards, backwards and repeated,
	// for both signs, then the special values.
	cmds = append(cmds, "reset")
	for _, ndx := range []int64{0, 1, 2, 255, 256, 509, 510, 511, 511, 510, 0,
		0x7fff, 0x7fff + 0x7fff, 0x7fff + 0x8000 + 0x7fff, 100, math.MaxInt32, 0, math.MaxInt32 - 1} {
		cmds = append(cmds, fmt.Sprintf("wndx %d", ndx))
	}
	for _, ndx := range []int64{-1, -2, -3, -101, -102, -101, -3, -100000, -math.MaxInt32, -2, -1} {
		cmds = append(cmds, fmt.Sprintf("wndx %d", ndx))
	}
	// Decoding, including malformed and truncated input.
	for _, h := range []string{"-", "00", "7f", "80", "8001", "bfff", "c0", "c0ffff", "dfffff",
		"e0ffffff", "efffffff", "f0", "f0ffffffff", "f7ffffffff", "f8", "f8ffffffffff", "fc", "ff"} {
		cmds = append(cmds, "rvarint "+h)
	}
	for _, min := range []int{3, 4} {
		for _, h := range []string{"-", "0000", "000000", "7f0000", "800000", "c00000", "c0000000",
			"fc0000000000000000", "fc00000000", "fe000000000000000000", "ff000000000000000000ff",
			"f8ffffffffffffff", "ffffffffffffffffff"} {
			cmds = append(cmds, fmt.Sprintf("rvarlong %d %s", min, h))
		}
	}
	cmds = append(cmds, "reset")
	for _, h := range []string{"-", "00", "01", "fd", "fe", "fe0000", "fe7fff", "fe80000000",
		"fe8000000000", "feff00ffff", "feffffffff", "fe7fffffff", "ff", "ff01", "ff00", "fffe0001", "fffe80", "fffeffffffff"} {
		cmds = append(cmds, "rndx "+h)
	}
	return cmds
}

func TestCodecsGolden(t *testing.T) {
	if *update {
		h := startHarness(t)
		var buf bytes.Buffer
		buf.WriteString("# Generated by rsync's own io.c codecs (codec-harness): command, tab, result.\n")
		for _, cmd := range goldenCommands() {
			fmt.Fprintf(&buf, "%s\t%s\n", cmd, h.run(cmd))
		}
		if err := os.WriteFile(goldenFile, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	var c codecs
	n := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		cmd, want, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed golden line %q", line)
		}
		if got := c.run(cmd); got != want {
			t.Errorf("%s: got %s, rsync %s", cmd, got, want)
		}
		n++
	}
	if n < 500 {
		t.Errorf("only %d golden cases, want more", n)
	}
}

// Random values and random bytes through both implementations.
func TestCodecsAgainstHarness(t *testing.T) {
	h := startHarness(t)
	var c codecs
	rng := rand.New(rand.NewPCG(1, 2))
	randInt := func(bits int) int64 {
		// Bias towards small magnitudes, where the encodings branch.
		v := int64(rng.Uint64() >> (64 - rng.IntN(bits) - 1))
		if rng.IntN(2) == 0 {
			v = -v
		}
		return v
	}
	randHex := func() string {
		b := make([]byte, rng.IntN(12))
		for i := range b {
			b[i] = byte(rng.Uint32())
			if rng.IntN(3) == 0 {
				b[i] = []byte{0, 0x7f, 0x80, 0xbf, 0xc0, 0xdf, 0xe0, 0xef, 0xf0, 0xf7, 0xf8, 0xfc, 0xfe, 0xff}[rng.IntN(14)]
			}
		}
		return encodeHex(b)
	}
	n := 200000
	if testing.Short() {
		n = 20000
	}
	for range n {
		var cmd string
		switch rng.IntN(7) {
		case 0:
			cmd = fmt.Sprintf("wvarint %d", int32(randInt(32)))
		case 1:
			cmd = fmt.Sprintf("wvarlong %d %d", minVarlongBytes+rng.IntN(maxVarlongBytes-minVarlongBytes+1), randInt(64))
		case 2:
			ndx := int32(randInt(32))
			if rng.IntN(2) == 0 {
				ndx = int32(rng.IntN(600)) - 120 // steps near the previous index
			}
			cmd = fmt.Sprintf("wndx %d", ndx)
		case 3:
			cmd = "rvarint " + randHex()
		case 4:
			cmd = fmt.Sprintf("rvarlong %d %s", minVarlongBytes+rng.IntN(maxVarlongBytes-minVarlongBytes+1), randHex())
		case 5:
			cmd = "rndx " + randHex()
		case 6:
			if rng.IntN(50) == 0 {
				cmd = "reset"
			} else {
				cmd = fmt.Sprintf("wvarint %d", int32(rng.Uint32()))
			}
		}
		if got, want := c.run(cmd), h.run(cmd); got != want {
			t.Fatalf("%s: got %s, rsync %s", cmd, got, want)
		}
	}
}

func FuzzVarintRoundTrip(f *testing.F) {
	for _, x := range boundaries32 {
		f.Add(int32(x))
	}
	f.Fuzz(func(t *testing.T, x int32) {
		b := AppendVarint(nil, x)
		r := bytes.NewReader(b)
		got, err := ReadVarint(r)
		if err != nil || got != x || r.Len() != 0 {
			t.Fatalf("varint %d: encoded %x, read back %d (%d bytes left), %v", x, b, got, r.Len(), err)
		}
	})
}

func FuzzVarlongRoundTrip(f *testing.F) {
	for _, x := range boundaries64 {
		f.Add(x, uint8(3))
		f.Add(x, uint8(4))
	}
	f.Fuzz(func(t *testing.T, x int64, m uint8) {
		minBytes := minVarlongBytes + int(m)%(maxVarlongBytes-minVarlongBytes+1)
		b := AppendVarlong(nil, x, minBytes)
		r := bytes.NewReader(b)
		got, err := ReadVarlong(r, minBytes)
		if err != nil || got != x || r.Len() != 0 {
			t.Fatalf("varlong %d (min %d): encoded %x, read back %d (%d bytes left), %v", x, minBytes, b, got, r.Len(), err)
		}
	})
}

// A sequence of indexes written by one NdxWriter reads back through one
// NdxReader. Index values are what rsync sends: non-negative, NdxDone, or
// the other negative specials and flist offsets above math.MinInt32.
func FuzzNdxRoundTrip(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{0xff, 0xfe, 0x7f, 0x80, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		var w NdxWriter
		var r NdxReader
		var want []int32
		var buf []byte
		for len(data) >= 4 {
			ndx := int32(uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24)
			data = data[4:]
			if ndx == math.MinInt32 {
				continue // -ndx does not fit; rsync never sends it
			}
			want = append(want, ndx)
			buf = w.AppendNdx(buf, ndx)
		}
		rd := bytes.NewReader(buf)
		for i, ndx := range want {
			got, err := r.ReadNdx(rd)
			if err != nil || got != ndx {
				t.Fatalf("index %d: wrote %d, read %d, %v (sequence %v, bytes %x)", i, ndx, got, err, want, buf)
			}
		}
		if rd.Len() != 0 {
			t.Fatalf("%d bytes left over", rd.Len())
		}
	})
}

// Arbitrary input never panics the decoders.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0xfe, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0xf8, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		ReadVarint(bytes.NewReader(data))
		for m := minVarlongBytes; m <= maxVarlongBytes; m++ {
			ReadVarlong(bytes.NewReader(data), m)
		}
		var r NdxReader
		rd := bytes.NewReader(data)
		for rd.Len() > 0 {
			if _, err := r.ReadNdx(rd); err != nil {
				break
			}
		}
	})
}

func TestConnProtocolSwitch(t *testing.T) {
	for _, tt := range []struct {
		proto int32
		want  string
	}{
		// Below 30: int32, longint, int32.
		{27, "2c010000" + "ffffffff" + "0000000001000000" + "05000000"},
		// From 30: varint, varlong(3), delta ndx (5 - (-1) = 6).
		{30, "812c" + "c100000000" + "06"},
	} {
		var buf bytes.Buffer
		c := &Conn{Writer: nopWC{&buf}, Reader: io.NopCloser(&buf), Session: Session{Protocol: tt.proto}}
		if err := c.WriteVarint30(300); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteVarlong30(1<<32, 3); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteNdx(5); err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(buf.Bytes()); got != tt.want {
			t.Errorf("protocol %d: wrote %s, want %s", tt.proto, got, tt.want)
		}
		if v, err := c.ReadVarint30(); err != nil || v != 300 {
			t.Errorf("protocol %d: ReadVarint30 = %d, %v", tt.proto, v, err)
		}
		if v, err := c.ReadVarlong30(3); err != nil || v != 1<<32 {
			t.Errorf("protocol %d: ReadVarlong30 = %d, %v", tt.proto, v, err)
		}
		if v, err := c.ReadNdx(); err != nil || v != 5 {
			t.Errorf("protocol %d: ReadNdx = %d, %v", tt.proto, v, err)
		}
	}
}

type nopWC struct{ io.Writer }

func (nopWC) Close() error { return nil }
