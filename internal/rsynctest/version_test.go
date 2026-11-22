package rsynctest

import "testing"

func TestVersionAtLeast(t *testing.T) {
	for _, tt := range []struct {
		have, want string
		ok         bool
	}{
		{"2.6.9", "3.0.0", false},
		{"3.0.9", "3.0.0", true},
		{"3.1.3", "3.2.0", false},
		{"3.2.7", "3.2.7", true},
		{"3.2.10", "3.2.7", true},
		{"3.5.2", "3.2.7", true},
		{"4.0.0dev-g90360b91", "3.2.7", true},
		{"4.0.0dev-g90360b91", "4.0.0", true},
		{"4.0", "3.9.9", true},
		{"3.4.0pre1", "3.4.0", true},
	} {
		if got := versionAtLeast(tt.have, tt.want); got != tt.ok {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tt.have, tt.want, got, tt.ok)
		}
	}
}
