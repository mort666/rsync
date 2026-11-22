//go:build linux

package flist_test

import (
	"regexp"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// --block-size fixes the delta block length. deltaTree changes 100 bytes at
// offset 100,000 of 200,000, which costs one block: 4,096 bytes with
// -B4096 (195,904 matched), 700 with rsync's own choice (199,300).
const blockSizeMatched = 200000 - 4096

// stockSumHeadRe is the line a stock sender logs as it starts on a file
// at -vvv (match.c:hash_search): b is the block length it got.
var stockSumHeadRe = regexp.MustCompile(`hash search (?:s->)?b(?:length)?=(\d+)`)

func TestBlockSize(t *testing.T) {
	// The stock client passes -B on to a gokrazy daemon, whose generator
	// picks the block length when it receives.
	t.Run("to gokrazy", func(t *testing.T) {
		src, dest, data := deltaTree(t)
		ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
		out, err := stock(t, "-B4096", "-r", "-I", "--stats", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
		if err != nil {
			t.Fatal(err)
		}
		checkDelta(t, dest, data)
		if got := matchedData(t, out); got != blockSizeMatched {
			t.Errorf("Matched data %d bytes, want %d: gokrazy's generator did not use 4 KiB blocks", got, blockSizeMatched)
		}
	})
	// The stock client's own generator picks it; gokrazy's sender follows.
	t.Run("from gokrazy", func(t *testing.T) {
		src, dest, data := deltaTree(t)
		ts := rsynctest.New(t, rsynctest.InteropModule(src))
		out, err := stock(t, "-B4096", "-r", "-I", "--stats", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
		if err != nil {
			t.Fatal(err)
		}
		checkDelta(t, dest, data)
		if got := matchedData(t, out); got != blockSizeMatched {
			t.Errorf("Matched data %d bytes, want %d", got, blockSizeMatched)
		}
	})
	// A gokrazy client's generator picks it when pulling. The stock
	// daemon's sender reports the sum head it got back to the client, as
	// info messages, at -vvv (max verbosity lets a daemon go that far).
	t.Run("gokrazy client pulls", func(t *testing.T) {
		src, dest, data := deltaTree(t)
		addr, _ := startStockDaemonConf(t, src, "  max verbosity = 4\n")
		out, err := gokrazyPull(t, addr, "", dest, "-B4096", "-r", "-I", "-vvv")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		checkDelta(t, dest, data)
		if m := stockSumHeadRe.FindStringSubmatch(out); m == nil || m[1] != "4096" {
			t.Errorf("stock daemon's sender got block length %q, want 4096", m)
		}
	})
	// A gokrazy client passes -B on to the stock daemon when pushing, whose
	// generator reports the block length it picked.
	t.Run("gokrazy client pushes", func(t *testing.T) {
		src, dest, data := deltaTree(t)
		addr, _ := startStockDaemonConf(t, dest, "  read only = false\n  max verbosity = 4\n")
		out, err := gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, "-B4096", "-r", "-I", "-vvv")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		checkDelta(t, dest, data)
		if !stockGenBlockRe.MatchString(out) {
			t.Errorf("stock daemon's generator did not report 4 KiB blocks")
		}
	})
}

// stockGenBlockRe is the sum head a stock generator logs at
// --debug=deltasum (-vv and up).
var stockGenBlockRe = regexp.MustCompile(`blength=4096\b|n=4096\b`)

// --compress-threads is accepted, though zstd here compresses on one
// thread, and it stays on the client, as rsync keeps it.
func TestCompressThreadsAccepted(t *testing.T) {
	src, dest, data := deltaTree(t)
	addr := startStockDaemonModule(t, dest, false)
	if out, err := gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, "-z", "--zt=4", "-r", "-I"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	checkDelta(t, dest, data)
}
