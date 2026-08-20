package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const providerUsageCapability = "subscription_usage"

var (
	providerUsageCodexBaseURL     = "https://chatgpt.com/backend-api"
	providerUsageHTTPClient       = &http.Client{Timeout: 8 * time.Second}
	providerUsageFreshTTL         = 2 * time.Minute
	providerUsageStaleTTL         = 30 * time.Minute
	providerUsageManualRefreshMin = 30 * time.Second
)

type ProviderUsageSnapshot struct {
	Supported            bool                  `json:"supported"`
	ProviderID           int64                 `json:"provider_id"`
	Kind                 string                `json:"kind,omitempty"`
	Plan                 string                `json:"plan,omitempty"`
	FetchedAt            time.Time             `json:"fetched_at,omitempty"`
	Stale                bool                  `json:"stale,omitempty"`
	Limits               []ProviderUsageLimit  `json:"limits,omitempty"`
	Credits              *ProviderUsageCredits `json:"credits,omitempty"`
	RateLimitReachedType string                `json:"rate_limit_reached_type,omitempty"`
}

type ProviderUsageLimit struct {
	ID      string                `json:"id"`
	Label   string                `json:"label"`
	Reached bool                  `json:"reached,omitempty"`
	Windows []ProviderUsageWindow `json:"windows,omitempty"`
}

type ProviderUsageWindow struct {
	ID              string     `json:"id"`
	UsedPercent     int        `json:"used_percent"`
	DurationMinutes int        `json:"duration_minutes,omitempty"`
	ResetsAt        *time.Time `json:"resets_at,omitempty"`
}

type ProviderUsageCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

type providerUsageFetcher interface {
	CacheKey(state map[string]any) string
	FetchUsage(ctx context.Context, state map[string]any) (*ProviderUsageSnapshot, error)
}

type codexProviderUsageFetcher struct{}

func providerUsageFetcherFor(providerKey string) providerUsageFetcher {
	switch providerKey {
	case openAICodexAuthProvider:
		return codexProviderUsageFetcher{}
	default:
		return nil
	}
}

type providerUsageCacheEntry struct {
	snapshot ProviderUsageSnapshot
	fetched  time.Time
}

type providerUsageCacheStore struct {
	mu      sync.RWMutex
	entries map[string]providerUsageCacheEntry
	locks   sync.Map
}

var globalProviderUsageCache = &providerUsageCacheStore{entries: map[string]providerUsageCacheEntry{}}

func (c *providerUsageCacheStore) entry(key string) (providerUsageCacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if ok {
		entry.snapshot = cloneProviderUsageSnapshot(entry.snapshot)
	}
	return entry, ok
}

func (c *providerUsageCacheStore) put(key string, entry providerUsageCacheEntry) {
	entry.snapshot = cloneProviderUsageSnapshot(entry.snapshot)
	c.mu.Lock()
	c.entries[key] = entry
	c.mu.Unlock()
}

func (c *providerUsageCacheStore) lockFor(key string) *sync.Mutex {
	value, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func cloneProviderUsageSnapshot(snapshot ProviderUsageSnapshot) ProviderUsageSnapshot {
	clone := snapshot
	clone.Limits = append([]ProviderUsageLimit(nil), snapshot.Limits...)
	for i := range clone.Limits {
		clone.Limits[i].Windows = append([]ProviderUsageWindow(nil), snapshot.Limits[i].Windows...)
		for j := range clone.Limits[i].Windows {
			if reset := snapshot.Limits[i].Windows[j].ResetsAt; reset != nil {
				resetCopy := *reset
				clone.Limits[i].Windows[j].ResetsAt = &resetCopy
			}
		}
	}
	if snapshot.Credits != nil {
		credits := *snapshot.Credits
		clone.Credits = &credits
	}
	return clone
}

func (codexProviderUsageFetcher) CacheKey(state map[string]any) string {
	accessToken := strings.TrimSpace(stringFromNested(state, "credentials", "access_token"))
	accountID := strings.TrimSpace(codexAccountIDFromState(state))
	subject, _ := jwtClaims(accessToken)["sub"].(string)
	subject = strings.TrimSpace(subject)
	identity := "account:" + accountID + "\x00subject:" + subject
	if accountID == "" || subject == "" {
		tokenSum := sha256.Sum256([]byte(accessToken))
		identity += "\x00token:" + hex.EncodeToString(tokenSum[:])
	}
	sum := sha256.Sum256([]byte(openAICodexAuthProvider + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}

func (codexProviderUsageFetcher) FetchUsage(ctx context.Context, state map[string]any) (*ProviderUsageSnapshot, error) {
	accessToken := strings.TrimSpace(stringFromNested(state, "credentials", "access_token"))
	if accessToken == "" {
		return nil, fmt.Errorf("OpenAI Codex auth is missing access_token")
	}
	endpoint := strings.TrimRight(providerUsageCodexBaseURL, "/") + "/wham/usage"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if accountID := strings.TrimSpace(codexAccountIDFromState(state)); accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "apteva-server/codex-usage")

	resp, err := providerUsageHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("Codex usage HTTP %d: %s", resp.StatusCode, summarizeUpstreamError(body))
	}

	var payload codexProviderUsagePayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Codex usage: %w", err)
	}
	snapshot := &ProviderUsageSnapshot{
		Supported:            true,
		Kind:                 "subscription_quota",
		Plan:                 strings.TrimSpace(payload.PlanType),
		RateLimitReachedType: strings.TrimSpace(payload.RateLimitReachedType.Type),
		Limits: []ProviderUsageLimit{
			codexUsageLimit("codex", "Codex", payload.RateLimit),
		},
	}
	if payload.Credits != nil {
		snapshot.Credits = &ProviderUsageCredits{
			HasCredits: payload.Credits.HasCredits,
			Unlimited:  payload.Credits.Unlimited,
			Balance:    strings.TrimSpace(payload.Credits.Balance),
		}
	}
	for _, additional := range payload.AdditionalRateLimits {
		id := strings.TrimSpace(additional.MeteredFeature)
		if id == "" {
			continue
		}
		label := strings.TrimSpace(additional.LimitName)
		if label == "" {
			label = id
		}
		snapshot.Limits = append(snapshot.Limits, codexUsageLimit(id, label, additional.RateLimit))
	}
	return snapshot, nil
}

type codexProviderUsagePayload struct {
	PlanType             string                         `json:"plan_type"`
	RateLimit            *codexProviderRateLimit        `json:"rate_limit"`
	Credits              *codexProviderUsageCredits     `json:"credits"`
	AdditionalRateLimits []codexProviderAdditionalLimit `json:"additional_rate_limits"`
	RateLimitReachedType codexProviderRateLimitReached  `json:"rate_limit_reached_type"`
}

type codexProviderRateLimitReached struct {
	Type string `json:"type"`
}

type codexProviderUsageCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

type codexProviderAdditionalLimit struct {
	LimitName      string                  `json:"limit_name"`
	MeteredFeature string                  `json:"metered_feature"`
	RateLimit      *codexProviderRateLimit `json:"rate_limit"`
}

type codexProviderRateLimit struct {
	LimitReached    bool                      `json:"limit_reached"`
	PrimaryWindow   *codexProviderUsageWindow `json:"primary_window"`
	SecondaryWindow *codexProviderUsageWindow `json:"secondary_window"`
}

type codexProviderUsageWindow struct {
	UsedPercent       int   `json:"used_percent"`
	LimitWindowSecond int   `json:"limit_window_seconds"`
	ResetAt           int64 `json:"reset_at"`
}

func codexUsageLimit(id, label string, limit *codexProviderRateLimit) ProviderUsageLimit {
	normalized := ProviderUsageLimit{ID: id, Label: label}
	if limit == nil {
		return normalized
	}
	normalized.Reached = limit.LimitReached
	if limit.PrimaryWindow != nil {
		normalized.Windows = append(normalized.Windows, normalizeCodexUsageWindow("primary", limit.PrimaryWindow))
	}
	if limit.SecondaryWindow != nil {
		normalized.Windows = append(normalized.Windows, normalizeCodexUsageWindow("secondary", limit.SecondaryWindow))
	}
	return normalized
}

func normalizeCodexUsageWindow(id string, window *codexProviderUsageWindow) ProviderUsageWindow {
	used := window.UsedPercent
	if used < 0 {
		used = 0
	} else if used > 100 {
		used = 100
	}
	durationMinutes := 0
	if window.LimitWindowSecond > 0 {
		durationMinutes = (window.LimitWindowSecond + 59) / 60
	}
	normalized := ProviderUsageWindow{ID: id, UsedPercent: used, DurationMinutes: durationMinutes}
	if window.ResetAt > 0 {
		reset := time.Unix(window.ResetAt, 0).UTC()
		normalized.ResetsAt = &reset
	}
	return normalized
}

func providerUsageStaleOrError(providerID int64, entry providerUsageCacheEntry, ok bool, fetchErr error) (*ProviderUsageSnapshot, error) {
	if ok && time.Since(entry.fetched) < providerUsageStaleTTL {
		snapshot := entry.snapshot
		snapshot.ProviderID = providerID
		snapshot.Stale = true
		return &snapshot, nil
	}
	return nil, fetchErr
}
