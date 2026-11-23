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

// TestClientFilters checks that filter rules work as in rsync: a gokrazy
// client's own, which protect what they exclude from --delete when it
// receives and decide what goes in the file list when it sends, and a
// stock client's, which a gokrazy daemon applies. An excluded folder's
// contents are protected too, and the first matching rule decides. Each
// case is compared with stock rsync's own local run from the same
// destination.
func TestClientFilters(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	put := func(t *testing.T, dir string, names ...string) {
		t.Helper()
		for _, name := range names {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
			os.Chtimes(p, mtime, mtime)
		}
	}
	src := t.TempDir()
	put(t, src, "a", "big", "logs/x.log", "logs/keep.txt")
	prepare := func(t *testing.T) string {
		dest := t.TempDir()
		put(t, dest, "keep", "extra", "logs/old.log", "logs/old.txt")
		return dest
	}
	for _, args := range [][]string{
		{"--delete", "--exclude=keep"},
		{"--delete", "--exclude=*.log"},
		{"--delete", "--exclude=keep", "--exclude=logs/"},
		{"--exclude=big"},
		{"--include=*.txt", "--exclude=logs/*"},
	} {
		args := append([]string{"-rt"}, args...)
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			ref := prepare(t)
			stock(t, append(args, src+"/", ref+"/")...)
			want := snapshot(t, ref)
			for _, dir := range []struct {
				name string
				run  func(t *testing.T, dest string) (string, error)
			}{
				// A gokrazy daemon applies the rules a stock client sends.
				{"stock client pulls from gokrazy", func(t *testing.T, dest string) (string, error) {
					ts := rsynctest.New(t, rsynctest.InteropModule(src))
					return stock(t, append(args, "rsync://localhost:"+ts.Port+"/interop/", dest+"/")...)
				}},
				{"stock client pushes to gokrazy", func(t *testing.T, dest string) (string, error) {
					ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
					return stock(t, append(args, src+"/", "rsync://localhost:"+ts.Port+"/interop/")...)
				}},
				{"gokrazy client pulls from stock", func(t *testing.T, dest string) (string, error) {
					addr, _ := startStockDaemonConf(t, src, "")
					return gokrazyPull(t, addr, "", dest, args...)
				}},
				{"gokrazy client pulls from gokrazy", func(t *testing.T, dest string) (string, error) {
					ts := rsynctest.New(t, rsynctest.InteropModule(src))
					return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{dest}, nil, args...)
				}},
				{"gokrazy client pushes to stock", func(t *testing.T, dest string) (string, error) {
					addr, _ := startStockDaemonConf(t, dest, "  read only = no\n")
					return gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
				}},
				{"gokrazy client pushes to gokrazy", func(t *testing.T, dest string) (string, error) {
					ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
					return gokrazyRun(t, "localhost:"+ts.Port, "interop/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, args...)
				}},
			} {
				t.Run(dir.name, func(t *testing.T) {
					dest := prepare(t)
					if out, err := dir.run(t, dest); err != nil {
						t.Fatalf("%v\n%s", err, out)
					}
					compareSnapshots(t, snapshot(t, dest), want)
				})
			}
		})
	}
}
