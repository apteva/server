package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

const proxyCallIDHeader = "X-Apteva-Call-ID"

func boundedProxyCallID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

type websocketProxyMetadata struct {
	Proxy     string `json:"proxy"`
	App       string `json:"app,omitempty"`
	InstallID int64  `json:"install_id,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
}

type proxyDirectionTelemetry struct {
	Direction             string    `json:"direction"`
	Operation             string    `json:"operation"`
	EndedAt               time.Time `json:"ended_at"`
	Bytes                 int64     `json:"bytes"`
	Messages              int64     `json:"messages,omitempty"`
	Error                 string    `json:"error,omitempty"`
	ErrorType             string    `json:"error_type,omitempty"`
	Category              string    `json:"category"`
	CloseCode             int       `json:"close_code,omitempty"`
	AfterFirstTermination bool      `json:"after_first_termination"`
}

// Each upgraded connection gets one identity shared by its two copy loops.
// Only transport metadata is recorded: no frame payloads, URLs or auth tokens.
type websocketProxyTrace struct {
	server    *Server
	prefix    string
	id        string
	callID    string
	agentID   int64
	threadID  string
	metadata  websocketProxyMetadata
	startedAt time.Time
	opened    sync.Once
	active    atomic.Bool
	mu        sync.Mutex
	results   []proxyDirectionTelemetry
	done      chan struct{}
}

func newWebsocketProxyTrace(s *Server, r *http.Request, prefix string, agentID int64, threadID string, metadata websocketProxyMetadata) *websocketProxyTrace {
	callID := strings.TrimSpace(r.Header.Get(proxyCallIDHeader))
	if callID == "" {
		callID = strings.TrimSpace(r.URL.Query().Get("call_id"))
	}
	callID = boundedProxyCallID(callID)
	return &websocketProxyTrace{server: s, prefix: prefix, id: generateID(), callID: callID,
		agentID: agentID, threadID: threadID, metadata: metadata, startedAt: time.Now(), done: make(chan struct{}, 2)}
}

func (t *websocketProxyTrace) open() {
	t.opened.Do(func() {
		t.active.Store(true)
		t.emit("opened", time.Now(), map[string]any{})
	})
}

func proxyTransportError(err error) string {
	if err == nil {
		return ""
	}
	// url.Error includes the token-bearing handshake URL. Keep the exact
	// underlying transport error and operation without that URL.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Op + ": " + proxyTransportError(urlErr.Err)
	}
	return err.Error()
}

func (t *websocketProxyTrace) recordDirection(direction, operation string, n, messages int64, err error) proxyDirectionTelemetry {
	category := realtimeProxyErrorCategory(err)
	if err == nil || errors.Is(err, io.EOF) {
		category = "eof"
	}
	result := proxyDirectionTelemetry{Direction: direction, Operation: operation, EndedAt: time.Now().UTC(),
		Bytes: n, Messages: messages, Error: proxyTransportError(err), Category: category}
	if err != nil {
		result.ErrorType = fmt.Sprintf("%T", err)
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		result.CloseCode = closeErr.Code
	}
	var netErr *net.OpError
	if operation == "copy" && errors.As(err, &netErr) {
		result.Operation = netErr.Op
	}
	t.mu.Lock()
	result.AfterFirstTermination = len(t.results) > 0
	t.results = append(t.results, result)
	t.mu.Unlock()
	t.emit("direction.closed", result.EndedAt, map[string]any{"termination": result})
	t.done <- struct{}{}
	return result
}

func (t *websocketProxyTrace) directions() []proxyDirectionTelemetry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]proxyDirectionTelemetry(nil), t.results...)
}

func (t *websocketProxyTrace) emit(kind string, at time.Time, fields map[string]any) {
	fields["connection_id"] = t.id
	fields["call_id"] = t.callID
	fields["agent_id"] = t.agentID
	fields["thread_id"] = t.threadID
	fields["route"] = t.metadata
	fields["started_at"] = t.startedAt.UTC()
	fields["observed_at"] = at.UTC()
	fields["duration_ms"] = max(int64(0), at.Sub(t.startedAt).Milliseconds())
	data, err := json.Marshal(fields)
	if err != nil {
		log.Printf("[WS-PROXY] connection=%s telemetry_encode_error=%v", t.id, err)
		return
	}
	log.Printf("[WS-PROXY] event=%s.%s data=%s", t.prefix, kind, data)
	if t.server == nil || t.server.store == nil {
		return
	}
	event := TelemetryEvent{ID: generateID(), AgentID: t.agentID, ThreadID: t.threadID,
		Type: t.prefix + "." + kind, Time: at.UTC(), Data: data}
	if err := t.server.store.InsertTelemetry([]TelemetryEvent{event}); err != nil {
		log.Printf("[WS-PROXY] connection=%s telemetry_persist_error=%v", t.id, err)
		return
	}
	if t.server.broadcaster != nil {
		t.server.broadcaster.Broadcast([]TelemetryEvent{event})
	}
}

// WriteTo observes io.Copy's actual completion (including write errors),
// rather than guessing a termination from an individual socket read.
type observedProxyBody struct {
	io.ReadWriteCloser
	trace     *websocketProxyTrace
	direction string
}

func (c *observedProxyBody) WriteTo(dst io.Writer) (int64, error) {
	c.trace.open()
	n, err := io.Copy(dst, c.ReadWriteCloser)
	c.trace.recordDirection(c.direction, "copy", n, 0, err)
	return n, err
}

type observedProxyConn struct {
	net.Conn
	trace *websocketProxyTrace
}

func (c *observedProxyConn) WriteTo(dst io.Writer) (int64, error) {
	c.trace.open()
	n, err := io.Copy(dst, c.Conn)
	c.trace.recordDirection("client_to_backend", "copy", n, 0, err)
	return n, err
}

type observedHalfCloseConn struct{ *observedProxyConn }

func (c *observedHalfCloseConn) CloseWrite() error {
	err := c.Conn.(interface{ CloseWrite() error }).CloseWrite()
	if err != nil {
		c.trace.emit("control.error", time.Now(), map[string]any{"operation": "close_write", "destination": "client", "error": proxyTransportError(err)})
	}
	return err
}

type observedHalfCloseBody struct{ *observedProxyBody }

func (c *observedHalfCloseBody) CloseWrite() error {
	err := c.ReadWriteCloser.(interface{ CloseWrite() error }).CloseWrite()
	if err != nil {
		c.trace.emit("control.error", time.Now(), map[string]any{"operation": "close_write", "destination": "backend", "error": proxyTransportError(err)})
	}
	return err
}

type observedProxyWriter struct {
	http.ResponseWriter
	trace *websocketProxyTrace
	conn  net.Conn
}

func (w *observedProxyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedProxyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.conn = conn
	wrapped := &observedProxyConn{Conn: conn, trace: w.trace}
	if _, ok := conn.(interface{ CloseWrite() error }); ok {
		return &observedHalfCloseConn{wrapped}, buffered, nil
	}
	return wrapped, buffered, nil
}

func (s *Server) serveObservedWebsocketProxy(proxy *httputil.ReverseProxy, w http.ResponseWriter, r *http.Request, metadata websocketProxyMetadata) {
	if !requestIsProtocolUpgrade(r) || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		proxy.ServeHTTP(w, r)
		return
	}
	trace := newWebsocketProxyTrace(s, r, "proxy.websocket", 0, "", metadata)
	writer := &observedProxyWriter{ResponseWriter: w, trace: trace}
	var backend io.ReadWriteCloser
	modify := proxy.ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		if modify != nil {
			if err := modify(resp); err != nil {
				return err
			}
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			// Carrier clients cannot attach our call header. Telephony can
			// identify the authenticated call in its upgrade response instead.
			if callID := boundedProxyCallID(resp.Header.Get(proxyCallIDHeader)); callID != "" {
				trace.callID = callID
			}
			if body, ok := resp.Body.(io.ReadWriteCloser); ok {
				backend = body
				wrapped := &observedProxyBody{ReadWriteCloser: body, trace: trace, direction: "backend_to_client"}
				resp.Body = wrapped
				if _, ok := body.(interface{ CloseWrite() error }); ok {
					resp.Body = &observedHalfCloseBody{wrapped}
				}
			}
		} else {
			trace.emit("rejected", time.Now(), map[string]any{"stage": "backend_handshake", "status_code": resp.StatusCode})
		}
		return nil
	}
	onError := proxy.ErrorHandler
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		trace.emit("failed", time.Now(), map[string]any{"stage": "handshake", "error": proxyTransportError(err), "category": realtimeProxyErrorCategory(err)})
		if onError != nil {
			onError(w, r, err)
		} else {
			http.Error(w, "backend unreachable", http.StatusBadGateway)
		}
	}
	proxy.ServeHTTP(writer, r)
	if !trace.active.Load() {
		return
	}
	// ReverseProxy can return after the first copy fails. Close both sockets
	// and collect the other loop's actual error, including cleanup errors.
	if writer.conn != nil {
		_ = writer.conn.Close()
	}
	if backend != nil {
		_ = backend.Close()
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-trace.done:
		case <-timer.C:
			trace.emit("closed", time.Now(), map[string]any{"directions": trace.directions(), "directions_complete": false})
			return
		}
	}
	trace.emit("closed", time.Now(), map[string]any{"directions": trace.directions(), "directions_complete": true})
}
