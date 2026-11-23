// Package outformat expands rsync's --out-format and --log-file-format
// escapes for one item, as rsync/log.c:log_formatted does.
package outformat

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gokrazy/rsync"
)

// Item is what an output line describes: a file list entry and what
// happened to it.
type Item struct {
	Name       string // the file list name, without a trailing slash
	Mode       uint32 // rsync mode: type and permission bits
	Length     int64
	ModTime    time.Time
	Uid, Gid   int32
	LinkTarget string // a symlink's target
	Hlink      string // the name a hard link was made to, for %L
	Iflags     uint32 // the item flags (rsync.ITEM_*)
	Bytes      int64  // the bytes this item took on the wire, for %b and %c
	Checksum   []byte // the whole-file checksum, for %C
}

// Context is what does not change from item to item.
type Context struct {
	// Op is "send", "recv" or "del.", for %o; with an 's' the transfer
	// arrow points out ('<').
	Op string
	// Sender is whether this end sends: %b counts what it wrote, %c what
	// it read, and the other way round for a receiver.
	Sender bool
	// LocalServer is a copy between two local folders, where every arrow
	// points in ('>').
	LocalServer bool

	PreserveUid, PreserveGid bool
	PreserveMTimes           bool
	ReceiverSymlinkTimes     bool // the receiver can set symlink times
	ChecksumLen              int  // bytes of the %C sum, for padding without one

	// Daemon fields: left nil on a client, which leaves %h, %a, %m, %P and
	// %u as they are, as rsync does.
	Host, Addr, Module, ModulePath, User *string

	// Now is for %t; nil means time.Now.
	Now func() time.Time
}

// Format returns the line (with its newline) that format makes of it.
func (c *Context) Format(format string, it Item) string {
	var out strings.Builder
	for {
		i := strings.IndexByte(format, '%')
		if i < 0 {
			out.WriteString(format)
			break
		}
		out.WriteString(format[:i])
		esc := format[i:]
		p := 1
		humanize := 0
		for p < len(esc) && esc[p] == '\'' {
			humanize++
			p++
		}
		width := ""
		if p < len(esc) && esc[p] == '-' {
			width += "-"
			p++
		}
		for p < len(esc) && esc[p] >= '0' && esc[p] <= '9' && len(width) < 32-8 {
			width += esc[p : p+1]
			p++
		}
		for p < len(esc) && esc[p] == '\'' {
			humanize++
			p++
		}
		if p >= len(esc) {
			// A trailing escape stays as it is.
			out.WriteString(esc)
			break
		}
		n, ok := c.expand(esc[p], width, humanize, it)
		if !ok {
			out.WriteString(esc[:p+1])
		} else {
			out.WriteString(n)
		}
		format = esc[p+1:]
	}
	out.WriteByte('\n')
	return out.String()
}

// pad applies a "%-10s" style width to s.
func pad(width, s string) string {
	if width == "" {
		return s
	}
	return fmt.Sprintf("%"+width+"s", s)
}

// expand is one escape; ok is false where rsync leaves it in place.
func (c *Context) expand(esc byte, width string, humanize int, it Item) (string, bool) {
	switch esc {
	case 'h':
		return padPtr(width, c.Host)
	case 'a':
		return padPtr(width, c.Addr)
	case 'm':
		return padPtr(width, c.Module)
	case 'P':
		return padPtr(width, c.ModulePath)
	case 'u':
		return padPtr(width, c.User)
	case 'l':
		return pad(width, BigNum(it.Length, humanize)), true
	case 'U':
		uid := int64(0)
		if c.PreserveUid {
			uid = int64(uint32(it.Uid))
		}
		return pad(width, strconv.FormatInt(uid, 10)), true
	case 'G':
		if !c.PreserveGid {
			return pad(width, "DEFAULT"), true
		}
		return pad(width, strconv.FormatInt(int64(uint32(it.Gid)), 10)), true
	case 'p':
		return pad(width, strconv.Itoa(os.Getpid())), true
	case 'M':
		return pad(width, strings.ReplaceAll(Timestring(it.ModTime), " ", "-")), true
	case 'B':
		return pad(width, Permstring(it.Mode)[1:]), true
	case 'o':
		return pad(width, c.Op), true
	case 'f':
		return pad(width, strings.TrimPrefix(it.Name, "/")), true
	case 'n':
		n := it.Name
		if it.Mode&rsync.S_IFMT == rsync.S_IFDIR {
			n += "/"
		}
		return pad(width, n), true
	case 'L':
		var arrow, n string
		switch {
		case it.Hlink != "":
			arrow, n = " => ", it.Hlink
		case it.Mode&rsync.S_IFMT == rsync.S_IFLNK && it.Iflags&rsync.ITEM_DELETED == 0:
			arrow, n = " -> ", it.LinkTarget
		default:
			if width == "" {
				return "", true
			}
			arrow = "    "
		}
		return arrow + pad(width, n), true
	case 't':
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		return pad(width, Timestring(now())), true
	case 'b', 'c':
		var b int64
		if it.Iflags&rsync.ITEM_TRANSFER != 0 && c.Sender != (esc == 'c') {
			b = it.Bytes
		}
		return pad(width, BigNum(b, humanize)), true
	case 'C':
		if it.Mode&rsync.S_IFMT == rsync.S_IFREG && len(it.Checksum) > 0 {
			return pad(width, hex.EncodeToString(it.Checksum)), true
		}
		return pad(width, strings.Repeat(" ", 2*c.ChecksumLen)), true
	case 'i':
		return pad(width, c.itemize(it)), true
	case '%':
		return pad(width, "%"), true
	}
	return "", false
}

func padPtr(width string, s *string) (string, bool) {
	if s == nil {
		return "", false
	}
	return pad(width, *s), true
}

// itemize is %i, the 11-character change summary (log.c).
func (c *Context) itemize(it Item) string {
	iflags := it.Iflags
	if iflags&rsync.ITEM_DELETED != 0 {
		return "*deleting  "
	}
	b := []byte("...........")
	switch {
	case iflags&rsync.ITEM_LOCAL_CHANGE != 0:
		b[0] = 'c'
		if iflags&rsync.ITEM_XNAME_FOLLOWS != 0 {
			b[0] = 'h'
		}
	case iflags&rsync.ITEM_TRANSFER == 0:
		b[0] = '.'
	case !c.LocalServer && strings.HasPrefix(c.Op, "s"):
		b[0] = '<'
	default:
		b[0] = '>'
	}
	flag := func(i int, set bool, ch byte) {
		if set {
			b[i] = ch
		}
	}
	mode := it.Mode & rsync.S_IFMT
	if mode == rsync.S_IFLNK {
		b[1] = 'L'
		if iflags&rsync.ITEM_REPORT_TIME != 0 {
			b[4] = 't'
			if !c.PreserveMTimes || !c.ReceiverSymlinkTimes || iflags&rsync.ITEM_REPORT_TIMEFAIL != 0 {
				b[4] = 'T'
			}
		}
	} else {
		switch mode {
		case rsync.S_IFDIR:
			b[1] = 'd'
		case rsync.S_IFIFO, rsync.S_IFSOCK:
			b[1] = 'S'
		case rsync.S_IFCHR, rsync.S_IFBLK:
			b[1] = 'D'
		default:
			b[1] = 'f'
		}
		flag(3, iflags&rsync.ITEM_REPORT_SIZE != 0, 's')
		if iflags&rsync.ITEM_REPORT_TIME != 0 {
			b[4] = 't'
			if !c.PreserveMTimes {
				b[4] = 'T'
			}
		}
	}
	flag(2, iflags&rsync.ITEM_REPORT_CHANGE != 0, 'c')
	flag(5, iflags&rsync.ITEM_REPORT_PERMS != 0, 'p')
	flag(6, iflags&rsync.ITEM_REPORT_OWNER != 0, 'o')
	flag(7, iflags&rsync.ITEM_REPORT_GROUP != 0, 'g')
	switch iflags & (rsync.ITEM_REPORT_ATIME | rsync.ITEM_REPORT_CRTIME) {
	case rsync.ITEM_REPORT_ATIME | rsync.ITEM_REPORT_CRTIME:
		b[8] = 'b'
	case rsync.ITEM_REPORT_ATIME:
		b[8] = 'u'
	case rsync.ITEM_REPORT_CRTIME:
		b[8] = 'n'
	}
	flag(9, iflags&rsync.ITEM_REPORT_ACL != 0, 'a')
	flag(10, iflags&rsync.ITEM_REPORT_XATTR != 0, 'x')

	switch {
	case iflags&(rsync.ITEM_IS_NEW|rsync.ITEM_MISSING_DATA) != 0:
		ch := byte('+')
		if iflags&rsync.ITEM_IS_NEW == 0 {
			ch = '?'
		}
		for i := 2; i < len(b); i++ {
			b[i] = ch
		}
	case b[0] == '.' || b[0] == 'h' || b[0] == 'c':
		if strings.Trim(string(b[2:]), ".") == "" {
			for i := 2; i < len(b); i++ {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// Has reports whether format uses escape esc (log.c:log_format_has).
func Has(format string, esc byte) bool {
	for {
		i := strings.IndexByte(format, '%')
		if i < 0 {
			return false
		}
		p := i + 1
		for p < len(format) && format[p] == '\'' {
			p++
		}
		width := 1
		if p < len(format) && format[p] == '-' {
			p++
			width++
		}
		for p < len(format) && format[p] >= '0' && format[p] <= '9' && width < 32-8 {
			p++
			width++
		}
		for p < len(format) && format[p] == '\'' {
			p++
		}
		if p >= len(format) {
			return false
		}
		if format[p] == esc {
			return true
		}
		if format[p] == '%' {
			p++
		}
		format = format[p:]
	}
}

// Timestring is rsync's local "YYYY/MM/DD hh:mm:ss" (util1.c:timestring).
func Timestring(t time.Time) string {
	return t.Local().Format("2006/01/02 15:04:05")
}

// Permstring is ls-style "drwxr-xr-x" (lib/permstring.c).
func Permstring(mode uint32) string {
	b := []byte("----------")
	const perms = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if mode&(1<<i) != 0 {
			b[9-i] = perms[8-i]
		}
	}
	if mode&0o4000 != 0 {
		b[3] = map[bool]byte{true: 's', false: 'S'}[mode&0o100 != 0]
	}
	if mode&0o2000 != 0 {
		b[6] = map[bool]byte{true: 's', false: 'S'}[mode&0o010 != 0]
	}
	if mode&0o1000 != 0 {
		b[9] = map[bool]byte{true: 't', false: 'T'}[mode&0o001 != 0]
	}
	switch mode & rsync.S_IFMT {
	case rsync.S_IFDIR:
		b[0] = 'd'
	case rsync.S_IFLNK:
		b[0] = 'l'
	case rsync.S_IFBLK:
		b[0] = 'b'
	case rsync.S_IFCHR:
		b[0] = 'c'
	case rsync.S_IFSOCK:
		b[0] = 's'
	case rsync.S_IFIFO:
		b[0] = 'p'
	}
	return string(b)
}

// BigNum is lib/compat.c:do_big_num: plain at 0, with thousands
// separators at 1, and in units of 1000 or 1024 at 2 or 3.
func BigNum(num int64, human int) string {
	abs := uint64(num)
	if num < 0 {
		abs = uint64(-num)
	}
	if human > 1 {
		mult := uint64(1000)
		if human != 2 {
			mult = 1024
		}
		units := "KMGTPE"
		unit := -1
		powi := uint64(1)
		for abs/mult >= powi && unit < len(units)-1 {
			powi *= mult
			unit++
		}
		if powi > 1 {
			precision := 2
			for powj := uint64(1); precision > 0; precision-- {
				powj *= 10
				if abs/powi < powj {
					break
				}
			}
			suffix := units[unit : unit+1]
			if mult == 1024 {
				suffix += "i"
			}
			return strconv.FormatFloat(float64(num)/float64(powi), 'f', precision, 64) + suffix
		}
	}
	s := strconv.FormatUint(abs, 10)
	if human > 0 {
		var b strings.Builder
		for i, ch := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(ch)
		}
		s = b.String()
	}
	if num < 0 {
		s = "-" + s
	}
	return s
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func itoa(i int) string { return strconv.Itoa(i) }
