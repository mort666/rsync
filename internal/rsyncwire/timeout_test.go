package rsyncwire

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestIdleTimeout(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	idle := NewIdleTimeout(1, a)
	defer idle.Stop()
	start := time.Now()
	_, err := idle.Reader(a).Read(make([]byte, 1))
	var te *TimeoutError
	if !errors.As(err, &te) || te.Seconds != 1 {
		t.Fatalf("Read = %v, want a TimeoutError of 1 s", err)
	}
	if d := time.Since(start); d < time.Second || d > 3*time.Second {
		t.Errorf("timed out after %v, want about 1 s", d)
	}
	if got, want := err.Error(), "io timeout after 1 seconds -- exiting"; got != want {
		t.Errorf("message %q, want rsync's %q", got, want)
	}
}

// Traffic keeps it alive.
func TestIdleTimeoutActive(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	idle := NewIdleTimeout(1, a)
	defer idle.Stop()
	go func() {
		for range 8 {
			b.Write([]byte{1})
			time.Sleep(300 * time.Millisecond)
		}
		b.Close()
	}()
	r := idle.Reader(a)
	buf := make([]byte, 1)
	for {
		if _, err := r.Read(buf); err != nil {
			var te *TimeoutError
			if errors.As(err, &te) {
				t.Fatalf("timed out despite traffic")
			}
			return
		}
	}
}
