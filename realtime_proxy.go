package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	realtimeProxyMaxMessageBytes = 1 << 20
	realtimeProxyPingInterval    = 30 * time.Second
	realtimeProxyPingTimeout     = 10 * time.Second
)

type realtimeProxyPeerKeepalive struct {
	LastPingSequence uint64 `json:"last_ping_sequence,omitempty"`
	LastPingUnixMS   int64  `json:"last_ping_unix_ms,omitempty"`
	LastPongSequence uint64 `json:"last_pong_sequence,omitempty"`
	LastPongUnixMS   int64  `json:"last_pong_unix_ms,omitempty"`
	PingFailures     uint64 `json:"ping_failures,omitempty"`
}

type realtimeProxyKeepalive struct {
	mu       sync.Mutex
	sequence uint64
	client   realtimeProxyPeerKeepalive
	core     realtimeProxyPeerKeepalive
}

type realtimeProxyKeepaliveSnapshot struct {
	Client realtimeProxyPeerKeepalive `json:"client"`
	Core   realtimeProxyPeerKeepalive `json:"core"`
}

func (k *realtimeProxyKeepalive) sendPings(
	now time.Time,
	write func(destination string, payload []byte, deadline time.Time) error,
	onFailure func(destination string, sequence uint64, err error),
) {
	k.mu.Lock()
	k.sequence++
	sequence := k.sequence
	sentAtUnixMS := now.UnixMilli()
	payload := []byte(fmt.Sprintf("rp:%d:%d", sequence, sentAtUnixMS))
	k.client.LastPingSequence = sequence
	k.client.LastPingUnixMS = sentAtUnixMS
	k.core.LastPingSequence = sequence
	k.core.LastPingUnixMS = sentAtUnixMS
	k.mu.Unlock()

	deadline := now.Add(realtimeProxyPingTimeout)
	for _, destination := range []string{"client", "core"} {
		if err := write(destination, payload, deadline); err != nil {
			k.mu.Lock()
			k.peer(destination).PingFailures++
			k.mu.Unlock()
			if onFailure != nil {
				onFailure(destination, sequence, err)
			}
		}
	}
}

func (k *realtimeProxyKeepalive) recordPong(destination, payload string, receivedAt time.Time) {
	sequence, _, ok := parseRealtimeProxyPingPayload(payload)
	if !ok {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	peer := k.peer(destination)
	if sequence > peer.LastPingSequence || sequence < peer.LastPongSequence {
		return
	}
	peer.LastPongSequence = sequence
	peer.LastPongUnixMS = receivedAt.UnixMilli()
}

func (k *realtimeProxyKeepalive) snapshot() realtimeProxyKeepaliveSnapshot {
	k.mu.Lock()
	defer k.mu.Unlock()
	return realtimeProxyKeepaliveSnapshot{Client: k.client, Core: k.core}
}

func (k *realtimeProxyKeepalive) peer(destination string) *realtimeProxyPeerKeepalive {
	if destination == "core" {
		return &k.core
	}
	return &k.client
}

func parseRealtimeProxyPingPayload(payload string) (sequence uint64, sentAtUnixMS int64, ok bool) {
	parts := strings.Split(payload, ":")
	if len(parts) != 3 || parts[0] != "rp" {
		return 0, 0, false
	}
	sequence, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || sequence == 0 {
		return 0, 0, false
	}
	sentAtUnixMS, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil || sentAtUnixMS <= 0 {
		return 0, 0, false
	}
	return sequence, sentAtUnixMS, true
}

type realtimeProxyCloseTelemetry struct {
	InitiatedBy       string                         `json:"initiated_by"`
	CloseCode         int                            `json:"close_code"`
	CloseReason       string                         `json:"close_reason,omitempty"`
	DurationMS        int64                          `json:"duration_ms"`
	TransportCategory string                         `json:"transport_category"`
	RelayCategory     string                         `json:"relay_category,omitempty"`
	Keepalive         realtimeProxyKeepaliveSnapshot `json:"keepalive"`
}

var realtimeProxyUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	// The single-use, thread-scoped core token is the authorization
	// capability. Telephony clients generally do not send an Origin header.
	CheckOrigin: func(*http.Request) bool { return true },
}

// handleRealtimeAudioProxy is intentionally not wrapped in user auth: a
// telephony carrier/sidecar cannot hold a dashboard session. It can only reach
// one core/thread using the short-lived single-use token minted by core.
func (s *Server) handleRealtimeAudioProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	agentID, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
	threadID := r.URL.Query().Get("thread")
	token := r.URL.Query().Get("token")
	if err != nil || agentID <= 0 || threadID == "" || token == "" || len(threadID) > 128 || len(token) > 256 {
		http.Error(w, "valid agent_id, thread and token required", http.StatusBadRequest)
		return
	}
	port := s.agents.GetPort(agentID)
	coreKey := s.agents.GetCoreAPIKey(agentID)
	if port <= 0 || coreKey == "" {
		http.Error(w, "agent is not running", http.StatusNotFound)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return
	}

	coreURL := url.URL{Scheme: "ws", Host: fmt.Sprintf("127.0.0.1:%d", port), Path: "/realtime/audio"}
	query := coreURL.Query()
	query.Set("thread", threadID)
	query.Set("token", token)
	coreURL.RawQuery = query.Encode()
	header := http.Header{"Authorization": []string{"Bearer " + coreKey}}
	coreConn, response, err := websocket.DefaultDialer.Dial(coreURL.String(), header)
	if err != nil {
		status := http.StatusBadGateway
		if response != nil && (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized) {
			status = http.StatusForbidden
		}
		http.Error(w, "audio bridge rejected", status)
		return
	}
	defer coreConn.Close()

	clientConn, err := realtimeProxyUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()
	startedAt := time.Now()
	clientConn.SetReadLimit(realtimeProxyMaxMessageBytes)
	coreConn.SetReadLimit(realtimeProxyMaxMessageBytes)
	refreshDeadline := func(conn *websocket.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	}
	keepalive := &realtimeProxyKeepalive{}
	clientConn.SetPongHandler(func(payload string) error {
		refreshDeadline(clientConn)
		keepalive.recordPong("client", payload, time.Now())
		return nil
	})
	coreConn.SetPongHandler(func(payload string) error {
		refreshDeadline(coreConn)
		keepalive.recordPong("core", payload, time.Now())
		return nil
	})
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(realtimeProxyPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case now := <-ticker.C:
				keepalive.sendPings(
					now,
					func(destination string, payload []byte, deadline time.Time) error {
						conn := clientConn
						if destination == "core" {
							conn = coreConn
						}
						return conn.WriteControl(websocket.PingMessage, payload, deadline)
					},
					func(destination string, sequence uint64, err error) {
						log.Printf(
							"[REALTIME-PROXY] agent=%d thread=%q ping_failed destination=%s sequence=%d elapsed=%s err=%v",
							agentID, threadID, destination, sequence, time.Since(startedAt).Round(time.Millisecond), err,
						)
					},
				)
			}
		}
	}()

	type proxyResult struct {
		err       error
		initiator string
		peer      *websocket.Conn
	}
	copyMessages := func(dst *websocket.Conn, dstSide string, src *websocket.Conn, srcSide string, done chan<- proxyResult) {
		for {
			_ = src.SetReadDeadline(time.Now().Add(2 * time.Minute))
			messageType, payload, err := src.ReadMessage()
			if err != nil {
				done <- proxyResult{err: err, initiator: srcSide, peer: dst}
				return
			}
			_ = dst.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := dst.WriteMessage(messageType, payload); err != nil {
				done <- proxyResult{err: err, initiator: dstSide, peer: src}
				return
			}
		}
	}
	done := make(chan proxyResult, 2)
	go copyMessages(coreConn, "core", clientConn, "client", done)
	go copyMessages(clientConn, "client", coreConn, "core", done)

	first := <-done
	close(pingDone)
	code, reason := realtimeProxyCloseDetails(first.err)
	relayErr := first.peer.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(time.Second),
	)
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	var peerClose *websocket.CloseError
	if errors.As(first.err, &peerClose) {
		// Close reasons can contain provider/customer detail. Forward them to
		// the peer, but keep logs limited to structured routing metadata.
		log.Printf("[REALTIME-PROXY] agent=%d thread=%q initiated_by=%s code=%d relay_err=%v",
			agentID, threadID, first.initiator, code, relayErr)
	} else {
		log.Printf("[REALTIME-PROXY] agent=%d thread=%q initiated_by=%s code=%d transport_err=%v relay_err=%v",
			agentID, threadID, first.initiator, code, first.err, relayErr)
	}
	s.recordRealtimeProxyClose(
		agentID,
		threadID,
		startedAt,
		time.Now(),
		first.initiator,
		code,
		reason,
		realtimeProxyErrorCategory(first.err),
		realtimeProxyErrorCategory(relayErr),
		keepalive.snapshot(),
	)
}

func (s *Server) recordRealtimeProxyClose(
	agentID int64,
	threadID string,
	startedAt time.Time,
	closedAt time.Time,
	initiatedBy string,
	code int,
	reason string,
	transportCategory string,
	relayCategory string,
	keepalive realtimeProxyKeepaliveSnapshot,
) {
	if s == nil || s.store == nil {
		return
	}
	duration := closedAt.Sub(startedAt)
	if duration < 0 {
		duration = 0
	}
	data, err := json.Marshal(realtimeProxyCloseTelemetry{
		InitiatedBy:       initiatedBy,
		CloseCode:         code,
		CloseReason:       boundedWebSocketCloseReason(reason),
		DurationMS:        duration.Milliseconds(),
		TransportCategory: transportCategory,
		RelayCategory:     relayCategory,
		Keepalive:         keepalive,
	})
	if err != nil {
		log.Printf("[REALTIME-PROXY] agent=%d thread=%q telemetry_encode_failed err=%v", agentID, threadID, err)
		return
	}
	event := TelemetryEvent{
		ID:       generateID(),
		AgentID:  agentID,
		ThreadID: threadID,
		Type:     "realtime.proxy.closed",
		Time:     closedAt.UTC(),
		Data:     data,
	}
	if err := s.store.InsertTelemetry([]TelemetryEvent{event}); err != nil {
		log.Printf("[REALTIME-PROXY] agent=%d thread=%q telemetry_persist_failed err=%v", agentID, threadID, err)
		return
	}
	if s.broadcaster != nil {
		s.broadcaster.Broadcast([]TelemetryEvent{event})
	}
}

func realtimeProxyErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return "websocket_close"
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	return "transport_error"
}

func realtimeProxyCloseDetails(err error) (int, string) {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code < 1000 || closeErr.Code == websocket.CloseNoStatusReceived ||
			closeErr.Code == websocket.CloseAbnormalClosure || closeErr.Code == websocket.CloseTLSHandshake {
			return websocket.CloseInternalServerErr, "realtime bridge transport error"
		}
		return closeErr.Code, boundedWebSocketCloseReason(closeErr.Text)
	}
	if err == nil {
		return websocket.CloseNormalClosure, ""
	}
	return websocket.CloseInternalServerErr, "realtime bridge transport error"
}

func boundedWebSocketCloseReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "")
	for len(reason) > 123 {
		_, size := utf8.DecodeLastRuneInString(reason)
		reason = reason[:len(reason)-size]
	}
	return reason
}

func publicRealtimeAudioURL(base string, agentID int64, threadID, token string) (string, error) {
	bridgeURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if bridgeURL.Scheme == "https" {
		bridgeURL.Scheme = "wss"
	} else if bridgeURL.Scheme == "http" {
		bridgeURL.Scheme = "ws"
	} else {
		return "", fmt.Errorf("unsupported public URL scheme %q", bridgeURL.Scheme)
	}
	bridgeURL.Path = "/api/realtime/audio"
	bridgeURL.RawQuery = ""
	query := bridgeURL.Query()
	query.Set("agent_id", strconv.FormatInt(agentID, 10))
	query.Set("thread", threadID)
	query.Set("token", token)
	bridgeURL.RawQuery = query.Encode()
	return bridgeURL.String(), nil
}

func callbackReachableBaseURL(configured string, request *http.Request) string {
	parsed, err := url.Parse(configured)
	if err != nil || request == nil || request.Host == "" {
		return configured
	}
	hostname := parsed.Hostname()
	if hostname != "localhost" && hostname != "127.0.0.1" && hostname != "::1" {
		return configured
	}
	return requestReachableBaseURL(request, configured)
}

// requestReachableBaseURL returns the server origin the caller already used.
// Runtime sidecars should keep their audio bridge on that reachable internal
// path instead of detouring through an unrelated public_url.
func requestReachableBaseURL(request *http.Request, fallback string) string {
	if request == nil || strings.TrimSpace(request.Host) == "" {
		return fallback
	}
	scheme := request.Header.Get("X-Forwarded-Proto")
	if comma := strings.IndexByte(scheme, ','); comma >= 0 {
		scheme = scheme[:comma]
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "http" && scheme != "https" {
		if request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + request.Host
}
