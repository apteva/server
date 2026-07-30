package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGetProviderPoolInjectsRealtimeBesideOpenAI(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	postJSON(t, s.handleRegister, map[string]string{"email": "voice@test.com", "password": "password123"})
	state, _ := json.Marshal(map[string]string{
		"OPENAI_API_KEY": "sk-test", "model_large": "gpt-5.2",
		"model_medium": "gpt-5.2", "model_small": "gpt-5-mini",
	})
	encrypted, err := Encrypt(s.secret, string(state))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(1, 0, "llm", "OpenAI", encrypted); err != nil {
		t.Fatal(err)
	}
	pool := s.GetProviderPool(1)
	found := false
	for _, provider := range pool {
		if provider.Type == "openai-realtime" {
			found = true
			if provider.ModelLarge != "gpt-realtime-2.1" || provider.ModelSmall != "gpt-realtime-2.1-mini" || provider.RealtimeVoice != "marin" {
				t.Fatalf("realtime provider = %#v", provider)
			}
		}
	}
	if !found {
		t.Fatalf("pool did not include openai-realtime: %#v", pool)
	}
}

func TestGetProviderPoolInjectsRealtimeBesideXAI(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	postJSON(t, s.handleRegister, map[string]string{"email": "xai-voice@test.com", "password": "password123"})
	state, _ := json.Marshal(map[string]string{
		"XAI_API_KEY": "xai-test", "model_large": "grok-4",
		"model_medium": "grok-4", "model_small": "grok-3-mini",
	})
	encrypted, err := Encrypt(s.secret, string(state))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(1, 0, "llm", "xAI", encrypted); err != nil {
		t.Fatal(err)
	}
	pool := s.GetProviderPool(1)
	for _, provider := range pool {
		if provider.Type == "xai-realtime" {
			if provider.ModelLarge != "grok-voice-latest" || provider.ModelSmall != "grok-voice-latest" || provider.RealtimeVoice != "eve" {
				t.Fatalf("xAI realtime provider = %#v", provider)
			}
			return
		}
	}
	t.Fatalf("pool did not include xai-realtime: %#v", pool)
}

func TestGetProviderPoolInjectsRealtimeBesideGoogle(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	postJSON(t, s.handleRegister, map[string]string{"email": "google-voice@test.com", "password": "password123"})
	state, _ := json.Marshal(map[string]string{
		"GOOGLE_API_KEY": "google-test", "model_large": "gemini-3.1-pro-preview",
		"model_medium": "gemini-3-flash-preview", "model_small": "gemini-3.1-flash-lite-preview",
	})
	encrypted, err := Encrypt(s.secret, string(state))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(1, 0, "llm", "Google", encrypted); err != nil {
		t.Fatal(err)
	}
	pool := s.GetProviderPool(1)
	for _, provider := range pool {
		if provider.Type == "google-realtime" {
			if provider.ModelLarge != "gemini-3.1-flash-live-preview" || provider.ModelSmall != "gemini-3.1-flash-live-preview" || provider.RealtimeVoice != "Kore" {
				t.Fatalf("Google realtime provider = %#v", provider)
			}
			return
		}
	}
	t.Fatalf("pool did not include google-realtime: %#v", pool)
}

func TestEnableRealtimeDefaultPreservesExplicitFalse(t *testing.T) {
	providers := []ProviderInfo{{Type: "xai-realtime"}}
	fresh := map[string]any{}
	enableRealtimeByDefault(fresh, providers)
	if fresh["realtime_enabled"] != true {
		t.Fatalf("fresh config = %#v", fresh)
	}
	disabled := map[string]any{"realtime_enabled": false}
	enableRealtimeByDefault(disabled, providers)
	if disabled["realtime_enabled"] != false {
		t.Fatalf("explicit false was overwritten: %#v", disabled)
	}
}

func TestRealtimeCompanionInheritsConfiguredProviderDefault(t *testing.T) {
	if !providerIsDefault("xai-realtime", "xai", 3) {
		t.Fatal("xAI realtime companion did not inherit the xAI default")
	}
	if providerIsDefault("openai-realtime", "xai", 2) {
		t.Fatal("unrelated realtime companion inherited the xAI default")
	}
	if !providerIsDefault("openai-realtime", "openai-realtime", 2) {
		t.Fatal("explicit realtime default was not honored")
	}
	if !providerIsDefault("google-realtime", "google", 4) {
		t.Fatal("Google realtime companion did not inherit the Google default")
	}
}

func TestPublicRealtimeAudioURLIsSecureAndEscaped(t *testing.T) {
	got, err := publicRealtimeAudioURL("https://agents.example/base?old=1", 42, "call/a b", "tok+secret")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "wss" || parsed.Host != "agents.example" || parsed.Path != "/api/realtime/audio" {
		t.Fatalf("URL = %s", got)
	}
	if parsed.Query().Get("agent_id") != "42" || parsed.Query().Get("thread") != "call/a b" || parsed.Query().Get("token") != "tok+secret" {
		t.Fatalf("query = %#v", parsed.Query())
	}
}

func TestCallbackReachableBaseURLUsesIncomingHostForLoopbackFallback(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://server.internal/api/apps/callback/threads/spawn-realtime", nil)
	request.Host = "server.internal:5280"
	if got := callbackReachableBaseURL("http://localhost:5280", request); got != "http://server.internal:5280" {
		t.Fatalf("reachable base = %q", got)
	}
	request.Header.Set("X-Forwarded-Proto", "https")
	if got := callbackReachableBaseURL("http://localhost:5280", request); got != "https://server.internal:5280" {
		t.Fatalf("forwarded base = %q", got)
	}
	if got := callbackReachableBaseURL("https://agents.example", request); got != "https://agents.example" {
		t.Fatalf("configured public URL changed = %q", got)
	}
}

func TestRealtimeAudioProxyAuthenticatesAndPreservesFrameTypes(t *testing.T) {
	authSeen := make(chan string, 1)
	querySeen := make(chan url.Values, 1)
	coreUpgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	coreServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen <- r.Header.Get("Authorization")
		querySeen <- r.URL.Query()
		conn, err := coreUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(messageType, payload); err != nil {
				return
			}
		}
	}))
	defer coreServer.Close()
	coreURL, _ := url.Parse(coreServer.URL)
	_, portText, _ := strings.Cut(coreURL.Host, ":")
	port, _ := strconv.Atoi(portText)

	s := newTestServer(t)
	s.agents.mu.Lock()
	s.agents.processes[42] = &runningAgent{port: port, coreAPIKey: "core-secret", reattached: true}
	s.agents.mu.Unlock()
	proxyServer := httptest.NewServer(http.HandlerFunc(s.handleRealtimeAudioProxy))
	defer proxyServer.Close()
	proxyURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/?agent_id=42&thread=voice-1&token=single-use"
	client, _, err := websocket.DefaultDialer.Dial(proxyURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetReadDeadline(time.Now().Add(2 * time.Second))

	if err := client.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := client.ReadMessage()
	if err != nil || typ != websocket.BinaryMessage || string(payload) != string([]byte{1, 2, 3}) {
		t.Fatalf("binary roundtrip type=%d payload=%v err=%v", typ, payload, err)
	}
	control := `{"type":"interrupt"}`
	if err := client.WriteMessage(websocket.TextMessage, []byte(control)); err != nil {
		t.Fatal(err)
	}
	typ, payload, err = client.ReadMessage()
	if err != nil || typ != websocket.TextMessage || string(payload) != control {
		t.Fatalf("text roundtrip type=%d payload=%q err=%v", typ, payload, err)
	}

	if got := <-authSeen; got != "Bearer core-secret" {
		t.Fatalf("core auth = %q", got)
	}
	query := <-querySeen
	if query.Get("thread") != "voice-1" || query.Get("token") != "single-use" {
		t.Fatalf("core query = %#v", query)
	}
}

func TestRealtimeAudioProxyPropagatesGracefulCoreClose(t *testing.T) {
	coreCloseSeen := make(chan *websocket.CloseError, 1)
	coreUpgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	coreServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coreUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "thread complete"),
			time.Now().Add(time.Second),
		); err != nil {
			return
		}
		_, _, err = conn.ReadMessage()
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			coreCloseSeen <- closeErr
		}
	}))
	defer coreServer.Close()
	coreURL, _ := url.Parse(coreServer.URL)
	_, portText, _ := strings.Cut(coreURL.Host, ":")
	port, _ := strconv.Atoi(portText)

	s := newTestServer(t)
	s.agents.mu.Lock()
	s.agents.processes[42] = &runningAgent{port: port, coreAPIKey: "core-secret", reattached: true}
	s.agents.mu.Unlock()
	proxyServer := httptest.NewServer(http.HandlerFunc(s.handleRealtimeAudioProxy))
	defer proxyServer.Close()
	proxyURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/?agent_id=42&thread=voice-close&token=single-use"
	client, _, err := websocket.DefaultDialer.Dial(proxyURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetReadDeadline(time.Now().Add(2 * time.Second))

	_, _, err = client.ReadMessage()
	var clientClose *websocket.CloseError
	if !errors.As(err, &clientClose) {
		t.Fatalf("client close error = %v", err)
	}
	if clientClose.Code != websocket.CloseNormalClosure || clientClose.Text != "thread complete" {
		t.Fatalf("client close = code %d reason %q", clientClose.Code, clientClose.Text)
	}
	select {
	case ack := <-coreCloseSeen:
		if ack.Code != websocket.CloseNormalClosure {
			t.Fatalf("core acknowledgement = code %d reason %q", ack.Code, ack.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("core did not receive a close acknowledgement")
	}
	closeEvent := waitForRealtimeProxyCloseEvent(t, s, 42, "voice-close")
	if closeEvent.InitiatedBy != "core" ||
		closeEvent.CloseCode != websocket.CloseNormalClosure ||
		closeEvent.CloseReason != "thread complete" ||
		closeEvent.TransportCategory != "websocket_close" ||
		closeEvent.RelayCategory != "" {
		t.Fatalf("close telemetry = %#v", closeEvent)
	}
}

func TestRealtimeAudioProxyPropagatesGracefulClientClose(t *testing.T) {
	coreCloseSeen := make(chan *websocket.CloseError, 1)
	coreUpgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	coreServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coreUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			coreCloseSeen <- closeErr
		}
	}))
	defer coreServer.Close()
	coreURL, _ := url.Parse(coreServer.URL)
	_, portText, _ := strings.Cut(coreURL.Host, ":")
	port, _ := strconv.Atoi(portText)

	s := newTestServer(t)
	s.agents.mu.Lock()
	s.agents.processes[42] = &runningAgent{port: port, coreAPIKey: "core-secret", reattached: true}
	s.agents.mu.Unlock()
	proxyServer := httptest.NewServer(http.HandlerFunc(s.handleRealtimeAudioProxy))
	defer proxyServer.Close()
	proxyURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/?agent_id=42&thread=voice-client-close&token=single-use"
	client, _, err := websocket.DefaultDialer.Dial(proxyURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseGoingAway, "caller finished"),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-coreCloseSeen:
		if got.Code != websocket.CloseGoingAway || got.Text != "caller finished" {
			t.Fatalf("core close = code %d reason %q", got.Code, got.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("core did not receive the client close")
	}
	closeEvent := waitForRealtimeProxyCloseEvent(t, s, 42, "voice-client-close")
	if closeEvent.InitiatedBy != "client" ||
		closeEvent.CloseCode != websocket.CloseGoingAway ||
		closeEvent.CloseReason != "caller finished" ||
		closeEvent.TransportCategory != "websocket_close" {
		t.Fatalf("close telemetry = %#v", closeEvent)
	}
}

func TestRealtimeProxyCloseDetailsPreservesPeerReason(t *testing.T) {
	code, reason := realtimeProxyCloseDetails(&websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: "secret detail"})
	if code != websocket.ClosePolicyViolation || reason != "secret detail" {
		t.Fatalf("close details = %d %q", code, reason)
	}
	code, reason = realtimeProxyCloseDetails(errors.New("broken pipe"))
	if code != websocket.CloseInternalServerErr || reason != "realtime bridge transport error" {
		t.Fatalf("transport close details = %d %q", code, reason)
	}
	code, reason = realtimeProxyCloseDetails(&websocket.CloseError{Code: websocket.CloseAbnormalClosure})
	if code != websocket.CloseInternalServerErr || reason != "realtime bridge transport error" {
		t.Fatalf("reserved close details = %d %q", code, reason)
	}
	longReason := strings.Repeat("é", 100)
	_, reason = realtimeProxyCloseDetails(&websocket.CloseError{Code: websocket.CloseNormalClosure, Text: longReason})
	if len(reason) > 123 || !strings.HasPrefix(longReason, reason) {
		t.Fatalf("bounded reason bytes=%d value=%q", len(reason), reason)
	}
}

func TestRealtimeProxyKeepaliveCorrelatesPingsAndPongs(t *testing.T) {
	keepalive := &realtimeProxyKeepalive{}
	now := time.Date(2026, time.July, 29, 12, 0, 0, 123_000_000, time.UTC)
	var payloads [][]byte
	write := func(destination string, payload []byte, deadline time.Time) error {
		if destination != "client" && destination != "core" {
			t.Fatalf("unexpected destination %q", destination)
		}
		if want := now.Add(realtimeProxyPingTimeout); !deadline.Equal(want) {
			t.Fatalf("deadline = %s, want %s", deadline, want)
		}
		payloads = append(payloads, append([]byte(nil), payload...))
		return nil
	}
	keepalive.sendPings(now, write, nil)
	if len(payloads) != 2 || string(payloads[0]) != string(payloads[1]) {
		t.Fatalf("first ping payloads = %q", payloads)
	}
	sequence, sentAtUnixMS, ok := parseRealtimeProxyPingPayload(string(payloads[0]))
	if !ok || sequence != 1 || sentAtUnixMS != now.UnixMilli() {
		t.Fatalf("first ping parsed as sequence=%d sent_at=%d ok=%v", sequence, sentAtUnixMS, ok)
	}

	now = now.Add(realtimeProxyPingInterval)
	payloads = nil
	keepalive.sendPings(now, write, nil)
	sequence, sentAtUnixMS, ok = parseRealtimeProxyPingPayload(string(payloads[0]))
	if !ok || sequence != 2 || sentAtUnixMS != now.UnixMilli() {
		t.Fatalf("second ping parsed as sequence=%d sent_at=%d ok=%v", sequence, sentAtUnixMS, ok)
	}

	clientPongAt := now.Add(15 * time.Millisecond)
	corePongAt := now.Add(25 * time.Millisecond)
	keepalive.recordPong("client", string(payloads[0]), clientPongAt)
	keepalive.recordPong("core", string(payloads[0]), corePongAt)
	keepalive.recordPong("client", "unrelated-provider-pong", now.Add(time.Second))
	snapshot := keepalive.snapshot()
	if snapshot.Client.LastPingSequence != 2 ||
		snapshot.Client.LastPongSequence != 2 ||
		snapshot.Client.LastPongUnixMS != clientPongAt.UnixMilli() {
		t.Fatalf("client keepalive = %#v", snapshot.Client)
	}
	if snapshot.Core.LastPingSequence != 2 ||
		snapshot.Core.LastPongSequence != 2 ||
		snapshot.Core.LastPongUnixMS != corePongAt.UnixMilli() {
		t.Fatalf("core keepalive = %#v", snapshot.Core)
	}
}

func TestRealtimeProxyKeepaliveRecordsPingFailuresPerDestination(t *testing.T) {
	keepalive := &realtimeProxyKeepalive{}
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	failures := make(map[string]uint64)
	keepalive.sendPings(
		now,
		func(destination string, _ []byte, _ time.Time) error {
			if destination == "client" {
				return errors.New("client write failed")
			}
			return nil
		},
		func(destination string, sequence uint64, err error) {
			if err == nil {
				t.Fatal("failure callback received nil error")
			}
			failures[destination] = sequence
		},
	)
	keepalive.sendPings(
		now.Add(realtimeProxyPingInterval),
		func(destination string, _ []byte, _ time.Time) error {
			if destination == "core" {
				return errors.New("core write failed")
			}
			return nil
		},
		func(destination string, sequence uint64, err error) {
			if err == nil {
				t.Fatal("failure callback received nil error")
			}
			failures[destination] = sequence
		},
	)

	snapshot := keepalive.snapshot()
	if snapshot.Client.PingFailures != 1 || snapshot.Core.PingFailures != 1 {
		t.Fatalf("keepalive failures = %#v", snapshot)
	}
	if failures["client"] != 1 || failures["core"] != 2 {
		t.Fatalf("failure callbacks = %#v", failures)
	}
}

func waitForRealtimeProxyCloseEvent(
	t *testing.T,
	s *Server,
	agentID int64,
	threadID string,
) realtimeProxyCloseTelemetry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := s.store.QueryTelemetry(agentID, "realtime.proxy.closed", time.Time{}, 10, threadID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) > 0 {
			if len(events) != 1 {
				t.Fatalf("close events = %d, want 1", len(events))
			}
			var data realtimeProxyCloseTelemetry
			if err := json.Unmarshal(events[0].Data, &data); err != nil {
				t.Fatalf("decode close telemetry: %v", err)
			}
			return data
		}
		if time.Now().After(deadline) {
			t.Fatalf("no realtime.proxy.closed telemetry for agent=%d thread=%q", agentID, threadID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
