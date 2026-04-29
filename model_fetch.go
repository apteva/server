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
	case "nvidia":
		models, err = fetchOpenAICompatModels("https://integrate.api.nvidia.com/v1/models", apiKey)
	case "opencode-go":
		// OpenCode Go's plan exposes a fixed curated list — there's no
		// /v1/models endpoint to discover from. Return the published
		// catalog directly so the dashboard's model picker has options.
		models = openCodeGoModels()
	case "venice":
		// Venice's /api/v1/models returns rich metadata (display name,
		// context_length, capabilities). Use the dedicated fetcher so
		// the picker can show "GLM 5.1 (200K)" rather than a bare slug.
		models, err = fetchVeniceModels(apiKey)
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

// openCodeGoModels returns the fixed curated catalog OpenCode Go's
// flat-rate plan exposes. There is no /v1/models endpoint to query
// (the plan defines the catalog), so we hardcode it from
// https://opencode.ai/docs/go/. Update this list when OpenCode Go's
// docs change.
//
// All prices are zero — OpenCode Go is subscription-priced, not
// per-token, so per-call $ figures don't apply. The dashboard's model
// picker still gets the IDs it needs.
func openCodeGoModels() []ModelInfo {
	return []ModelInfo{
		// OpenAI-compat endpoint variants
		{ID: "kimi-k2.6", Name: "Kimi K2.6", ContextSize: 256_000},
		{ID: "kimi-k2.5", Name: "Kimi K2.5", ContextSize: 256_000},
		{ID: "qwen3.6-plus", Name: "Qwen3.6 Plus", ContextSize: 128_000},
		{ID: "qwen3.5-plus", Name: "Qwen3.5 Plus", ContextSize: 128_000},
		{ID: "glm-5.1", Name: "GLM-5.1", ContextSize: 128_000},
		{ID: "glm-5", Name: "GLM-5", ContextSize: 128_000},
		{ID: "mimo-v2.5-pro", Name: "MiMo V2.5 Pro", ContextSize: 256_000},
		{ID: "mimo-v2.5", Name: "MiMo V2.5", ContextSize: 256_000},
		{ID: "mimo-v2-pro", Name: "MiMo V2 Pro", ContextSize: 256_000},
		{ID: "mimo-v2-omni", Name: "MiMo V2 Omni", ContextSize: 256_000},
		// Anthropic-style endpoint variants — exposed in the picker
		// for completeness; using them requires routing the request
		// through provider_anthropic.go's path (separate work to wire).
		{ID: "minimax-m2.7", Name: "MiniMax M2.7", ContextSize: 196_608},
		{ID: "minimax-m2.5", Name: "MiniMax M2.5", ContextSize: 196_608},
		{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextSize: 128_000},
		{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextSize: 128_000},
	}
}

// fetchFireworksModels uses the native /v1/accounts/fireworks/models
// endpoint instead of /inference/v1/models. The OpenAI-compat endpoint
// only returns ~11 curated entries with no pagination; the native one
// paginates through the full catalog (200+ entries, including newer
// models like kimi-k2p6 that the compat endpoint omits).
func fetchFireworksModels(apiKey string) ([]ModelInfo, error) {
	var models []ModelInfo
	seen := map[string]bool{}
	pageToken := ""
	for page := 0; page < 20; page++ { // hard cap defensively; real catalog is ~1 page
		url := "https://api.fireworks.ai/v1/accounts/fireworks/models?pageSize=200"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}
		data, err := apiGet(url, map[string]string{"Authorization": "Bearer " + apiKey})
		if err != nil {
			return nil, err
		}
		var resp struct {
			Models []struct {
				Name          string `json:"name"`
				DisplayName   string `json:"displayName"`
				State         string `json:"state"`
				ContextLength int    `json:"contextLength"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Models {
			if m.State != "" && m.State != "READY" {
				continue
			}
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			display := m.DisplayName
			if display == "" {
				display = m.Name
				if parts := strings.Split(display, "/"); len(parts) > 1 {
					display = parts[len(parts)-1]
				}
			}
			models = append(models, ModelInfo{
				ID:          m.Name,
				Name:        display,
				ContextSize: m.ContextLength,
			})
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
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

// fetchOpenAICompatModels works for any provider with an OpenAI-compatible /models endpoint.
func fetchOpenAICompatModels(url, apiKey string) ([]ModelInfo, error) {
	data, err := apiGet(url, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
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

// ── Venice ──

// fetchVeniceModels queries Venice's /api/v1/models. The response is
// OpenAI-shaped at the top level (`data: [{id, ...}]`) but each entry
// carries a `model_spec` block with display name, context length, and
// capability flags. We surface the friendly display name so the picker
// reads "GLM 5.1" instead of "zai-org-glm-5-1", and limit to text
// models (Venice ships TTS, image, embeddings on the same endpoint).
func fetchVeniceModels(apiKey string) ([]ModelInfo, error) {
	data, err := apiGet("https://api.venice.ai/api/v1/models", map[string]string{
		"Authorization": "Bearer " + apiKey,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID            string `json:"id"`
			Type          string `json:"type"`
			ContextLength int    `json:"context_length"`
			ModelSpec     struct {
				Name    string `json:"name"`
				Pricing struct {
					Input struct {
						USD float64 `json:"usd"`
					} `json:"input"`
					Output struct {
						USD float64 `json:"usd"`
					} `json:"output"`
				} `json:"pricing"`
			} `json:"model_spec"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var models []ModelInfo
	for _, m := range resp.Data {
		if m.Type != "" && m.Type != "text" {
			continue
		}
		name := m.ModelSpec.Name
		if name == "" {
			name = m.ID
		}
		models = append(models, ModelInfo{
			ID:          m.ID,
			Name:        name,
			ContextSize: m.ContextLength,
			InputCost:   m.ModelSpec.Pricing.Input.USD,
			OutputCost:  m.ModelSpec.Pricing.Output.USD,
		})
	}
	return models, nil
}

// ── Pricing defaults ──

// modelPricing is input/cached/output per 1M tokens for a single model.
// Cached rate falls back to input when the model doesn't publish one
// (look up returns input/10 as a reasonable default only if nothing is
// stored — explicit entries win).
type modelPricing struct {
	input  float64
	cached float64
	output float64
}

// Canonical pricing table keyed by the model string we receive in
// llm.done telemetry. Rates are USD per 1M tokens for
// (uncached input, cached input, output). Adding a model anywhere else
// in the stack is a no-op for cost — this is the single source of truth.
//
// Provider pricing sources (verified via web search, April 2026):
//   - Fireworks:  https://fireworks.ai/pricing
//   - Anthropic:  https://platform.claude.com/docs/en/about-claude/pricing
//   - OpenAI:     https://openai.com/api/pricing/
// Bump the comment date when you refresh the table against the vendors.
var modelPricingTable = map[string]modelPricing{
	// Anthropic (https://platform.claude.com/docs/en/about-claude/pricing)
	"claude-opus-4-7":           {5.0, 0.5, 25.0},
	"claude-opus-4-6":           {5.0, 0.5, 25.0},
	"claude-sonnet-4-6":         {3.0, 0.3, 15.0},
	"claude-sonnet-4-20250514":  {3.0, 0.3, 15.0},
	"claude-haiku-4-5-20251001": {1.0, 0.10, 5.0},

	// OpenAI (https://openai.com/api/pricing/)
	"gpt-5.4":      {2.50, 0.25, 15.00},
	"gpt-5.4-mini": {0.75, 0.075, 4.50},
	"gpt-4.1":      {2.0, 0.5, 8.0},
	"gpt-4.1-mini": {0.40, 0.10, 1.60},
	"gpt-4.1-nano": {0.10, 0.025, 0.40},

	// Fireworks (https://fireworks.ai/pricing)
	"accounts/fireworks/models/kimi-k2p6":        {0.95, 0.16, 4.00},
	"accounts/fireworks/models/kimi-k2p5":        {0.60, 0.10, 3.00},
	"accounts/fireworks/routers/kimi-k2p5-turbo": {0.99, 0.16, 4.94},
	"accounts/fireworks/models/minimax-m2p7":     {0.30, 0.06, 1.20},
	"accounts/fireworks/models/minimax-m2p5":     {0.30, 0.03, 1.20},

	// OpenCode Go (https://opencode.ai/docs/go/) — flat-rate
	// subscription, NOT per-token. Costs left at 0/0/0 so the
	// dashboard's per-call $ figure stays blank for these requests
	// rather than pretending each call has a tiny per-token cost
	// (the real cost is the monthly subscription divided by usage).
	// If we later want to surface "% of monthly cap consumed" we'd
	// add a separate gauge keyed by provider name, not pricing.
	"kimi-k2.6":         {0, 0, 0},
	"kimi-k2.5":         {0, 0, 0},
	"qwen3.6-plus":      {0, 0, 0},
	"qwen3.5-plus":      {0, 0, 0},
	"glm-5.1":           {0, 0, 0},
	"glm-5":             {0, 0, 0},
	"mimo-v2.5-pro":     {0, 0, 0},
	"mimo-v2.5":         {0, 0, 0},
	"mimo-v2-pro":       {0, 0, 0},
	"mimo-v2-omni":      {0, 0, 0},
	"minimax-m2.7":      {0, 0, 0},
	"minimax-m2.5":      {0, 0, 0},
	"deepseek-v4-pro":   {0, 0, 0},
	"deepseek-v4-flash": {0, 0, 0},
}

// LookupModelPricing returns the per-1M pricing for a model, derived
// entirely from the event-supplied model string. Callers receive the
// three rates + ok=true when the model is known; ok=false means we
// can't price the event and it should be left un-enriched.
func LookupModelPricing(model string) (input, cached, output float64, ok bool) {
	if p, found := modelPricingTable[model]; found {
		return p.input, p.cached, p.output, true
	}
	return 0, 0, 0, false
}

// Legacy helper kept for the analytics panel that still queries by
// (providerType, model). Prefer LookupModelPricing for new code — it
// doesn't need the provider hint.
func GetModelPricing(_, modelID string) (input, output float64) {
	if p, ok := modelPricingTable[modelID]; ok {
		return p.input, p.output
	}
	return 0, 0
}
