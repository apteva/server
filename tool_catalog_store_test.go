package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var legacyCatalogFixture = json.RawMessage(`{"revision":1,"aliases":{"z":"b","a":"a"},"tools":[{"identity":{"name":"b"},"parameters":{"z":1,"a":2},"loading":"auto","description":"B"},{"identity":{"name":"a"},"description":"A"}]}`)

func TestCatalogStoreDeduplicatesOlderCoreRefreshesAcrossAgents(t *testing.T) {
	store := newTestStore(t)
	at := time.Now().UTC()
	events := make([]TelemetryEvent, 6996)
	for i := range events {
		var d map[string]any
		_ = json.Unmarshal(legacyCatalogFixture, &d)
		d["revision"] = i + 1
		if i%2 == 1 {
			tools := d["tools"].([]any)
			tools[0], tools[1] = tools[1], tools[0]
		}
		data, _ := json.Marshal(d)
		events[i] = TelemetryEvent{ID: fmt.Sprintf("refresh-%d", i), AgentID: int64(i%22 + 1), Type: "tool.catalog", Time: at, Data: data}
	}
	if err := store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	var catalogs, observations, count, bytesStored int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM tool_catalogs`).Scan(&catalogs); err != nil {
		t.Fatal(err)
	}
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM tool_catalog_agents`).Scan(&observations)
	_ = store.db.QueryRow(`SELECT COUNT(*),SUM(length(data)) FROM telemetry WHERE type='tool.catalog'`).Scan(&count, &bytesStored)
	if catalogs != 1 || observations != 22 || count != 6996 || bytesStored > 6996*400 {
		t.Fatalf("growth catalogs=%d agent_links=%d events=%d bytes=%d", catalogs, observations, count, bytesStored)
	}
	var hash string
	_ = store.db.QueryRow(`SELECT hash FROM tool_catalogs`).Scan(&hash)
	body, err := store.readToolCatalog(context.Background(), hash, 1)
	if err != nil || !bytes.Contains(body, []byte(`"tools"`)) {
		t.Fatalf("debug lookup missing: %s %v", body, err)
	}
	if _, err = store.readToolCatalog(context.Background(), hash, 99); err == nil {
		t.Fatal("catalog leaked to an agent without observation")
	}
	for _, event := range events {
		if bytes.Contains(event.Data, []byte(`"tools"`)) || !bytes.Contains(event.Data, []byte(hash)) {
			t.Fatal("full catalog retained in event")
		}
	}
	t.Logf("6996 refreshes across22agents: 1 catalog; %d bytes in reference events", bytesStored)
}

func TestCatalogStoreSemanticChangesAndCanonicalHash(t *testing.T) {
	store := newTestStore(t)
	hash, _, _, _, err := canonicalToolCatalog(legacyCatalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("shared canonical fixture hash=%s", hash)
	inputs := []string{string(legacyCatalogFixture), strings.Replace(string(legacyCatalogFixture), `"z":1`, `"z":2`, 1), strings.Replace(string(legacyCatalogFixture), `"auto"`, `"deferred"`, 1), strings.Replace(string(legacyCatalogFixture), `"z":"b"`, `"z":"a"`, 1)}
	for i, input := range inputs {
		if err = store.InsertTelemetry([]TelemetryEvent{{ID: fmt.Sprint(i), AgentID: 1, Type: "tool.catalog", Time: time.Now(), Data: json.RawMessage(input)}}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM tool_catalogs`).Scan(&count)
	if count != 4 {
		t.Fatalf("semantic changes lost: %d", count)
	}
}

func TestCatalogDebugLookupHonorsContentAndAgentScope(t *testing.T) {
	s, install := telemetryReadFixture(t)
	events := []TelemetryEvent{{ID: "catalog", AgentID: 41, Type: "tool.catalog", Time: time.Now(), Data: legacyCatalogFixture}}
	if err := s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	var d struct {
		Hash string `json:"catalog_hash"`
	}
	_ = json.Unmarshal(events[0].Data, &d)
	path := "/catalogs/" + d.Hash + "?agent_id=41"
	if r := telemetryReadRequest(s, install, path); r.Code != 403 {
		t.Fatalf("metadata observer read schemas: %d %s", r.Code, r.Body.String())
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.telemetry.read"]' WHERE id=?`, install); err != nil {
		t.Fatal(err)
	}
	if r := telemetryReadRequest(s, install, path); r.Code != 200 || !strings.Contains(r.Body.String(), `"description":"B"`) {
		t.Fatalf("authorized catalog read: %d %s", r.Code, r.Body.String())
	}
	if r := telemetryReadRequest(s, install, "/catalogs/"+d.Hash+"?agent_id=42"); r.Code == 200 {
		t.Fatal("foreign-project catalog read allowed")
	}
}

func TestCatalogLiveEventsAndRetentionStayBounded(t *testing.T) {
	s := newTestServer(t)
	at := time.Now().Add(-8 * 24 * time.Hour)
	events := []TelemetryEvent{{ID: "old", AgentID: 1, Type: "tool.catalog", Time: at, Data: legacyCatalogFixture}}
	if err := s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	live := []TelemetryEvent{{ID: "live", AgentID: 1, Type: "tool.catalog", Time: time.Now(), Data: legacyCatalogFixture}}
	raw, _ := json.Marshal(live)
	r := httptest.NewRequest("POST", "/telemetry/live", bytes.NewReader(raw))
	r.Header.Set("X-Agent-Secret", s.instanceSecret)
	w := httptest.NewRecorder()
	s.handleLiveTelemetry(w, r)
	if w.Code != 200 {
		t.Fatalf("live: %d %s", w.Code, w.Body.String())
	}
	replay, _ := s.broadcaster.ReplaySince(1)
	_ = replay
	s.broadcaster.mu.Lock()
	for i := 0; i < s.broadcaster.replaySize; i++ {
		if bytes.Contains(s.broadcaster.replay[i].Data, []byte(`"tools"`)) {
			t.Fatal("live replay retains full catalog")
		}
	}
	s.broadcaster.mu.Unlock()
	if _, err := s.store.CleanOldTelemetry(7 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM tool_catalogs`).Scan(&count)
	if count != 1 {
		t.Fatal("live-referenced catalog removed")
	}
	if _, err := s.store.WipeTelemetry(); err != nil {
		t.Fatal(err)
	}
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM tool_catalogs`).Scan(&count)
	if count != 0 {
		t.Fatal("wipe retained catalog content")
	}
}

func TestRestartAuditContainsAttributionWithoutValuesOrCredentials(t *testing.T) {
	s, install := telemetryReadFixture(t)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET local_pid=222,runtime_generation=4 WHERE id=?`, install); err != nil {
		t.Fatal(err)
	}
	before := map[string]any{"transcribe_model": "nova-3", "transcribe_language": "auto", "api_key": "old-secret"}
	after := map[string]any{"transcribe_model": "nova-2", "transcribe_language": "en", "api_key": "credential-value-never-log"}
	r := httptest.NewRequest("PUT", "/apps/installs/23/config", nil)
	r.Header.Set("X-User-ID", "17")
	r.Header.Set("X-Request-ID", "request-123")
	r.Header.Set("Authorization", "Bearer credential-value-never-log")
	r = r.WithContext(context.WithValue(r.Context(), appCallbackPrincipalKey{}, appCallbackPrincipal{installID: 99, userID: 17}))
	a := restartAuditForRequest(r, "configuration_changed", changedInstallSettingNames(before, after))
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	s.logAppRestart(install, a, 111, 3, errors.New("credential-value-never-log"))
	text := output.String()
	for _, value := range []string{"nova-2", "nova-3", "credential-value-never-log", "old-secret", "Bearer"} {
		if strings.Contains(text, value) {
			t.Fatalf("audit leaked values: %s", text)
		}
	}
	for _, want := range []string{`"old_pid":111`, `"new_pid":222`, `"initiating_user_id":17`, `"initiating_app_install_id":99`, `"request_id":"request-123"`, `"transcribe_model"`, `"status":"failed"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("audit missing %s: %s", want, text)
		}
	}
}

func TestCatalogReferencesRecoverAfterRetentionAndRemainWhileUsed(t *testing.T) {
	store := newTestStore(t)
	hash, _, _, _, err := canonicalToolCatalog(legacyCatalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	old := []TelemetryEvent{{ID: "snapshot", AgentID: 1, Type: "tool.catalog", Time: time.Now().Add(-8 * 24 * time.Hour), Data: legacyCatalogFixture}}
	if err = store.InsertTelemetry(old); err != nil {
		t.Fatal(err)
	}
	fresh := []TelemetryEvent{{ID: "request", AgentID: 1, Type: "tool.manifest", Time: time.Now(), Data: json.RawMessage(`{"catalog_hash":"` + hash + `","tools":[{"name":"b"}]}`)}}
	if err = store.InsertTelemetry(fresh); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CleanOldTelemetry(7 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = store.readToolCatalog(context.Background(), hash, 1); err != nil {
		t.Fatal("fresh manifest lost catalog to retention", err)
	}
	if _, err = store.WipeTelemetry(); err != nil {
		t.Fatal(err)
	}
	fresh[0].ID = "post-wipe"
	if err = store.InsertTelemetry(fresh); err != nil {
		t.Fatal(err)
	}
	missing := store.missingToolCatalogs(fresh)
	if len(missing) != 1 || missing[0] != hash {
		t.Fatalf("missing snapshot not requested: %v", missing)
	}
	recovered := []TelemetryEvent{{ID: "recovery", AgentID: 1, Type: "tool.catalog", Time: time.Now(), Data: legacyCatalogFixture}}
	if err = store.InsertTelemetry(recovered); err != nil {
		t.Fatal(err)
	}
	if len(store.missingToolCatalogs(fresh)) != 0 {
		t.Fatal("recovered catalog still missing")
	}
}

func TestCatalogMalformedEventDoesNotFailBatch(t *testing.T) {
	store := newTestStore(t)
	at := time.Now().UTC()
	events := []TelemetryEvent{
		{ID: "bad-ref", AgentID: 1, Type: "tool.catalog", Time: at, Data: json.RawMessage(`{"catalog_hash":"not-a-hash","catalog_reference":true}`)},
		{ID: "bad-tools", AgentID: 1, Type: "tool.catalog", Time: at, Data: json.RawMessage(`{"tools":[1,2]}`)},
		{ID: "ok", AgentID: 1, Type: "tool.catalog", Time: at, Data: legacyCatalogFixture},
	}
	if err := store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	var stored, catalogs int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM telemetry WHERE type='tool.catalog'`).Scan(&stored)
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM tool_catalogs`).Scan(&catalogs)
	if stored != 3 || catalogs != 1 {
		t.Fatalf("stored=%d catalogs=%d", stored, catalogs)
	}
}
