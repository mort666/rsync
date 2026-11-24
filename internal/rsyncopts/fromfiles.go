package rsyncopts

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// readFilterFile reads the rules of --exclude-from or --include-from FILE
// ("-" is stdin), as rsync's parse_filter_file with XFLG_OLD_PREFIXES: one
// pattern a line (a NUL-ended one with an earlier --from0), empty lines and
// ones starting with ';' or '#' skipped, "- " and "+ " keeping their own
// type, "!" clearing the list, and the rest taking prefix.
func (o *Options) readFilterFile(name, prefix string) ([]string, error) {
	var r io.Reader
	if name == "-" {
		if o.osenv == nil || o.osenv.Stdin == nil {
			return nil, fmt.Errorf("no stdin to read %s rules from", strings.TrimSpace(prefix))
		}
		r = o.osenv.Stdin
	} else {
		f, err := os.Open(name)
		if err != nil {
			kind := "ex"
			if prefix == "+ " {
				kind = "in"
			}
			return nil, fmt.Errorf("failed to open %sclude file %s: %w", kind, name, err)
		}
		defer f.Close()
		r = f
	}
	lines, err := splitLines(r, o.eol_nulls != 0)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	var rules []string
	for _, line := range lines {
		switch {
		case line == "" || line[0] == ';' || line[0] == '#':
		case strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "+ ") || line == "!":
			rules = append(rules, line)
		default:
			rules = append(rules, prefix+line)
		}
	}
	return rules, nil
}

// splitLines splits r at NULs, or at CRs and LFs, as rsync reads filter
// and --files-from files.
func splitLines(r io.Reader, nulls bool) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, b := range data {
			if nulls && b == 0 || !nulls && (b == '\n' || b == '\r') {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// FilesFrom is --files-from: a file of names to send, "-" for stdin (on a
// server, the connection), or "host:FILE" for one on the remote side.
func (o *Options) FilesFrom() string { return o.files_from }

// FilesFromRemote says whether --files-from names a file on the remote
// side ("host:FILE", ":FILE", "host::FILE" or "rsync://host/FILE", as
// options.c:check_for_hostspec), and the file's path there.
func (o *Options) FilesFromRemote() (path string, remote bool) {
	s := o.files_from
	if len(s) > 8 && strings.EqualFold(s[:8], "rsync://") {
		rest := s[8:]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return rest[i+1:], true
		}
		return "", true
	}
	i := strings.IndexByte(s, ':')
	if i < 0 || strings.Contains(s[:i], "/") {
		return s, false
	}
	return strings.TrimPrefix(s[i+1:], ":"), true
}

// CheckSenderPaths refuses what a gokrazy sender does not do yet: -R is
// only there under --files-from, which implies it, and there with the
// implied folders.
func (o *Options) CheckSenderPaths() error {
	switch {
	case o.relative_paths == 1 && o.files_from == "":
		return fmt.Errorf("-R (--relative) is not supported yet, except as --files-from implies it")
	case o.files_from != "" && o.relative_paths == 0:
		return fmt.Errorf("--no-relative with --files-from is not supported yet")
	case o.relative_paths == 1 && o.implied_dirs == 0:
		return fmt.Errorf("--no-implied-dirs is not supported yet")
	}
	return nil
}

// FromNulls is --from0: the *-from files hold NUL-ended names.
func (o *Options) FromNulls() bool { return o.eol_nulls != 0 }

// LoadFilesFrom reads a local --files-from file ("-" is stdin), once, so
// a client can read it before a sandbox shuts it out. Its names keep
// comment lines: a receiving client sends them on as they are, as rsync's
// does (io.c:forward_filesfrom_data); a sender skips them.
func (o *Options) LoadFilesFrom() error {
	if o.files_from == "" || o.filesFromLoaded {
		return nil
	}
	if _, remote := o.FilesFromRemote(); remote {
		return nil
	}
	var r io.Reader
	if o.files_from == "-" {
		if o.osenv == nil || o.osenv.Stdin == nil {
			return fmt.Errorf("no stdin to read --files-from names from")
		}
		r = o.osenv.Stdin
	} else {
		f, err := os.Open(o.files_from)
		if err != nil {
			return fmt.Errorf("failed to open files-from file %s: %w", o.files_from, err)
		}
		defer f.Close()
		r = f
	}
	lines, err := splitLines(r, o.FromNulls())
	if err != nil {
		return fmt.Errorf("reading files-from file %s: %w", o.files_from, err)
	}
	for _, l := range lines {
		if l != "" {
			o.filesFromNames = append(o.filesFromNames, l)
		}
	}
	o.filesFromLoaded = true
	return nil
}

// FilesFromNames are the names of a loaded local --files-from file, comment
// lines included.
func (o *Options) FilesFromNames() []string { return o.filesFromNames }

// ReadFilesFrom reads the names of a local --files-from file as a sender
// does (flist.c:send_file_list with RL_DUMP_COMMENTS): one a line, or
// NUL-ended with --from0, skipping empty lines and ones starting with '#'
// or ';'.
func (o *Options) ReadFilesFrom(r io.Reader) ([]string, error) {
	lines, err := splitLines(r, o.FromNulls())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range lines {
		if l == "" || l[0] == '#' || l[0] == ';' {
			continue
		}
		names = append(names, l)
	}
	return names, nil
}
