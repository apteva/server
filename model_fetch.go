package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type ModelInfo struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ContextSize int     `json:"context_size,omitempty"`
	InputCost   float64 `json:"input_cost,omitempty"`  // per 1M tokens
	OutputCost  float64 `json:"output_cost,omitempty"` // per 1M tokens
}

// modelCache stores fetched models with TTL.
type modelCache struct {
	mu      sync.RWMutex
	entries map[string]modelCacheEntry // key: "type:keyprefix"
}

type modelCacheEntry struct {
	models  []ModelInfo
	fetched time.Time
}

var modelCacheTTL = 1 * time.Hour
var globalModelCache = &modelCache{entries: make(map[string]modelCacheEntry)}

// FetchModels returns the model list for a provider, using cache if fresh.
func FetchModels(providerType, apiKey string) ([]ModelInfo, error) {
	cacheKey := providerType + ":" + apiKey[:min(8, len(apiKey))]

	globalModelCache.mu.RLock()
	if entry, ok := globalModelCache.entries[cacheKey]; ok && time.Since(entry.fetched) < modelCacheTTL {
		globalModelCache.mu.RUnlock()
		return entry.models, nil
	}
	globalModelCache.mu.RUnlock()

	var models []ModelInfo
	var err error

	switch providerType {
	case "fireworks":
		models, err = fetchFireworksModels(apiKey)
	case "openai":
		models, err = fetchOpenAIModels(apiKey)
	case "anthropic":
		models, err = fetchAnthropicModels(apiKey)
	case "google":
		models, err = fetchGoogleModels(apiKey)
	default:
		return nil, fmt.Errorf("unknown provider type: %s", providerType)
	}

	if err != nil {
		return nil, err
	}

	// Sort by ID
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })

	// Cache
	globalModelCache.mu.Lock()
	globalModelCache.entries[cacheKey] = modelCacheEntry{models: models, fetched: time.Now()}
	globalModelCache.mu.Unlock()

	return models, nil
}

func apiGet(url string, headers map[string]string) ([]byte, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1000))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

// ── Fireworks ──

func fetchFireworksModels(apiKey string) ([]ModelInfo, error) {
	data, err := apiGet("https://api.fireworks.ai/inference/v1/models", map[string]string{
		"Authorization": "Bearer " + apiKey,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	json.Unmarshal(data, &resp)

	var models []ModelInfo
	for _, m := range resp.Data {
		name := m.ID
		if parts := strings.Split(name, "/"); len(parts) > 1 {
			name = parts[len(parts)-1]
		}
		models = append(models, ModelInfo{ID: m.ID, Name: name})
	}
	return models, nil
}

// ── OpenAI ──

func fetchOpenAIModels(apiKey string) ([]ModelInfo, error) {
	data, err := apiGet("https://api.openai.com/v1/models", map[string]string{
		"Authorization": "Bearer " + apiKey,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	json.Unmarshal(data, &resp)

	var models []ModelInfo
	for _, m := range resp.Data {
		models = append(models, ModelInfo{ID: m.ID, Name: m.ID})
	}
	return models, nil
}

// ── Anthropic ──

func fetchAnthropicModels(apiKey string) ([]ModelInfo, error) {
	data, err := apiGet("https://api.anthropic.com/v1/models?limit=100", map[string]string{
		"x-api-key":         apiKey,
		"anthropic-version": "2023-06-01",
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID             string `json:"id"`
			DisplayName    string `json:"display_name"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
	}
	json.Unmarshal(data, &resp)

	var models []ModelInfo
	for _, m := range resp.Data {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		models = append(models, ModelInfo{
			ID:          m.ID,
			Name:        name,
			ContextSize: m.MaxInputTokens,
		})
	}
	return models, nil
}

// ── Google ──

func fetchGoogleModels(apiKey string) ([]ModelInfo, error) {
	data, err := apiGet("https://generativelanguage.googleapis.com/v1beta/models?key="+apiKey, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Models []struct {
			Name               string `json:"name"`
			DisplayName        string `json:"displayName"`
			InputTokenLimit    int    `json:"inputTokenLimit"`
			SupportedMethods   []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	json.Unmarshal(data, &resp)

	var models []ModelInfo
	for _, m := range resp.Models {
		// Filter to models that support generateContent
		supportsGen := false
		for _, method := range m.SupportedMethods {
			if method == "generateContent" {
				supportsGen = true
				break
			}
		}
		if !supportsGen {
			continue
		}
		id := strings.TrimPrefix(m.Name, "models/")
		models = append(models, ModelInfo{
			ID:          id,
			Name:        m.DisplayName,
			ContextSize: m.InputTokenLimit,
		})
	}
	return models, nil
}

// ── Pricing defaults ──

var defaultPricing = map[string]map[string][2]float64{
	"anthropic": {
		"claude-opus-4-6":         {15.0, 75.0},
		"claude-sonnet-4-6":       {3.0, 15.0},
		"claude-sonnet-4-20250514": {3.0, 15.0},
		"claude-haiku-4-5-20251001": {0.80, 4.0},
	},
	"openai": {
		"gpt-4.1":       {2.0, 8.0},
		"gpt-4.1-mini":  {0.40, 1.60},
		"gpt-4.1-nano":  {0.10, 0.40},
	},
	"fireworks": {
		"accounts/fireworks/models/kimi-k2p5": {0.60, 3.0},
	},
}

// GetModelPricing returns input/output cost per 1M tokens.
func GetModelPricing(providerType, modelID string) (input, output float64) {
	if providerPricing, ok := defaultPricing[providerType]; ok {
		if costs, ok := providerPricing[modelID]; ok {
			return costs[0], costs[1]
		}
	}
	return 0, 0
}
