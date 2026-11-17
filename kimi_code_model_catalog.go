package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var kimiCodeModelsURL = "https://api.kimi.ai/coding/v1/models"

// Membership discovery is authoritative: never substitute a public catalog or
// Moonshot Platform endpoint on an auth, quota or network failure.
func fetchKimiCodeModels(apiKey string) ([]ModelInfo, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("Kimi Code membership API key is required")
	}
	req, err := http.NewRequest(http.MethodGet, kimiCodeModelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid Kimi Code catalog endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "Apteva")
	client := &http.Client{
		Timeout: 15 * time.Second,
		// Do not send a membership credential to a redirected endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Kimi Code model discovery unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		reason := "model discovery failed"
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			reason = "membership key rejected; use a Kimi Code key, not a Moonshot Platform key"
		case http.StatusForbidden:
			reason = "access denied; check membership permissions, plan and quota in the Kimi Code console"
		case http.StatusTooManyRequests:
			reason = "rate or quota limit reached; check the Kimi Code console"
		}
		// Do not echo untrusted error bodies, which can contain credentials.
		retry := ""
		if value := resp.Header.Get("Retry-After"); value != "" {
			if seconds, parseErr := strconv.ParseUint(value, 10, 32); parseErr == nil {
				retry = fmt.Sprintf("; retry after %d seconds", seconds)
			} else if when, parseErr := http.ParseTime(value); parseErr == nil {
				retry = "; retry after " + when.UTC().Format(time.RFC3339)
			}
		}
		return nil, fmt.Errorf("Kimi Code HTTP %d: %s%s", resp.StatusCode, reason, retry)
	}
	const maxCatalogBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil || len(raw) > maxCatalogBytes {
		return nil, fmt.Errorf("Kimi Code model catalog unreadable or too large")
	}
	return parseKimiCodeModels(raw)
}

// Schema follows MoonshotAI/kimi-cli's auth/platforms.py ModelInfo. Context
// length and modalities come from this account's response, never plan guesses.
func parseKimiCodeModels(raw []byte) ([]ModelInfo, error) {
	var catalog struct {
		Data []struct {
			ID                string `json:"id"`
			DisplayName       string `json:"display_name"`
			ContextLength     int    `json:"context_length"`
			SupportsReasoning bool   `json:"supports_reasoning"`
			SupportsImage     bool   `json:"supports_image_in"`
			SupportsVideo     bool   `json:"supports_video_in"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil || catalog.Data == nil {
		return nil, fmt.Errorf("invalid Kimi Code model catalog")
	}
	models := make([]ModelInfo, 0, len(catalog.Data))
	seen := map[string]bool{}
	for _, entry := range catalog.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(entry.DisplayName)
		if name == "" {
			name = id
		}
		context := max(0, entry.ContextLength)
		caps := ProviderModelCapabilities{ContextWindow: context, InputModalities: []string{"text"}}
		if entry.SupportsImage {
			caps.InputModalities = append(caps.InputModalities, "image")
		}
		if entry.SupportsVideo {
			caps.InputModalities = append(caps.InputModalities, "video")
		}
		if entry.SupportsReasoning {
			switch id {
			case "k3.1-pro", "k3.1-pro-highspeed", "kimi-for-coding":
				caps.DefaultReasoningLevel = "max"
				caps.SupportedReasoningLevels = kimiCodeReasoningLevels()
			case "k3", "k3-256k":
				caps.DefaultReasoningLevel = "high"
				caps.SupportedReasoningLevels = kimiCodeReasoningLevels()
				// Highspeed K2.7 does not have adjustable effort. Unknown models
				// need documented capabilities before gaining effort controls.
			}
		}
		models = append(models, ModelInfo{
			ID: id, Name: name, ContextSize: context, Capabilities: caps,
			Methods: []string{"chat_completion"},
			// Subscription usage is not priced per token.
		})
	}
	return models, nil
}

func kimiCodeReasoningLevels() []ModelReasoningLevel {
	return []ModelReasoningLevel{{Effort: "low"}, {Effort: "high"}, {Effort: "max"}}
}
