package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func deviceAgentFixture(t *testing.T) (deviceTestFixture, *AccountDevice, *Agent) {
	t.Helper()
	f := newDeviceTest(t)
	p, err := f.s.store.CreateProject(f.user.ID, "Device project", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.s.store.CreateAgent(f.user.ID, "Hardware agent", "", "autonomous", "{}", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := f.pair(t, "", "")
	_, err = f.s.store.db.Exec(`UPDATE account_devices SET tools=?,events=? WHERE id=?`, `[{"name":"lamp_set","description":"Switch lamp","input_schema":{"type":"object","properties":{"on":{"type":"boolean"}},"required":["on"],"additionalProperties":false},"confirm":"hold"},{"name":"lamp_get","description":"Read lamp","input_schema":{"type":"object"},"read_only":true}]`, `[{"capability":"button","kind":"pressed"}]`, id)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.s.store.accountDevice(id, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, d, a
}
func deviceMCPRequest(t *testing.T, s *Server, path, method string, params any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "call-1", "method": method, "params": params})
	r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:10001"
	w := httptest.NewRecorder()
	s.handleDeviceMCP(w, r)
	return w
}
func deviceAuthorizedPath(t *testing.T, s *Server, d *AccountDevice, a *Agent) string {
	t.Helper()
	u, _ := url.Parse("http://127.0.0.1/mcp/devices/" + d.ID)
	entry := map[string]any{}
	if err := s.authorizeDeviceMCPConfig(a, entry, u); err != nil {
		t.Fatal(err)
	}
	return entry["url"].(string)
}
func TestDeviceMCPGrantsScopeAndRevocation(t *testing.T) {
	f, d, a := deviceAgentFixture(t)
	u, _ := url.Parse("http://127.0.0.1/mcp/devices/" + d.ID)
	if err := f.s.authorizeDeviceMCPConfig(a, map[string]any{}, u); err == nil {
		t.Fatal("ungranted agent authorized")
	}
	if _, err := f.s.saveDeviceGrant(d, a, []string{"fabricated_tool"}); err == nil {
		t.Fatal("invented device tool granted")
	}
	if _, err := f.s.saveDeviceGrant(d, a, []string{"lamp_get"}); err != nil {
		t.Fatal(err)
	}
	path := deviceAuthorizedPath(t, f.s, d, a)
	w := deviceMCPRequest(t, f.s, path, "tools/list", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "lamp_get") || strings.Contains(w.Body.String(), "lamp_set") {
		t.Fatalf("tool scope: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"readOnlyHint":true`) {
		t.Fatal("missing hardware annotations")
	}
	w = deviceMCPRequest(t, f.s, path, "tools/call", map[string]any{"name": "lamp_set", "arguments": map[string]bool{"on": true}})
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("ungranted write accepted")
	}
	other, _ := f.s.store.CreateUser("outsider-hardware@test.com", "unused")
	foreign, _ := f.s.store.CreateProject(other.ID, "Foreign", "", "")
	target, _ := f.s.store.CreateAgent(other.ID, "Foreign", "", "autonomous", "{}", foreign.ID)
	if _, err := f.s.saveDeviceGrant(d, target, nil); err == nil {
		t.Fatal("cross-project grant accepted without authorization")
	}
	// Account-wide device grants can target another project the owner manages.
	second, _ := f.s.store.CreateProject(f.user.ID, "Second", "", "")
	another, _ := f.s.store.CreateAgent(f.user.ID, "Another", "", "autonomous", "{}", second.ID)
	if _, err := f.s.saveDeviceGrant(d, another, []string{"lamp_get"}); err != nil {
		t.Fatal("account-wide device could not be granted across owned projects", err)
	}
	_, _ = f.s.store.db.Exec("UPDATE agents SET project_id=? WHERE id=?", second.ID, a.ID)
	if w = deviceMCPRequest(t, f.s, path, "tools/list", nil); w.Code != 403 {
		t.Fatal("moving agent reused captured project grant")
	}
	_, _ = f.s.store.db.Exec("UPDATE agents SET project_id=? WHERE id=?", a.ProjectID, a.ID)
	if _, err := f.s.saveDeviceGrant(d, a, []string{"lamp_get"}); err != nil {
		t.Fatal(err)
	}
	if w = deviceMCPRequest(t, f.s, path, "tools/list", nil); w.Code != 403 {
		t.Fatal("old revision token remained valid")
	}
	path = deviceAuthorizedPath(t, f.s, d, a)
	_, _ = f.s.store.db.Exec("DELETE FROM device_tool_grants WHERE device_id=? AND agent_id=?", d.ID, a.ID)
	if w = deviceMCPRequest(t, f.s, path, "tools/list", nil); w.Code != 403 {
		t.Fatal("revoked tool grant accepted")
	}
}
func TestDeviceMCPInvocationAndCancellation(t *testing.T) {
	f, d, a := deviceAgentFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	// Recover the paired device credential through a fresh pairing for this live socket.
	id, token := f.pair(t, "", "")
	d.ID = id
	conn := deviceConnect(t, server.URL, token)
	d, _ = f.s.store.accountDevice(id, f.user.ID)
	if _, err := f.s.saveDeviceGrant(d, a, []string{"lamp_set"}); err != nil {
		t.Fatal(err)
	}
	path := deviceAuthorizedPath(t, f.s, d, a)
	reply := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		reply <- deviceMCPRequest(t, f.s, path, "tools/call", map[string]any{"name": "lamp_set", "arguments": map[string]bool{"on": true}})
	}()
	frame := deviceReadFrame(t, conn, "invoke")
	deviceWriteFrame(t, conn, "result", frame.ID, map[string]any{"ok": false, "denied": true, "error": "Human declined"})
	w := <-reply
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), "Human declined") {
		t.Fatalf("denial lost: %s", w.Body.String())
	}
	go func() {
		reply <- deviceMCPRequest(t, f.s, path, "tools/call", map[string]any{"name": "lamp_set", "arguments": map[string]bool{"on": true}})
	}()
	frame = deviceReadFrame(t, conn, "invoke")
	_, _ = f.s.store.db.Exec("DELETE FROM device_tool_grants WHERE device_id=? AND agent_id=?", d.ID, a.ID)
	f.s.cancelDeviceAgentInvocations(d.ID, a.ID)
	cancelFrame := deviceReadFrame(t, conn, "cancel_invoke")
	if cancelFrame.ID != frame.ID {
		t.Fatal("revocation canceled wrong invocation")
	}
	select {
	case w = <-reply:
		if !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatal("revocation did not fail invocation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not cancel request")
	}
	conn.Close()
}
func TestDeviceToolSchemaValidation(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"on":{"type":"boolean"}},"required":["on"],"additionalProperties":false}`)
	for _, raw := range []string{`{}`, `{"on":"yes"}`, `{"on":true,"other":1}`, `null`, `[]`} {
		if validateDeviceToolArguments(schema, json.RawMessage(raw)) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if err := validateDeviceToolArguments(schema, json.RawMessage(`{"on":true}`)); err != nil {
		t.Fatal(err)
	}
	if validateDeviceToolArguments(json.RawMessage(`{"type":"object","oneOf":[]}`), json.RawMessage(`{}`)) == nil {
		t.Fatal("unsupported assertion ignored")
	}
}
func deviceSubscribe(t *testing.T, f deviceTestFixture, d *AccountDevice, a *Agent, cooldown int) DeviceEventBinding {
	t.Helper()
	w := deviceTestRequest(t, f.mux, "POST", "/devices/"+d.ID+"/event-bindings", f.key, map[string]any{"agent_id": a.ID, "project_id": "forged", "capability": "button", "kind": "pressed", "thread_id": "main", "instruction": "Handle the button press", "enabled": true, "max_age_seconds": 300, "cooldown_seconds": cooldown})
	if w.Code != 200 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
	}
	var b DeviceEventBinding
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if b.ProjectID != a.ProjectID {
		t.Fatal("unchecked project trusted")
	}
	return b
}
func deviceEventFrame(id string, at time.Time) deviceFrame {
	raw, _ := json.Marshal(deviceEvent{ID: id, Capability: "button", Kind: "pressed", At: at})
	return deviceFrame{Type: "event", ID: id, Data: raw}
}
func deviceDeliveryStatus(t *testing.T, s *Server, event string) (int64, string) {
	t.Helper()
	var id int64
	var status string
	if err := s.store.db.QueryRow("SELECT id,status FROM device_event_outbox WHERE event_id=?", event).Scan(&id, &status); err != nil {
		t.Fatal(err)
	}
	return id, status
}
func TestDeviceEventDurabilityDedupAndCapturedTarget(t *testing.T) {
	f, d, a := deviceAgentFixture(t)
	b := deviceSubscribe(t, f, d, a, 0)
	frame := deviceEventFrame("stable-button", time.Now())
	dup, err := f.s.persistDeviceEvent(d, frame)
	if err != nil || dup {
		t.Fatal(dup, err)
	}
	dup, err = f.s.persistDeviceEvent(d, frame)
	if err != nil || !dup {
		t.Fatal("lost-ACK replay not deduplicated", dup, err)
	}
	conflicting := frame
	conflicting.Data = bytes.Replace(frame.Data, []byte("pressed"), []byte("other"), 1)
	if _, err = f.s.persistDeviceEvent(d, conflicting); err == nil {
		t.Fatal("ID payload conflict accepted")
	}
	var count int
	_ = f.s.store.db.QueryRow("SELECT count(*) FROM device_event_outbox").Scan(&count)
	if count != 1 {
		t.Fatal("duplicate fanout", count)
	}
	// Switching the device selection after ingestion cannot retarget the queued event.
	another, _ := f.s.store.CreateAgent(f.user.ID, "Other", "", "autonomous", "{}", a.ProjectID)
	_, _ = f.s.store.db.Exec("UPDATE account_devices SET agent_id=? WHERE id=?", strconv.FormatInt(another.ID, 10), d.ID)
	var snapshot string
	_ = f.s.store.db.QueryRow("SELECT binding_json FROM device_event_outbox").Scan(&snapshot)
	var captured DeviceEventBinding
	_ = json.Unmarshal([]byte(snapshot), &captured)
	if captured.AgentID != a.ID || captured.ID != b.ID {
		t.Fatal("selected agent redirected queued event")
	}
	id, _ := deviceDeliveryStatus(t, f.s, "stable-button")
	f.s.deliverDeviceEvent(context.Background(), id)
	_, status := deviceDeliveryStatus(t, f.s, "stable-button")
	if status != "pending" {
		t.Fatal("stopped agent event was lost", status)
	}
	w := deviceTestRequest(t, f.mux, "PATCH", "/devices/"+d.ID+"/event-bindings/"+b.ID, f.key, map[string]bool{"enabled": false})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	_, status = deviceDeliveryStatus(t, f.s, "stable-button")
	if status != "canceled" {
		t.Fatal("disabled subscription left event queued")
	}
}
func TestDeviceEventsDeliveryRetriesAndProjectRevocation(t *testing.T) {
	f, d, a := deviceAgentFixture(t)
	deviceSubscribe(t, f, d, a, 0)
	var mu sync.Mutex
	var ids []string
	attempt := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" || r.Header.Get("Authorization") != "Bearer node-core-key" {
			http.Error(w, "invalid route or credential", 403)
			return
		}
		var body struct {
			EventID  string `json:"event_id"`
			ThreadID string `json:"thread_id"`
			Track    bool   `json:"track_lifecycle"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ThreadID != "main" || !body.Track {
			http.Error(w, "invalid target", 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, body.EventID)
		attempt++
		if attempt == 1 {
			http.Error(w, "temporary failure", 503)
			return
		}
		writeJSON(w, map[string]any{"events": map[string]any{"executions": map[string]string{body.EventID: "exe_button"}, "accepted": []string{body.EventID}}})
	}))
	defer core.Close()
	host, _ := url.Parse(core.URL)
	_, portText, _ := net.SplitHostPort(host.Host)
	port, _ := strconv.Atoi(portText)
	f.s.agents.processes[a.ID] = &runningAgent{port: port, coreAPIKey: "node-core-key", reattached: true}
	if _, err := f.s.persistDeviceEvent(d, deviceEventFrame("retry-button", time.Now())); err != nil {
		t.Fatal(err)
	}
	id, _ := deviceDeliveryStatus(t, f.s, "retry-button")
	f.s.deliverDeviceEvent(context.Background(), id)
	_, status := deviceDeliveryStatus(t, f.s, "retry-button")
	if status != "pending" {
		t.Fatal("retry lost")
	}
	_, _ = f.s.store.db.Exec("UPDATE device_event_outbox SET next_attempt=0 WHERE id=?", id)
	f.s.deliverDeviceEvent(context.Background(), id)
	_, status = deviceDeliveryStatus(t, f.s, "retry-button")
	if status != "delivered" {
		t.Fatal(status)
	}
	mu.Lock()
	if len(ids) != 2 || ids[0] != ids[1] || !strings.Contains(ids[0], d.ID) {
		t.Fatal("retry changed Core deduplication ID", ids)
	}
	mu.Unlock()
	var execution string
	_ = f.s.store.db.QueryRow("SELECT execution_id FROM device_event_outbox WHERE id=?", id).Scan(&execution)
	if execution != "exe_button" {
		t.Fatal("durable receipt not retained")
	}
	if _, err := f.s.persistDeviceEvent(d, deviceEventFrame("revoked-project", time.Now())); err != nil {
		t.Fatal(err)
	}
	id, _ = deviceDeliveryStatus(t, f.s, "revoked-project")
	_, _ = f.s.store.db.Exec("UPDATE agents SET project_id='' WHERE id=?", a.ID)
	f.s.deliverDeviceEvent(context.Background(), id)
	_, status = deviceDeliveryStatus(t, f.s, "revoked-project")
	if status != "canceled" {
		t.Fatal("project move did not cancel pending event", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 {
		t.Fatal("revoked project event reached Core")
	}
}
func TestDeviceEventsCooldownExpiryAndSpoofing(t *testing.T) {
	f, d, a := deviceAgentFixture(t)
	deviceSubscribe(t, f, d, a, 60)
	for _, id := range []string{"first", "cooldown"} {
		if _, err := f.s.persistDeviceEvent(d, deviceEventFrame(id, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	_, status := deviceDeliveryStatus(t, f.s, "cooldown")
	if status != "suppressed" {
		t.Fatal("cooldown failed", status)
	}
	_, _ = f.s.store.db.Exec("UPDATE device_event_bindings SET last_queued=0 WHERE device_id=?", d.ID)
	if _, err := f.s.persistDeviceEvent(d, deviceEventFrame("too-old", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	_, status = deviceDeliveryStatus(t, f.s, "too-old")
	if status != "expired" {
		t.Fatal("expired event delivered")
	}
	foreign, _ := f.s.store.CreateUser("foreign-device-event@test.com", "unused")
	p, _ := f.s.store.CreateProject(foreign.ID, "Foreign", "", "")
	target, _ := f.s.store.CreateAgent(foreign.ID, "Foreign", "", "autonomous", "{}", p.ID)
	w := deviceTestRequest(t, f.mux, "POST", "/devices/"+d.ID+"/event-bindings", f.key, map[string]any{"agent_id": target.ID, "capability": "button", "kind": "pressed", "enabled": true})
	if w.Code != 403 {
		t.Fatalf("foreign project subscription: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.s.persistDeviceEvent(d, deviceEventFrame("", time.Now())); err == nil {
		t.Fatal("unstable event accepted")
	}
	if err := f.s.store.revokeAccountDevice(d.ID, d.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.persistDeviceEvent(d, deviceEventFrame("after-revoke", time.Now())); err == nil {
		t.Fatal("revoked device accepted")
	}
}

func TestDeviceGrantAPIUsesNormalAgentAttachments(t *testing.T) {
	f, d, a := deviceAgentFixture(t)
	w := deviceTestRequest(t, f.mux, "PUT", "/devices/"+d.ID+"/grants/"+strconv.FormatInt(a.ID, 10), f.key, map[string]any{"tools": []string{"lamp_get"}})
	if w.Code != 200 {
		t.Fatalf("grant/attach: %d %s", w.Code, w.Body.String())
	}
	latest, err := f.s.store.GetAgentByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := f.s.currentAgentMCPServers(latest, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(entries)
	if !strings.Contains(string(raw), "/mcp/devices/"+d.ID) || !strings.Contains(string(raw), "mcp_token") {
		t.Fatal("grant did not persist signed Core MCP config")
	}
	w = deviceTestRequest(t, f.mux, "DELETE", "/devices/"+d.ID+"/grants/"+strconv.FormatInt(a.ID, 10), f.key, nil)
	if w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	if _, _, _, err = f.s.authorizeDeviceToolGrant(d.ID, a.ID); err == nil {
		t.Fatal("revoked grant still active")
	}
	latest, _ = f.s.store.GetAgentByID(a.ID)
	entries, err = f.s.currentAgentMCPServers(latest, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(entries)
	if strings.Contains(string(raw), "/mcp/devices/"+d.ID) {
		t.Fatal("MCP attachment not removed")
	}
}

func TestDeviceOwnerCanRevokeGrantAfterProjectAccessLoss(t *testing.T) {
	f, d, a := deviceAgentFixture(t)
	if _, err := f.s.saveDeviceGrant(d, a, []string{"lamp_get"}); err != nil {
		t.Fatal(err)
	}
	other, _ := f.s.store.CreateUser("lost-access-device@test.com", "unused")
	p, _ := f.s.store.CreateProject(other.ID, "Private", "", "")
	_, _ = f.s.store.db.Exec("UPDATE agents SET project_id=? WHERE id=?", p.ID, a.ID)
	w := deviceTestRequest(t, f.mux, "DELETE", "/devices/"+d.ID+"/grants/"+strconv.FormatInt(a.ID, 10), f.key, nil)
	if w.Code != 200 {
		t.Fatalf("hardware owner could not revoke old grant: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.s.store.deviceToolGrant(d.ID, a.ID); err == nil {
		t.Fatal("old grant retained")
	}
}
