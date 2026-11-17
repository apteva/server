package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	grokBuildModelCatalogBaseURL  = integrationGrokBuildRuntimeURL
	grokBuildModelCatalogClient   = &http.Client{Timeout: 15 * time.Second}
	grokBuildModelCatalogFreshTTL = 15 * time.Minute
)

type grokBuildCatalogCacheEntry struct {
	models  []ModelInfo
	etag    string
	fetched time.Time
}

type grokBuildCatalogCacheStore struct {
	mu      sync.RWMutex
	entries map[string]grokBuildCatalogCacheEntry
	locks   sync.Map
}

var globalGrokBuildCatalogCache = &grokBuildCatalogCacheStore{entries: map[string]grokBuildCatalogCacheEntry{}}

func (cache *grokBuildCatalogCacheStore) entry(key string) (grokBuildCatalogCacheEntry, bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	entry, ok := cache.entries[key]
	if ok {
		entry.models = cloneModelInfos(entry.models)
	}
	return entry, ok
}

func (cache *grokBuildCatalogCacheStore) put(key string, entry grokBuildCatalogCacheEntry) {
	entry.models = cloneModelInfos(entry.models)
	cache.mu.Lock()
	cache.entries[key] = entry
	cache.mu.Unlock()
}

func (cache *grokBuildCatalogCacheStore) lockFor(key string) *sync.Mutex {
	value, _ := cache.locks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func fetchGrokBuildModelCatalog(ctx context.Context, credentials map[string]string, force bool) ([]ModelInfo, error) {
	accessToken := strings.TrimSpace(credentials["access_token"])
	if accessToken == "" {
		return nil, fmt.Errorf("Grok Build auth is missing access_token")
	}
	identity := strings.TrimSpace(credentials["principal_id"])
	if identity == "" {
		identity = strings.TrimSpace(credentials["user_id"])
	}
	sum := sha256.Sum256([]byte(grokBuildModelCatalogBaseURL + "\x00" + identity + "\x00" + accessToken))
	cacheKey := hex.EncodeToString(sum[:])
	if entry, ok := globalGrokBuildCatalogCache.entry(cacheKey); ok && !force && time.Since(entry.fetched) < grokBuildModelCatalogFreshTTL {
		return entry.models, nil
	}

	lock := globalGrokBuildCatalogCache.lockFor(cacheKey)
	lock.Lock()
	defer lock.Unlock()
	entry, hasEntry := globalGrokBuildCatalogCache.entry(cacheKey)
	if hasEntry && !force && time.Since(entry.fetched) < grokBuildModelCatalogFreshTTL {
		return entry.models, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(grokBuildModelCatalogBaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	applyGrokBuildSessionHeaders(req, credentials)
	if hasEntry && entry.etag != "" {
		req.Header.Set("If-None-Match", entry.etag)
	}
	resp, err := grokBuildModelCatalogClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && hasEntry {
		entry.fetched = time.Now()
		globalGrokBuildCatalogCache.put(cacheKey, entry)
		return entry.models, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("Grok Build models HTTP %d: %s", resp.StatusCode, summarizeUpstreamError(body))
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Grok Build models: %w", err)
	}
	models := parseGrokBuildModels(payload.Data)
	if len(models) == 0 {
		return nil, fmt.Errorf("Grok Build model catalog contained no usable models")
	}
	globalGrokBuildCatalogCache.put(cacheKey, grokBuildCatalogCacheEntry{
		models: models, etag: resp.Header.Get("ETag"), fetched: time.Now(),
	})
	return cloneModelInfos(models), nil
}

func applyGrokBuildSessionHeaders(req *http.Request, credentials map[string]string) {
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(credentials["access_token"]))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", grokBuildClientVersion)
	req.Header.Set("x-grok-client-identifier", "apteva-server")
	req.Header.Set("x-grok-client-mode", "headless")
	req.Header.Set("x-authenticateresponse", "authenticate-response")
	userID := strings.TrimSpace(credentials["user_id"])
	if userID == "" {
		userID = strings.TrimSpace(credentials["account_id"])
	}
	if userID != "" {
		req.Header.Set("x-userid", userID)
	}
	if email := strings.TrimSpace(credentials["account_email"]); email != "" {
		req.Header.Set("x-email", email)
	}
}

func parseGrokBuildModels(entries []map[string]any) []ModelInfo {
	models := make([]ModelInfo, 0, len(entries))
	for index, entry := range entries {
		meta, _ := entry["_meta"].(map[string]any)
		id := firstGrokBuildString(entry, meta, "model", "modelId", "id")
		if id == "" || grokBuildBool(entry, meta, "hidden") {
			continue
		}
		supported := grokBuildOptionalBool(entry, meta, "supportedInApi", "supported_in_api")
		if supported != nil && !*supported {
			continue
		}
		name := firstGrokBuildString(entry, meta, "name")
		if name == "" {
			name = id
		}
		contextWindow := firstGrokBuildInt(entry, meta, "contextWindow", "context_window", "totalContextTokens")
		inputModalities := firstGrokBuildStrings(entry, meta, "inputModalities", "input_modalities")
		levels := firstGrokBuildReasoningLevels(entry, meta, "reasoningEfforts", "reasoning_efforts")
		models = append(models, ModelInfo{
			ID:           id,
			Name:         name,
			Description:  firstGrokBuildString(entry, meta, "description"),
			ContextSize:  contextWindow,
			Priority:     index,
			SupportedAPI: supported,
			Capabilities: ProviderModelCapabilities{
				ContextWindow:            contextWindow,
				MaxContextWindow:         contextWindow,
				DefaultReasoningLevel:    firstGrokBuildString(entry, meta, "reasoningEffort", "reasoning_effort"),
				SupportedReasoningLevels: levels,
				InputModalities:          inputModalities,
			},
		})
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Priority != models[j].Priority {
			return models[i].Priority < models[j].Priority
		}
		return models[i].ID < models[j].ID
	})
	return models
}

func applyGrokBuildCatalogToState(state map[string]any, models []ModelInfo) {
	if state == nil || len(models) == 0 {
		return
	}
	byID := make(map[string]ModelInfo, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	defaultModel := ""
	for _, candidate := range []string{"grok-4.6", "grok-4.5"} {
		if _, ok := byID[candidate]; ok {
			defaultModel = candidate
			break
		}
	}
	if defaultModel == "" {
		defaultModel = models[0].ID
	}
	selectedCapabilities := map[string]ProviderModelCapabilities{}
	for _, tier := range []string{"large", "medium", "small"} {
		key := "model_" + tier
		selected := strings.TrimSpace(stringValue(state[key]))
		if _, ok := byID[selected]; !ok {
			selected = defaultModel
			state[key] = selected
		}
		if model, ok := byID[selected]; ok {
			selectedCapabilities[selected] = model.Capabilities
		}
	}
	state["model_capabilities"] = selectedCapabilities
}

func firstGrokBuildString(entry, meta map[string]any, keys ...string) string {
	for _, values := range []map[string]any{entry, meta} {
		for _, key := range keys {
			if value := strings.TrimSpace(stringValue(values[key])); value != "" && value != "<nil>" {
				return value
			}
		}
	}
	return ""
}

func firstGrokBuildInt(entry, meta map[string]any, keys ...string) int {
	for _, values := range []map[string]any{entry, meta} {
		for _, key := range keys {
			if value := loosePositiveInt64(values[key]); value > 0 {
				return int(value)
			}
		}
	}
	return 0
}

func grokBuildOptionalBool(entry, meta map[string]any, keys ...string) *bool {
	for _, values := range []map[string]any{entry, meta} {
		for _, key := range keys {
			if value, ok := values[key].(bool); ok {
				copy := value
				return &copy
			}
		}
	}
	return nil
}

func grokBuildBool(entry, meta map[string]any, keys ...string) bool {
	value := grokBuildOptionalBool(entry, meta, keys...)
	return value != nil && *value
}

func firstGrokBuildStrings(entry, meta map[string]any, keys ...string) []string {
	for _, values := range []map[string]any{entry, meta} {
		for _, key := range keys {
			raw, ok := values[key].([]any)
			if !ok {
				continue
			}
			out := make([]string, 0, len(raw))
			for _, item := range raw {
				if value := strings.TrimSpace(stringValue(item)); value != "" {
					out = append(out, value)
				}
			}
			return out
		}
	}
	return nil
}

func firstGrokBuildReasoningLevels(entry, meta map[string]any, keys ...string) []ModelReasoningLevel {
	for _, values := range []map[string]any{entry, meta} {
		for _, key := range keys {
			raw, ok := values[key].([]any)
			if !ok {
				continue
			}
			levels := make([]ModelReasoningLevel, 0, len(raw))
			for _, item := range raw {
				effort := ""
				description := ""
				switch typed := item.(type) {
				case string:
					effort = strings.TrimSpace(typed)
				case map[string]any:
					effort = strings.TrimSpace(stringValue(typed["effort"]))
					description = strings.TrimSpace(stringValue(typed["description"]))
				}
				if effort != "" {
					levels = append(levels, ModelReasoningLevel{Effort: effort, Description: description})
				}
			}
			return levels
		}
	}
	return nil
}
