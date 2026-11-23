package rsyncwire

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// TimeoutError ends a transfer that has read and written nothing for
// --timeout seconds (io.c:check_timeout).
type TimeoutError struct{ Seconds int }

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("io timeout after %d seconds -- exiting", e.Seconds)
}

// IdleTimeout watches a connection's reads and writes, and closes it once
// it has been idle for the timeout, so that a read or write blocked on it
// returns: with a TimeoutError, through the Reader and Writer it wraps.
//
// rsync also sends keep-alive messages while it is busy, so that the far
// end's timeout does not fire; gokrazy/rsync does not yet.
type IdleTimeout struct {
	timeout  time.Duration
	closer   io.Closer
	last     atomic.Int64 // UnixNano of the last read or write
	timedOut atomic.Bool
	stopOnce sync.Once
	stop     chan struct{}
}

// NewIdleTimeout starts watching. closer closes the connection; Stop ends
// the watch.
func NewIdleTimeout(seconds int, closer io.Closer) *IdleTimeout {
	t := &IdleTimeout{
		timeout: time.Duration(seconds) * time.Second,
		closer:  closer,
		stop:    make(chan struct{}),
	}
	t.touch()
	go t.watch()
	return t
}

func (t *IdleTimeout) touch() { t.last.Store(time.Now().UnixNano()) }

func (t *IdleTimeout) watch() {
	tick := time.NewTicker(min(t.timeout/4, time.Second))
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case now := <-tick.C:
			if now.Sub(time.Unix(0, t.last.Load())) >= t.timeout {
				t.timedOut.Store(true)
				t.closer.Close()
				return
			}
		}
	}
}

// Stop ends the watch.
func (t *IdleTimeout) Stop() { t.stopOnce.Do(func() { close(t.stop) }) }

func (t *IdleTimeout) err(err error) error {
	if err != nil && t.timedOut.Load() {
		return &TimeoutError{Seconds: int(t.timeout / time.Second)}
	}
	return err
}

// Reader is r, counting its reads as activity.
func (t *IdleTimeout) Reader(r io.Reader) io.Reader { return idleReader{t, r} }

// Writer is w, counting its writes as activity.
func (t *IdleTimeout) Writer(w io.Writer) io.Writer { return idleWriter{t, w} }

type idleReader struct {
	t *IdleTimeout
	r io.Reader
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.t.touch()
	}
	return n, r.t.err(err)
}

type idleWriter struct {
	t *IdleTimeout
	w io.Writer
}

func (w idleWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		w.t.touch()
	}
	return n, w.t.err(err)
}
