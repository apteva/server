package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apteva/server/internal/admission"
)

// Store ownership shares accounting with lightweight Server wrappers used by
// managed tool paths. Transport accounting never schedules work by installation identity.
type automaticRuntime struct {
	bodyBytes  int64
	once       sync.Once
	controller *admission.Observer
	mu         sync.Mutex
}

func (a *automaticRuntime) get() *admission.Observer {
	a.once.Do(func() { a.controller = admission.NewObserver() })
	return a.controller
}
func (s *Server) automaticRuntime() *automaticRuntime {
	if s.store != nil {
		return &s.store.automatic
	}
	return &s.automatic
}

func (s *Server) handleAutomaticAdmission(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", 405)
		return
	}
	writeJSON(w, s.automaticRuntime().get().Snapshot())
}
func writeAdmissionError(w http.ResponseWriter, err error) {
	var e *admission.Error
	if errors.As(err, &e) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": e.Reason, "error_code": e.Code, "retryable": true, "retry_after_ms": 1000})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		http.Error(w, "caller deadline exceeded", 504)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	http.Error(w, "app gateway failed", 502)
}
func isAdmissionFailure(err error) bool { var e *admission.Error; return errors.As(err, &e) }

type automaticTransport struct {
	server                            *Server
	base                              http.RoundTripper
	target, operation, caller, source string
	background                        bool
}

func (t *automaticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	a := t.server.automaticRuntime()
	permit, err := a.get().Acquire(r.Context(), admission.Request{Key: t.target, Operation: t.operation, Caller: t.caller, Background: t.background})
	if err != nil {
		return nil, err
	}
	started := time.Now()
	finish := func(failed, overloaded bool) {
		permit.Finish(admission.Result{Duration: time.Since(started), Failed: failed, Canceled: r.Context().Err() != nil, Overloaded: overloaded})
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	// Preserve cleanup even if a custom transport panics; let net/http retain
	// its normal panic handling after releasing admission resources.
	returned := false
	defer func() {
		if !returned {
			finish(true, false)
		}
	}()
	resp, err := base.RoundTrip(r)
	returned = true
	if err != nil {
		finish(true, false)
		return nil, err
	}
	resp.Header.Set("X-Apteva-Admission-Wait-Ms", strconv.FormatInt(permit.Wait.Milliseconds(), 10))
	resp.Header.Add("Server-Timing", "apteva_gateway_queue;dur=0")
	failed := resp.StatusCode >= 500
	overloaded := resp.StatusCode == 429 || resp.StatusCode == 503
	resp.Body = &admissionBody{ReadCloser: resp.Body, finish: func(readErr error) { finish(failed || readErr != nil, overloaded) }}
	return resp, nil
}

type admissionBody struct {
	io.ReadCloser
	once   sync.Once
	finish func(error)
}

func (b *admissionBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(func() {
			if err == io.EOF {
				b.finish(nil)
			} else {
				b.finish(err)
			}
		})
	}
	return n, err
}
func (b *admissionBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.finish(err) })
	return err
}

// Use a bounded, parsed tool identity; no events, centre filters, access tokens
// or query strings become diagnostic labels. Authorization runs before this.
func admissionOperation(r *http.Request, tail string) string {
	if tail == "/mcp" && r.GetBody != nil {
		body, err := r.GetBody()
		if err == nil {
			defer body.Close()
			var rpc struct {
				Method string `json:"method"`
				Params struct {
					Name      string                     `json:"name"`
					Arguments map[string]json.RawMessage `json:"arguments"`
				} `json:"params"`
			}
			if json.NewDecoder(io.LimitReader(body, 50<<20)).Decode(&rpc) == nil && rpc.Method == "tools/call" && len(rpc.Params.Name) <= 128 {
				return admissionToolIdentity(rpc.Params.Name, rpc.Params.Arguments)
			}
		}
	}
	// Trace identity only: never a concurrency key. Hash the entire endpoint
	// so nested API routes remain distinct without exposing paths or tokens.
	sum := sha256.Sum256([]byte(tail))
	return fmt.Sprintf("%s /#%x", r.Method, sum[:12])
}

func (s *Server) admitIntegration(ctx context.Context, id int64, operation string) (*admission.Observation, error) {
	return s.automaticRuntime().get().Acquire(ctx, admission.Request{Key: fmt.Sprintf("integration:%d", id), Operation: operation, Caller: "integration"})
}

func admissionToolIdentity(name string, args map[string]json.RawMessage) string {
	for _, key := range []string{"name", "id"} {
		if value := args[key]; len(value) > 0 && len(value) <= 256 {
			sum := sha256.Sum256(value)
			return fmt.Sprintf("%s#%x", name, sum[:6])
		}
	}
	return name
}
func admissionCallbackIdentity(name string, args map[string]any) string {
	selected := map[string]json.RawMessage{}
	for _, key := range []string{"name", "id"} {
		if value, ok := args[key]; ok {
			switch value.(type) {
			case string, float64, int64, json.Number:
				b, _ := json.Marshal(value)
				selected[key] = b
			}
		}
	}
	return admissionToolIdentity(name, selected)
}

// Bound aggregate encoded JSON which the gateway must decode or buffer.
// Ordinary streamed proxy bodies do not consume this reservation.
func (s *Server) holdAdmissionBody(w http.ResponseWriter, r *http.Request) (func(), bool) {
	if r.Body == nil || r.ContentLength == 0 || !(strings.Contains(r.Header.Get("Content-Type"), "json") || strings.HasSuffix(r.URL.Path, "/mcp") || strings.Contains(r.URL.Path, "/apps/callback/")) {
		return func() {}, true
	}
	n := r.ContentLength
	if n < 0 {
		n = 50 << 20
	}
	if n > 50<<20 {
		http.Error(w, "request body too large", 413)
		return nil, false
	}
	a := s.automaticRuntime()
	a.mu.Lock()
	if a.bodyBytes+n > 64<<20 {
		a.mu.Unlock()
		writeAdmissionError(w, &admission.Error{Code: "adaptive_queue_full", Reason: "request payload budget occupied"})
		return nil, false
	}
	a.bodyBytes += n
	a.mu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
	var once sync.Once
	return func() { once.Do(func() { a.mu.Lock(); a.bodyBytes -= n; a.mu.Unlock() }) }, true
}
