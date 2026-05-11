package main

// EnsureBinaries — fetcher behaviour:
//   * Empty / nil manifest dep list → no-op, empty PATH prefix.
//   * Successful download + sha256 + extract → returns the cache dir,
//     marker file present, executables chmodded.
//   * sha256 mismatch → install fails hard, no .ok marker left behind.
//   * Missing source for runtime platform → required=true errors,
//     required=false returns empty path.
//   * Idempotent — second call short-circuits on the .ok marker.
//
// We test against tar.gz fixtures (stdlib decoder) and raw archives so
// the test suite has no external tool dependency. tar.xz support is
// exercised in the integration / production path; we don't gate CI on
// xz being installed on the test runner.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// makeTarGzFixture builds a tar.gz archive in-memory whose layout is
//
//	<wrapper>/<exe>     containing "fake exe body for <exe>"
//
// for each (wrapper, exe) pair. Returns the tarball bytes + its sha256
// hex digest so the test can wire them into both the HTTP fixture and
// the manifest source.
func makeTarGzFixture(t *testing.T, wrapper string, exes ...string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if wrapper != "" {
		if err := tw.WriteHeader(&tar.Header{
			Name:     wrapper + "/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		}); err != nil {
			t.Fatalf("write wrapper hdr: %v", err)
		}
	}
	for _, exe := range exes {
		body := []byte("fake exe body for " + exe)
		path := exe
		if wrapper != "" {
			path = wrapper + "/" + exe
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     path,
			Mode:     0o755,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write exe hdr: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write exe body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

// withTempBinariesHome forces binariesRoot() to a t.TempDir so the
// test never touches the real ~/.apteva. Returns the temp dir for
// assertions.
func withTempBinariesHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APTEVA_HOME", dir)
	return filepath.Join(dir, "binaries")
}

func TestEnsureBinaries_NoDepsNoop(t *testing.T) {
	withTempBinariesHome(t)
	m := &sdk.Manifest{}
	pp, err := EnsureBinaries(m, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pp != "" {
		t.Errorf("expected empty PATH prefix, got %q", pp)
	}
}

func TestEnsureBinaries_TarGzHappyPath(t *testing.T) {
	binsDir := withTempBinariesHome(t)

	archive, sum := makeTarGzFixture(t, "ffmpeg-7.0.2-static", "ffmpeg", "ffprobe")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	platform := runtime.GOOS + "-" + runtime.GOARCH
	m := &sdk.Manifest{
		Requires: sdk.Requires{
			Binaries: []sdk.BinaryDep{{
				Name:        "ffmpeg",
				Version:     "7.0.2",
				Executables: []string{"ffmpeg", "ffprobe"},
				Required:    true,
				Sources: map[string]sdk.BinarySource{
					platform: {URL: srv.URL, SHA256: sum, Archive: "tar.gz", StripRoot: 1},
				},
			}},
		},
	}

	progressMsgs := []string{}
	pp, err := EnsureBinaries(m, func(s string) { progressMsgs = append(progressMsgs, s) })
	if err != nil {
		t.Fatalf("EnsureBinaries: %v", err)
	}
	cacheDir := filepath.Join(binsDir, "ffmpeg-7.0.2-"+platform)
	if pp != cacheDir {
		t.Errorf("PATH prefix = %q, want %q", pp, cacheDir)
	}
	for _, exe := range []string{"ffmpeg", "ffprobe"} {
		path := filepath.Join(cacheDir, exe)
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing %s: %v", exe, err)
		}
		if st.Mode()&0o111 == 0 {
			t.Errorf("%s not executable: mode=%v", exe, st.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(cacheDir, ".ok")); err != nil {
		t.Errorf("missing .ok marker: %v", err)
	}
	if len(progressMsgs) == 0 {
		t.Error("expected at least one progress message")
	}
}

func TestEnsureBinaries_ChecksumMismatchFailsHard(t *testing.T) {
	binsDir := withTempBinariesHome(t)
	archive, _ := makeTarGzFixture(t, "wrap", "ffmpeg")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	platform := runtime.GOOS + "-" + runtime.GOARCH
	m := &sdk.Manifest{
		Requires: sdk.Requires{
			Binaries: []sdk.BinaryDep{{
				Name:        "ffmpeg",
				Version:     "7.0.2",
				Executables: []string{"ffmpeg"},
				Required:    true,
				Sources: map[string]sdk.BinarySource{
					platform: {URL: srv.URL, SHA256: "deadbeef" + "00000000000000000000000000000000000000000000000000000000", Archive: "tar.gz", StripRoot: 1},
				},
			}},
		},
	}
	if _, err := EnsureBinaries(m, nil); err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
	// No leftover cache dir.
	cacheDir := filepath.Join(binsDir, "ffmpeg-7.0.2-"+platform)
	if _, err := os.Stat(filepath.Join(cacheDir, ".ok")); err == nil {
		t.Error(".ok marker should not be written on checksum failure")
	}
}

func TestEnsureBinaries_NoSourceForPlatformRequired(t *testing.T) {
	withTempBinariesHome(t)
	m := &sdk.Manifest{
		Requires: sdk.Requires{
			Binaries: []sdk.BinaryDep{{
				Name:     "ffmpeg",
				Version:  "7.0.2",
				Required: true,
				Sources: map[string]sdk.BinarySource{
					"plan9-amd64": {URL: "x", SHA256: "y"},
				},
			}},
		},
	}
	if _, err := EnsureBinaries(m, nil); err == nil {
		t.Fatal("expected error for missing platform source")
	}
}

func TestEnsureBinaries_NoSourceForPlatformOptional(t *testing.T) {
	withTempBinariesHome(t)
	m := &sdk.Manifest{
		Requires: sdk.Requires{
			Binaries: []sdk.BinaryDep{{
				Name:     "ffmpeg",
				Version:  "7.0.2",
				Required: false,
				Sources: map[string]sdk.BinarySource{
					"plan9-amd64": {URL: "x", SHA256: "y"},
				},
			}},
		},
	}
	pp, err := EnsureBinaries(m, nil)
	if err != nil {
		t.Fatalf("optional dep should not error: %v", err)
	}
	if pp != "" {
		t.Errorf("expected empty PATH for skipped optional dep, got %q", pp)
	}
}

func TestEnsureBinaries_IdempotentOnSecondCall(t *testing.T) {
	binsDir := withTempBinariesHome(t)
	archive, sum := makeTarGzFixture(t, "wrap", "ffmpeg")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Write(archive)
	}))
	defer srv.Close()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	m := &sdk.Manifest{
		Requires: sdk.Requires{
			Binaries: []sdk.BinaryDep{{
				Name:        "ffmpeg",
				Version:     "7.0.2",
				Executables: []string{"ffmpeg"},
				Required:    true,
				Sources: map[string]sdk.BinarySource{
					platform: {URL: srv.URL, SHA256: sum, Archive: "tar.gz", StripRoot: 1},
				},
			}},
		},
	}
	if _, err := EnsureBinaries(m, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := EnsureBinaries(m, nil); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected 1 HTTP fetch, got %d (second call should hit .ok marker)", hits)
	}
	if _, err := os.Stat(filepath.Join(binsDir, "ffmpeg-7.0.2-"+platform, ".ok")); err != nil {
		t.Errorf("missing .ok after second call: %v", err)
	}
}

func TestEnsureBinaries_RawArchive(t *testing.T) {
	binsDir := withTempBinariesHome(t)
	body := []byte("just an executable's bytes")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	m := &sdk.Manifest{
		Requires: sdk.Requires{
			Binaries: []sdk.BinaryDep{{
				Name:        "single",
				Version:     "1",
				Executables: []string{"thetool"},
				Required:    true,
				Sources: map[string]sdk.BinarySource{
					platform: {URL: srv.URL, SHA256: hex.EncodeToString(sum[:]), Archive: "raw"},
				},
			}},
		},
	}
	if _, err := EnsureBinaries(m, nil); err != nil {
		t.Fatalf("EnsureBinaries: %v", err)
	}
	thetool := filepath.Join(binsDir, "single-1-"+platform, "thetool")
	st, err := os.Stat(thetool)
	if err != nil {
		t.Fatalf("thetool missing: %v", err)
	}
	if st.Mode()&0o111 == 0 {
		t.Errorf("raw payload not chmodded executable: mode=%v", st.Mode())
	}
}
