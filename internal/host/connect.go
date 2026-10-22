package host

import (
	"io"
	"net"
	"net/http"
	"time"
)

// CONNECT is a byte tunnel, not an ordinary HTTP response body. Keep it pinned
// to its original slot until the client closes it; deployment never kills an
// active proxy tunnel just to meet a rollout deadline.
func (b *backend) connect(w http.ResponseWriter, r *http.Request) {
	upstream, err := net.DialTimeout("tcp", b.url.Host, 10*time.Second)
	if err != nil {
		http.Error(w, "backend unavailable", 502)
		return
	}
	defer upstream.Close()
	req := r.Clone(r.Context())
	req.Header = r.Header.Clone()
	req.Header.Set("X-Apteva-Host-Token", b.secret)
	req.Header.Set("X-Apteva-Host-Remote-Addr", r.RemoteAddr)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	req.Header.Set("X-Apteva-Host-Scheme", scheme)
	if err = req.Write(upstream); err != nil {
		http.Error(w, "backend unavailable", 502)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT requires HTTP/1.1", 505)
		return
	}
	client, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buffer); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	// Release both copy goroutines before the request is counted as drained.
	_ = client.Close()
	_ = upstream.Close()
	<-done
}
