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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
	"NVIDIA": {
		url:            "https://integrate.api.nvidia.com/v1/models",
		headers:        map[string]string{"Authorization": "Bearer {NVIDIA_API_KEY}"},
		modelCountPath: "data",
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

// handleTestProvider runs the configured probe against the stored
// (encrypted) credentials for one provider. Always returns 200 with
// a ProviderTestResult body — the OK field is the success signal.
// 4xx + body would double-confuse the dashboard's fetch wrapper.
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/providers/"), "/test")
	id, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	provider, encData, err := s.store.GetProvider(userID, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	plain, err := Decrypt(s.secret, encData)
	if err != nil {
		writeJSON(w, ProviderTestResult{
			OK:    false,
			Error: "decrypt credentials: " + err.Error(),
		})
		return
	}
	var data map[string]string
	if err := json.Unmarshal([]byte(plain), &data); err != nil {
		writeJSON(w, ProviderTestResult{
			OK:    false,
			Error: "parse credentials: " + err.Error(),
		})
		return
	}
	writeJSON(w, runProviderHealthCheck(provider.Name, data))
}

// runProviderHealthCheck is the shared probe runner. Used by the
// standalone test endpoint AND the pre-flight gate in
// handleCreateProvider. Returns a fully-populated ProviderTestResult
// — caller writes it back unmodified.
func runProviderHealthCheck(providerName string, data map[string]string) ProviderTestResult {
	probe, ok := providerProbes[providerName]
	if !ok {
		return ProviderTestResult{
			OK:      true,
			Skipped: true,
			Reason:  "no health probe defined for " + providerName,
		}
	}

	// Templating: expand every {KEY} substring in url, headers, body.
	// Missing values get an empty replacement so a partially-filled
	// form still produces a meaningful upstream error (rather than
	// firing a literal "{API_KEY}" header).
	url := expandTemplate(probe.url, data)
	method := probe.method
	if method == "" {
		method = http.MethodGet
	}

	// Pre-flight URL sanity — catches the OLLAMA_HOST="" /
	// BROWSER_API_URL="" case without a wasted network round-trip.
	if strings.HasPrefix(url, "/") || strings.Contains(url, "{") || url == "" {
		return ProviderTestResult{
			OK:    false,
			Error: "missing or invalid URL — check that all required fields are filled",
		}
	}

	bodyReader := io.Reader(nil)
	if probe.body != "" {
		bodyReader = bytes.NewReader([]byte(probe.body))
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return ProviderTestResult{
			OK:    false,
			Error: "build request: " + err.Error(),
		}
	}
	for k, v := range probe.headers {
		expanded := expandTemplate(v, data)
		// Bare "Bearer " with no key after means the operator left
		// the field empty — fail fast with a useful message rather
		// than letting the upstream return its own opaque 401.
		if strings.HasSuffix(strings.TrimSpace(expanded), "Bearer") || expanded == "" {
			return ProviderTestResult{
				OK:    false,
				Error: fmt.Sprintf("header %q has empty value — fill in the API key", k),
			}
		}
		req.Header.Set(k, expanded)
	}
	if probe.body != "" {
		ct := probe.contentType
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	t0 := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(t0).Milliseconds()
	if err != nil {
		// Network failure — distinguish from upstream auth failure so
		// the operator knows to check connectivity vs the key itself.
		return ProviderTestResult{
			OK:        false,
			LatencyMS: latency,
			Error:     fmt.Sprintf("request failed: %v", err),
		}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	expected := probe.expectStatus
	if len(expected) == 0 {
		expected = []int{200}
	}
	if !containsInt(expected, resp.StatusCode) {
		return ProviderTestResult{
			OK:         false,
			LatencyMS:  latency,
			StatusCode: resp.StatusCode,
			Error:      fmt.Sprintf("HTTP %d: %s", resp.StatusCode, summarizeUpstreamError(body)),
		}
	}

	out := ProviderTestResult{
		OK:         true,
		LatencyMS:  latency,
		StatusCode: resp.StatusCode,
	}
	if probe.modelCountPath != "" {
		if n, err := countArrayAtPath(body, probe.modelCountPath); err == nil {
			out.ModelCount = n
		}
	}
	return out
}

// expandTemplate replaces every {KEY} placeholder in s with
// data[KEY]. Missing keys collapse to "" so a partially-filled form
// still produces a useful upstream error from the runner above
// (which checks for empty Authorization values explicitly).
func expandTemplate(s string, data map[string]string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				b.WriteByte(s[i])
				i++
				continue
			}
			key := s[i+1 : i+end]
			b.WriteString(data[key])
			i += end + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// countArrayAtPath parses body as JSON, navigates to path, and
// returns len(array). Returns 0 + nil error when the path is absent
// (the probe still passed, we just can't quote a count). Path is a
// single top-level key today; nested addressing isn't needed for
// any current provider.
func countArrayAtPath(body []byte, path string) (int, error) {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return 0, err
	}
	arr, ok := v[path].([]any)
	if !ok {
		return 0, nil
	}
	return len(arr), nil
}

func containsInt(haystack []int, needle int) bool {
	for _, x := range haystack {
		if x == needle {
			return true
		}
	}
	return false
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
