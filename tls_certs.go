package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CertCache fronts the certs app for the TLS listener. tls.Config's
// GetCertificate fires on every TLS handshake (ClientHello SNI), so
// it must return fast — we serve from an in-memory cache keyed by
// SNI, refreshed on a 60-second tick.
//
// On cache miss we attempt one synchronous fetch via cert_material;
// failures fall through to a TLS handshake error (browser shows
// "no cert"). That's acceptable behavior — fresh attaches just need
// to wait for the next refresh cycle.

type CertCache struct {
	server *Server

	mu     sync.RWMutex
	byName map[string]*tls.Certificate

	stopCh chan struct{}
}

func NewCertCache(s *Server) *CertCache {
	return &CertCache{
		server: s,
		byName: map[string]*tls.Certificate{},
		stopCh: make(chan struct{}),
	}
}

func (c *CertCache) Start(refresh time.Duration) {
	go c.refreshLoop(refresh)
}

func (c *CertCache) Stop() { close(c.stopCh) }

func (c *CertCache) refreshLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	c.refreshOnce()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			c.refreshOnce()
		}
	}
}

// refreshOnce calls certs.cert_list, then for each live cert fetches
// material. Cheap when nothing has changed; we don't have a delta
// feed yet so this is the simplest correct refresh strategy.
func (c *CertCache) refreshOnce() {
	if c.server == nil || c.server.installedApps == nil {
		return
	}
	if c.server.installedApps.GetByName("certs") == nil {
		c.replace(map[string]*tls.Certificate{})
		return
	}
	raw, err := callInstalledAppTool(c.server, "certs", "cert_list", map[string]any{})
	if err != nil {
		log.Printf("[cert-cache] list failed: %v", err)
		return
	}
	var env struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Result.Content) == 0 {
		return
	}
	var inner struct {
		Certs []struct {
			FQDN   string `json:"fqdn"`
			Status string `json:"status"`
		} `json:"certs"`
	}
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &inner); err != nil {
		return
	}

	next := map[string]*tls.Certificate{}
	for _, ce := range inner.Certs {
		if ce.Status != "live" || ce.FQDN == "" {
			continue
		}
		// Reuse the cached cert when possible — saves the cert_material
		// round-trip on every refresh.
		c.mu.RLock()
		existing := c.byName[strings.ToLower(ce.FQDN)]
		c.mu.RUnlock()
		if existing != nil && !nearExpiry(existing) {
			next[strings.ToLower(ce.FQDN)] = existing
			continue
		}
		cert, err := c.fetchMaterial(ce.FQDN)
		if err != nil {
			log.Printf("[cert-cache] fetch %s: %v", ce.FQDN, err)
			continue
		}
		next[strings.ToLower(ce.FQDN)] = cert
	}
	c.replace(next)
}

func nearExpiry(cert *tls.Certificate) bool {
	if cert == nil || cert.Leaf == nil {
		return false
	}
	return time.Until(cert.Leaf.NotAfter) < 24*time.Hour
}

func (c *CertCache) replace(next map[string]*tls.Certificate) {
	c.mu.Lock()
	c.byName = next
	c.mu.Unlock()
}

// fetchMaterial calls the certs app's privileged cert_material tool.
// Decodes the PEM into a tls.Certificate ready for SNI.
func (c *CertCache) fetchMaterial(fqdn string) (*tls.Certificate, error) {
	raw, err := callInstalledAppTool(c.server, "certs", "cert_material", map[string]any{"fqdn": fqdn})
	if err != nil {
		return nil, err
	}
	var env struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Result.Content) == 0 {
		return nil, errors.New("cert_material: empty envelope")
	}
	var payload struct {
		Found   bool   `json:"found"`
		FQDN    string `json:"fqdn"`
		CertPEM string `json:"cert_pem"`
		KeyPEM  string `json:"key_pem"`
	}
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &payload); err != nil {
		return nil, fmt.Errorf("cert_material: decode: %w", err)
	}
	if !payload.Found {
		return nil, errors.New("cert not live")
	}
	cert, err := tls.X509KeyPair([]byte(payload.CertPEM), []byte(payload.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("X509KeyPair: %w", err)
	}
	return &cert, nil
}

// GetCertificate is the tls.Config hook. Hot path — keep it lock-fast.
// On miss, do one synchronous lazy fetch so newly-issued certs work
// before the next refresh cycle.
func (c *CertCache) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if name == "" {
		return nil, errors.New("no SNI")
	}
	c.mu.RLock()
	cert, ok := c.byName[name]
	c.mu.RUnlock()
	if ok {
		return cert, nil
	}
	// Lazy fetch. Don't block other handshakes for too long.
	cert, err := c.fetchMaterial(name)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.byName[name] = cert
	c.mu.Unlock()
	return cert, nil
}

// startTLSListener wires up an :{addr} HTTPS listener fronting the
// same handler stack as plain HTTP. Skipped silently when addr is
// empty so existing deployments keep their current behavior.
func startTLSListener(addr string, handler http.Handler, cache *CertCache) {
	if strings.TrimSpace(addr) == "" {
		return
	}
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cache.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
	srv := &http.Server{
		Addr:      addr,
		Handler:   handler,
		TLSConfig: cfg,
	}
	go func() {
		log.Printf("[tls] listening on %s", addr)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Printf("[tls] listener exited: %v", err)
		}
	}()
}
