package receiver

import (
	"io"
	"io/fs"
	"math"
	"os"
	"sync"

	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/progress"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsynctoken"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/rsyncxattr"
)

// TransferOpts is a subset of Opts which is required for implementing a receiver.
type TransferOpts struct {
	Verbose  bool
	DryRun   bool
	Server   bool
	Progress bool

	DeleteMode        bool
	PreserveGid       bool
	PreserveUid       bool
	PreserveLinks     bool
	PreservePerms     bool
	PreserveDevices   bool
	PreserveSpecials  bool
	PreserveTimes     bool
	PreserveHardlinks bool

	// Symlink options on the receiving side: -K counts a symlink to a
	// folder at the destination as that folder, --safe-links leaves out
	// symlinks that lead outside the transfer, --munge-links puts
	// rsyncopts.SymlinkPrefix before every target it creates.
	KeepDirlinks bool
	SafeLinks    bool
	MungeLinks   bool

	// BasisDirs are the alternate folders of --compare-dest, --copy-dest
	// or --link-dest, which AltDest names (rsyncopts.CompareDest, ...).
	BasisDirs []string
	AltDest   int

	// ModifyWindow is --modify-window (see modTimeEqual).
	ModifyWindow int

	// Which files the generator leaves alone: --size-only compares sizes
	// alone, --existing skips what the destination lacks,
	// --ignore-existing what it has, -u what is newer there, and
	// --max-size and --min-size (-1 for none) regular files by size.
	SizeOnly          bool
	IgnoreNonExisting bool
	IgnoreExisting    bool
	UpdateOnly        bool
	MaxSize           int64
	MinSize           int64

	// Force removes a folder that is in a file's way with what it holds
	// (--force). IgnoreErrors deletes despite I/O errors on the sending
	// side (--ignore-errors). MaxDelete caps deletions (--max-delete),
	// NoMaxDelete for none.
	Force        bool
	IgnoreErrors bool
	MaxDelete    int

	// NumericIds is --numeric-ids: no name lists, ids as sent.
	NumericIds bool

	// RemoveSourceFiles tells the sender each file this side has
	// (MSG_SUCCESS), so that it removes its copy.
	RemoveSourceFiles bool

	// DeleteAfter deletes once the transfer is done (--delete-delay,
	// --delete-after) rather than before it.
	DeleteAfter    bool
	IgnoreTimes    bool
	AlwaysChecksum bool
	WholeFile      bool

	// PreserveXattrs is how many -X were given (see
	// rsyncopts.Options.PreserveXattrs).
	PreserveXattrs int

	// FakeSuper is --fake-super: what only root can set (owners, devices,
	// specials, symlinks, setuid bits) goes into the user.rsync.%stat
	// xattr of a regular file or folder instead. Super is --super: act as
	// root.
	FakeSuper bool
	Super     bool

	// BlockSize is --block-size: the delta block length for every file,
	// or 0 for rsync's choice per file (sum_sizes_sqroot).
	BlockSize int32

	// OmitDirTimes leaves the times of folders alone (--omit-dir-times),
	// with PreserveTimes too.
	OmitDirTimes bool

	Inplace     bool
	KeepPartial bool
	PartialDir  string

	// AppendMode controls --append / --append-verify on the receiver. 0 = off,
	// 1 = --append (trust existing prefix), 2 = --append-verify (checksum
	// verify the prefix on the sender). Only AppendMode > 0 changes the
	// generator's request: it sends sums for the existing partial prefix so
	// the sender knows to skip those bytes.
	AppendMode int

	// DoFsync fsyncs every written file (--fsync).
	DoFsync bool

	// SparseFiles turns long runs of zeros into holes (--sparse). It only
	// applies to files written through a temporary file; see outputMode.Sparse.
	SparseFiles bool

	// OnFile, when set, is called for every file list entry as it is
	// received, in wire order, before the list is sorted.
	OnFile func(*File)

	// UnverifiedAppend accepts an appended tail without the file sum check:
	// the sender's sum covers the whole file, so a destination that holds a
	// placeholder rather than the real prefix can never match it. The remote
	// sum is still read to keep the stream in step.
	UnverifiedAppend bool

	InfoGTE  func(rsyncopts.InfoLevel, uint16) bool
	DebugGTE func(rsyncopts.DebugLevel, uint16) bool
}

type Transfer struct {
	// config
	Logger   log.Logger
	Opts     *TransferOpts
	Dest     string
	DestRoot *os.Root
	Env      *rsyncos.Env
	Progress progress.Printer

	// Excluded, if non-nil, reports whether a destination path (relative to
	// DestRoot, slash-separated) is protected from deletion by the
	// client-supplied filter list. Consulted only when DeleteMode is on.
	Excluded func(name string) bool

	// OpenBasisDir, if non-nil, opens an alternate folder of --link-dest
	// and the like, as the daemon does to keep them inside its module.
	// Otherwise a relative one is taken from Dest, as rsync does.
	OpenBasisDir func(dir string) (*os.Root, error)

	// Implied, when set, is what a receiving client asked for: a name
	// outside of it fails the file list (rsync 3.4, CVE-2022-29154).
	Implied *ImpliedNames

	// TellClient, when set, sends the client a message with tag, as an rsync
	// server tells it about a file it could not delete (rsync/delete.c
	// delete_item); the client counts the message as an error. Servers set
	// it; a client counts the error in Env.XferErrors instead.
	TellClient func(tag uint8, msg string)

	// state
	Conn         *rsyncwire.Conn
	Seed         int32
	IOErrors     int32
	Users        map[int32]mapping
	Groups       map[int32]mapping
	defaultPerms fs.FileMode
	retouchDirs  []retouchDir

	// hlinkLater maps each later member of a hard-link group to the
	// group's first (hlink.go). It is read-only once the transfer runs.
	hlinkLater map[*File]*File

	// basisRoots are the alternate folders, nil where one could not be
	// opened (alt.go); altBasis records, for a file, the one the generator
	// sent block sums of, which the receiver then reads.
	basisRoots []*os.Root

	// deleteLeft is how many more entries --max-delete lets go;
	// deleteSkipped how many it kept. The delete pass and the generator
	// clearing a folder out of a file's way share them.
	deleteLeft, deleteSkipped int64
	altBasis                  sync.Map           // *File -> int
	delStats                  rsyncwire.DelStats // what --delete removed (see countDeleted)
	touchedBlocks             int64              // 4 KiB blocks written to files (protocol 33 MSG_BLOCK_STATS)
	dec                       rsynctoken.Decoder // see decoder

	// xattrCache holds the file list's xattr lists (-X). genXattr is the
	// generator's finding for the file it is on, xattrFetched the values
	// the receiver got with the file it is on.
	xattrCache   rsyncxattr.Cache
	xattrGen     rsyncxattr.GeneratorState
	xattrRecv    rsyncxattr.ReceiverState
	genXattr     xattrState
	xattrFetched map[int][]byte
}

type retouchDir struct {
	f    *File
	perm fs.FileMode
}

func (rt *Transfer) listOnly() bool { return rt.Dest == "" }

// decoder is the session's token decoder, made on first use: it keeps the
// compressor's state from file to file.
func (rt *Transfer) decoder() (rsynctoken.Decoder, error) {
	if rt.dec == nil {
		dec, err := rsynctoken.NewDecoder(rt.Conn.Session, rt.Conn.Reader)
		if err != nil {
			return nil, err
		}
		rt.dec = dec
	}
	return rt.dec, nil
}

// closeDecoder stops what the decoder runs, if anything: zstd's decoder
// has a goroutine of its own.
func (rt *Transfer) closeDecoder() {
	if c, ok := rt.dec.(io.Closer); ok {
		c.Close()
	}
}

// strong is the session's strong checksum (MD4 or MD5, seeded).
func (rt *Transfer) strong() rsyncchecksum.Strong {
	return rsyncchecksum.NewStrong(rt.Conn.Session, rt.Seed)
}

// NoMaxDelete is TransferOpts.MaxDelete without --max-delete.
const NoMaxDelete = math.MinInt32
