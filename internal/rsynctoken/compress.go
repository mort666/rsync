package rsynctoken

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// Names are the compressors in rsync's order (compat.c valid_compressions).
// Both sides pick from the other's list by their own ranking, so a list may
// leave names out but must never reorder them.
var Names = []string{"zstd", "lz4", "zlibx", "zlib", "none"}

// receiveNames are the ones this side can read: all but zstd.
var receiveNames = []string{"lz4", "zlibx", "zlib", "none"}

// supported is the list for a side that sends (isSender) or receives.
func supported(isSender bool) []string {
	if isSender {
		return Names
	}
	return receiveNames
}

// Negotiate settles a session's compressor, as compat.c
// negotiate_the_strings and parse_compress_choice do, right after the
// checksum (rsyncchecksum.Negotiate). choice is what the options asked for
// (rsyncopts.Options.CompressChoice): "" for none, "auto" to negotiate, or
// a name, which the client passed on to the server and both sides use.
//
// Negotiation needs CF_VARINT_FLIST_FLAGS. Each side then sends its list
// (rsync's RSYNC_COMPRESS_LIST can narrow and reorder it) and picks from
// the other's; without it, the compressor is zlib, as before rsync 3.2.
//
// level is --compress-level, or a negative value for the compressor's
// default. A zlib level of 0 turns compression off, as it does in rsync.
func Negotiate(c *rsyncwire.Conn, choice string, level int, isServer, isSender bool) error {
	s := &c.Session
	s.Compress, s.CompressLevel = "", 0
	switch choice {
	case "", "none":
		return nil
	case "auto":
		if s.CompatFlags&rsyncwire.CfVarintFlistFlags == 0 {
			s.Compress = "zlib"
			break
		}
		ours := rsyncwire.OurList(os.Getenv("RSYNC_COMPRESS_LIST"), isServer, supported(isSender), supported(isSender))
		name, err := c.NegotiateName("compress", ours, isServer)
		if err != nil {
			return err
		}
		s.Compress = name
	default:
		if err := CheckChoice(choice, isSender); err != nil {
			return err
		}
		s.Compress = choice
	}
	if s.Compress == "none" {
		s.Compress = ""
		return nil
	}
	s.CompressLevel = compressionLevel(s.Compress, level)
	if s.CompressLevel < 0 {
		s.Compress, s.CompressLevel = "", 0
	}
	return nil
}

// CheckChoice reports whether a side that sends (isSender) or receives can
// use the compressor choice names (see rsyncopts.Options.CompressChoice).
func CheckChoice(choice string, isSender bool) error {
	switch choice {
	case "", "auto", "none":
		return nil
	}
	if !slices.Contains(Names, choice) {
		return fmt.Errorf("unknown compress name: %s (supported: %s)", choice, strings.Join(Names, " "))
	}
	if !slices.Contains(supported(isSender), choice) {
		return fmt.Errorf("gokrazy/rsync cannot receive %s-compressed data (it can receive: %s)", choice, strings.Join(receiveNames, " "))
	}
	return nil
}

// compressionLevel is token.c init_compression_level: the level for name,
// from the one asked for (negative: the default). It is negative when the
// level turns compression off.
func compressionLevel(name string, level int) int {
	switch name {
	case "zlib", "zlibx":
		switch {
		case level < 0:
			return 6
		case level == 0: // Z_NO_COMPRESSION turns it off
			return -1
		}
		return min(level, 9)
	case "zstd":
		if level <= 0 {
			return 3 // ZSTD_CLEVEL_DEFAULT
		}
		return min(level, 22)
	}
	return 0 // lz4 has no levels
}

// NewEncoder is the encoder for the session's compressor, writing to w.
func NewEncoder(s rsyncwire.Session, w io.Writer) (Encoder, error) {
	switch s.Compress {
	case "":
		return &simpleEncoder{w: w}, nil
	case "zlib", "zlibx":
		return newDeflateEncoder(w, s.Compress == "zlibx", s.Protocol, s.CompressLevel), nil
	case "lz4":
		return newLZ4Encoder(w), nil
	case "zstd":
		return newZstdEncoder(w, s.CompressLevel)
	}
	return nil, fmt.Errorf("BUG: unknown compressor %q", s.Compress)
}

// NewDecoder is the decoder for the session's compressor, reading from r.
// It reads no further than the end of each file's tokens.
func NewDecoder(s rsyncwire.Session, r io.Reader) (Decoder, error) {
	switch s.Compress {
	case "":
		return &simpleDecoder{r: r}, nil
	case "zlib", "zlibx":
		return newDeflateDecoder(r, s.Compress == "zlibx", s.Protocol), nil
	case "lz4":
		return newLZ4Decoder(r), nil
	}
	return nil, fmt.Errorf("cannot receive %s-compressed data", s.Compress)
}
