package main

// Source apps are intentionally buildable on an npx-only installation. Fetch
// Go only when a source app needs it, into the same user-owned Apteva data
// tree as the downloaded CLI binaries. Never modify the system toolchain.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	goReleaseURL        = "https://go.dev"
	maxGoArchiveBytes   = 512 << 20
	maxGoExtractedBytes = 1 << 30
)

var managedGoMu sync.Mutex
var goVersionPattern = regexp.MustCompile(`\bgo(\d+\.\d+(?:\.\d+)?)\b`)

type goToolVersion struct{ major, minor, patch int }

func parseGoToolVersion(value string) (goToolVersion, bool) {
	value = strings.TrimPrefix(value, "go")
	parts := strings.Split(value, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return goToolVersion{}, false
	}
	var numbers [3]int
	for i, part := range parts {
		if part == "" {
			return goToolVersion{}, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return goToolVersion{}, false
		}
		numbers[i] = n
	}
	return goToolVersion{numbers[0], numbers[1], numbers[2]}, true
}

func (v goToolVersion) compare(other goToolVersion) int {
	for i, n := range []int{v.major, v.minor, v.patch} {
		want := []int{other.major, other.minor, other.patch}[i]
		if n < want {
			return -1
		}
		if n > want {
			return 1
		}
	}
	return 0
}

func moduleRequiredGoVersion(dir string) (goToolVersion, error) {
	minimum := goToolVersion{1, 22, 0}
	content, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if errors.Is(err, os.ErrNotExist) {
		return minimum, nil // direct resolver callers may not have a module
	}
	if err != nil {
		return minimum, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(strings.SplitN(line, "//", 2)[0])
		if len(fields) != 2 || (fields[0] != "go" && fields[0] != "toolchain") {
			continue
		}
		version, ok := parseGoToolVersion(fields[1])
		if !ok {
			return minimum, fmt.Errorf("unsupported %s version %q in go.mod", fields[0], fields[1])
		}
		if version.compare(minimum) > 0 {
			minimum = version
		}
	}
	return minimum, nil
}

func goBinaryVersion(binary string) (goToolVersion, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "version")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := cmd.Output()
	if err != nil {
		return goToolVersion{}, false
	}
	match := goVersionPattern.FindStringSubmatch(string(out))
	if len(match) != 2 {
		return goToolVersion{}, false
	}
	return parseGoToolVersion(match[1])
}

func managedGoHome() (string, error) {
	root := os.Getenv("APTEVA_HOME")
	if root == "" {
		root = os.Getenv("DATA_DIR")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", errors.New("set APTEVA_HOME or DATA_DIR to install the Go toolchain")
		}
		root = filepath.Join(home, ".apteva")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, "toolchains"), nil
}

func managedGoBinary(root string) string {
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(root, "go", "bin", name)
}

func cachedManagedGo(root string, required goToolVersion) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	best := goToolVersion{}
	result := ""
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		version, ok := parseGoToolVersion(entry.Name())
		if !ok || version.compare(required) < 0 || (result != "" && version.compare(best) <= 0) {
			continue
		}
		binary := managedGoBinary(filepath.Join(root, entry.Name()))
		actual, valid := goBinaryVersion(binary)
		if !valid || actual.compare(required) < 0 {
			continue
		}
		best, result = version, binary
	}
	return result
}

func ensureManagedGo(required goToolVersion, progress func(string)) (string, error) {
	root, err := managedGoHome()
	if err != nil {
		return "", err
	}
	managedGoMu.Lock()
	defer managedGoMu.Unlock()
	if binary := cachedManagedGo(root, required); binary != "" {
		return binary, nil
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	return installManagedGo(context.Background(), client, goReleaseURL, root, required, progress)
}

type goRelease struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
	Files   []struct {
		Filename string `json:"filename"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		SHA256   string `json:"sha256"`
	} `json:"files"`
}

func selectGoRelease(releases []goRelease, required goToolVersion) (goRelease, string, error) {
	extension := ".tar.gz"
	if runtime.GOOS == "windows" {
		extension = ".zip"
	}
	var chosen goRelease
	filename := ""
	best := goToolVersion{}
	for _, release := range releases {
		version, ok := parseGoToolVersion(release.Version)
		if !release.Stable || !ok || version.compare(required) < 0 || (filename != "" && version.compare(best) <= 0) {
			continue
		}
		for _, file := range release.Files {
			want := release.Version + "." + runtime.GOOS + "-" + runtime.GOARCH + extension
			if file.Filename == want && file.OS == runtime.GOOS && file.Arch == runtime.GOARCH && len(file.SHA256) == 64 {
				chosen, filename, best = release, file.Filename, version
				break
			}
		}
	}
	if filename == "" {
		return goRelease{}, "", fmt.Errorf("no stable Go release for %s/%s satisfying Go %d.%d.%d", runtime.GOOS, runtime.GOARCH, required.major, required.minor, required.patch)
	}
	return chosen, filename, nil
}

func installManagedGo(ctx context.Context, client *http.Client, baseURL, root string, required goToolVersion, progress func(string)) (string, error) {
	if binary := cachedManagedGo(root, required); binary != "" {
		return binary, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/dl/?mode=json", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch Go release catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Go release catalog returned HTTP %d", response.StatusCode)
	}
	metadata, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(metadata) > 8<<20 {
		return "", errors.New("Go release catalog is unavailable or too large")
	}
	var releases []goRelease
	if err := json.Unmarshal(metadata, &releases); err != nil {
		return "", fmt.Errorf("decode Go release catalog: %w", err)
	}
	release, filename, err := selectGoRelease(releases, required)
	if err != nil {
		return "", err
	}
	if progress != nil {
		progress("Downloading " + release.Version + "…")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	archivePath := filepath.Join(stage, "download")
	expected := ""
	for _, file := range release.Files {
		if file.Filename == filename {
			expected = strings.ToLower(file.SHA256)
			break
		}
	}
	if err := downloadVerifiedGo(ctx, client, baseURL+"/dl/"+filename, archivePath, expected); err != nil {
		return "", err
	}
	if progress != nil {
		progress("Extracting " + release.Version + "…")
	}
	if strings.HasSuffix(filename, ".zip") {
		err = extractGoZip(archivePath, stage)
	} else {
		err = extractGoTar(archivePath, stage)
	}
	if err != nil {
		return "", err
	}
	binary := managedGoBinary(stage)
	actual, ok := goBinaryVersion(binary)
	want, _ := parseGoToolVersion(release.Version)
	if !ok || actual.compare(want) != 0 {
		return "", fmt.Errorf("downloaded %s toolchain did not pass version check", release.Version)
	}
	if err := os.Remove(archivePath); err != nil {
		return "", err
	}
	final := filepath.Join(root, release.Version)
	if err := os.Rename(stage, final); err != nil {
		if existing := cachedManagedGo(root, required); existing != "" {
			return existing, nil // another server process completed the same install
		}
		return "", fmt.Errorf("activate Go toolchain: %w", err)
	}
	return managedGoBinary(final), nil
}

func downloadVerifiedGo(ctx context.Context, client *http.Client, url, destination, expected string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download Go toolchain: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Go toolchain download returned HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.CopyN(io.MultiWriter(file, hash), response.Body, maxGoArchiveBytes+1)
	closeErr := file.Close()
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxGoArchiveBytes {
		return errors.New("Go toolchain archive exceeds size limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("Go toolchain SHA-256 mismatch")
	}
	return nil
}

func safeGoArchivePath(name string) (string, error) {
	if strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe Go archive path %q", name)
	}
	clean := path.Clean(name)
	if clean != "go" && !strings.HasPrefix(clean, "go/") {
		return "", fmt.Errorf("unsafe Go archive path %q", name)
	}
	return filepath.FromSlash(clean), nil
}

func writeGoArchiveFile(destination string, source io.Reader, size int64, mode os.FileMode) error {
	if size < 0 || size > maxGoExtractedBytes {
		return errors.New("Go archive entry exceeds size limit")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(file, source, size)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(destination, mode.Perm())
}

func extractGoTar(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	total := int64(0)
	type link struct {
		name, target string
		hard         bool
	}
	var links []link
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		clean, err := safeGoArchivePath(header.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			total += header.Size
			if total > maxGoExtractedBytes {
				return errors.New("Go archive extracted size limit exceeded")
			}
			if err := writeGoArchiveFile(target, reader, header.Size, header.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			linkTarget := header.Linkname
			if header.Typeflag == tar.TypeSymlink {
				if strings.HasPrefix(linkTarget, "/") || strings.Contains(linkTarget, "\\") {
					return fmt.Errorf("unsafe Go archive link %q", linkTarget)
				}
				linkTarget = path.Join(path.Dir(header.Name), linkTarget)
			}
			if _, err := safeGoArchivePath(linkTarget); err != nil {
				return err
			}
			links = append(links, link{header.Name, header.Linkname, header.Typeflag == tar.TypeLink})
		default:
			return fmt.Errorf("unsupported Go archive entry type %d", header.Typeflag)
		}
	}
	// Create links last: no later file can be extracted through a symlink.
	for _, item := range links {
		clean, _ := safeGoArchivePath(item.name)
		linkPath := filepath.Join(destination, clean)
		if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
			return err
		}
		if item.hard {
			target, _ := safeGoArchivePath(item.target)
			err = os.Link(filepath.Join(destination, target), linkPath)
		} else {
			err = os.Symlink(item.target, linkPath)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func extractGoZip(archivePath, destination string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	total := int64(0)
	for _, item := range archive.File {
		clean, err := safeGoArchivePath(item.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, clean)
		if item.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if !item.FileInfo().Mode().IsRegular() {
			return fmt.Errorf("unsupported Go zip entry %q", item.Name)
		}
		total += int64(item.UncompressedSize64)
		if total > maxGoExtractedBytes {
			return errors.New("Go archive extracted size limit exceeded")
		}
		body, err := item.Open()
		if err != nil {
			return err
		}
		err = writeGoArchiveFile(target, body, int64(item.UncompressedSize64), item.Mode())
		closeErr := body.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
