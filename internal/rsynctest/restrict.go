package rsynctest

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gokrazy/rsync/internal/restrict"
	"github.com/landlock-lsm/go-landlock/landlock"
)

func init() {
	restrict.ExtraHook = func() []landlock.Rule {
		rules := []landlock.Rule{
			// contains /usr/bin/rsync (and library deps)
			landlock.RODirs("/usr"),
			landlock.RODirs("/nix").IgnoreIfMissing(),

			// for t.TempDir()
			landlock.RWDirs(os.TempDir()).WithRefer(),

			// used in some of our test code
			landlock.RWFiles("/dev/null"),
		}
		// Tests run against whichever rsync comes first on PATH, which is not
		// always under /usr (testdata/rsync-matrix puts pinned builds first).
		// Allow its directory too, or the sandboxed test process cannot
		// execute it.
		if p, err := exec.LookPath("rsync"); err == nil {
			if p, err := filepath.EvalSymlinks(p); err == nil {
				if p, err := filepath.Abs(p); err == nil {
					rules = append(rules, landlock.RODirs(filepath.Dir(p)))
				}
			}
		}
		// testdata/rsync-matrix/run-tests.sh -p puts a wrapper script first
		// on PATH, which runs a build from another directory.
		for _, dir := range filepath.SplitList(os.Getenv("RSYNCTEST_EXTRA_RODIRS")) {
			if dir != "" {
				rules = append(rules, landlock.RODirs(dir))
			}
		}
		return rules
	}
}
