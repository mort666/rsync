package rsyncopts

import (
	"slices"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsyncxattr"
)

func parseGokrazy(t *testing.T, args ...string) (*Options, error) {
	t.Helper()
	osenv := &rsyncos.Env{}
	opts := NewOptionsWithGokrazyDefaults(osenv)
	pc := NewContext(opts)
	err := pc.ParseArguments(osenv, append(args, "src", "dest"))
	return opts, err
}

func TestFakeSuper(t *testing.T) {
	if !rsyncxattr.Supported {
		if _, err := parseGokrazy(t, "--fake-super"); err == nil || !strings.Contains(err.Error(), "extended attributes") {
			t.Errorf("--fake-super without xattrs: %v", err)
		}
		return
	}
	opts, err := parseGokrazy(t, "-a", "--fake-super")
	if err != nil {
		t.Fatal(err)
	}
	if !opts.FakeSuper() || opts.Super() {
		t.Errorf("FakeSuper %v, Super %v; want true, false", opts.FakeSuper(), opts.Super())
	}
	// Not passed on: the remote side takes -M--fake-super for that.
	if slices.Contains(opts.ServerOptions(), "--fake-super") {
		t.Errorf("server options %q carry --fake-super", opts.ServerOptions())
	}
	if _, err := parseGokrazy(t, "-XX", "--fake-super"); err == nil || !strings.Contains(err.Error(), "conflicts with -XX") {
		t.Errorf("-XX --fake-super: %v", err)
	}

	// A daemon module without fake super takes the client's --fake-super
	// as --super; with it, the module's setting wins.
	opts.SetFakeSuper(false)
	if opts.FakeSuper() || !opts.Super() {
		t.Errorf("after SetFakeSuper(false): FakeSuper %v, Super %v; want false, true", opts.FakeSuper(), opts.Super())
	}
	plain, _ := parseGokrazy(t, "-a")
	plain.SetFakeSuper(false)
	if plain.FakeSuper() || plain.Super() {
		t.Errorf("SetFakeSuper(false) without --fake-super turned something on")
	}
	if err := plain.SetFakeSuper(true); err != nil || !plain.FakeSuper() {
		t.Errorf("SetFakeSuper(true) = %v, FakeSuper %v", err, plain.FakeSuper())
	}
	xx, _ := parseGokrazy(t, "-XX")
	if err := xx.SetFakeSuper(true); err == nil {
		t.Errorf("SetFakeSuper(true) with -XX: no error")
	}
}

func TestRemoteOption(t *testing.T) {
	opts, err := parseGokrazy(t, "-a", "-M--fake-super", "--remote-option=--log-file=x")
	if err != nil {
		t.Fatal(err)
	}
	sargv := opts.ServerOptions()
	if got := sargv[len(sargv)-2:]; !slices.Equal(got, []string{"--fake-super", "--log-file=x"}) {
		t.Errorf("server options end in %q, want the -M options", got)
	}
	if opts.FakeSuper() {
		t.Errorf("-M--fake-super turned it on here")
	}
	if _, err := parseGokrazy(t, "-Mfake"); err == nil || !strings.Contains(err.Error(), "must start with a dash") {
		t.Errorf("-Mfake: %v", err)
	}
}
