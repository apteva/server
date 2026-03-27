package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	catalogRepo    = "apteva/integrations"
	catalogTarball = "https://api.github.com/repos/" + catalogRepo + "/tarball/main"
)

// GET /integrations/catalog/status
func (s *Server) handleCatalogStatus(w http.ResponseWriter, r *http.Request) {
	integrationsDir := filepath.Join(s.dataDir, "integrations")

	var lastUpdated *time.Time
	if info, err := os.Stat(integrationsDir); err == nil {
		t := info.ModTime()
		lastUpdated = &t
	}

	writeJSON(w, map[string]any{
		"count":        s.catalog.Count(),
		"installed":    s.catalog.Count() > 0,
		"last_updated": lastUpdated,
	})
}

// POST /integrations/catalog/download
func (s *Server) handleCatalogDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	integrationsDir := filepath.Join(s.dataDir, "integrations")

	// Download tarball from GitHub
	fmt.Fprintf(os.Stderr, "downloading integration catalog from %s...\n", catalogRepo)

	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", catalogTarball, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("download failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		http.Error(w, fmt.Sprintf("GitHub returned %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// Clean and recreate target dir
	os.RemoveAll(integrationsDir)
	os.MkdirAll(integrationsDir, 0755)

	// Extract .json files from tarball
	count, err := extractAppsFromTarball(resp.Body, integrationsDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("extraction failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Reload catalog
	s.catalog.LoadFromDir(integrationsDir)
	fmt.Fprintf(os.Stderr, "integration catalog updated: %d integrations\n", s.catalog.Count())

	writeJSON(w, map[string]any{
		"status":    "downloaded",
		"count":     count,
		"directory": integrationsDir,
	})
}

// extractAppsFromTarball reads a .tar.gz stream and extracts src/apps/*.json files
func extractAppsFromTarball(r io.Reader, destDir string) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	count := 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, fmt.Errorf("tar: %w", err)
		}

		// GitHub tarballs have a prefix like "apteva-integrations-abc1234/"
		// We want files matching */src/apps/*.json
		name := header.Name
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if !strings.Contains(name, "/src/apps/") {
			continue
		}

		// Extract just the filename
		base := filepath.Base(name)
		destPath := filepath.Join(destDir, base)

		f, err := os.Create(destPath)
		if err != nil {
			continue
		}
		io.Copy(f, tr)
		f.Close()
		count++
	}

	return count, nil
}
