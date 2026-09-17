package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

const maxAppArtifactBytes = 256 << 20
const maxAppArtifactExpandedBytes = 1 << 30

type appArtifactReceipt struct {
	SHA256 string            `json:"sha256"`
	Files  map[string]string `json:"files"`
}

func validArtifactDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}
func artifactAppRoot(m *sdk.Manifest, bin string) string {
	root := filepath.Join(filepath.Dir(bin), "src")
	if m.Runtime.Source != nil && m.Runtime.Source.Entry != "" {
		root = filepath.Join(root, filepath.FromSlash(m.Runtime.Source.Entry))
	}
	return root
}
func applyArtifactResourceEnv(m *sdk.Manifest, bin string, env map[string]string) {
	if filepath.Base(filepath.Dir(filepath.Dir(bin))) != "artifacts" {
		return
	}
	root := artifactAppRoot(m, bin)
	env["APTEVA_UI_DIR"] = filepath.Join(root, "ui")
	if m.DB != nil && m.DB.Migrations != "" {
		env["APTEVA_MIGRATIONS_DIR"] = filepath.Join(root, m.DB.Migrations)
	}
}
func safeArtifactRelative(name string) bool {
	return name != "" && !filepath.IsAbs(name) && !strings.Contains(name, "\\") && name != ".." && !strings.HasPrefix(filepath.Clean(name), ".."+string(filepath.Separator))
}
func artifactFileHash(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("artifact file is not regular: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func verifyAppArtifact(root, digest string, m *sdk.Manifest) error {
	raw, err := os.ReadFile(filepath.Join(root, ".artifact-receipt.json"))
	if err != nil {
		return err
	}
	var receipt appArtifactReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.SHA256 != digest || len(receipt.Files) == 0 {
		return errors.New("invalid artifact cache receipt")
	}
	for rel, want := range receipt.Files {
		if !safeArtifactRelative(rel) {
			return errors.New("invalid artifact cache path")
		}
		got, err := artifactFileHash(filepath.Join(root, rel))
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("artifact cache checksum mismatch: %s", rel)
		}
	}
	if err := filepath.WalkDir(filepath.Join(root, "src"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in artifact cache")
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if receipt.Files[rel] == "" {
				return fmt.Errorf("unverified artifact resource: %s", rel)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if receipt.Files["bin"] == "" {
		return errors.New("artifact cache has no executable")
	}
	return validateAppArtifact(root, m)
}
func validateAppArtifact(root string, m *sdk.Manifest) error {
	entry := ""
	if m.Runtime.Source != nil {
		entry = m.Runtime.Source.Entry
	}
	if entry != "" && !safeArtifactRelative(entry) {
		return errors.New("invalid artifact source entry")
	}
	appRoot := artifactAppRoot(m, filepath.Join(root, "bin"))
	raw, err := os.ReadFile(filepath.Join(appRoot, "apteva.yaml"))
	if err != nil {
		return fmt.Errorf("artifact manifest: %w", err)
	}
	packaged, err := sdk.ParseManifest(raw)
	if err != nil {
		return err
	}
	want := *m
	want.Runtime.Artifacts = nil
	packaged.Runtime.Artifacts = nil
	if !reflect.DeepEqual(want, *packaged) {
		return errors.New("artifact manifest does not match requested app version and capabilities")
	}
	requireFile := func(rel string) error {
		if !safeArtifactRelative(rel) {
			return fmt.Errorf("invalid artifact resource path %q", rel)
		}
		_, err := artifactFileHash(filepath.Join(appRoot, rel))
		return err
	}
	if _, err := artifactFileHash(filepath.Join(root, "bin")); err != nil {
		return fmt.Errorf("artifact executable: %w", err)
	}
	if m.DB != nil && m.DB.Migrations != "" {
		if !safeArtifactRelative(m.DB.Migrations) {
			return errors.New("artifact migrations must be relative")
		}
		info, err := os.Stat(filepath.Join(appRoot, m.DB.Migrations))
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("artifact migrations directory missing")
		}
	}
	for _, s := range m.Provides.Skills {
		if s.BodyFile != "" {
			if err := requireFile(s.BodyFile); err != nil {
				return fmt.Errorf("artifact skill %s: %w", s.Name, err)
			}
		}
	}
	entries := []string{}
	for _, p := range m.Provides.UIPanels {
		entries = append(entries, p.Entry)
	}
	for _, c := range m.Provides.UIComponents {
		entries = append(entries, c.Entry)
	}
	for _, surface := range m.Provides.UISurfaces {
		entries = append(entries, surface.Entry)
	}
	entries = append(entries, m.Icon)
	for _, entry := range entries {
		if strings.HasPrefix(entry, "/ui/") {
			if err := requireFile(strings.TrimPrefix(entry, "/")); err != nil {
				return fmt.Errorf("artifact UI: %w", err)
			}
		}
	}
	return nil
}

// Complete packages are immutable and isolated from source builds and live data:
// <cache>/<app>/<version>/artifacts/<sha>/bin + src/<source.entry>/...
// Only absence of a platform artifact permits source fallback. Any selected
// artifact error propagates, leaving previous runtimes and caches intact.
func (sup *LocalSupervisor) fetchAppArtifact(m *sdk.Manifest, progress func(string)) (string, error) {
	a, ok := m.Runtime.Artifacts[localPlatform()]
	if !ok {
		return "", errors.New("no artifact for this platform")
	}
	if progress == nil {
		progress = func(string) {}
	}
	digest := strings.ToLower(a.SHA256)
	if !validArtifactDigest(digest) || a.URL == "" {
		return "", errors.New("artifact requires URL and SHA-256")
	}
	for _, part := range []string{m.Name, m.Version} {
		if part == "" || part == "." || part == ".." || filepath.Base(part) != part {
			return "", errors.New("invalid artifact app/version")
		}
	}
	parent := filepath.Join(sup.cacheDir, m.Name, m.Version, "artifacts")
	dest := filepath.Join(parent, digest)
	bin := filepath.Join(dest, "bin")
	if _, err := os.Stat(dest); err == nil {
		progress("Verifying cached app…")
		if err := verifyAppArtifact(dest, digest, m); err != nil {
			return "", err
		}
		return bin, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(parent, 0755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	progress("Downloading prebuilt app…")
	body, err := openBundleSource(a.URL)
	if err != nil {
		return "", err
	}
	defer body.Close()
	archive, err := os.CreateTemp(parent, ".download-")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, h), io.LimitReader(body, maxAppArtifactBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxAppArtifactBytes {
		return "", errors.New("artifact exceeds download size limit")
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return "", errors.New("artifact SHA-256 mismatch")
	}
	progress("Verifying and extracting app…")
	if _, err := archive.Seek(0, 0); err != nil {
		return "", err
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	receipt := appArtifactReceipt{SHA256: digest, Files: map[string]string{}}
	var total int64
	count := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		count++
		if count > 20000 {
			return "", errors.New("too many artifact entries")
		}
		rel := filepath.Clean(filepath.FromSlash(header.Name))
		if !safeArtifactRelative(rel) {
			return "", fmt.Errorf("unsafe artifact path %q", header.Name)
		}
		if rel != "." && rel != "bin" && rel != "src" && !strings.HasPrefix(rel, "src"+string(filepath.Separator)) {
			return "", fmt.Errorf("unsupported artifact path %q", rel)
		}
		path := filepath.Join(stage, rel)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0755); err != nil {
				return "", err
			}
		case tar.TypeReg, tar.TypeRegA:
			total += header.Size
			if header.Size < 0 || total > maxAppArtifactExpandedBytes {
				return "", errors.New("artifact exceeds expanded size limit")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return "", err
			}
			mode := os.FileMode(0644)
			if rel == "bin" || header.Mode&0111 != 0 {
				mode = 0755
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return "", err
			}
			hash := sha256.New()
			_, copyErr := io.Copy(io.MultiWriter(f, hash), tr)
			closeErr := f.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			receipt.Files[rel] = hex.EncodeToString(hash.Sum(nil))
		default:
			return "", fmt.Errorf("unsupported artifact entry type for %s", rel)
		}
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return "", err
	}
	if err := validateAppArtifact(stage, m); err != nil {
		return "", err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(stage, ".artifact-receipt.json"), raw, 0644); err != nil {
		return "", err
	}
	if err := os.Rename(stage, dest); err != nil {
		// Another install may have finished the same immutable package first.
		if verifyErr := verifyAppArtifact(dest, digest, m); verifyErr != nil {
			return "", err
		}
	}
	return bin, nil
}
