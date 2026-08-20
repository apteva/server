package main

// providers_test_handler.go — `POST /api/providers/:id/test` and the
// shared health-check runner for LLM / embeddings / TTS / browser /
// integrations providers seeded inline in store.go.
//
// Same shape as connections_test_handler.go:
//
//   * Pre-flight on create (handleCreateProvider) — refuses to save
//     credentials we can't authenticate against the upstream.
//   * Standalone POST /providers/:id/test — Test button in the
//     dashboard's Settings page.
//   * Skipped result when the provider type has no credentials
//     (Apteva Local) or no health probe is defined.
//
// Probe table is hand-written below (one row per seeded provider
// name) because providers don't have a per-app JSON catalog the way
// connections do. Adding a new provider type means: seed it in
// store.go AND add a row here. The table lives next to the runner so
// "is this provider testable" is one grep away.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// ProviderTestResult mirrors ConnectionTestResult so the dashboard
// can render either shape with one component.
type ProviderTestResult struct {
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped,omitempty"`
	Reason     string `json:"reason,omitempty"` // why skipped
	LatencyMS  int64  `json:"latency_ms"`
	StatusCode int    `json:"status_code,omitempty"` // upstream HTTP status
	Error      string `json:"error,omitempty"`       // human-readable failure
	// ModelCount is optional success detail — the dashboard renders it
	// as "12 models available" on a working OpenAI/Anthropic/Fireworks
	// key so the operator gets confirmation the probe actually saw the
	// upstream's catalog, not a cached cdn 200.
	ModelCount        int    `json:"model_count,omitempty"`
	Model             string `json:"model,omitempty"`
	ResponseText      string `json:"response_text,omitempty"`
	PromptTokens      int    `json:"prompt_tokens,omitempty"`
	CompletionTokens  int    `json:"completion_tokens,omitempty"`
	CachedTokens      int    `json:"cached_tokens,omitempty"`
	ToolCallCount     int    `json:"tool_call_count,omitempty"`
	ToolName          string `json:"tool_name,omitempty"`
	ToolArguments     string `json:"tool_arguments,omitempty"`
	ComputerCallCount int    `json:"computer_call_count,omitempty"`
	ComputerAction    string `json:"computer_action,omitempty"`
	VisionText        string `json:"vision_text,omitempty"`
}

// providerProbe captures everything needed to issue a single auth
// probe against an upstream. URL + headers are Go-template-style
// {KEY} substitutions; the runner expands them from the plaintext
// credentials map before the request fires.
type providerProbe struct {
	// Method defaults to GET when empty.
	method string
	// URL is the upstream endpoint. {KEY} placeholders expand from
	// the provider's plaintext credentials data (e.g. {OLLAMA_HOST}).
	url string
	// Headers — same {KEY} substitution. Authorization-style headers
	// go here.
	headers map[string]string
	// Body is the raw POST body for probes that need one (Voyage's
	// embeddings endpoint, mostly). Empty for GET probes.
	body string
	// ContentType for the body. Defaults to application/json.
	contentType string
	// ExpectStatus — 200 by default. Some providers return 200 for
	// every shape; others (e.g. Anthropic) return 401 with a JSON
	// error body that the runner surfaces verbatim.
	expectStatus []int
	// ModelCountPath — JSON-path-ish pointer into the response body
	// that yields a count or array. We do this with two patterns:
	//   "data" → expect a JSON object with a "data" array; report
	//     len(data). OpenAI, Fireworks, NVIDIA, Anthropic /v1/models
	//     all follow this shape.
	//   "voices" → ElevenLabs.
	//   "" → don't extract a count.
	modelCountPath string
}

// providerProbes is keyed by the provider_types.name column.
//
// Entries pinned to specific provider name strings (not type slugs)
// because store.go seeds rows by name as the operator-visible label.
// If a row is renamed, add the new name here AND keep the old one
// pointing at the same probe so existing installs don't 404 on test.
var providerProbes = map[string]providerProbe{
	// LLM providers — every one of these exposes /v1/models (or
	// equivalent) under the same bearer-style auth their inference
	// endpoint accepts. A 200 list confirms both that the key is
	// valid AND that the operator has at least basic-tier access.
	"OpenAI": {
		url:            "https://api.openai.com/v1/models",
		headers:        map[string]string{"Authorization": "Bearer {OPENAI_API_KEY}"},
		modelCountPath: "data",
	},
	"Anthropic": {
		url: "https://api.anthropic.com/v1/models",
		headers: map[string]string{
			"x-api-key":         "{ANTHROPIC_API_KEY}",
			"anthropic-version": "2023-06-01",
		},
		modelCountPath: "data",
	},
	"Fireworks": {
		url:            "https://api.fireworks.ai/inference/v1/models",
		headers:        map[string]string{"Authorization": "Bearer {FIREWORKS_API_KEY}"},
		modelCountPath: "data",
	},
	"Google": {
		url:            "https://generativelanguage.googleapis.com/v1beta/models?key={GOOGLE_API_KEY}",
		modelCountPath: "models",
	},
	"NVIDIA": {
		url:            "https://integrate.api.nvidia.com/v1/models",
		headers:        map[string]string{"Authorization": "Bearer {NVIDIA_API_KEY}"},
		modelCountPath: "data",
	},
	"xAI": {
		url:            "https://api.x.ai/v1/language-models",
		headers:        map[string]string{"Authorization": "Bearer {XAI_API_KEY}"},
		modelCountPath: "models",
	},
	"Ollama": {
		// Local-only; the operator points OLLAMA_HOST at their
		// daemon. No auth header. /api/tags lists pulled models.
		url:            "{OLLAMA_HOST}/api/tags",
		modelCountPath: "models",
	},

	// Embeddings.
	"Voyage": {
		// No /models endpoint; cheapest probe is a tiny embed call
		// against a token-efficient model. ~1 token, costs roughly
		// nothing, validates auth + quota.
		method:         http.MethodPost,
		url:            "https://api.voyageai.com/v1/embeddings",
		headers:        map[string]string{"Authorization": "Bearer {VOYAGE_API_KEY}"},
		body:           `{"input":"x","model":"voyage-3"}`,
		modelCountPath: "",
	},

	// TTS.
	"ElevenLabs": {
		url:            "https://api.elevenlabs.io/v1/voices",
		headers:        map[string]string{"xi-api-key": "{ELEVENLABS_API_KEY}"},
		modelCountPath: "voices",
	},

	// Browser automation.
	"Browserbase": {
		url:            "https://api.browserbase.com/v1/projects",
		headers:        map[string]string{"x-bb-api-key": "{BROWSERBASE_API_KEY}"},
		modelCountPath: "",
	},
	"Steel": {
		url:            "https://api.steel.dev/v1/sessions?limit=1",
		headers:        map[string]string{"Authorization": "Bearer {STEEL_API_KEY}"},
		modelCountPath: "",
	},
	"Browser Engine": {
		// Self-hosted variant — URL comes from BROWSER_API_URL.
		url:            "{BROWSER_API_URL}/health",
		headers:        map[string]string{"Authorization": "Bearer {BROWSER_API_KEY}"},
		modelCountPath: "",
	},

	// Integrations.
	"Composio": {
		url:            "https://backend.composio.dev/api/v1/apps",
		headers:        map[string]string{"x-api-key": "{COMPOSIO_API_KEY}"},
		modelCountPath: "",
	},
	// "Apteva Local" intentionally has no probe — providers without
	// credentials skip the gate with reason="no credentials to test".
}

// summarizeUpstreamError extracts a one-line operator-readable error
// string from an upstream response body. Most LLM providers return
// {"error":{"message":"...","type":"..."}} on auth failures; pick
// that out when present, fall through to a truncated raw body
// otherwise. 240-char cap matches connections_test_handler's
// previewBody so the dashboard's two error rows look identical.
func summarizeUpstreamError(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// Common shape A: {"error": {"message": "..."}}
	var a struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &a); err == nil && a.Error.Message != "" {
		return a.Error.Message
	}
	// Common shape B: {"detail": "..."} (Composio, Steel)
	var b struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &b); err == nil && b.Detail != "" {
		return b.Detail
	}
	// Fallback: raw body, single-line, truncated.
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// ErrProviderPreflightFailed is returned by the pre-flight check in
// handleCreateProvider when the probe came back negative. The
// handler maps it to a 400 with the ProviderTestResult body so the
// dashboard can render the upstream's error inline.
var ErrProviderPreflightFailed = errors.New("provider health check failed")
