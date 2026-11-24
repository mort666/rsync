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

// fromFilesTree makes the source for TestFromFiles, with a list inside it
// for the cases that name a --files-from on the sending side.
func fromFilesTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, content := range map[string]string{
		"a.txt":       "a",
		"top.txt":     "top",
		"d/b.txt":     "b",
		"d/g.txt":     "g",
		"d/e/f.txt":   "f",
		"other/h.txt": "h",
		"keep/k.txt":  "k",
		"lists/names": "d/e/f.txt\na.txt\n",
	} {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// fromFilesLists writes the lists the cases name, outside the source.
func fromFilesLists(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	lists := map[string]string{
		// A nested file, a folder, a comment line, a CRLF line and a
		// missing one. (A name climbing out, "../top.txt", is sent as
		// "top.txt", which a stock client pulling refuses as unrequested,
		// from a stock daemon too; TestFilesFromNames has it.)
		"LIST":  "a.txt\nd/e/f.txt\n# a comment\nd\ntop.txt\r\nmissing\n",
		"LIST0": "a.txt\x00d/e/f.txt\x00d\x00",
		"EX":    "# comment\n; comment\n\ng.txt\n+ keep/\n- other\n",
		"EX0":   "g.txt\x00- other\x00",
		"IN":    "b.txt\n*/\n- *\n",
	}
	paths := map[string]string{}
	for name, content := range lists {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[name] = p
	}
	return paths
}

var fromFilesArgs = [][]string{
	{"-t", "--files-from=LIST"},
	{"-rt", "--files-from=LIST"},
	{"-a", "--files-from=LIST"},
	{"-t", "--from0", "--files-from=LIST0"},
	{"-rt", "--from0", "--files-from=LIST0"},
	{"-rt", "--exclude-from=EX"},
	{"-rt", "--from0", "--exclude-from=EX0"},
	{"-rt", "--include-from=IN"},
	{"-rt", "--exclude-from=EX", "--files-from=LIST"},
}

// TestFromFiles compares what --files-from, --exclude-from and
// --include-from leave with stock rsync's own local run, in all five
// directions and a gokrazy client pulling from a gokrazy daemon, which
// has both ends of the names sent over the connection.
func TestFromFiles(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	src := fromFilesTree(t)
	lists := fromFilesLists(t)
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
		{"gokrazy client pulls from gokrazy", func(t *testing.T, dest string, args ...string) (string, error) {
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{dest}, nil, args...)
		}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			for _, opt := range fromFilesArgs {
				args := make([]string, len(opt))
				for i, a := range opt {
					for name, p := range lists {
						a = strings.Replace(a, "="+name, "="+p, 1)
					}
					args[i] = a
				}
				t.Run(strings.Join(opt, " "), func(t *testing.T) {
					// The missing name ends the runs with an error by
					// design; only what they leave counts.
					ref := t.TempDir()
					stock(t, append(args, src+"/", ref+"/")...)
					dest := t.TempDir()
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

// TestFilesFromRemote has the --files-from list on the sending side
// (":FILE"), read there inside the module, when a client pulls; and checks
// that a push asking for that is refused rather than left hanging.
func TestFilesFromRemote(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	src := fromFilesTree(t)
	for _, extra := range [][]string{{"-t"}, {"-rt"}} {
		args := append(extra, "--files-from=:lists/names")
		ref := t.TempDir()
		if out, err := stock(t, append(extra, "--files-from="+filepath.Join(src, "lists/names"), src+"/", ref+"/")...); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		want := snapshot(t, ref)
		for _, dir := range []struct {
			name string
			run  func(t *testing.T, dest string) (string, error)
		}{
			{"gokrazy to stock", func(t *testing.T, dest string) (string, error) {
				ts := rsynctest.New(t, rsynctest.InteropModule(src))
				return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
			}},
			{"gokrazy client pulls", func(t *testing.T, dest string) (string, error) {
				addr, _ := startStockDaemonConf(t, src, "")
				return gokrazyPull(t, addr, "", dest, args...)
			}},
			{"gokrazy client pulls from gokrazy", func(t *testing.T, dest string) (string, error) {
				ts := rsynctest.New(t, rsynctest.InteropModule(src))
				return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{dest}, nil, args...)
			}},
		} {
			t.Run(strings.Join(extra, " ")+"/"+dir.name, func(t *testing.T) {
				dest := t.TempDir()
				if out, err := dir.run(t, dest); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				compareSnapshots(t, snapshot(t, dest), want)
			})
		}
	}

	// Pushing with the list on the receiving side needs the receiver to
	// send it back; gokrazy refuses both ends of that for now.
	t.Run("stock push to gokrazy refused", func(t *testing.T) {
		dest := t.TempDir()
		os.MkdirAll(filepath.Join(dest, "lists"), 0o755)
		os.WriteFile(filepath.Join(dest, "lists/names"), []byte("a.txt\n"), 0o644)
		ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
		out, err := stock(t, "-t", "--files-from=:lists/names", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
		if err == nil {
			t.Fatalf("the push went through, want a refusal:\n%s", out)
		}
		// Below protocol 31 a stock sender reads the list from the
		// connection unmultiplexed (main.c:client_run), so any message
		// from the server, this refusal too, reaches it as garbage.
		if stockDaemonProtocol(stockMaxProtocol(t)) >= 31 && !strings.Contains(out, "not supported yet") {
			t.Errorf("err %v, want the refusal shown:\n%s", err, out)
		}
	})
	t.Run("gokrazy push refused", func(t *testing.T) {
		addr, _ := startStockDaemonConf(t, t.TempDir(), "  read only = no\n")
		out, err := gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, "-t", "--files-from=:lists/names")
		if err == nil || !strings.Contains(err.Error()+out, "not supported yet") {
			t.Errorf("err %v, want a refusal:\n%s", err, out)
		}
	})
}
