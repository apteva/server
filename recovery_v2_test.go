package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoveryV2FreshHostAndWrappedKey(t *testing.T) {
	source := newTestServer(t)
	source.dataDir = t.TempDir()
	source.localApps = NewLocalSupervisor(t.TempDir())
	id := insertTestInstall(t, source, "offline-app")
	source.store.db.Exec("UPDATE app_installs SET status='stopped' WHERE id=?", id)
	appDir := filepath.Join(source.localApps.cacheDir, "offline-app", "data", strconv.FormatInt(id, 10))
	os.MkdirAll(appDir, 0700)
	mustSeedSqlite(t, filepath.Join(appDir, "app.db"))
	os.WriteFile(filepath.Join(appDir, "attachment.txt"), []byte("persistent attachment"), 0600)
	os.WriteFile(filepath.Join(appDir, "worker.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	ensureTestAdmin(t, source)
	agent, err := source.store.CreateAgent(1, "recover", "", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(source.agents.dataDir, "instance_"+strconv.FormatInt(agent.ID, 10))
	os.MkdirAll(agentDir, 0700)
	os.WriteFile(filepath.Join(agentDir, "config.json"), []byte(`{"directive":"keep me"}`), 0600)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Backup-Passphrase", "a separate recovery passphrase")
	out := httptest.NewRecorder()
	source.writePlatformSnapshot(out, req)
	if out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	archive := out.Body.Bytes()
	dest := newTestServer(t)
	dest.dataDir = t.TempDir()
	dest.dbPath = dest.store.path
	dest.secret = bytes.Repeat([]byte{0x75}, 32)
	dest.localApps = NewLocalSupervisor(t.TempDir())
	insertTestInstall(t, dest, "different-existing-app")
	restore := func(pass string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/", bytes.NewReader(archive))
		r.Header.Set("X-Backup-Passphrase", pass)
		w := httptest.NewRecorder()
		dest.restorePlatformSnapshot(w, r)
		return w
	}
	if w := restore("incorrect passphrase"); w.Code != 400 {
		t.Fatalf("wrong key accepted: %d", w.Code)
	}
	w := restore("a separate recovery passphrase")
	if w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	recovered := filepath.Join(dest.localApps.cacheDir, "offline-app", "data", strconv.FormatInt(id, 10))
	if _, err = os.Stat(filepath.Join(recovered, "attachment.txt")); !os.IsNotExist(err) {
		t.Fatal("restore changed active app data before restart")
	}
	dest.store.Close()
	if err = applyPendingRecovery(dest.dbPath); err != nil {
		t.Fatal(err)
	}
	if err = applyPendingRecovery(dest.dbPath); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(recovered, "attachment.txt")); err != nil || string(b) != "persistent attachment" {
		t.Fatalf("app attachment: %q %v", b, err)
	}
	if info, err := os.Stat(filepath.Join(recovered, "worker.sh")); err != nil || info.Mode().Perm()&0100 == 0 {
		t.Fatalf("executable permission lost: %v", err)
	}
	if err = validateRecoveryDB(filepath.Join(recovered, "app.db")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest.agents.dataDir, "instance_"+strconv.FormatInt(agent.ID, 10), "config.json")); err != nil || !bytes.Contains(b, []byte("keep me")) {
		t.Fatalf("agent configuration: %q %v", b, err)
	}
	key, err := LoadSecret(dest.dataDir)
	if err != nil || !bytes.Equal(key, source.secret) {
		t.Fatalf("key recovery failed: %v", err)
	}
	reopened, err := NewStore(dest.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var name string
	if err = reopened.db.QueryRow("SELECT name FROM apps WHERE id=(SELECT app_id FROM app_installs WHERE id=?)", id).Scan(&name); err != nil || name != "offline-app" {
		t.Fatalf("snapshot identities: %q %v", name, err)
	}
}

func TestTelemetrySummariesCorrectReplayUpdateDeleteAndReopen(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	a, err := s.store.CreateAgent(1, "metrics", "", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute).Add(30 * time.Second)
	events := []TelemetryEvent{{ID: "excluded", AgentID: a.ID, Type: "llm.done", Time: start.Add(-time.Second), Data: json.RawMessage(`{"tokens_in":100}`)}, {ID: "partial", AgentID: a.ID, Type: "llm.done", Time: start.Add(time.Second), Data: json.RawMessage(`{"tokens_in":3,"cost_usd":0.1}`)}, {ID: "whole", AgentID: a.ID, Type: "llm.done", Time: start.Add(time.Minute), Data: json.RawMessage(`{"tokens_in":7,"cost_usd":0.2}`)}}
	if err = s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	if err = s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	check := func(store *Store, want int) {
		t.Helper()
		v, err := store.TelemetryStats(a.ID, start)
		if err != nil || v.TotalTokensIn != want {
			t.Fatalf("summary=%+v want tokens=%d err=%v", v, want, err)
		}
	}
	check(s.store, 10)
	if _, err = s.store.db.Exec("UPDATE telemetry SET data=? WHERE id='whole'", `{"tokens_in":11}`); err != nil {
		t.Fatal(err)
	}
	check(s.store, 14)
	if _, err = s.store.db.Exec("DELETE FROM telemetry WHERE id='partial'"); err != nil {
		t.Fatal(err)
	}
	check(s.store, 11)
	s.store.Close()
	db, err := NewStore(s.store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check(db, 11)
	if _, err = db.CleanOldTelemetry(time.Second); err != nil {
		t.Fatal(err)
	}
	check(db, 0)
}

func TestConnectionRefreshConcurrentCallersShareRotation(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	raw, _ := Encrypt(s.secret, `{"access_token":"old","refresh_token":"old-refresh"}`)
	result, err := s.store.db.Exec("INSERT INTO connections(user_id,app_slug,app_name,name,auth_type,encrypted_credentials) VALUES(1,'test','Test','test','oauth2',?)", raw)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			credentials := map[string]string{"access_token": "old", "refresh_token": "old-refresh"}
			err := s.refreshConnectionCredentials(id, credentials, func(c map[string]string) error {
				calls.Add(1)
				time.Sleep(10 * time.Millisecond)
				c["access_token"] = "new"
				c["refresh_token"] = "new-refresh"
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			if credentials["access_token"] != "new" {
				t.Error("stale token")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh called %d times", calls.Load())
	}
}

func TestRequestDrainCancelsExternalAndKeepsCallbacks(t *testing.T) {
	entered := make(chan struct{})
	d := newRequestDrain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/apps/callback/x" {
			w.WriteHeader(204)
			return
		}
		close(entered)
		<-r.Context().Done()
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/instances", nil))
	}()
	<-entered
	d.begin()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request not canceled")
	}
	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest("POST", "/api/instances", nil))
	if w.Code != 503 {
		t.Fatalf("new work accepted: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/api/apps/callback/x", nil)
	req.RemoteAddr = "127.0.0.1:3333"
	w = httptest.NewRecorder()
	d.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatal("internal callback unavailable")
	}
}

func TestTelemetrySummaryFractionalMinuteBoundary(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	a, err := s.store.CreateAgent(1, "fractions", "", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	var events []TelemetryEvent
	for i, offset := range []time.Duration{30 * time.Second, 30*time.Second + 500*time.Millisecond, time.Minute - time.Nanosecond, time.Minute + time.Millisecond} {
		events = append(events, TelemetryEvent{ID: strconv.Itoa(i), AgentID: a.ID, Type: "llm.done", Time: base.Add(offset), Data: json.RawMessage(`{"tokens_in":1}`)})
	}
	if err = s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		since time.Time
		want  int
	}{{base.Add(30*time.Second + time.Nanosecond), 3}, {base.Add(time.Minute), 1}, {base.Add(time.Minute - time.Nanosecond), 2}} {
		stats, err := s.store.TelemetryStats(a.ID, test.since)
		if err != nil || stats.TotalTokensIn != test.want {
			t.Fatalf("since %s stats %+v want %d err %v", test.since, stats, test.want, err)
		}
	}
}

func TestOutboxRetrySurvivesDatabaseReopen(t *testing.T) {
	s := newTestServer(t)
	var calls atomic.Int32
	var mu sync.Mutex
	ids := map[string]int{}
	a, _ := auditCore(t, s, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		ids[body["event_id"]]++
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(200)
		}
	})
	_, err := s.store.CreateAppEventSubscription(1, a.ID, "durable", "audit:*", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := AppEvent{App: "audit", Topic: "changed", Data: json.RawMessage(`{}`), Time: time.Now()}
	if err = s.queueAppSubscriptions(ev); err != nil {
		t.Fatal(err)
	}
	d := NewAppEventDispatcher(s)
	d.drainOutbox(context.Background())
	path := s.store.path
	s.store.Close()
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.store = store
	if _, err = store.db.Exec("UPDATE app_subscription_outbox SET next_attempt=0"); err != nil {
		t.Fatal(err)
	}
	NewAppEventDispatcher(s).drainOutbox(context.Background())
	var delivered int
	if err = store.db.QueryRow("SELECT COUNT(*) FROM app_subscription_outbox WHERE status='delivered'").Scan(&delivered); err != nil || delivered != 1 {
		t.Fatalf("delivered %d err %v", delivered, err)
	}
	if len(ids) != 1 || ids[""] != 0 || calls.Load() != 2 {
		t.Fatalf("retry identity changed: %v calls=%d", ids, calls.Load())
	}
}

func TestSlowMCPStartupDoesNotBlockPeersAndCanBeCanceled(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	manager := NewMCPManager()
	defer manager.StopAll()
	done := make(chan error, 1)
	go func() {
		_, err := manager.Start(&MCPServerRecord{ID: 1, Transport: "http", URL: server.URL}, nil)
		done <- err
	}()
	<-entered
	checked := make(chan struct{})
	go func() { manager.IsRunning(999); close(checked) }()
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("unrelated lookup blocked")
	}
	manager.Stop(1)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled startup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("startup did not cancel")
	}
}

func TestEdgeConcurrentBuffersRespectSharedBudget(t *testing.T) {
	cache := NewEdgeCache()
	cache.maxBytes = 4096
	cache.maxItem = 1024
	var writers []*cacheWriter
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		w.Header().Set("Cache-Control", "public, max-age=60")
		cw := cache.wrap(w, httptest.NewRequest("GET", "/"+strconv.Itoa(i), nil), "example.com")
		cw.Write(bytes.Repeat([]byte{'a'}, 768))
		writers = append(writers, cw)
	}
	var capacity int64
	for _, cw := range writers {
		capacity += int64(cw.buf.Cap())
	}
	if capacity > cache.maxBytes {
		t.Fatalf("buffer capacity=%d budget=%d", capacity, cache.maxBytes)
	}
	for _, cw := range writers {
		cw.finalize()
	}
	if cache.inflight != 0 || cache.curBytes > cache.maxBytes {
		t.Fatalf("cache accounting inflight=%d stored=%d", cache.inflight, cache.curBytes)
	}
}

func TestEdgeStoredCapacityAndInflightShareBudget(t *testing.T) {
	c := NewEdgeCache()
	c.maxBytes = 8
	c.maxItem = 8
	c.inflight = 6
	c.store("other", &cacheEntry{size: 5, body: make([]byte, 5)})
	if c.curBytes+c.inflight > c.maxBytes {
		t.Fatal("stored fill exceeded reserved budget")
	}
	c.inflight = 0
	w := httptest.NewRecorder()
	w.Header().Set("Cache-Control", "public, max-age=60")
	cw := c.wrap(w, httptest.NewRequest("GET", "/", nil), "example.com")
	cw.Write([]byte("123"))
	cw.Write([]byte("45"))
	cw.finalize()
	if c.curBytes != 6 {
		t.Fatalf("stored backing capacity should be 6, got %d", c.curBytes)
	}
}

func TestEdgeNeverCachesTruncatedOrStreamingResponse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		c := NewEdgeCache()
		w := httptest.NewRecorder()
		w.Header().Set("Cache-Control", "public, max-age=60")
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Length", "10")
		}
		cw := c.wrap(w, httptest.NewRequest("GET", "/", nil), "example.com")
		cw.Write([]byte("123"))
		cw.finalize()
		if len(c.entries) != 0 || c.inflight != 0 {
			t.Fatalf("cached incomplete/streaming response: stream=%v", stream)
		}
	}
}

func TestIntegrationPoolNormalizesTLSCredentialFieldNames(t *testing.T) {
	plain := &AppTemplate{}
	if _, err := integrationHTTPClient(plain, nil, time.Second); err != nil {
		t.Fatal(err)
	}
	secured := &AppTemplate{}
	secured.Auth.MTLS = &MutualTLSConfig{CertField: " cert ", KeyField: " key "}
	if _, err := integrationHTTPClient(secured, map[string]string{"cert": "invalid certificate", "key": "invalid private key"}, time.Second); err == nil {
		t.Fatal("TLS configuration reused an unauthenticated transport instead of validating its credentials")
	}
}
