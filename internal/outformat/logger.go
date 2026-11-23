package outformat

import (
	"io"
	"os"
	"sync"
	"time"

	"github.com/gokrazy/rsync"
)

// Code says where a line goes (rsync's enum logcode).
type Code int

const (
	// Client is the --out-format on stdout only (FCLIENT).
	Client Code = iota
	// Log is the --log-file only (FLOG).
	Log
	// Info is both (FINFO).
	Info
)

// significant are the item flags worth a line (rsync.h
// SIGNIFICANT_ITEM_FLAGS).
const significant = 0xffff &^ (rsync.ITEM_BASIS_TYPE_FOLLOWS | rsync.ITEM_XNAME_FOLLOWS | rsync.ITEM_LOCAL_CHANGE)

// Logger prints items to stdout with the --out-format and to the
// --log-file with its format (log.c:log_item and friends). Its methods
// may be called from several goroutines.
type Logger struct {
	Context

	Server bool // a server prints nothing to stdout
	DryRun bool

	// LogBefore prints a transferred item before its data, and only to
	// the log file after it (rsync's log_before_transfer).
	LogBefore bool
	Progress  bool // --info=progress at level 1

	Stdout       io.Writer
	StdoutFormat string
	StdoutHasI   int  // see rsyncopts.Options.StdoutFormatHasI
	StdoutHasOI  bool // see rsyncopts.Options.StdoutFormatHasOorI
	InfoName     int  // --info=name level
	InfoDel      bool // --info=del (-v)

	LogFile       io.Writer // nil without --log-file
	LogFileFormat string
	LogFileHasI   bool

	mu sync.Mutex
}

// LogItem prints it as code says (log.c:log_item).
func (l *Logger) LogItem(code Code, it Item) {
	if l == nil {
		return
	}
	if code != Log && l.StdoutFormat != "" && !l.Server && l.Stdout != nil {
		l.write(l.Stdout, l.Format(l.StdoutFormat, it))
	}
	if code != Client && l.LogFileFormat != "" {
		l.Logf("%s", l.Format(l.LogFileFormat, it))
	}
}

// Itemizing is whether this end reports items it is told of
// (receiver.c and sender.c: itemizing).
func (l *Logger) Itemizing() bool {
	if l == nil {
		return false
	}
	if l.Server {
		return l.LogFileHasI
	}
	return l.StdoutHasI != 0
}

// MaybeLogItem prints an item there was no data for, if it is worth a
// line (log.c:maybe_log_item).
func (l *Logger) MaybeLogItem(it Item) {
	if l == nil {
		return
	}
	itemizing := l.Itemizing()
	sig := it.Iflags & significant
	seeItem := itemizing && (sig != 0 || it.Hlink != "" || l.StdoutHasI > 1 || (l.InfoName >= 2 && l.StdoutHasI != 0))
	localChange := it.Iflags&rsync.ITEM_LOCAL_CHANGE != 0 && sig != 0
	if l.Server {
		if l.LogFile != nil && !l.DryRun && seeItem && (sig != 0 || l.LogFileHasI) {
			l.LogItem(Log, it)
		}
		return
	}
	if seeItem || localChange || it.Hlink != "" || (it.Mode&rsync.S_IFMT == rsync.S_IFDIR && sig != 0) {
		code := Client
		if sig != 0 || l.LogFileHasI {
			code = Info
		}
		l.LogItem(code, it)
	}
}

// LogDelete prints a deleted name, as "deleting %n" or with the format
// when it has %o or %i (log.c:log_delete); a server tells its client
// instead.
func (l *Logger) LogDelete(name string, mode uint32) {
	if l == nil {
		return
	}
	it := Item{Name: name, Mode: mode, Iflags: rsync.ITEM_DELETED}
	if !l.Server && (l.InfoDel || l.StdoutFormat != "") && l.Stdout != nil {
		format := "deleting %n"
		if l.StdoutHasOI {
			format = l.StdoutFormat
		}
		l.write(l.Stdout, l.del(format, it))
	}
	if l.LogFile != nil && l.LogFileFormat != "" {
		format := "deleting %n"
		if l.LogFileHasI || Has(l.LogFileFormat, 'o') {
			format = l.LogFileFormat
		}
		l.Logf("%s", l.del(format, it))
	}
}

func (l *Logger) del(format string, it Item) string {
	c := l.Context
	c.Op = "del."
	return c.Format(format, it)
}

// Logf writes a line to the --log-file, after the time and process ID as
// rsync's logit does.
func (l *Logger) Logf(format string, args ...any) {
	if l == nil || l.LogFile == nil {
		return
	}
	msg := sprintf(format, args...)
	if len(msg) == 0 || msg[len(msg)-1] != '\n' {
		msg += "\n"
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	l.write(l.LogFile, Timestring(now())+" ["+itoa(os.Getpid())+"] "+msg)
}

func (l *Logger) write(w io.Writer, s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	io.WriteString(w, s)
}

// Before prints a transferred item as its data starts: in full when
// LogBefore, else just its name with --progress (receiver.c and sender.c
// recv_files, send_files).
func (l *Logger) Before(it Item, name string) {
	switch {
	case l == nil:
	case l.LogBefore:
		l.LogItem(Client, it)
	case !l.Server && l.InfoName >= 1 && l.Progress && l.Stdout != nil:
		l.write(l.Stdout, name+"\n")
	}
}

// After prints a transferred item once its data is through.
func (l *Logger) After(it Item) {
	if l == nil {
		return
	}
	code := Info
	if l.LogBefore {
		code = Log
	}
	l.LogItem(code, it)
}
