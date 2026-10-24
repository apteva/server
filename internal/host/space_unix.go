//go:build darwin || linux

package host

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// Preflight keeps one consistent backup and one migration clone. Reserve
// headroom for their WALs and ongoing application writes before starting.
func preflightSpace(db, destination string) error {
	info, err := os.Stat(db)
	if err != nil {
		return err
	}
	size := info.Size()
	if wal, e := os.Stat(db + "-wal"); e == nil {
		size += wal.Size()
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(filepath.Dir(destination), &stat); err != nil {
		return err
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	required := uint64(size)*3 + 256<<20
	if available < required {
		return fmt.Errorf("insufficient free disk space for preflight: need %d bytes, have %d; current server retained", required, available)
	}
	return nil
}
