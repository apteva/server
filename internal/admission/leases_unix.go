//go:build linux || darwin

package admission

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// File locks are process-owned leases: a crash releases the slot immediately.
// Never unlink slot files: another process may still hold the inode's lock.
func takeLease(dir, key string, limit int) (func(), error) {
	if dir == "" {
		return func() {}, nil
	}
	sum := sha256.Sum256([]byte(key))
	base := filepath.Join(dir, fmt.Sprintf("%x", sum[:16]))
	if err := os.MkdirAll(base, 0700); err != nil {
		return nil, err
	}
	for i := 0; i < limit; i++ {
		f, err := os.OpenFile(filepath.Join(base, fmt.Sprint(i)), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, err
		}
	}
	return nil, nil
}
