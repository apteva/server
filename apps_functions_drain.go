package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Route NEW before asking OLD to drain. SIGTERM would cancel SDK lifetime
// contexts and kill outstanding work, so do not send it until drain completes.
// A hard upper bound covers Functions' five-minute invocation limit plus slack.
func drainFunctions(ctx context.Context, spec activationSpec, poll time.Duration) error {
	host := spec.probeHost
	if host == "" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(spec.httpPort)) + "/runtime/drain"
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+spec.env["APTEVA_APP_TOKEN"])
		req.Header.Set("X-Apteva-Drain-Token", spec.env["APTEVA_APP_DRAIN_TOKEN"])
		resp, err := client.Do(req)
		if err == nil {
			var state struct {
				Draining bool `json:"draining"`
				Drained  bool `json:"drained"`
				Active   int  `json:"active_work"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&state)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && state.Draining && state.Drained && state.Active == 0 {
				return nil
			}
			if resp.StatusCode == http.StatusNotFound {
				// Older Functions has no drain endpoint. Keep it alive for the bounded
				// invocation lifetime after the route switch instead of killing it early.
				<-ctx.Done()
				return fmt.Errorf("legacy Functions drain grace elapsed: %w", ctx.Err())
			}
		}
		timer.Reset(poll)
	}
}
func drainOldFunctions(p *localProc) {
	if p == nil || p.spec.appName != "functions" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 330*time.Second)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-p.done:
			cancel()
		case <-done:
		}
	}()
	started := time.Now()
	err := drainFunctions(ctx, p.spec, 100*time.Millisecond)
	log.Printf("[APPS-LOCAL] functions drain elapsed=%s result=%v", time.Since(started), err)
}
