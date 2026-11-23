//go:build linux

package flist_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// selectionTree makes the source for TestSelection.
func selectionTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	now := time.Now().Truncate(time.Second)
	for name, content := range map[string]string{
		"same-size":   "AAAA",
		"changed":     "new content",
		"dest-newer":  "source",
		"near-time":   "SRCX",
		"was-dir":     "a file now",
		"new-file":    "new",
		"newdir/file": "in a new folder",
		"big":         strings.Repeat("b", 100),
		"small":       "small",
	} {
		full := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// prepareSelection gives dest what makes each option matter.
func prepareSelection(t *testing.T, src, dest string) {
	t.Helper()
	at := func(name string) time.Time {
		st, err := os.Stat(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		return st.ModTime()
	}
	put := func(name, content string, mtime time.Time) {
		t.Helper()
		full := filepath.Join(dest, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	put("same-size", "BBBB", at("same-size").Add(-time.Hour))    // --size-only keeps it
	put("changed", "old", at("changed").Add(-time.Hour))         // --ignore-existing keeps it
	put("dest-newer", "newer!", at("dest-newer").Add(time.Hour)) // -u keeps it
	put("near-time", "DSTX", at("near-time").Add(2*time.Second)) // --modify-window=5 keeps it
	put("big", "old big", at("big").Add(-time.Hour))
	put("small", "old", at("small").Add(-time.Hour))
	put("was-dir/inner", "in the way", at("was-dir")) // --force removes it
	for _, name := range []string{"extra1", "extra2", "extra3"} {
		put(name, "not in the source", at("big")) // --delete, --max-delete
	}
}

var selectionArgs = [][]string{
	{"--size-only"},
	{"--existing"},
	{"--ignore-existing"},
	{"--max-size=10"},
	{"--min-size=10"},
	{"-u"},
	{"--modify-window=5"},
	{"--delete"},
	{"--delete", "--max-delete=1"},
	{"--delete", "--max-delete=0"},
	{"--delete-before"},
	{"--delete-during"},
	{"--delete-delay"},
	{"--delete-after"},
	{"--exclude=big"},
	{"--delete", "--exclude=extra*"},
	// What an exclude rule matches stays, unless --delete-excluded.
	{"--delete", "--exclude=extra2"},
	{"--delete-excluded", "--exclude=extra2"},
	{"--delete-excluded", "--exclude=big"},
	{"--force"},
	{"--timeout=30"},
	{"-g", "--numeric-ids"},
}

// TestSelection compares what each of the options that pick files, and
// the delete limits, leave behind with stock rsync's own local run from
// the same destination, in all five directions.
func TestSelection(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	src := selectionTree(t)
	type run func(t *testing.T, dest string, args ...string) (string, error)
	for _, dir := range []struct {
		name string
		run  run
	}{
		{"stock to gokrazy", func(t *testing.T, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
		}},
		{"gokrazy to stock", func(t *testing.T, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
		}},
		{"gokrazy client pulls", func(t *testing.T, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, "")
			return gokrazyPull(t, addr, "", dest, args...)
		}},
		{"gokrazy client pushes", func(t *testing.T, dest string, args ...string) (string, error) {
			addr, _ := startStockDaemonConf(t, dest, "  read only = no\n")
			return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
		{"gokrazy to gokrazy", func(t *testing.T, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			for _, opt := range selectionArgs {
				args := append([]string{"-rt"}, opt...)
				t.Run(strings.Join(opt, " "), func(t *testing.T) {
					// Some of these end with errors by design (a folder in
					// the way without --force, the delete limit); only what
					// they leave counts.
					ref := t.TempDir()
					prepareSelection(t, src, ref)
					stock(t, append(args, src+"/", ref+"/")...)
					dest := t.TempDir()
					prepareSelection(t, src, dest)
					out, err := dir.run(t, dest, args...)
					if err != nil && strings.Contains(out, "unknown option") {
						t.Fatalf("%v\n%s", err, out)
					}
					compareSnapshots(t, snapshot(t, dest), snapshot(t, ref))
				})
			}
		})
	}
}

// TestRemoveSourceFiles compares what --remove-source-files leaves on both
// sides with stock rsync's own run: the sent files gone from the source,
// one the destination already had too, the folders kept. The daemon
// directions need a writable module to remove from.
func TestRemoveSourceFiles(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	make := func(t *testing.T) (src, dest string) {
		src, dest = t.TempDir(), t.TempDir()
		for _, d := range []string{src, dest} {
			if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		for name, content := range map[string]string{"a": "a", "sub/b": "b", "had": "already there"} {
			p := filepath.Join(src, name)
			os.WriteFile(p, []byte(content), 0o644)
			os.Chtimes(p, old, old)
		}
		os.WriteFile(filepath.Join(dest, "had"), []byte("already there"), 0o644)
		os.Chtimes(filepath.Join(dest, "had"), old, old)
		return src, dest
	}
	args := []string{"-rt", "--remove-source-files"}
	refSrc, refDest := make(t)
	if out, err := stock(t, append(args, refSrc+"/", refDest+"/")...); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantSrc, wantDest := snapshot(t, refSrc), snapshot(t, refDest)
	if _, ok := wantSrc["a"]; ok {
		t.Fatalf("stock rsync left the source: %v", wantSrc)
	}
	for _, dir := range []struct {
		name string
		run  func(t *testing.T, src, dest string) (string, error)
	}{
		{"stock to gokrazy", func(t *testing.T, src, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
		}},
		{"gokrazy to stock", func(t *testing.T, src, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(src))
			return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
		}},
		{"gokrazy client pulls", func(t *testing.T, src, dest string) (string, error) {
			addr, _ := startStockDaemonConf(t, src, "  read only = no\n")
			return gokrazyPull(t, addr, "", dest, args...)
		}},
		{"gokrazy client pushes", func(t *testing.T, src, dest string) (string, error) {
			addr, _ := startStockDaemonConf(t, dest, "  read only = no\n")
			return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
		{"gokrazy to gokrazy", func(t *testing.T, src, dest string) (string, error) {
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			src, dest := make(t)
			if out, err := dir.run(t, src, dest); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			compareSnapshots(t, snapshot(t, src), wantSrc)
			compareSnapshots(t, snapshot(t, dest), wantDest)
		})
	}

	// A read-only module may not have its files removed.
	t.Run("read-only module", func(t *testing.T) {
		src, dest := make(t)
		ts := rsynctest.New(t, rsynctest.InteropModule(src))
		out, err := stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
		if err == nil || !strings.Contains(out, "read-only module") {
			t.Errorf("err %v, want a read-only module refusal:\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(src, "a")); err != nil {
			t.Errorf("source removed from a read-only module: %v", err)
		}
	})
}
