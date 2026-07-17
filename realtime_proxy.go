package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

const realtimeProxyMaxMessageBytes = 1 << 20

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
	clientConn.SetReadLimit(realtimeProxyMaxMessageBytes)
	coreConn.SetReadLimit(realtimeProxyMaxMessageBytes)
	refreshDeadline := func(conn *websocket.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	}
	clientConn.SetPongHandler(func(string) error { refreshDeadline(clientConn); return nil })
	coreConn.SetPongHandler(func(string) error { refreshDeadline(coreConn); return nil })
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case deadline := <-ticker.C:
				writeDeadline := deadline.Add(10 * time.Second)
				_ = clientConn.WriteControl(websocket.PingMessage, nil, writeDeadline)
				_ = coreConn.WriteControl(websocket.PingMessage, nil, writeDeadline)
			}
		}
	}()

	copyMessages := func(dst, src *websocket.Conn, done chan<- error) {
		for {
			_ = src.SetReadDeadline(time.Now().Add(2 * time.Minute))
			messageType, payload, err := src.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			_ = dst.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := dst.WriteMessage(messageType, payload); err != nil {
				done <- err
				return
			}
		}
	}
	done := make(chan error, 2)
	go copyMessages(coreConn, clientConn, done)
	go copyMessages(clientConn, coreConn, done)
	if err := <-done; err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		log.Printf("[REALTIME-PROXY] agent=%d thread=%q closed: %v", agentID, threadID, err)
	}
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
	scheme := request.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + request.Host
}
