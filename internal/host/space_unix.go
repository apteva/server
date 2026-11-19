//go:build darwin || linux

package host

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

// preflightSpace requires room for copies full copies of the database (with
// its WAL) plus headroom for the check's own files and ongoing writes.
func preflightSpace(db, destination string, copies int, headroom uint64) error {
	info, err := os.Stat(db)
	if err != nil {
		return err
	}
	size := info.Size()
	if wal, e := os.Stat(db + "-wal"); e == nil {
		size += wal.Size()
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(destination, &stat); err != nil {
		return err
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	required := uint64(size)*uint64(copies) + headroom
	if available < required {
		return fmt.Errorf("insufficient free disk space for preflight: need %d bytes, have %d; current server retained", required, available)
	}
	return nil
}
