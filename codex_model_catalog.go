package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// codexModelCatalogClientVersion declares the Codex catalog schema Apteva
// understands. OpenAI filters models by this value; 0.144.0 is the first
// schema version that exposes the GPT-5.6 Sol/Terra/Luna family.
const codexModelCatalogClientVersion = "0.144.0"

var (
	codexModelCatalogBaseURL  = openAICodexBackendAPIBaseURL
	codexModelCatalogClient   = &http.Client{Timeout: 15 * time.Second}
	codexModelCatalogFreshTTL = 15 * time.Minute
	codexModelCatalogStaleTTL = 24 * time.Hour
)

type ModelReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description,omitempty"`
}

// ProviderModelCapabilities is the provider-neutral subset of catalog
// metadata that affects Apteva runtime behavior. Codex-specific UI/client
// policy fields stay out of core until Apteva explicitly supports them.
type ProviderModelCapabilities struct {
	ContextWindow                 int                   `json:"context_window,omitempty"`
	MaxContextWindow              int                   `json:"max_context_window,omitempty"`
	EffectiveContextWindowPercent int                   `json:"effective_context_window_percent,omitempty"`
	DefaultReasoningLevel         string                `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels      []ModelReasoningLevel `json:"supported_reasoning_levels,omitempty"`
	InputModalities               []string              `json:"input_modalities,omitempty"`
	SupportsParallelToolCalls     *bool                 `json:"supports_parallel_tool_calls,omitempty"`
	SupportsReasoningSummaries    *bool                 `json:"supports_reasoning_summaries,omitempty"`
	SupportsImageDetailOriginal   *bool                 `json:"supports_image_detail_original,omitempty"`
	SupportsSearchTool            *bool                 `json:"supports_search_tool,omitempty"`
}

type codexCatalogCacheEntry struct {
	models  []ModelInfo
	etag    string
	fetched time.Time
}

type codexCatalogCacheStore struct {
	mu      sync.RWMutex
	entries map[string]codexCatalogCacheEntry
	locks   sync.Map
}

var globalCodexCatalogCache = &codexCatalogCacheStore{entries: map[string]codexCatalogCacheEntry{}}

func codexCatalogCacheKey(accountID, accessToken string) string {
	accountID = strings.TrimSpace(accountID)
	accessToken = strings.TrimSpace(accessToken)
	subject, _ := jwtClaims(accessToken)["sub"].(string)
	subject = strings.TrimSpace(subject)
	identity := "account:" + accountID + "\x00subject:" + subject
	if accountID == "" || subject == "" {
		tokenSum := sha256.Sum256([]byte(accessToken))
		identity += "\x00token:" + hex.EncodeToString(tokenSum[:])
	}
	sum := sha256.Sum256([]byte(codexModelCatalogBaseURL + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}

func cloneModelInfos(in []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, len(in))
	copy(out, in)
	for i := range out {
		out[i].Capabilities.SupportedReasoningLevels = append([]ModelReasoningLevel(nil), in[i].Capabilities.SupportedReasoningLevels...)
		out[i].Capabilities.InputModalities = append([]string(nil), in[i].Capabilities.InputModalities...)
	}
	return out
}

func (c *codexCatalogCacheStore) entry(key string) (codexCatalogCacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if ok {
		entry.models = cloneModelInfos(entry.models)
	}
	return entry, ok
}

func (c *codexCatalogCacheStore) put(key string, entry codexCatalogCacheEntry) {
	entry.models = cloneModelInfos(entry.models)
	c.mu.Lock()
	c.entries[key] = entry
	c.mu.Unlock()
}

func (c *codexCatalogCacheStore) lockFor(key string) *sync.Mutex {
	v, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func fetchCodexModelCatalog(ctx context.Context, accessToken, accountID string, force bool) ([]ModelInfo, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("OpenAI Codex auth is missing access_token")
	}
	key := codexCatalogCacheKey(accountID, accessToken)
	if entry, ok := globalCodexCatalogCache.entry(key); ok && !force && time.Since(entry.fetched) < codexModelCatalogFreshTTL {
		return entry.models, nil
	}

	lock := globalCodexCatalogCache.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	entry, hasEntry := globalCodexCatalogCache.entry(key)
	if hasEntry && !force && time.Since(entry.fetched) < codexModelCatalogFreshTTL {
		return entry.models, nil
	}

	endpoint := strings.TrimRight(codexModelCatalogBaseURL, "/") + "/models?client_version=" + url.QueryEscape(codexModelCatalogClientVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if strings.TrimSpace(accountID) != "" {
		req.Header.Set("ChatGPT-Account-ID", strings.TrimSpace(accountID))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "apteva-server/codex-catalog-"+codexModelCatalogClientVersion)
	if hasEntry && entry.etag != "" {
		req.Header.Set("If-None-Match", entry.etag)
	}

	resp, err := codexModelCatalogClient.Do(req)
	if err != nil {
		if hasEntry && time.Since(entry.fetched) < codexModelCatalogStaleTTL {
			return entry.models, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && hasEntry {
		entry.fetched = time.Now()
		globalCodexCatalogCache.put(key, entry)
		return entry.models, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if hasEntry && resp.StatusCode >= 500 && time.Since(entry.fetched) < codexModelCatalogStaleTTL {
			return entry.models, nil
		}
		return nil, fmt.Errorf("Codex models HTTP %d: %s", resp.StatusCode, summarizeUpstreamError(body))
	}

	var payload struct {
		Models []struct {
			Slug                          string                `json:"slug"`
			DisplayName                   string                `json:"display_name"`
			Description                   string                `json:"description"`
			Visibility                    string                `json:"visibility"`
			Priority                      int                   `json:"priority"`
			SupportedInAPI                *bool                 `json:"supported_in_api"`
			ContextWindow                 int                   `json:"context_window"`
			MaxContextWindow              int                   `json:"max_context_window"`
			EffectiveContextWindowPercent int                   `json:"effective_context_window_percent"`
			DefaultReasoningLevel         string                `json:"default_reasoning_level"`
			SupportedReasoningLevels      []ModelReasoningLevel `json:"supported_reasoning_levels"`
			InputModalities               []string              `json:"input_modalities"`
			SupportsParallelToolCalls     *bool                 `json:"supports_parallel_tool_calls"`
			SupportsReasoningSummaries    *bool                 `json:"supports_reasoning_summaries"`
			SupportsImageDetailOriginal   *bool                 `json:"supports_image_detail_original"`
			SupportsSearchTool            *bool                 `json:"supports_search_tool"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Codex models: %w", err)
	}
	models := make([]ModelInfo, 0, len(payload.Models))
	for _, model := range payload.Models {
		if strings.TrimSpace(model.Slug) == "" || !strings.EqualFold(strings.TrimSpace(model.Visibility), "list") ||
			!codexRuntimeModelAllowed(model.Slug) {
			continue
		}
		name := strings.TrimSpace(model.DisplayName)
		if name == "" {
			name = model.Slug
		}
		models = append(models, ModelInfo{
			ID:           model.Slug,
			Name:         name,
			Description:  model.Description,
			ContextSize:  model.ContextWindow,
			Priority:     model.Priority,
			SupportedAPI: model.SupportedInAPI,
			Capabilities: ProviderModelCapabilities{
				ContextWindow:                 model.ContextWindow,
				MaxContextWindow:              model.MaxContextWindow,
				EffectiveContextWindowPercent: model.EffectiveContextWindowPercent,
				DefaultReasoningLevel:         model.DefaultReasoningLevel,
				SupportedReasoningLevels:      model.SupportedReasoningLevels,
				InputModalities:               model.InputModalities,
				SupportsParallelToolCalls:     model.SupportsParallelToolCalls,
				SupportsReasoningSummaries:    model.SupportsReasoningSummaries,
				SupportsImageDetailOriginal:   model.SupportsImageDetailOriginal,
				SupportsSearchTool:            model.SupportsSearchTool,
			},
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("Codex model catalog contained no visible models")
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Priority != models[j].Priority {
			return models[i].Priority < models[j].Priority
		}
		return models[i].ID < models[j].ID
	})
	globalCodexCatalogCache.put(key, codexCatalogCacheEntry{
		models:  models,
		etag:    resp.Header.Get("ETag"),
		fetched: time.Now(),
	})
	return cloneModelInfos(models), nil
}

// Luna is advertised by the account catalog but the Codex Responses backend
// currently rejects it with model_not_found for accounts where Sol and Terra
// work. Keep the restriction at the server boundary so UI selections, agent
// defaults, and app calls all share the same executable model set.
func codexRuntimeModelAllowed(modelID string) bool {
	return !strings.EqualFold(strings.TrimSpace(modelID), "gpt-5.6-luna")
}

func codexDefaultModel(models []ModelInfo, tier string) string {
	preferences := map[string][]string{
		"large":  {"gpt-5.6-terra", "gpt-5.6-sol", "gpt-5.5", "gpt-5.4"},
		"medium": {"gpt-5.6-terra", "gpt-5.6-sol", "gpt-5.5", "gpt-5.4"},
		"small":  {"gpt-5.6-terra", "gpt-5.6-sol", "gpt-5.4-mini", "gpt-5.4", "gpt-5.5"},
	}
	available := make(map[string]bool, len(models))
	for _, model := range models {
		available[model.ID] = true
	}
	for _, candidate := range preferences[tier] {
		if available[candidate] {
			return candidate
		}
	}
	if len(models) > 0 {
		return models[0].ID
	}
	return ""
}

func applyCodexCatalogToState(state map[string]any, models []ModelInfo) {
	if state == nil || len(models) == 0 {
		return
	}
	byID := make(map[string]ModelInfo, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	// Keep the temporary GPT-5.6 rollout conservative: when Terra is
	// available for this account, use it for every role even if Sol was
	// previously selected. Accounts without Terra retain the normal
	// account-aware fallback below.
	_, pinTerra := byID["gpt-5.6-terra"]
	selectedCaps := map[string]ProviderModelCapabilities{}
	for _, tier := range []string{"large", "medium", "small"} {
		key := "model_" + tier
		selected := strings.TrimSpace(stringValue(state[key]))
		if pinTerra {
			selected = "gpt-5.6-terra"
			state[key] = selected
		} else if _, ok := byID[selected]; !ok {
			selected = codexDefaultModel(models, tier)
			state[key] = selected
		}
		if model, ok := byID[selected]; ok {
			selectedCaps[selected] = model.Capabilities
		}
	}
	state["model_capabilities"] = selectedCaps
}

func codexStateHasNonTerra56Selection(state map[string]any) bool {
	for _, tier := range []string{"large", "medium", "small"} {
		model := strings.ToLower(strings.TrimSpace(stringValue(state["model_"+tier])))
		if strings.HasPrefix(model, "gpt-5.6-") && model != "gpt-5.6-terra" {
			return true
		}
	}
	return false
}

func codexAccountIDFromState(state map[string]any) string {
	for _, value := range []string{
		stringFromNested(state, "account", "chatgpt_account_id"),
		stringFromNested(state, "credentials", "account_id"),
		stringFromNested(state, "account", "id"),
	} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
