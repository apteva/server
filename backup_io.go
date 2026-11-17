package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"modernc.org/sqlite"
)

// Shared by management and app callbacks, including separate Backup installs.
var platformBackupMu sync.Mutex

const backupDiskReserve = int64(512 << 20)

func backupSpace(path string, additional int64) error {
	free, err := runtimeStateDiskFree(path)
	if err != nil {
		return err
	}
	return checkBackupSpace(free, additional)
}

func checkBackupSpace(free, additional int64) error {
	if additional < 0 || free < backupDiskReserve || additional > free-backupDiskReserve {
		return fmt.Errorf("backup needs %d bytes plus %d bytes disk reserve; %d available", additional, backupDiskReserve, free)
	}
	return nil
}

func backupPause(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Incremental backups avoid VACUUM's full rebuild and release the source lock
// between steps in rollback mode. WAL readers pin a consistent view so active
// writers cannot continuously restart the copy. Never use a pooled write
// connection for that read transaction (_txlock=immediate would reserve writes).
func onlineBackupDB(ctx context.Context, db *sql.DB, dst string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			os.Remove(dst)
			os.Remove(dst + "-wal")
			os.Remove(dst + "-shm")
		}
	}()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var seq int
	var schema, sourcePath string
	if err = conn.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &schema, &sourcePath); err != nil {
		return err
	}
	if sourcePath != "" {
		if err = backupSpace(filepath.Dir(sourcePath), 0); err != nil {
			return err
		}
	}
	var mode string
	var pages, pageSize int64
	if err = conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if mode == "wal" {
		if _, err = conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
	}
	if err = conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return err
	}
	if err = conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return err
	}
	if pages < 0 || pageSize <= 0 || pages > maxRestoreEntryBytes/pageSize {
		return fmt.Errorf("database exceeds recovery file limit")
	}
	if err = backupSpace(filepath.Dir(dst), pages*pageSize); err != nil {
		return err
	}
	err = conn.Raw(func(raw any) (result error) {
		source, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite driver does not support online backup")
		}
		b, e := source.NewBackup((&url.URL{Scheme: "file", Path: dst}).String())
		if e != nil {
			return e
		}
		defer func() {
			if e := b.Finish(); result == nil {
				result = e
			}
		}()
		for {
			if e = ctx.Err(); e != nil {
				return e
			}
			if sourcePath != "" {
				if e = backupSpace(filepath.Dir(sourcePath), 0); e != nil {
					return e
				}
			}
			if e = backupSpace(filepath.Dir(dst), 2<<20); e != nil {
				return e
			}
			more, e := b.Step(int32(max(1, (1<<20)/pageSize)))
			if e != nil {
				var se *sqlite.Error
				if !errors.As(e, &se) || (se.Code()&255 != 5 && se.Code()&255 != 6) {
					return e
				}
				if e = backupPause(ctx, 10*time.Millisecond); e != nil {
					return e
				}
				continue
			}
			if !more {
				return nil
			}
			if e = backupPause(ctx, time.Millisecond); e != nil {
				return e
			}
		}
	})
	completed = err == nil
	return err
}

// Staged input is private and immutable. Link it on the destination filesystem
// when possible, avoiding another full restore copy; EXDEV falls back to copy.
func backupStageFile(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(src, dst); err != nil {
		if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ENOTSUP) {
			return err
		}
		// Do not overwrite a case-folding collision or another staged file.
		f, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		if e = f.Close(); e != nil {
			return e
		}
		return backupCopyFile(ctx, src, dst)
	}
	f, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func onlineBackupPath(ctx context.Context, src, dst string) error {
	u := &url.URL{Scheme: "file", Path: src, RawQuery: "mode=ro&_pragma=busy_timeout(100)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	return onlineBackupDB(ctx, db, dst)
}

// A bounded copy buffer also prevents io.Copy fast paths from bypassing
// cancellation. Yield periodically to leave CPU and I/O time for live traffic.
type backupReader struct {
	ctx   context.Context
	in    io.Reader
	since int64
}

func (r *backupReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if r.since >= 4<<20 {
		if e := backupPause(r.ctx, time.Millisecond); e != nil {
			return 0, e
		}
		r.since = 0
	}
	if len(p) > 128<<10 {
		p = p[:128<<10]
	}
	n, e := r.in.Read(p)
	r.since += int64(n)
	return n, e
}

type backupDiskWriter struct {
	ctx   context.Context
	out   io.Writer
	path  string
	since int64
}

func (w *backupDiskWriter) Write(p []byte) (int, error) {
	if e := w.ctx.Err(); e != nil {
		return 0, e
	}
	if w.since == 0 || w.since >= 4<<20 {
		if e := backupSpace(w.path, int64(len(p))); e != nil {
			return 0, e
		}
		w.since = 0
	}
	n, e := w.out.Write(p)
	w.since += int64(n)
	return n, e
}

func backupCopyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err = backupSpace(filepath.Dir(dst), info.Size()); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(&backupDiskWriter{ctx: ctx, out: out, path: filepath.Dir(dst)}, &backupReader{ctx: ctx, in: in})
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

// filepath.WalkDir reads/sorts a whole directory before visiting its files.
// Stream names in batches so an over-limit directory cannot allocate memory
// proportional to millions of entries before our inventory check can reject it.
func walkBackupTree(ctx context.Context, root string, visit fs.WalkDirFunc) error {
	info, err := os.Lstat(root)
	if err != nil {
		return visit(root, nil, err)
	}
	var walk func(string, fs.DirEntry) error
	walk = func(path string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(path, entry, nil); err != nil {
			if err == filepath.SkipDir {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		dir, err := os.Open(path)
		if err != nil {
			return err
		}
		defer dir.Close()
		for {
			entries, err := dir.ReadDir(128)
			if err != nil && err != io.EOF {
				return err
			}
			for _, child := range entries {
				if err := walk(filepath.Join(path, child.Name()), child); err != nil {
					return err
				}
			}
			if err == io.EOF {
				return nil
			}
		}
	}
	return walk(root, fs.FileInfoToDirEntry(info))
}
