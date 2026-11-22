package rsyncwire

import "fmt"

// DelStats counts what --delete removed, by type, as protocol 31 sends it
// after NDX_DEL_STATS (main.c:write_del_stats). Files are regular files
// only; rsync's own deleted_files is Total.
type DelStats struct {
	Files, Dirs, Symlinks, Devices, Specials int64
}

// Total is the number of entries deleted.
func (d DelStats) Total() int64 {
	return d.Files + d.Dirs + d.Symlinks + d.Devices + d.Specials
}

// maxWireDelStat is rsync's MAX_WIRE_DEL_STAT: the most a count may say.
const maxWireDelStat = 1<<31 - 1

// WriteDelStats sends NDX_DEL_STATS and the counts (protocol 31+).
func (c *Conn) WriteDelStats(d DelStats) error {
	if err := c.WriteNdx(NdxDelStats); err != nil {
		return err
	}
	for _, n := range []int64{d.Files, d.Dirs, d.Symlinks, d.Devices, d.Specials} {
		if err := c.WriteVarint(int32(min(n, maxWireDelStat))); err != nil {
			return err
		}
	}
	return nil
}

// readDelStats reads the counts that follow NDX_DEL_STATS
// (main.c:read_del_stats).
func (c *Conn) readDelStats() (DelStats, error) {
	var d DelStats
	for _, n := range []*int64{&d.Files, &d.Dirs, &d.Symlinks, &d.Devices, &d.Specials} {
		v, err := c.ReadVarint()
		if err != nil {
			return DelStats{}, err
		}
		if v < 0 {
			return DelStats{}, fmt.Errorf("invalid delete count %d", v)
		}
		*n = int64(v)
	}
	return d, nil
}
