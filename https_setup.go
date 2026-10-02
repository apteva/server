package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPS setup is instance infrastructure. It does not install or call apps.
// The active configuration and pending setup are separate so a failed domain
// change cannot replace working callbacks or drop the existing certificate.
type instanceHTTPSConfig struct {
	Hostname             string   `json:"hostname"`
	Mode                 string   `json:"mode"`
	Email                string   `json:"email,omitempty"`
	HTTPPort             int      `json:"http_port"`
	HTTPSPort            int      `json:"https_port"`
	TrustedProxies       []string `json:"trusted_proxy_cidrs,omitempty"`
	ConnectionID         int64    `json:"connection_id,omitempty"`
	OwnerID              int64    `json:"owner_id,omitempty"`
	TokenEncrypted       string   `json:"token_encrypted,omitempty"`
	CertificateEncrypted string   `json:"certificate_encrypted,omitempty"`
	SetStrict            bool     `json:"set_cloudflare_strict,omitempty"`
	TrustCloudflare      bool     `json:"trust_cloudflare,omitempty"`
	AcceptTerms          bool     `json:"accept_acme_terms"`
}
type instanceHTTPSState struct {
	Active    *instanceHTTPSConfig `json:"active,omitempty"`
	Pending   *instanceHTTPSConfig `json:"pending,omitempty"`
	Phase     string               `json:"phase"`
	Message   string               `json:"message"`
	Addresses []string             `json:"addresses,omitempty"`
	Warnings  []string             `json:"warnings,omitempty"`
	Probe     string               `json:"probe"`
	UpdatedAt time.Time            `json:"updated_at"`
}
type instanceHTTPSManager struct {
	server                      *Server
	mu                          sync.RWMutex
	operation                   sync.Mutex
	state                       instanceHTTPSState
	preparing                   bool
	handler                     http.Handler
	existingHTTP, existingHTTPS string
	primaryHTTP                 string
	httpServer, httpsServer     *http.Server
	boundHTTP, boundHTTPS       int
	stop                        chan struct{}
	ctx                         context.Context
	cancel                      context.CancelFunc
}

var instanceTrustedHTTPSProxies atomic.Pointer[[]netip.Prefix]

func newInstanceHTTPSManager(s *Server) *instanceHTTPSManager {
	ctx, cancel := context.WithCancel(context.Background())
	n := &instanceHTTPSManager{server: s, stop: make(chan struct{}), ctx: ctx, cancel: cancel}
	_ = json.Unmarshal([]byte(s.store.GetSetting("instance_https")), &n.state)
	if n.state.Probe == "" {
		n.state.Probe = generateToken(24)
	}
	if n.state.Phase == "" {
		n.state.Phase = "unconfigured"
	}
	if n.state.Pending != nil && n.state.Phase != "error" {
		n.state.Phase = "interrupted"
		n.state.Message = "Setup was interrupted by a restart. Check the connection or retry setup."
	}
	n.applyTrustedProxies(n.state.Active)
	return n
}
func (n *instanceHTTPSManager) saveLocked() error {
	n.state.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(n.state)
	if err != nil {
		return err
	}
	return n.server.store.SetSetting("instance_https", string(raw))
}
func (n *instanceHTTPSManager) phase(phase, message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.state.Phase = phase
	n.state.Message = message
	_ = n.saveLocked()
}
func (n *instanceHTTPSManager) warning(message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, existing := range n.state.Warnings {
		if existing == message {
			return
		}
	}
	n.state.Warnings = append(n.state.Warnings, message)
	_ = n.saveLocked()
}
func publicHTTPSConfig(c *instanceHTTPSConfig) *instanceHTTPSConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.TokenEncrypted = ""
	out.CertificateEncrypted = ""
	out.OwnerID = 0
	return &out
}
func (n *instanceHTTPSManager) allowsHost(host string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, c := range []*instanceHTTPSConfig{n.state.Active, n.state.Pending} {
		if c != nil && strings.EqualFold(c.Hostname, stripHostPort(host)) {
			return true
		}
	}
	return false
}
func (n *instanceHTTPSManager) activeHost(host string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.state.Active != nil && strings.EqualFold(n.state.Active.Hostname, stripHostPort(host))
}
func (n *instanceHTTPSManager) serveProbe(w http.ResponseWriter, r *http.Request) bool {
	n.mu.RLock()
	probe := n.state.Probe
	n.mu.RUnlock()
	if r.URL.Path != "/.well-known/apteva-instance/"+probe || !n.allowsHost(r.Host) {
		return false
	}
	if r.TLS == nil {
		n.mu.RLock()
		config := n.state.Active
		if n.preparing && n.state.Pending != nil {
			config = n.state.Pending
		}
		native := config != nil && strings.EqualFold(config.Hostname, stripHostPort(r.Host)) && config.Mode != "proxy"
		n.mu.RUnlock()
		if native {
			http.Error(w, "the proxy must connect to the HTTPS origin; use Full (strict) with Cloudflare", http.StatusUpgradeRequired)
			return true
		}
		trusted := requestFromLoopback(r) || requestFromConfiguredProxy(r)
		n.mu.RLock()
		peer, _ := remoteRequestIP(r)
		for _, cfg := range []*instanceHTTPSConfig{n.state.Pending, n.state.Active} {
			if cfg != nil && cfg.Hostname == stripHostPort(r.Host) {
				for _, raw := range cfg.TrustedProxies {
					if prefix, err := netip.ParsePrefix(raw); err == nil && prefix.Contains(peer) {
						trusted = true
					}
				}
			}
		}
		n.mu.RUnlock()
		if !trusted || r.Header.Get("X-Forwarded-Proto") != "https" {
			http.Error(w, "configure the proxy CIDR and forward X-Forwarded-Proto=https", http.StatusConflict)
			return true
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, probe)
	return true
}
func (n *instanceHTTPSManager) applyTrustedProxies(c *instanceHTTPSConfig) {
	prefixes := []netip.Prefix{}
	if c != nil {
		for _, v := range c.TrustedProxies {
			if p, err := netip.ParsePrefix(v); err == nil {
				prefixes = append(prefixes, p.Masked())
			}
		}
	}
	instanceTrustedHTTPSProxies.Store(&prefixes)
}
func (n *instanceHTTPSManager) managedCertificate(host string) (*tls.Certificate, error) {
	n.mu.RLock()
	var selected *instanceHTTPSConfig
	// Serve a prepared candidate during verification, then restore the active
	// certificate if verification fails.
	candidates := []*instanceHTTPSConfig{n.state.Active}
	if n.preparing {
		candidates = append([]*instanceHTTPSConfig{n.state.Pending}, candidates...)
	}
	for _, c := range candidates {
		if c != nil && c.Hostname == host {
			if c.Mode == "direct" {
				n.mu.RUnlock()
				return nil, nil
			}
			if c.CertificateEncrypted != "" {
				copy := *c
				selected = &copy
				break
			}
		}
	}
	n.mu.RUnlock()
	if selected == nil {
		if n.allowsHost(host) {
			return nil, errors.New("the configured HTTPS certificate is not ready; retry domain setup")
		}
		return nil, nil
	}
	raw, err := Decrypt(n.server.secret, selected.CertificateEncrypted)
	if err != nil {
		return nil, err
	}
	return validateInstanceCertificate(host, raw, raw)
}
func validateInstanceCertificate(host, certPEM, keyPEM string) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("certificate and private key do not match: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return nil, fmt.Errorf("certificate does not cover %s", host)
	}
	if time.Now().Before(leaf.NotBefore) || time.Now().After(leaf.NotAfter) {
		return nil, fmt.Errorf("certificate is not currently valid")
	}
	pair.Leaf = leaf
	return &pair, nil
}
func validateHTTPSConfig(c *instanceHTTPSConfig) error {
	host, err := normalizeIngressHostname(c.Hostname)
	if err != nil {
		return err
	}
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return fmt.Errorf("enter a fully qualified domain name, without a scheme or path")
	}
	c.Hostname = host
	switch c.Mode {
	case "direct", "cloudflare", "proxy", "import":
	default:
		return fmt.Errorf("choose direct, cloudflare, proxy, or import")
	}
	if c.HTTPPort == 0 {
		c.HTTPPort = 80
	}
	if c.HTTPSPort == 0 {
		c.HTTPSPort = 443
	}
	if c.HTTPPort < 1 || c.HTTPPort > 65535 || c.HTTPSPort < 1 || c.HTTPSPort > 65535 || c.HTTPPort == c.HTTPSPort {
		return fmt.Errorf("HTTP and HTTPS ports must be distinct valid ports")
	}
	if (c.Mode == "direct" || c.Mode == "cloudflare") && !c.AcceptTerms {
		return fmt.Errorf("accept the certificate authority's terms before requesting a certificate")
	}
	for i, raw := range c.TrustedProxies {
		p, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil || p.Bits() == 0 {
			return fmt.Errorf("enter explicit trusted proxy CIDRs; trusting the entire internet is not allowed")
		}
		c.TrustedProxies[i] = p.Masked().String()
	}
	return nil
}
func (n *instanceHTTPSManager) view(uid int64) map[string]any {
	n.mu.RLock()
	state := n.state
	certificateConfig := n.state.Active
	if certificateConfig == nil {
		certificateConfig = n.state.Pending
	}
	state.Active = publicHTTPSConfig(state.Active)
	state.Pending = publicHTTPSConfig(state.Pending)
	state.Probe = ""
	n.mu.RUnlock()
	out := map[string]any{"state": state, "public_url": n.server.publicBaseURL(), "connections": []any{}}
	var choices []map[string]any
	if conns, err := n.server.store.ListConnections(uid); err == nil {
		for _, c := range conns {
			if c.AppSlug == "cloudflare" && c.Status == "active" && !isAppOwnedConnection(c) {
				choices = append(choices, map[string]any{"id": c.ID, "name": c.Name})
			}
		}
	}
	if choices != nil {
		out["connections"] = choices
	}
	cfg := state.Active
	if cfg == nil {
		cfg = state.Pending
	}
	if cfg != nil {
		out["dns_records"] = []map[string]string{{"type": "A", "name": cfg.Hostname, "value": "Your server's public IPv4 address"}, {"type": "AAAA (optional)", "name": cfg.Hostname, "value": "Only add IPv6 if this server is reachable over IPv6"}}
		var cert *tls.Certificate
		if certificateConfig != nil && certificateConfig.CertificateEncrypted != "" {
			if raw, err := Decrypt(n.server.secret, certificateConfig.CertificateEncrypted); err == nil {
				if pair, err := tls.X509KeyPair([]byte(raw), []byte(raw)); err == nil && len(pair.Certificate) > 0 {
					if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err == nil {
						pair.Leaf = leaf
						cert = &pair
					}
				}
			}
		}
		if cert != nil {
			out["certificate"] = map[string]any{"issuer": cert.Leaf.Issuer.String(), "expires_at": cert.Leaf.NotAfter, "renewal": map[bool]string{true: "automatic", false: "manual replacement"}[cfg.Mode == "cloudflare"]}
		} else if cfg.Mode == "direct" {
			if info, err := n.server.ingressCerts.CachedCertificateInfo(cfg.Hostname); err == nil {
				out["certificate"] = info
			}
		}
	}
	return out
}
func (s *Server) handleInstanceHTTPS(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requirePlatformAdmin(w, r)
	if !ok {
		return
	}
	if r.Header.Get("X-Apteva-Operator-ID") != strconv.FormatInt(uid, 10) {
		http.Error(w, "administrator session or private API key required", 403)
		return
	}
	n := s.instanceHTTPS
	if n == nil {
		http.Error(w, "HTTPS setup is not ready", 503)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, n.view(uid))
	case http.MethodPost:
		if cloneQuarantineEnabled() {
			http.Error(w, "HTTPS changes are disabled in clone quarantine", 409)
			return
		}
		var req struct {
			instanceHTTPSConfig
			Action string `json:"action"`
			Token  string `json:"cloudflare_token"`
			Cert   string `json:"certificate_pem"`
			Key    string `json:"private_key_pem"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid HTTPS setup request", 400)
			return
		}
		host := stripHostPort(r.Host)
		localHost := host == "localhost"
		if ip := net.ParseIP(host); ip != nil {
			localHost = ip.IsLoopback()
		}
		if (req.Token != "" || req.Key != "") && !requestIsTLS(r) && !(localHost && requestFromLoopback(r)) {
			http.Error(w, "Use HTTPS to submit credentials, or run apteva https setup locally on the server over SSH", 400)
			return
		}
		if !n.operation.TryLock() {
			http.Error(w, "HTTPS setup is already running", 409)
			return
		}
		if req.Action == "cancel" {
			n.mu.Lock()
			if n.state.Pending == nil {
				n.mu.Unlock()
				n.operation.Unlock()
				http.Error(w, "there is no unfinished setup to discard", 400)
				return
			}
			previous := n.state
			n.state.Pending = nil
			n.state.Phase = "unconfigured"
			n.state.Message = "Unfinished domain setup discarded"
			if n.state.Active != nil {
				n.state.Phase = "active"
				n.state.Message = "Retained the active domain configuration"
			}
			n.state.Warnings = nil
			err := n.saveLocked()
			if err != nil {
				n.state = previous
			}
			n.mu.Unlock()
			if err == nil && (previous.Active == nil || previous.Active.Mode == "proxy") {
				if n.httpServer != nil {
					_ = n.httpServer.Close()
					n.httpServer = nil
					n.boundHTTP = 0
				}
				if n.httpsServer != nil {
					_ = n.httpsServer.Close()
					n.httpsServer = nil
					n.boundHTTPS = 0
				}
			}
			n.operation.Unlock()
			if err != nil {
				http.Error(w, "cannot discard setup", 500)
				return
			}
			writeJSON(w, n.view(uid))
			return
		}
		if req.Action == "check" || req.Action == "retry" {
			n.mu.RLock()
			c := n.state.Pending
			if c == nil {
				c = n.state.Active
			}
			var cfg instanceHTTPSConfig
			if c != nil {
				cfg = *c
			}
			n.mu.RUnlock()
			if c == nil {
				n.operation.Unlock()
				http.Error(w, "configure a domain first", 400)
				return
			}
			n.mu.Lock()
			n.state.Phase = "queued"
			n.state.Message = "Preparing HTTPS checks"
			n.state.Warnings = nil
			err := n.saveLocked()
			n.mu.Unlock()
			if err != nil {
				n.operation.Unlock()
				http.Error(w, "cannot save HTTPS check", 500)
				return
			}
			go func() { defer n.operation.Unlock(); n.run(cfg, req.Action == "check") }()
			w.WriteHeader(202)
			writeJSON(w, n.view(uid))
			return
		}
		if req.Action != "setup" {
			n.operation.Unlock()
			http.Error(w, "unknown action", 400)
			return
		}
		cfg := req.instanceHTTPSConfig
		cfg.OwnerID = uid
		cfg.TokenEncrypted = ""
		cfg.CertificateEncrypted = ""
		if err := validateHTTPSConfig(&cfg); err != nil {
			n.operation.Unlock()
			http.Error(w, err.Error(), 400)
			return
		}
		var routeCount int
		if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM ingress_routes WHERE hostname=? AND status='active'`, cfg.Hostname).Scan(&routeCount); err != nil || routeCount > 0 {
			n.operation.Unlock()
			http.Error(w, "this hostname is already assigned to an app ingress route; use another hostname", 409)
			return
		}
		n.mu.RLock()
		for _, old := range []*instanceHTTPSConfig{n.state.Pending, n.state.Active} {
			if old != nil && old.Hostname == cfg.Hostname && old.Mode == cfg.Mode {
				cfg.CertificateEncrypted = old.CertificateEncrypted
				if req.Token == "" && cfg.ConnectionID == 0 {
					cfg.TokenEncrypted = old.TokenEncrypted
				}
				break
			}
		}
		n.mu.RUnlock()
		if cfg.Mode == "cloudflare" {
			if req.Token != "" {
				enc, err := Encrypt(s.secret, req.Token)
				if err != nil {
					n.operation.Unlock()
					http.Error(w, "cannot protect Cloudflare credentials", 500)
					return
				}
				cfg.TokenEncrypted = enc
				cfg.ConnectionID = 0
			}
			if _, err := n.cloudflareToken(cfg); err != nil {
				n.operation.Unlock()
				http.Error(w, err.Error(), 400)
				return
			}
		}
		if cfg.Mode == "import" && (req.Cert != "" || req.Key != "" || cfg.CertificateEncrypted == "") {
			if _, err := validateInstanceCertificate(cfg.Hostname, req.Cert, req.Key); err != nil {
				n.operation.Unlock()
				http.Error(w, err.Error(), 400)
				return
			}
			enc, err := Encrypt(s.secret, req.Cert+"\n"+req.Key)
			if err != nil {
				n.operation.Unlock()
				http.Error(w, "cannot protect certificate", 500)
				return
			}
			cfg.CertificateEncrypted = enc
		}
		n.mu.Lock()
		old := n.state
		n.state.Pending = &cfg
		n.state.Phase = "queued"
		n.state.Message = "Preparing domain setup"
		n.state.Warnings = nil
		err := n.saveLocked()
		if err != nil {
			n.state = old
		}
		n.mu.Unlock()
		if err != nil {
			n.operation.Unlock()
			http.Error(w, "cannot save HTTPS setup", 500)
			return
		}
		go func() { defer n.operation.Unlock(); n.run(cfg, false) }()
		w.WriteHeader(202)
		writeJSON(w, n.view(uid))
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", 405)
	}
}

func (n *instanceHTTPSManager) bind(handler http.Handler, primaryHTTP, existingHTTP, existingHTTPS string) {
	n.operation.Lock()
	defer n.operation.Unlock()
	n.handler = handler
	n.primaryHTTP = primaryHTTP
	n.existingHTTP = existingHTTP
	n.existingHTTPS = existingHTTPS
	n.mu.RLock()
	active := n.state.Active
	n.mu.RUnlock()
	if active != nil && active.Mode != "proxy" {
		if err := n.ensureListeners(*active); err != nil {
			n.phase("error", err.Error())
		}
	}
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-n.stop:
				return
			case <-timer.C:
				n.renew()
				timer.Reset(12 * time.Hour)
			}
		}
	}()
}
func (n *instanceHTTPSManager) close(ctx context.Context) {
	n.cancel()
	close(n.stop)
	n.operation.Lock()
	defer n.operation.Unlock()
	if n.httpServer != nil {
		_ = n.httpServer.Shutdown(ctx)
	}
	if n.httpsServer != nil {
		_ = n.httpsServer.Shutdown(ctx)
	}
}
func (n *instanceHTTPSManager) ensureListeners(c instanceHTTPSConfig) error {
	if n.handler == nil {
		return errors.New("server listeners are still starting; retry shortly")
	}
	// Existing environment-configured ingress is shared, never replaced.
	if n.existingHTTPS != "" {
		_, p, err := net.SplitHostPort(n.existingHTTPS)
		if err != nil || p != strconv.Itoa(c.HTTPSPort) {
			return fmt.Errorf("HTTPS is already configured at %s; use the matching listener port", n.existingHTTPS)
		}
	}
	if n.httpsServer != nil && n.boundHTTPS != c.HTTPSPort {
		return fmt.Errorf("an HTTPS listener already uses port %d; retain that port and adjust external forwarding if needed", n.boundHTTPS)
	}
	if n.existingHTTPS == "" && n.httpsServer == nil {
		listener, err := net.Listen("tcp", ":"+strconv.Itoa(c.HTTPSPort))
		if err != nil {
			return fmt.Errorf("cannot listen on HTTPS port %d: %w. Check for another service; on Linux use `sudo apteva https prepare-service --system --data-dir <instance-directory> --restart`, or forward public ports to higher local ports", c.HTTPSPort, err)
		}
		srv := &http.Server{Handler: n.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: n.server.ingressCerts.GetCertificate, NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}}}
		n.httpsServer = srv
		n.boundHTTPS = c.HTTPSPort
		go func() { _ = srv.ServeTLS(listener, "", "") }()
	}
	_, primaryPort, _ := net.SplitHostPort(n.primaryHTTP)
	usingPrimary := primaryPort == strconv.Itoa(c.HTTPPort)
	if c.Mode == "direct" && !usingPrimary && n.existingHTTP != "" {
		_, port, err := net.SplitHostPort(n.existingHTTP)
		if err != nil || port != strconv.Itoa(c.HTTPPort) {
			return fmt.Errorf("HTTP ingress already uses %s; use its listener port for validation", n.existingHTTP)
		}
	}
	if c.Mode == "direct" && n.httpServer != nil && n.boundHTTP != c.HTTPPort {
		return fmt.Errorf("an HTTP listener already uses port %d; retain that port", n.boundHTTP)
	}
	if c.Mode == "direct" && !usingPrimary && n.existingHTTP == "" && n.httpServer == nil {
		listener, err := net.Listen("tcp", ":"+strconv.Itoa(c.HTTPPort))
		if err != nil {
			return fmt.Errorf("cannot listen on HTTP port %d for certificate validation: %w", c.HTTPPort, err)
		}
		srv := &http.Server{Handler: n.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
		n.httpServer = srv
		n.boundHTTP = c.HTTPPort
		go func() { _ = srv.Serve(listener) }()
	}
	return nil
}
func (n *instanceHTTPSManager) run(c instanceHTTPSConfig, checkOnly bool) {
	n.mu.Lock()
	n.preparing = true
	n.mu.Unlock()
	defer func() { n.mu.Lock(); n.preparing = false; n.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(n.ctx, 8*time.Minute)
	defer cancel()
	fail := func(err error) { n.phase("error", err.Error()) }
	n.phase("dns", "Checking DNS records")
	ips, err := publicHTTPSAddresses(ctx, c.Hostname)
	if err != nil || len(ips) == 0 {
		fail(fmt.Errorf("DNS for %s is not ready: %v. Add public A/AAAA records, then retry", c.Hostname, err))
		return
	}
	var addresses []string
	for _, ip := range ips {
		addresses = append(addresses, ip.String())
	}
	n.mu.Lock()
	n.state.Addresses = addresses
	n.mu.Unlock()
	if c.Mode != "proxy" {
		n.phase("listener", "Preparing HTTPS listener")
		if err := n.ensureListeners(c); err != nil {
			fail(err)
			return
		}
	}
	if c.Mode == "import" && c.TrustCloudflare {
		ranges, err := cloudflareHTTPSProxyRanges(ctx)
		if err != nil {
			fail(err)
			return
		}
		c.TrustedProxies = ranges
	}
	if !checkOnly {
		n.phase("certificate", "Preparing the HTTPS certificate")
		switch c.Mode {
		case "direct":
			// Autocert performs public HTTP-01/TLS-ALPN validation and maintains renewal.
			result := make(chan error, 1)
			go func() {
				_, err := n.server.ingressCerts.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: c.Hostname, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256}, SupportedCurves: []tls.CurveID{tls.CurveP256}, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
				result <- err
			}()
			select {
			case err := <-result:
				if err != nil {
					fail(fmt.Errorf("certificate issuance failed: %w. Ensure public ports 80/443 reach this server; for proxied Cloudflare use Cloudflare mode", err))
					return
				}
			case <-ctx.Done():
				fail(errors.New("certificate validation timed out; check DNS and public ports 80/443"))
				return
			}
		case "cloudflare":
			if err := n.prepareCloudflare(ctx, &c); err != nil {
				fail(err)
				return
			}
		}
		n.mu.Lock()
		if n.state.Pending != nil && n.state.Pending.Hostname == c.Hostname {
			n.state.Pending = &c
		} else if n.state.Active != nil && n.state.Active.Hostname == c.Hostname {
			n.state.Active = &c
		}
		err := n.saveLocked()
		n.mu.Unlock()
		if err != nil {
			fail(err)
			return
		}
	}
	if checkOnly && (c.Mode == "cloudflare" || c.Mode == "import") {
		bundle, err := Decrypt(n.server.secret, c.CertificateEncrypted)
		if err == nil {
			_, err = validateInstanceCertificate(c.Hostname, bundle, bundle)
		}
		if err != nil || c.CertificateEncrypted == "" {
			fail(errors.New("no valid certificate is prepared; retry setup or import a replacement"))
			return
		}
	}
	if checkOnly && c.Mode == "direct" {
		info, err := n.server.ingressCerts.CachedCertificateInfo(c.Hostname)
		if err != nil || info.Status != "live" {
			fail(errors.New("no valid automatic certificate is cached; retry setup to obtain one"))
			return
		}
	}
	n.phase("verifying", "Verifying HTTPS reaches this Apteva instance")
	if c.Mode != "proxy" {
		if err := n.verifyOrigin(ctx, c); err != nil {
			fail(err)
			return
		}
	}
	if err := n.verifyPublic(ctx, c); err != nil {
		fail(fmt.Errorf("public HTTPS verification failed: %w. The previous public URL is unchanged. Check DNS, forwarding and certificate trust, then retry", err))
		return
	}
	if checkOnly {
		n.mu.RLock()
		pending := n.state.Pending != nil
		n.mu.RUnlock()
		if pending {
			n.phase("ready", "HTTPS checks passed. Retry setup to complete configuration and activate this address.")
		} else {
			n.phase("active", "HTTPS verified from this server; public address is active")
		}
		return
	}
	n.mu.Lock()
	previous := n.state
	n.state.Active = &c
	n.state.Pending = nil
	n.state.Phase = "active"
	n.state.Message = "HTTPS verified from this server; public address is active"
	n.state.UpdatedAt = time.Now().UTC()
	raw, _ := json.Marshal(n.state)
	tx, err := n.server.store.db.Begin()
	if err == nil {
		_, err = tx.Exec(`INSERT INTO server_settings(key,value) VALUES('instance_https',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(raw))
		if err == nil {
			_, err = tx.Exec(`INSERT INTO server_settings(key,value) VALUES('public_url',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "https://"+c.Hostname)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	if err != nil {
		n.state = previous
	}
	n.mu.Unlock()
	if err != nil {
		fail(err)
		return
	}
	n.applyTrustedProxies(&c)
}
func (n *instanceHTTPSManager) verifyOrigin(ctx context.Context, c instanceHTTPSConfig) error {
	originHost := "127.0.0.1"
	if host, _, err := net.SplitHostPort(n.existingHTTPS); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		originHost = host
	}
	originAddress := net.JoinHostPort(originHost, strconv.Itoa(c.HTTPSPort))
	if c.Mode == "direct" {
		conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: &tls.Config{ServerName: c.Hostname, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", originAddress)
		if err != nil {
			return fmt.Errorf("origin HTTPS listener check failed: %w", err)
		}
		return conn.Close()
	}
	expected, err := n.server.ingressCerts.GetCertificate(&tls.ClientHelloInfo{ServerName: c.Hostname})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(expected.Certificate[0])
	// Pin the configured origin certificate. This also supports imported Origin
	// CA certificates, whose trust anchor intentionally is not in browser roots.
	tlsConfig := &tls.Config{ServerName: c.Hostname, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || sha256.Sum256(cs.PeerCertificates[0].Raw) != digest {
			return errors.New("origin is serving a different certificate")
		}
		leaf := cs.PeerCertificates[0]
		if time.Now().After(leaf.NotAfter) || time.Now().Before(leaf.NotBefore) {
			return errors.New("origin certificate is not valid")
		}
		return leaf.VerifyHostname(c.Hostname)
	}}
	conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: tlsConfig}).DialContext(ctx, "tcp", originAddress)
	if err != nil {
		return fmt.Errorf("origin HTTPS listener check failed: %w", err)
	}
	return conn.Close()
}

// Resolve once and pin each connection, preventing DNS changes from redirecting
// diagnostics into a private network.
func publicHTTPSAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("no public DNS addresses")
	}
	for i, ip := range addresses {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
			return nil, errors.New("domain resolves to a private/local address; public setup requires public DNS")
		}
		addresses[i] = ip
	}
	return addresses, nil
}
func (n *instanceHTTPSManager) verifyPublic(ctx context.Context, c instanceHTTPSConfig) error {
	addresses, err := publicHTTPSAddresses(ctx, c.Hostname)
	if err != nil {
		return err
	}
	n.mu.RLock()
	probe := n.state.Probe
	n.mu.RUnlock()
	// Check every published address, including IPv6, instead of hiding a broken
	// route behind a successful connection to a different address.
	for _, ip := range addresses {
		if err := verifyHTTPSAddress(ctx, c.Hostname, probe, ip); err != nil {
			return fmt.Errorf("%s: %w", ip, err)
		}
	}
	return nil
}
func verifyHTTPSAddress(ctx context.Context, host, probe string, ip netip.Addr) error {
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), "443"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+host+"/.well-known/apteva-instance/"+probe, nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		return errors.New("the proxy is using HTTP to reach a native HTTPS origin; select Full (strict) in Cloudflare or forward to the HTTPS listener")
	}
	if resp.StatusCode == http.StatusConflict {
		return errors.New("proxy must preserve Host and send X-Forwarded-Proto=https; add its source CIDR to trusted proxies")
	}
	if resp.StatusCode != 200 || string(raw) != probe {
		return fmt.Errorf("the domain did not return this instance's verification response (HTTP %d)", resp.StatusCode)
	}
	return nil
}
func (n *instanceHTTPSManager) renew() {
	if !n.operation.TryLock() {
		return
	}
	defer n.operation.Unlock()
	n.mu.RLock()
	var c instanceHTTPSConfig
	if n.state.Active != nil {
		c = *n.state.Active
	}
	phase, message := n.state.Phase, n.state.Message
	n.mu.RUnlock()
	if c.Hostname == "" {
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, 8*time.Minute)
	defer cancel()
	if c.Mode == "cloudflare" || c.TrustCloudflare {
		if ranges, err := cloudflareHTTPSProxyRanges(ctx); err == nil {
			c.TrustedProxies = ranges
		} else {
			n.warning("Could not refresh Cloudflare proxy addresses; the last saved ranges remain in use.")
		}
	}
	if c.Mode == "cloudflare" || c.Mode == "import" {
		bundle, err := Decrypt(n.server.secret, c.CertificateEncrypted)
		var cert *tls.Certificate
		if err == nil {
			cert, err = validateInstanceCertificate(c.Hostname, bundle, bundle)
		}
		if err != nil || cert == nil || time.Until(cert.Leaf.NotAfter) < 30*24*time.Hour {
			if c.Mode == "import" {
				n.phase("renewal_required", "The imported certificate expires soon or is invalid. Import its replacement.")
				return
			}
			token, err := n.cloudflareToken(c)
			var zone, renewed string
			if err == nil {
				zone, err = cloudflareHTTPSZone(ctx, token, c.Hostname)
			}
			if err == nil {
				renewed, err = n.issueCloudflareCertificate(ctx, c, token, zone)
			}
			if err == nil {
				c.CertificateEncrypted, err = Encrypt(n.server.secret, renewed)
			}
			if err != nil {
				n.phase("renewal_required", "Automatic certificate renewal failed: "+err.Error())
				return
			}
		}
	}
	n.mu.Lock()
	previous := n.state
	n.state.Active = &c
	n.state.Phase, n.state.Message = phase, message
	if phase == "renewal_required" && c.Mode == "cloudflare" {
		n.state.Phase = "active"
		n.state.Message = "Automatic certificate renewal completed"
	}
	err := n.saveLocked()
	if err != nil {
		n.state = previous
	}
	n.mu.Unlock()
	if err != nil {
		n.phase("renewal_required", "Could not save renewed HTTPS configuration")
		return
	}
	n.applyTrustedProxies(&c)
}
