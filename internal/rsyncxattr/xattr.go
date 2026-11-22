// Package rsyncxattr is rsync's xattrs.c: extended attributes (-X) in the
// file list, the generator's requests for values the file list abbreviated,
// and the sender's answers.
//
// Each file-list entry carries its xattr list after the --checksum sum: the
// index plus one of a list sent before, or 0 and the list itself, sorted by
// name. A value longer than 32 bytes travels as its MD5 sum. If the
// receiver does not have a value with that sum, the generator asks for it
// by its position in the sender's list, and the sender sends it before the
// file's data.
package rsyncxattr

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// Limits (xattrs.c, rsync.h).
const (
	maxFullDatum   = 32        // MAX_FULL_DATUM: longer values are abbreviated
	maxWireCount   = 65536     // MAX_WIRE_XATTR_COUNT
	maxWireNameLen = 4096      // MAX_WIRE_XATTR_NAMELEN, with the NUL
	maxValueBytes  = 128 << 20 // MAX_XATTR_VALUE_BYTES
	maxListBytes   = 512 << 20 // MAX_XATTR_LIST_BYTES
	sumLen         = md5.Size  // xattr_sum_len: MD5 from protocol 30
	userPrefix     = "user."
	systemPrefix   = "system."
	// rsyncPercent starts rsync's own names, such as --fake-super's
	// user.rsync.%stat, which only go across with -XX. On Linux rsync's
	// prefix (RSYNC_PREFIX) is in the user namespace.
	rsyncPercent = "user.rsync.%"
)

// Entry is one extended attribute.
type Entry struct {
	Name  string
	Len   int      // the value's length
	Value []byte   // the value, unless Abbrev
	Sum   [16]byte // the value's MD5 sum, when Abbrev
	Num   int      // 1-based position in the sender's list
}

// Abbrev reports whether the file list carries only the value's sum.
func (e Entry) Abbrev() bool { return e.Len > maxFullDatum }

// NewEntry is an entry for a value this side has, abbreviated as the file
// list would carry it.
func NewEntry(name string, value []byte) Entry {
	e := Entry{Name: name, Len: len(value)}
	if e.Abbrev() {
		e.Sum = md5.Sum(value)
	} else {
		e.Value = value
	}
	return e
}

// matches reports whether e, from the file list, has value.
func (e Entry) matches(value []byte) bool {
	if len(value) != e.Len {
		return false
	}
	if e.Abbrev() {
		return md5.Sum(value) == e.Sum
	}
	return bytes.Equal(value, e.Value)
}

// List is a file's extended attributes, sorted by name.
type List []Entry

// Sort orders l by name and numbers it, as rsync_xal_get does.
func (l List) Sort() {
	slices.SortFunc(l, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	for i := range l {
		l[i].Num = i + 1
	}
}

// key identifies a list's content for reuse.
func (l List) key() string {
	var b strings.Builder
	for _, e := range l {
		fmt.Fprintf(&b, "%q %d ", e.Name, e.Len)
		if e.Abbrev() {
			b.Write(e.Sum[:])
		} else {
			b.Write(e.Value)
		}
		b.WriteByte(0)
	}
	return b.String()
}

// Cache holds the lists one side sent or received, in order: an entry
// refers to an earlier list by its index. Both sides add a list for every
// list sent in full, so their indices agree.
type Cache struct {
	lists []List
	byKey map[string]int
}

// List returns the list with index ndx, or nil for -1.
func (c *Cache) List(ndx int) List {
	if ndx < 0 || ndx >= len(c.lists) {
		return nil
	}
	return c.lists[ndx]
}

func (c *Cache) store(l List) int {
	c.lists = append(c.lists, l)
	return len(c.lists) - 1
}

// AppendSend appends the file-list encoding of l to b (send_xattr) and
// returns l's index. l must be sorted (Sort).
func (c *Cache) AppendSend(b []byte, l List) ([]byte, int) {
	if c.byKey == nil {
		c.byKey = make(map[string]int)
	}
	k := l.key()
	if ndx, ok := c.byKey[k]; ok {
		return rsyncwire.AppendVarint(b, int32(ndx+1)), ndx
	}
	b = rsyncwire.AppendVarint(b, 0)
	b = rsyncwire.AppendVarint(b, int32(len(l)))
	for _, e := range l {
		b = rsyncwire.AppendVarint(b, int32(len(e.Name)+1))
		b = rsyncwire.AppendVarint(b, int32(e.Len))
		b = append(b, e.Name...)
		b = append(b, 0)
		if e.Abbrev() {
			b = append(b, e.Sum[:]...)
		} else {
			b = append(b, e.Value...)
		}
	}
	ndx := c.store(l)
	c.byKey[k] = ndx
	return b, ndx
}

// Receive reads a file-list entry's xattr list (receive_xattr) and returns
// its index. keep says which names this side keeps (KeepReceived); the
// list is stored whole, so indices stay in step with the sender's.
func (c *Cache) Receive(r io.Reader, keep func(name string) bool) (int, error) {
	ndx, err := rsyncwire.ReadVarint(r)
	if err != nil {
		return -1, err
	}
	if ndx < 0 || int(ndx) > len(c.lists) {
		return -1, fmt.Errorf("xattr list index %d out of range (%d lists)", ndx, len(c.lists))
	}
	if ndx != 0 {
		return int(ndx) - 1, nil
	}
	count, err := rsyncwire.ReadVarint(r)
	if err != nil {
		return -1, err
	}
	if count < 0 || count > maxWireCount {
		return -1, fmt.Errorf("xattr count %d out of range", count)
	}
	var l List
	total := 0
	for num := 1; num <= int(count); num++ {
		nameLen, err := rsyncwire.ReadVarint(r)
		if err != nil {
			return -1, err
		}
		valueLen, err := rsyncwire.ReadVarint(r)
		if err != nil {
			return -1, err
		}
		if nameLen < 1 || nameLen > maxWireNameLen {
			return -1, fmt.Errorf("xattr name length %d out of range", nameLen)
		}
		if valueLen < 0 || valueLen > maxValueBytes {
			return -1, fmt.Errorf("xattr value length %d out of range", valueLen)
		}
		total += int(nameLen) + int(valueLen)
		if total > maxListBytes {
			return -1, fmt.Errorf("xattr list over %d bytes", maxListBytes)
		}
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(r, name); err != nil {
			return -1, err
		}
		if name[nameLen-1] != 0 {
			return -1, fmt.Errorf("invalid xattr name received (missing trailing NUL)")
		}
		e := Entry{Name: string(name[:nameLen-1]), Len: int(valueLen), Num: num}
		if e.Abbrev() {
			if _, err := io.ReadFull(r, e.Sum[:]); err != nil {
				return -1, err
			}
		} else {
			e.Value = make([]byte, valueLen)
			if _, err := io.ReadFull(r, e.Value); err != nil {
				return -1, err
			}
		}
		if keep(e.Name) {
			l = append(l, e)
		}
	}
	// rsync re-sorts only where it renames; names are kept as sent here,
	// in the sender's (sorted) order.
	return c.store(l), nil
}

// Filter decides which names a side reads, keeps and replaces, as
// rsync does on Linux.
type Filter struct {
	// Level is how many -X were given: rsync.%-names need two.
	Level int
	// Root is whether this side runs as root (am_root). Without it, Linux
	// only lets it read and write the user namespace.
	Root bool
}

// KeepLocal reports whether the side reading its own files takes name
// (rsync_xal_get): a sender takes every namespace but system, which holds
// ACLs (-A); a receiver as root the same, otherwise only user names.
// rsync.%-names only go out with -XX.
func (f Filter) KeepLocal(name string, isSender bool) bool {
	userOnly := !isSender && !f.Root
	if userOnly {
		if !strings.HasPrefix(name, userPrefix) {
			return false
		}
	} else if strings.HasPrefix(name, systemPrefix) {
		return false
	}
	if isSender && f.Level < 2 && strings.HasPrefix(name, rsyncPercent) {
		return false
	}
	return true
}

// KeepReceived reports whether a receiver keeps a name from the file list
// (receive_xattr): as root every name, otherwise user names only, and
// rsync.%-names only with -XX.
func (f Filter) KeepReceived(name string) bool {
	if !f.Root && !strings.HasPrefix(name, userPrefix) {
		return false
	}
	if f.Level < 2 && strings.HasPrefix(name, rsyncPercent) {
		return false
	}
	return true
}

// Diff compares the sender's list for a file with the receiver's own
// values (xattr_diff with find_all): whether they differ, and the
// positions in the sender's list of abbreviated values the receiver does
// not have, which the generator asks for.
func Diff(sender List, local map[string][]byte) (differs bool, todo []int) {
	if len(sender) != len(local) {
		differs = true
	}
	for _, e := range sender {
		v, ok := local[e.Name]
		if ok && e.matches(v) {
			continue
		}
		differs = true
		if e.Abbrev() {
			todo = append(todo, e.Num)
		}
	}
	return differs, todo
}

// RequestFollows reports whether a request (generator to sender) or the
// values (sender to receiver) follow an item's attributes: with -X, for
// ITEM_REPORT_XATTR, unless protocol 31's optimisation says the receiver
// copies them from a local file (want_xattr_optim with ITEM_XNAME_FOLLOWS
// and ITEM_LOCAL_CHANGE).
func RequestFollows(iflags uint32, proto int32, avoidOptim bool) bool {
	const (
		reportXattr   = 1 << 8
		xnameAndLocal = 1<<12 | 1<<14
	)
	if iflags&reportXattr == 0 {
		return false
	}
	wantOptim := proto >= 31 && !avoidOptim
	return !(wantOptim && iflags&xnameAndLocal == xnameAndLocal)
}

// AppendRequest appends the generator's request for the values at nums
// (positions in the sender's list, in its order) to b, ending it with 0
// (send_xattr_request with no name).
func AppendRequest(b []byte, nums []int) []byte {
	prior := 0
	for _, n := range nums {
		b = rsyncwire.AppendVarint(b, int32(n-prior))
		prior = n
	}
	return append(b, 0)
}

// SenderState is the sender's progress on each list's abbreviated values
// (XSTATE_ABBREV, XSTATE_TODO, XSTATE_DONE): files that share a list share
// it, so a value goes to the receiver once.
type SenderState struct {
	state map[int]map[int]byte // list index → position → state
}

const (
	stateTodo = 1
	stateDone = 2
)

// ReadRequest reads what AppendRequest wrote (recv_xattr_request on the
// sender) for a file with list ndx, and marks the values asked for. Each
// must be an abbreviated value not asked for before.
func (s *SenderState) ReadRequest(r io.Reader, l List, ndx int) error {
	num := 0
	for {
		rel, err := rsyncwire.ReadVarint(r)
		if err != nil {
			return err
		}
		if rel == 0 {
			return nil
		}
		num += int(rel)
		i := slices.IndexFunc(l, func(e Entry) bool { return e.Num == num })
		if i < 0 {
			return fmt.Errorf("could not find xattr #%d", num)
		}
		if s.state == nil {
			s.state = make(map[int]map[int]byte)
		}
		if s.state[ndx] == nil {
			s.state[ndx] = make(map[int]byte)
		}
		if !l[i].Abbrev() || s.state[ndx][num] != 0 {
			return fmt.Errorf("internal abbrev error on xattr #%d (%s)", num, l[i].Name)
		}
		s.state[ndx][num] = stateTodo
	}
}

// AppendValues appends the sender's answer for a file with list ndx
// (send_xattr_request with a name): each value asked for and not yet sent,
// read again with value, then 0. A value that cannot be read goes as an
// empty one, as rsync sends it.
func (s *SenderState) AppendValues(b []byte, l List, ndx int, value func(name string) ([]byte, error)) ([]byte, error) {
	prior := 0
	var firstErr error
	for _, e := range l {
		if s.state[ndx][e.Num] != stateTodo {
			continue
		}
		s.state[ndx][e.Num] = stateDone
		b = rsyncwire.AppendVarint(b, int32(e.Num-prior))
		prior = e.Num
		v, err := value(e.Name)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to re-read xattr %s: %v", e.Name, err)
			}
			v = nil
		}
		b = rsyncwire.AppendVarint(b, int32(len(v)))
		b = append(b, v...)
	}
	return append(b, 0), firstErr
}

// GeneratorState remembers which lists' abbreviated values the generator
// has dealt with: asked for, or found the receiver has. It asks for each
// once per list (send_xattr_request with no name marks them done).
type GeneratorState struct {
	done map[int]bool // list index
}

// Todo narrows Diff's todo to values the generator has not dealt with.
func (g *GeneratorState) Todo(ndx int, todo []int) []int {
	if g.done[ndx] {
		return nil
	}
	return todo
}

// Done marks list ndx's values dealt with: the generator sent an item for
// a file with it (itemize, for ITEM_TRANSFER or ITEM_REPORT_XATTR).
func (g *GeneratorState) Done(ndx int) {
	if g.done == nil {
		g.done = make(map[int]bool)
	}
	g.done[ndx] = true
}

// ReadValues reads what AppendValues wrote (recv_xattr_request on the
// receiver): the full values, by their position in the sender's list.
func ReadValues(r io.Reader) (map[int][]byte, error) {
	values := make(map[int][]byte)
	num := 0
	for {
		rel, err := rsyncwire.ReadVarint(r)
		if err != nil {
			return nil, err
		}
		if rel == 0 {
			return values, nil
		}
		num += int(rel)
		n, err := rsyncwire.ReadVarint(r)
		if err != nil {
			return nil, err
		}
		if n < 0 || n > maxValueBytes {
			return nil, fmt.Errorf("xattr value length %d out of range", n)
		}
		v := make([]byte, n)
		if _, err := io.ReadFull(r, v); err != nil {
			return nil, err
		}
		// A receiver that dropped the name (not root) still reads it.
		values[num] = v
	}
}

// ReceiverState holds the full values a receiver learnt for each list's
// abbreviated entries, from the sender or from a file it already had, for
// later files with the same list (rsync replaces the sum in its list).
type ReceiverState struct {
	learnt map[int]map[int][]byte // list index → position → value
}

// Values is what a receiver writes for a file with list ndx: the full
// value of every entry, from the list, from fetched (by position), from
// what it learnt before, or from the basis file's own values when the sum
// matches (rsync_xal_set). It learns the abbreviated ones.
func (s *ReceiverState) Values(l List, ndx int, fetched map[int][]byte, basis map[string][]byte) (map[string][]byte, error) {
	if s.learnt == nil {
		s.learnt = make(map[int]map[int][]byte)
	}
	learnt := s.learnt[ndx]
	if learnt == nil {
		learnt = make(map[int][]byte)
		s.learnt[ndx] = learnt
	}
	out := make(map[string][]byte, len(l))
	var missing []string
	for _, e := range l {
		if !e.Abbrev() {
			out[e.Name] = e.Value
			continue
		}
		// Sent in full (empty if the sender could not read it again),
		// learnt for an earlier file, or the basis has it.
		v, ok := fetched[e.Num]
		if !ok {
			v, ok = learnt[e.Num]
		}
		if !ok {
			if bv, has := basis[e.Name]; has && e.matches(bv) {
				v, ok = bv, true
			}
		}
		if !ok {
			missing = append(missing, e.Name)
			continue
		}
		out[e.Name] = v
		learnt[e.Num] = v
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("missing abbreviated xattr value, %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// FromValues is the list a sender sends for values: sorted, numbered and
// abbreviated.
func FromValues(values map[string][]byte) List {
	l := make(List, 0, len(values))
	for name, v := range values {
		l = append(l, NewEntry(name, v))
	}
	l.Sort()
	return l
}
