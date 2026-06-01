package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// proxiedClient returns an http.Client that routes everything through the
// given EnvironmentEdge, exactly as an in-environment sidecar would via HTTP_PROXY.
func proxiedClient(t *testing.T, edge *EnvironmentEdge) *http.Client {
	t.Helper()
	pu, err := url.Parse(edge.ProxyURL())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
		Timeout:   5 * time.Second,
	}
}

func TestCassetteRoundTrip(t *testing.T) {
	c := newCassette()
	c.put("GET", "api.example.com", "/v1/x", nil, 200, map[string]string{"Content-Type": "application/json"}, []byte(`{"a":1}`))
	// Same path, different body → distinct entry (body-keyed).
	c.put("POST", "api.example.com", "/v1/x", []byte(`{"q":"hi"}`), 201, nil, []byte(`{"created":true}`))
	c.put("POST", "api.example.com", "/v1/x", []byte(`{"q":"bye"}`), 202, nil, []byte(`{"created":false}`))

	if got := len(c.Entries); got != 3 {
		t.Fatalf("expected 3 entries, got %d", got)
	}

	path := filepath.Join(t.TempDir(), "cassette.json")
	if err := c.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadCassette(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ent, ok := loaded.lookup("GET", "api.example.com", "/v1/x", nil)
	if !ok || ent.Status != 200 || ent.Body != `{"a":1}` {
		t.Fatalf("GET lookup wrong: ok=%v ent=%+v", ok, ent)
	}
	hi, ok := loaded.lookup("POST", "api.example.com", "/v1/x", []byte(`{"q":"hi"}`))
	if !ok || hi.Status != 201 {
		t.Fatalf("POST hi lookup wrong: ok=%v ent=%+v", ok, hi)
	}
	bye, ok := loaded.lookup("POST", "api.example.com", "/v1/x", []byte(`{"q":"bye"}`))
	if !ok || bye.Status != 202 {
		t.Fatalf("POST bye lookup wrong: ok=%v ent=%+v", ok, bye)
	}
	if _, ok := loaded.lookup("POST", "api.example.com", "/v1/x", []byte(`{"q":"unknown"}`)); ok {
		t.Fatalf("unknown body should miss")
	}
}

func TestEnvironmentEdgeMockAndBlock(t *testing.T) {
	edge, err := startEnvironmentEdge(SandboxPolicy{
		Mocks: []HTTPMock{{
			Host:   "api.example.com",
			Path:   "/v1/things",
			Method: "GET",
			Status: 200,
			Body:   json.RawMessage(`{"ok":true}`),
		}},
	}, EdgeBlock, nil)
	if err != nil {
		t.Fatalf("start edge: %v", err)
	}
	defer edge.Stop()
	client := proxiedClient(t, edge)

	// Mock hit.
	resp, err := client.Get("http://api.example.com/v1/things")
	if err != nil {
		t.Fatalf("mock GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("mock response wrong: %d %s", resp.StatusCode, body)
	}

	// Unmatched → blocked in EdgeBlock mode.
	resp2, err := client.Get("http://api.example.com/v1/other")
	if err != nil {
		t.Fatalf("block GET: %v", err)
	}
	io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 block, got %d", resp2.StatusCode)
	}

	calls := edge.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 recorded calls, got %d", len(calls))
	}
	if !calls[0].Mocked {
		t.Errorf("first call should be Mocked: %+v", calls[0])
	}
	if !calls[1].Blocked {
		t.Errorf("second call should be Blocked: %+v", calls[1])
	}
}

func TestEnvironmentEdgeReplay(t *testing.T) {
	c := newCassette()
	c.put("GET", "api.example.com", "/v1/replayme", nil, 200, map[string]string{"Content-Type": "application/json"}, []byte(`{"replayed":true}`))

	edge, err := startEnvironmentEdge(SandboxPolicy{}, EdgeReplay, c)
	if err != nil {
		t.Fatalf("start edge: %v", err)
	}
	defer edge.Stop()
	client := proxiedClient(t, edge)

	// Cassette hit.
	resp, err := client.Get("http://api.example.com/v1/replayme")
	if err != nil {
		t.Fatalf("replay GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"replayed":true}` {
		t.Fatalf("replay response wrong: %d %s", resp.StatusCode, body)
	}

	// Replay miss → blocked (the determinism guarantee).
	resp2, err := client.Get("http://api.example.com/v1/nothere")
	if err != nil {
		t.Fatalf("replay miss GET: %v", err)
	}
	io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 on replay miss, got %d", resp2.StatusCode)
	}
}

func TestDefaultBinaryResolverMissing(t *testing.T) {
	if _, err := defaultBinaryResolver("definitely-not-a-real-app-xyz"); err == nil {
		t.Fatalf("expected error for unknown app binary")
	}
}
