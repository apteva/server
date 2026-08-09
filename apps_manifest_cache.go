package main

// In-memory cache for manifest YAML fetched from registry entries.
// The marketplace endpoint resolves manifest_url for every catalog
// row to compute the Surfaces summary; without a cache that's N
// cross-internet round-trips per page load. With a 1h TTL the
// catalog feels instant after the first fetch and updates roll out
// within an hour.
//
// Failures (network error, non-200, parse error) are NOT cached —
// next call retries. Successful manifests are cached even when
// they're parse-rejected by ValidateManifest (we cache the parsed
// struct, so re-parsing is skipped) — that keeps malformed manifests
// from spamming us with retries while still letting them recover
// once the upstream fixes the file.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type manifestCacheEntry struct {
	manifest *sdk.Manifest
	fetched  time.Time
}

var (
	manifestCacheMu sync.RWMutex
	manifestCache   = map[string]manifestCacheEntry{}
	// 1 minute keeps "Update available" detection responsive without
	// hammering raw.githubusercontent.com — for a handful of installed
	// apps that's <10 fetches/min worst case. The marketplace handler
	// already amortizes the read by building one parallel batch per
	// page load, so the cache mostly serves to coalesce within that
	// batch + handle SSE-driven re-renders, not to span minutes.
	manifestCacheTTL = time.Minute
)

// fetchAndCacheManifest returns the parsed manifest for url, hitting
// the cache when fresh and the network otherwise. Returns nil + err
// for failures so the caller can decide whether to surface them.
func (s *Server) fetchAndCacheManifest(url string) (*sdk.Manifest, error) {
	if url == "" {
		return nil, fmt.Errorf("empty manifest_url")
	}
	manifestCacheMu.RLock()
	if e, ok := manifestCache[url]; ok && time.Since(e.fetched) < manifestCacheTTL {
		manifestCacheMu.RUnlock()
		return e.manifest, nil
	}
	manifestCacheMu.RUnlock()

	// Preserve the marketplace cache's existing timeout and document limit for
	// native manifests. Agent Plugin resolution is an additive second step.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/x-yaml, text/yaml, text/plain, */*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned http %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}
	body, err = s.resolveAgentPluginManifestDocument(url, body)
	if err != nil {
		return nil, err
	}
	m, err := sdk.ParseManifest(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", url, err)
	}
	manifestCacheMu.Lock()
	manifestCache[url] = manifestCacheEntry{manifest: m, fetched: time.Now()}
	manifestCacheMu.Unlock()
	return m, nil
}

// CuratedRegistry is the parsed payload of registry.json — same
// schema the dashboard renders in the marketplace tab. Cached
// in-memory so refreshManifestFromUpstream's per-app lookup doesn't
// re-fetch the registry once per call.
type CuratedRegistry struct {
	Schema string          `json:"schema"`
	Apps   []RegistryEntry `json:"apps"`
}

type registryCacheEntry struct {
	registry *CuratedRegistry
	fetched  time.Time
}

var (
	registryCacheMu sync.RWMutex
	registryCache   registryCacheEntry
	// Same 1-minute TTL as the manifest cache. Registry edits are
	// rare and "update available" doesn't need to be sub-second.
	registryCacheTTL = time.Minute
)

// fetchAndCacheRegistry returns the parsed curated registry, using
// the configured registry URL (env override → defaultRegistryURL).
// Cached for registryCacheTTL; failures aren't cached.
func (s *Server) fetchAndCacheRegistry() (*CuratedRegistry, error) {
	registryCacheMu.RLock()
	if registryCache.registry != nil && time.Since(registryCache.fetched) < registryCacheTTL {
		reg := registryCache.registry
		registryCacheMu.RUnlock()
		return reg, nil
	}
	registryCacheMu.RUnlock()

	url := getRegistryURLFromEnv()
	if url == "" {
		url = defaultRegistryURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s returned http %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}
	var reg CuratedRegistry
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", url, err)
	}
	registryCacheMu.Lock()
	registryCache = registryCacheEntry{registry: &reg, fetched: time.Now()}
	registryCacheMu.Unlock()
	return &reg, nil
}
