package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

const proxyCallIDHeader = "X-Apteva-Call-ID"
const proxyConnectionIDHeader = "X-Apteva-Proxy-Connection-ID"
const proxyRouteCategoryHeader = "X-Apteva-Proxy-Route-Category"

var proxyProcessStart = time.Now()
var proxyProcessID = generateID()
var proxyEventSequence atomic.Uint64
var proxyDiagnosticURL = regexp.MustCompile(`(?i)(?:https?|wss?)://[^\s<>"']+`)

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
	Direction             string              `json:"direction"`
	Operation             string              `json:"operation"`
	EndedAt               time.Time           `json:"ended_at"`
	Bytes                 int64               `json:"bytes"`
	Messages              int64               `json:"messages,omitempty"`
	Error                 string              `json:"error,omitempty"`
	ErrorType             string              `json:"error_type,omitempty"`
	Category              string              `json:"category"`
	CloseCode             int                 `json:"close_code,omitempty"`
	EventID               string              `json:"event_id,omitempty"`
	EventSequence         uint64              `json:"event_sequence"`
	MonotonicNS           int64               `json:"monotonic_ns"`
	SourceLeg             string              `json:"source_leg"`
	DestinationLeg        string              `json:"destination_leg"`
	LocalCloseRequested   bool                `json:"local_close_requested"`
	LocalCloseEventID     string              `json:"local_close_event_id,omitempty"`
	ErrorChain            []proxyErrorDetail  `json:"error_chain,omitempty"`
	Activity              [2]proxyLegActivity `json:"activity"`
	AfterFirstTermination bool                `json:"after_first_termination"`
}

// Each upgraded connection gets one identity shared by its two copy loops.
// Only transport metadata is recorded: no frame payloads, URLs or auth tokens.
type websocketProxyTrace struct {
	server           *Server
	prefix           string
	id               string
	callID           string
	agentID          int64
	threadID         string
	metadata         websocketProxyMetadata
	startedAt        time.Time
	opened           sync.Once
	active           atomic.Bool
	mu               sync.Mutex
	results          []proxyDirectionTelemetry
	done             chan struct{}
	routeCategory    string
	first            *proxyDirectionTelemetry
	activity         [2]proxyLegActivity
	localClose       map[string]string
	closeCauseIDs    map[string]string
	handshakeEventID string
	closeReason      string
	closers          map[string]io.Closer
	seenErrors       map[string]bool
	closed           bool
	history          []proxyForwardingSnapshot
	incidentAt       time.Time
	lastSample       time.Time
	writeIntervalMax [2]int64
	requestContext   context.Context
	previousActivity [2]proxyLegActivity
}

func newWebsocketProxyTrace(s *Server, r *http.Request, prefix string, agentID int64, threadID string, metadata websocketProxyMetadata) *websocketProxyTrace {
	callID := strings.TrimSpace(r.Header.Get(proxyCallIDHeader))
	if callID == "" {
		callID = strings.TrimSpace(r.URL.Query().Get("call_id"))
	}
	callID = boundedProxyCallID(callID)
	// An app backend, not a public correlation hint, authenticates app call IDs.
	if prefix == "proxy.websocket" {
		callID = ""
	}
	return &websocketProxyTrace{server: s, prefix: prefix, id: generateID(), callID: callID,
		agentID: agentID, threadID: threadID, metadata: metadata, startedAt: time.Now(), done: make(chan struct{}, 2), routeCategory: "diagnostic", requestContext: r.Context(), localClose: make(map[string]string), closers: make(map[string]io.Closer), seenErrors: make(map[string]bool)}
}

func (t *websocketProxyTrace) open() {
	t.opened.Do(func() {
		t.active.Store(true)
		if t.server != nil {
			t.server.proxyTelemetry().track(t)
		}
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
	return proxyDiagnosticURL.ReplaceAllString(err.Error(), "[redacted URL]")
}

func (t *websocketProxyTrace) recordDirection(direction, operation string, n, messages int64, err error) proxyDirectionTelemetry {
	// Fallback for callers without direct instrumentation; instrumented copies
	// have already captured the first error before io.Copy returns.
	leg := "client"
	if strings.HasPrefix(direction, "backend") || strings.HasPrefix(direction, "core") {
		leg = "backend"
	}
	t.mu.Lock()
	missing := t.first == nil
	t.mu.Unlock()
	if missing {
		terminal := err
		if terminal == nil {
			terminal = io.EOF
		}
		t.observeIO(leg, operation, 0, terminal, 0)
	}
	at := time.Now()
	result := proxyDirectionTelemetry{Direction: direction, Operation: operation, EndedAt: at.UTC(),
		Bytes: n, Messages: messages, Error: proxyTransportError(err), Category: realtimeProxyErrorCategory(err)}
	if err == nil || errors.Is(err, io.EOF) {
		result.Category = "eof"
	}
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
	t.emitLocked("direction.closed", at, map[string]any{"termination": result})
	t.mu.Unlock()
	t.done <- struct{}{}
	return result
}

func (t *websocketProxyTrace) directions() []proxyDirectionTelemetry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]proxyDirectionTelemetry(nil), t.results...)
}

func (t *websocketProxyTrace) emit(kind string, at time.Time, fields map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emitLocked(kind, at, fields)
}

func (t *websocketProxyTrace) emitLocked(kind string, at time.Time, fields map[string]any) string {
	id, _ := fields["event_id"].(string)
	if id == "" {
		id = generateID()
	}
	if kind == "failed" || kind == "rejected" {
		t.handshakeEventID = id
	}
	fields["connection_id"] = t.id // compatibility with existing consumers
	fields["proxy_connection_id"] = t.id
	fields["call_id"] = t.callID
	fields["agent_id"] = t.agentID
	fields["thread_id"] = t.threadID
	fields["route"] = t.metadata
	fields["route_category"] = t.routeCategory
	fields["byte_unit"] = "transport_bytes"
	if t.prefix == "realtime.proxy" {
		fields["byte_unit"] = "message_payload_bytes"
	}
	fields["started_at"] = t.startedAt.UTC()
	fields["observed_at"] = at.UTC()
	fields["duration_ms"] = max(int64(0), at.Sub(t.startedAt).Milliseconds())
	fields["process_id"] = proxyProcessID
	fields["monotonic_ns"] = at.Sub(proxyProcessStart).Nanoseconds()
	if _, exists := fields["event_sequence"]; !exists {
		fields["event_sequence"] = proxyEventSequence.Add(1)
	}
	fields["event_id"] = id
	if kind == "closed" {
		t.closed = true
		fields["first_observed_termination"] = t.first
		fields["activity"] = t.activity
	}
	if t.server != nil {
		t.server.proxyTelemetry().enqueue(proxyTelemetryRecord{id: id, trace: t, kind: t.prefix + "." + kind, at: at, fields: fields})
	}
	return id
}

// Suppress WriterTo/ReaderFrom fast paths inside the copy so every transport
// read and write (including TLS and buffered upgrade bytes) is observed.
type proxyReaderOnly struct{ io.Reader }
type proxyWriterOnly struct{ io.Writer }

// WriteTo also records final copy totals, independently of direct I/O errors.
type observedProxyBody struct {
	io.ReadWriteCloser
	trace     *websocketProxyTrace
	direction string
}

func (c *observedProxyBody) WriteTo(dst io.Writer) (int64, error) {
	c.trace.open()
	n, err := io.Copy(proxyWriterOnly{dst}, proxyReaderOnly{c})
	c.trace.recordDirection(c.direction, "copy", n, 0, err)
	return n, err
}

type observedProxyConn struct {
	net.Conn
	trace  *websocketProxyTrace
	reader io.Reader
}

func (c *observedProxyConn) WriteTo(dst io.Writer) (int64, error) {
	c.trace.open()
	n, err := io.Copy(proxyWriterOnly{dst}, proxyReaderOnly{c})
	c.trace.recordDirection("client_to_backend", "copy", n, 0, err)
	return n, err
}

func (c *observedProxyBody) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := c.ReadWriteCloser.Read(p)
	if c.trace.active.Load() {
		c.trace.observeIO("backend", "read", n, err, time.Since(start))
	}
	return n, err
}
func (c *observedProxyBody) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := c.ReadWriteCloser.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if c.trace.active.Load() {
		c.trace.observeIO("backend", "write", n, err, time.Since(start))
	}
	return n, err
}
func (c *observedProxyBody) Close() error {
	c.trace.requestClose("backend", "close", "", "")
	return c.ReadWriteCloser.Close()
}
func (c *observedProxyConn) Read(p []byte) (int, error) {
	start := time.Now()
	reader := c.reader
	if reader == nil {
		reader = c.Conn
	}
	n, err := reader.Read(p)
	if c.trace.active.Load() {
		c.trace.observeIO("client", "read", n, err, time.Since(start))
	}
	return n, err
}
func (c *observedProxyConn) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := c.Conn.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if c.trace.active.Load() || err != nil {
		observedN := n
		if !c.trace.active.Load() {
			observedN = 0
		}
		c.trace.observeIO("client", "write", observedN, err, time.Since(start))
	}
	return n, err
}
func (c *observedProxyConn) Close() error {
	c.trace.requestClose("client", "close", "", "")
	return c.Conn.Close()
}

type observedHalfCloseConn struct{ *observedProxyConn }

func (c *observedHalfCloseConn) CloseWrite() error {
	c.trace.requestClose("client", "close_write", "", "")
	err := c.Conn.(interface{ CloseWrite() error }).CloseWrite()
	if err != nil {
		c.trace.emit("control.error", time.Now(), map[string]any{"operation": "close_write", "destination": "client", "error": proxyTransportError(err)})
	}
	return err
}

type observedHalfCloseBody struct{ *observedProxyBody }

func (c *observedHalfCloseBody) CloseWrite() error {
	c.trace.requestClose("backend", "close_write", "", "")
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
	wrapped := &observedProxyConn{Conn: conn, trace: w.trace, reader: buffered.Reader}
	var observed net.Conn = wrapped
	if _, ok := conn.(interface{ CloseWrite() error }); ok {
		observed = &observedHalfCloseConn{wrapped}
	}
	w.conn = observed
	w.trace.setCloser("client", observed)
	// ReverseProxy versions differ in whether they copy from conn or brw.Reader.
	// Both must drain the original buffer, then pass through our direct reader.
	return observed, bufio.NewReadWriter(bufio.NewReader(observed), bufio.NewWriter(observed)), nil
}

func (s *Server) serveObservedWebsocketProxy(proxy *httputil.ReverseProxy, w http.ResponseWriter, r *http.Request, metadata websocketProxyMetadata) {
	if !requestIsProtocolUpgrade(r) || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		proxy.ServeHTTP(w, r)
		return
	}
	trace := newWebsocketProxyTrace(s, r, "proxy.websocket", 0, "", metadata)
	// Hooks are per-request even when the caller reuses a ReverseProxy.
	copyProxy := *proxy
	proxy = &copyProxy
	if director := proxy.Director; director != nil {
		proxy.Director = func(out *http.Request) {
			director(out)
			out.Header.Set(proxyConnectionIDHeader, trace.id)
			trace.setRoute(out.URL.Path)
		}
	}
	if rewrite := proxy.Rewrite; rewrite != nil {
		proxy.Rewrite = func(out *httputil.ProxyRequest) {
			rewrite(out)
			out.Out.Header.Set(proxyConnectionIDHeader, trace.id)
			trace.setRoute(out.Out.URL.Path)
		}
	}
	w.Header().Set(proxyConnectionIDHeader, trace.id)
	stopWatch := trace.watch(r)
	defer stopWatch()
	writer := &observedProxyWriter{ResponseWriter: w, trace: trace}
	modify := proxy.ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Read backend correlation before all status handling, including 409.
		trace.adoptResponse(resp)
		resp.Header.Set(proxyConnectionIDHeader, trace.id)
		if resp.StatusCode == http.StatusSwitchingProtocols {
			if body, ok := resp.Body.(io.ReadWriteCloser); ok {
				wrapped := &observedProxyBody{ReadWriteCloser: body, trace: trace, direction: "backend_to_client"}
				resp.Body = wrapped
				if _, ok := body.(interface{ CloseWrite() error }); ok {
					resp.Body = &observedHalfCloseBody{wrapped}
				}
				trace.setCloser("backend", resp.Body.(io.ReadWriteCloser))
			}
		} else {
			trace.emit("rejected", time.Now(), map[string]any{"stage": "backend_handshake", "status_code": resp.StatusCode})
		}
		if modify != nil {
			if err := modify(resp); err != nil {
				return err
			}
		}
		resp.Header.Set(proxyConnectionIDHeader, trace.id)
		return nil
	}
	onError := proxy.ErrorHandler
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		trace.emit("failed", time.Now(), map[string]any{"stage": "handshake", "error": proxyTransportError(err), "error_type": fmt.Sprintf("%T", err), "category": realtimeProxyErrorCategory(err)})
		if onError != nil {
			onError(w, r, err)
		} else {
			http.Error(w, "backend unreachable", http.StatusBadGateway)
		}
	}
	proxy.ServeHTTP(writer, r)
	trace.closeConnections("")
	if !trace.active.Load() {
		trace.emit("closed", time.Now(), map[string]any{"directions_complete": false, "copy_not_started": true})
		return
	}
	// Collect the other copy loop after explicitly closing both legs.
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
