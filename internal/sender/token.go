package sender

import (
	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsynctoken"
)

// encoder is the session's token encoder, made on first use: it keeps the
// compressor's state from file to file.
func (st *Transfer) encoder() (rsynctoken.Encoder, error) {
	if st.enc == nil {
		enc, err := rsynctoken.NewEncoder(st.Conn.Session, st.Conn.Writer)
		if err != nil {
			return nil, err
		}
		st.enc = enc
	}
	return st.enc, nil
}

// sendLiteral sends data as literal tokens alone (token -2), without
// ending the file.
func (st *Transfer) sendLiteral(data []byte) error {
	enc, err := st.encoder()
	if err != nil {
		return err
	}
	for len(data) > 0 {
		n := min(len(data), rsync.ChunkSize)
		if err := enc.SendToken(-2, data[:n], nil); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// endFile ends a file's tokens (token -1).
func (st *Transfer) endFile() error {
	enc, err := st.encoder()
	if err != nil {
		return err
	}
	return enc.SendToken(-1, nil, nil)
}

// rsync/token.c:send_token, called by match.c:matched: the n bytes of
// literal data at offset, then token i, the block of blockLen bytes at
// blockOffset (or -2 for literal data alone, -1 at the end of the file).
func (st *Transfer) sendToken(ms *mapStruct, i int32, offset, n, blockOffset, blockLen int64) error {
	if n > 0 && st.Opts.DebugGTE(rsyncopts.DEBUG_DELTASUM, 3) {
		st.Logger.Printf("sending unmatched chunks offset=%d, n=%d", offset, n)
	}
	enc, err := st.encoder()
	if err != nil {
		return err
	}
	// All but the last chunk go alone; the last goes with the token, so a
	// compressor sees the run end where rsync's would.
	var last []byte
	for l := int64(0); l < n; {
		n1 := min(int64(rsync.ChunkSize), n-l)
		chunk, err := ms.ptr(offset+l, int32(n1))
		if err != nil {
			return err
		}
		l += n1
		if l < n {
			if err := enc.SendToken(-2, chunk, nil); err != nil {
				return err
			}
			continue
		}
		// Reading the block may move the file window under chunk.
		last = append(st.litBuf[:0], chunk...)
		st.litBuf = last
	}
	var block []byte
	if i >= 0 && st.Conn.Session.Compress == "zlib" {
		// zlib puts matched blocks into the compressor's history.
		if block, err = ms.ptr(blockOffset, int32(blockLen)); err != nil {
			return err
		}
	}
	return enc.SendToken(i, last, block)
}
