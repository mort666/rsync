package safelink

import "testing"

func TestUnsafe(t *testing.T) {
	for _, tc := range []struct {
		dest, src string
		want      bool
	}{
		{"", "a", true},
		{"/etc/hostname", "a", true},
		{"file", "link", false},
		{"sub/file", "link", false},
		{"../x", "link", true},
		{"../x", "dir/link", false},
		{"../../x", "dir/link", true},
		{"..//..//x", "a/b/link", false},
		{"a/../x", "link", true},
		{"a/..", "dir/link", true},
		{"..", "dir/link", false},
		{"..", "link", true},
		{"./x", "link", false},
		{"../outside/o", "unsafe", true},
		{"../outside/o", "src/unsafe", false},
		{"x", "../link", false},
	} {
		if got := Unsafe(tc.dest, tc.src); got != tc.want {
			t.Errorf("Unsafe(%q, %q) = %v, want %v", tc.dest, tc.src, got, tc.want)
		}
	}
}
