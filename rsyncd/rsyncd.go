// Package rsyncd implements an rsync server (only), but note that gokrazy/rsync
// contains a native Go rsync implementation that supports sending and receiving
// files as client or server, compatible with the original tridge rsync (from
// the samba project) or openrsync (used on OpenBSD and macOS 15+).
package rsyncd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/progress"
	"github.com/gokrazy/rsync/internal/receiver"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsynctoken"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/sender"
	"github.com/mmcloughlin/md4"
)

type Module struct {
	Name        string   `toml:"name"`
	Path        string   `toml:"path"` // If empty, FS must be non-nil
	FS          fs.FS    `toml:"-"`    // If set, serve from this instead of Path
	ACL         []string `toml:"acl"`
	Writable    bool     `toml:"writable"`     // Must be false if FS is set
	AuthUsers   []string `toml:"auth_users"`   // Usernames allowed to connect; empty means no auth
	SecretsFile string   `toml:"secrets_file"` // Path to file with user:password lines
	// FakeSuper keeps owners, devices, special files, symlinks and
	// permission bits the daemon cannot set itself in the user.rsync.%stat
	// xattr, as rsyncd.conf's "fake super" does. Without it, a client's
	// --fake-super is taken as --super. Linux only.
	FakeSuper bool `toml:"fake_super"`
}

// Option specifies the server options.
type Option interface {
	applyServer(*Server)
}

type serverOptionFunc func(server *Server)

func (f serverOptionFunc) applyServer(s *Server) {
	f(s)
}

// WithLogger specifies the logger to use for the server.
// It also sets the global logger used by the rsync package.
func WithLogger(logger log.Logger) Option {
	return serverOptionFunc(func(s *Server) {
		s.logger = logger
	})
}

func WithStderr(stderr io.WriteCloser) Option {
	return serverOptionFunc(func(s *Server) {
		s.stderr = stderr
	})
}

// WithTrace logs the frames and bytes of every connection at the given
// --debug=io level (see rsyncwire.TraceFrames and friends). A client's own
// --debug=io raises the level for its connection, but only from the point
// where the server has read the client's arguments.
func WithTrace(level int) Option {
	return serverOptionFunc(func(s *Server) {
		s.traceLevel = level
	})
}

func DontRestrict() Option {
	return serverOptionFunc(func(s *Server) {
		s.dontRestrict = true
	})
}

func NewServer(modules []Module, opts ...Option) (*Server, error) {
	for _, mod := range modules {
		if err := validateModule(mod); err != nil {
			return nil, err
		}
	}

	server := &Server{
		modules: modules,
	}

	for _, opt := range opts {
		opt.applyServer(server)
	}

	// Default to os.Stderr if no stderr was specified.
	// Explicitly use io.Discard if you do not want stderr.
	if server.stderr == nil {
		server.stderr = os.Stderr
	}

	if server.logger == nil {
		// TODO: use the logger in a *rsyncos.Env instead
		server.logger = log.New(server.stderr)
	}

	// An empty module list means this server is a sender
	// (e.g. started in command mode with --server --sender),
	// in which case restrict.MaybeFileSystem() will be called
	// by the caller of NewServer().
	if !server.dontRestrict && len(server.modules) > 0 {
		if err := restrictToModules(server.modules); err != nil {
			return nil, err
		}
	}

	return server, nil
}

type Server struct {
	stderr       io.WriteCloser
	logger       log.Logger
	dontRestrict bool
	traceLevel   int

	modules []Module
}

// traceConn returns the trace for conn at level (or the server's own level,
// if higher), and makes conn log its raw bytes if that level asks for it.
func (s *Server) traceConn(conn *Conn, level int) *rsyncwire.Trace {
	trace := rsyncwire.NewTrace(s.logger.Printf, "server", max(level, s.traceLevel))
	if !conn.traced && trace != nil && trace.Level >= rsyncwire.TraceRaw {
		conn.crd.R = trace.WrapReader(conn.crd.R)
		conn.cwr.W = trace.WrapWriter(conn.cwr.W)
		conn.traced = true
	}
	return trace
}

func (s *Server) getModule(requestedModule string) (Module, error) {
	for _, mod := range s.modules {
		if mod.Name == requestedModule {
			return mod, nil
		}
	}

	return Module{}, fmt.Errorf("no such module: %s", requestedModule)
}

func (s *Server) formatModuleList() string {
	if len(s.modules) == 0 {
		return ""
	}
	var list strings.Builder
	for _, mod := range s.modules {
		comment := mod.Name // for now
		fmt.Fprintf(&list, "%s\t%s\n",
			mod.Name,
			comment)
	}
	return list.String()
}

func checkACL(acls []string, remoteAddr string) error {
	if len(acls) == 0 {
		return nil
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return fmt.Errorf("BUG: invalid remote address %q", remoteAddr)
	}
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return fmt.Errorf("BUG: invalid remote host %q", host)
	}
	for _, acl := range acls {
		// TODO(performance): move ACL parsing to config-time to make ACL checks
		// less expensive
		i := strings.Index(acl, " ")
		if i < 0 {
			return fmt.Errorf("invalid acl: %q (no space found)", acl)
		}
		action, who := acl[:i], acl[i+len(" "):]
		if action != "allow" && action != "deny" {
			return fmt.Errorf("invalid acl: %q (syntax: allow|deny <all|ipnet>)", acl)
		}
		if who == "all" {
			// The all keyword matches any remote IP address
		} else {
			_, net, err := net.ParseCIDR(who)
			if err != nil {
				return fmt.Errorf("invalid acl: %q (syntax: allow|deny <all|ipnet>)", acl)
			}
			if !net.Contains(remoteIP) {
				// Skip this instruction, the remote IP does not match
				continue
			}
		}
		switch action {
		case "allow":
			return nil
		case "deny":
			return fmt.Errorf("access denied (acl %q)", acl)
		default:
			return fmt.Errorf("invalid acl: %q (syntax: allow|deny <all|ipnet>)", acl)
		}
	}
	return nil
}

func (s *Server) HandleDaemonConn(ctx context.Context, conn *Conn) (err error) {
	stop := context.AfterFunc(ctx, func() {
		// Pending Read or Write calls will return with an error.
		conn.closer.Close()
	})
	defer stop()

	conn.handshakeDeadline(true)

	const terminationCommand = "@RSYNCD: OK\n"
	trace := s.traceConn(conn, 0)
	cwr := conn.cwr
	rd := conn.rd
	// send server greeting

	io.WriteString(cwr, rsyncwire.Greeting(rsyncwire.MaxProtocolVersion, rsyncchecksum.AuthDigestList()))

	// read client greeting
	clientGreeting, err := rd.ReadSlice('\n')
	if err != nil {
		return err
	}
	// From protocol 30 the greeting reads "31.0", from rsync 3.2.7 on
	// followed by the client's auth digests.
	remoteProtocol, clientDigests, err := rsyncwire.ParseGreeting(string(clientGreeting))
	if err != nil {
		if strings.Contains(err.Error(), "omitted") {
			fmt.Fprintf(cwr, "@ERROR: your client %s\n", strings.TrimPrefix(err.Error(), "the peer "))
		} else {
			io.WriteString(cwr, "@ERROR: protocol startup error\n")
		}
		return fmt.Errorf("invalid client greeting: %v", err)
	}
	// handleConn negotiates again once the client's --protocol is known.
	// Checking now refuses a client we cannot talk to before it picks a
	// module.
	greeted, err := rsyncwire.NegotiateProtocol("Server", rsyncwire.MaxProtocolVersion, remoteProtocol)
	if err != nil {
		fmt.Fprintf(cwr, "@ERROR: %v\n", err)
		return err
	}

	// read requested module(s), if any
	line, err := rd.ReadSlice('\n')
	if err != nil {
		return err
	}
	requestedModule := strings.TrimSpace(string(line))
	if requestedModule == "" || requestedModule == "#list" {
		s.logger.Printf("client %v requested rsync module listing", conn.name)
		io.WriteString(cwr, s.formatModuleList())
		io.WriteString(cwr, "@RSYNCD: EXIT\n")
		return nil
	}
	s.logger.Printf("client %v requested rsync module %q", conn.name, requestedModule)
	module, err := s.getModule(requestedModule)
	if err != nil {
		fmt.Fprintf(cwr, "@ERROR: Unknown module %q\n", requestedModule)
		return err
	}

	if err := checkACL(module.ACL, conn.name); err != nil {
		fmt.Fprintf(cwr, "@ERROR: %v\n", err)
		return err
	}

	if len(module.AuthUsers) > 0 {
		if err := s.authServer(rd, cwr, &module, conn.name, greeted, clientDigests); err != nil {
			fmt.Fprintf(cwr, "@ERROR: auth failed on module %s\n", module.Name)
			return err
		}
	}

	io.WriteString(cwr, terminationCommand)

	// read requested flags: from protocol 30 each ends in a NUL, before it
	// in a newline (clientserver.c rl_nulls)
	eol := byte('\n')
	if greeted >= 30 {
		eol = 0
	}
	var flags []string
	for {
		line, err := rd.ReadSlice(eol)
		if err != nil {
			return err
		}
		// Only the line end goes: a path may start or end with a space.
		flag := strings.TrimRight(string(line), "\r\n\x00")
		if eol == 0 {
			flag = string(line[:len(line)-1])
		}
		s.logger.Printf("client sent: %q", flag)
		if flag == "" {
			break
		}
		const maxArgs = 1000 // matches rsync.h:MAX_ARGS
		if len(flags) >= maxArgs {
			return fmt.Errorf("too many args")
		}
		flags = append(flags, flag)
	}
	conn.handshakeDeadline(false)

	s.logger.Printf("flags: %+v", flags)
	osenv := &rsyncos.Env{Stderr: s.stderr}
	pc := rsyncopts.NewContext(rsyncopts.NewOptionsWithGokrazyDefaults(osenv))
	err = pc.ParseArguments(osenv, flags)
	if err == nil {
		// Refuse what this server cannot do before the protocol starts,
		// so the client hears why.
		err = rsynctoken.CheckChoice(pc.Options.CompressChoice())
	}
	if err == nil {
		err = pc.Options.SetFakeSuper(module.FakeSuper)
	}
	if err != nil {
		err = fmt.Errorf("parsing server args: %v", err)

		// terminate connection with an error about which flag is not supported
		c := &rsyncwire.Conn{
			Reader:  io.NopCloser(rd),
			Writer:  cwr,
			Session: rsyncwire.Session{Protocol: greeted},
		}

		// The client reads the rest of the protocol setup before the
		// error: compat flags from protocol 30, then the seed.
		if err := c.WriteCompatFlags(""); err != nil {
			return err
		}
		const errorSeed = 0xee
		if err := c.WriteInt32(errorSeed); err != nil {
			return err
		}

		// Switch to multiplexing protocol, but only for server-side transmissions.
		// Transmissions received from the client are not multiplexed.
		mpx := &rsyncwire.MultiplexWriter{Writer: c.Writer, Trace: trace}
		mpx.WriteMsg(rsyncwire.MsgErrorXfer, fmt.Appendf(nil, "gokr-rsync [sender]: %v\n", err))

		return err
	}
	remaining := pc.RemainingArgs
	s.logger.Printf("remaining: %q", remaining)
	// remaining[0] is always "."
	// remaining[1] is the first directory
	if len(remaining) < 2 {
		return fmt.Errorf("invalid args: at least one directory required")
	}
	if got, want := remaining[0], "."; got != want {
		return fmt.Errorf("protocol error: got %q, expected %q", got, want)
	}
	paths := remaining[1:]
	s.logger.Printf("paths: %q", paths)

	// Strip the module_name/ prefix out of the paths and expand them as
	// rsync does, see expandArgs.
	fsys, closeFS := moduleArgFS(&module)
	expanded := expandArgs(fsys, module.Name, pc.RemainingArgs[1:])
	closeFS()
	pc.RemainingArgs = append(pc.RemainingArgs[:1:1], expanded...)

	s.logger.Printf("expanded paths: %q", pc.RemainingArgs[1:])

	return s.handleConn(ctx, conn, &module, pc, remoteProtocol)
}

type Conn struct {
	name   string
	crd    *rsyncwire.CountingReader
	cwr    *rsyncwire.CountingWriter
	rd     *bufio.Reader
	closer io.Closer
	traced bool // crd and cwr log raw bytes

	// deadliner, when set, bounds the handshake (handshakeTimeout).
	deadliner interface{ SetDeadline(time.Time) error }
}

// handshakeTimeout bounds the daemon handshake, from the greeting to the
// last argument, as rsync 3.5 does (DAEMON_HANDSHAKE_TIMEOUT,
// CVE-2026-70464): a client that stalls in it would otherwise hold its
// connection open forever.
var handshakeTimeout = 60 * time.Second

// handshakeDeadline starts (on) or ends the handshake's deadline.
func (c *Conn) handshakeDeadline(on bool) {
	if c.deadliner == nil {
		return
	}
	var t time.Time
	if on {
		t = time.Now().Add(handshakeTimeout)
	}
	c.deadliner.SetDeadline(t)
}

func NewConnection(r io.ReadCloser, w io.WriteCloser, name string) *Conn {
	crd, cwr := rsyncwire.CounterPair(r, w)
	rd := bufio.NewReader(crd)
	return &Conn{
		name: name,
		crd:  crd,
		cwr:  cwr,
		rd:   rd,
		closer: &rsync.BothCloser{
			ReadCloser:  r,
			WriteCloser: w,
		},
	}
}

// This method is only exported until we refactor; use HandleConnArgs() instead
func (s *Server) InternalHandleConn(ctx context.Context, conn *Conn, module *Module, pc *rsyncopts.Context) error {
	return s.handleConn(ctx, conn, module, pc, 0 /* exchange versions */)
}

func (s *Server) HandleConnArgs(ctx context.Context, conn *Conn, module *Module, args []string) error {
	osenv := &rsyncos.Env{Stderr: s.stderr}
	pc := rsyncopts.NewContext(rsyncopts.NewOptionsWithGokrazyDefaults(osenv))
	if err := pc.ParseArguments(osenv, args); err != nil {
		return fmt.Errorf("parsing server args: %v", err)
	}
	return s.handleConn(ctx, conn, module, pc, 0 /* exchange versions */)
}

// handleConn is equivalent to rsync/main.c:start_server
//
// remoteProtocol is the version from the client's daemon greeting. When it is
// 0, both sides exchange versions on conn first, as over a remote shell.
func (s *Server) handleConn(ctx context.Context, conn *Conn, module *Module, pc *rsyncopts.Context, remoteProtocol int32) (err error) {
	rd := conn.rd
	crd := conn.crd
	cwr := conn.cwr
	opts := pc.Options
	paths := pc.RemainingArgs[1:]
	trace := s.traceConn(conn, opts.DebugLevel(rsyncopts.DEBUG_IO))

	// “SHOULD be unique to each connection” as per
	// https://github.com/JohannesBuchner/Jarsync/blob/master/jarsync/rsync.txt
	//
	// Computed the same way that tridge rsync does it, but the details do not
	// matter. The goal is to have a checksum seed each time.
	sessionChecksumSeed := int32(time.Now().Unix()) ^ (int32(os.Getpid()) << 6)

	c := &rsyncwire.Conn{
		Reader: struct {
			io.Reader
			io.Closer
		}{
			Reader: rd,
			Closer: conn.closer,
		},
		Writer: cwr,
	}

	if err := rsyncwire.CheckProtocolOption("Server", opts.ProtocolVersion()); err != nil {
		return err
	}
	if remoteProtocol == 0 {
		remoteProtocol, err = c.ReadInt32()
		if err != nil {
			return err
		}
		if err := c.WriteInt32(opts.ProtocolVersion()); err != nil {
			return err
		}
	}
	proto, err := rsyncwire.NegotiateProtocol("Server", opts.ProtocolVersion(), remoteProtocol)
	if err != nil {
		return err
	}
	c.Session.Protocol = proto
	if opts.DebugGTE(rsyncopts.DEBUG_PROTO, 1) {
		s.logger.Printf("(Server) Protocol versions: remote=%d, negotiated=%d", remoteProtocol, proto)
	}
	if err := opts.CheckProtocol(proto); err != nil {
		return err
	}

	if err := c.WriteCompatFlags(opts.ShellCommand()); err != nil {
		return err
	}
	if err := rsyncchecksum.Negotiate(c, opts.ChecksumChoice(), true); err != nil {
		return err
	}
	if opts.DebugGTE(rsyncopts.DEBUG_NSTR, 1) {
		s.logger.Printf("Server checksum: %s", rsyncchecksum.NewStrong(c.Session, 0).Name())
	}
	if err := rsynctoken.Negotiate(c, opts.CompressChoice(), opts.CompressLevel(), true); err != nil {
		return err
	}
	if c.Session.Compress != "" && opts.DebugGTE(rsyncopts.DEBUG_NSTR, 1) {
		s.logger.Printf("Server compress: %s (level %d)", c.Session.Compress, c.Session.CompressLevel)
	}
	if err := c.WriteInt32(sessionChecksumSeed); err != nil {
		return err
	}

	// Switch to multiplexing protocol, but only for server-side transmissions.
	// Transmissions received from the client are not multiplexed.
	mpx := &rsyncwire.MultiplexWriter{Writer: c.Writer, Trace: trace}
	// Update cwr to track the multiplexed writer,
	// but copy the number of bytes written.
	cwr = &rsyncwire.CountingWriter{
		W:            mpx,
		BytesWritten: cwr.BytesWritten,
	}
	c.Writer = cwr
	c.Mux = mpx

	// What the client tells this side (logging, I/O error bits) lands here,
	// shared by the transfer.
	env := &rsyncos.Env{Stderr: s.stderr}

	// From protocol 30 the client multiplexes its output as well
	// (main.c:start_server, need_messages_from_generator).
	if c.Session.Protocol >= 30 {
		mrd := &rsyncwire.MultiplexReader{Env: env, Reader: rd, Trace: trace, Protocol: c.Session.Protocol}
		c.Reader = struct {
			io.Reader
			io.Closer
		}{
			Reader: bufio.NewReaderSize(mrd, 256*1024),
			Closer: conn.closer,
		}
	}

	if opts.Sender() {
		// If returning an error, send the error to the client for display, too:
		defer func() {
			if err != nil {
				mpx.WriteMsg(rsyncwire.MsgErrorXfer, fmt.Appendf(nil, "gokr-rsync [sender]: %v\n", err))
			}
		}()

		return s.handleConnSender(module, crd, cwr, paths, opts, c, sessionChecksumSeed, mpx, env)
	}

	// If returning an error, send the error to the client for display, too:
	defer func() {
		if err != nil {
			mpx.WriteMsg(rsyncwire.MsgErrorXfer, fmt.Appendf(nil, "gokr-rsync [receiver]: %v\n", err))
		}
	}()
	return s.handleConnReceiver(module, crd, cwr, paths, opts, c, sessionChecksumSeed, mpx, env)
}

// handleConnReceiver is equivalent to rsync/main.c:do_server_recv
func (s *Server) handleConnReceiver(module *Module, crd *rsyncwire.CountingReader, cwr *rsyncwire.CountingWriter, paths []string, opts *rsyncopts.Options, c *rsyncwire.Conn, sessionChecksumSeed int32, mpx *rsyncwire.MultiplexWriter, env *rsyncos.Env) (err error) {
	var destPath string
	implicitModule := module == nil
	if implicitModule {
		if len(paths) != 1 {
			return fmt.Errorf("precisely one destination path required, got %q", paths)
		}
		module = &Module{
			Name:     "implicit",
			Path:     paths[0],
			Writable: true,
		}
		destPath = module.Path
	}
	if opts.Verbose() {
		s.logger.Printf("handleConnReceiver(module=%+v, destPath=%q)", module, destPath)
	}

	if !module.Writable {
		return fmt.Errorf("ERROR: module is read only")
	}

	rt := &receiver.Transfer{
		Logger: s.logger,
		Opts: &receiver.TransferOpts{
			DryRun:   opts.DryRun(),
			Server:   opts.Server(),
			Verbose:  opts.Verbose(),
			Progress: opts.Progress(),

			DeleteMode:       opts.DeleteMode(),
			PreserveGid:      opts.PreserveGid(),
			PreserveUid:      opts.PreserveUid(),
			PreserveLinks:    opts.PreserveLinks(),
			PreservePerms:    opts.PreservePerms(),
			PreserveDevices:  opts.PreserveDevices(),
			PreserveSpecials: opts.PreserveSpecials(),
			PreserveTimes:    opts.PreserveMTimes(),
			OmitDirTimes:     opts.OmitDirTimes(),
			// TODO: PreserveHardlinks: opts.PreserveHardlinks,
			IgnoreTimes:    opts.IgnoreTimes(),
			AlwaysChecksum: opts.AlwaysChecksum(),
			WholeFile:      opts.WholeFile(),

			Inplace:        opts.Inplace(),
			BlockSize:      opts.BlockSize(),
			PreserveXattrs: opts.PreserveXattrs(),
			FakeSuper:      opts.FakeSuper(),
			Super:          opts.Super(),
			KeepPartial:    opts.KeepPartial(),
			PartialDir:     opts.PartialDir(),

			AppendMode: opts.AppendMode(),

			DoFsync:     opts.DoFsync(),
			SparseFiles: opts.SparseFiles(),

			InfoGTE:  opts.InfoGTE,
			DebugGTE: opts.DebugGTE,
		},
		Dest:     module.Path,
		Env:      env,
		Conn:     c,
		Seed:     sessionChecksumSeed,
		Progress: progress.NewPrinter(io.Discard, time.Now),
		TellClient: func(tag uint8, msg string) {
			mpx.WriteMsg(tag, []byte(msg))
		},
	}
	if err := os.MkdirAll(rt.Dest, 0755); err != nil {
		return fmt.Errorf("MkdirAll(dest=%s): %v", rt.Dest, err)
	}
	rt.DestRoot, err = os.OpenRoot(rt.Dest)
	if err != nil {
		return fmt.Errorf("OpenRoot(dest=%s): %v", rt.Dest, err)
	}
	defer rt.DestRoot.Close()

	if !implicitModule {
		if len(paths) > 1 {
			return fmt.Errorf("module is available, and at most one destination path is allowed, got %q", paths)
		}
		// Descend into subdirectory (if requested),
		// using the os.OpenRoot traversal-safe API.
		if len(paths) == 1 && paths[0] != "/" {
			// Trim leading and trailing slash:
			// (*os.Root).MkdirAll("foo/") was a security issue:
			// https://go.dev/issue/79005, so no longer works.
			subdir := strings.Trim(paths[0], "/")
			subRoot, err := rt.DestRoot.OpenRoot(subdir)
			if err != nil {
				if os.IsNotExist(err) {
					if err := rt.DestRoot.MkdirAll(subdir, 0755); err != nil {
						return fmt.Errorf("MkdirAll(%s): %v", subdir, err)
					}
					subRoot, err = rt.DestRoot.OpenRoot(subdir)
				}
				if err != nil {
					return fmt.Errorf("OpenRoot(%s): %v", subdir, err)
				}
			}
			if name := subRoot.Name(); filepath.IsAbs(name) {
				rt.Dest = name
			} else {
				// Go changed behavior: In Go 1.25, subRoot.Name()
				// did not return an absolute path:
				// https://go.googlesource.com/go/+/ed7f804
				rt.Dest = filepath.Join(rt.Dest, name)
			}
			rt.DestRoot = subRoot
			if opts.Verbose() {
				s.logger.Printf("opened subdirectory %q", rt.Dest)
			}
		}
	}

	if opts.PreserveHardLinks() {
		return fmt.Errorf("support for hard links not yet implemented")
	}

	if opts.ReceiverWantsFilterList(c.Session.Protocol) {
		// receive the exclusion list (openrsync’s is always empty)
		exclusionList, err := sender.RecvFilterList(c)
		if err != nil {
			return err
		}
		s.logger.Printf("exclusion list read (entries: %d)", len(exclusionList.Filters))
		rt.Excluded = exclusionList.Excluded
	}

	// receive file list
	if opts.InfoGTE(rsyncopts.INFO_FLIST, 1) {
		s.logger.Printf("receiving file list")
	}
	fileList, err := rt.ReceiveFileList()
	if err != nil {
		return err
	}
	if opts.InfoGTE(rsyncopts.INFO_FLIST, 1) {
		s.logger.Printf("received %d names", len(fileList))
	}
	stats, err := rt.Do(c, fileList, true)
	if err != nil {
		return err
	}
	if opts.InfoGTE(rsyncopts.INFO_STATS, 1) {
		s.logger.Printf("stats: %+v", stats)
	}
	return nil
}

// handleConnSender is equivalent to rsync/main.c:do_server_sender
func (s *Server) handleConnSender(module *Module, crd *rsyncwire.CountingReader, cwr *rsyncwire.CountingWriter, paths []string, opts *rsyncopts.Options, c *rsyncwire.Conn, sessionChecksumSeed int32, mpx *rsyncwire.MultiplexWriter, env *rsyncos.Env) (err error) {
	if module == nil {
		module = &Module{
			Name: "implicit",
			Path: rsync.FileSystemRoot,
		}
	}

	// A server sender passes the client generator's delete stats on to
	// the client's receiver (protocol 31, rsync.c:read_ndx_and_attrs).
	c.ForwardDelStats = true
	st := &sender.Transfer{
		Logger:   s.logger,
		Opts:     opts,
		Conn:     c,
		Seed:     sessionChecksumSeed,
		Env:      env,
		Progress: progress.NewPrinter(io.Discard, time.Now),
		TellClient: func(tag uint8, msg string) {
			mpx.WriteMsg(tag, []byte(msg))
		},
	}
	// receive the exclusion list (openrsync’s is always empty)

	if module.FS != nil {
		st.Source = sender.NewFSSource(module.FS)
	}

	exclusionList, err := sender.RecvFilterList(st.Conn)
	if err != nil {
		return err
	}
	st.Logger.Printf("exclusion list read (entries: %d)", len(exclusionList.Filters))

	stats, err := st.Do(crd, cwr, module.Path, paths, exclusionList)
	if err != nil {
		return err
	}

	s.logger.Printf("handleConnSender done. stats: %+v", stats)

	return nil
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close() // unblocks Accept()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil // ignore expected 'use of closed network connection' error on context cancel
			default:
				// Keep serving even when all file descriptors are in use.
				// As older transfers finish, new connections can be accepted.
				temporary := errors.Is(err, syscall.EMFILE) || // too many open files
					errors.Is(err, syscall.ENFILE) // too many open files in system
				if temporary {
					s.logger.Printf("accept: %v (retrying)", err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				return err
			}
		}
		remoteAddr := conn.RemoteAddr()
		s.logger.Printf("remote connection from %s", remoteAddr)
		go func() {
			defer conn.Close()
			c := NewConnection(conn, conn, remoteAddr.String())
			c.deadliner = conn
			if err := s.HandleDaemonConn(ctx, c); err != nil {
				s.logger.Printf("[%s] handle: %v", remoteAddr, err)
				// Perform a “lingering close” [1] to work around
				// “the TCP reset problem” (per RFC 9112 Section 9.6):
				// When the server closes the TCP socket while there
				// is still data in the recv buffer (usually the case
				// with rsync, which is heavily pipelined),
				// the kernel sends RST (not FIN), which the client
				// might implement by dropping queued data,
				// as mandated by the TCP RFC 9293 Section 3.10.7.4.
				//
				// As of 2026, Linux does not follow the RFC, Windows does.
				//
				// A lingering close shuts down the socket (sends FIN)
				// and waits until the client has received it,
				// which the client confirms by closing.
				//
				// [1]: https://apenwarr.ca/log/20090814-solinger-is-not-the-same-as-apaches-lingering-close
				const lingerTimeout = 5 * time.Second
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
				conn.SetReadDeadline(time.Now().Add(lingerTimeout))
				io.Copy(io.Discard, conn)
			}
		}()
	}
}

func (s *Server) authServer(rd *bufio.Reader, cwr io.Writer, module *Module, remoteAddr string, proto int32, clientDigests string) error {
	digest, err := rsyncchecksum.NegotiateAuth(proto, clientDigests)
	if err != nil {
		fmt.Fprintf(cwr, "@ERROR: your client does not support one of our daemon-auth checksums: %s\n", rsyncchecksum.AuthDigestList())
		return err
	}
	challenge := genChallenge()
	fmt.Fprintf(cwr, "@RSYNCD: AUTHREQD %s\n", challenge)

	// ReadSlice like the rest of the handshake (upstream 28ec171): a response
	// longer than the reader's buffer is rejected instead of growing memory
	// until a newline arrives. Copy it out before the next read reuses it.
	raw, err := rd.ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("reading auth response: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	sp := strings.IndexByte(line, ' ')
	if sp < 0 {
		s.logger.Printf("auth failed on module %s from %s: invalid response", module.Name, remoteAddr)
		return fmt.Errorf("invalid auth response")
	}
	user, response := line[:sp], line[sp+1:]

	matched := false
	for _, allowed := range module.AuthUsers {
		if allowed == user {
			matched = true
			break
		}
	}
	if !matched {
		s.logger.Printf("auth failed on module %s from %s for %s: unknown user", module.Name, remoteAddr, user)
		return fmt.Errorf("auth failed")
	}

	secret, err := lookupSecret(module.SecretsFile, user)
	if err != nil {
		s.logger.Printf("auth failed on module %s from %s for %s: %v", module.Name, remoteAddr, user, err)
		return fmt.Errorf("auth failed")
	}

	expected := rsyncchecksum.AuthResponse(digest, secret, challenge)
	if response != expected {
		s.logger.Printf("auth failed on module %s from %s for %s: password mismatch", module.Name, remoteAddr, user)
		return fmt.Errorf("auth failed")
	}

	s.logger.Printf("auth ok on module %s from %s for %s", module.Name, remoteAddr, user)
	return nil
}

func genChallenge() string {
	var buf [16]byte
	rand.Read(buf[:])
	h := md4.New()
	h.Write([]byte{0, 0, 0, 0})
	h.Write(buf[:])
	return base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString(h.Sum(nil))
}

func lookupSecret(path, user string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no secrets file configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading secrets file: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[0] == user {
			return parts[1], nil
		}
	}
	return "", fmt.Errorf("user not found in secrets file")
}

func validateModule(mod Module) error {
	if mod.Name == "" {
		return errors.New("module has no name")
	}
	if mod.FS != nil {
		if mod.Writable {
			return fmt.Errorf("module %q: FS modules cannot be writable", mod.Name)
		}
		if mod.Path != "" {
			return fmt.Errorf("module %q: cannot specify both Path and FS", mod.Name)
		}
	} else {
		if mod.Path == "" {
			return fmt.Errorf("module %q has empty path", mod.Name)
		}
	}

	return nil
}
