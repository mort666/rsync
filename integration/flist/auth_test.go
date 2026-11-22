//go:build linux

package flist_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncd"
)

// Daemon authentication: the digest is MD4 with a zero seed below protocol
// 30, MD5 from 30, and from rsync 3.2.7 the strongest one both greetings
// list (sha512). A wrong digest fails as a wrong password would.

func authTree(t *testing.T) (dir, secrets string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("secret data"), 0o644); err != nil {
		t.Fatal(err)
	}
	secrets = filepath.Join(t.TempDir(), "secrets")
	if err := os.WriteFile(secrets, []byte("alice:sesame\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, secrets
}

// stockEnv is stock, with extra environment variables.
func stockEnv(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	bin := rsynctest.TridgeOrGTFO(t, "compares with stock rsync")
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	t.Logf("%s %s\n%s", bin, strings.Join(args, " "), buf.String())
	return buf.String(), err
}

func stockNegotiates() bool {
	out, err := exec.Command("rsync", "--version").Output()
	if err != nil {
		return false
	}
	// rsync 3.2.7 and newer list auth digests in their greetings.
	for _, old := range []string{"version 2.", "version 3.0.", "version 3.1.", "version 3.2.0", "version 3.2.1", "version 3.2.2", "version 3.2.3", "version 3.2.4", "version 3.2.5", "version 3.2.6"} {
		if strings.Contains(string(out), old) {
			return false
		}
	}
	return true
}

func TestAuthToGokrazy(t *testing.T) {
	dir, secrets := authTree(t)
	ts := rsynctest.New(t, []rsyncd.Module{{Name: "mod", Path: dir, AuthUsers: []string{"alice"}, SecretsFile: secrets}})
	protocols(t, func(t *testing.T, proto int) {
		url := "rsync://alice@localhost:" + ts.Port + "/mod/"
		args := []string{fmt.Sprintf("--protocol=%d", proto), "-r"}
		if stockNegotiates() {
			args = append(args, "--debug=nstr")
		}
		dest := t.TempDir()
		out, err := stockEnv(t, []string{"RSYNC_PASSWORD=sesame"}, append(args, url, dest+"/")...)
		if err != nil {
			t.Fatalf("right password: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(dest, "f")); err != nil || string(got) != "secret data" {
			t.Errorf("f: %q, %v", got, err)
		}
		if stockNegotiates() && !strings.Contains(out, "negotiated daemon auth checksum: sha512") {
			t.Errorf("stock rsync did not negotiate sha512")
		}
		if _, err := stockEnv(t, []string{"RSYNC_PASSWORD=wrong"}, append(args, url, t.TempDir()+"/")...); err == nil {
			t.Errorf("wrong password: no error")
		}
	})
}

func TestAuthFromGokrazy(t *testing.T) {
	dir, secrets := authTree(t)
	addr, _ := startStockDaemonConf(t, dir, "  read only = yes\n  auth users = alice\n  secrets file = "+secrets+"\n")
	t.Setenv("RSYNC_USERNAME", "alice")
	pass := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(pass, []byte("sesame\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(t.TempDir(), "wrong")
	if err := os.WriteFile(wrong, []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	protocols(t, func(t *testing.T, proto int) {
		dest := t.TempDir()
		out, err := gokrazyPull(t, addr, "", dest, fmt.Sprintf("--protocol=%d", proto), "-r", "--password-file="+pass, "--debug=nstr")
		if err != nil {
			t.Fatalf("right password: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(dest, "f")); err != nil || string(got) != "secret data" {
			t.Errorf("f: %q, %v", got, err)
		}
		// Without digest lists it is MD5 from protocol 30 and MD4 below,
		// by the version both sides end up with, which run-tests.sh -p
		// can force below proto.
		want := regexp.MustCompile(`negotiated daemon auth checksum: md[45]\b`)
		if stockNegotiates() {
			want = regexp.MustCompile(`negotiated daemon auth checksum: sha512`)
		}
		if !want.MatchString(out) {
			t.Errorf("gokrazy client did not negotiate %s:\n%s", want, out)
		}
		if _, err := gokrazyPull(t, addr, "", t.TempDir(), fmt.Sprintf("--protocol=%d", proto), "-r", "--password-file="+wrong); err == nil {
			t.Errorf("wrong password: no error")
		}
	})
}
