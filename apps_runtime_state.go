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
	"strings"
	"time"
)

const runtimeSnapshotLimit int64 = 512 << 20

func (m *appRuntimeManager) transferState(old, d *runtimeDeployment, op *runtimeOperation) error {
	if op.SnapshotPath == "" {
		path, sha, err := m.snapshot(*old, *op)
		if err != nil {
			return err
		}
		op.SnapshotPath, op.SnapshotSHA256 = path, sha
		if err = m.saveOperation(*op); err != nil {
			return err
		}
	}
	if d.Execution.SnapshotSHA256 == op.SnapshotSHA256 {
		return nil
	}
	if d.Status == "starting" || d.Status == "ready" || d.Status == "active" {
		return fmt.Errorf("cannot replace state beneath a started execution without verified restore outcome")
	}
	return m.restoreSnapshot(d, *op)
}
func (m *appRuntimeManager) snapshot(d runtimeDeployment, op runtimeOperation) (string, string, error) {
	root := filepath.Join(m.s.localApps.cacheDir, ".runtime-snapshots")
	if err := os.MkdirAll(root, 0700); err != nil {
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
	if d.Placement.local() {
		if len(d.Manifest.Runtime.Storage) > 0 {
			return "", "", fmt.Errorf("native local storage mounts are not mapped; migration needs an explicit storage backend")
		}
		if err = writeRuntimeSnapshot(m.localStateDir(d), f); err != nil {
			return "", "", err
		}
	} else {
		b, e := m.backend(d)
		if e != nil {
			return "", "", e
		}
		remote := b.(*remoteRuntimeBackend)
		ctx, cancel := context.WithTimeout(m.ctx, 20*time.Minute)
		defer cancel()
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, runtimeEndpoint(remote.base, remote.registration.Endpoint)+"/snapshot?deployment_id="+d.ID, nil)
		if e != nil {
			return "", "", e
		}
		req.Header.Set("Authorization", "Bearer "+remote.token)
		req.Header.Set("Idempotency-Key", op.ID+":snapshot")
		resp, e := m.streamHTTP().Do(req)
		if e != nil {
			return "", "", fmt.Errorf("snapshot download failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", "", fmt.Errorf("snapshot download rejected")
		}
		n, e := io.Copy(f, io.LimitReader(resp.Body, runtimeSnapshotLimit+1))
		if e != nil || n > runtimeSnapshotLimit {
			return "", "", fmt.Errorf("snapshot exceeds transfer bound")
		}
	}
	if err = f.Sync(); err != nil {
		return "", "", err
	}
	if err = f.Close(); err != nil {
		return "", "", err
	}
	sha, err := artifactFileHash(path)
	if err != nil {
		return "", "", err
	}
	ok = true
	return path, sha, nil
}
func (m *appRuntimeManager) streamHTTP() *http.Client {
	return &http.Client{Transport: m.http.Transport, Timeout: 20 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (m *appRuntimeManager) restoreSnapshot(d *runtimeDeployment, op runtimeOperation) error {
	sha, err := artifactFileHash(op.SnapshotPath)
	if err != nil || sha != op.SnapshotSHA256 {
		return fmt.Errorf("persisted migration snapshot is missing or does not match its digest")
	}
	if d.Placement.local() {
		if err = readRuntimeSnapshot(op.SnapshotPath, m.localStateDir(*d)); err != nil {
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
	f, err := os.Open(op.SnapshotPath)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, runtimeEndpoint(remote.base, remote.registration.Endpoint)+"/restore?deployment_id="+d.ID, f)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+remote.token)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Apteva-Snapshot-SHA256", sha)
	req.Header.Set("Idempotency-Key", op.ID+":"+d.ID+":restore")
	resp, err := m.streamHTTP().Do(req)
	if err != nil {
		return fmt.Errorf("snapshot restore unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Apteva-Snapshot-SHA256") != sha {
		return fmt.Errorf("backend did not verify restored state digest")
	}
	d.Execution.SnapshotSHA256 = sha
	return m.saveDeployment(*d)
}
func writeRuntimeSnapshot(root string, out io.Writer) error {
	gz, _ := gzip.NewWriterLevel(out, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot excludes links and special files")
		}
		total += info.Size()
		if total > runtimeSnapshotLimit {
			return fmt.Errorf("persistent state exceeds snapshot limit")
		}
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
		defer f.Close()
		_, err = io.CopyN(tw, f, info.Size())
		return err
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
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	count := 0
	for {
		hdr, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe snapshot entry")
		}
		total += hdr.Size
		count++
		if hdr.Size < 0 || total > runtimeSnapshotLimit || count > 100000 {
			return fmt.Errorf("snapshot expansion exceeds limits")
		}
		target := filepath.Join(stage, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			_, e = io.CopyN(out, tr, hdr.Size)
			out.Close()
			if e != nil {
				return e
			}
		default:
			return fmt.Errorf("snapshot links and special files are forbidden")
		}
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
