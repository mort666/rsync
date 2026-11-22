package rsyncchecksum

import (
	"fmt"
	"os"
	"strings"

	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// Negotiate settles a session's strong checksum, as compat.c
// negotiate_the_strings and checksum.c parse_checksum_choice do, between
// the compat flags and the checksum seed.
//
// With --checksum-choice (choice, "xfer" or "xfer,file") neither side
// sends a list: both use the choice, which the client passes to the server.
// Otherwise, when the server set CF_VARINT_FLIST_FLAGS, each side sends its
// list (rsync's RSYNC_CHECKSUM_LIST can narrow and reorder it) and picks
// from the other's. Without either, the protocol's default applies.
func Negotiate(c *rsyncwire.Conn, choice string, isServer bool) error {
	if choice != "" {
		xfer, file, ok := strings.Cut(strings.ToLower(choice), ",")
		if !ok {
			file = xfer
		}
		for _, n := range []*string{&xfer, &file} {
			if *n == "auto" {
				*n = "" // the protocol's default
				continue
			}
			if !Supported(*n) {
				return fmt.Errorf("unknown checksum name in --checksum-choice: %q (supported: %s)", *n, strings.Join(Names, " "))
			}
		}
		c.Session.Checksum, c.Session.FileChecksum = xfer, file
		return nil
	}
	if c.Session.CompatFlags&rsyncwire.CfVarintFlistFlags == 0 {
		return nil
	}
	ours := rsyncwire.OurList(os.Getenv("RSYNC_CHECKSUM_LIST"), isServer, Names)
	name, err := c.NegotiateName("checksum", ours, isServer)
	if err != nil {
		return err
	}
	c.Session.Checksum, c.Session.FileChecksum = name, name
	return nil
}
