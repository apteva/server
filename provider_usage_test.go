package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func installTestProviderUsage(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	oldBaseURL := providerUsageCodexBaseURL
	oldClient := providerUsageHTTPClient
	oldFreshTTL := providerUsageFreshTTL
	oldStaleTTL := providerUsageStaleTTL
	oldManualRefreshMin := providerUsageManualRefreshMin
	providerUsageCodexBaseURL = server.URL
	providerUsageHTTPClient = server.Client()
	providerUsageFreshTTL = 2 * time.Minute
	providerUsageStaleTTL = 30 * time.Minute
	providerUsageManualRefreshMin = 30 * time.Second
	globalProviderUsageCache = &providerUsageCacheStore{entries: map[string]providerUsageCacheEntry{}}
	t.Cleanup(func() {
		server.Close()
		providerUsageCodexBaseURL = oldBaseURL
		providerUsageHTTPClient = oldClient
		providerUsageFreshTTL = oldFreshTTL
		providerUsageStaleTTL = oldStaleTTL
		providerUsageManualRefreshMin = oldManualRefreshMin
		globalProviderUsageCache = &providerUsageCacheStore{entries: map[string]providerUsageCacheEntry{}}
	})
}

func createTestCodexUsageProvider(t *testing.T, s *Server, state map[string]any) *Provider {
	t.Helper()
	if len(s.secret) == 0 {
		s.secret = testSecret()
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := Encrypt(s.secret, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := s.store.CreateProvider(1, 15, "llm", "OpenAI Codex", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func connectedTestCodexUsageState(accessToken string) map[string]any {
	return map[string]any{
		"auth": map[string]any{"provider": openAICodexAuthProvider},
		"account": map[string]any{
			"chatgpt_account_id": "account-usage",
		},
		"credentials": map[string]any{
			"access_token": accessToken,
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	}
}

func requestTestProviderUsage(t *testing.T, s *Server, userID, providerID int64, refresh bool) (*httptest.ResponseRecorder, ProviderUsageSnapshot) {
	t.Helper()
	path := "/providers/" + itoa64(providerID) + "/usage"
	if refresh {
		path += "?refresh=1"
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleProviderUsage(rec, req)
	var snapshot ProviderUsageSnapshot
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &snapshot)
	}
	return rec, snapshot
}

func writeTestCodexUsage(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plan_type": "pro",
		"rate_limit": map[string]any{
			"allowed":       true,
			"limit_reached": false,
			"primary_window": map[string]any{
				"used_percent": 17, "limit_window_seconds": 18000,
				"reset_after_seconds": 900, "reset_at": 1783720896,
			},
			"secondary_window": map[string]any{
				"used_percent": 42, "limit_window_seconds": 604800,
				"reset_after_seconds": 86400, "reset_at": 1784271684,
			},
		},
		"credits": map[string]any{"has_credits": true, "unlimited": false, "balance": "12.50"},
		"additional_rate_limits": []map[string]any{{
			"limit_name": "GPT-5.3-Codex-Spark", "metered_feature": "codex_spark",
			"rate_limit": map[string]any{
				"allowed": true, "limit_reached": false,
				"primary_window": map[string]any{
					"used_percent": 3, "limit_window_seconds": 3600,
					"reset_after_seconds": 120, "reset_at": 1783721000,
				},
			},
		}},
	})
}

func TestProviderUsageCodexNormalizesCachesAndThrottlesRefresh(t *testing.T) {
	var calls atomic.Int32
	installTestProviderUsage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/wham/usage" {
			t.Errorf("path=%q want /wham/usage", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-usage" {
			t.Errorf("Authorization=%q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-usage" {
			t.Errorf("ChatGPT-Account-ID=%q", got)
		}
		writeTestCodexUsage(w)
	}))

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	provider := createTestCodexUsageProvider(t, s, connectedTestCodexUsageState("access-usage"))

	rec, snapshot := requestTestProviderUsage(t, s, 1, provider.ID, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !snapshot.Supported || snapshot.Kind != "subscription_quota" || snapshot.Plan != "pro" || snapshot.Stale {
		t.Fatalf("snapshot metadata=%+v", snapshot)
	}
	if snapshot.ProviderID != provider.ID || snapshot.FetchedAt.IsZero() {
		t.Fatalf("snapshot identity=%+v", snapshot)
	}
	if len(snapshot.Limits) != 2 {
		t.Fatalf("limits=%+v", snapshot.Limits)
	}
	primary := snapshot.Limits[0]
	if primary.ID != "codex" || len(primary.Windows) != 2 {
		t.Fatalf("primary limit=%+v", primary)
	}
	if primary.Windows[0].UsedPercent != 17 || primary.Windows[0].DurationMinutes != 300 {
		t.Fatalf("5h window=%+v", primary.Windows[0])
	}
	if primary.Windows[1].UsedPercent != 42 || primary.Windows[1].DurationMinutes != 10080 {
		t.Fatalf("weekly window=%+v", primary.Windows[1])
	}
	if snapshot.Limits[1].Label != "GPT-5.3-Codex-Spark" || len(snapshot.Limits[1].Windows) != 1 {
		t.Fatalf("additional limit=%+v", snapshot.Limits[1])
	}
	if snapshot.Credits == nil || !snapshot.Credits.HasCredits || snapshot.Credits.Balance != "12.50" {
		t.Fatalf("credits=%+v", snapshot.Credits)
	}

	// Cached reads and immediate manual refreshes share the same upstream
	// result; refresh=1 cannot turn the Providers page into a polling loop.
	if rec, _ := requestTestProviderUsage(t, s, 1, provider.ID, false); rec.Code != http.StatusOK {
		t.Fatalf("cached usage status=%d", rec.Code)
	}
	if rec, _ := requestTestProviderUsage(t, s, 1, provider.ID, true); rec.Code != http.StatusOK {
		t.Fatalf("throttled refresh status=%d", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1", got)
	}
}

func TestProviderUsageCodexServesRecentCacheAsStaleOnFailure(t *testing.T) {
	var fail atomic.Bool
	installTestProviderUsage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "temporary outage", http.StatusServiceUnavailable)
			return
		}
		writeTestCodexUsage(w)
	}))
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	state := connectedTestCodexUsageState("access-stale")
	provider := createTestCodexUsageProvider(t, s, state)
	if rec, _ := requestTestProviderUsage(t, s, 1, provider.ID, false); rec.Code != http.StatusOK {
		t.Fatalf("initial status=%d body=%s", rec.Code, rec.Body.String())
	}

	key := (codexProviderUsageFetcher{}).CacheKey(state)
	globalProviderUsageCache.mu.Lock()
	entry := globalProviderUsageCache.entries[key]
	entry.fetched = time.Now().Add(-providerUsageFreshTTL - time.Second)
	globalProviderUsageCache.entries[key] = entry
	globalProviderUsageCache.mu.Unlock()
	fail.Store(true)

	rec, snapshot := requestTestProviderUsage(t, s, 1, provider.ID, false)
	if rec.Code != http.StatusOK || !snapshot.Stale {
		t.Fatalf("stale fallback status=%d snapshot=%+v body=%s", rec.Code, snapshot, rec.Body.String())
	}
}

func TestProviderUsageCacheSingleflightsConcurrentAccountReads(t *testing.T) {
	var calls atomic.Int32
	installTestProviderUsage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(40 * time.Millisecond)
		writeTestCodexUsage(w)
	}))
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	state := connectedTestCodexUsageState("access-concurrent")
	provider := createTestCodexUsageProvider(t, s, state)
	fetcher := codexProviderUsageFetcher{}

	start := make(chan struct{})
	errs := make(chan error, 8)
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			snapshot, err := s.fetchProviderUsage(t.Context(), 1, provider, openAICodexAuthProvider, state, fetcher, false)
			if err != nil {
				errs <- err
				return
			}
			if !snapshot.Supported || len(snapshot.Limits) == 0 {
				errs <- fmt.Errorf("invalid snapshot: %+v", snapshot)
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent upstream calls=%d want 1", got)
	}
}

func TestProviderUsageCodexRefreshesExpiredOAuthBeforeFetch(t *testing.T) {
	freshToken := testJWTWithExpiry(time.Now().Add(time.Hour))
	installTestProviderUsage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+freshToken {
			t.Errorf("usage Authorization=%q", got)
		}
		writeTestCodexUsage(w)
	}))
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("refresh_token") != "refresh-usage" {
			t.Fatalf("refresh token=%q", r.Form.Get("refresh_token"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": freshToken, "refresh_token": "next-refresh-usage"})
	}))
	defer tokenServer.Close()
	oldTokenEndpoint := openAICodexTokenEndpoint
	openAICodexTokenEndpoint = tokenServer.URL
	defer func() { openAICodexTokenEndpoint = oldTokenEndpoint }()

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	state := connectedTestCodexUsageState("expired-access")
	state["credentials"].(map[string]any)["refresh_token"] = "refresh-usage"
	state["credentials"].(map[string]any)["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	provider := createTestCodexUsageProvider(t, s, state)
	rec, snapshot := requestTestProviderUsage(t, s, 1, provider.ID, false)
	if rec.Code != http.StatusOK || !snapshot.Supported {
		t.Fatalf("refreshed usage status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProviderUsageUnsupportedAndOwnershipIsolation(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	raw, _ := json.Marshal(map[string]any{"OPENAI_API_KEY": "sk-test"})
	encrypted, _ := Encrypt(s.secret, string(raw))
	provider, err := s.store.CreateProvider(1, 1, "llm", "OpenAI", encrypted)
	if err != nil {
		t.Fatal(err)
	}

	rec, snapshot := requestTestProviderUsage(t, s, 1, provider.ID, false)
	if rec.Code != http.StatusOK || snapshot.Supported {
		t.Fatalf("unsupported status=%d snapshot=%+v", rec.Code, snapshot)
	}
	other, err := s.store.CreateUser("provider-usage-other@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	rec, _ = requestTestProviderUsage(t, s, other.ID, provider.ID, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user usage status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCodexProviderTypeAdvertisesSubscriptionUsage(t *testing.T) {
	s := newTestServer(t)
	providerType, err := s.store.GetProviderType(15)
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range providerType.Capabilities {
		if capability == providerUsageCapability {
			return
		}
	}
	t.Fatalf("Codex capabilities=%v missing %q", providerType.Capabilities, providerUsageCapability)
}
