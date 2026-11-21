package rsyncwire_test

import (
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

func TestNegotiateProtocol(t *testing.T) {
	for _, tt := range []struct {
		ours, remote int32
		want         int32
		wantErr      string
	}{
		{ours: 27, remote: 27, want: 27},
		// A newer peer comes down to our version.
		{ours: 27, remote: 31, want: 27},
		{ours: 27, remote: 40, want: 27},
		{ours: 27, remote: 26, wantErr: "older than the oldest"},
		{ours: 27, remote: 41, wantErr: "is your shell clean"},
		{ours: 27, remote: 0, wantErr: "is your shell clean"},
		// "@RSY" read as a little-endian int32: a daemon greeting where a
		// binary version was expected.
		{ours: 27, remote: 0x59535240, wantErr: "is your shell clean"},
		{ours: 26, remote: 31, wantErr: "--protocol must be at least 27 on the Client"},
		{ours: 28, remote: 31, wantErr: "--protocol must be no more than 27 on the Client"},
	} {
		got, err := rsyncwire.NegotiateProtocol("Client", tt.ours, tt.remote)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NegotiateProtocol(%d, %d) = %d, %v; want error containing %q", tt.ours, tt.remote, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NegotiateProtocol(%d, %d) = %d, %v; want %d", tt.ours, tt.remote, got, err, tt.want)
		}
	}
}
