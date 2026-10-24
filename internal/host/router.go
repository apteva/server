package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// Router queues new requests during ownership transfer. Requests are forwarded
// exactly once; mutating requests are never automatically retried or replayed.
type Router struct {
	mu                sync.Mutex
	active            *backend
	starting          *backend
	gate              chan struct{}
	changed           chan struct{}
	queued            int
	MaxQueued         int
	drainingCallbacks bool
}
type backend struct {
	secret string
	url    *url.URL
	proxy  *httputil.ReverseProxy
	active int
}

func NewRouter() *Router {
	return &Router{gate: make(chan struct{}), changed: make(chan struct{}), MaxQueued: 4096}
}
func newBackend(raw string) (*backend, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, errors.New("invalid backend")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(secret)
	p := httputil.NewSingleHostReverseProxy(u)
	b := &backend{url: u, proxy: p, secret: token}
	b.setSecret(token)
	p.FlushInterval = -1
	// Preserve Host for tenant routing and preserve client identity at ingress.
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
	}
	return b, nil
}
func (r *Router) notifyLocked() { close(r.changed); r.changed = make(chan struct{}) }
func (r *Router) SetStarting(b *backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starting = b
	r.notifyLocked()
}
func (r *Router) clearStarting(b *backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.starting == b {
		r.starting = nil
		r.notifyLocked()
	}
}
func (r *Router) Publish(b *backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = b
	r.starting = nil
	if r.gate != nil {
		close(r.gate)
		r.gate = nil
	}
	r.notifyLocked()
}
func (r *Router) Pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate == nil {
		r.gate = make(chan struct{})
	}
	r.drainingCallbacks = true
	r.notifyLocked()
}
func (r *Router) Resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starting = nil
	if r.gate != nil {
		close(r.gate)
		r.gate = nil
	}
	r.notifyLocked()
}

func startupCallback(req *http.Request) bool {
	// The backend still authenticates every callback. Never use a forwarding
	// header to classify loopback, and never expose the private host API here.
	ip, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil || !net.ParseIP(ip).IsLoopback() {
		return false
	}
	p := req.URL.Path
	return strings.HasPrefix(p, "/api/apps/callback/") || strings.HasPrefix(p, "/apps/callback/") || p == "/api/telemetry" || p == "/api/telemetry/live" || p == "/api/apps/apteva-server/mcp"
}
func reconnectable(req *http.Request) bool {
	// These GET subscriptions carry persisted chat state, not an in-flight tool
	// invocation. EventSource reconnects and the dashboard reloads durable state.
	return req.Method == http.MethodGet && (req.URL.Path == "/api/apps/channel-chat/stream" || req.URL.Path == "/api/telemetry/stream" || req.URL.Path == "/api/telemetry/live") && !strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
}
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	queued := false
	defer func() {
		if queued {
			r.mu.Lock()
			r.queued--
			r.mu.Unlock()
		}
	}()
	for {
		r.mu.Lock()
		b := r.active
		callback := startupCallback(req)
		if r.starting != nil && callback {
			b = r.starting
		}
		if b != nil && (r.gate == nil || b == r.starting || (r.drainingCallbacks && callback)) {
			tracked := !reconnectable(req)
			if tracked {
				b.active++
			}
			r.mu.Unlock()
			if tracked {
				defer func() { r.mu.Lock(); b.active--; r.notifyLocked(); r.mu.Unlock() }()
			}
			if req.Method == http.MethodConnect {
				b.connect(w, req)
			} else {
				b.proxy.ServeHTTP(w, req)
			}
			return
		}
		if !queued {
			if r.queued >= r.MaxQueued {
				r.mu.Unlock()
				w.Header().Set("Retry-After", "1")
				http.Error(w, "deployment queue full", http.StatusServiceUnavailable)
				return
			}
			r.queued++
			queued = true
		}
		ch := r.changed
		r.mu.Unlock()
		select {
		case <-req.Context().Done():
			return
		case <-ch:
		}
	}
}
func (r *Router) Drain(ctx context.Context, b *backend) error {
	for {
		r.mu.Lock()
		n := b.active
		ch := r.changed
		if n == 0 {
			r.drainingCallbacks = false
		}
		r.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

func (b *backend) setSecret(token string) {
	b.secret = token
	director := httputil.NewSingleHostReverseProxy(b.url).Director
	b.proxy.Director = func(req *http.Request) {
		remote := req.RemoteAddr
		scheme := "http"
		if req.TLS != nil {
			scheme = "https"
		}
		director(req)
		req.Header.Set("X-Apteva-Host-Token", token)
		req.Header.Set("X-Apteva-Host-Remote-Addr", remote)
		req.Header.Set("X-Apteva-Host-Scheme", scheme)
	}
}
