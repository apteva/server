package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func poolComponentFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	s, id := pageToolsFixture(t)
	if _, err := s.store.db.Exec(`INSERT INTO app_component_declarations(install_id,parent_version,name,declaration_json) SELECT id,version,'fixture','{}' FROM app_installs WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	s.manifestRefreshInFlight.Store(true)
	return s, id
}
func waitPoolCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("pool synchronization barrier timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func holdRequestConnections(t *testing.T, s *Store, count int) []*sql.Conn {
	t.Helper()
	out := []*sql.Conn{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < count; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	t.Cleanup(func() {
		for _, c := range out {
			c.Close()
		}
	})
	return out
}

func TestDatabasePoolSingleRequestConnectionListsAndLoadsComponents(t *testing.T) {
	s, _ := poolComponentFixture(t)
	s.store.db.SetMaxOpenConns(2)
	s.store.db.SetMaxIdleConns(2)
	if s.store.db.Stats().InUse != 1 {
		t.Fatal("fixture must account for reserved usage-tracker connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request := httptest.NewRequest("GET", "/apps?project_id=p", nil).WithContext(ctx)
	request.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleListApps(w, request)
	if w.Code != 200 {
		t.Fatalf("component listing: HTTP %d %s", w.Code, w.Body.String())
	}
	var apps []AppRow
	if err := json.Unmarshal(w.Body.Bytes(), &apps); err != nil || len(apps) != 1 || len(apps[0].Components) != 1 {
		t.Fatalf("component data missing: %s %v", w.Body.String(), err)
	}
	s.LoadInstalledAppsContext(ctx)
	if len(s.installedApps.ListForProject("p")) != 1 {
		t.Fatal("loading token/runtime metadata retained an outer cursor")
	}
	// Operation publication must use its existing transaction connection too.
	op := seedInstallOperation(t, s, apps[0].InstallID, "running")
	if err := s.appRuntimeManager().saveOperation(op); err != nil {
		t.Fatalf("transaction helper borrowed a second connection: %v", err)
	}
	if got := s.store.db.Stats().InUse; got != 1 {
		t.Fatalf("pool did not return to usage-tracker baseline: %d", got)
	}
}

func TestDatabasePoolAuthenticationCancellationIs503AndKeepsCredentials(t *testing.T) {
	s, _ := poolComponentFixture(t)
	key := "pool-auth-key"
	if _, err := s.store.CreateAPIKey(1, "pool", HashAPIKey(key), "test"); err != nil {
		t.Fatal(err)
	}
	session := "pool-session"
	if err := s.store.CreateSession(session, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.store.db.SetMaxOpenConns(2)
	s.store.db.SetMaxIdleConns(2)
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for _, mode := range []string{"key", "session"} {
		t.Run(mode, func(t *testing.T) {
			held := holdRequestConnections(t, s.store, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("GET", "/agents", nil).WithContext(ctx)
			if mode == "key" {
				req.Header.Set("Authorization", "Bearer "+key)
			} else {
				req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
			}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			waitBefore := s.store.db.Stats().WaitCount
			go func() { defer close(done); handler(w, req) }()
			waitPoolCondition(t, func() bool { return s.store.db.Stats().WaitCount > waitBefore })
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancelled authentication stayed blocked")
			}
			if w.Code != 503 || w.Header().Get("Set-Cookie") != "" {
				t.Fatalf("database failure invalidated credential: HTTP %d %v", w.Code, w.Header())
			}
			held[0].Close()
			req = httptest.NewRequest("GET", "/agents", nil)
			if mode == "key" {
				req.Header.Set("Authorization", "Bearer "+key)
			} else {
				req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
			}
			w = httptest.NewRecorder()
			handler(w, req)
			if w.Code != 200 {
				t.Fatalf("original credential stopped working: HTTP %d", w.Code)
			}
		})
	}
	if _, err := s.store.GetSession(session); err != nil {
		t.Fatal("session revoked by database cancellation")
	}
	waitPoolCondition(t, func() bool { return s.store.db.Stats().InUse == 1 })
}

func TestDatabasePoolLegacyAppPolicyCancellationIsServiceFailure(t *testing.T) {
	s, id := poolComponentFixture(t)
	allowed, err := s.runtimeLegacyTokenAllowedContext(context.Background(), id)
	if err != nil || !allowed {
		t.Fatalf("unmanaged app policy: allowed=%v err=%v", allowed, err)
	}
	s.store.db.SetMaxOpenConns(2)
	held := holdRequestConnections(t, s.store, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	before := s.store.db.Stats().WaitCount
	go func() {
		_, err := s.runtimeLegacyTokenAllowedContext(ctx, id)
		done <- err
	}()
	waitPoolCondition(t, func() bool { return s.store.db.Stats().WaitCount > before })
	cancel()
	select {
	case err := <-done:
		w := httptest.NewRecorder()
		if !errors.Is(err, context.Canceled) || !databaseHTTPFailure(w, err) || w.Code != 503 {
			t.Fatalf("policy lookup hid a service failure: HTTP %d %v", w.Code, err)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy app policy ignored request cancellation")
	}
	held[0].Close()
	if allowed, err := s.runtimeLegacyTokenAllowedContext(context.Background(), id); err != nil || !allowed {
		t.Fatalf("cancellation invalidated the app policy: %v %v", allowed, err)
	}
}

func TestDatabasePoolMetadataDeadlineReadinessAndLiveness(t *testing.T) {
	s, _ := poolComponentFixture(t)
	s.ready.Store(true)
	s.store.guard().timeout = 80 * time.Millisecond
	s.store.db.SetMaxOpenConns(2)
	s.store.db.SetMaxIdleConns(2)
	held := holdRequestConnections(t, s.store, 1)
	start := time.Now()
	_, err := s.store.GetUserByID(1)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("metadata deadline not bounded: %v", err)
	}
	w := httptest.NewRecorder()
	s.handleDatabaseReadiness(w, httptest.NewRequest("GET", "/ready", nil))
	if w.Code != 503 {
		t.Fatalf("exhausted request pool passed readiness: HTTP %d", w.Code)
	}
	alive := httptest.NewRecorder()
	writeServerHealth(alive, true)
	if alive.Code != 200 {
		t.Fatal("pool exhaustion broke liveness")
	}
	held[0].Close()
	w = httptest.NewRecorder()
	s.handleDatabaseReadiness(w, httptest.NewRequest("GET", "/ready", nil))
	if w.Code != 200 {
		t.Fatalf("readiness did not recover: %s", w.Body.String())
	}
	var result struct {
		Database struct {
			MaxOpen    int `json:"max_open"`
			InUse      int `json:"in_use"`
			WaitCount  int `json:"wait_count"`
			Operations map[string]struct {
				Count    int `json:"count"`
				Timeouts int `json:"timeouts"`
			} `json:"operations"`
		} `json:"database"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Database.WaitCount < 2 || result.Database.Operations["row"].Timeouts < 2 || result.Database.MaxOpen != 2 {
		t.Fatalf("pool/latency metrics missing: %s %v", w.Body.String(), err)
	}
}

func TestDatabasePoolCursorCleanupOnScanAndIterationErrors(t *testing.T) {
	s, _ := poolComponentFixture(t)
	s.store.db.SetMaxOpenConns(2)
	// The list reader expects 21 columns. Even a malformed projection must close.
	if records, err := s.readListedApps(context.Background(), "SELECT 1"); err == nil || records != nil {
		t.Fatal("scan failure was silently skipped")
	}
	if s.store.db.Stats().InUse != 1 {
		t.Fatal("scan error leaked cursor")
	}
	rows, err := s.store.db.Query(`SELECT 1 UNION ALL SELECT json_extract('invalid-json','$')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = collectSQLRows(rows, func(r rowScanner) (int, error) { var n int; err := r.Scan(&n); return n, err })
	if err == nil {
		t.Fatal("iteration error was silently ignored")
	}
	if s.store.db.Stats().InUse != 1 {
		t.Fatal("iteration error leaked cursor")
	}
}

func TestDatabasePoolConcurrentDashboardAndBackgroundReturnToBaseline(t *testing.T) {
	s, id := poolComponentFixture(t)
	key := "concurrent-pool-key"
	if _, err := s.store.CreateAPIKey(1, "pool", HashAPIKey(key), "test"); err != nil {
		t.Fatal(err)
	}
	if s.store.db.Stats().MaxOpenConnections != sqliteMaxOpenConns {
		t.Fatal("production connection bound changed")
	}
	held := holdRequestConnections(t, s.store, sqliteMaxOpenConns-1)
	const requests = 16
	start := make(chan struct{})
	codes := make(chan int, requests)
	background := make(chan error, 1)
	var wg sync.WaitGroup
	handler := s.authMiddleware(s.handleListApps)
	before := s.store.db.Stats().WaitCount
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req := httptest.NewRequest("GET", "/apps?project_id=p", nil).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			handler(w, req)
			codes <- w.Code
		}()
	}
	go func() {
		<-start
		_, err := s.store.dbExec(`UPDATE app_installs SET status_message=status_message WHERE id=?`, id)
		background <- err
	}()
	close(start)
	// Force all requests to overlap at acquisition before releasing one slot.
	waitPoolCondition(t, func() bool { return s.store.db.Stats().WaitCount >= before+requests+1 })
	held[0].Close()
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 && code != 503 {
			t.Fatalf("overlap produced auth denial or internal failure: HTTP %d", code)
		}
	}
	if err := <-background; err != nil {
		t.Fatal(err)
	}
	for _, c := range held[1:] {
		c.Close()
	}
	waitPoolCondition(t, func() bool { return s.store.db.Stats().InUse == 1 })
	if s.store.db.Stats().WaitDuration == 0 {
		t.Fatal("overlap was not exercised")
	}
}

func TestDatabasePoolCancellationBoundsManagerAndStoreShutdown(t *testing.T) {
	s, _ := poolComponentFixture(t)
	m := s.appRuntimeManager()
	s.store.db.SetMaxOpenConns(2)
	held := holdRequestConnections(t, s.store, 1)
	done := make(chan error, 1)
	before := s.store.db.Stats().WaitCount
	go func() { _, err := m.loadDeployment("unavailable"); done <- err }()
	waitPoolCondition(t, func() bool { return s.store.db.Stats().WaitCount > before })
	start := time.Now()
	m.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("manager shutdown did not cancel DB acquisition: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("manager shutdown stayed blocked")
	}
	if time.Since(start) > time.Second {
		t.Fatal("manager close exceeded bound")
	}
	done = make(chan error, 1)
	before = s.store.db.Stats().WaitCount
	go func() { _, err := s.store.GetUserByID(1); done <- err }()
	waitPoolCondition(t, func() bool { return s.store.db.Stats().WaitCount > before })
	closed := make(chan error, 1)
	go func() { closed <- s.store.Close() }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Store.Close waited on exhausted pool")
	}
	select {
	case err := <-done:
		if !databaseUnavailable(err) && !strings.Contains(err.Error(), "closed") {
			t.Fatalf("unexpected shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("database wait survived shutdown")
	}
	held[0].Close()
}
