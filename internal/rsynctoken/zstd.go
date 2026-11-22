package rsynctoken

import (
	"bufio"
	"io"

	"github.com/klauspost/compress/zstd"
)

// zstd, sending only (send_zstd_token).
//
// rsync keeps one zstd stream for the whole session: one frame that never
// ends, with a flush at the end of each literal run. Writing that is easy.
// Reading it is not: the decoder here would have to stop at each flush
// without reading on, which klauspost/compress/zstd cannot do. So
// gokrazy/rsync only offers zstd when it sends (see Names).

type zstdEncoder struct {
	runWriter
	zw *zstd.Encoder
}

// frameWriter writes what the compressor produces as DEFLATED_DATA frames.
type frameWriter struct{ rw *runWriter }

func (f frameWriter) Write(p []byte) (int, error) {
	f.rw.data(p)
	return len(p), nil
}

func newZstdEncoder(w io.Writer, level int) (*zstdEncoder, error) {
	e := &zstdEncoder{runWriter: runWriter{w: bufio.NewWriterSize(w, 64*1024)}}
	e.reset()
	// One goroutine: the encoder then writes its output during Write and
	// Flush, never behind the token writer's back.
	zw, err := zstd.NewWriter(frameWriter{&e.runWriter},
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		// rsync's decoder takes windows up to 128 MiB
		// (ZSTD_WINDOWLOG_LIMIT_DEFAULT); stay well below.
		zstd.WithWindowSize(8<<20),
	)
	if err != nil {
		return nil, err
	}
	e.zw = zw
	return e, nil
}

func (e *zstdEncoder) SendToken(token int32, lit, _ []byte) error {
	e.token(token, len(lit))
	if len(lit) != 0 || e.flushPending {
		if _, err := e.zw.Write(lit); err != nil {
			return err
		}
		if token != -2 {
			if err := e.zw.Flush(); err != nil {
				return err
			}
		}
		e.flushPending = token == -2
	}
	return e.end(token)
}
