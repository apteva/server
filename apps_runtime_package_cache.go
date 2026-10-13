package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func (m *appRuntimeManager) packageCacheRoot() (string, error) {
	if m.s.localApps == nil {
		return "", fmt.Errorf("package preparation needs a configured local build cache")
	}
	root := filepath.Join(m.s.localApps.cacheDir, ".runtime-packages")
	return root, os.MkdirAll(root, 0700)
}
func runtimePackageLock(ctx context.Context, filename string) (func(), error) {
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func runtimePackageContained(root, rel string) (string, error) {
	if !runtimePackagePath(rel) {
		return "", fmt.Errorf("invalid package path")
	}
	p := root
	for _, part := range strings.Split(rel, "/") {
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("package paths cannot contain symlinks")
		}
	}
	return p, nil
}
func runtimePackageMetadataHash(meta runtimePackageMetadata) string {
	meta.ID = ""
	return runtimeHash([]byte(runtimeJSON(meta)))
}
func verifyRuntimePreparedPackage(root string, p runtimePreparedPackage) error {
	if p.MetadataSHA256 != runtimePackageMetadataHash(p.Metadata) || p.Metadata.ID != "pkg-"+p.MetadataSHA256 || !runtimeDigest(p.MetadataSHA256) || len(p.Metadata.Files) == 0 || len(p.Metadata.Files) > 1024 {
		return fmt.Errorf("invalid immutable package metadata")
	}
	var total int64
	seen := map[string]bool{}
	found := false
	for _, file := range p.Metadata.Files {
		if seen[file.Path] || file.Size < 0 || file.Size > runtimePackageFileLimit || !runtimeDigest(file.SHA256) {
			return fmt.Errorf("invalid package file metadata")
		}
		seen[file.Path] = true
		filename, err := runtimePackageContained(root, file.Path)
		if err != nil {
			return err
		}
		info, err := os.Stat(filename)
		if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
			return fmt.Errorf("package file size or type mismatch")
		}
		digest, err := artifactFileHash(filename)
		if err != nil || digest != file.SHA256 {
			return fmt.Errorf("package file checksum mismatch")
		}
		if file.Path == p.Metadata.Executable {
			found = digest == p.ExecutableSHA256
		}
		total += file.Size
	}
	if !found || total > runtimePackageLimit {
		return fmt.Errorf("package missing verified executable or exceeds size limit")
	}
	return nil
}
func (m *appRuntimeManager) cachedRuntimePackage(buildKey string) (runtimePreparedPackage, error) {
	var p runtimePreparedPackage
	var raw string
	err := m.s.store.db.QueryRow(`SELECT b.record_json FROM app_runtime_package_builds b JOIN app_runtime_packages p ON p.id=b.package_id WHERE b.build_key=?`, buildKey).Scan(&raw)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return p, err
	}
	root, err := m.packageCacheRoot()
	if err == nil {
		err = verifyRuntimePreparedPackage(filepath.Join(root, p.Metadata.ID), p)
	}
	if err == nil {
		_, err = m.s.store.db.Exec(`UPDATE app_runtime_packages SET last_used=? WHERE id=?`, time.Now().UnixNano(), p.Metadata.ID)
	}
	return p, err
}

// Retained deployments (including stopped rollback candidates) pin their
// packages. GC removes only unreferenced verified packages under the cache lock.
func (m *appRuntimeManager) pruneRuntimePackages(root string, incoming, limit int64) error {
	rows, err := m.s.store.db.Query(`SELECT id,bytes,last_used FROM app_runtime_packages ORDER BY last_used,id`)
	if err != nil {
		return err
	}
	type entry struct {
		id          string
		bytes, used int64
	}
	var entries []entry
	var total int64
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.id, &e.bytes, &e.used); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
		total += e.bytes
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].used < entries[j].used })
	for _, e := range entries {
		if total+incoming <= limit {
			break
		}
		var pinned int
		if err = m.s.store.db.QueryRow(`SELECT count(*) FROM app_runtime_deployments WHERE status!='deleted' AND json_extract(record_json,'$.package.metadata.id')=?`, e.id).Scan(&pinned); err != nil {
			return err
		}
		// A concurrent builder may have prepared but not yet attached its result.
		if err = m.s.store.db.QueryRow(`SELECT ?+count(*) FROM app_runtime_deployments WHERE status!='deleted' AND json_extract(record_json,'$.package_preparation.build_key') IN (SELECT build_key FROM app_runtime_package_builds WHERE package_id=?)`, pinned, e.id).Scan(&pinned); err != nil {
			return err
		}
		if pinned != 0 {
			continue
		}
		if !strings.HasPrefix(e.id, "pkg-") || !runtimeDigest(strings.TrimPrefix(e.id, "pkg-")) {
			return fmt.Errorf("invalid package cache identity")
		}
		if err = os.RemoveAll(filepath.Join(root, e.id)); err != nil {
			return err
		}
		if _, err = m.s.store.db.Exec(`DELETE FROM app_runtime_packages WHERE id=?`, e.id); err != nil {
			return err
		}
		_, _ = m.s.store.db.Exec(`DELETE FROM app_runtime_package_deliveries WHERE package_id=?`, e.id)
		_, _ = m.s.store.db.Exec(`DELETE FROM app_runtime_package_builds WHERE package_id=?`, e.id)
		total -= e.bytes
	}
	if total+incoming > limit {
		return fmt.Errorf("package cache capacity exhausted by retained deployments")
	}
	return nil
}
func (m *appRuntimeManager) publishRuntimePackage(root, stage string, p runtimePreparedPackage) error {
	unlock, err := runtimePackageLock(m.ctx, filepath.Join(root, ".cache.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	var existing string
	err = m.s.store.db.QueryRow(`SELECT record_json FROM app_runtime_packages WHERE id=?`, p.Metadata.ID).Scan(&existing)
	if err == nil {
		var prior runtimePreparedPackage
		if json.Unmarshal([]byte(existing), &prior) != nil || runtimeJSON(prior.Metadata) != runtimeJSON(p.Metadata) {
			return fmt.Errorf("package identity collision")
		}
		if err = verifyRuntimePreparedPackage(filepath.Join(root, p.Metadata.ID), prior); err != nil {
			return err
		}
		_, err = m.s.store.db.Exec(`INSERT INTO app_runtime_package_builds(build_key,package_id,record_json) VALUES(?,?,?) ON CONFLICT(build_key) DO NOTHING`, p.BuildKey, p.Metadata.ID, runtimeJSON(p))
		return err
	}
	if err != sql.ErrNoRows {
		return err
	}
	var size int64
	for _, f := range p.Metadata.Files {
		size += f.Size
	}
	if err = m.pruneRuntimePackages(root, size, runtimePackageCacheLimit); err != nil {
		return err
	}
	dest := filepath.Join(root, p.Metadata.ID)
	// A crash after rename but before the receipt write leaves an immutable,
	// inspectable package; adopt matching bytes rather than overwrite them.
	if _, err = os.Stat(dest); err == nil {
		if err = verifyRuntimePreparedPackage(dest, p); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err = os.Rename(stage, dest); err != nil {
		return err
	}
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO app_runtime_packages(id,build_key,record_json,bytes,last_used) VALUES(?,?,?,?,?)`, p.Metadata.ID, p.BuildKey, runtimeJSON(p), size, time.Now().UnixNano()); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO app_runtime_package_builds(build_key,package_id,record_json) VALUES(?,?,?)`, p.BuildKey, p.Metadata.ID, runtimeJSON(p)); err != nil {
		return err
	}
	return tx.Commit()
}

func runtimePackageWorkspace(ctx context.Context, root string) (string, func(), error) {
	// At most two preparation workspaces across processes sharing this cache.
	var slot *os.File
	for slot == nil {
		for i := 0; i < 2; i++ {
			f, err := os.OpenFile(filepath.Join(root, fmt.Sprintf(".builder-%d.lock", i)), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return "", nil, err
			}
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				slot = f
				break
			}
			f.Close()
			if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
				return "", nil, err
			}
		}
		if slot == nil {
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	releaseSlot := func() { _ = syscall.Flock(int(slot.Fd()), syscall.LOCK_UN); slot.Close() }
	unlock, err := runtimePackageLock(ctx, filepath.Join(root, ".workspace.lock"))
	if err != nil {
		releaseSlot()
		return "", nil, err
	}
	defer unlock()
	entries, err := os.ReadDir(root)
	if err != nil {
		releaseSlot()
		return "", nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".build-") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		f, err := os.OpenFile(filepath.Join(dir, ".work.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			releaseSlot()
			return "", nil, err
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			err = os.RemoveAll(dir)
			if err != nil {
				f.Close()
				releaseSlot()
				return "", nil, err
			}
		}
		f.Close()
	}
	work, err := os.MkdirTemp(root, ".build-")
	if err != nil {
		releaseSlot()
		return "", nil, err
	}
	file, err := os.OpenFile(filepath.Join(work, ".work.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err == nil {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	}
	if err != nil {
		if file != nil {
			file.Close()
		}
		os.RemoveAll(work)
		releaseSlot()
		return "", nil, err
	}
	return work, func() { os.RemoveAll(work); file.Close(); releaseSlot() }, nil
}
