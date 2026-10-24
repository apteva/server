package host

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPSListenerSurvivesServerHandoff(t *testing.T) {
	h, c, _, candidate := fixture(t)
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certServer.Close()
	cert := certServer.TLS.Certificates[0]
	var err error
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	h.ingress.mu.Lock()
	h.ingress.certificates["example.com"] = cachedCertificate{certificate: &cert, checked: time.Now()}
	h.ingress.mu.Unlock()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	if err = Call(context.Background(), c, "POST", "/listeners", ListenerRequest{HTTPS: addr}, nil); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"}}, Timeout: 8 * time.Second}
	defer client.CloseIdleConnections()
	check := func(want string) {
		t.Helper()
		resp, err := client.Get("https://" + addr)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != want {
			t.Fatalf("HTTPS backend=%q", b)
		}
	}
	check("1.0.0")
	if err = Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, c); s.Job.Phase != "succeeded" {
		t.Fatalf("deployment=%+v", s.Job)
	}
	client.CloseIdleConnections()
	check("2.0.0")
	if h.Snapshot().HTTPSAddr != addr {
		t.Fatal("HTTPS listener address changed")
	}
}
func TestWebsocketBlocksRetirementWithoutDroppingCall(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, b, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err = conn.WriteMessage(kind, b); err != nil {
				return
			}
		}
	}))
	defer backend.Close()
	router := NewRouter()
	blue, _ := newBackend(backend.URL)
	router.Publish(blue)
	front := httptest.NewServer(router)
	defer front.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	router.Pause()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err = router.Drain(ctx, blue); err == nil {
		t.Fatal("active realtime call was treated as drained")
	}
	if err = conn.WriteMessage(websocket.TextMessage, []byte("still connected")); err != nil {
		t.Fatal(err)
	}
	_, b, err := conn.ReadMessage()
	if err != nil || string(b) != "still connected" {
		t.Fatalf("call interrupted: %q %v", b, err)
	}
	router.Resume()
}
func TestCONNECTTunnelKeepsOriginalAuthorityAndBytes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "example.test:443" {
			http.Error(w, "wrong authority", 400)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, conn)
	}))
	defer backend.Close()
	router := NewRouter()
	blue, _ := newBackend(backend.URL)
	router.Publish(blue)
	front := httptest.NewServer(router)
	defer front.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n")
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT: %q %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	_, _ = conn.Write([]byte("ping"))
	b := make([]byte, 4)
	if _, err = io.ReadFull(reader, b); err != nil || string(b) != "ping" {
		t.Fatalf("tunnel failed: %q %v", b, err)
	}
}
func TestGatewayOverwritesSpoofedPrivateHeaders(t *testing.T) {
	observed := make(chan http.Header, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { observed <- r.Header.Clone(); w.WriteHeader(204) }))
	defer backend.Close()
	router := NewRouter()
	blue, _ := newBackend(backend.URL)
	router.Publish(blue)
	request := httptest.NewRequest("GET", "http://public.example/", nil)
	request.RemoteAddr = "198.51.100.9:1234"
	request.Header.Set("X-Apteva-Host-Remote-Addr", "127.0.0.1:1")
	request.Header.Set("X-Apteva-Host-Token", "spoofed")
	router.ServeHTTP(httptest.NewRecorder(), request)
	headers := <-observed
	if headers.Get("X-Apteva-Host-Remote-Addr") != "198.51.100.9:1234" || headers.Get("X-Apteva-Host-Token") != blue.secret {
		t.Fatal("untrusted request controlled private origin headers")
	}
}

func TestDrainingAllowsCallbacksNeededByExistingRequests(t *testing.T) {
	router := NewRouter()
	entered := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/work" {
			close(entered)
			<-release
			<-callbackDone
		}
		if r.URL.Path == "/api/apps/callback/finish" {
			close(callbackDone)
		}
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	backend, err := newBackend(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	router.Publish(backend)
	frontend := httptest.NewServer(router)
	defer frontend.Close()
	requests := &http.Client{Timeout: 2 * time.Second}
	defer requests.CloseIdleConnections()
	workDone := make(chan error, 1)
	go func() {
		response, err := requests.Get(frontend.URL + "/work")
		if err == nil {
			response.Body.Close()
		}
		workDone <- err
	}()
	<-entered
	router.Pause()
	drainDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { drainDone <- router.Drain(ctx, backend) }()
	close(release)
	response, err := requests.Post(frontend.URL+"/api/apps/callback/finish", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := <-workDone; err != nil {
		t.Fatal(err)
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("callback deadlocked request drain: %v", err)
	}
	router.mu.Lock()
	admitted := router.drainingCallbacks
	router.mu.Unlock()
	if admitted {
		t.Fatal("callback admission stayed open after exclusive drain")
	}
}

func TestListenerRemovalRetainsOtherOwners(t *testing.T) {
	h, _, _, _ := fixture(t)
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserve.Addr().String()
	reserve.Close()
	if err := h.ensureListeners(ListenerRequest{Owner: "manual", HTTP: addr}); err != nil {
		t.Fatal(err)
	}
	if err := h.ensureListeners(ListenerRequest{Owner: "instance", HTTP: addr}); err != nil {
		t.Fatal(err)
	}
	h.ingress.mu.Lock()
	original := h.ingress.listeners["http:"+addr]
	h.ingress.mu.Unlock()
	if err := h.ensureListeners(ListenerRequest{Owner: "instance"}); err != nil {
		t.Fatal(err)
	}
	h.ingress.mu.Lock()
	retained := h.ingress.listeners["http:"+addr]
	h.ingress.mu.Unlock()
	if retained != original || retained == nil {
		t.Fatal("removing one owner replaced shared listener")
	}
	response, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := h.ensureListeners(ListenerRequest{Owner: "manual"}); err != nil {
		t.Fatal(err)
	}
	h.ingress.mu.Lock()
	remaining := h.ingress.listeners["http:"+addr]
	h.ingress.mu.Unlock()
	if remaining != nil {
		t.Fatal("unused listener was retained")
	}
}
