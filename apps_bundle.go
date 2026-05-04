package main

// Static-app bundle delivery for the Apteva Apps system.
//
// Counterpart to LocalSupervisor.fetchBinary (which downloads + caches
// a single sidecar binary). This file handles the static-UI-app case:
// fetch a prebuilt asset tarball, verify its sha256, extract it under
// the apps cache so resolveStaticInstallDir can mount it.
//
// Authors ship one tarball per release (built once on CI). The output
// is browser code, so there's no per-platform matrix the way binaries
// have one — every install on every host pulls the same artifact.
//
// Cache layout:
//   <cacheDir>/<name>/<version>/dist/        — extracted tree
//   <cacheDir>/<name>/<version>/.bundle-sha  — marker; sha256 of the
//                                              tarball that produced
//                                              the current dist/
//
// The marker lets us short-circuit re-installs of the same version: if
// the manifest's sha matches the marker, we trust the cache. Versions
// are expected to be immutable (release-asset pattern); to invalidate,
// authors bump the version, not the tarball.

import (
	"archive/tar"
	"compress/gzip"
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

// fetchAndExtractBundle downloads a tarball from url, verifies it
// against sha256hex, and atomically extracts it into destDir. Returns
// destDir on success.
//
// Idempotent: if destDir/.bundle-sha already matches sha256hex, no
// network or filesystem work happens.
//
// On any failure (mismatched hash, malformed tar, partial download)
// the in-progress staging dir is removed and destDir is left
// untouched, so a previous good extraction is never corrupted by a
// later bad one.
func fetchAndExtractBundle(url, sha256hex, destDir string) error {
	if url == "" {
		return fmt.Errorf("bundle url required")
	}
	if len(sha256hex) != 64 {
		return fmt.Errorf("bundle sha256 must be 64 hex chars (got %d)", len(sha256hex))
	}
	sha256hex = strings.ToLower(sha256hex)

	// Cache hit: marker file matches the requested digest.
	markerPath := filepath.Join(filepath.Dir(destDir), ".bundle-sha")
	if existing, err := os.ReadFile(markerPath); err == nil {
		if strings.TrimSpace(string(existing)) == sha256hex {
			if _, err := os.Stat(destDir); err == nil {
				return nil
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}

	body, err := openBundleSource(url)
	if err != nil {
		return err
	}
	defer body.Close()

	// Hash while extracting: stream into a tee so we don't have to
	// land the tarball on disk first. The tar reader pulls from the
	// hashing reader, so by EOF we have both extraction and digest.
	hasher := sha256.New()
	tee := io.TeeReader(body, hasher)

	// Stage to a sibling temp dir, then atomic-rename into place.
	stage, err := os.MkdirTemp(filepath.Dir(destDir), ".bundle-stage-")
	if err != nil {
		return fmt.Errorf("mkdir stage: %w", err)
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			os.RemoveAll(stage)
		}
	}()

	if err := extractTarGz(tee, stage); err != nil {
		return fmt.Errorf("extract bundle: %w", err)
	}
	// Drain any trailing bytes so the digest covers the whole file.
	// extractTarGz stops at the tar end-of-archive marker, but the
	// gzip stream may have padding or the http body may have more.
	io.Copy(io.Discard, tee)

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != sha256hex {
		return fmt.Errorf("bundle sha256 mismatch: manifest=%s actual=%s", sha256hex, got)
	}

	// Replace any previous extraction atomically.
	_ = os.RemoveAll(destDir)
	if err := os.Rename(stage, destDir); err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}
	cleanupStage = false

	if err := os.WriteFile(markerPath, []byte(sha256hex+"\n"), 0644); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	return nil
}

// openBundleSource returns a ReadCloser for url. http(s):// is the
// production path; file:// (or absolute path) lets local-only testing
// skip the release dance — same convention LocalSupervisor.fetchBinary
// uses.
func openBundleSource(url string) (io.ReadCloser, error) {
	if filepath.IsAbs(url) || strings.HasPrefix(url, "file://") {
		path := strings.TrimPrefix(url, "file://")
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open bundle %q: %w", path, err)
		}
		return f, nil
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch bundle %s: %w", url, err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("fetch bundle %s: http %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// extractTarGz reads a gzipped tar from r and writes its regular files
// + directories into destDir. Symlinks and special files are skipped
// (static UI apps don't need them, and refusing them avoids the
// classic tar-symlink-escape vulnerability). Paths that try to escape
// destDir via .. are rejected.
func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		// Reject absolute or escaping paths.
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return fmt.Errorf("refusing tar entry with unsafe path: %q", hdr.Name)
		}
		target := filepath.Join(absDest, clean)
		// Belt and suspenders: ensure target stayed under destDir.
		if !strings.HasPrefix(target, absDest+string(os.PathSeparator)) && target != absDest {
			return fmt.Errorf("refusing tar entry that escapes dest: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			f.Close()
		default:
			// Skip symlinks, hardlinks, devices, etc. Static UI bundles
			// don't legitimately need them.
		}
	}
}
