package rsynctoken

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/klauspost/compress/zstd"
)

// Most tests here compare with upstream rsync's own token.c, built on its
// own as testdata/tokharness (see its Makefile): once with rsync's bundled
// zlib, once with the system zlib, which distribution builds use. They skip
// when it is not built. RSYNC_TOKEN_HARNESS names the folder holding the
// binaries (default testdata/tokharness/bin).

func harnessDir() string {
	if d := os.Getenv("RSYNC_TOKEN_HARNESS"); d != "" {
		return d
	}
	d, _ := filepath.Abs("testdata/tokharness/bin")
	return d
}

// forEachHarness runs f once per C harness built.
func forEachHarness(t *testing.T, f func(t *testing.T, h string)) {
	ran := false
	for _, name := range []string{"tokharness", "tokharness-sysz"} {
		p := filepath.Join(harnessDir(), name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		ran = true
		label := map[string]string{"tokharness": "bundled-zlib", "tokharness-sysz": "system-zlib"}[name]
		t.Run(label, func(t *testing.T) { f(t, p) })
	}
	if !ran {
		t.Skip("upstream token.c harness not built; see testdata/tokharness/Makefile")
	}
}

// harnessHas reports whether harness h was built with algo.
func harnessHas(h, algo string) bool {
	err := exec.Command(h, "recv", algo, "31", "/dev/null", "1").Run()
	var ee *exec.ExitError
	return !(errors.As(err, &ee) && ee.ExitCode() == 2)
}

func session(algo string, proto, level int) rsyncwire.Session {
	s := rsyncwire.Session{Protocol: int32(proto), CompressLevel: level}
	if algo != "none" {
		s.Compress = algo
	}
	return s
}

func newEncoder(t testing.TB, w io.Writer, algo string, proto, level int) Encoder {
	t.Helper()
	e, err := NewEncoder(session(algo, proto, level), w)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func newDecoder(t testing.TB, r io.Reader, algo string, proto int) Decoder {
	t.Helper()
	d, err := NewDecoder(session(algo, proto, 0), r)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func goEncode(t testing.TB, c *Case, algo string, proto, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Encode(newEncoder(t, &buf, algo, proto, level)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func goDecode(t testing.TB, stream []byte, c *Case, algo string, proto int) ([]byte, error) {
	t.Helper()
	return Decode(newDecoder(t, bytes.NewReader(stream), algo, proto), c.Basis, c.BlockLen)
}

func cEncode(t testing.TB, h, dir, algo string, proto, level int) []byte {
	t.Helper()
	cmd := exec.Command(h, "send", algo, strconv.Itoa(proto), strconv.Itoa(level),
		filepath.Join(dir, "new"), filepath.Join(dir, "ops"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("C send: %v: %s", err, stderr.String())
	}
	return out
}

func cDecode(h, dir string, stream []byte, algo string, proto, blockLen int) ([]byte, error) {
	cmd := exec.Command(h, "recv", algo, strconv.Itoa(proto),
		filepath.Join(dir, "basis"), strconv.Itoa(blockLen))
	cmd.Stdin = bytes.NewReader(stream)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%v: %s", err, stderr.String())
	}
	return out, nil
}

// canDecode reports whether this package reads algo.
func canDecode(algo string) bool { return slices.Contains(receiveNames, algo) }

// levels for algo in the cross tests: lz4 has none.
func levels(algo string) []int {
	switch algo {
	case "lz4", "none":
		return []int{0}
	case "zstd":
		return []int{1, 3, 19}
	}
	return []int{1, 6, 9}
}

type params struct {
	seed     uint64
	size     int
	blockLen int
}

// Small and large files, block sizes either side of the 64 KiB piece size
// that triggers the protocol 30 history bug.
var cases = []params{
	{1, 64 * 1024, 700},
	{2, 256 * 1024, 700},
	{3, 512 * 1024, 2048},
	{4, 1 << 20, 8192},
	{5, 2 << 20, 100_000}, // > 0xffff: exercises the pre-31 bug path
	{6, 300 * 1024, 131072},
	{7, 128 * 1024, 64},
}

func TestCrossImplementation(t *testing.T) {
	forEachHarness(t, func(t *testing.T, h string) {
		for _, p := range cases {
			c := GenCase(p.seed, p.size, p.blockLen)
			dir := t.TempDir()
			if err := c.WriteFiles(dir); err != nil {
				t.Fatal(err)
			}
			for _, algo := range Names {
				if !harnessHas(h, algo) {
					continue
				}
				for _, proto := range []int{30, 31} {
					for _, level := range levels(algo) {
						name := fmt.Sprintf("seed%d/blk%d/%s/p%d/l%d", p.seed, p.blockLen, algo, proto, level)
						if canDecode(algo) {
							t.Run(name+"/C-to-Go", func(t *testing.T) {
								stream := cEncode(t, h, dir, algo, proto, level)
								got, err := goDecode(t, stream, c, algo, proto)
								if err != nil {
									t.Fatal(err)
								}
								if !bytes.Equal(got, c.New) {
									t.Fatalf("decoded %d bytes, want %d, content differs", len(got), len(c.New))
								}
							})
						}
						t.Run(name+"/Go-to-C", func(t *testing.T) {
							stream := goEncode(t, c, algo, proto, level)
							got, err := cDecode(h, dir, stream, algo, proto, p.blockLen)
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(got, c.New) {
								t.Fatalf("C decoded %d bytes, want %d, content differs", len(got), len(c.New))
							}
						})
					}
				}
			}
		}
	})
}

// TestManySeeds runs a lot of small random cases in both directions.
func TestManySeeds(t *testing.T) {
	forEachHarness(t, func(t *testing.T, h string) {
		n := 300
		if testing.Short() {
			n = 30
		}
		algos := []string{"zlib", "zlibx"}
		if harnessHas(h, "lz4") {
			algos = append(algos, "lz4")
		}
		for seed := uint64(100); seed < uint64(100+n); seed++ {
			blockLen := []int{50, 300, 700, 1500, 70_000}[seed%5]
			c := GenCase(seed, 20*1024+int(seed%7)*37*1024, blockLen)
			dir := t.TempDir()
			if err := c.WriteFiles(dir); err != nil {
				t.Fatal(err)
			}
			algo := algos[seed%uint64(len(algos))]
			proto := 30 + int(seed/2%2)
			level := 1 + int(seed%9)
			stream := cEncode(t, h, dir, algo, proto, level)
			if got, err := goDecode(t, stream, c, algo, proto); err != nil || !bytes.Equal(got, c.New) {
				t.Fatalf("seed %d %v p%d l%d: C->Go failed: %v", seed, algo, proto, level, err)
			}
			stream = goEncode(t, c, algo, proto, level)
			if got, err := cDecode(h, dir, stream, algo, proto, blockLen); err != nil || !bytes.Equal(got, c.New) {
				t.Fatalf("seed %d %v p%d l%d: Go->C failed: %v", seed, algo, proto, level, err)
			}
		}
	})
}

// TestNegativeControls checks that the tests can fail: were the history
// not kept the way upstream keeps it, decoding would break.
func TestNegativeControls(t *testing.T) {
	forEachHarness(t, func(t *testing.T, h string) {
		c := GenCase(42, 512*1024, 100_000)
		dir := t.TempDir()
		if err := c.WriteFiles(dir); err != nil {
			t.Fatal(err)
		}
		t.Run("zlib stream decoded without matched data in history", func(t *testing.T) {
			stream := cEncode(t, h, dir, "zlib", 31, 6)
			got, err := goDecode(t, stream, c, "zlibx", 31)
			if err == nil && bytes.Equal(got, c.New) {
				t.Fatal("decoding upstream zlib as zlibx succeeded; the test data has no back-references into matched blocks")
			}
		})
		t.Run("protocol 30 stream decoded with protocol 31 history", func(t *testing.T) {
			stream := cEncode(t, h, dir, "zlib", 30, 6)
			got, err := goDecode(t, stream, c, "zlib", 31)
			if err == nil && bytes.Equal(got, c.New) {
				t.Fatal("protocol 30 bug emulation made no difference; blocks over 64 KiB are not being exercised")
			}
		})
		t.Run("Go zlib stream decoded by upstream as zlibx", func(t *testing.T) {
			stream := goEncode(t, c, "zlib", 31, 6)
			got, err := cDecode(h, dir, stream, "zlibx", 31, c.BlockLen)
			if err == nil && bytes.Equal(got, c.New) {
				t.Fatal("upstream decoded Go's zlib stream without matched-data history; Go is not using the dictionary")
			}
		})
	})
}

// TestLongTokenRun covers runs of more than 65,535 blocks, which upstream
// splits into several TOKENRUN records.
func TestLongTokenRun(t *testing.T) {
	c := GenLongRun(150_000, 16)
	for _, algo := range receiveNames {
		stream := goEncode(t, c, algo, 31, 6)
		if got, err := goDecode(t, stream, c, algo, 31); err != nil || !bytes.Equal(got, c.New) {
			t.Fatalf("%v Go->Go: %v", algo, err)
		}
	}
	forEachHarness(t, func(t *testing.T, h string) {
		dir := t.TempDir()
		if err := c.WriteFiles(dir); err != nil {
			t.Fatal(err)
		}
		for _, algo := range Names {
			if !harnessHas(h, algo) {
				continue
			}
			if canDecode(algo) {
				cs := cEncode(t, h, dir, algo, 31, 6)
				if got, err := goDecode(t, cs, c, algo, 31); err != nil || !bytes.Equal(got, c.New) {
					t.Fatalf("%v C->Go: %v", algo, err)
				}
			}
			gs := goEncode(t, c, algo, 31, 6)
			if got, err := cDecode(h, dir, gs, algo, 31, c.BlockLen); err != nil || !bytes.Equal(got, c.New) {
				t.Fatalf("%v Go->C: %v", algo, err)
			}
		}
	})
}

// TestMultipleFiles uses one encoder and one decoder for several files, as
// a transfer does. Upstream starts each file's deflate stream afresh, so no
// history may leak from one file into the next.
func TestMultipleFiles(t *testing.T) {
	var files []*Case
	for i := range 6 {
		files = append(files, GenCase(uint64(500+i), 64*1024+i*40*1024, []int{700, 2048, 100_000}[i%3]))
	}
	for _, algo := range receiveNames {
		var stream bytes.Buffer
		e := newEncoder(t, &stream, algo, 31, 6)
		var starts []int
		for _, c := range files {
			starts = append(starts, stream.Len())
			if err := c.Encode(e); err != nil {
				t.Fatal(err)
			}
		}
		starts = append(starts, stream.Len())
		d := newDecoder(t, bytes.NewReader(stream.Bytes()), algo, 31)
		for i, c := range files {
			// Each file's stream decodes on its own: nothing carried over.
			own := stream.Bytes()[starts[i]:starts[i+1]]
			if got, err := goDecode(t, own, c, algo, 31); err != nil || !bytes.Equal(got, c.New) {
				t.Fatalf("%v file %d alone: %v", algo, i, err)
			}
			got, err := Decode(d, c.Basis, c.BlockLen)
			if err != nil || !bytes.Equal(got, c.New) {
				t.Fatalf("%v file %d with the shared decoder: %v", algo, i, err)
			}
		}
		// The decoder read no further than the last END_FLAG.
		if _, _, err := d.Next(); err != io.ErrUnexpectedEOF {
			t.Errorf("%v: after the last file, Next = %v, want io.ErrUnexpectedEOF", algo, err)
		}
	}
}

// TestDecoderStopsAtEnd checks that a decoder reads nothing past a file's
// END_FLAG: the file's checksum follows on the connection.
func TestDecoderStopsAtEnd(t *testing.T) {
	c := GenCase(77, 100*1024, 700)
	trailer := []byte("the file's checksum")
	for _, algo := range receiveNames {
		stream := append(goEncode(t, c, algo, 31, 6), trailer...)
		r := bytes.NewReader(stream)
		// No ReadByte: the decoder must add one without reading ahead.
		d := newDecoder(t, struct{ io.Reader }{r}, algo, 31)
		if got, err := Decode(d, c.Basis, c.BlockLen); err != nil || !bytes.Equal(got, c.New) {
			t.Fatalf("%v: %v", algo, err)
		}
		rest, _ := io.ReadAll(r)
		if !bytes.Equal(rest, trailer) {
			t.Errorf("%v: %d bytes left after the tokens, want the %d-byte trailer", algo, len(rest), len(trailer))
		}
	}
}

// TestZstdStream checks the zstd encoder without the harness: its frame
// payloads, joined, are one zstd stream holding the literal data of every
// file in turn.
func TestZstdStream(t *testing.T) {
	var files []*Case
	for i := range 3 {
		files = append(files, GenCase(uint64(600+i), 200*1024, 700))
	}
	var stream bytes.Buffer
	e := newEncoder(t, &stream, "zstd", 31, 3)
	var literal []byte
	for _, c := range files {
		if err := c.Encode(e); err != nil {
			t.Fatal(err)
		}
		pos := 0
		for _, op := range c.Ops {
			literal = append(literal, c.New[pos:pos+op.N]...)
			pos += op.N
			if op.Token >= 0 {
				pos += op.TokLen
			}
		}
	}
	// Pick the DEFLATED_DATA payloads out of the token stream.
	var payload []byte
	rr := runReader{r: bytes.NewReader(stream.Bytes())}
	buf := make([]byte, maxDataCount)
	for {
		flag, err := rr.r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case flag&0xc0 == deflatedData:
			p, err := rr.readData(flag, buf)
			if err != nil {
				t.Fatal(err)
			}
			payload = append(payload, p...)
		case flag == endFlag:
		default:
			if _, err := rr.token(flag); err != nil {
				t.Fatal(err)
			}
			rr.rxRun = 0
		}
	}
	zr, err := zstd.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	got, err := io.ReadAll(zr)
	// The frame never ends, so the reader reports that once the data runs
	// out.
	if !bytes.Equal(got, literal) {
		t.Fatalf("zstd payload decodes to %d bytes, want %d (%v)", len(got), len(literal), err)
	}
}

// TestShortDictionary checks that priming the encoder with less than the
// full window still makes a stream upstream accepts.
func TestShortDictionary(t *testing.T) {
	forEachHarness(t, func(t *testing.T, h string) {
		c := GenCase(11, 1<<20, 700)
		dir := t.TempDir()
		if err := c.WriteFiles(dir); err != nil {
			t.Fatal(err)
		}
		for _, limit := range []int{1, 1024, 4096, 8192, 16384, windowSize} {
			for _, algo := range []string{"zlib", "zlibx"} {
				var buf bytes.Buffer
				e := newDeflateEncoder(&buf, algo == "zlibx", 31, 6)
				e.dictLimit = limit
				if err := c.Encode(e); err != nil {
					t.Fatal(err)
				}
				got, err := cDecode(h, dir, buf.Bytes(), algo, 31, c.BlockLen)
				if err != nil || !bytes.Equal(got, c.New) {
					t.Fatalf("limit %d %v: upstream decode failed: %v", limit, algo, err)
				}
				t.Logf("dictionary %5d %-5v: %d bytes", limit, algo, buf.Len())
			}
		}
	})
}

// FuzzDecode feeds arbitrary bytes to each decoder. It must never panic or
// hang, and must report an error, not io.EOF, for truncated input.
func FuzzDecode(f *testing.F) {
	basis := GenCase(1, 4096, 64).Basis
	for i, algo := range receiveNames {
		// Block indices in these refer to another basis; they are only
		// starting points for the mutator.
		f.Add(byte(i), goEncode(f, GenCase(3, 8*1024, 64), algo, 31, 6))
	}
	f.Add(byte(0), []byte{0x40, 0x05, 1, 2, 3, 4, 5, 0x81, 0xc3, 0xff, 0xff, 0x00})
	f.Add(byte(1), []byte{0x21, 0xff, 0xff, 0xff, 0x7f, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, sel byte, stream []byte) {
		algo := receiveNames[int(sel)%len(receiveNames)]
		proto := 30 + int(sel/8%2)
		d := newDecoder(t, bytes.NewReader(stream), algo, proto)
		count := (len(basis) + 63) / 64
		out := 0
		for out < 64<<20 { // a run token turns 3 bytes into 4 MiB; cap it
			lit, tok, err := d.Next()
			if err != nil {
				if err == io.EOF && !strings.Contains(string(stream), "\x00") && algo != "none" {
					t.Fatalf("io.EOF without END_FLAG in the input")
				}
				return
			}
			if lit != nil {
				out += len(lit)
				continue
			}
			if tok < 0 || int(tok) >= count {
				return // the receiver rejects this
			}
			b := basis[int(tok)*64 : min(int(tok+1)*64, len(basis))]
			if w := d.See(int64(len(b))); w != nil {
				w.Write(b)
			}
			out += len(b)
		}
	})
}

// FuzzRoundTrip makes random cases from the fuzz input and checks the
// decoder reads back what the encoder wrote.
func FuzzRoundTrip(f *testing.F) {
	f.Add(uint64(1), uint16(700), uint32(64*1024), byte(0))
	f.Fuzz(func(t *testing.T, seed uint64, blockLen uint16, size uint32, sel byte) {
		bl := 16 + int(blockLen)%20000
		sz := 1 + int(size%(512*1024))
		c := GenCase(seed, sz, bl)
		algo := receiveNames[int(sel)%len(receiveNames)]
		proto := 30 + int(sel/8%2)
		level := 1 + int(sel/16%9)
		stream := goEncode(t, c, algo, proto, level)
		got, err := goDecode(t, stream, c, algo, proto)
		if err != nil || !bytes.Equal(got, c.New) {
			t.Fatalf("%s round trip failed: %v", algo, err)
		}
	})
}

func BenchmarkEncode(b *testing.B) {
	c := GenCase(9, 4<<20, 700)
	for _, limit := range []int{8 * 1024, defaultDictLimit, windowSize} {
		for _, algo := range []string{"zlib", "zlibx"} {
			b.Run(fmt.Sprintf("%s/dict%d", algo, limit), func(b *testing.B) {
				b.SetBytes(int64(len(c.New)))
				b.ReportAllocs()
				var size int
				for b.Loop() {
					var buf bytes.Buffer
					e := newDeflateEncoder(&buf, algo == "zlibx", 31, 6)
					e.dictLimit = limit
					if err := c.Encode(e); err != nil {
						b.Fatal(err)
					}
					size = buf.Len()
				}
				b.ReportMetric(float64(size), "bytes")
			})
		}
	}
	for _, algo := range []string{"lz4", "zstd"} {
		b.Run(algo, func(b *testing.B) {
			b.SetBytes(int64(len(c.New)))
			for b.Loop() {
				goEncode(b, c, algo, 31, 3)
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	c := GenCase(9, 4<<20, 700)
	for _, algo := range receiveNames {
		stream := goEncode(b, c, algo, 31, 6)
		b.Run(algo, func(b *testing.B) {
			b.SetBytes(int64(len(c.New)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := goDecode(b, stream, c, algo, 31); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
