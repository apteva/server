package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

type IngressCertManager struct {
	server    *Server
	manager   *autocert.Manager
	fallback  *CertCache
	challenge http.Handler
	cacheDir  string
}

func NewIngressCertManager(s *Server) *IngressCertManager {
	cacheDir := strings.TrimSpace(os.Getenv("APTEVA_ACME_CACHE_DIR"))
	if cacheDir == "" {
		base := strings.TrimSpace(os.Getenv("APTEVA_HOME"))
		if base == "" {
			if h, err := os.UserHomeDir(); err == nil && h != "" {
				base = filepath.Join(h, ".apteva")
			}
		}
		if base == "" {
			base = ".apteva"
		}
		cacheDir = filepath.Join(base, "ingress-certs")
	}
	renewBefore := 30 * 24 * time.Hour
	if days, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("APTEVA_ACME_RENEW_BEFORE_DAYS"))); days > 0 {
		renewBefore = time.Duration(days) * 24 * time.Hour
	}
	directoryURL := strings.TrimSpace(os.Getenv("APTEVA_ACME_DIRECTORY_URL"))
	if directoryURL == "" {
		directoryURL = autocert.DefaultACMEDirectory
	}

	icm := &IngressCertManager{
		server:   s,
		fallback: NewCertCache(s),
		cacheDir: cacheDir,
	}
	m := &autocert.Manager{
		Prompt:      autocert.AcceptTOS,
		Cache:       autocert.DirCache(cacheDir),
		Email:       strings.TrimSpace(os.Getenv("APTEVA_ACME_EMAIL")),
		RenewBefore: renewBefore,
		Client:      &acme.Client{DirectoryURL: directoryURL},
		HostPolicy: func(ctx context.Context, host string) error {
			if s != nil && s.ingressAllowsCertificate(host) {
				return nil
			}
			return errors.New("host is not registered in Apteva ingress")
		},
	}
	icm.manager = m
	icm.challenge = m.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	return icm
}

func (m *IngressCertManager) Start(refresh time.Duration) {
	if m == nil {
		return
	}
	if m.fallback != nil {
		m.fallback.Start(refresh)
	}
}

func (m *IngressCertManager) Stop() {
	if m == nil || m.fallback == nil {
		return
	}
	m.fallback.Stop()
}

func (m *IngressCertManager) Allow(hostname string) {
	// HostPolicy is DB-backed, so there is no allow-list to mutate.
	// This hook exists for ExposeIngressRoute to make the intent
	// explicit and gives us a stable place for future warm-up logic.
}

func (m *IngressCertManager) ServeHTTPChallenge(w http.ResponseWriter, r *http.Request) bool {
	if m == nil || m.challenge == nil || !strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
		return false
	}
	m.challenge.ServeHTTP(w, r)
	return true
}

func (m *IngressCertManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hello.ServerName), "."))
	if name == "" {
		return nil, errors.New("no SNI")
	}
	if m != nil && m.manager != nil && m.server != nil && m.server.ingressAllowsCertificate(name) {
		cert, err := m.manager.GetCertificate(hello)
		if err == nil {
			return cert, nil
		}
		log.Printf("[ingress-cert] %s: %v", name, err)
	}
	if m != nil && m.fallback != nil {
		return m.fallback.GetCertificate(hello)
	}
	return nil, errors.New("no certificate manager")
}

func (m *IngressCertManager) CachedCertificateInfo(hostname string) (IngressCertCacheInfo, error) {
	var out IngressCertCacheInfo
	host, err := normalizeIngressHostname(hostname)
	if err != nil {
		return out, err
	}
	out.FQDN = host
	if m == nil || strings.TrimSpace(m.cacheDir) == "" {
		out.Status = "cache_unavailable"
		return out, nil
	}
	cert, path, err := readAutocertCachedLeaf(m.cacheDir, host)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.Status = "not_cached"
			return out, nil
		}
		out.Status = "error"
		out.Error = err.Error()
		return out, nil
	}
	out.CachePath = path
	out.Serial = cert.SerialNumber.String()
	out.Issuer = cert.Issuer.String()
	out.NotBefore = cert.NotBefore.UTC().Format(time.RFC3339)
	out.NotAfter = cert.NotAfter.UTC().Format(time.RFC3339)
	now := time.Now()
	switch {
	case now.Before(cert.NotBefore):
		out.Status = "pending"
	case now.After(cert.NotAfter):
		out.Status = "expired"
	default:
		out.Status = "live"
	}
	return out, nil
}

func readAutocertCachedLeaf(cacheDir, host string) (*x509.Certificate, string, error) {
	for _, key := range []string{host, host + "+rsa"} {
		path := filepath.Join(cacheDir, filepath.Clean("/"+key))
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, path, err
		}
		for {
			block, rest := pem.Decode(data)
			if block == nil {
				break
			}
			data = rest
			if block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, path, err
			}
			return cert, path, nil
		}
		return nil, path, errors.New("no certificate PEM block in cache entry")
	}
	return nil, "", os.ErrNotExist
}

func startIngressTLSListener(addr string, handler http.Handler, certs *IngressCertManager) {
	if strings.TrimSpace(addr) == "" {
		return
	}
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: certs.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1", acme.ALPNProto},
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         cfg,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		log.Printf("[ingress-tls] listening on %s", addr)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Printf("[ingress-tls] listener exited: %v", err)
		}
	}()
}

func startIngressHTTPListener(addr string, handler http.Handler) {
	if strings.TrimSpace(addr) == "" {
		return
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		log.Printf("[ingress-http] listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("[ingress-http] listener exited: %v", err)
		}
	}()
}

func ingressListenAddrs(primaryHTTPAddr string) (string, string) {
	httpAddr := strings.TrimSpace(os.Getenv("APTEVA_HTTP_LISTEN_ADDR"))
	httpsAddr := strings.TrimSpace(os.Getenv("APTEVA_HTTPS_LISTEN_ADDR"))
	if legacy := strings.TrimSpace(os.Getenv("APTEVA_TLS_LISTEN_ADDR")); legacy != "" && httpsAddr == "" {
		httpsAddr = legacy
	}
	enabled := envTruthy(os.Getenv("APTEVA_INGRESS_ENABLED"))
	if !enabled && httpAddr == "" && httpsAddr == "" {
		return "", ""
	}
	if enabled {
		if httpAddr == "" {
			httpAddr = ":80"
		}
		if httpsAddr == "" {
			httpsAddr = ":443"
		}
	}
	if normalizeListenAddr(httpAddr) == normalizeListenAddr(primaryHTTPAddr) {
		httpAddr = ""
	}
	return httpAddr, httpsAddr
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func normalizeListenAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, ":") {
		return "0.0.0.0" + addr
	}
	return addr
}
