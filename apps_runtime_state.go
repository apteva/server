package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

func (m *appRuntimeManager) transferState(old, d *runtimeDeployment, op *runtimeOperation) error {
	if old.Status != "stopped" || !old.Execution.StoppedConfirmed {
		return fmt.Errorf("state transfer requires confirmed source shutdown")
	}
	if err := m.pinStatePolicy(*old, *d, op); err != nil {
		return err
	}
	if op.SnapshotPath == "" {
		if err := m.captureStateSnapshot(*old, op); err != nil {
			return err
		}
	}
	return m.restoreSnapshot(d, *op)
}
func (m *appRuntimeManager) captureStateSnapshot(source runtimeDeployment, op *runtimeOperation) error {
	path, sha, err := m.snapshot(source, *op)
	if err != nil {
		return err
	}
	previous := *op
	op.SnapshotPath, op.SnapshotSHA256, op.SnapshotSourceID = path, sha, source.ID
	if err = m.saveOperation(*op); err != nil {
		*op = previous
		os.Remove(path)
		return err
	}
	// The previous archive is retained until the new digest and provenance are durable.
	if previous.SnapshotPath != "" && previous.SnapshotPath != path {
		_ = os.Remove(previous.SnapshotPath)
	}
	return nil
}
func (m *appRuntimeManager) snapshot(d runtimeDeployment, op runtimeOperation) (string, string, error) {
	if d.Status != "stopped" || !d.Execution.StoppedConfirmed {
		return "", "", fmt.Errorf("snapshot requires confirmed source shutdown")
	}
	policy, err := operationRuntimeStatePolicy(op)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := m.stateTransferContext(op, policy)
	defer cancel()
	if err = m.checkOwner(op); err != nil {
		return "", "", err
	}
	if m.s.localApps == nil {
		return "", "", fmt.Errorf("Server snapshot cache is unavailable")
	}
	root := filepath.Join(m.s.localApps.cacheDir, ".runtime-snapshots")
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", "", err
	}
	if err = m.checkSnapshotSource(ctx, d, op, policy); err != nil {
		return "", "", err
	}
	f, err := os.CreateTemp(root, op.ID+"-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	path := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	var expectedSHA string
	bounded := &runtimeBoundedWriter{ctx: ctx, out: f, remaining: policy.MaxArchiveBytes}
	if d.Placement.local() {
		if len(d.Manifest.Runtime.Storage) > 0 {
			return "", "", fmt.Errorf("native local storage mounts are not mapped; migration needs an explicit storage backend")
		}
		if err = writeRuntimeSnapshotContext(ctx, m.localStateDir(d), bounded, policy); err != nil {
			return "", "", err
		}
	} else {
		b, e := m.backend(d)
		if e != nil {
			return "", "", e
		}
		remote := b.(*remoteRuntimeBackend)
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, runtimeEndpoint(remote.base, remote.registration.Endpoint)+"/snapshot?deployment_id="+d.ID, nil)
		if e != nil {
			return "", "", e
		}
		req.Header.Set("Authorization", "Bearer "+remote.token)
		req.Header.Set("Idempotency-Key", op.ID+":"+d.ID+":snapshot")
		req.Header.Set("X-Apteva-State-Transfer-Policy", runtimeJSON(policy))
		resp, e := m.streamHTTP().Do(req)
		if e != nil {
			return "", "", fmt.Errorf("snapshot download failed: %w", e)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", "", fmt.Errorf("snapshot download rejected (HTTP %d)", resp.StatusCode)
		}
		if resp.ContentLength > policy.MaxArchiveBytes {
			return "", "", fmt.Errorf("snapshot exceeds archive transfer limit")
		}
		expectedSHA = resp.Header.Get("X-Apteva-Snapshot-SHA256")
		if _, e = io.Copy(bounded, io.LimitReader(resp.Body, policy.MaxArchiveBytes+1)); e != nil {
			return "", "", e
		}
	}
	if err = f.Sync(); err != nil {
		return "", "", err
	}
	if err = f.Close(); err != nil {
		return "", "", err
	}
	// Validate expansion, paths and gzip integrity even for a remote download.
	if _, err = inspectRuntimeSnapshot(ctx, path, policy); err != nil {
		return "", "", err
	}
	sha, err := runtimeSnapshotHash(ctx, path)
	if err != nil {
		return "", "", err
	}
	if expectedSHA != "" && expectedSHA != sha {
		return "", "", fmt.Errorf("snapshot download digest does not match backend checksum")
	}
	ok = true
	return path, sha, nil
}
func (m *appRuntimeManager) streamHTTP() *http.Client {
	// Each streaming request owns its negotiated deadline.
	return &http.Client{Transport: m.http.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (m *appRuntimeManager) restoreSnapshot(d *runtimeDeployment, op runtimeOperation) error {
	policy, err := operationRuntimeStatePolicy(op)
	if err != nil {
		return err
	}
	ctx, cancel := m.stateTransferContext(op, policy)
	defer cancel()
	sha, err := runtimeSnapshotHash(ctx, op.SnapshotPath)
	if err != nil || sha != op.SnapshotSHA256 {
		return fmt.Errorf("persisted migration snapshot is missing or does not match its digest")
	}
	// A recorded verified restore is safe to reuse, including after a lost start
	// response. Never rewrite that state underneath an execution.
	if d.Execution.SnapshotSHA256 == sha {
		return nil
	}
	if err = m.confirmRestoreTarget(ctx, *d, op, policy); err != nil {
		return err
	}
	stats, err := inspectRuntimeSnapshot(ctx, op.SnapshotPath, policy)
	if err != nil {
		return err
	}
	archive, expanded := runtimeStateDiskBudget(stats)
	if d.Placement.local() {
		free, e := runtimeStateDiskFree(m.localStateDir(*d))
		if e != nil {
			return e
		}
		if err = runtimeRequireDisk(&free, expanded, "Restore destination"); err != nil {
			return err
		}
		if err = m.checkOwner(op); err != nil {
			return err
		}
		if err = readRuntimeSnapshotContext(ctx, op.SnapshotPath, m.localStateDir(*d), policy); err != nil {
			return err
		}
		d.Execution.SnapshotSHA256 = sha
		return m.saveDeployment(*d)
	}
	b, err := m.backend(*d)
	if err != nil {
		return err
	}
	remote := b.(*remoteRuntimeBackend)
	// Refresh free space immediately before uploading, not just during preflight.
	reqInfo, err := m.request(*d, op, "inspect")
	if err != nil {
		return err
	}
	observed, err := remote.inspectContext(ctx, reqInfo)
	if err != nil {
		return err
	}
	if _, err = runtimeMergeExecution(d.Execution, observed); err != nil {
		return err
	}
	if observed.State != "prepared" && !(observed.State == "stopped" && observed.StoppedConfirmed) {
		return fmt.Errorf("restore target is not confirmed quiescent")
	}
	if err = runtimeRequireDisk(observed.StateAvailableBytes, expanded+archive, "Restore destination"); err != nil {
		return err
	}
	f, err := os.Open(op.SnapshotPath)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, runtimeEndpoint(remote.base, remote.registration.Endpoint)+"/restore?deployment_id="+d.ID, f)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Authorization", "Bearer "+remote.token)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Apteva-Snapshot-SHA256", sha)
	req.Header.Set("X-Apteva-State-Transfer-Policy", runtimeJSON(policy))
	req.Header.Set("Idempotency-Key", op.ID+":"+d.ID+":"+sha+":restore")
	if err = m.checkOwner(op); err != nil {
		return err
	}
	resp, err := m.streamHTTP().Do(req)
	if err != nil {
		return fmt.Errorf("snapshot restore unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Apteva-Snapshot-SHA256") != sha {
		return fmt.Errorf("backend did not verify restored state digest")
	}
	d.Execution.SnapshotSHA256 = sha
	return m.saveDeployment(*d)
}
func writeRuntimeSnapshot(root string, out io.Writer) error {
	return writeRuntimeSnapshotContext(context.Background(), root, out, legacyRuntimeStatePolicy())
}
func writeRuntimeSnapshotContext(ctx context.Context, root string, out io.Writer, policy runtimeStateTransferPolicy) error {
	if err := validateRuntimeStatePolicy(policy); err != nil {
		return err
	}
	bounded := &runtimeBoundedWriter{ctx: ctx, out: out, remaining: policy.MaxArchiveBytes}
	gz, _ := gzip.NewWriterLevel(bounded, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	_, err := walkRuntimeStateContext(ctx, root, policy, func(path, rel string, info os.FileInfo) error {
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err = tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.CopyN(tw, &runtimeContextReader{ctx, f}, info.Size())
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		tw.Close()
		gz.Close()
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
func readRuntimeSnapshot(path, dest string) error {
	return readRuntimeSnapshotContext(context.Background(), path, dest, legacyRuntimeStatePolicy())
}
func readRuntimeSnapshotContext(ctx context.Context, path, dest string, policy runtimeStateTransferPolicy) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("snapshot destination must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".runtime-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if _, err = consumeRuntimeSnapshot(ctx, path, stage, policy); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	backup := dest + ".runtime-previous"
	if _, err = os.Lstat(backup); err == nil {
		return fmt.Errorf("previous restoration needs cleanup before another restore")
	}
	if _, err = os.Lstat(dest); err == nil {
		if err = os.Rename(dest, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.Rename(stage, dest); err != nil {
		_ = os.Rename(backup, dest)
		return err
	}
	return os.RemoveAll(backup)
}
func runtimeHash(raw []byte) string { b := sha256.Sum256(raw); return hex.EncodeToString(b[:]) }

// One set of limits/path rules for live metadata checks and stopped snapshots.
type runtimeStateSize struct {
	Bytes   int64
	Entries int
}

// Presence detection does not impose transfer limits on ordinary same-host
// updates or stop/delete. Full validation is reserved for a state transfer.
func runtimeStatePresent(root string) (bool, error) {
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("persistent state could not be inspected")
	}
	if !info.IsDir() {
		return true, nil
	}
	f, err := os.Open(root)
	if err != nil {
		return false, fmt.Errorf("persistent state could not be inspected")
	}
	defer f.Close()
	entries, err := f.Readdirnames(1)
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("persistent state could not be inspected")
	}
	return len(entries) > 0, nil
}

func inspectRuntimeState(root string) (runtimeStateSize, error) { return walkRuntimeState(root, nil) }
func inspectRuntimeStateContext(ctx context.Context, root string, policy runtimeStateTransferPolicy) (runtimeStateSize, error) {
	return walkRuntimeStateContext(ctx, root, policy, nil)
}
func walkRuntimeState(root string, visit func(string, string, os.FileInfo) error) (runtimeStateSize, error) {
	return walkRuntimeStateContext(context.Background(), root, legacyRuntimeStatePolicy(), visit)
}
func walkRuntimeStateContext(ctx context.Context, root string, policy runtimeStateTransferPolicy, visit func(string, string, os.FileInfo) error) (runtimeStateSize, error) {
	var stats runtimeStateSize
	if err := validateRuntimeStatePolicy(policy); err != nil {
		return stats, err
	}
	if !filepath.IsAbs(root) {
		return stats, fmt.Errorf("persistent state path must be absolute")
	}
	if info, err := os.Lstat(root); os.IsNotExist(err) {
		return stats, nil
	} else if err != nil || !info.IsDir() {
		return stats, fmt.Errorf("persistent state root is unavailable or not a directory")
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if err != nil {
			return fmt.Errorf("persistent state could not be inspected")
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("state transfer excludes links and special files")
		}
		stats.Entries++
		if info.Mode().IsRegular() {
			if info.Size() < 0 || info.Size() > policy.MaxExpandedBytes-stats.Bytes {
				return fmt.Errorf("persistent state exceeds snapshot limit")
			}
			stats.Bytes += info.Size()
		}
		if int64(stats.Entries) > policy.MaxEntries {
			return fmt.Errorf("persistent state exceeds snapshot entry limit")
		}
		if visit != nil {
			return visit(path, rel, info)
		}
		return nil
	})
	return stats, err
}
