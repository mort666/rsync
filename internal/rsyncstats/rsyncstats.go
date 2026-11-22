package rsyncstats

type TransferStats struct {
	Read    int64 // total bytes read (from network connection)
	Written int64 // total bytes written (to network connection)
	Size    int64 // total size of files

	// Deleted is how many entries the receiver's --delete removed, as a
	// protocol 31 sender reports it to a receiving client.
	Deleted int64
}
