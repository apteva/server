package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sourceArchiveFixture(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	zipper := gzip.NewWriter(&raw)
	writer := tar.NewWriter(zipper)
	for name, content := range entries {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func TestClonePublicGitHubArchiveSparse(t *testing.T) {
	archive := sourceArchiveFixture(t, map[string]string{
		"apteva-apps-abc/mcp/conversations/go.mod": "module example.test/conversations\n",
		"apteva-apps-abc/mcp/other/secret":         "not requested",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.EscapedPath(), "/apteva/apps/tarball/conversations%2Fv0.24.5") {
			t.Errorf("unexpected archive path: %q", r.URL.EscapedPath())
		}
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "src")
	err := clonePublicGitHubArchiveFrom(server.Client(), server.URL, dir, "https://github.com/apteva/apps.git", "conversations/v0.24.5", []string{"mcp/conversations"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mcp", "conversations", "go.mod")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mcp", "other", "secret")); !os.IsNotExist(err) {
		t.Fatalf("sparse archive included another app: %v", err)
	}
}

func TestClonePublicGitHubArchiveRejectsTraversal(t *testing.T) {
	archive := sourceArchiveFixture(t, map[string]string{
		"apteva-apps-abc/../../outside": "unsafe",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	defer server.Close()
	parent := t.TempDir()
	err := clonePublicGitHubArchiveFrom(server.Client(), server.URL, filepath.Join(parent, "src"), "https://github.com/apteva/apps.git", "main", nil)
	if err == nil {
		t.Fatal("accepted path traversal")
	}
	if _, err := os.Stat(filepath.Join(parent, "outside")); !os.IsNotExist(err) {
		t.Fatalf("archive escaped cache directory: %v", err)
	}
}

func TestPublicGitHubRepoRejectsOtherSources(t *testing.T) {
	for _, repo := range []string{"git@github.com:apteva/apps.git", "https://example.com/apteva/apps.git", "https://github.com/apteva/apps/extra.git"} {
		if _, _, err := publicGitHubRepo(repo); err == nil {
			t.Fatalf("accepted %q without Git", repo)
		}
	}
}

func TestClonePublicGitHubOfficialArchive(t *testing.T) {
	if os.Getenv("RUN_GITHUB_ARCHIVE_TESTS") != "1" {
		t.Skip("set RUN_GITHUB_ARCHIVE_TESTS=1 for the GitHub archive smoke test")
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	dir := filepath.Join(t.TempDir(), "src")
	if err := clonePublicGitHubArchiveFrom(client, githubArchiveAPI, dir, "https://github.com/apteva/apps.git", "conversations/v0.24.5", []string{"mcp/conversations"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mcp", "conversations", "go.mod")); err != nil {
		t.Fatal(err)
	}
}
