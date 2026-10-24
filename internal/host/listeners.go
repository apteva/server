package host

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type ListenerRequest struct {
	Owner string `json:"owner,omitempty"`
	HTTP  string `json:"http,omitempty"`
	HTTPS string `json:"https,omitempty"`
}
type CertificateRequest struct {
	ServerName string   `json:"server_name"`
	Protocols  []string `json:"protocols"`
}
type CertificateResponse struct {
	Chain [][]byte `json:"chain"`
	Key   []byte   `json:"key"`
}
type cachedCertificate struct {
	certificate *tls.Certificate
	checked     time.Time
}
type ingress struct {
	mu           sync.Mutex
	listeners    map[string]*http.Server
	certificates map[string]cachedCertificate
}

func newIngress() *ingress {
	return &ingress{listeners: map[string]*http.Server{}, certificates: map[string]cachedCertificate{}}
}
func (h *Host) ensureListeners(req ListenerRequest) error {
	if req.Owner == "" {
		req.Owner = "manual"
	}
	if req.Owner != "manual" && req.Owner != "instance" && req.Owner != "environment-http" && req.Owner != "environment-https" {
		return fmt.Errorf("invalid listener owner")
	}
	h.ingress.mu.Lock()
	defer h.ingress.mu.Unlock()
	h.mu.Lock()
	old := h.state.Listeners
	desired := make(map[string]ListenerRequest, len(old)+1)
	for k, v := range old {
		desired[k] = v
	}
	h.mu.Unlock()
	if req.HTTP == "" && req.HTTPS == "" {
		delete(desired, req.Owner)
	} else {
		desired[req.Owner] = req
	}
	addresses := map[string]struct{ protocol, addr string }{}
	for _, entry := range desired {
		for _, listener := range []struct{ protocol, addr string }{{"http", entry.HTTP}, {"https", entry.HTTPS}} {
			if listener.addr == "" {
				continue
			}
			_, port, err := net.SplitHostPort(listener.addr)
			if err != nil {
				return err
			}
			_, primaryPort, _ := net.SplitHostPort(h.cfg.ListenAddr)
			if port == primaryPort {
				if listener.protocol == "https" {
					return fmt.Errorf("HTTPS conflicts with primary HTTP")
				}
				continue
			}
			addresses[listener.protocol+":"+listener.addr] = listener
		}
	}
	created := map[string]*http.Server{}
	cleanup := func() {
		for _, srv := range created {
			_ = srv.Close()
		}
	}
	for key, entry := range addresses {
		if h.ingress.listeners[key] != nil {
			continue
		}
		listener, err := net.Listen("tcp", entry.addr)
		if err != nil {
			cleanup()
			return err
		}
		srv := &http.Server{Handler: h.router, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
		if entry.protocol == "https" {
			srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1", "acme-tls/1"}, GetCertificate: h.getCertificate}
		}
		created[key] = srv
		if entry.protocol == "https" {
			go srv.ServeTLS(listener, "", "")
		} else {
			go srv.Serve(listener)
		}
	}
	h.mu.Lock()
	priorHTTP, priorHTTPS := h.state.HTTPAddr, h.state.HTTPSAddr
	h.state.Listeners = desired
	h.state.HTTPAddr = ""
	h.state.HTTPSAddr = ""
	for _, owner := range []string{"manual", "environment-http", "environment-https", "instance"} {
		entry := desired[owner]
		if entry.HTTP != "" {
			h.state.HTTPAddr = entry.HTTP
		}
		if entry.HTTPS != "" {
			h.state.HTTPSAddr = entry.HTTPS
		}
	}
	err := h.saveLocked()
	if err != nil {
		h.state.Listeners = old
		h.state.HTTPAddr = priorHTTP
		h.state.HTTPSAddr = priorHTTPS
	}
	h.mu.Unlock()
	if err != nil {
		cleanup()
		return err
	}
	for key, srv := range created {
		h.ingress.listeners[key] = srv
	}
	for key, srv := range h.ingress.listeners {
		if _, keep := addresses[key]; !keep {
			_ = srv.Close()
			delete(h.ingress.listeners, key)
		}
	}
	return nil
}
func (h *Host) closeListeners() {
	h.ingress.mu.Lock()
	defer h.ingress.mu.Unlock()
	for _, srv := range h.ingress.listeners {
		_ = srv.Close()
	}
}
func (h *Host) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	key := hello.ServerName
	challenge := false
	for _, p := range hello.SupportedProtos {
		if p == "acme-tls/1" {
			challenge = true
		}
	}
	h.ingress.mu.Lock()
	cached, ok := h.ingress.certificates[key]
	h.ingress.mu.Unlock()
	if !challenge && ok && time.Since(cached.checked) < time.Minute && cached.certificate.Leaf != nil && time.Until(cached.certificate.Leaf.NotAfter) > time.Minute {
		return cached.certificate, nil
	}
	h.mu.Lock()
	s := h.active
	h.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("certificate manager is starting")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := UnixClient(s.socket)
	client.Timeout = 2 * time.Minute
	defer client.CloseIdleConnections()
	var wire CertificateResponse
	err := Call(ctx, client, "POST", "/certificate", CertificateRequest{ServerName: key, Protocols: hello.SupportedProtos}, &wire)
	if err != nil {
		if !challenge && ok && cached.certificate.Leaf != nil && time.Now().Before(cached.certificate.Leaf.NotAfter) {
			return cached.certificate, nil
		}
		return nil, err
	}
	private, err := x509.ParsePKCS8PrivateKey(wire.Key)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: wire.Chain, PrivateKey: private}
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("empty certificate chain")
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	if !challenge {
		h.ingress.mu.Lock()
		h.ingress.certificates[key] = cachedCertificate{certificate: cert, checked: time.Now()}
		h.ingress.mu.Unlock()
	}
	return cert, nil
}
func (h *Host) listenersHandler(w http.ResponseWriter, r *http.Request) {
	var request ListenerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
		http.Error(w, "invalid listeners", 400)
		return
	}
	if err := h.ensureListeners(request); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	w.WriteHeader(204)
}
