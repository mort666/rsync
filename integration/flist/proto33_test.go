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
	"github.com/gokrazy/rsync/rsyncclient"
)

// Protocol 33: the receiver counts the 4 KiB blocks it writes, and a
// server receiver sends the count to its client (MSG_BLOCK_STATS).

// blockFiles are the source files: a fresh copy writes 0+1+1+2+3+16 blocks.
var blockFiles = map[string]int{"empty": 0, "one": 1, "block": 4096, "blockplus": 4097, "three": 10000, "dir/big": 64 * 1024}

// blockTree makes the source, and a destination for an in-place delta: big
// with one byte changed in its sixth block, the rest missing.
func blockTree(t *testing.T) (src, fresh, delta string) {
	t.Helper()
	src = filepath.Join(t.TempDir(), "src")
	r := rand.New(rand.NewPCG(3, 4))
	var big []byte
	for name, size := range blockFiles {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(r.Uint32())
		}
		if name == "dir/big" {
			big = data
		}
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fresh = t.TempDir()
	delta = t.TempDir()
	if err := os.MkdirAll(filepath.Join(delta, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := bytes.Clone(big)
	old[5*4096+100] ^= 0xff
	if err := os.WriteFile(filepath.Join(delta, "dir/big"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	return src, fresh, delta
}

func checkBlockTree(t *testing.T, src, dest string) {
	t.Helper()
	for name := range blockFiles {
		want, _ := os.ReadFile(filepath.Join(src, name))
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes, %v; want %d bytes as sent", name, len(got), err, len(want))
		}
	}
}

// touchedRe is the line stock rsync's --stats prints from protocol 33.
var touchedRe = regexp.MustCompile(`Number of 4 KiB logical blocks touched: ([\d,]+)`)

// stockTouched is the count in stock rsync's --stats, or -1 if it has none.
func stockTouched(t *testing.T, out string) int64 {
	t.Helper()
	m := touchedRe.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// blockArgs are the transfers: a fresh copy, and an in-place delta with
// rsync's default blocks, where only the changed block is written again.
var blockArgs = map[string][]string{
	"fresh": {"-r", "-I"},
	"delta": {"-r", "-I", "--inplace", "--no-whole-file"},
}

func TestTouchedBlocks(t *testing.T) {
	protocols(t, func(t *testing.T, proto int) {
		p := fmt.Sprintf("--protocol=%d", proto)
		for _, kind := range []string{"fresh", "delta"} {
			t.Run(kind, func(t *testing.T) {
				args := append([]string{p}, blockArgs[kind]...)
				dest := func(fresh, delta string) string {
					if kind == "fresh" {
						return fresh
					}
					return delta
				}

				// The blocks either receiver writes: a fresh copy writes
				// every block; the delta only the 700-byte block holding
				// the changed byte, which straddles two 4 KiB blocks,
				// besides the other files.
				want := map[string]int64{"fresh": 23, "delta": 9}[kind]

				// Stock rsync counts the same, receiving from itself.
				src, fresh, delta := blockTree(t)
				ref := dest(fresh, delta)
				addr := startStockDaemonModule(t, ref, false)
				out, err := stock(t, append(args, "--stats", src+"/", "rsync://"+addr+"/mod/")...)
				if err != nil {
					t.Fatal(err)
				}
				checkBlockTree(t, src, ref)
				if got := stockTouched(t, out); got != wantAt(want, stockDaemonProtocol(proto), -1) {
					t.Errorf("protocol %d: stock rsync counts %d blocks receiving from itself, want %d", stockDaemonProtocol(proto), got, want)
				}

				t.Run("stock to gokrazy", func(t *testing.T) {
					src, fresh, delta := blockTree(t)
					dst := dest(fresh, delta)
					ts := rsynctest.New(t, rsynctest.WritableInteropModule(dst))
					out, err := stock(t, append(args, "--stats", src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
					if err != nil {
						t.Fatal(err)
					}
					checkBlockTree(t, src, dst)
					got := max(stockTouched(t, out), 0)
					if want := wantAt(want, proto, 0); got != want {
						t.Errorf("stock rsync prints %d blocks from gokrazy's receiver, want %d", got, want)
					}
				})
				t.Run("gokrazy pushes", func(t *testing.T) {
					src, fresh, delta := blockTree(t)
					dst := dest(fresh, delta)
					addr := startStockDaemonModule(t, dst, false)
					res, log, err := gokrazyRunResult(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
					if err != nil {
						t.Fatalf("%v\n%s", err, log)
					}
					checkBlockTree(t, src, dst)
					if got, want := res.Stats.TouchedBlocks, wantAt(want, stockDaemonProtocol(proto), 0); got != want {
						t.Errorf("gokrazy got %d blocks from stock rsync's receiver, want %d", got, want)
					}
				})
				t.Run("gokrazy pulls", func(t *testing.T) {
					src, fresh, delta := blockTree(t)
					dst := dest(fresh, delta)
					addr, _ := startStockDaemonConf(t, src, "")
					res, log, err := gokrazyRunResult(t, addr, "mod/", []string{dst + "/"}, nil, args...)
					if err != nil {
						t.Fatalf("%v\n%s", err, log)
					}
					checkBlockTree(t, src, dst)
					if got, want := res.Stats.TouchedBlocks, wantAt(want, stockDaemonProtocol(proto), 0); got != want {
						t.Errorf("gokrazy's receiver counted %d blocks, want %d", got, want)
					}
				})
				t.Run("gokrazy to gokrazy", func(t *testing.T) {
					src, fresh, delta := blockTree(t)
					dst := dest(fresh, delta)
					ts := rsynctest.New(t, rsynctest.WritableInteropModule(dst))
					res, log, err := gokrazyRunResult(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
					if err != nil {
						t.Fatalf("%v\n%s", err, log)
					}
					checkBlockTree(t, src, dst)
					if got, want := res.Stats.TouchedBlocks, wantAt(want, proto, 0); got != want {
						t.Errorf("gokrazy got %d blocks from its own receiver, want %d", got, want)
					}
				})
			})
		}
	})
}

// wantAt is the count want at a session running proto, or none below 33,
// where there is no count.
func wantAt(want int64, proto int, none int64) int64 {
	if proto < 33 {
		return none
	}
	return want
}
