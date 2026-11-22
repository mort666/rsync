package rsyncopts

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestAltDest(t *testing.T) {
	opts, err := parseGokrazy(t, "--link-dest=../a", "--link-dest", "/b")
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.BasisDirs(); !slices.Equal(got, []string{"../a", "/b"}) || opts.AltDestType() != LinkDest {
		t.Errorf("BasisDirs %q, AltDestType %d", got, opts.AltDestType())
	}
	// Only a receiving server needs them.
	if slices.Contains(opts.ServerOptions(), "--link-dest") {
		t.Errorf("receiving client passes them on: %q", opts.ServerOptions())
	}
	opts.SetSender()
	if got := strings.Join(opts.ServerOptions(), " "); !strings.Contains(got, "--link-dest ../a --link-dest /b") {
		t.Errorf("sending client's server options %q", got)
	}

	if _, err := parseGokrazy(t, "--link-dest=a", "--copy-dest=b"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("--link-dest with --copy-dest: %v", err)
	}
	var many []string
	for i := range MaxBasisDirs + 1 {
		many = append(many, fmt.Sprintf("--compare-dest=d%d", i))
	}
	if _, err := parseGokrazy(t, many...); err == nil || !strings.Contains(err.Error(), "at most 20") {
		t.Errorf("21 folders: %v", err)
	}
}

func TestLinkServerOptions(t *testing.T) {
	// A receiving client asks the sender to follow links; a sending one
	// follows them itself and tells the receiver to keep folder links.
	recv, err := parseGokrazy(t, "-rLkK", "-H", "--safe-links", "--copy-unsafe-links")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(recv.ServerOptions(), " ")
	for _, want := range []string{"L", "k", "H", "--safe-links", "--copy-unsafe-links"} {
		if !strings.Contains(got, want) {
			t.Errorf("receiving client's server options %q lack %s", got, want)
		}
	}
	if strings.Contains(strings.Fields(got)[2], "K") {
		t.Errorf("receiving client passes -K: %q", got)
	}
	recv.SetSender()
	got = strings.Join(recv.ServerOptions(), " ")
	if flags := strings.Fields(got)[1]; !strings.Contains(flags, "K") || strings.ContainsAny(flags, "Lk") {
		t.Errorf("sending client's flags %q, want K and no L or k", flags)
	}
}
