package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type runtimeBrowserClaims struct {
	Issuer       string                `json:"issuer"`
	Principal    *sdk.TrustedPrincipal `json:"principal,omitempty"`
	InstallID    int64                 `json:"install_id"`
	ProjectID    string                `json:"project_id"`
	UserID       int64                 `json:"user_id"`
	DeploymentID string                `json:"deployment_id"`
	Generation   int64                 `json:"generation"`
	Operations   []string              `json:"operations"`
	Origin       string                `json:"origin"`
	ExpiresAt    int64                 `json:"expires_at"`
	Nonce        string                `json:"nonce"`
}

func (s *Server) runtimeGrantInstall(token string) (id, user int64, status string, err error) {
	err = s.store.db.QueryRow(`SELECT i.id,COALESCE(i.installed_by,0),i.status FROM app_runtime_credentials c JOIN app_runtime_deployments d ON d.id=c.deployment_id JOIN app_installs i ON i.id=d.install_id WHERE c.token_hash=? AND d.status IN ('prepared','starting','ready','active','retiring') AND i.runtime_managed=1`, HashAPIKey(token)).Scan(&id, &user, &status)
	return
}
func (s *Server) runtimeLegacyTokenAllowed(id int64) bool {
	if !s.runtimeManaged(id) {
		return true
	}
	d, err := s.appRuntimeManager().active(id)
	if err == nil && d.CredentialEncrypted == "" && d.Status == "active" {
		return true
	}
	ds, e := s.appRuntimeManager().deployments(id)
	if e != nil {
		return false
	}
	for _, old := range ds {
		if old.CredentialEncrypted == "" && old.Status == "retiring" {
			return true
		}
	}
	return false
}
func (s *Server) handleRuntimeBrowserCredentials(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if _, ok := s.requireAppInstallAccess(w, r, id, ProjectEditor); !ok {
		return
	}
	d, err := s.appRuntimeManager().active(id)
	if err != nil || d.Execution.BrowserEndpoint == "" {
		http.Error(w, "direct browser endpoint unavailable", 409)
		return
	}
	var body struct {
		Origin     string   `json:"origin"`
		Operations []string `json:"operations"`
	}
	if runtimeDecode(r, &body) != nil || len(body.Operations) == 0 || len(body.Operations) > 3 {
		http.Error(w, "invalid browser credential request", 400)
		return
	}
	for _, v := range body.Operations {
		if v != "read" && v != "write" && v != "stream" {
			http.Error(w, "unknown browser operation", 400)
			return
		}
	}
	if !s.runtimeBrowserOriginAllowed(id, body.Origin) {
		http.Error(w, "browser origin must match the managed Apteva origin", 403)
		return
	}
	var project string
	s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, id).Scan(&project)
	claims := runtimeBrowserClaims{InstallID: id, ProjectID: project, UserID: getUserID(r), DeploymentID: d.ID, Generation: d.Generation, Operations: body.Operations, Origin: body.Origin, ExpiresAt: time.Now().Unix() + 300, Nonce: generateToken(16), Issuer: s.runtimeIssuer()}
	claims.Principal = s.trustedAppPrincipal(r, project)

	encoded := base64.RawURLEncoding.EncodeToString([]byte(runtimeJSON(claims)))
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("apteva-runtime-browser/v1:" + encoded))
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"token": token, "expires_at": claims.ExpiresAt, "endpoint": d.Execution.BrowserEndpoint})
}
func (s *Server) validateRuntimeBrowser(token string, id int64, origin, operation string) (runtimeBrowserClaims, error) {
	var c runtimeBrowserClaims
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(token) > 4096 {
		return c, fmt.Errorf("invalid browser credential")
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return c, fmt.Errorf("invalid browser signature")
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("apteva-runtime-browser/v1:" + parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return c, fmt.Errorf("invalid browser signature")
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil || json.Unmarshal(raw, &c) != nil {
		return c, fmt.Errorf("invalid browser claims")
	}
	if c.Issuer != s.runtimeIssuer() || c.InstallID != id || c.UserID <= 0 || c.ExpiresAt <= time.Now().Unix() || c.Origin != origin || !runtimeContains(c.Operations, operation) {
		return c, fmt.Errorf("browser credential scope mismatch or expiry")
	}
	var project string
	if e = s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, id).Scan(&project); e != nil || project != c.ProjectID {
		return c, fmt.Errorf("browser credential project mismatch")
	}
	if project == "" && s.store.GetPlatformRole(c.UserID) != PlatformAdmin {
		return c, fmt.Errorf("browser user no longer has global access")
	}
	if project != "" && s.effectiveRoleOnProject(c.UserID, project).Rank() < ProjectEditor.Rank() {
		return c, fmt.Errorf("browser user no longer has project access")
	}
	d, e := s.appRuntimeManager().active(id)
	if e != nil || d.ID != c.DeploymentID || d.Generation != c.Generation || d.Status != "active" {
		return c, fmt.Errorf("browser credential deployment is no longer active")
	}
	if !s.runtimeBrowserOriginAllowed(id, origin) {
		return c, fmt.Errorf("browser origin is no longer approved")
	}
	var n int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_browser_revocations WHERE token_hash=?`, HashAPIKey(token)).Scan(&n)
	if n != 0 {
		return c, fmt.Errorf("browser credential revoked")
	}
	return c, nil
}
func (s *Server) handleRuntimeBrowserAuthorize(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.Header.Get("X-Apteva-App-Install-ID"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "authenticated app required", 403)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var body struct {
		Token     string `json:"token"`
		Origin    string `json:"origin"`
		Operation string `json:"operation"`
	}
	if runtimeDecode(r, &body) != nil {
		http.Error(w, "invalid authorization request", 400)
		return
	}
	claims, err := s.validateRuntimeBrowser(body.Token, id, body.Origin, body.Operation)
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	writeJSON(w, claims)
}
func (s *Server) handleRuntimeOwnership(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.Header.Get("X-Apteva-App-Install-ID"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "authenticated app required", 403)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var deployment string
	var generation int64
	var granted int
	// The authenticated credential identifies the execution. Neither a pending
	// pointer nor the public routing pointer confers permission to admit work.
	err = s.store.db.QueryRow(`SELECT d.id,d.generation,COALESCE(o.granted,0)
	 FROM app_runtime_credentials c JOIN app_runtime_deployments d ON d.id=c.deployment_id
	 LEFT JOIN app_runtime_execution_owners o ON o.deployment_id=d.id AND o.install_id=d.install_id AND o.generation=d.generation
	 WHERE c.token_hash=? AND d.install_id=? AND d.status IN ('ready','active','retiring')`, HashAPIKey(token), id).Scan(&deployment, &generation, &granted)
	if err != nil {
		http.Error(w, "execution ownership unavailable", 409)
		return
	}
	writeJSON(w, map[string]any{"active": granted == 1, "deployment_id": deployment, "generation": generation, "expires_at": time.Now().Unix() + 15})
}

func runtimeSignedBrowserToken(s *Server, c runtimeBrowserClaims) string {
	if c.Issuer == "" {
		c.Issuer = s.runtimeIssuer()
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(runtimeJSON(c)))
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("apteva-runtime-browser/v1:" + encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) runtimeIssuer() string { return "apteva:" + runtimeHash([]byte(s.instanceSecret)) }
func (s *Server) handleRuntimeBrowserRevoke(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if _, ok := s.requireAppInstallAccess(w, r, id, ProjectEditor); !ok {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if runtimeDecode(r, &body) != nil {
		http.Error(w, "invalid revocation", 400)
		return
	}
	parts := strings.Split(body.Token, ".")
	if len(parts) != 2 {
		http.Error(w, "invalid credential", 400)
		return
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var c runtimeBrowserClaims
	if json.Unmarshal(raw, &c) != nil || c.InstallID != id || len(c.Operations) == 0 {
		http.Error(w, "invalid credential scope", 400)
		return
	}
	if c.UserID != getUserID(r) && s.store.GetPlatformRole(getUserID(r)) != PlatformAdmin {
		http.Error(w, "credential belongs to another user", 403)
		return
	}
	if _, err := s.validateRuntimeBrowser(body.Token, id, c.Origin, c.Operations[0]); err != nil {
		http.Error(w, "invalid or expired credential", 400)
		return
	}
	_, err := s.store.db.Exec(`INSERT OR IGNORE INTO app_runtime_browser_revocations(token_hash,expires_at) VALUES(?,?)`, HashAPIKey(body.Token), c.ExpiresAt)
	if err != nil {
		http.Error(w, "revocation unavailable", 503)
		return
	}
	writeJSON(w, map[string]bool{"revoked": true})
}

func (s *Server) runtimeBrowserOrigin() string {
	u, e := url.Parse(s.publicBaseURL())
	if e != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
