//go:build linux

package flist_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gokrazy/rsync/internal/rsynctest"
)

// logLines are a --log-file's lines without the time and process ID, and
// with the byte counts of the totals left out: they depend on how each
// end encodes the data.
func logLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d \[\d+\] `)
	counts := regexp.MustCompile(`^sent \d+ bytes  received \d+ bytes  `)
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
		if !prefix.MatchString(line) {
			t.Errorf("log line without time and process ID: %q", line)
		}
		line = counts.ReplaceAllString(prefix.ReplaceAllString(line, ""), "sent ... received ... ")
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return lines
}

// TestLogFile compares a gokrazy client's --log-file with stock rsync's
// for the same pull and push.
func TestLogFile(t *testing.T) {
	rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	if v := rsynctest.RsyncVersion(t); v != "" && v < "3.1" {
		t.Skipf("stock rsync %s logs differently", v)
	}
	src := selectionTree(t)
	// Folder times are pinned, as in TestOutFormat.
	dirTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for _, dir := range []string{"newdir", "."} {
		if err := os.Chtimes(filepath.Join(src, dir), dirTime, dirTime); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"default format", nil},
		{"own format", []string{"--log-file-format=%o %n %l"}},
		{"delete", []string{"--delete"}},
	} {
		for _, push := range []bool{false, true} {
			name := tt.name + "/pull"
			if push {
				name = tt.name + "/push"
			}
			t.Run(name, func(t *testing.T) {
				run := func(t *testing.T, bin, logFile string) {
					dest := t.TempDir()
					prepareSelection(t, src, dest)
					if err := os.Chtimes(dest, dirTime, dirTime); err != nil {
						t.Fatal(err)
					}
					// --force: rsync logs its error messages too, which a
					// gokrazy client does not yet; was-dir would give one.
					args := append([]string{"-rt", "--force", "--log-file=" + logFile}, tt.args...)
					if push {
						addr, _ := startStockDaemonConf(t, dest, "  read only = no\n")
						args = append(args, src+"/", "rsync://"+addr+"/mod/")
					} else {
						addr, _ := startStockDaemonConf(t, src, "")
						args = append(args, "rsync://"+addr+"/mod/", dest+"/")
					}
					if bin == "" {
						rsynctest.Output(t, append([]string{"gokr-rsync"}, args...)...)
					} else if out, err := stock(t, args...); err != nil {
						t.Logf("stock: %v\n%s", err, out)
					}
				}
				dir := t.TempDir()
				refLog, gotLog := filepath.Join(dir, "stock.log"), filepath.Join(dir, "gokrazy.log")
				run(t, "stock", refLog)
				run(t, "", gotLog)
				got, want := logLines(t, gotLog), logLines(t, refLog)
				if extra, missing := lineDiff(got, want); len(extra)+len(missing) > 0 {
					t.Errorf("logged, and stock did not:\n  %s\nstock logged, and this did not:\n  %s", strings.Join(extra, "\n  "), strings.Join(missing, "\n  "))
				}
			})
		}
	}
}
