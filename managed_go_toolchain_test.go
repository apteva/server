package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestModuleRequiredGoVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/app\n\ngo 1.24\ntoolchain go1.25.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := moduleRequiredGoVersion(dir)
	if err != nil || got != (goToolVersion{1, 25, 1}) {
		t.Fatalf("moduleRequiredGoVersion() = %v, %v", got, err)
	}
	if _, ok := parseGoToolVersion("go1.25.1-evil"); ok {
		t.Fatal("accepted non-release toolchain version")
	}
}

func TestResolveGoBinaryInstallsWhenMissingOrOutdated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/app\ngo 1.25.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	t.Setenv("PATH", pathDir)
	called := 0
	installer := func(required goToolVersion, progress func(string)) (string, error) {
		called++
		if required != (goToolVersion{1, 25, 1}) {
			t.Fatalf("required = %v", required)
		}
		return "/managed/go", nil
	}
	got, err := resolveGoBinaryWithInstaller(dir, nil, nil, installer)
	if err != nil || got != "/managed/go" || called != 1 {
		t.Fatalf("missing Go: got %q, %v; installer called %d times", got, err, called)
	}
	oldGo := filepath.Join(pathDir, "go")
	if err := os.WriteFile(oldGo, []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo 'go version go1.24.4 test'; fi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err = resolveGoBinaryWithInstaller(dir, nil, nil, installer)
	if err != nil || got != "/managed/go" || called != 2 {
		t.Fatalf("outdated Go: got %q, %v; installer called %d times", got, err, called)
	}
}

func fakeGoTar(t *testing.T, version, entry string) []byte {
	t.Helper()
	var raw bytes.Buffer
	zipper := gzip.NewWriter(&raw)
	writer := tar.NewWriter(zipper)
	script := fmt.Sprintf("#!/bin/sh\necho 'go version %s test'\n", version)
	if err := writer.WriteHeader(&tar.Header{Name: entry, Mode: 0755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func fakeGoReleaseServer(t *testing.T, archive []byte, checksum string) *httptest.Server {
	t.Helper()
	filename := "go1.25.1." + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	release := []goRelease{{Version: "go1.25.1", Stable: true}}
	release[0].Files = append(release[0].Files, struct {
		Filename string `json:"filename"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		SHA256   string `json:"sha256"`
	}{filename, runtime.GOOS, runtime.GOARCH, checksum})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/":
			if r.URL.RawQuery != "mode=json" {
				t.Errorf("unexpected query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(release)
		case "/dl/" + filename:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestInstallManagedGoVerifiedAndCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	archive := fakeGoTar(t, "go1.25.1", "go/bin/go")
	sum := sha256.Sum256(archive)
	server := fakeGoReleaseServer(t, archive, hex.EncodeToString(sum[:]))
	root := t.TempDir()
	got, err := installManagedGo(context.Background(), server.Client(), server.URL, root, goToolVersion{1, 25, 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if version, ok := goBinaryVersion(got); !ok || version != (goToolVersion{1, 25, 1}) {
		t.Fatalf("installed binary %q has version %v, valid=%v", got, version, ok)
	}
	server.Close()
	gotAgain, err := installManagedGo(context.Background(), http.DefaultClient, "http://127.0.0.1:1", root, goToolVersion{1, 25, 1}, nil)
	if err != nil || gotAgain != got {
		t.Fatalf("cached install = %q, %v; want %q", gotAgain, err, got)
	}
}

func TestInstallManagedGoRejectsCorruptOrUnsafeArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar fixture is Unix-only")
	}
	for _, tc := range []struct {
		name, entry, checksum string
	}{
		{name: "checksum", entry: "go/bin/go", checksum: strings.Repeat("0", 64)},
		{name: "traversal", entry: "go/../../outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := fakeGoTar(t, "go1.25.1", tc.entry)
			sum := sha256.Sum256(archive)
			checksum := tc.checksum
			if checksum == "" {
				checksum = hex.EncodeToString(sum[:])
			}
			server := fakeGoReleaseServer(t, archive, checksum)
			defer server.Close()
			root := t.TempDir()
			if _, err := installManagedGo(context.Background(), server.Client(), server.URL, root, goToolVersion{1, 25, 1}, nil); err == nil {
				t.Fatal("unsafe archive installed")
			}
			if _, err := os.Stat(filepath.Join(root, "outside")); !os.IsNotExist(err) {
				t.Fatalf("archive escaped install root: %v", err)
			}
		})
	}
}

func TestExtractGoZipRejectsTraversal(t *testing.T) {
	var raw bytes.Buffer
	writer := zip.NewWriter(&raw)
	file, err := writer.Create("go/../../outside")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("unsafe")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	archivePath := filepath.Join(root, "go.zip")
	if err := os.WriteFile(archivePath, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extractGoZip(archivePath, root); err == nil {
		t.Fatal("accepted a path outside go/")
	}
}

func TestInstallManagedGoOfficialRelease(t *testing.T) {
	if os.Getenv("RUN_GO_TOOLCHAIN_DOWNLOAD_TESTS") != "1" {
		t.Skip("set RUN_GO_TOOLCHAIN_DOWNLOAD_TESTS=1 for the official release smoke test")
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	path, err := installManagedGo(context.Background(), client, goReleaseURL, t.TempDir(), goToolVersion{1, 25, 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	version, ok := goBinaryVersion(path)
	if !ok || version.compare(goToolVersion{1, 25, 1}) < 0 {
		t.Fatalf("official toolchain = %q, version=%v, valid=%v", path, version, ok)
	}
}
