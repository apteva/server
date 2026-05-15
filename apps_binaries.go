package main

// Native-binary dependencies — fetch + extract + cache the executables
// declared in a manifest's `requires.binaries` block.
//
// Why this exists. Apteva apps that wrap ffmpeg, yt-dlp, qpdf, etc.
// historically needed the operator to install those binaries on the
// host before the app would actually work. That broke the "install an
// app and it works" expectation, especially for first-party apps like
// media that fail every probe without ffprobe on PATH. This module
// closes the loop by declaring the dep in the manifest and fetching
// it during installFromSource, the same way the source-installer
// fetches the app's own Go code.
//
// Cache layout. Per-binary versioned dirs live at
// ~/.apteva/binaries/<name>-<version>-<os>-<arch>/, shared across
// every install of every app that declares the same dep. An empty
// `.ok` marker at the dir root indicates a complete extraction;
// presence of the marker short-circuits future EnsureBinaries calls.
// Manual repair: drop your binaries in, touch .ok, retry the install.
//
// Trust. Each BinarySource carries a mandatory sha256; downloads
// fail hard on mismatch. The manifest lives in the app's git repo,
// which the platform already trusts (it builds the app from there).
//
// Failure modes.
//
//   required=true + fetch fails  → install errors with a clear message.
//   required=true + no source for runtime platform → same.
//   required=false + fetch fails → install proceeds; the missing
//     binary won't be on PATH, app should degrade gracefully.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// binariesRoot returns the per-host cache dir for native-binary deps.
// Honours APTEVA_HOME for parity with the rest of the install paths.
func binariesRoot() string {
	home := os.Getenv("APTEVA_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".apteva")
		} else {
			home = ".apteva"
		}
	}
	return filepath.Join(home, "binaries")
}

func runtimePlatform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

func binaryCacheKey(dep sdk.BinaryDep) string {
	return fmt.Sprintf("%s-%s-%s", dep.Name, dep.Version, runtimePlatform())
}

// EnsureBinaries resolves every binary dep in the manifest. For each
// dep that isn't already cached, it downloads, verifies checksum,
// extracts the listed executables, and writes a `.ok` marker. Returns
// the colon-separated list of dir paths the caller should prepend to
// the sidecar's PATH.
//
// progress is invoked with a human-readable status string for each
// fetch so the install card can surface "Downloading ffmpeg…" instead
// of staring at "Building…" for 30s. Passing nil is fine.
func EnsureBinaries(m *sdk.Manifest, progress func(string)) (pathPrefix string, err error) {
	if m == nil || len(m.Requires.Binaries) == 0 {
		return "", nil
	}
	if progress == nil {
		progress = func(string) {}
	}
	root := binariesRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create binaries root: %w", err)
	}
	platform := runtimePlatform()
	var dirs []string
	for _, dep := range m.Requires.Binaries {
		dir, err := ensureBinary(dep, platform, root, progress)
		if err != nil {
			if dep.Required {
				return "", fmt.Errorf("binary dep %s: %w", dep.Name, err)
			}
			log.Printf("[APPS-BIN] optional dep %s failed (%v); skipping", dep.Name, err)
			continue
		}
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator)), nil
}

func ensureBinary(dep sdk.BinaryDep, platform, root string, progress func(string)) (string, error) {
	key := binaryCacheKey(dep)
	dir := filepath.Join(root, key)
	okPath := filepath.Join(dir, ".ok")
	if _, err := os.Stat(okPath); err == nil {
		return dir, nil
	}
	// PATH fallback. If every declared executable already resolves on
	// the operator's PATH (homebrew, apt, etc.), don't bother fetching
	// a vendored copy — let the sidecar inherit the existing PATH.
	// Returns "" so EnsureBinaries doesn't prepend an empty cache dir.
	// Especially useful on darwin-arm64 where we ship no source URLs.
	if len(dep.Executables) > 0 {
		allInPath := true
		for _, exe := range dep.Executables {
			if _, err := exec.LookPath(exe); err != nil {
				allInPath = false
				break
			}
		}
		if allInPath {
			log.Printf("[APPS-BIN] %s resolved on PATH (executables=%v); skipping fetch", dep.Name, dep.Executables)
			return "", nil
		}
	}
	src, ok := dep.Sources[platform]
	if !ok {
		return "", fmt.Errorf("no source for %s", platform)
	}
	if strings.TrimSpace(src.URL) == "" {
		return "", fmt.Errorf("source for %s has no url", platform)
	}
	if strings.TrimSpace(src.SHA256) == "" {
		return "", fmt.Errorf("source for %s has no sha256 — refusing to fetch unverified binary", platform)
	}
	progress(fmt.Sprintf("Downloading %s %s…", dep.Name, dep.Version))
	if err := fetchAndExtractBinary(dep, src, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(okPath, []byte("ok"), 0o644); err != nil {
		return "", fmt.Errorf("write ok marker: %w", err)
	}
	log.Printf("[APPS-BIN] cached %s at %s", key, dir)
	return dir, nil
}

func fetchAndExtractBinary(dep sdk.BinaryDep, src sdk.BinarySource, dir string) error {
	// Reset the target dir so a half-extracted previous attempt
	// doesn't leak in (callers retry on Required:true failures).
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	stageDir, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".stage-")
	if err != nil {
		return fmt.Errorf("stage dir: %w", err)
	}
	defer os.RemoveAll(stageDir)

	archivePath := filepath.Join(stageDir, "archive.bin")
	gotSum, err := downloadWithSum(src.URL, archivePath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(gotSum, src.SHA256) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", gotSum, src.SHA256)
	}

	extractRoot := filepath.Join(stageDir, "extract")
	if err := os.MkdirAll(extractRoot, 0o755); err != nil {
		return err
	}
	archiveKind := strings.ToLower(strings.TrimSpace(src.Archive))
	switch archiveKind {
	case "", "raw":
		// No archive — the body IS the binary. There must be exactly
		// one executable listed, and we drop the downloaded file in
		// as that executable.
		if len(dep.Executables) != 1 {
			return fmt.Errorf("archive=raw requires exactly one executables entry, got %d", len(dep.Executables))
		}
		dst := filepath.Join(extractRoot, dep.Executables[0])
		if err := os.Rename(archivePath, dst); err != nil {
			return fmt.Errorf("rename raw: %w", err)
		}
	case "tar.xz", "tar.gz", "tar":
		if err := extractTar(archivePath, extractRoot, archiveKind); err != nil {
			return fmt.Errorf("extract %s: %w", archiveKind, err)
		}
	case "zip":
		if err := extractZip(archivePath, extractRoot); err != nil {
			return fmt.Errorf("extract zip: %w", err)
		}
	default:
		return fmt.Errorf("unsupported archive format %q", src.Archive)
	}

	// Locate listed executables inside extractRoot honouring strip_root.
	for _, exe := range dep.Executables {
		srcPath, err := findExecutable(extractRoot, exe, src.StripRoot)
		if err != nil {
			return fmt.Errorf("locate %s: %w", exe, err)
		}
		dstPath := filepath.Join(dir, exe)
		if err := copyFileForBinary(srcPath, dstPath); err != nil {
			return fmt.Errorf("copy %s: %w", exe, err)
		}
		if err := os.Chmod(dstPath, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", exe, err)
		}
	}
	return nil
}

// downloadWithSum streams the URL to disk while computing sha256.
// One pass over the bytes, no buffering of the full archive in
// memory — ffmpeg static tarballs run to 80MB and could be much
// larger for other deps.
func downloadWithSum(url, dst string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "apteva-server/binaries")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get: http %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return "", fmt.Errorf("copy: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTar handles tar, tar.gz, tar.xz. tar.xz shells out to `xz`
// because Go's stdlib doesn't ship an xz decoder and pulling in a
// pure-Go xz module just for this is over-investment — every host
// we run on has tar + xz available (ubuntu, debian, alpine, macOS).
func extractTar(archivePath, destDir, kind string) error {
	switch kind {
	case "tar":
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer f.Close()
		return extractTarStream(f, destDir)
	case "tar.gz":
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		return extractTarStream(gz, destDir)
	case "tar.xz":
		// `tar -xJf <archive> -C <destDir>` does the whole thing in
		// one syscall and handles xz natively. Avoids adding
		// github.com/ulikunitz/xz as a Go dep for one feature.
		cmd := exec.Command("tar", "-xJf", archivePath, "-C", destDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tar -xJf: %w (output: %s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("unsupported tar variant %s", kind)
	}
}

func extractTarStream(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// Sanitize path — refuse anything that climbs out of destDir.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("tar contains unsafe path %q", hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			// Skip symlinks — we only care about the executable files
			// listed in dep.Executables. A symlink inside the archive
			// could point outside destDir; refuse to follow it.
			continue
		default:
			// Skip device files, FIFOs, etc.
			continue
		}
	}
}

func extractZip(archivePath, destDir string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		clean := filepath.Clean(f.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("zip contains unsafe path %q", f.Name)
		}
		target := filepath.Join(destDir, clean)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		out.Close()
	}
	return nil
}

// findExecutable looks up `name` inside extractRoot. If stripRoot>0,
// we descend through stripRoot single-child directory layers first
// (mirrors tar --strip-components: johnvansickle's tarballs expand to
// ffmpeg-7.0.2-amd64-static/{ffmpeg,ffprobe}, strip_root=1 means look
// inside that one wrapper dir).
//
// If the file isn't where strip_root says it should be, fall back to
// a recursive walk so manifest authors don't have to know the exact
// archive layout.
func findExecutable(extractRoot, name string, stripRoot int) (string, error) {
	base := extractRoot
	for i := 0; i < stripRoot; i++ {
		ents, err := os.ReadDir(base)
		if err != nil {
			return "", err
		}
		// Pick the single directory entry (or the first if multiple —
		// rare). Manifest authors set strip_root=1 because the archive
		// has one wrapper dir; if there are multiple, we walk anyway.
		var nextDir string
		for _, e := range ents {
			if e.IsDir() {
				nextDir = filepath.Join(base, e.Name())
				break
			}
		}
		if nextDir == "" {
			break
		}
		base = nextDir
	}
	direct := filepath.Join(base, name)
	if st, err := os.Stat(direct); err == nil && !st.IsDir() {
		return direct, nil
	}
	// Fallback recursive walk.
	var found string
	werr := filepath.WalkDir(extractRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if werr != nil {
		return "", werr
	}
	if found == "" {
		return "", fmt.Errorf("executable %q not found in archive", name)
	}
	return found, nil
}

func copyFileForBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
