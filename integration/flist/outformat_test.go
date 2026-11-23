//go:build linux

package flist_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// outLines are the lines a run printed with an OUT: format, sorted: the
// delete pass runs at another point in gokrazy (before the transfer,
// where it leaves the same), so the order of deletes differs.
func outLines(out string) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if i := strings.Index(line, "OUT:"); i >= 0 {
			lines = append(lines, line[i:])
		}
	}
	slices.Sort(lines)
	return lines
}

var outFormatArgs = [][]string{
	{"--out-format=OUT:%i:%n%L"},
	{"-ii", "--out-format=OUT:%i:%n%L"},
	{"--out-format=OUT:%n%L"},
	{"--delete", "--out-format=OUT:%i:%n%L"},
	{"--delete", "--out-format=OUT:%n%L"},
	{"-n", "--delete", "--out-format=OUT:%i:%n%L"},
	{"--out-format=OUT:%o:%n:%l:%B"},
}

// TestOutFormat compares what a client prints for each item (-i,
// --out-format) with stock rsync's client in the same direction: the
// gokrazy client's own lines, and those a stock client prints from what a
// gokrazy server reports.
func TestOutFormat(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	if v := rsynctest.RsyncVersion(t); v != "" && v < "3.1" {
		t.Skipf("stock rsync %s itemizes differently", v)
	}
	src := selectionTree(t)
	if err := os.Symlink("small", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "emptydir"), 0o750); err != nil {
		t.Fatal(err)
	}
	// Folder times are pinned: from the clock, whether a folder's item
	// shows a time change would depend on the second each run starts in.
	// The destination's top folder gets the source's time, so that only
	// what the delete pass does to it could change it.
	dirTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for _, dir := range []string{"newdir", "emptydir", "."} {
		if err := os.Chtimes(filepath.Join(src, dir), dirTime, dirTime); err != nil {
			t.Fatal(err)
		}
	}
	prepare := func(t *testing.T, dest string) {
		prepareSelection(t, src, dest)
		if err := os.Chtimes(dest, dirTime, dirTime); err != nil {
			t.Fatal(err)
		}
	}
	type run func(t *testing.T, dest string, args ...string) (string, error)
	pullRef := func(t *testing.T, dest string, args ...string) (string, error) {
		addr, _ := startStockDaemonConf(t, src, "")
		return stock(t, append(args, "rsync://"+addr+"/mod/", dest+"/")...)
	}
	pushRef := func(t *testing.T, dest string, args ...string) (string, error) {
		addr, _ := startStockDaemonConf(t, dest, "  read only = no\n")
		return stock(t, append(args, src+"/", "rsync://"+addr+"/mod/")...)
	}
	for _, dir := range []struct {
		name string
		run  run
		ref  run
	}{
		{"stock to gokrazy", func(t *testing.T, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
		}, pushRef},
		{"gokrazy to stock", func(t *testing.T, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
		}, pullRef},
		{"gokrazy client pulls", func(t *testing.T, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, "")
			return gokrazyPull(t, addr, "", dest, args...)
		}, pullRef},
		{"gokrazy client pushes", func(t *testing.T, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, dest, "  read only = no\n")
			return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}, pushRef},
		{"gokrazy to gokrazy", func(t *testing.T, dest string, args ...string) (string, error) {
			// Neither end is stock here: speak the protocol the stock
			// reference was held to, which decides when --delete runs.
			if p := os.Getenv("RSYNC_MATRIX_PROTOCOL"); p != "" {
				args = append([]string{"--protocol=" + p}, args...)
			}
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}, pushRef},
	} {
		t.Run(dir.name, func(t *testing.T) {
			for _, opt := range outFormatArgs {
				args := append([]string{"-rlpt"}, opt...)
				t.Run(strings.Join(opt, " "), func(t *testing.T) {
					ref := t.TempDir()
					prepare(t, ref)
					refOut, _ := dir.ref(t, ref, args...)
					dest := t.TempDir()
					prepare(t, dest)
					before := snapshot(t, dest)
					out, err := dir.run(t, dest, args...)
					if slices.Contains(args, "-n") {
						// A dry run changes nothing.
						compareSnapshots(t, snapshot(t, dest), before)
					}
					if err != nil && strings.Contains(out, "unknown option") {
						t.Fatalf("%v\n%s", err, out)
					}
					got, want := outLines(out), outLines(refOut)
					if extra, missing := lineDiff(got, want); len(extra)+len(missing) > 0 {
						t.Errorf("printed, and stock did not:\n  %s\nstock printed, and this did not:\n  %s", strings.Join(extra, "\n  "), strings.Join(missing, "\n  "))
					}
				})
			}
		})
	}
}

// lineDiff is what got has more of than want, and the other way round.
func lineDiff(got, want []string) (extra, missing []string) {
	count := make(map[string]int)
	for _, l := range want {
		count[l]++
	}
	for _, l := range got {
		if count[l] > 0 {
			count[l]--
		} else {
			extra = append(extra, l)
		}
	}
	for _, l := range want {
		if count[l] > 0 {
			count[l]--
			missing = append(missing, l)
		}
	}
	return extra, missing
}
