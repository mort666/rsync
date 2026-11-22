//go:build linux

package flist_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/internal/rsynctoken"
	"github.com/gokrazy/rsync/rsyncclient"
)

// -z against stock rsync, live, with each compressor in each direction.
// The tree mixes a delta transfer (matched blocks, which zlib puts into
// its history, then literal data that copies them), a file sent whole, and
// a file of random data, which no compressor shrinks.

// zTree writes src and a dest that already has an older copy of "delta".
func zTree(t *testing.T) (src, dest string, want map[string][]byte) {
	t.Helper()
	src, dest = filepath.Join(t.TempDir(), "src"), t.TempDir()
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2))
	text := func(n int) []byte {
		words := strings.Fields("rsync token deflate history window block match literal sender receiver compress")
		var b bytes.Buffer
		for b.Len() < n {
			b.WriteString(words[r.IntN(len(words))])
			b.WriteByte(" \n"[r.IntN(2)])
		}
		return b.Bytes()[:n]
	}
	noise := make([]byte, 100000)
	for i := range noise {
		noise[i] = byte(r.Uint32())
	}
	old := text(400000)
	delta := bytes.Clone(old)
	// New data that copies matched data from just before it, then a
	// changed middle.
	copy(delta[200000:], old[150000:160000])
	copy(delta[300000:], text(5000))
	want = map[string][]byte{
		"delta": delta,
		"whole": text(300000),
		"noise": noise,
		"empty": nil,
	}
	for name, data := range want {
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dest, "delta"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	return src, dest, want
}

func checkZTree(t *testing.T, dest string, want map[string][]byte) {
	t.Helper()
	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s differs after the transfer (%d bytes, want %d; %v)", name, len(got), len(data), err)
		}
	}
}

var sentRe = regexp.MustCompile(`Total bytes (sent|received): ([0-9,]+)`)

// wireBytes is how much the stock side sent (pushing) or received
// (pulling), from its --stats output.
func wireBytes(t *testing.T, out, which string) int {
	t.Helper()
	for _, m := range sentRe.FindAllStringSubmatch(out, -1) {
		if m[1] == which {
			n, _ := strconv.Atoi(strings.ReplaceAll(m[2], ",", ""))
			return n
		}
	}
	t.Fatalf("no Total bytes %s in rsync output:\n%s", which, out)
	return 0
}

// stockCompressors is what the stock rsync can use: its compress list,
// or zlib alone before rsync 3.2.
func stockCompressors(t *testing.T) []string {
	if !rsynctest.RsyncVersionAtLeast(t, "3.2.0") {
		return nil
	}
	out, _ := stock(t, "--version")
	_, list, ok := strings.Cut(out, "Compress list:")
	if !ok {
		return nil
	}
	line, _, _ := strings.Cut(strings.TrimSpace(list), "\n")
	return strings.Fields(line)
}

// The uncompressed tree is about 800 KB, a quarter of it random; text
// compresses well, so anything compressed moves less than this.
const zLimit = 500000

func TestCompressFromGokrazy(t *testing.T) {
	cases := append([]string{"-z"}, stockCompressors(t)...)
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			src, dest, want := zTree(t)
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			args := []string{"-z", "-r", "-I", "--stats", "--debug=nstr"}
			if c != "-z" {
				args = append(args, "--zc="+c)
			}
			if c == "-z" && len(cases) == 1 {
				args = []string{"-z", "-r", "-I", "--stats"} // no --debug before 3.1
			}
			out, err := stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
			if err != nil {
				t.Fatalf("%v", err)
			}
			checkZTree(t, dest, want)
			if c != "none" {
				if got := wireBytes(t, out, "received"); got > zLimit {
					t.Errorf("stock rsync received %d bytes, want under %d: not compressed", got, zLimit)
				}
			}
			// Below protocol 30 (run-tests.sh -p) nothing is negotiated: zlib.
			if c == "-z" && len(cases) > 1 && !regexp.MustCompile(`negotiated compress: zstd|Client compress: zlib `).MatchString(out) {
				t.Errorf("stock rsync did not negotiate zstd from a gokrazy sender")
			}
		})
	}
}

func TestCompressToGokrazy(t *testing.T) {
	cases := append([]string{"-z"}, stockCompressors(t)...)
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			src, dest, want := zTree(t)
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			args := []string{"-z", "-r", "-I", "--stats", "--debug=nstr"}
			if c != "-z" {
				args = append(args, "--zc="+c)
			}
			if c == "-z" && len(cases) == 1 {
				args = []string{"-z", "-r", "-I", "--stats"}
			}
			out, err := stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
			if c == "zstd" {
				// gokrazy/rsync cannot receive zstd.
				if err == nil || !strings.Contains(out, "cannot receive zstd") {
					t.Fatalf("pushing zstd: %v, want an error that says gokrazy/rsync cannot receive it", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v", err)
			}
			checkZTree(t, dest, want)
			if c != "none" {
				if got := wireBytes(t, out, "sent"); got > zLimit {
					t.Errorf("stock rsync sent %d bytes, want under %d: not compressed", got, zLimit)
				}
			}
			if c == "-z" && len(cases) > 1 && !regexp.MustCompile(`negotiated compress: lz4|Client compress: zlib `).MatchString(out) {
				t.Errorf("stock rsync did not negotiate lz4 with a gokrazy receiver")
			}
		})
	}
}

// A gokrazy client against a stock daemon, pulling and pushing.
func TestCompressGokrazyClient(t *testing.T) {
	cases := []string{"-z"}
	if len(stockCompressors(t)) > 0 {
		cases = append(cases, rsynctoken.Names...)
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			args := []string{"-z", "-r", "-I", "--debug=nstr"}
			if c != "-z" {
				args = append(args, "--zc="+c)
			}

			src, dest, want := zTree(t)
			addr, _ := startStockDaemonConf(t, src, "")
			out, err := gokrazyPull(t, addr, "", dest, args...)
			if c == "zstd" {
				if err == nil || !strings.Contains(err.Error()+out, "cannot receive zstd") {
					t.Errorf("pulling with zstd: %v, want an error that says gokrazy/rsync cannot receive it", err)
				}
			} else {
				if err != nil {
					t.Fatalf("pull: %v\n%s", err, out)
				}
				checkZTree(t, dest, want)
			}

			src, dest, want = zTree(t)
			addr = startStockDaemonModule(t, dest, false)
			if out, err := gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			checkZTree(t, dest, want)
		})
	}
}

// Compression at each protocol, where nothing is negotiated below 30: zlib.
func TestCompressProtocols(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		p := fmt.Sprintf("--protocol=%d", proto)
		src, dest, want := zTree(t)
		ts := rsynctest.New(t, rsynctest.InteropModule(src))
		if _, err := stock(t, p, "-z", "-r", "-I", "rsync://localhost:"+ts.Port+"/interop/", dest+"/"); err != nil {
			t.Fatalf("pull: %v", err)
		}
		checkZTree(t, dest, want)

		src, dest, want = zTree(t)
		ts = rsynctest.New(t, rsynctest.WritableInteropModule(dest))
		if _, err := stock(t, p, "-z", "-r", "-I", src+"/", "rsync://localhost:"+ts.Port+"/interop/"); err != nil {
			t.Fatalf("push: %v", err)
		}
		checkZTree(t, dest, want)
	})
}

// A gokrazy client against a gokrazy daemon, pulling and pushing. Neither
// side receives zstd, so -z settles on lz4.
func TestCompressGokrazyToGokrazy(t *testing.T) {
	for _, c := range []string{"-z", "lz4", "zlibx", "zlib", "none"} {
		t.Run(c, func(t *testing.T) {
			args := []string{"-z", "-r", "-I", "--debug=nstr"}
			if c != "-z" {
				args = append(args, "--zc="+c)
			}
			wantName := c
			if c == "-z" {
				wantName = "lz4"
			}

			src, dest, want := zTree(t)
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			out, err := gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{dest}, nil, args...)
			if err != nil {
				t.Fatalf("pull: %v\n%s", err, out)
			}
			checkZTree(t, dest, want)
			if wantName != "none" && !strings.Contains(out, "compress: "+wantName) {
				t.Errorf("pull did not use %s:\n%s", wantName, out)
			}

			src, dest, want = zTree(t)
			ts = rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			if out, err := gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			checkZTree(t, dest, want)
		})
	}
}
