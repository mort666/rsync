package rsyncd

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/sender"
)

// filesFrom limits a server sender to the names of --files-from: sent by
// the client after its filter list ("-"), or read from a file on this side,
// inside the module. The names are relative to the one source folder
// (flist.c:send_file_list).
func (s *Server) filesFrom(st *sender.Transfer, module *Module, rawRd io.Reader, paths []string) error {
	opts := st.Opts
	if len(paths) != 1 {
		return fmt.Errorf("--files-from takes one source folder, got %q", paths)
	}
	if !strings.HasSuffix(paths[0], "/") {
		paths[0] += "/"
	}
	var names []string
	if name := opts.FilesFrom(); name == "-" {
		// Below protocol 31 the names come unmultiplexed
		// (flist.c:send_file_list, io_end_multiplex_in).
		r := rawRd
		if st.Conn.Session.Protocol >= 31 {
			r = st.Conn.Reader
		}
		lines, err := sender.ReadFilesFromConn(r)
		if err != nil {
			return err
		}
		names = sender.FilesFromNames(lines, true)
	} else {
		data, err := readModuleFile(module, name)
		if err != nil {
			return fmt.Errorf("failed to open files-from file %s: %v", name, err)
		}
		lines, err := opts.ReadFilesFrom(bytes.NewReader(data))
		if err != nil {
			return err
		}
		names = sender.FilesFromNames(lines, false)
	}
	st.Only = names
	st.OnlyRecurse = opts.Recurse()
	return nil
}

// readModuleFile reads name inside a module (a leading / is the module's
// top, as options.c does with module_dirlen), or anywhere for a server
// over a remote shell.
func readModuleFile(module *Module, name string) ([]byte, error) {
	if module.FS != nil {
		return fs.ReadFile(module.FS, strings.TrimPrefix(path.Clean("/"+name), "/"))
	}
	if module.Path == rsync.FileSystemRoot {
		return os.ReadFile(name)
	}
	root, err := os.OpenRoot(module.Path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(strings.TrimPrefix(path.Clean("/"+name), "/"))
}
