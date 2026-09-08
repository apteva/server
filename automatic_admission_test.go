package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apteva/server/internal/admission"
)

func autoTestServer() *Server {
	s := &Server{}
	s.automatic.once.Do(func() {
		s.automatic.controller = admission.NewObserver()
	})
	return s
}
func TestAutomaticGatewaySharedAcrossCallersAndDistinctInputs(t *testing.T) {
	s := autoTestServer()
	var active, peak atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := active.Add(1)
		for {
			old := peak.Load()
			if old >= n || peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer active.Add(-1)
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, string(body))
	}))
	defer up.Close()
	var wg sync.WaitGroup
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value := fmt.Sprintf("centre-%d", i)
			if i == 6 {
				value = "all-centres"
			}
			client := &http.Client{Transport: &automaticTransport{server: s, target: "app:42", operation: fmt.Sprintf("operation-%d", i), caller: fmt.Sprintf("caller-%d", i), source: fmt.Sprintf("app:%d", i+100)}}
			resp, e := client.Post(up.URL, "text/plain", strings.NewReader(value))
			if e != nil {
				t.Error(e)
				return
			}
			defer resp.Body.Close()
			b, e := io.ReadAll(resp.Body)
			if e != nil || string(b) != value {
				t.Errorf("result changed: %s %v", b, e)
			}
		}(i)
	}
	wg.Wait()
	if peak.Load() < 2 {
		t.Fatalf("requests were serialized: peak %d", peak.Load())
	}
	if s.automatic.get().Snapshot()["active"].(int) != 0 {
		t.Fatal("gateway permit leaked")
	}
	t.Log("7 distinct requests from 7 callers/operations to one app: concurrent upstream execution; all results unchanged")
}
func TestAutomaticGatewayCancellationReleasesTransport(t *testing.T) {
	s := autoTestServer()
	entered := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
	}))
	defer up.Close()
	client := &http.Client{Transport: &automaticTransport{server: s, target: "app:7", operation: "slow", caller: "one"}}
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", up.URL, strings.NewReader("distinct"))
		done := make(chan error, 1)
		go func() {
			resp, e := client.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			done <- e
		}()
		<-entered
		cancel()
		if e := <-done; !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("downstream not canceled")
		}
		if s.automatic.get().Snapshot()["active"].(int) != 0 {
			t.Fatal("permit leaked")
		}
	}
}
func TestAutomaticGatewayOverloadHTTPContract(t *testing.T) {
	w := httptest.NewRecorder()
	writeAdmissionError(w, &admission.Error{Code: "adaptive_queue_full", Reason: "busy"})
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), "adaptive_queue_full") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAutomaticRequestBodyBudgetAndIdentity(t *testing.T) {
	s := autoTestServer()
	req := httptest.NewRequest("POST", "/apps/callback/apps/example/call", strings.NewReader("{}"))
	// Callback JSON remains budgeted even if Content-Type is omitted.
	req.ContentLength = 40 << 20
	release, ok := s.holdAdmissionBody(httptest.NewRecorder(), req)
	if !ok {
		t.Fatal("first body refused")
	}
	w := httptest.NewRecorder()
	other := req.Clone(context.Background())
	if _, ok := s.holdAdmissionBody(w, other); ok || w.Code != 429 {
		t.Fatal("payload budget not enforced", w.Code)
	}
	release()
	release()
	if s.automatic.bodyBytes != 0 {
		t.Fatal("body reservation leak")
	}
	body := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"execute","arguments":{"name":"some-private-resource","event":{"centre":"all"}}}}`
	r, _ := http.NewRequest("POST", "http://example/mcp", strings.NewReader(body))
	direct := admissionOperation(r, "/mcp")
	callback := admissionCallbackIdentity("execute", map[string]any{"name": "some-private-resource", "event": map[string]any{"centre": "different"}})
	if direct != callback || strings.Contains(direct, "private") || strings.Contains(direct, "centre") {
		t.Fatal("gateway identity differs or exposes arguments", direct, callback)
	}
}
func TestAutomaticGatewayBodyKeepsPermitUntilConsumed(t *testing.T) {
	s := autoTestServer()
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "complete")
	}))
	defer up.Close()
	tr := &automaticTransport{server: s, target: "app:9", operation: "stream", caller: "one"}
	req, _ := http.NewRequest("GET", up.URL, nil)
	resp, e := tr.RoundTrip(req)
	if e != nil {
		t.Fatal(e)
	}
	if s.automatic.get().Snapshot()["active"].(int) != 1 {
		t.Fatal("released on headers")
	}
	close(release)
	b, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || string(b) != "complete" {
		t.Fatal(string(b), e)
	}
	if s.automatic.get().Snapshot()["active"].(int) != 0 {
		t.Fatal("body permit leak")
	}
}

func TestAutomaticGatewayFastRequestDoesNotWaitForSameOperation(t *testing.T) {
	s := autoTestServer()
	entered := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(entered)
			<-release
		}
		fmt.Fprint(w, "ok")
	}))
	defer up.Close()
	defer close(release)
	client := &http.Client{Transport: &automaticTransport{server: s, target: "app:10", operation: "same", caller: "same"}}
	go func() {
		resp, err := client.Get(up.URL + "/slow")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", up.URL+"/fast", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("fast request queued behind slow request", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Apteva-Admission-Wait-Ms") != "0" {
		t.Fatal(resp.Header)
	}
	io.Copy(io.Discard, resp.Body)
	if s.automatic.get().Snapshot()["queued"].(int) != 0 {
		t.Fatal("transport queued work")
	}
}

func TestAutomaticHTTPResourceOperationsDoNotExposeNames(t *testing.T) {
	r := httptest.NewRequest("POST", "/fn/private-heavy?token=secret", nil)
	heavy := admissionOperation(r, "/gw/flexylead/pilotage/heavy")
	light := admissionOperation(r, "/gw/flexylead/session/bootstrap")
	if heavy == light || strings.Contains(heavy, "private") || strings.Contains(heavy, "secret") {
		t.Fatal(heavy, light)
	}
}
