package rsyncopts

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncostest"
)

// parseSizeArg reads sizes as rsync's parse_size_arg does.
func TestParseSizeArg(t *testing.T) {
	for _, tt := range []struct {
		arg     string
		max     int64
		want    int64
		wantErr string
	}{
		{arg: "700", max: -1, want: 700},
		{arg: "0", max: -1, want: 0},
		{arg: "8k", max: -1, want: 8192},
		{arg: "8K", max: -1, want: 8192},
		{arg: "1.5k", max: -1, want: 1536},
		{arg: ".5k", max: -1, want: 512},
		{arg: "2kb", max: -1, want: 2000},
		{arg: "2KB", max: -1, want: 2000},
		{arg: "1KiB", max: -1, want: 1024},
		{arg: "1k-1", max: -1, want: 1023},
		{arg: "1k+1", max: -1, want: 1025},
		{arg: "3m", max: -1, want: 3 << 20},
		{arg: "1g", max: -1, want: 1 << 30},
		{arg: "12b", max: -1, want: 12},
		{arg: "128k", max: 1 << 17, want: 1 << 17},
		{arg: "128k+1", max: 1 << 17, wantErr: "too large (max: 131072)"},
		{arg: "200k", max: 1 << 17, wantErr: "too large"},
		{arg: "", max: -1, wantErr: "invalid"},
		{arg: "k", max: -1, wantErr: "invalid"},
		{arg: "8x", max: -1, wantErr: "invalid"},
		{arg: "8kx", max: -1, wantErr: "invalid"},
		{arg: "8k+2", max: -1, wantErr: "invalid"},
		{arg: "-1", max: -1, wantErr: "invalid"},
		{arg: "99999999999999999999", max: -1, wantErr: "too large"},
	} {
		got, err := parseSizeArg(tt.arg, 'b', "block-size", 0, tt.max, false)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseSizeArg(%q) = %d, %v; want an error with %q", tt.arg, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("parseSizeArg(%q) = %d, %v; want %d", tt.arg, got, err, tt.want)
		}
	}
	if _, err := parseSizeArg("10", 'b', "min-size", 100, -1, false); err == nil || !strings.Contains(err.Error(), "too small (min: 100)") {
		t.Errorf("below the minimum: %v", err)
	}
	if got, err := parseSizeArg("0", 'b', "max-size", 100, -1, true); err != nil || got != 0 {
		t.Errorf("0 for unlimited: %d, %v", got, err)
	}
}

// --block-size reaches the server as -B<n>, checked against the protocol's
// largest block; --compress-threads is accepted and stays on this side.
func TestBlockSizeAndCompressThreads(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		block   int32
		threads int
		wantErr string
	}{
		{args: nil},
		{args: []string{"-B", "4096"}, block: 4096},
		{args: []string{"--block-size=8k"}, block: 8192},
		{args: []string{"-B4k"}, block: 4096},
		{args: []string{"-B", "200k"}, wantErr: "too large"},
		{args: []string{"--protocol=29", "-B", "200k"}, block: 200 << 10},
		{args: []string{"--compress-threads=4"}, threads: 4},
		{args: []string{"--zt=2", "-z"}, threads: 2},
		{args: []string{"--zt=-3"}, threads: 0},
	} {
		for _, gokrazy := range []bool{true, false} {
			osenv := rsyncostest.New(t)
			opts := NewOptions(osenv)
			if gokrazy {
				opts = NewOptionsWithGokrazyDefaults(osenv)
			}
			pc := NewContext(opts)
			err := pc.ParseArguments(osenv, append([]string{"-r"}, tt.args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("%q: %v, want an error with %q", tt.args, err, tt.wantErr)
				}
				continue
			}
			if err != nil {
				t.Fatalf("gokrazy=%v %q: %v", gokrazy, tt.args, err)
			}
			o := pc.Options
			if o.BlockSize() != tt.block || o.CompressThreads() != tt.threads {
				t.Errorf("%q: BlockSize %d, CompressThreads %d; want %d, %d", tt.args, o.BlockSize(), o.CompressThreads(), tt.block, tt.threads)
			}
			sargs := o.ServerOptions()
			if want := tt.block != 0; slices.ContainsFunc(sargs, func(a string) bool { return strings.HasPrefix(a, "-B") }) != want {
				t.Errorf("%q: server options %q, want -B: %v", tt.args, sargs, want)
			}
			if tt.block != 0 && !slices.Contains(sargs, fmt.Sprintf("-B%d", tt.block)) {
				t.Errorf("%q: server options %q, want -B%d", tt.args, sargs, tt.block)
			}
			if slices.ContainsFunc(sargs, func(a string) bool { return strings.Contains(a, "threads") || strings.HasPrefix(a, "--zt") }) {
				t.Errorf("%q: server options %q pass --compress-threads on", tt.args, sargs)
			}
		}
	}
}
