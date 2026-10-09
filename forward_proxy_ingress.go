package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var forwardProxyTransport = &http.Transport{Proxy: nil, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 90 * time.Second}

// SNI identifies the proxy; HTTP Host and CONNECT authority identify its
// destination. Only an explicit, app-owned forward_proxy registration may
// enter this handler. Forwarded host/proto headers are never trusted here.
func (hr *HostRouter) serveForwardProxyIngress(w http.ResponseWriter, r *http.Request) bool {
	if hr.server == nil || hr.server.routeCache == nil {
		return false
	}
	var hit RouteHit
	var ok bool
	if r.TLS != nil {
		hit, ok = hr.lookup(r.TLS.ServerName)
	}
	if !ok || hit.Target == nil || hit.Target.Query().Get("ingress_mode") != "forward_proxy" {
		// Do not expose the app's admin/transport surface through Host routing.
		if byHost, exists := hr.lookup(r.Host); exists && byHost.Target != nil && byHost.Target.Query().Get("ingress_mode") == "forward_proxy" {
			http.Error(w, "native HTTPS proxy connection required", http.StatusBadRequest)
			return true
		}
		return false
	}
	if r.ProtoMajor != 1 {
		http.Error(w, "HTTP/1.1 proxy transport required", http.StatusHTTPVersionNotSupported)
		return true
	}
	if r.Method != http.MethodConnect && (!r.URL.IsAbs() || r.URL.Scheme != "http" || r.URL.User != nil) {
		http.Error(w, "absolute http URL or CONNECT authority required", http.StatusBadRequest)
		return true
	}
	target, available := hr.resolveTarget(hit)
	token := hr.resolveAppToken(hit)
	if !available || token == "" {
		http.Error(w, "proxy installation unavailable", http.StatusServiceUnavailable)
		return true
	}
	metadata := struct {
		Method  string      `json:"method"`
		URL     string      `json:"url"`
		Host    string      `json:"host"`
		Headers http.Header `json:"headers"`
	}{r.Method, r.URL.String(), r.Host, r.Header.Clone()}
	// Internal caller assertions must not be promoted into origin requests.
	clearPrincipalHeaders(&http.Request{Header: metadata.Headers})
	for name := range metadata.Headers {
		if strings.HasPrefix(strings.ToLower(name), "x-apteva-") {
			metadata.Headers.Del(name)
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil || len(encoded) > 64<<10 {
		http.Error(w, "proxy headers too large", http.StatusRequestHeaderFieldsTooLarge)
		return true
	}
	q := url.Values{"endpoint_id": {hit.Target.Query().Get("endpoint_id")}}
	target.RawQuery = q.Encode()
	body := r.Body
	if r.Method == http.MethodConnect {
		body = nil
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), body)
	if err != nil {
		http.Error(w, "invalid proxy transport", 502)
		return true
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Apteva-Proxy-Request", base64.RawStdEncoding.EncodeToString(encoded))
	req.Header.Set("X-Apteva-Proxy-Host", hit.Hostname)
	if r.Method == http.MethodConnect {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "apteva-proxy")
	}
	resp, err := forwardProxyTransport.RoundTrip(req)
	if err != nil {
		http.Error(w, "proxy transport unavailable", 502)
		return true
	}
	defer resp.Body.Close()
	if r.Method == http.MethodConnect && resp.StatusCode == http.StatusSwitchingProtocols {
		upstream, duplex := resp.Body.(io.ReadWriteCloser)
		hijacker, canHijack := w.(http.Hijacker)
		if !duplex || !canHijack {
			http.Error(w, "proxy streaming unavailable", 502)
			return true
		}
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			return true
		}
		defer client.Close()
		_ = client.SetDeadline(time.Time{})
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if buffered.Flush() != nil {
			return true
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		_ = upstream.Close()
		<-done
		return true
	}
	for key, values := range resp.Header {
		switch strings.ToLower(key) {
		case "connection", "upgrade", "keep-alive", "transfer-encoding", "trailer", "x-apteva-proxy-request":
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	if resp.StatusCode == 101 {
		http.Error(w, "unexpected proxy upgrade", 502)
		return true
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
	return true
}
