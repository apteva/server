package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Run against a freshly built Node, without real GPIO, provider calls or live accounts:
// APTEVA_NODE_TEST_BINARY=../node/bin/apteva-node go test -run TestDeviceRealNodeDaemon
func TestDeviceRealNodeDaemon(t *testing.T) {
	binary := os.Getenv("APTEVA_NODE_TEST_BINARY")
	if binary == "" {
		t.Skip("set APTEVA_NODE_TEST_BINARY for real-daemon integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	f, d, a := deviceAgentFixture(t)
	id, token := f.pair(t, "", "")
	d.ID = id
	var online atomic.Bool
	online.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/devices/link" && !online.Load() {
			http.Error(w, "test outage", 503)
			return
		}
		http.StripPrefix("/api", f.mux).ServeHTTP(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "node.yaml")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uiAddress := listener.Addr().String()
	listener.Close()
	config := fmt.Sprintf("name: integration_node\nstate_dir: %q\nserver:\n  url: %q\n  token: %q\nui:\n  listen: %q\ncapabilities:\n  - id: lamp\n    kind: relay\n    pin: 17\n    confirm: hold\n  - id: button\n    kind: button\n    pin: 23\n", filepath.Join(dir, "state"), server.URL, token, uiAddress)
	if err = os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var command *exec.Cmd
	stop := func() {
		if command == nil {
			return
		}
		_ = command.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			command.Process.Kill()
			<-done
			t.Error("Node failed graceful stop")
		}
		command = nil
	}
	start := func() {
		command = exec.Command(binary, "-config", configPath, "-sim")
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(stop)
	start()
	wait := func(label string, fn func() bool) {
		t.Helper()
		until := time.Now().Add(12 * time.Second)
		for time.Now().Before(until) {
			if fn() {
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
		t.Fatalf("timeout: %s", label)
	}
	snapshot := func() map[string]any {
		resp, err := http.Get("http://" + uiAddress + "/api/snapshot")
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		var data map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&data)
		return data
	}
	connected := func() bool { _, ok := f.s.deviceLinks.Load(id); return ok }
	wait("real Node connected", connected)
	d, err = f.s.store.accountDevice(id, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.saveDeviceGrant(d, a, []string{"lamp_set", "lamp_get"}); err != nil {
		t.Fatal(err)
	}
	path := deviceAuthorizedPath(t, f.s, d, a)
	listed := deviceMCPRequest(t, f.s, path, "tools/list", nil)
	if !strings.Contains(listed.Body.String(), "lamp_set") || strings.Contains(listed.Body.String(), "system_info") {
		t.Fatal("real Node discovery violated grant")
	}
	ui, _, err := websocket.DefaultDialer.Dial("ws://"+uiAddress+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ui.Close()
	var initial map[string]any
	_ = ui.ReadJSON(&initial)
	replies := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replies <- deviceMCPRequest(t, f.s, path, "tools/call", map[string]any{"name": "lamp_set", "arguments": map[string]bool{"on": true}})
	}()
	var approval string
	wait("local confirmation", func() bool {
		s := snapshot()
		list, _ := s["approvals"].([]any)
		if len(list) == 0 {
			return false
		}
		item, _ := list[0].(map[string]any)
		approval, _ = item["id"].(string)
		return approval != ""
	})
	if err = ui.WriteJSON(map[string]any{"type": "approve", "id": approval, "allow": true, "held": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-replies:
		if strings.Contains(result.Body.String(), `"isError":true`) {
			t.Fatal("approved hardware invocation failed", result.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MCP call did not complete")
	}
	deviceSubscribe(t, f, d, a, 0)
	online.Store(false)
	f.s.closeDeviceLink(id, "test outage")
	wait("link disconnected", func() bool { return !connected() })
	if err = ui.WriteJSON(map[string]any{"type": "press", "capability": "button", "down": true}); err != nil {
		t.Fatal(err)
	}
	wait("event saved before outage restart", func() bool { s := snapshot(); q, _ := s["queued"].(float64); return q == 1 })
	stop()
	ui.Close()
	start()
	wait("restarted daemon UI", func() bool { return snapshot() != nil })
	online.Store(true)
	wait("reconnected event receipt", func() bool {
		var n int
		_ = f.s.store.db.QueryRow("SELECT count(*) FROM device_event_receipts WHERE device_id=?", id).Scan(&n)
		return n == 1
	})
	wait("durable event acknowledged", func() bool { q, _ := snapshot()["queued"].(float64); return q == 0 })
	var eventID string
	_ = f.s.store.db.QueryRow("SELECT event_id FROM device_event_receipts WHERE device_id=?", id).Scan(&eventID)
	if eventID == "" {
		t.Fatal("real Node event missing stable identity")
	}
	// A fake Core verifies only transport and durable admission; no LLM is used.
	var coreEvents atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" || r.Header.Get("Authorization") != "Bearer integration-core" {
			http.Error(w, "invalid Core request", 403)
			return
		}
		var body struct {
			ID string `json:"event_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		coreEvents.Add(1)
		writeJSON(w, map[string]any{"events": map[string]any{"executions": map[string]string{body.ID: "exe_real_node"}}})
	}))
	defer core.Close()
	coreURL, _ := url.Parse(core.URL)
	_, portText, _ := net.SplitHostPort(coreURL.Host)
	port, _ := strconv.Atoi(portText)
	f.s.agents.mu.Lock()
	f.s.agents.processes[a.ID] = &runningAgent{port: port, coreAPIKey: "integration-core", reattached: true}
	f.s.agents.mu.Unlock()
	f.s.processDeviceEventBatch(t.Context())
	_, status := deviceDeliveryStatus(t, f.s, eventID)
	if status != "delivered" || coreEvents.Load() != 1 {
		t.Fatal("real Node event did not reach captured Core target", status)
	}
	// Replaying the disk event after a lost ACK must not fan out again.
	var raw string
	_ = f.s.store.db.QueryRow("SELECT event_json FROM device_event_receipts WHERE device_id=? AND event_id=?", id, eventID).Scan(&raw)
	if dup, err := f.s.persistDeviceEvent(d, deviceFrame{Type: "event", ID: eventID, Data: json.RawMessage(raw)}); err != nil || !dup {
		t.Fatal("lost ACK replay failed", err)
	}
	f.s.processDeviceEventBatch(t.Context())
	if coreEvents.Load() != 1 {
		t.Fatal("duplicate hardware event invoked Core again")
	}
	response := deviceTestRequest(t, f.mux, "DELETE", "/devices/"+id, f.key, nil)
	if response.Code != 200 {
		t.Fatal("revoke failed")
	}
	wait("real Node revoked to standalone", func() bool { link, _ := snapshot()["link"].(map[string]any); return link["state"] == "standalone" })
	if response = deviceMCPRequest(t, f.s, path, "tools/list", nil); response.Code != 403 {
		t.Fatal("revoked real device MCP still callable")
	}
}
