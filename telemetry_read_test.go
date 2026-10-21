package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func telemetryReadFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	s, id := telemetryTestServer(t, "proj-1", "platform.telemetry.observe", "platform.instances.read")
	_, e := s.store.db.Exec(`INSERT OR IGNORE INTO projects(id,user_id,name) VALUES('proj-1',1,'One'),('proj-2',1,'Two');INSERT OR IGNORE INTO project_members(project_id,user_id,role) VALUES('proj-1',1,'owner'),('proj-2',1,'owner')`)
	if e != nil {
		t.Fatal(e)
	}
	return s, id
}
func telemetryReadRequest(s *Server, id int64, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/telemetry"+path, nil)
	r.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleTelemetryRead(w, r, id, strings.Split(path, "?")[0])
	return w
}
func TestTelemetryReadHistoryProjectionPaginationAndScope(t *testing.T) {
	s, id := telemetryReadFixture(t)
	at := time.Now().UTC()
	events := []TelemetryEvent{{ID: "a", AgentID: 41, ThreadID: "main", Type: "tool.call", Time: at, Data: json.RawMessage(`{"name":"crm_search","arguments":{"secret":"sensitive"},"thought":"private","duration_ms":3}`)}, {ID: "b", AgentID: 41, ThreadID: "main", Type: "llm.done", Time: at, Data: json.RawMessage(`{"message":"secret"}`)}, {ID: "foreign-project", AgentID: 42, Time: at, Type: "llm.done"}, {ID: "same-project", AgentID: 99, Time: at, Type: "llm.done"}}
	if e := s.store.InsertTelemetry(events); e != nil {
		t.Fatal(e)
	}
	first := telemetryReadRequest(s, id, "/history?agent_id=41&limit=1")
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	var page sdk.TelemetryPage
	json.Unmarshal(first.Body.Bytes(), &page)
	if len(page.Events) != 1 || page.Events[0].ID != "b" || page.NextCursor == "" {
		t.Fatalf("page: %+v", page)
	}
	// A concurrent insert cannot move the page boundary or duplicate a row.
	s.store.InsertTelemetry([]TelemetryEvent{{ID: "new", AgentID: 41, Time: at, Type: "llm.done"}})
	second := telemetryReadRequest(s, id, "/history?agent_id=41&limit=1&cursor="+url.QueryEscape(page.NextCursor))
	if second.Code != 200 {
		t.Fatal(second.Body.String())
	}
	json.Unmarshal(second.Body.Bytes(), &page)
	if len(page.Events) != 1 || page.Events[0].ID != "a" || page.Events[0].ServerID == "" {
		t.Fatalf("next page: %+v", page)
	}
	if strings.Contains(second.Body.String(), "sensitive") || strings.Contains(second.Body.String(), "private") || !strings.Contains(second.Body.String(), "crm_search") {
		t.Fatal("metadata projection", second.Body.String())
	}
	for _, path := range []string{"/history?agent_id=42", "/history?project_id=proj-2", "/history?projection=content"} {
		if r := telemetryReadRequest(s, id, path); r.Code == 200 {
			t.Fatal("unauthorized observation", path, r.Body.String())
		}
	}
	if r := telemetryReadRequest(s, id, "/history?agent_id=99"); r.Code != 200 {
		t.Fatal("approved project metadata should include project agents", r.Body.String())
	}
	if r := telemetryReadRequest(s, id, "/history?since=invalid"); r.Code != 400 {
		t.Fatal("invalid time accepted")
	}
	if r := telemetryReadRequest(s, id, "/history?events=tool"); r.Code != 200 || !strings.Contains(r.Body.String(), "crm_search") {
		t.Fatal("event prefix filter", r.Body.String())
	}
	s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.telemetry.read"]' WHERE id=?`, id)
	if r := telemetryReadRequest(s, id, "/history?projection=content&agent_id=99"); r.Code == 200 {
		t.Fatal("legacy raw permission broadened to other users")
	}
	if r := telemetryReadRequest(s, id, "/history?projection=content&agent_id=41"); r.Code != 200 || !strings.Contains(r.Body.String(), "sensitive") {
		t.Fatal("explicit raw read missing", r.Body.String())
	}
}
func TestTelemetryReadRuntimeObservationDoesNotGrantControl(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	owner := seedRuntimeAPIInstall(t, s, "owner", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	observer := seedRuntimeAPIInstall(t, s, "viewer", sdk.PermRuntimesObserve, sdk.PermTelemetryObserve)
	s.store.db.Exec(`UPDATE app_installs SET project_id='proj-1',status='running' WHERE id=?`, observer)
	created := runtimeAPIRequest(t, s, owner, "POST", "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "observe-me", TTLSeconds: 60})
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	listed := telemetryReadRequest(s, observer, "/runtimes")
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), "observe-me") {
		t.Fatal(listed.Body.String())
	}
	for _, secret := range []string{"owner_install_id", "control_endpoint", "api_key", "proxy_url", "data_dir", "\"port\"", "command"} {
		if strings.Contains(listed.Body.String(), secret) {
			t.Fatal("private runtime data leaked", secret)
		}
	}
	if r := runtimeAPIRequest(t, s, observer, "DELETE", "/apps/callback/runtimes/observe-me", nil); r.Code != 404 {
		t.Fatal("observer gained runtime control", r.Code)
	}
	s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.runtimes.read"]' WHERE id=?`, observer)
	if r := telemetryReadRequest(s, observer, "/runtimes/observe-me"); r.Code != 404 {
		t.Fatal("own-only permission broadened", r.Code)
	}
	s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.runtimes.observe"]' WHERE id=?`, observer)
	s.store.db.Exec(`DELETE FROM project_members WHERE project_id='proj-1' AND user_id=1`)
	if r := telemetryReadRequest(s, observer, "/runtimes"); r.Code != 403 {
		t.Fatal("revoked project membership remained effective", r.Code)
	}
}
func TestTelemetryReadReplayEpochAndOverflow(t *testing.T) {
	b := NewTelemetryBroadcaster()
	b.Broadcast([]TelemetryEvent{{ID: "original"}})
	_, epoch, start, reason := b.replayTelemetry("", 0)
	if start != 0 || reason != "" {
		t.Fatal("fresh replay")
	}
	replacement := NewTelemetryBroadcaster()
	replacement.Broadcast([]TelemetryEvent{{ID: "replacement"}})
	events, _, _, reason := replacement.replayTelemetry(epoch, 1)
	if reason != "server_restarted" || len(events) != 1 {
		t.Fatal("matching sequence hid restart", reason)
	}
	batch := make([]TelemetryEvent, telemetryReplayCap+2)
	b.Broadcast(batch)
	events, _, start, reason = b.replayTelemetry(epoch, 1)
	if reason != "retention_exceeded" || len(events) != telemetryReplayCap || start < 1 {
		t.Fatal("missing retention gap", reason, start, len(events))
	}
}
func TestTelemetryReadStreamBackfillAndRevocation(t *testing.T) {
	s, id := telemetryReadFixture(t)
	s.broadcaster = NewTelemetryBroadcaster()
	event := TelemetryEvent{ID: "retained", AgentID: 41, ThreadID: "main", Time: time.Now().UTC(), Type: "llm.done", Data: json.RawMessage(`{"message":"private"}`)}
	s.store.InsertTelemetry([]TelemetryEvent{event})
	s.broadcaster.Broadcast([]TelemetryEvent{event})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-User-ID", "1")
		s.handleTelemetryRead(w, r, id, "/stream")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"?format=delivery&events=llm.done&agent_id=41&backfill=true", nil)
	response, e := http.DefaultClient.Do(request)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	history, live, revoked := false, false, false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var d sdk.TelemetryDelivery
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) != nil {
			t.Fatal(line)
		}
		if d.Kind == "history" {
			history = d.History != nil && len(d.History.Events) == 1
		}
		if d.Kind == "event" {
			live = d.Event != nil && d.Event.ID == event.ID
			if strings.Contains(line, "private") {
				t.Fatal("stream leaked content")
			}
			s.store.db.Exec(`UPDATE app_installs SET permissions_json='[]' WHERE id=?`, id)
		}
		if d.Kind == "error" {
			revoked = true
			break
		}
	}
	if !history || !live || !revoked {
		t.Fatalf("history=%v live=%v revocation=%v err=%v", history, live, revoked, scanner.Err())
	}
}
func TestTelemetryReadAPICallbackParityAndReadOnlyKeys(t *testing.T) {
	s, id := telemetryReadFixture(t)
	s.store.InsertTelemetry([]TelemetryEvent{{ID: "one", AgentID: 41, Type: "llm.done", Time: time.Now().UTC()}})
	app := telemetryReadRequest(s, id, "/history?agent_id=41")
	api := telemetryReadRequest(s, 0, "/history?project_id=proj-1&agent_id=41")
	if app.Code != 200 || api.Code != 200 || app.Body.String() != api.Body.String() {
		t.Fatalf("app %s API %s", app.Body.String(), api.Body.String())
	}
	if !readOnlyAPIRequestAllowed(httptest.NewRequest("GET", "/telemetry", nil)) || readOnlyAPIRequestAllowed(httptest.NewRequest("POST", "/telemetry/runtimes/observe-me", nil)) {
		t.Fatal("read-only key routing")
	}
	// The old dashboard endpoint keeps its array shape and content projection.
	r := httptest.NewRequest("GET", "/history?agent_id=41&type=llm", nil)
	r.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleQueryTelemetry(rec, r)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "[") {
		t.Fatal("legacy history compatibility", rec.Code, rec.Body.String())
	}
	_ = fmt.Sprint(id)
}

func TestTelemetryReadTopologyIsContentFreeAndReportsUnknownTools(t *testing.T) {
	s, id := telemetryReadFixture(t)
	// A saved MCP attachment is read as metadata only; its credentials and URL
	// must never be copied into the observation response.
	config := `{"threads":[{"id":"worker","parent_id":"main","directive":"private-thread"}],"mcp_servers":[{"name":"research","url":"https://private-control.invalid/?key=secret","headers":{"Authorization":"secret"},"allowed_tools":["search"]}]}`
	s.store.db.Exec(`UPDATE agents SET config=? WHERE id=41`, config)
	s.agents = NewAgentManager(t.TempDir(), "unused-core")
	response := telemetryReadRequest(s, id, "/agents/41/topology")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, bad := range []string{"private-thread", "private-control", "Authorization", "secret", "url"} {
		if strings.Contains(response.Body.String(), bad) {
			t.Fatal("topology leaked private config", response.Body.String())
		}
	}
	var topology sdk.AgentTopology
	if e := json.Unmarshal(response.Body.Bytes(), &topology); e != nil {
		t.Fatal(e)
	}
	if len(topology.Threads) != 2 || topology.Threads[1].ParentID != "main" || len(topology.Attachments) != 1 || fmt.Sprint(topology.Attachments[0].Tools) != "[search]" {
		t.Fatalf("topology: %+v", topology)
	}
	inventory := telemetryReadRequest(s, id, "/agents")
	if inventory.Code != 200 || !strings.Contains(inventory.Body.String(), `"id":41`) || strings.Contains(inventory.Body.String(), "directive") {
		t.Fatal("agent inventory", inventory.Body.String())
	}
}
func TestTelemetryReadHistoryLargeContentIsExplicitlyBounded(t *testing.T) {
	s, id := telemetryReadFixture(t)
	s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.telemetry.read"]' WHERE id=?`, id)
	raw, _ := json.Marshal(map[string]any{"message": strings.Repeat("secret-", 50000)})
	s.store.InsertTelemetry([]TelemetryEvent{{ID: "large", AgentID: 41, Type: "llm.done", Time: time.Now().UTC(), Data: raw}})
	response := telemetryReadRequest(s, id, "/history?agent_id=41&projection=content")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "content_omitted") || len(response.Body.Bytes()) > 4096 {
		t.Fatal("large content response not bounded", response.Code, response.Body.Len())
	}
}

func TestTelemetryReadPageByteLimitDoesNotSkipEvents(t *testing.T) {
	s, id := telemetryReadFixture(t)
	s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.telemetry.read"]' WHERE id=?`, id)
	raw, _ := json.Marshal(map[string]any{"message": strings.Repeat("x", 200<<10)})
	events := []TelemetryEvent{}
	for i := 0; i < 12; i++ {
		events = append(events, TelemetryEvent{ID: fmt.Sprintf("large-%02d", i), AgentID: 41, Type: "llm.done", Time: time.Now().UTC(), Data: raw})
	}
	s.store.InsertTelemetry(events)
	seen := map[string]bool{}
	cursor := ""
	for pageCount := 0; pageCount < 3; pageCount++ {
		response := telemetryReadRequest(s, id, "/history?agent_id=41&projection=content&limit=500&cursor="+url.QueryEscape(cursor))
		if response.Code != 200 || response.Body.Len() > 2<<20 {
			t.Fatal("unbounded history page", response.Code, response.Body.Len())
		}
		var page sdk.TelemetryPage
		json.Unmarshal(response.Body.Bytes(), &page)
		for _, event := range page.Events {
			if seen[event.ID] {
				t.Fatal("duplicate across byte-limited pages")
			}
			seen[event.ID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 12 {
		t.Fatal("events skipped by byte limit", len(seen))
	}
}

func TestTelemetryReadMetadataEnforcesFieldTypes(t *testing.T) {
	s := &Server{instanceSecret: "fixture"}
	event := s.projectTelemetryEvent(TelemetryEvent{Type: "tool.call", Data: json.RawMessage(`{"name":"search","duration_ms":"private-text","success":"private-text","target_agent_id":9007199254740993,"arguments":{"private":"data"}}`)}, telemetryAgent{}, "metadata")
	metadata, e := event.Metadata()
	if e != nil || metadata.ToolName != "search" || metadata.TargetAgentID != 9007199254740993 || strings.Contains(string(event.Data), "private") {
		t.Fatal("metadata types or identity precision", string(event.Data), e)
	}
}

func TestTelemetryReadStreamDoesNotFollowInstallationProjectMove(t *testing.T) {
	s, id := telemetryReadFixture(t)
	s.broadcaster = NewTelemetryBroadcaster()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-User-ID", "1")
		s.handleTelemetryRead(w, r, id, "/stream")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"?format=delivery&events=llm.done", nil)
	response, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	moved, closed := false, false
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame sdk.TelemetryDelivery
		json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame)
		if frame.Kind == "ready" && !moved {
			s.store.db.Exec(`UPDATE app_installs SET project_id='proj-2' WHERE id=?`, id)
			moved = true
		}
		if frame.Kind == "error" {
			closed = true
			break
		}
	}
	if !moved || !closed {
		t.Fatal("stream silently switched to another project", scanner.Err())
	}
}

// The public SDK consumes history and live delivery through the original API
// endpoints. The app callback exposes those same capabilities at its old SSE URL.
func TestTelemetryReadUnifiedSDKAndCallbackRoutes(t *testing.T) {
	s, id := telemetryReadFixture(t)
	s.broadcaster = NewTelemetryBroadcaster()
	retained := TelemetryEvent{ID: "stored", AgentID: 41, ThreadID: "chat-one", Type: "llm.done", Time: time.Now().UTC(), Data: json.RawMessage(`{"message":"private"}`)}
	if err := s.store.InsertTelemetry([]TelemetryEvent{retained}); err != nil {
		t.Fatal(err)
	}
	s.broadcaster.Broadcast([]TelemetryEvent{retained})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-User-ID", "1")
		switch r.URL.Path {
		case "/api/telemetry":
			s.handleQueryTelemetry(w, r)
		case "/api/telemetry/stream":
			s.handleTelemetryStream(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	client, err := sdk.NewTelemetryClient(server.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	query := sdk.TelemetryQuery{ProjectID: "proj-1", AgentID: 41, Events: []string{"llm.done"}, ThreadPrefix: "chat-"}
	page, err := client.QueryTelemetry(ctx, query)
	if err != nil || len(page.Events) != 1 || strings.Contains(string(page.Events[0].Data), "private") {
		t.Fatal("SDK history", page, err)
	}
	deliveries, err := client.WatchTelemetry(ctx, sdk.TelemetryWatch{Query: query, Backfill: true})
	if err != nil {
		t.Fatal(err)
	}
	history, live := false, false
	for delivery := range deliveries {
		if delivery.Kind == "history" {
			history = delivery.History != nil && len(delivery.History.Events) == 1
			s.broadcaster.Broadcast([]TelemetryEvent{{ID: "live", AgentID: 41, ThreadID: "chat-one", Type: "llm.done", Time: time.Now().UTC()}})
		}
		if delivery.Event != nil {
			if delivery.Event.ID == "stored" {
				t.Fatal("history/live overlap was not deduplicated")
			}
			if delivery.Event.ID == "live" {
				live = true
				break
			}
		}
	}
	if !history || !live {
		t.Fatal("unified SDK stream", history, live, ctx.Err())
	}
	cancel()
	response := runtimeAPIRequest(t, s, id, "GET", "/apps/callback/telemetry/history?agent_id=41", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"id":"stored"`) {
		t.Fatal("callback history", response.Code, response.Body.String())
	}
	for _, path := range []string{"/apps/callback/observations/telemetry", "/apps/callback/telemetry/stream"} {
		if response := runtimeAPIRequest(t, s, id, "GET", path, nil); response.Code != 404 {
			t.Fatal("parallel route remains", path, response.Code)
		}
	}
	if readOnlyAPIRequestAllowed(httptest.NewRequest("GET", "/observations/telemetry/stream", nil)) {
		t.Fatal("removed observation route is still allowlisted")
	}
}

func TestTelemetryReadLegacyStreamRevokesProjectAccess(t *testing.T) {
	s, id := telemetryReadFixture(t)
	// An administrator deliberately retains access without project membership.
	if _, err := s.store.db.Exec(`UPDATE users SET role='user' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.telemetry.read"]' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	flushes := 0
	recorder := &telemetryFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	recorder.onFlush = func() {
		flushes++
		if flushes == 1 {
			if _, err := s.store.db.Exec(`DELETE FROM project_members WHERE project_id='proj-1' AND user_id=1`); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.handleCallbackTelemetry(recorder, telemetryRequest(ctx, "?events=llm.done"), id)
	if ctx.Err() != nil {
		t.Fatal("legacy stream did not close after access was revoked")
	}
	if len(collectSSEEvents(t, recorder.Body.String())) != 0 {
		t.Fatal("legacy stream received a non-event control frame")
	}
}
