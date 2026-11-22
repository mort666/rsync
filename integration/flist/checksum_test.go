//go:build linux

package flist_test

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncclient"
)

// Each strong checksum, forced with --checksum-choice, in both directions
// against stock rsync. Block sums that disagree still copy the file
// correctly, only in full, so these tests check how much data matched.

var matchedRe = regexp.MustCompile(`Matched data: ([0-9,]+) bytes`)

func matchedData(t *testing.T, out string) int {
	t.Helper()
	m := matchedRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no Matched data in rsync output:\n%s", out)
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// deltaTree writes a 200,000-byte file to src and a copy to dest, then
// changes 100 bytes in the middle of src. A delta transfer moves about one
// block; everything else must match.
func deltaTree(t *testing.T) (src, dest string, data []byte) {
	t.Helper()
	src, dest = filepath.Join(t.TempDir(), "src"), t.TempDir()
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	data = make([]byte, 200000)
	rand.Read(data)
	if err := os.WriteFile(filepath.Join(dest, "f"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	data = bytes.Clone(data)
	rand.Read(data[100000:100100])
	if err := os.WriteFile(filepath.Join(src, "f"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return src, dest, data
}

func checkDelta(t *testing.T, dest string, data []byte) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(dest, "f")); err != nil || !bytes.Equal(got, data) {
		t.Errorf("f differs after the transfer (%v)", err)
	}
}

func stockHasChoices(t *testing.T) {
	if v := rsynctest.RsyncVersion(t); !rsynctest.RsyncVersionAtLeast(t, "3.2.0") {
		t.Skipf("rsync %s has no --checksum-choice", v)
	}
}

// stockChecksums are the checksums of rsyncchecksum.Names that the stock
// rsync takes in --checksum-choice: sha1 needs OpenSSL, the SHA-3 sums
// rsync 4.0 and an OpenSSL with them.
func stockChecksums(t *testing.T) []string {
	t.Helper()
	bin := rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, cc := range rsyncchecksum.Names {
		if err := exec.Command(bin, "--cc="+cc, "--list-only", probe).Run(); err != nil {
			t.Logf("stock rsync has no %s checksum (%v)", cc, err)
			continue
		}
		names = append(names, cc)
	}
	return names
}

// sha1 and the SHA-3 sums are negotiated when both sides list them first
// in RSYNC_CHECKSUM_LIST. The SHA-3 sums are offered only that way, or with
// --checksum-choice; rsyncchecksum's tests check they are not by default.
func TestChecksumList(t *testing.T) {
	stockHasChoices(t)
	if p, err := strconv.Atoi(os.Getenv("RSYNC_MATRIX_PROTOCOL")); err == nil && p < 30 {
		t.Skip("nothing is negotiated below protocol 30")
	}
	for _, cc := range []string{"sha3-512", "sha3-384", "sha3-256", "sha1"} {
		t.Run(cc, func(t *testing.T) {
			if !slicesContains(stockChecksums(t), cc) {
				t.Skipf("stock rsync has no %s", cc)
			}
			t.Setenv("RSYNC_CHECKSUM_LIST", cc+" md5")
			for _, dir := range []string{"from gokrazy", "to gokrazy"} {
				src, dest, data := deltaTree(t)
				var out string
				var err error
				if dir == "from gokrazy" {
					ts := rsynctest.New(t, rsynctest.InteropModule(src))
					out, err = stock(t, "--debug=nstr", "-r", "-I", "--stats", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
				} else {
					ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
					out, err = stock(t, "--debug=nstr", "-r", "-I", "--stats", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
				}
				if err != nil {
					t.Fatalf("%s: %v", dir, err)
				}
				if !strings.Contains(out, "negotiated checksum: "+cc) {
					t.Errorf("%s: stock rsync did not negotiate %s", dir, cc)
				}
				if got := matchedData(t, out); got < 190000 {
					t.Errorf("%s: Matched data %d bytes, want nearly all of 200000: the block sums disagree", dir, got)
				}
				checkDelta(t, dest, data)
			}
		})
	}
}

func slicesContains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestChecksumChoiceFromGokrazy(t *testing.T) {
	stockHasChoices(t)
	for _, cc := range stockChecksums(t) {
		t.Run(cc, func(t *testing.T) {
			src, dest, data := deltaTree(t)
			ts := rsynctest.New(t, rsynctest.InteropModule(src))
			out, err := stock(t, "--cc="+cc, "-r", "-I", "--stats", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
			if err != nil {
				t.Fatalf("%v", err)
			}
			if got := matchedData(t, out); got < 190000 {
				t.Errorf("Matched data %d bytes, want nearly all of 200000: the block sums disagree", got)
			}
			checkDelta(t, dest, data)
		})
	}
}

func TestChecksumChoiceToGokrazy(t *testing.T) {
	stockHasChoices(t)
	for _, cc := range stockChecksums(t) {
		t.Run(cc, func(t *testing.T) {
			src, dest, data := deltaTree(t)
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			out, err := stock(t, "--cc="+cc, "-r", "-I", "--stats", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
			if err != nil {
				t.Fatalf("%v", err)
			}
			if got := matchedData(t, out); got < 190000 {
				t.Errorf("Matched data %d bytes, want nearly all of 200000: the block sums disagree", got)
			}
			checkDelta(t, dest, data)
		})
	}
}

// A gokrazy client with --checksum-choice against a stock daemon, both
// ways. Its own counts are not reported, so the stock daemon's log says
// how much matched.
func TestChecksumChoiceGokrazyClient(t *testing.T) {
	stockHasChoices(t)
	for _, cc := range stockChecksums(t) {
		t.Run(cc, func(t *testing.T) {
			src, dest, data := deltaTree(t)
			addr, _ := startStockDaemonConf(t, src, "")
			if out, err := gokrazyPull(t, addr, "", dest, "--cc="+cc, "-r", "-I"); err != nil {
				t.Fatalf("pull: %v\n%s", err, out)
			}
			checkDelta(t, dest, data)

			src, dest, data = deltaTree(t)
			addr = startStockDaemonModule(t, dest, false)
			if out, err := gokrazyRun(t, addr, "mod/", []string{src + "/"}, []rsyncclient.Option{rsyncclient.WithSender()}, "--cc="+cc, "-r", "-I"); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			checkDelta(t, dest, data)
		})
	}
}

// --checksum compares whole-file sums, of the negotiated checksum's length
// (8 bytes for xxh64 and xxh3). A file of the same size and time but other
// content must be sent; one that is the same must not.
func TestAlwaysChecksumEachAlgorithm(t *testing.T) {
	stockHasChoices(t)
	for _, cc := range stockChecksums(t) {
		t.Run(cc, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src")
			dest := t.TempDir()
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string][2]string{"same": {"aaaa", "aaaa"}, "differs": {"bbbb", "cccc"}} {
				for i, dir := range []string{src, dest} {
					p := filepath.Join(dir, name)
					if err := os.WriteFile(p, []byte(content[i]), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(p, oldTime, oldTime); err != nil {
						t.Fatal(err)
					}
				}
			}
			ts := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
			out, err := stock(t, "--cc="+cc, "-rtci", src+"/", "rsync://localhost:"+ts.Port+"/interop/")
			if err != nil {
				t.Fatalf("%v", err)
			}
			if !strings.Contains(out, "differs") || strings.Contains(out, "same") {
				t.Errorf("want only \"differs\" sent:\n%s", out)
			}
			if got, _ := os.ReadFile(filepath.Join(dest, "differs")); string(got) != "bbbb" {
				t.Errorf("differs = %q, want bbbb", got)
			}
		})
	}
}

// Without --checksum-choice, stock rsync 3.2+ and gokrazy/rsync negotiate
// the first name in both lists: xxh128 where stock rsync has xxHash.
func TestChecksumNegotiated(t *testing.T) {
	stockHasChoices(t)
	src, dest, data := deltaTree(t)
	ts := rsynctest.New(t, rsynctest.InteropModule(src))
	out, err := stock(t, "--debug=nstr", "-r", "-I", "--stats", "rsync://localhost:"+ts.Port+"/interop/", dest+"/")
	if err != nil {
		t.Fatalf("%v", err)
	}
	// Below protocol 30 (run-tests.sh -p) nothing is negotiated.
	if !regexp.MustCompile(`Client negotiated checksum: (xxh128|md5)|Client checksum: md[45]`).MatchString(out) {
		t.Errorf("no checksum in stock rsync's output")
	}
	if got := matchedData(t, out); got < 190000 {
		t.Errorf("Matched data %d bytes, want nearly all of 200000", got)
	}
	checkDelta(t, dest, data)
}
