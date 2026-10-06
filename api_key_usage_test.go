package main

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitForAPIKeyUsage(t *testing.T, s *Store, id int64) (string, string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		var at, ip string
		if err := s.db.QueryRow("SELECT COALESCE(last_used,''),COALESCE(last_used_ip,'') FROM api_keys WHERE id=?", id).Scan(&at, &ip); err != nil {
			t.Fatal(err)
		}
		if at != "" {
			return at, ip
		}
		if time.Now().After(deadline) {
			t.Fatal("usage metadata was not eventually saved")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAPIKeyUsageAuthenticationDoesNotWaitForWriter(t *testing.T) {
	for _, kind := range []string{"private", "delegated_user", "public_client"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestServer(t)
			s.installedApps = NewInstalledAppsRegistry()
			u, err := s.store.CreateUser("usage@example.com", "hash")
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.store.CreateProject(u.ID, "Usage", "", "")
			if err != nil {
				t.Fatal(err)
			}
			raw, scope := "sk-usage-test", "app_action"
			if kind == "delegated_user" {
				raw, scope = "uk_usage_test", "app_user"
			}
			if kind == "public_client" {
				raw = "pk_usage_test"
			}
			key, err := s.store.CreateAPIKey(u.ID, "usage", HashAPIKey(raw), raw[:3], APIKeyCreateOptions{
				Kind: kind, ProjectID: p.ID, Access: APIKeyReadWrite, Scopes: fmt.Sprintf(`[{"type":%q,"app":"catalog","actions":["list"]}]`, scope), RateLimitPerMinute: 10000, AllowedOrigins: `["https://example.com"]`,
			})
			if err != nil {
				t.Fatal(err)
			}
			sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer sidecar.Close()
			s.installedApps.Add(&InstalledApp{InstallID: 1, AppName: "catalog", ProjectID: p.ID, SidecarURL: sidecar.URL, Token: "test"})
			mux := http.NewServeMux()
			s.registerAppRuntimeRoutes(mux)
			// This writer models cleanup without changing committed credentials.
			tx, err := s.store.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec("UPDATE api_keys SET name=name WHERE id=?", key.ID); err != nil {
				t.Fatal(err)
			}
			// The old synchronous metadata operation must block while our
			// actual authenticated request reaches its handler/proxy immediately.
			baseline := make(chan time.Duration, 1)
			go func() {
				start := time.Now()
				_, _ = s.store.db.Exec("UPDATE api_keys SET last_used=last_used WHERE id=?", key.ID)
				baseline <- time.Since(start)
			}()
			done := make(chan *httptest.ResponseRecorder, 1)
			start := time.Now()
			go func() {
				r := httptest.NewRequest(http.MethodPost, "/apps/catalog/mcp?project_id="+p.ID, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list","arguments":{}}}`))
				r.Header.Set("Authorization", "Bearer "+raw)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", "https://example.com")
				w := httptest.NewRecorder()
				if kind == "private" {
					s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })(w, r)
				} else {
					mux.ServeHTTP(w, r)
				}
				done <- w
			}()
			var authTime time.Duration
			select {
			case w := <-done:
				authTime = time.Since(start)
				if w.Code != 200 {
					t.Fatalf("auth status=%d: %s", w.Code, w.Body.String())
				}
			case <-time.After(500 * time.Millisecond):
				_ = tx.Rollback()
				<-done
				t.Fatal("authentication waited for a database writer")
			}
			s.store.apiKeyUsage.signal()
			// Allow the background worker's short busy timeout to expire.
			select {
			case <-baseline:
				t.Fatal("baseline write unexpectedly bypassed writer lock")
			case <-time.After(250 * time.Millisecond):
			}
			if s.store.apiKeyUsage.pendingCount() != 1 {
				t.Fatal("failed metadata write was not retained for retry")
			}
			s.store.apiKeyUsage.mu.Lock()
			queuedAt := s.store.apiKeyUsage.pending[key.ID].at.Format("2006-01-02 15:04:05")
			s.store.apiKeyUsage.mu.Unlock()
			var busy int
			if err := s.store.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil || busy != sqliteBusyTimeoutMS {
				t.Fatalf("request busy timeout changed: %d %v", busy, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			baselineTime := <-baseline
			s.store.apiKeyUsage.signal()
			at, _ := waitForAPIKeyUsage(t, s.store, key.ID)
			if at != queuedAt {
				t.Fatalf("usage timestamp reflects flush time instead of request time: %s != %s", at, queuedAt)
			}
			t.Logf("%s: authenticated request=%s; synchronous write=%s; eventual last_used=%s", kind, authTime, baselineTime, at)
		})
	}
}

func TestAPIKeyUsageBoundedCoalescingAndRetry(t *testing.T) {
	w := &apiKeyUsageTracker{pending: make(map[int64]apiKeyUsageUpdate), wake: make(chan struct{}, 1)}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 1000; j++ {
				w.record(1, "192.0.2.1", true)
			}
		})
	}
	wg.Wait()
	if w.pendingCount() != 1 {
		t.Fatal("requests were not coalesced")
	}
	old := w.take()
	w.record(1, "192.0.2.2", true)
	w.retry(old)
	if w.pending[1].ip != "192.0.2.2" || !w.pending[1].at.After(old[0].at) {
		t.Fatal("retry overwrote newer usage")
	}
	w.record(1, "", false)
	if w.pending[1].ip != "192.0.2.2" {
		t.Fatal("private lookup cleared the IP")
	}
	for id := int64(2); id <= apiKeyUsageCapacity+20; id++ {
		w.record(id, strings.Repeat("x", 200), true)
	}
	if w.pendingCount() != apiKeyUsageCapacity || w.dropped.Load() != 20 {
		t.Fatalf("queue is unbounded: pending=%d dropped=%d", w.pendingCount(), w.dropped.Load())
	}
	if len(w.pending[2].ip) != 128 {
		t.Fatal("unbounded IP value")
	}
	batch := w.take()
	if len(batch) != apiKeyUsageBatchSize {
		t.Fatalf("batch size=%d", len(batch))
	}
	for id := int64(10000); id < 10000+apiKeyUsageBatchSize; id++ {
		w.record(id, "", false)
	}
	w.retry(batch)
	if w.pendingCount() != apiKeyUsageCapacity {
		t.Fatal("retry exceeded capacity")
	}
	w.stopped = true
	w.record(99999, "", false)
	if _, ok := w.pending[99999]; ok {
		t.Fatal("recorded after shutdown")
	}
}

func TestAPIKeyUsageShutdownFlushAndCredentialValidation(t *testing.T) {
	s := newTestStore(t)
	u, err := s.CreateUser("shutdown@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.CreateAPIKey(u.ID, "usage", HashAPIKey("test-usage"), "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"revoked_at", "expires_at"} {
		if _, err := s.db.Exec("UPDATE api_keys SET "+column+"='2000-01-01' WHERE id=?", k.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.getPrivateAPIKeyPrincipal(HashAPIKey("test-usage")); err == nil {
			t.Fatal("inactive key authenticated")
		}
		if s.apiKeyUsage.pendingCount() != 0 {
			t.Fatal("invalid credentials queued usage")
		}
		if _, err := s.db.Exec("UPDATE api_keys SET "+column+"=NULL WHERE id=?", k.ID); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		s.MarkAPIKeyUsed(k.ID, "192.0.2.8")
	}
	if _, _, err := s.getPrivateAPIKeyPrincipal(HashAPIKey("test-usage")); err != nil {
		t.Fatal(err)
	}
	// Close flushes sub-batch queues immediately, preserving the supplied IP.
	s.apiKeyUsage.Close()
	at, ip := waitForAPIKeyUsage(t, s, k.ID)
	if ip != "192.0.2.8" {
		t.Fatalf("IP lost on private lookup: %q (%s)", ip, at)
	}
	if s.apiKeyUsage.pendingCount() != 0 {
		t.Fatal("shutdown did not flush")
	}
	if err := s.DeleteAPIKey(u.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.getPrivateAPIKeyPrincipal(HashAPIKey("test-usage")); err != sql.ErrNoRows {
		t.Fatalf("deleted key accepted: %v", err)
	}
}

func TestAPIKeyUsageShutdownHasDeadline(t *testing.T) {
	s := newTestStore(t)
	u, err := s.CreateUser("deadline@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.CreateAPIKey(u.ID, "usage", HashAPIKey("deadline"), "test")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	s.MarkAPIKeyUsed(k.ID, "192.0.2.9")
	start := time.Now()
	s.apiKeyUsage.Close()
	if elapsed := time.Since(start); elapsed > apiKeyUsageShutdownTimeout+500*time.Millisecond {
		t.Fatalf("shutdown exceeded deadline: %s", elapsed)
	}
	select {
	case <-s.apiKeyUsage.done:
	default:
		t.Fatal("worker did not stop")
	}
}

func TestAPIKeyUsageMultipleBatchesDoNotReviveCredentials(t *testing.T) {
	s := newTestStore(t)
	u, err := s.CreateUser("batches@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	const keys = 3*apiKeyUsageBatchSize + 3
	ids := make([]int64, keys)
	for i := range ids {
		result, err := tx.Exec("INSERT INTO api_keys(user_id,name,key_hash,key_prefix) VALUES(?,?,?,?)", u.ID, "batch", HashAPIKey(fmt.Sprint(i)), "test")
		if err != nil {
			t.Fatal(err)
		}
		ids[i], err = result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		s.MarkAPIKeyUsed(id, "192.0.2.10")
	}
	if _, err := tx.Exec("UPDATE api_keys SET revoked_at=CURRENT_TIMESTAMP WHERE id=?", ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("DELETE FROM api_keys WHERE id=?", ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.apiKeyUsage.Close()
	var updated int
	if err := s.db.QueryRow("SELECT count(*) FROM api_keys WHERE last_used IS NOT NULL AND last_used_ip='192.0.2.10'").Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if updated != keys-2 || s.apiKeyUsage.pendingCount() != 0 {
		t.Fatalf("batches incomplete: updated=%d want=%d pending=%d", updated, keys-2, s.apiKeyUsage.pendingCount())
	}
	var revoked, used sql.NullString
	if err := s.db.QueryRow("SELECT revoked_at,last_used FROM api_keys WHERE id=?", ids[0]).Scan(&revoked, &used); err != nil {
		t.Fatal(err)
	}
	if !revoked.Valid || used.Valid {
		t.Fatal("queued usage changed revoked credentials")
	}
	if _, _, err := s.getPrivateAPIKeyPrincipal(HashAPIKey("0")); err != sql.ErrNoRows {
		t.Fatalf("revoked key accepted: %v", err)
	}
	if _, _, err := s.getPrivateAPIKeyPrincipal(HashAPIKey("1")); err != sql.ErrNoRows {
		t.Fatalf("deleted key recreated: %v", err)
	}
}

func TestAPIKeyUsageRecoversDiscardedConnection(t *testing.T) {
	s := newTestStore(t)
	u, err := s.CreateUser("reconnect@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.CreateAPIKey(u.ID, "usage", HashAPIKey("reconnect"), "test")
	if err != nil {
		t.Fatal(err)
	}
	// With an empty queue the worker does not touch the connection. Model
	// database/sql discarding it after a driver interrupt or connection error.
	if err := s.apiKeyUsage.conn.Raw(func(any) error { return driver.ErrBadConn }); err != driver.ErrBadConn {
		t.Fatalf("discard: %v", err)
	}
	s.MarkAPIKeyUsed(k.ID, "192.0.2.11")
	s.apiKeyUsage.signal()
	_, ip := waitForAPIKeyUsage(t, s, k.ID)
	if ip != "192.0.2.11" {
		t.Fatalf("retry lost usage IP: %s", ip)
	}
}
