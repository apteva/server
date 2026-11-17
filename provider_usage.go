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
	providerUsageGrokBuildBaseURL = integrationGrokBuildRuntimeURL
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
type grokBuildProviderUsageFetcher struct{}

func providerUsageFetcherFor(providerKey string) providerUsageFetcher {
	switch providerKey {
	case openAICodexAuthProvider:
		return codexProviderUsageFetcher{}
	case integrationGrokBuildSlug:
		return grokBuildProviderUsageFetcher{}
	default:
		return nil
	}
}

func (grokBuildProviderUsageFetcher) CacheKey(state map[string]any) string {
	accessToken := strings.TrimSpace(stringFromNested(state, "credentials", "access_token"))
	identity := strings.TrimSpace(stringFromNested(state, "credentials", "principal_id"))
	if identity == "" {
		identity = strings.TrimSpace(stringFromNested(state, "credentials", "user_id"))
	}
	if identity == "" {
		identity = strings.TrimSpace(stringFromNested(state, "credentials", "account_email"))
	}
	if identity == "" {
		tokenSum := sha256.Sum256([]byte(accessToken))
		identity = "token:" + hex.EncodeToString(tokenSum[:])
	}
	sum := sha256.Sum256([]byte(integrationGrokBuildSlug + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}

func (grokBuildProviderUsageFetcher) FetchUsage(ctx context.Context, state map[string]any) (*ProviderUsageSnapshot, error) {
	credentials := map[string]string{
		"access_token":  strings.TrimSpace(stringFromNested(state, "credentials", "access_token")),
		"account_id":    strings.TrimSpace(stringFromNested(state, "credentials", "account_id")),
		"account_email": strings.TrimSpace(stringFromNested(state, "credentials", "account_email")),
		"user_id":       strings.TrimSpace(stringFromNested(state, "credentials", "user_id")),
		"principal_id":  strings.TrimSpace(stringFromNested(state, "credentials", "principal_id")),
	}
	if credentials["access_token"] == "" {
		return nil, fmt.Errorf("Grok Build auth is missing access_token")
	}

	userPayload, err := fetchGrokBuildUsageJSON(ctx, "/user", credentials, false)
	if err != nil {
		return nil, err
	}
	userID := grokBuildUsageString(userPayload, "userId", "user_id", "id")
	if !validGrokBuildUsageUserID(userID) {
		return nil, fmt.Errorf("Grok Build account identity could not be verified")
	}
	credentials["user_id"] = userID

	billingPayload, err := fetchGrokBuildUsageJSON(ctx, "/billing?format=credits", credentials, true)
	if err != nil {
		return nil, err
	}
	return normalizeGrokBuildUsage(billingPayload)
}

func fetchGrokBuildUsageJSON(ctx context.Context, path string, credentials map[string]string, includeUserID bool) (map[string]any, error) {
	endpoint := strings.TrimRight(providerUsageGrokBuildBaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	requestCredentials := credentials
	if !includeUserID {
		requestCredentials = make(map[string]string, len(credentials))
		for key, value := range credentials {
			requestCredentials[key] = value
		}
		requestCredentials["user_id"] = ""
		requestCredentials["account_id"] = ""
	}
	applyGrokBuildSessionHeaders(req, requestCredentials)
	req.Header.Set("User-Agent", "apteva-server/grok-build-usage")

	resp, err := providerUsageHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("Grok Build usage HTTP %d: %s", resp.StatusCode, summarizeUpstreamError(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Grok Build usage: %w", err)
	}
	return payload, nil
}

func normalizeGrokBuildUsage(payload map[string]any) (*ProviderUsageSnapshot, error) {
	config, _ := payload["config"].(map[string]any)
	if config == nil {
		return nil, fmt.Errorf("Grok Build billing response is missing config")
	}
	plan := grokBuildUsageString(payload, "subscriptionTier", "subscription_tier", "plan")
	period, _ := firstGrokBuildUsageObject(config, "currentPeriod", "current_period")
	periodType := grokBuildUsageString(period, "type")
	windowID := normalizeGrokBuildUsagePeriodID(periodType)
	start := grokBuildUsageTime(period, "start")
	end := grokBuildUsageTime(period, "end")
	if start == nil {
		start = grokBuildUsageTime(config, "billingPeriodStart", "billing_period_start")
	}
	if end == nil {
		end = grokBuildUsageTime(config, "billingPeriodEnd", "billing_period_end")
	}
	durationMinutes := 0
	if start != nil && end != nil && end.After(*start) {
		durationMinutes = int(end.Sub(*start).Minutes())
	}

	used, hasUsed := grokBuildUsagePercent(config, "creditUsagePercent", "credit_usage_percent")
	if !hasUsed {
		usedCents, hasUsedCents := grokBuildUsageCents(config, "used")
		limitCents, hasLimitCents := grokBuildUsageCents(config, "monthlyLimit", "monthly_limit")
		if hasUsedCents && hasLimitCents && limitCents > 0 {
			used = clampProviderUsagePercent(float64(usedCents) * 100 / float64(limitCents))
			hasUsed = true
		}
	}

	snapshot := &ProviderUsageSnapshot{
		Supported: true,
		Kind:      "subscription_quota",
		Plan:      plan,
	}
	if hasUsed {
		window := ProviderUsageWindow{
			ID:              windowID,
			UsedPercent:     used,
			DurationMinutes: durationMinutes,
			ResetsAt:        end,
		}
		snapshot.Limits = append(snapshot.Limits, ProviderUsageLimit{
			ID: "grok-build", Label: "Grok Build", Reached: used >= 100,
			Windows: []ProviderUsageWindow{window},
		})
	}

	if rawProducts, ok := firstGrokBuildUsageArray(config, "productUsage", "product_usage"); ok {
		for index, raw := range rawProducts {
			if index >= 16 {
				break
			}
			product, _ := raw.(map[string]any)
			label := grokBuildUsageString(product, "product", "name")
			productUsed, ok := grokBuildUsagePercent(product, "usagePercent", "usage_percent")
			if label == "" || !ok {
				continue
			}
			snapshot.Limits = append(snapshot.Limits, ProviderUsageLimit{
				ID: normalizeProviderUsageID(label), Label: label, Reached: productUsed >= 100,
				Windows: []ProviderUsageWindow{{
					ID: windowID, UsedPercent: productUsed, DurationMinutes: durationMinutes, ResetsAt: end,
				}},
			})
		}
	}

	if prepaid, ok := grokBuildUsageCents(config, "prepaidBalance", "prepaid_balance"); ok {
		snapshot.Credits = &ProviderUsageCredits{
			HasCredits: prepaid > 0,
			Balance:    formatProviderUsageCents(prepaid),
		}
	}
	if len(snapshot.Limits) == 0 && snapshot.Credits == nil {
		return nil, fmt.Errorf("Grok Build billing response contained no usable usage data")
	}
	return snapshot, nil
}

func validGrokBuildUsageUserID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func grokBuildUsageString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := values[key].(string)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value != "" && len(value) <= 256 {
			return value
		}
	}
	return ""
}

func firstGrokBuildUsageObject(values map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if value, ok := values[key].(map[string]any); ok {
			return value, true
		}
	}
	return nil, false
}

func firstGrokBuildUsageArray(values map[string]any, keys ...string) ([]any, bool) {
	for _, key := range keys {
		if value, ok := values[key].([]any); ok {
			return value, true
		}
	}
	return nil, false
}

func grokBuildUsagePercent(values map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		value, ok := values[key].(float64)
		if ok && value >= 0 && value <= 100 {
			return clampProviderUsagePercent(value), true
		}
	}
	return 0, false
}

func clampProviderUsagePercent(value float64) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return int(value + 0.5)
}

func grokBuildUsageCents(values map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		wrapper, ok := values[key].(map[string]any)
		if !ok {
			continue
		}
		value, ok := wrapper["val"].(float64)
		if ok && value >= 0 && value <= 1_000_000_000_000 && value == float64(int64(value)) {
			return int64(value), true
		}
	}
	return 0, false
}

func grokBuildUsageTime(values map[string]any, keys ...string) *time.Time {
	for _, key := range keys {
		raw, ok := values[key].(string)
		if !ok || len(raw) > 64 {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			parsed = parsed.UTC()
			return &parsed
		}
	}
	return nil
}

func normalizeGrokBuildUsagePeriodID(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "USAGE_PERIOD_TYPE_"))
	if value == "" {
		return "primary"
	}
	return normalizeProviderUsageID(value)
}

func normalizeProviderUsageID(value string) string {
	var builder strings.Builder
	lastDash := false
	for _, char := range strings.ToLower(value) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
			lastDash = false
		} else if !lastDash && builder.Len() > 0 {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func formatProviderUsageCents(value int64) string {
	return fmt.Sprintf("%d.%02d", value/100, value%100)
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
