package rsyncxattr

import (
	"bytes"
	"crypto/md5"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

var long40 = bytes.Repeat([]byte("x"), 40)

// The file-list encoding, byte for byte (send_xattr): index+1 (0 for a new
// list), the count, then per entry the name's length with its NUL, the
// value's length, the name and the value, or the MD5 sum of a value over
// 32 bytes.
func TestAppendSendBytes(t *testing.T) {
	var c Cache
	l := FromValues(map[string][]byte{"user.b": long40, "user.a": []byte("v")})
	got, ndx := c.AppendSend(nil, l)
	sum := md5.Sum(long40)
	want := slices.Concat(
		[]byte{0, 2},
		[]byte{7, 1}, []byte("user.a\x00"), []byte("v"),
		[]byte{7, 40}, []byte("user.b\x00"), sum[:],
	)
	if ndx != 0 || !bytes.Equal(got, want) {
		t.Errorf("AppendSend = %x (index %d), want %x (index 0)", got, ndx, want)
	}
	// The same list again is a reference to it.
	got, ndx = c.AppendSend(nil, FromValues(map[string][]byte{"user.a": []byte("v"), "user.b": long40}))
	if ndx != 0 || !bytes.Equal(got, []byte{1}) {
		t.Errorf("same list again = %x (index %d), want 01 (index 0)", got, ndx)
	}
	got, ndx = c.AppendSend(nil, nil)
	if ndx != 1 || !bytes.Equal(got, []byte{0, 0}) {
		t.Errorf("empty list = %x (index %d), want 0000 (index 1)", got, ndx)
	}
}

// What one side sends, the other reads back, with the same indices.
func TestSendReceive(t *testing.T) {
	var send, recv Cache
	lists := []List{
		FromValues(map[string][]byte{"user.a": []byte("1"), "user.big": long40}),
		FromValues(map[string][]byte{"user.a": []byte("1"), "user.big": long40}),
		nil,
		FromValues(map[string][]byte{"security.selinux": []byte("ctx"), "user.c": nil}),
	}
	var wire []byte
	var ndxs []int
	for _, l := range lists {
		var ndx int
		wire, ndx = send.AppendSend(wire, l)
		ndxs = append(ndxs, ndx)
	}
	r := bytes.NewReader(wire)
	keep := Filter{Level: 1, Root: true}.KeepReceived
	for i, l := range lists {
		ndx, err := recv.Receive(r, keep)
		if err != nil {
			t.Fatal(err)
		}
		if ndx != ndxs[i] {
			t.Errorf("list %d: index %d, sender's %d", i, ndx, ndxs[i])
		}
		got := recv.List(ndx)
		if len(got) != len(l) {
			t.Fatalf("list %d: %d entries, want %d", i, len(got), len(l))
		}
		for j := range l {
			if got[j].Name != l[j].Name || got[j].Len != l[j].Len || got[j].Num != j+1 || got[j].Sum != l[j].Sum || !bytes.Equal(got[j].Value, l[j].Value) {
				t.Errorf("list %d entry %d: %+v, want %+v", i, j, got[j], l[j])
			}
		}
	}
	if r.Len() != 0 {
		t.Errorf("%d bytes left over", r.Len())
	}
}

func TestFilter(t *testing.T) {
	for _, tt := range []struct {
		f                    Filter
		name                 string
		sender, local, recvd bool
	}{
		{Filter{Level: 1}, "user.a", true, true, true},
		{Filter{Level: 1}, "security.selinux", true, false, false},
		{Filter{Level: 1, Root: true}, "security.selinux", true, true, true},
		{Filter{Level: 1, Root: true}, "trusted.x", true, true, true},
		{Filter{Level: 1, Root: true}, "system.posix_acl_access", false, false, true},
		{Filter{Level: 1}, "user.rsync.%stat", false, true, false},
		{Filter{Level: 2}, "user.rsync.%stat", true, true, true},
		{Filter{Level: 1}, "user.rsync.plain", true, true, true},
	} {
		if got := tt.f.KeepLocal(tt.name, true); got != tt.sender {
			t.Errorf("%+v KeepLocal(%q, sender) = %v, want %v", tt.f, tt.name, got, tt.sender)
		}
		if got := tt.f.KeepLocal(tt.name, false); got != tt.local {
			t.Errorf("%+v KeepLocal(%q, receiver) = %v, want %v", tt.f, tt.name, got, tt.local)
		}
		if got := tt.f.KeepReceived(tt.name); got != tt.recvd {
			t.Errorf("%+v KeepReceived(%q) = %v, want %v", tt.f, tt.name, got, tt.recvd)
		}
	}
}

// A receiver that drops names still numbers entries by the sender's list,
// so requests name the sender's positions.
func TestReceiveKeepsSenderNumbers(t *testing.T) {
	var send, recv Cache
	wire, _ := send.AppendSend(nil, FromValues(map[string][]byte{"security.a": []byte("1"), "user.b": long40}))
	ndx, err := recv.Receive(bytes.NewReader(wire), Filter{Level: 1}.KeepReceived)
	if err != nil {
		t.Fatal(err)
	}
	l := recv.List(ndx)
	if len(l) != 1 || l[0].Name != "user.b" || l[0].Num != 2 {
		t.Errorf("list %+v, want user.b alone as #2", l)
	}
}

func TestReceiveRejects(t *testing.T) {
	keep := func(string) bool { return true }
	for _, tt := range []struct {
		name string
		wire []byte
		want string
	}{
		{"index past the lists", []byte{5}, "out of range"},
		{"negative count", append([]byte{0}, rsyncwire.AppendVarint(nil, -1)...), "out of range"},
		{"count over the limit", append([]byte{0}, rsyncwire.AppendVarint(nil, maxWireCount+1)...), "out of range"},
		{"value over the limit", slices.Concat([]byte{0, 1, 7}, rsyncwire.AppendVarint(nil, maxValueBytes+1)), "out of range"},
		{"name without NUL", slices.Concat([]byte{0, 1, 2, 1}, []byte("ab"), []byte("v")), "trailing NUL"},
		{"empty name", []byte{0, 1, 0, 0}, "name length"},
		{"truncated", []byte{0, 1, 7, 1, 'u'}, "EOF"},
	} {
		var c Cache
		_, err := c.Receive(bytes.NewReader(tt.wire), keep)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error with %q", tt.name, err, tt.want)
		}
	}
}

func TestDiff(t *testing.T) {
	l := FromValues(map[string][]byte{"user.a": []byte("1"), "user.big": long40})
	other := bytes.Clone(long40)
	other[3] = 'y'
	for _, tt := range []struct {
		name    string
		local   map[string][]byte
		differs bool
		todo    []int
	}{
		{"same", map[string][]byte{"user.a": []byte("1"), "user.big": long40}, false, nil},
		{"none here", nil, true, []int{2}},
		{"small differs", map[string][]byte{"user.a": []byte("2"), "user.big": long40}, true, nil},
		{"big differs", map[string][]byte{"user.a": []byte("1"), "user.big": other}, true, []int{2}},
		{"extra here", map[string][]byte{"user.a": []byte("1"), "user.big": long40, "user.z": nil}, true, nil},
	} {
		differs, todo := Diff(l, tt.local)
		if differs != tt.differs || !slices.Equal(todo, tt.todo) {
			t.Errorf("%s: Diff = %v, %v; want %v, %v", tt.name, differs, todo, tt.differs, tt.todo)
		}
	}
}

// The request round trip: the generator asks, the sender marks and
// answers once per list, the receiver reads and learns.
func TestRequestRoundTrip(t *testing.T) {
	l := FromValues(map[string][]byte{"user.a": long40, "user.b": []byte("small"), "user.c": bytes.Repeat([]byte("c"), 50)})
	req := AppendRequest(nil, []int{1, 3})
	if !bytes.Equal(req, []byte{1, 2, 0}) {
		t.Errorf("request = %x, want 010200", req)
	}
	var s SenderState
	if err := s.ReadRequest(bytes.NewReader(req), l, 0); err != nil {
		t.Fatal(err)
	}
	values := map[string][]byte{"user.a": long40, "user.c": bytes.Repeat([]byte("c"), 50)}
	ans, err := s.AppendValues(nil, l, 0, func(name string) ([]byte, error) { return values[name], nil })
	if err != nil {
		t.Fatal(err)
	}
	// Asked again for the same list: rsync's sender calls it an error.
	if err := s.ReadRequest(bytes.NewReader(AppendRequest(nil, []int{1})), l, 0); err == nil || !strings.Contains(err.Error(), "internal abbrev error") {
		t.Errorf("second request: %v, want an abbrev error", err)
	}
	// A small value is never asked for.
	if err := s.ReadRequest(bytes.NewReader(AppendRequest(nil, []int{2})), l, 1); err == nil {
		t.Errorf("request for a small value: no error")
	}
	fetched, err := ReadValues(bytes.NewReader(ans))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fetched[1], long40) || len(fetched[3]) != 50 || len(fetched) != 2 {
		t.Errorf("fetched %v", fetched)
	}
	var rs ReceiverState
	got, err := rs.Values(l, 0, fetched, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"user.a": long40, "user.b": []byte("small"), "user.c": bytes.Repeat([]byte("c"), 50)}
	if !maps.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("Values = %v", got)
	}
	// A later file with the same list needs no request.
	if got, err := rs.Values(l, 0, nil, nil); err != nil || !maps.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("Values from what was learnt = %v, %v", got, err)
	}
	// Another list has learnt nothing, but the basis has the value.
	if got, err := rs.Values(l, 1, nil, map[string][]byte{"user.a": long40, "user.c": bytes.Repeat([]byte("c"), 50)}); err != nil || !maps.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("Values from the basis = %v, %v", got, err)
	}
	if _, err := rs.Values(l, 2, nil, map[string][]byte{"user.a": []byte("wrong")}); err == nil || !strings.Contains(err.Error(), "missing abbreviated") {
		t.Errorf("Values without the value: %v", err)
	}
	var g GeneratorState
	if got := g.Todo(0, []int{1}); !slices.Equal(got, []int{1}) {
		t.Errorf("Todo before Done = %v", got)
	}
	g.Done(0)
	if got := g.Todo(0, []int{1}); got != nil {
		t.Errorf("Todo after Done = %v", got)
	}
}

func TestRequestFollows(t *testing.T) {
	const report, xnameLocal = 1 << 8, 1<<12 | 1<<14
	for _, tt := range []struct {
		iflags uint32
		proto  int32
		avoid  bool
		want   bool
	}{
		{0, 31, false, false},
		{report, 30, false, true},
		{report, 31, false, true},
		{report | xnameLocal, 31, false, false},
		{report | xnameLocal, 30, false, true},
		{report | xnameLocal, 31, true, true},
	} {
		if got := RequestFollows(tt.iflags, tt.proto, tt.avoid); got != tt.want {
			t.Errorf("RequestFollows(%#x, %d, %v) = %v, want %v", tt.iflags, tt.proto, tt.avoid, got, tt.want)
		}
	}
}

// FuzzReceive feeds arbitrary bytes to the file-list reader: an error, never
// a panic or a huge allocation from a small input.
func FuzzReceive(f *testing.F) {
	var c Cache
	wire, _ := c.AppendSend(nil, FromValues(map[string][]byte{"user.a": long40, "user.b": []byte("v")}))
	f.Add(wire)
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, b []byte) {
		var c Cache
		r := bytes.NewReader(b)
		for range 4 {
			if _, err := c.Receive(r, func(string) bool { return true }); err != nil {
				return
			}
		}
	})
}
