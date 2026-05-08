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

// POST /integrations/catalog/reload — re-reads local integration definitions from disk
func (s *Server) handleCatalogReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	before := s.catalog.Count()
	if err := s.catalog.LoadFromDir(s.appsDir); err != nil {
		http.Error(w, fmt.Sprintf("reload failed: %v", err), http.StatusInternalServerError)
		return
	}
	after := s.catalog.Count()
	fmt.Fprintf(os.Stderr, "integration catalog reloaded: %d integrations (was %d)\n", after, before)

	writeJSON(w, map[string]any{
		"status":   "reloaded",
		"before":   before,
		"after":    after,
		"apps_dir": s.appsDir,
	})
}

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

// POST /integrations/catalog/download — kept as a curl-able power-user
// endpoint. The Settings UI no longer surfaces it; auto-download on
// first boot is now the primary path.
func (s *Server) handleCatalogDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	count, dir, err := downloadIntegrationCatalog(s.catalog, s.dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"status":    "downloaded",
		"count":     count,
		"directory": dir,
	})
}

// downloadIntegrationCatalog fetches the latest integration catalog
// tarball from the apteva/integrations github repo, extracts app
// JSON files into <dataDir>/integrations/, and reloads the in-memory
// catalog. Used by both the manual download endpoint and the boot-
// time fallback when no catalog is found on disk. Free function so
// it can be called before the Server struct is fully constructed.
func downloadIntegrationCatalog(catalog *AppCatalog, dataDir string) (int, string, error) {
	integrationsDir := filepath.Join(dataDir, "integrations")
	fmt.Fprintf(os.Stderr, "downloading integration catalog from %s...\n", catalogRepo)

	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", catalogTarball, nil)
	if err != nil {
		return 0, integrationsDir, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, integrationsDir, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, integrationsDir, fmt.Errorf("github returned http %d", resp.StatusCode)
	}

	os.RemoveAll(integrationsDir)
	os.MkdirAll(integrationsDir, 0755)

	count, err := extractAppsFromTarball(resp.Body, integrationsDir)
	if err != nil {
		return 0, integrationsDir, fmt.Errorf("extract: %w", err)
	}

	catalog.LoadFromDir(integrationsDir)
	fmt.Fprintf(os.Stderr, "integration catalog updated: %d integrations\n", catalog.Count())
	return count, integrationsDir, nil
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
