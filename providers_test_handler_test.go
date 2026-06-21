package main

// providers_test_handler_test.go — pins the probe runner behaviour
// without going over the wire to any real upstream:
//
//   * happy path: 200 + JSON body with `data` array → ModelCount.
//   * 401 from upstream: OK=false + status_code 401 + error string
//     pulled from {"error":{"message":...}}.
//   * empty Authorization header (operator left field blank) →
//     OK=false, no network round-trip.
//   * provider name not in the probe table → Skipped=true.
//   * no-creds provider name (Apteva Local) → Skipped=true.
//
// The runner is tested by hijacking the probe table to point at an
// httptest server. Probe URLs reference the test server's URL via a
// templating key ({TEST_BASE_URL}) so the runner exercises the same
// substitution code path the production probes use.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withTestProbe swaps in a synthetic probe pointing at `srv.URL`,
// returns a cleanup that restores the original entry. Lets a test
// hand-craft the probe shape for the case it cares about without
// touching the production table.
func withTestProbe(t *testing.T, name string, p providerProbe) func() {
	t.Helper()
	orig, existed := providerProbes[name]
	providerProbes[name] = p
	return func() {
		if existed {
			providerProbes[name] = orig
		} else {
			delete(providerProbes, name)
		}
	}
}

func TestProviderHealthCheck_HappyPathParsesModelCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-good-key" {
			http.Error(w, "wrong auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`))
	}))
	defer srv.Close()

	defer withTestProbe(t, "FakeOpenAI", providerProbe{
		url:            srv.URL + "/v1/models",
		headers:        map[string]string{"Authorization": "Bearer {API_KEY}"},
		modelCountPath: "data",
	})()

	res := runProviderHealthCheck("FakeOpenAI", map[string]string{"API_KEY": "sk-good-key"})
	if !res.OK {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if res.StatusCode != 200 {
		t.Errorf("status_code=%d, want 200", res.StatusCode)
	}
	if res.ModelCount != 3 {
		t.Errorf("model_count=%d, want 3", res.ModelCount)
	}
}

func TestProviderHealthCheck_GoogleQueryKeyProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "google-good-key" {
			http.Error(w, "missing key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-flash"},{"name":"models/gemini-2.5-pro"}]}`))
	}))
	defer srv.Close()

	defer withTestProbe(t, "FakeGoogle", providerProbe{
		url:            srv.URL + "/v1beta/models?key={GOOGLE_API_KEY}",
		modelCountPath: "models",
	})()

	res := runProviderHealthCheck("FakeGoogle", map[string]string{"GOOGLE_API_KEY": "google-good-key"})
	if !res.OK {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if res.ModelCount != 2 {
		t.Errorf("model_count=%d, want 2", res.ModelCount)
	}
}

func TestProviderHealthCheck_UpstreamAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	defer withTestProbe(t, "FakeOpenAI", providerProbe{
		url:     srv.URL + "/v1/models",
		headers: map[string]string{"Authorization": "Bearer {API_KEY}"},
	})()

	res := runProviderHealthCheck("FakeOpenAI", map[string]string{"API_KEY": "sk-wrong"})
	if res.OK {
		t.Fatalf("expected ok=false for 401, got %+v", res)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status_code=%d", res.StatusCode)
	}
	if !strings.Contains(res.Error, "Invalid API key") {
		t.Errorf("error=%q, want to mention 'Invalid API key'", res.Error)
	}
}

func TestProviderHealthCheck_MissingAPIKeyShortCircuits(t *testing.T) {
	// An empty {API_KEY} substitution collapses to "Bearer " — the
	// runner catches this before firing a request, so no httptest
	// server is needed.
	defer withTestProbe(t, "FakeOpenAI", providerProbe{
		url:     "https://api.invalid.example/v1/models",
		headers: map[string]string{"Authorization": "Bearer {API_KEY}"},
	})()

	res := runProviderHealthCheck("FakeOpenAI", map[string]string{}) // no API_KEY
	if res.OK {
		t.Fatalf("expected ok=false for empty key, got %+v", res)
	}
	if !strings.Contains(res.Error, "empty") && !strings.Contains(res.Error, "fill in") {
		t.Errorf("error=%q should hint at empty/missing key", res.Error)
	}
	if res.LatencyMS != 0 {
		t.Errorf("expected no round-trip (latency=0), got %dms", res.LatencyMS)
	}
}

func TestProviderHealthCheck_UnknownProviderSkipped(t *testing.T) {
	res := runProviderHealthCheck("NoSuchProvider", map[string]string{"X": "y"})
	if !res.Skipped {
		t.Errorf("expected skipped=true for unknown provider, got %+v", res)
	}
	if !res.OK {
		t.Errorf("unknown provider should ok=true (no probe ran)")
	}
}

func TestProviderHealthCheck_AptevaLocalIsSkipped(t *testing.T) {
	// Apteva Local intentionally has no probe — confirms the table
	// entry stays absent rather than getting a synthetic stub later.
	res := runProviderHealthCheck("Apteva Local", map[string]string{})
	if !res.Skipped {
		t.Errorf("Apteva Local should skip (no creds), got %+v", res)
	}
}

func TestProviderHealthCheck_BadTemplateURLFailsFast(t *testing.T) {
	// A probe whose URL relies on {OLLAMA_HOST} but the operator
	// hasn't filled it in → template expands to "/api/tags" which
	// the runner rejects before hitting any network.
	defer withTestProbe(t, "FakeOllama", providerProbe{
		url: "{OLLAMA_HOST}/api/tags",
	})()
	res := runProviderHealthCheck("FakeOllama", map[string]string{})
	if res.OK {
		t.Fatalf("expected ok=false for missing OLLAMA_HOST, got %+v", res)
	}
	if !strings.Contains(res.Error, "invalid URL") && !strings.Contains(res.Error, "missing") {
		t.Errorf("error=%q should mention missing/invalid URL", res.Error)
	}
}

func TestSummarizeUpstreamError_PrefersStructured(t *testing.T) {
	// OpenAI-style.
	if got := summarizeUpstreamError([]byte(`{"error":{"message":"bad key"}}`)); got != "bad key" {
		t.Errorf("openai shape: got %q", got)
	}
	// FastAPI / Steel / Composio shape.
	if got := summarizeUpstreamError([]byte(`{"detail":"unauthorized"}`)); got != "unauthorized" {
		t.Errorf("detail shape: got %q", got)
	}
	// Truncates verbose HTML.
	long := strings.Repeat("x", 500)
	if got := summarizeUpstreamError([]byte(long)); !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation suffix, got %q", got[len(got)-10:])
	}
}
