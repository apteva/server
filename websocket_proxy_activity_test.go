package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebsocketProxyRejectedHandshakeCorrelation(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprint(rewrite), func(t *testing.T) {
			s := newTestServer(t)
			received := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Get(proxyConnectionIDHeader)
				w.Header().Set(proxyCallIDHeader, "validated-call")
				w.Header().Set(proxyRouteCategoryHeader, "carrier_media")
				w.Header().Set(proxyConnectionIDHeader, "backend-spoof")
				http.Error(w, "already connected", 409)
			}))
			defer backend.Close()
			target, _ := url.Parse(backend.URL)
			proxy := httputil.NewSingleHostReverseProxy(target)
			if rewrite {
				proxy.Director = nil
				proxy.Rewrite = func(r *httputil.ProxyRequest) { r.SetURL(target) }
			}
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.serveObservedWebsocketProxy(proxy, w, r, websocketProxyMetadata{Proxy: "test"})
			}))
			defer front.Close()
			_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/media/secret-path?token=secret-query", http.Header{proxyConnectionIDHeader: {"client-spoof"}, proxyCallIDHeader: {"client-call"}})
			if err == nil || resp == nil || resp.StatusCode != 409 {
				t.Fatalf("expected rejection: %v %v", resp, err)
			}
			defer resp.Body.Close()
			id := <-received
			if id == "" || id == "client-spoof" || resp.Header.Get(proxyConnectionIDHeader) != id {
				t.Fatalf("spoofed/lost connection identity %q %v", id, resp.Header)
			}
			event := waitProxyTelemetry(t, s, 0, "proxy.websocket.rejected")
			var fields map[string]any
			json.Unmarshal(event.Data, &fields)
			if fields["call_id"] != "validated-call" || fields["proxy_connection_id"] != id || fields["route_category"] != "carrier_media" {
				t.Fatalf("missing response correlation: %s", event.Data)
			}
			if strings.Contains(string(event.Data), "secret-") || strings.Contains(string(event.Data), "client-call") {
				t.Fatalf("unsafe telemetry %s", event.Data)
			}
			waitProxyTelemetry(t, s, 0, "proxy.websocket.closed")
		})
	}
	trace := newWebsocketProxyTrace(nil, httptest.NewRequest("GET", "/?call_id=spoof", nil), "proxy.websocket", 0, "", websocketProxyMetadata{})
	if trace.callID != "" {
		t.Fatal("untrusted request call ID adopted")
	}
}

type faultProxyTransport struct {
	net.Conn
	err error
}

func (c *faultProxyTransport) Read([]byte) (int, error)  { return 0, c.err }
func (c *faultProxyTransport) Write([]byte) (int, error) { return 0, c.err }
func (*faultProxyTransport) Close() error                { return nil }

func TestWebsocketProxyFirstObservationAndCleanup(t *testing.T) {
	for _, leg := range []string{"client", "backend"} {
		for _, op := range []string{"read", "write"} {
			t.Run(leg+"_"+op, func(t *testing.T) {
				trace := newWebsocketProxyTrace(nil, httptest.NewRequest("GET", "/", nil), "proxy.websocket", 0, "", websocketProxyMetadata{})
				trace.open()
				want := &net.OpError{Op: op, Net: "tcp", Err: syscall.ECONNRESET}
				raw := &faultProxyTransport{err: want}
				var transport io.ReadWriteCloser = &observedProxyConn{Conn: raw, trace: trace}
				if leg == "backend" {
					transport = &observedProxyBody{ReadWriteCloser: raw, trace: trace}
				}
				var err error
				if op == "read" {
					_, err = transport.Read(make([]byte, 8))
				} else {
					_, err = transport.Write([]byte("audio"))
				}
				if !errors.Is(err, syscall.ECONNRESET) {
					t.Fatal(err)
				}
				first := *trace.first
				if first.Operation != op || (first.SourceLeg != leg && first.DestinationLeg != leg) || first.LocalCloseRequested || len(first.ErrorChain) != 2 || first.EventID == "" || first.EventSequence == 0 || first.MonotonicNS < 0 {
					t.Fatalf("lost first cause: %#v", first)
				}
				trace.requestClose("client", "close", "other_leg_terminated", first.EventID)
				trace.observeIO("backend", "cleanup_read", 0, net.ErrClosed, 0)
				if trace.first.EventID != first.EventID || trace.first.Error != first.Error || trace.first.LocalCloseRequested {
					t.Fatalf("first changed: %#v", trace.first)
				}
				if trace.localClose["client:close"] == "" {
					t.Fatal("close intent missing")
				}
			})
		}
	}
}

type optimizedProxyBody struct{ proxyTestBody }

func (*optimizedProxyBody) WriteTo(io.Writer) (int64, error) {
	panic("unobserved WriterTo optimization")
}

type optimizedProxyWriter struct{ strings.Builder }

func (*optimizedProxyWriter) ReadFrom(io.Reader) (int64, error) {
	panic("unobserved ReaderFrom optimization")
}
func TestWebsocketProxyCopyCannotBypassObservations(t *testing.T) {
	trace := newWebsocketProxyTrace(nil, httptest.NewRequest("GET", "/", nil), "proxy.websocket", 0, "", websocketProxyMetadata{})
	body := &observedProxyBody{ReadWriteCloser: &optimizedProxyBody{proxyTestBody{strings.NewReader("audio")}}, trace: trace, direction: "backend_to_client"}
	dst := &optimizedProxyWriter{}
	if n, err := body.WriteTo(dst); n != 5 || err != nil || dst.String() != "audio" {
		t.Fatalf("copy %d %v", n, err)
	}
	if trace.activity[1].ReadBytes != 5 || trace.first.Operation != "read" || trace.first.Category != "eof" {
		t.Fatalf("read not observed: %#v", trace.first)
	}
}

func TestWebsocketProxyTLSAndBufferedUpgrade(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		t.Run(fmt.Sprint(tlsMode), func(t *testing.T) {
			s := newTestServer(t)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := realtimeProxyUpgrader.Upgrade(w, r, http.Header{proxyCallIDHeader: {"tls-call"}})
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				kind, payload, err := c.ReadMessage()
				if err == nil {
					err = c.WriteMessage(kind, payload)
				}
				if err != nil {
					t.Error(err)
				}
			})
			var backend *httptest.Server
			if tlsMode {
				backend = httptest.NewTLSServer(handler)
			} else {
				backend = httptest.NewServer(handler)
			}
			defer backend.Close()
			target, _ := url.Parse(backend.URL)
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.Transport = backend.Client().Transport
			fh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.serveObservedWebsocketProxy(proxy, w, r, websocketProxyMetadata{Proxy: "test"})
			})
			var front *httptest.Server
			if tlsMode {
				front = httptest.NewTLSServer(fh)
			} else {
				front = httptest.NewServer(fh)
			}
			defer front.Close()
			if tlsMode {
				dialer := *websocket.DefaultDialer
				dialer.TLSClientConfig = front.Client().Transport.(*http.Transport).TLSClientConfig
				c, _, err := dialer.Dial("wss"+strings.TrimPrefix(front.URL, "https")+"/media", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(2 * time.Second))
				if err = c.WriteMessage(websocket.BinaryMessage, []byte("abc")); err != nil {
					t.Fatal(err)
				}
				_, payload, err := c.ReadMessage()
				if err != nil || string(payload) != "abc" {
					t.Fatalf("TLS echo %q %v", payload, err)
				}
				c.ReadMessage()
				c.Close()
			} else {
				// Put the first masked WebSocket frame in the SAME write as the HTTP
				// upgrade: net/http may buffer it before handing over the connection.
				addr := strings.TrimPrefix(front.URL, "http://")
				c, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				upgrade := "GET /media HTTP/1.1\r\nHost: " + addr + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
				frame := []byte{0x82, 0x83, 1, 2, 3, 4, 'a' ^ 1, 'b' ^ 2, 'c' ^ 3}
				if _, err = c.Write(append([]byte(upgrade), frame...)); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(c)
				resp, err := http.ReadResponse(reader, nil)
				if err != nil || resp.StatusCode != 101 {
					t.Fatalf("upgrade %v %v", resp, err)
				}
				echoed := make([]byte, 5)
				if _, err = io.ReadFull(reader, echoed); err != nil || string(echoed[2:]) != "abc" {
					t.Fatalf("buffered frame lost %v %v", echoed, err)
				}
				c.Close()
			}
			closed := waitProxyTelemetry(t, s, 0, "proxy.websocket.closed")
			var fields struct {
				First    *proxyDirectionTelemetry `json:"first_observed_termination"`
				Activity [2]proxyLegActivity      `json:"activity"`
			}
			json.Unmarshal(closed.Data, &fields)
			if fields.First == nil || fields.First.Operation != "read" || fields.Activity[0].ReadBytes < 9 || fields.Activity[1].WriteBytes < 9 || fields.Activity[1].ReadBytes < 5 || fields.Activity[0].WriteBytes < 5 {
				t.Fatalf("transport coverage missing: %s", closed.Data)
			}
		})
	}
}

func TestWebsocketProxyHistoryWindow(t *testing.T) {
	trace := newWebsocketProxyTrace(nil, httptest.NewRequest("GET", "/", nil), "proxy.websocket", 0, "", websocketProxyMetadata{})
	trace.active.Store(true)
	base := time.Now()
	for i := -100; i <= 0; i++ {
		trace.observeIO("client", "read", 2, nil, 0)
		trace.observeIO("backend", "write", 2, nil, 10*time.Millisecond)
		if trace.sample(base.Add(time.Duration(i)*time.Second), false) {
			t.Fatal("premature finalization")
		}
	}
	trace.observeIO("client", "read", 0, io.EOF, 0)
	trace.incidentAt = base
	for i := 1; i < 30; i++ {
		if trace.sample(base.Add(time.Duration(i)*time.Second), false) {
			t.Fatal("post window too short")
		}
	}
	if len(trace.history) != 90 || trace.history[0].At.Before(base.Add(-60*time.Second)) || trace.history[0].Activity[1].WriteBytes == 0 {
		t.Fatalf("history not bounded: %d", len(trace.history))
	}
	if !trace.sample(base.Add(30*time.Second), false) || len(trace.history) != 0 {
		t.Fatal("post window not finalized")
	}
}

func TestWebsocketProxyAsyncCollectorPressureAndRecovery(t *testing.T) {
	s := newTestServer(t)
	c := &proxyTelemetryCollector{server: s, queue: make(chan proxyTelemetryRecord, 4), shutdown: make(chan struct{}), stop: make(chan struct{}), sampled: make(chan struct{}), finished: make(chan struct{}), histories: make(map[*websocketProxyTrace]bool)}
	c.accepting.Store(true)
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	var attempts atomic.Int64
	c.persist = func(ctx context.Context, events []TelemetryEvent) error {
		if attempts.Add(1) == 1 {
			entered <- struct{}{}
			select {
			case <-blocked:
			case <-ctx.Done():
			}
			return errors.New("test persistence outage")
		}
		return s.store.insertProxyTelemetry(ctx, events)
	}
	trace := newWebsocketProxyTrace(nil, httptest.NewRequest("GET", "/", nil), "proxy.websocket", 0, "", websocketProxyMetadata{})
	record := proxyTelemetryRecord{id: generateID(), trace: trace, kind: "proxy.websocket.test", at: time.Now(), fields: map[string]any{"test": true}}
	go c.run()
	go c.sampler()
	c.enqueue(record)
	<-entered
	start := time.Now()
	for i := 0; i < 1000; i++ {
		c.enqueue(record)
	}
	if time.Since(start) > 100*time.Millisecond || c.dropped.Load() == 0 {
		t.Fatal("forwarding enqueue blocked or failed to record drops")
	}
	close(blocked)
	loss := waitProxyTelemetry(t, s, 0, "proxy.telemetry.loss")
	var fields map[string]any
	json.Unmarshal(loss.Data, &fields)
	if fields["dropped_events"].(float64) == 0 || fields["persistence_failures"].(float64) == 0 {
		t.Fatalf("missing loss report %s", loss.Data)
	}
	c.stopOnce.Do(func() { close(c.stop) })
	select {
	case <-c.finished:
	case <-time.After(3 * time.Second):
		t.Fatal("collector did not stop")
	}
}

func TestWebsocketProxyServerShutdownPrecedesTermination(t *testing.T) {
	s := newTestServer(t)
	connected := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := realtimeProxyUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		close(connected)
		c.ReadMessage()
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serveObservedWebsocketProxy(httputil.NewSingleHostReverseProxy(target), w, r, websocketProxyMetadata{Proxy: "test"})
	}))
	defer front.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	<-connected
	s.shutdownWebsocketProxies()
	closeEvent := waitProxyTelemetry(t, s, 0, "proxy.websocket.closed")
	var fields struct {
		First *proxyDirectionTelemetry `json:"first_observed_termination"`
	}
	json.Unmarshal(closeEvent.Data, &fields)
	if fields.First == nil || !fields.First.LocalCloseRequested {
		t.Fatalf("shutdown intent not observed before socket error: %s", closeEvent.Data)
	}
	requested := waitProxyTelemetry(t, s, 0, "proxy.websocket.local_close_requested")
	var requestedFields map[string]any
	json.Unmarshal(requested.Data, &requestedFields)
	if requestedFields["trigger_event_id"] == "" {
		t.Fatalf("shutdown cause link missing %s", requested.Data)
	}
	if !strings.Contains(string(requested.Data), "server_shutdown") {
		t.Fatalf("wrong reason %s", requested.Data)
	}
}

func TestWebsocketProxyRequestCancellationReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	trace := newWebsocketProxyTrace(nil, request, "proxy.websocket", 0, "", websocketProxyMetadata{})
	cancel()
	// Simulate net/http's cancellation watcher beating our watcher to Close.
	trace.requestClose("backend", "close", "", "")
	if trace.localClose["backend:close"] == "" {
		t.Fatal("cancellation close missing")
	}
	// The decision is observable in the collector's structured record.
	s := newTestServer(t)
	trace = newWebsocketProxyTrace(s, request, "proxy.websocket", 0, "", websocketProxyMetadata{})
	trace.requestClose("backend", "close", "", "")
	event := waitProxyTelemetry(t, s, 0, "proxy.websocket.local_close_requested")
	if !strings.Contains(string(event.Data), "request_canceled") {
		t.Fatalf("cancellation mislabeled %s", event.Data)
	}
}

func TestWebsocketProxyErrorsRedactURLs(t *testing.T) {
	for _, err := range []error{errors.New("read failed for wss://host/media/private?token=secret after timeout"), &url.Error{Op: "Get", URL: "https://host/path?token=secret", Err: syscall.ECONNRESET}} {
		value := proxyTransportError(err)
		if strings.Contains(value, "secret") || strings.Contains(value, "/media/") || !strings.Contains(value, "timeout") && !strings.Contains(value, "connection reset by peer") {
			t.Fatalf("unsafe or lost error %q", value)
		}
	}
}
