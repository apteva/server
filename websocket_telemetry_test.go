package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func waitProxyTelemetry(t *testing.T, s *Server, agentID int64, eventType string) TelemetryEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := s.store.QueryTelemetry(agentID, eventType, time.Time{}, 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(events) > 0 {
			return events[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing %s", eventType)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWebsocketProxyTelemetryCapturesBothDirections(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(proxyCallIDHeader) != "reported-call" {
			t.Error("call correlation header was lost")
		}
		conn, err := upgrader.Upgrade(w, r, http.Header{proxyCallIDHeader: {"call-123"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close() // Abrupt transport termination after an echo.
		kind, payload, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(kind, payload)
		}
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	s := newTestServer(t)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serveObservedWebsocketProxy(httputil.NewSingleHostReverseProxy(target), w, r, websocketProxyMetadata{Proxy: "app", App: "telephony", InstallID: 123})
	}))
	defer front.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/media?token=secret-token", http.Header{proxyCallIDHeader: {"reported-call"}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := client.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	kind, payload, err := client.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(payload) != string([]byte{1, 2, 3}) {
		t.Fatalf("echo corrupted: %d %v %v", kind, payload, err)
	}
	_, _, _ = client.ReadMessage()
	_ = client.Close()
	event := waitProxyTelemetry(t, s, 0, "proxy.websocket.closed")
	var closed struct {
		ConnectionID string                    `json:"connection_id"`
		CallID       string                    `json:"call_id"`
		StartedAt    time.Time                 `json:"started_at"`
		ObservedAt   time.Time                 `json:"observed_at"`
		Complete     bool                      `json:"directions_complete"`
		Directions   []proxyDirectionTelemetry `json:"directions"`
	}
	if err := json.Unmarshal(event.Data, &closed); err != nil {
		t.Fatal(err)
	}
	if closed.CallID != "call-123" || closed.ConnectionID == "" || !closed.Complete || len(closed.Directions) != 2 {
		t.Fatalf("incomplete correlation or directions: %s", event.Data)
	}
	seen := map[string]bool{}
	for _, direction := range closed.Directions {
		seen[direction.Direction] = true
		if direction.Bytes < 3 || direction.EndedAt.Before(closed.StartedAt) || direction.EndedAt.After(closed.ObservedAt) {
			t.Fatalf("invalid byte count or timestamp: %#v", direction)
		}
	}
	if !seen["client_to_backend"] || !seen["backend_to_client"] {
		t.Fatalf("directions: %#v", seen)
	}
	opened := waitProxyTelemetry(t, s, 0, "proxy.websocket.opened")
	if !strings.Contains(string(opened.Data), closed.ConnectionID) || opened.Time.After(event.Time) || strings.Contains(string(event.Data), "secret-token") {
		t.Fatalf("bad lifecycle or leaked token: %s %s", opened.Data, event.Data)
	}
}

type proxyTestBody struct{ io.Reader }

func (*proxyTestBody) Write(p []byte) (int, error) { return len(p), nil }
func (*proxyTestBody) Close() error                { return nil }

type proxyErrorWriter struct{ err error }

func (w proxyErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWebsocketProxyTelemetryRetainsExactWriteFailure(t *testing.T) {
	s := newTestServer(t)
	trace := newWebsocketProxyTrace(s, httptest.NewRequest("GET", "/?call_id=call-456", nil), "proxy.websocket", 0, "", websocketProxyMetadata{Proxy: "test"})
	want := &net.OpError{Op: "write", Net: "tcp", Err: errors.New("connection reset by peer")}
	body := &observedProxyBody{ReadWriteCloser: &proxyTestBody{Reader: strings.NewReader("audio")}, trace: trace, direction: "backend_to_client"}
	if _, err := body.WriteTo(proxyErrorWriter{want}); !errors.Is(err, want) {
		t.Fatalf("write error changed: %v", err)
	}
	got := trace.directions()[0]
	if got.Operation != "write" || got.Error != want.Error() || got.Category != "transport_error" || got.Bytes != 0 {
		t.Fatalf("lost exact termination: %#v", got)
	}
}

func TestRealtimeProxyHandshakeFailureTelemetry(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "token rejected", http.StatusForbidden)
	}))
	defer core.Close()
	coreURL, _ := url.Parse(core.URL)
	addr, _ := net.ResolveTCPAddr("tcp", coreURL.Host)
	s := newTestServer(t)
	s.agents.processes[42] = &runningAgent{port: addr.Port, coreAPIKey: "core-secret", reattached: true}
	front := httptest.NewServer(http.HandlerFunc(s.handleRealtimeAudioProxy))
	defer front.Close()
	_, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/?agent_id=42&thread=tel-call-456&token=secret-token", http.Header{proxyCallIDHeader: {"call-456"}})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("handshake result: %v %v", response, err)
	}
	event := waitProxyTelemetry(t, s, 42, "realtime.proxy.failed")
	var data map[string]any
	_ = json.Unmarshal(event.Data, &data)
	if data["call_id"] != "call-456" || data["stage"] != "core_handshake" || data["backend_status_code"] != float64(403) || data["error"] == "" {
		t.Fatalf("missing handshake details: %s", event.Data)
	}
	if strings.Contains(string(event.Data), "secret-token") || strings.Contains(string(event.Data), "core-secret") {
		t.Fatalf("handshake telemetry leaked credentials: %s", event.Data)
	}
}
