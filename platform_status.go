package main

// Platform self-update status — server-side counterpart to the
// `apteva update` CLI subcommand.
//
// Polls the canonical version manifest (apteva/version.json on
// raw.githubusercontent.com — same publish pattern apps use for
// app-registry/registry.json), compares against the versions baked
// into our own binary via -ldflags, and exposes the result at
// /api/platform-status. The dashboard renders a small "update
// available" pill from the response. The action itself stays in
// the CLI: this endpoint is purely informational.
//
// Caches the latest manifest to <dataDir>/platform-status.json so
// an offline restart doesn't lose the current view; the cache is
// best-effort, never load-bearing.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	platformVersionURL = "https://raw.githubusercontent.com/apteva/apteva/main/version.json"
	platformPollEvery  = 6 * time.Hour
)

// platformVersionManifest mirrors the schema apteva/version.json
// publishes (defined in apteva/update.go). Only the fields we read
// here are listed; unknown keys are tolerated.
type platformVersionManifest struct {
	Schema          string            `json:"schema"`
	Version         string            `json:"version"`
	ReleasedAt      string            `json:"released_at"`
	ReleaseNotesURL string            `json:"release_notes_url"`
	Components      map[string]string `json:"components"`
}

type platformComponentStatus struct {
	Name            string `json:"name"`
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
}

type platformStatusView struct {
	PolledAt        time.Time                 `json:"polled_at"`
	BundleVersion   string                    `json:"bundle_version,omitempty"`     // version of the published tarball
	ReleaseNotesURL string                    `json:"release_notes_url,omitempty"`
	Components      []platformComponentStatus `json:"components"`
	UpdateAvailable bool                      `json:"update_available"` // true if any component says so
	Error           string                    `json:"error,omitempty"`
}

type platformStatusPoller struct {
	cachePath string
	mu        sync.RWMutex
	view      platformStatusView
	stop      chan struct{}
}

func newPlatformStatusPoller(dataDir string) *platformStatusPoller {
	p := &platformStatusPoller{
		cachePath: filepath.Join(dataDir, "platform-status.json"),
		stop:      make(chan struct{}),
	}
	p.loadCache()
	return p
}

// Run blocks; caller should `go p.Run()`. First poll fires
// immediately so the dashboard has data on the first request.
func (p *platformStatusPoller) Run() {
	p.poll()
	t := time.NewTicker(platformPollEvery)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.poll()
		}
	}
}

func (p *platformStatusPoller) Stop() { close(p.stop) }

// Refresh forces an immediate re-poll. Wired to
// POST /api/platform-status/refresh so the dashboard's "Check now"
// button doesn't have to wait for the 6h tick.
func (p *platformStatusPoller) Refresh() platformStatusView {
	p.poll()
	return p.View()
}

func (p *platformStatusPoller) View() platformStatusView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.view
}

func (p *platformStatusPoller) poll() {
	now := time.Now().UTC()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(platformVersionURL)
	if err != nil {
		p.recordError(now, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		p.recordError(now, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		p.recordError(now, err.Error())
		return
	}
	var m platformVersionManifest
	if err := json.Unmarshal(body, &m); err != nil {
		p.recordError(now, "manifest unmarshal: "+err.Error())
		return
	}

	view := platformStatusView{
		PolledAt:        now,
		BundleVersion:   m.Version,
		ReleaseNotesURL: m.ReleaseNotesURL,
	}

	// versionInfo() (server/main.go) returns the locally-baked versions
	// keyed by short component name. The version manifest keys by the
	// canonical "apteva-<name>" form. Map between the two so the
	// component list reads naturally for dashboard consumers.
	current := versionInfo()
	pairs := []struct {
		display       string // what the dashboard shows
		manifestKey   string // key in the published manifest
		localKey      string // key in versionInfo()
	}{
		{"apteva", "apteva", "cli"},
		{"apteva-server", "apteva-server", "apteva"},
		{"apteva-core", "apteva-core", "core"},
		{"apteva-dashboard", "apteva-dashboard", "dashboard"},
		{"apteva-integrations", "apteva-integrations", "integrations"},
	}

	for _, e := range pairs {
		curStr, _ := current[e.localKey].(string)
		latest := m.Components[e.manifestKey]
		// "dev" means no -ldflags injection happened (source build /
		// build-local.sh) — those installs aren't update candidates,
		// don't claim an update is available.
		updateAvail := latest != "" && curStr != "" && curStr != "dev" && curStr != latest
		view.Components = append(view.Components, platformComponentStatus{
			Name:            e.display,
			Current:         curStr,
			Latest:          latest,
			UpdateAvailable: updateAvail,
		})
	}
	// Whether to fire the pill is decided by the BUNDLE version only —
	// release tarballs ship cli + server + core atomically, so the
	// umbrella version is the canonical "what's installed". Per-component
	// flags above stay informational; aggregating them into the pill
	// turns a stale-by-one-key manifest into a false positive (which is
	// how this very check first fired even though the user was on the
	// latest published bundle).
	cliCurrent, _ := current["cli"].(string)
	view.UpdateAvailable = m.Version != "" && cliCurrent != "" && cliCurrent != "dev" && cliCurrent != m.Version

	p.mu.Lock()
	p.view = view
	p.mu.Unlock()
	p.saveCache()
}

func (p *platformStatusPoller) recordError(now time.Time, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Preserve the last-good components on transient failures — better
	// for the dashboard to show a slightly stale view than to flicker
	// between full data and an empty error state.
	p.view.PolledAt = now
	p.view.Error = msg
}

func (p *platformStatusPoller) loadCache() {
	data, err := os.ReadFile(p.cachePath)
	if err != nil {
		return
	}
	var v platformStatusView
	if err := json.Unmarshal(data, &v); err != nil {
		return
	}
	p.mu.Lock()
	p.view = v
	p.mu.Unlock()
}

func (p *platformStatusPoller) saveCache() {
	p.mu.RLock()
	data, err := json.MarshalIndent(p.view, "", "  ")
	p.mu.RUnlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(p.cachePath, data, 0644)
}

// HTTP handlers — both registered in server/main.go's apiMux.

func (s *Server) handlePlatformStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.platformStatus.View())
}

func (s *Server) handlePlatformStatusRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.platformStatus.Refresh())
}
