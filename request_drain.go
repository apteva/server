package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Keep authenticated app callbacks available until their processes are stopped,
// while canceling external work before its runtime dependencies disappear.
type requestDrain struct {
	next     http.Handler
	mu       sync.Mutex
	draining bool
	active   map[*http.Request]context.CancelFunc
}

func newRequestDrain(next http.Handler) *requestDrain {
	return &requestDrain{next: next, active: map[*http.Request]context.CancelFunc{}}
}
func (d *requestDrain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	callback := requestFromLoopback(r) && (strings.HasPrefix(r.URL.Path, "/apps/callback/") || strings.HasPrefix(r.URL.Path, "/api/apps/callback/"))
	if callback {
		d.next.ServeHTTP(w, r)
		return
	}
	d.mu.Lock()
	if d.draining {
		d.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	d.active[r] = cancel
	d.mu.Unlock()
	defer func() { cancel(); d.mu.Lock(); delete(d.active, r); d.mu.Unlock() }()
	d.next.ServeHTTP(w, r.WithContext(ctx))
}
func (d *requestDrain) begin() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.draining = true
	for _, cancel := range d.active {
		cancel()
	}
}

func (d *requestDrain) wait(timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		d.mu.Lock()
		n := len(d.active)
		d.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}
