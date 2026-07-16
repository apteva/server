package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestIngressExposeRoute_UpsertsRouteCacheAndRejectsConflictingOwner(t *testing.T) {
	s := newTestServer(t)
	s.routeCache = NewRouteCache()

	route, err := s.ExposeIngressRoute(IngressExposeRequest{
		Hostname:       "App.Example.COM.",
		Target:         "http://127.0.0.1:3000",
		OwnerInstallID: 10,
		OwnerKind:      "deploy",
	})
	if err != nil {
		t.Fatalf("ExposeIngressRoute: %v", err)
	}
	if route.Hostname != "app.example.com" {
		t.Fatalf("hostname = %q, want app.example.com", route.Hostname)
	}
	if route.CertFQDN != "app.example.com" || route.TLSMode != "auto" {
		t.Fatalf("cert/tls defaults mismatched: %+v", route)
	}
	if got, ok := s.routeCache.Lookup("app.example.com"); !ok || got.target.String() != "http://127.0.0.1:3000" || got.kind != "deploy" {
		t.Fatalf("route cache mismatch: ok=%v route=%+v", ok, got)
	}

	updated, err := s.ExposeIngressRoute(IngressExposeRequest{
		Hostname:       "app.example.com",
		Target:         "http://127.0.0.1:3001",
		OwnerInstallID: 10,
		OwnerKind:      "deploy",
		AllowHTTP:      true,
	})
	if err != nil {
		t.Fatalf("same-owner update: %v", err)
	}
	if updated.Target != "http://127.0.0.1:3001" || !updated.AllowHTTP {
		t.Fatalf("updated route mismatch: %+v", updated)
	}
	if got, ok := s.routeCache.Lookup("app.example.com"); !ok || got.target.String() != "http://127.0.0.1:3001" || !got.allowHTTP {
		t.Fatalf("updated cache mismatch: ok=%v route=%+v", ok, got)
	}

	_, err = s.ExposeIngressRoute(IngressExposeRequest{
		Hostname:       "app.example.com",
		Target:         "http://127.0.0.1:4000",
		OwnerInstallID: 11,
		OwnerKind:      "fleet",
	})
	if err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("expected ownership conflict, got %v", err)
	}
}

func TestIngressAllowsCertificateForConfiguredPrimaryHost(t *testing.T) {
	s := newTestServer(t)
	s.primaryHost = "Agents.Example.com"
	if !s.ingressAllowsCertificate("agents.example.com:443") {
		t.Fatal("configured primary host was not certificate eligible")
	}
	if s.ingressAllowsCertificate("other.example.com") {
		t.Fatal("unregistered non-primary host was certificate eligible")
	}
}

func TestCallbackIngress_RequiresPermissionAndScopesOwner(t *testing.T) {
	s := newTestServer(t)
	s.routeCache = NewRouteCache()

	withoutPerm := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "no-ingress"}
	installWithoutPerm := seedInstallWithBindings(t, s, "no-ingress", withoutPerm, map[string]any{})

	req := httptest.NewRequest(http.MethodPost, "/apps/callback/ingress/expose",
		strings.NewReader(`{"hostname":"blocked.example.com","target":"http://127.0.0.1:7100"}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installWithoutPerm))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without permission, got %d: %s", rec.Code, rec.Body.String())
	}

	installGrantManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "install-grant"}
	installGrantID := seedInstallWithBindings(t, s, "install-grant", installGrantManifest, map[string]any{})
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET permissions_json = ? WHERE id = ?`,
		`["platform.ingress.write"]`,
		installGrantID,
	); err != nil {
		t.Fatalf("update install permissions: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/apps/callback/ingress/expose",
		strings.NewReader(`{"hostname":"grant.example.com","target":"http://127.0.0.1:7102"}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installGrantID))
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected install permissions to authorize ingress, got %d: %s", rec.Code, rec.Body.String())
	}

	withPerm := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "deploy",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermIngressWrite},
		},
	}
	installID := seedInstallWithBindings(t, s, "deploy", withPerm, map[string]any{})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json = '[]' WHERE id = ?`, installID); err != nil {
		t.Fatalf("clear install permissions: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/apps/callback/ingress/expose",
		strings.NewReader(`{"hostname":"tenant.example.com","target":"http://127.0.0.1:7101"}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected manifest-only permission to be rejected, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET permissions_json = ? WHERE id = ?`,
		`["platform.ingress.write"]`,
		installID,
	); err != nil {
		t.Fatalf("approve install permissions: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/apps/callback/ingress/expose",
		strings.NewReader(`{"hostname":"tenant.example.com","target":"http://127.0.0.1:7101"}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected approved permission to authorize ingress, got %d: %s", rec.Code, rec.Body.String())
	}
	var exposeOut struct {
		Route IngressRoute `json:"route"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &exposeOut); err != nil {
		t.Fatalf("decode expose response: %v", err)
	}
	if exposeOut.Route.OwnerInstallID != installID || exposeOut.Route.OwnerKind != "app" || exposeOut.Route.ProjectID != "proj-1" {
		t.Fatalf("route ownership/project mismatch: %+v", exposeOut.Route)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/callback/ingress/routes", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from routes, got %d: %s", rec.Code, rec.Body.String())
	}
	var routesOut struct {
		Routes []IngressRoute `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &routesOut); err != nil {
		t.Fatalf("decode routes response: %v", err)
	}
	if len(routesOut.Routes) != 1 || routesOut.Routes[0].Hostname != "tenant.example.com" {
		t.Fatalf("unexpected routes: %+v", routesOut.Routes)
	}
}

func TestIngressCertsReportsCachedAutocertStatus(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	cacheDir := t.TempDir()
	s.ingressCerts = &IngressCertManager{cacheDir: cacheDir}

	if err := writeTestCachedCert(cacheDir, "cert.example.com", time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("write cached cert: %v", err)
	}
	if _, err := s.ExposeIngressRoute(IngressExposeRequest{
		Hostname: "cert.example.com",
		Target:   "http://127.0.0.1:3000",
	}); err != nil {
		t.Fatalf("ExposeIngressRoute: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ingress/certs", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleIngressCerts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Certs []IngressCertCacheInfo `json:"certs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode certs response: %v", err)
	}
	if len(out.Certs) != 1 {
		t.Fatalf("cert count = %d, want 1: %+v", len(out.Certs), out.Certs)
	}
	cert := out.Certs[0]
	if cert.FQDN != "cert.example.com" || cert.RouteHost != "cert.example.com" || cert.Status != "live" || cert.TLSMode != "auto" {
		t.Fatalf("unexpected cert status: %+v", cert)
	}
}

func writeTestCachedCert(cacheDir, host string, notBefore, notAfter time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	entry := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return os.WriteFile(filepath.Join(cacheDir, host), entry, 0o600)
}
