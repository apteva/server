package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const managedControlBodyLimit = 2 << 20

type managedTenantIdentity struct {
	TenantID      string `json:"tenant_id"`
	ControllerURL string `json:"controller_url"`
	Token         string `json:"token"`
}

type managedReconcileGrant = sdk.ManagedConnectionGrantDelivery

type managedReconcileResponse struct {
	TenantID      string                    `json:"tenant_id"`
	Grants        []managedReconcileGrant   `json:"grants"`
	RevokedGrants []string                  `json:"revoked_grant_ids,omitempty"`
	Bundles       []sdk.ManagedTenantBundle `json:"bundles"`
}

func managedTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validManagedID(v string) bool {
	if v == "" || len(v) > 200 {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:", r) {
			continue
		}
		return false
	}
	return true
}

func (s *Server) authorizeManagedTenantApp(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return 0, 0, false
	}
	if !installHasPermission(s, installID, sdk.PermManagedTenantsManage) {
		http.Error(w, "missing permission: "+string(sdk.PermManagedTenantsManage), http.StatusForbidden)
		return 0, 0, false
	}
	var userID int64
	var projectID string
	if err := s.store.db.QueryRow(`SELECT COALESCE(installed_by,0), COALESCE(project_id,'') FROM app_installs WHERE id=? AND status='running'`, installID).Scan(&userID, &projectID); err != nil || userID <= 0 || projectID != "" {
		http.Error(w, "managed tenant control requires a running global app installation", http.StatusForbidden)
		return 0, 0, false
	}
	if s.store.GetPlatformRole(userID) != PlatformAdmin {
		http.Error(w, "managed tenant control requires an admin-owned app installation", http.StatusForbidden)
		return 0, 0, false
	}
	return installID, userID, true
}

// handleCallbackManagedTenants is the privileged controller-facing surface.
// It is called by a SaaS/control-plane app, never by tenant installations.
func (s *Server) handleCallbackManagedTenants(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, userID, ok := s.authorizeManagedTenantApp(w, r)
	if !ok {
		return
	}
	switch {
	case len(parts) == 1 && parts[0] == "tenants" && r.Method == http.MethodPost:
		s.handleManagedTenantEnsure(w, r, installID)
	case len(parts) == 1 && parts[0] == "enrollments" && r.Method == http.MethodPost:
		s.handleManagedEnrollmentCreate(w, r, installID)
	case len(parts) == 1 && parts[0] == "grants" && r.Method == http.MethodPost:
		s.handleManagedGrantEnsure(w, r, installID, userID)
	case len(parts) == 4 && parts[0] == "grants" && parts[3] == "delivery" && r.Method == http.MethodGet:
		s.handleManagedGrantDelivery(w, r, installID, parts[1], parts[2])
	case len(parts) == 3 && parts[0] == "grants" && r.Method == http.MethodDelete:
		s.handleManagedGrantRevoke(w, r, installID, parts[1], parts[2])
	case len(parts) == 1 && parts[0] == "bundles" && r.Method == http.MethodPost:
		s.handleManagedBundleEnsure(w, r, installID)
	case len(parts) == 3 && parts[0] == "bundles" && r.Method == http.MethodGet:
		s.handleManagedBundleGet(w, r, installID, parts[1], parts[2])
	default:
		http.Error(w, "managed tenant callback not found", http.StatusNotFound)
	}
}

func (s *Server) handleManagedTenantEnsure(w http.ResponseWriter, r *http.Request, installID int64) {
	var body sdk.ManagedTenantRequest
	r.Body = http.MaxBytesReader(w, r.Body, managedControlBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.TenantID = strings.TrimSpace(body.TenantID)
	body.AccountID = strings.TrimSpace(body.AccountID)
	if !validManagedID(body.TenantID) {
		http.Error(w, "tenant_id is required and may contain letters, numbers, dash, underscore, dot, or colon", http.StatusBadRequest)
		return
	}
	var owner int64
	err := s.store.db.QueryRow(`SELECT owner_app_install_id FROM managed_tenants WHERE tenant_id=?`, body.TenantID).Scan(&owner)
	if err == nil && owner != installID {
		http.Error(w, "tenant_id is owned by another app installation", http.StatusConflict)
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "lookup tenant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := s.store.db.Exec(`INSERT INTO managed_tenants(tenant_id,account_id,owner_app_install_id,status,updated_at)
		VALUES(?,?,?,'active',CURRENT_TIMESTAMP)
		ON CONFLICT(tenant_id) DO UPDATE SET account_id=excluded.account_id,status='active',updated_at=CURRENT_TIMESTAMP
		WHERE managed_tenants.owner_app_install_id=excluded.owner_app_install_id`, body.TenantID, body.AccountID, installID)
	if err != nil {
		http.Error(w, "ensure tenant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil || rowsAffected != 1 {
		http.Error(w, "tenant_id is owned by another app installation", http.StatusConflict)
		return
	}
	var status, updated string
	if err := s.store.db.QueryRow(`SELECT status,updated_at FROM managed_tenants WHERE tenant_id=?`, body.TenantID).Scan(&status, &updated); err != nil {
		http.Error(w, "load tenant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditManagedTenant(body.TenantID, "app:"+strconv.FormatInt(installID, 10), "tenant.ensured", "", "success", "")
	writeJSON(w, sdk.ManagedTenant{TenantID: body.TenantID, AccountID: body.AccountID, Status: status, UpdatedAt: parseManagedTime(updated)})
}

func (s *Server) handleManagedBundleGet(w http.ResponseWriter, r *http.Request, installID int64, tenantID, bundleID string) {
	if !validManagedID(tenantID) || !validManagedID(bundleID) {
		http.Error(w, "invalid tenant or bundle id", http.StatusBadRequest)
		return
	}
	var owner, revision int64
	var desired, status, lastError, updated string
	err := s.store.db.QueryRow(`SELECT t.owner_app_install_id,b.revision,b.desired_json,b.status,b.last_error,b.updated_at FROM managed_tenant_bundles b JOIN managed_tenants t ON t.tenant_id=b.tenant_id WHERE b.tenant_id=? AND b.bundle_id=?`, tenantID, bundleID).Scan(&owner, &revision, &desired, &status, &lastError, &updated)
	if err != nil || owner != installID {
		http.Error(w, "bundle not found", http.StatusNotFound)
		return
	}
	out := sdk.ManagedTenantBundle{TenantID: tenantID, BundleID: bundleID, Revision: revision, Status: status, LastError: lastError, UpdatedAt: parseManagedTime(updated)}
	_ = json.Unmarshal([]byte(desired), &out.Apps)
	writeJSON(w, out)
}

func (s *Server) handleManagedEnrollmentCreate(w http.ResponseWriter, r *http.Request, installID int64) {
	var body sdk.ManagedTenantEnrollmentRequest
	r.Body = http.MaxBytesReader(w, r.Body, managedControlBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.TenantID = strings.TrimSpace(body.TenantID)
	body.AccountID = strings.TrimSpace(body.AccountID)
	if !validManagedID(body.TenantID) {
		http.Error(w, "tenant_id is required and may contain letters, numbers, dash, underscore, dot, or colon", http.StatusBadRequest)
		return
	}
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	if ttl < time.Minute || ttl > 24*time.Hour {
		http.Error(w, "expires_in_seconds must be between 60 and 86400", http.StatusBadRequest)
		return
	}
	var owner int64
	err := s.store.db.QueryRow(`SELECT owner_app_install_id FROM managed_tenants WHERE tenant_id=?`, body.TenantID).Scan(&owner)
	if err == nil && owner != installID {
		http.Error(w, "tenant_id is owned by another app installation", http.StatusConflict)
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "lookup tenant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ticket := "apte_" + generateToken(32)
	expires := time.Now().UTC().Add(ttl)
	tx, err := s.store.db.Begin()
	if err == nil {
		_, err = tx.Exec(`INSERT INTO managed_tenants(tenant_id,account_id,owner_app_install_id,status,updated_at)
			VALUES(?,?,?,'pending',CURRENT_TIMESTAMP)
			ON CONFLICT(tenant_id) DO UPDATE SET account_id=excluded.account_id,status='pending',updated_at=CURRENT_TIMESTAMP`, body.TenantID, body.AccountID, installID)
	}
	if err == nil {
		_, err = tx.Exec(`INSERT INTO managed_tenant_enrollments(tenant_id,ticket_hash,expires_at,used_at,created_at)
			VALUES(?,?,?,NULL,CURRENT_TIMESTAMP)
			ON CONFLICT(tenant_id) DO UPDATE SET ticket_hash=excluded.ticket_hash,expires_at=excluded.expires_at,used_at=NULL,created_at=CURRENT_TIMESTAMP`, body.TenantID, managedTokenHash(ticket), expires)
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		http.Error(w, "create enrollment: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "create enrollment: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditManagedTenant(body.TenantID, "app:"+strconv.FormatInt(installID, 10), "enrollment.created", "", "success", "")
	writeJSON(w, sdk.ManagedTenantEnrollment{TenantID: body.TenantID, AccountID: body.AccountID, Ticket: ticket, ExpiresAt: expires})
}

func (s *Server) handleManagedGrantEnsure(w http.ResponseWriter, r *http.Request, installID, userID int64) {
	var body sdk.ManagedConnectionGrantRequest
	r.Body = http.MaxBytesReader(w, r.Body, managedControlBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.TenantID, body.GrantID, body.AppSlug = strings.TrimSpace(body.TenantID), strings.TrimSpace(body.GrantID), strings.TrimSpace(body.AppSlug)
	if !validManagedID(body.TenantID) || !validManagedID(body.GrantID) || body.ConnectionID <= 0 || body.AppSlug == "" {
		http.Error(w, "tenant_id, grant_id, connection_id, and app_slug are required", http.StatusBadRequest)
		return
	}
	if len(body.AllowedTools) == 0 {
		http.Error(w, "allowed_tools must be non-empty", http.StatusBadRequest)
		return
	}
	var tenantOwner int64
	if err := s.store.db.QueryRow(`SELECT owner_app_install_id FROM managed_tenants WHERE tenant_id=?`, body.TenantID).Scan(&tenantOwner); err != nil || tenantOwner != installID {
		http.Error(w, "managed tenant not owned by this app", http.StatusForbidden)
		return
	}
	conn, _, err := s.store.GetConnection(userID, body.ConnectionID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	if conn.OwnerAppInstallID != installID || conn.CredentialManagement != "app" || conn.CredentialExportPolicy != "never" || conn.Status != "active" {
		http.Error(w, "only active, non-exportable connections owned by this app may be delegated", http.StatusForbidden)
		return
	}
	if conn.AppSlug != body.AppSlug {
		http.Error(w, "app_slug does not match connection", http.StatusBadRequest)
		return
	}
	normalizedTools, err := s.normalizeManagedAllowedTools(body.AppSlug, body.AllowedTools)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.AllowedTools = normalizedTools
	allowedJSON, _ := json.Marshal(body.AllowedTools)
	publicJSON, _ := json.Marshal(body.PublicFields)
	constraintsJSON, _ := json.Marshal(body.Constraints)
	var tokenEncrypted, existingStatus string
	err = s.store.db.QueryRow(`SELECT token_encrypted,status FROM managed_connection_grants WHERE tenant_id=? AND grant_id=? AND owner_app_install_id=?`, body.TenantID, body.GrantID, installID).Scan(&tokenEncrypted, &existingStatus)
	if errors.Is(err, sql.ErrNoRows) {
		token := "aptg_" + generateToken(32)
		tokenEncrypted, err = Encrypt(s.secret, token)
		if err == nil {
			_, err = s.store.db.Exec(`INSERT INTO managed_connection_grants
				(grant_id,tenant_id,owner_app_install_id,connection_id,app_slug,project_id,status,token_hash,token_encrypted,allowed_tools_json,public_fields_json,constraints_json)
				VALUES(?,?,?,?,?,?,'active',?,?,?,?,?)`, body.GrantID, body.TenantID, installID, body.ConnectionID, body.AppSlug, body.ProjectID, managedTokenHash(token), tokenEncrypted, string(allowedJSON), string(publicJSON), string(constraintsJSON))
		}
	} else if err == nil {
		if existingStatus == "revoked" {
			token := "aptg_" + generateToken(32)
			tokenEncrypted, err = Encrypt(s.secret, token)
			if err == nil {
				_, err = s.store.db.Exec(`UPDATE managed_connection_grants SET connection_id=?,app_slug=?,project_id=?,status='active',token_hash=?,token_encrypted=?,allowed_tools_json=?,public_fields_json=?,constraints_json=?,updated_at=CURRENT_TIMESTAMP
					WHERE tenant_id=? AND grant_id=? AND owner_app_install_id=?`, body.ConnectionID, body.AppSlug, body.ProjectID, managedTokenHash(token), tokenEncrypted, string(allowedJSON), string(publicJSON), string(constraintsJSON), body.TenantID, body.GrantID, installID)
			}
		} else {
			_, err = s.store.db.Exec(`UPDATE managed_connection_grants SET connection_id=?,app_slug=?,project_id=?,status='active',allowed_tools_json=?,public_fields_json=?,constraints_json=?,updated_at=CURRENT_TIMESTAMP
				WHERE tenant_id=? AND grant_id=? AND owner_app_install_id=?`, body.ConnectionID, body.AppSlug, body.ProjectID, string(allowedJSON), string(publicJSON), string(constraintsJSON), body.TenantID, body.GrantID, installID)
		}
	}
	if err != nil {
		http.Error(w, "ensure grant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditManagedTenant(body.TenantID, "app:"+strconv.FormatInt(installID, 10), "grant.ensured", body.GrantID, "success", "")
	writeJSON(w, sdk.ManagedConnectionGrant{TenantID: body.TenantID, GrantID: body.GrantID, ConnectionID: body.ConnectionID, AppSlug: body.AppSlug, ProjectID: body.ProjectID, Status: "active", AllowedTools: uniqueNonEmpty(body.AllowedTools), PublicFields: body.PublicFields, Constraints: body.Constraints, UpdatedAt: time.Now().UTC()})
}

func (s *Server) handleManagedGrantDelivery(w http.ResponseWriter, _ *http.Request, installID int64, tenantID, grantID string) {
	w.Header().Set("Cache-Control", "no-store")
	if !validManagedID(tenantID) || !validManagedID(grantID) {
		http.Error(w, "invalid tenant or grant id", http.StatusBadRequest)
		return
	}
	var out sdk.ManagedConnectionGrantDelivery
	var status, tokenEncrypted, toolsRaw, publicRaw, constraintsRaw string
	err := s.store.db.QueryRow(`SELECT connection_id,app_slug,project_id,status,token_encrypted,allowed_tools_json,public_fields_json,constraints_json
		FROM managed_connection_grants WHERE tenant_id=? AND grant_id=? AND owner_app_install_id=?`, tenantID, grantID, installID).
		Scan(&out.ConnectionID, &out.AppSlug, &out.ProjectID, &status, &tokenEncrypted, &toolsRaw, &publicRaw, &constraintsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "grant not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load grant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if status != "active" {
		http.Error(w, "grant is not active", http.StatusConflict)
		return
	}
	out.ControllerToken, err = Decrypt(s.secret, tokenEncrypted)
	if err != nil {
		http.Error(w, "decrypt grant token", http.StatusInternalServerError)
		return
	}
	out.TenantID = tenantID
	out.GrantID = grantID
	out.ControllerExecute = s.publicBaseURL() + "/api/managed/grants/" + url.PathEscape(grantID) + "/execute"
	_ = json.Unmarshal([]byte(toolsRaw), &out.AllowedTools)
	_ = json.Unmarshal([]byte(publicRaw), &out.PublicFields)
	_ = json.Unmarshal([]byte(constraintsRaw), &out.Constraints)
	s.auditManagedTenant(tenantID, "app:"+strconv.FormatInt(installID, 10), "grant.delivered", grantID, "success", "")
	writeJSON(w, out)
}

func (s *Server) handleManagedGrantRevoke(w http.ResponseWriter, r *http.Request, installID int64, tenantID, grantID string) {
	if !validManagedID(tenantID) || !validManagedID(grantID) {
		http.Error(w, "invalid tenant or grant id", http.StatusBadRequest)
		return
	}
	res, err := s.store.db.Exec(`UPDATE managed_connection_grants SET status='revoked',updated_at=CURRENT_TIMESTAMP WHERE tenant_id=? AND grant_id=? AND owner_app_install_id=?`, tenantID, grantID, installID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		http.Error(w, "grant not found", http.StatusNotFound)
		return
	}
	s.auditManagedTenant(tenantID, "app:"+strconv.FormatInt(installID, 10), "grant.revoked", grantID, "success", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleManagedBundleEnsure(w http.ResponseWriter, r *http.Request, installID int64) {
	var body sdk.ManagedTenantBundleRequest
	r.Body = http.MaxBytesReader(w, r.Body, managedControlBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.TenantID, body.BundleID = strings.TrimSpace(body.TenantID), strings.TrimSpace(body.BundleID)
	if !validManagedID(body.TenantID) || !validManagedID(body.BundleID) || len(body.Apps) > 64 {
		http.Error(w, "valid tenant_id and bundle_id are required; apps is limited to 64", http.StatusBadRequest)
		return
	}
	var owner int64
	if err := s.store.db.QueryRow(`SELECT owner_app_install_id FROM managed_tenants WHERE tenant_id=?`, body.TenantID).Scan(&owner); err != nil || owner != installID {
		http.Error(w, "managed tenant not owned by this app", http.StatusForbidden)
		return
	}
	for i := range body.Apps {
		body.Apps[i].Key = strings.TrimSpace(body.Apps[i].Key)
		if !validManagedID(body.Apps[i].Key) || (strings.TrimSpace(body.Apps[i].ManifestURL) == "") == (strings.TrimSpace(body.Apps[i].ManifestYAML) == "") {
			http.Error(w, "each app needs a valid key and exactly one of manifest_url or manifest_yaml", http.StatusBadRequest)
			return
		}
	}
	desired, _ := json.Marshal(body.Apps)
	_, err := s.store.db.Exec(`INSERT INTO managed_tenant_bundles(tenant_id,bundle_id,revision,desired_json,status,last_error,created_by_install_id)
		VALUES(?,?,1,?,'pending','',?)
		ON CONFLICT(tenant_id,bundle_id) DO UPDATE SET revision=managed_tenant_bundles.revision+1,desired_json=excluded.desired_json,status='pending',last_error='',created_by_install_id=excluded.created_by_install_id,updated_at=CURRENT_TIMESTAMP`, body.TenantID, body.BundleID, string(desired), installID)
	if err != nil {
		http.Error(w, "ensure bundle: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var revision int64
	var updated string
	_ = s.store.db.QueryRow(`SELECT revision,updated_at FROM managed_tenant_bundles WHERE tenant_id=? AND bundle_id=?`, body.TenantID, body.BundleID).Scan(&revision, &updated)
	s.auditManagedTenant(body.TenantID, "app:"+strconv.FormatInt(installID, 10), "bundle.ensured", body.BundleID, "success", "")
	writeJSON(w, sdk.ManagedTenantBundle{TenantID: body.TenantID, BundleID: body.BundleID, Revision: revision, Status: "pending", Apps: body.Apps, UpdatedAt: parseManagedTime(updated)})
}

// handleManagedPublic serves the tenant-authenticated control-plane surface.
func (s *Server) handleManagedPublic(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/managed/"), "/")
	parts := strings.Split(rest, "/")
	switch {
	case rest == "enroll" && r.Method == http.MethodPost:
		s.handleManagedPublicEnroll(w, r)
	case rest == "reconcile" && r.Method == http.MethodGet:
		s.handleManagedPublicReconcile(w, r)
	case rest == "reconcile/report" && r.Method == http.MethodPost:
		s.handleManagedPublicReport(w, r)
	case len(parts) == 3 && parts[0] == "grants" && parts[2] == "execute" && r.Method == http.MethodPost:
		s.handleManagedPublicGrantExecute(w, r, parts[1])
	default:
		http.Error(w, "managed endpoint not found", http.StatusNotFound)
	}
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return ""
}

func (s *Server) handleManagedPublicEnroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ticket string `json:"ticket"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Ticket) == "" {
		http.Error(w, "ticket is required", http.StatusBadRequest)
		return
	}
	hash := managedTokenHash(strings.TrimSpace(body.Ticket))
	var tenantID, expected string
	var expires time.Time
	var used sql.NullTime
	err := s.store.db.QueryRow(`SELECT tenant_id,ticket_hash,expires_at,used_at FROM managed_tenant_enrollments WHERE ticket_hash=?`, hash).Scan(&tenantID, &expected, &expires, &used)
	if err != nil || subtle.ConstantTimeCompare([]byte(hash), []byte(expected)) != 1 || used.Valid || time.Now().UTC().After(expires) {
		http.Error(w, "invalid or expired enrollment ticket", http.StatusUnauthorized)
		return
	}
	identity := "apti_" + generateToken(32)
	tx, err := s.store.db.Begin()
	if err == nil {
		res, xerr := tx.Exec(`UPDATE managed_tenant_enrollments SET used_at=CURRENT_TIMESTAMP WHERE tenant_id=? AND used_at IS NULL AND expires_at>CURRENT_TIMESTAMP`, tenantID)
		err = xerr
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				err = errors.New("enrollment ticket was already used")
			}
		}
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE managed_tenants SET identity_token_hash=?,status='active',last_seen_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE tenant_id=?`, managedTokenHash(identity), tenantID)
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		http.Error(w, "enrollment failed", http.StatusConflict)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "enrollment failed", http.StatusInternalServerError)
		return
	}
	s.auditManagedTenant(tenantID, "tenant", "tenant.enrolled", "", "success", "")
	writeJSON(w, managedTenantIdentity{TenantID: tenantID, ControllerURL: s.publicBaseURL(), Token: identity})
}

func (s *Server) authenticateManagedTenant(r *http.Request) (string, bool) {
	token := bearerToken(r)
	if token == "" {
		return "", false
	}
	hash := managedTokenHash(token)
	var tenantID, expected string
	if err := s.store.db.QueryRow(`SELECT tenant_id,identity_token_hash FROM managed_tenants WHERE identity_token_hash=? AND status='active'`, hash).Scan(&tenantID, &expected); err != nil {
		return "", false
	}
	return tenantID, subtle.ConstantTimeCompare([]byte(hash), []byte(expected)) == 1
}

func (s *Server) handleManagedPublicReconcile(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.authenticateManagedTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_, _ = s.store.db.Exec(`UPDATE managed_tenants SET last_seen_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE tenant_id=?`, tenantID)
	out := managedReconcileResponse{TenantID: tenantID, Grants: []managedReconcileGrant{}, RevokedGrants: []string{}, Bundles: []sdk.ManagedTenantBundle{}}
	rows, err := s.store.db.Query(`SELECT grant_id,connection_id,app_slug,project_id,token_encrypted,allowed_tools_json,public_fields_json,constraints_json FROM managed_connection_grants WHERE tenant_id=? AND status='active' ORDER BY grant_id`, tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var g managedReconcileGrant
		var tokenEnc, toolsRaw, publicRaw, constraintsRaw string
		if err := rows.Scan(&g.GrantID, &g.ConnectionID, &g.AppSlug, &g.ProjectID, &tokenEnc, &toolsRaw, &publicRaw, &constraintsRaw); err != nil {
			rows.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		g.TenantID = tenantID
		g.ControllerToken, err = Decrypt(s.secret, tokenEnc)
		if err != nil {
			rows.Close()
			http.Error(w, "decrypt grant token", http.StatusInternalServerError)
			return
		}
		g.ControllerExecute = s.publicBaseURL() + "/api/managed/grants/" + url.PathEscape(g.GrantID) + "/execute"
		_ = json.Unmarshal([]byte(toolsRaw), &g.AllowedTools)
		_ = json.Unmarshal([]byte(publicRaw), &g.PublicFields)
		_ = json.Unmarshal([]byte(constraintsRaw), &g.Constraints)
		out.Grants = append(out.Grants, g)
	}
	rows.Close()
	revokedRows, err := s.store.db.Query(`SELECT grant_id FROM managed_connection_grants WHERE tenant_id=? AND status='revoked' ORDER BY grant_id`, tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for revokedRows.Next() {
		var id string
		if err := revokedRows.Scan(&id); err != nil {
			revokedRows.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out.RevokedGrants = append(out.RevokedGrants, id)
	}
	revokedRows.Close()
	rows, err = s.store.db.Query(`SELECT bundle_id,revision,desired_json,status,last_error,updated_at FROM managed_tenant_bundles WHERE tenant_id=? ORDER BY bundle_id`, tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var b sdk.ManagedTenantBundle
		var desired, updated string
		if err := rows.Scan(&b.BundleID, &b.Revision, &desired, &b.Status, &b.LastError, &updated); err != nil {
			rows.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		b.TenantID, b.UpdatedAt = tenantID, parseManagedTime(updated)
		_ = json.Unmarshal([]byte(desired), &b.Apps)
		out.Bundles = append(out.Bundles, b)
	}
	rows.Close()
	writeJSON(w, out)
}

func (s *Server) handleManagedPublicReport(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.authenticateManagedTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		BundleID string `json:"bundle_id"`
		Revision int64  `json:"revision"`
		Status   string `json:"status"`
		Error    string `json:"error,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Status != "applied" && body.Status != "error" {
		http.Error(w, "status must be applied or error", http.StatusBadRequest)
		return
	}
	res, err := s.store.db.Exec(`UPDATE managed_tenant_bundles SET status=?,last_error=?,updated_at=CURRENT_TIMESTAMP WHERE tenant_id=? AND bundle_id=? AND revision=?`, body.Status, truncate(body.Error, 2000), tenantID, body.BundleID, body.Revision)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		http.Error(w, "bundle revision not found", http.StatusConflict)
		return
	}
	s.auditManagedTenant(tenantID, "tenant", "bundle.reported", body.BundleID, body.Status, truncate(body.Error, 500))
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleManagedPublicGrantExecute(w http.ResponseWriter, r *http.Request, grantID string) {
	token := bearerToken(r)
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var tenantID, expected, status, appSlug, toolsRaw, constraintsRaw string
	var connID int64
	err := s.store.db.QueryRow(`SELECT tenant_id,token_hash,status,connection_id,app_slug,allowed_tools_json,constraints_json FROM managed_connection_grants WHERE grant_id=? AND token_hash=?`, grantID, managedTokenHash(token)).Scan(&tenantID, &expected, &status, &connID, &appSlug, &toolsRaw, &constraintsRaw)
	if err != nil || status != "active" || subtle.ConstantTimeCompare([]byte(expected), []byte(managedTokenHash(token))) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 50<<20)).Decode(&body); err != nil || body.Tool == "" {
		http.Error(w, "tool and valid input are required", http.StatusBadRequest)
		return
	}
	if body.Input == nil {
		body.Input = map[string]any{}
	}
	var allowed []string
	var constraints sdk.ManagedGrantConstraints
	_ = json.Unmarshal([]byte(toolsRaw), &allowed)
	_ = json.Unmarshal([]byte(constraintsRaw), &constraints)
	if !listContainsFold(allowed, body.Tool) {
		s.auditManagedTenant(tenantID, "grant:"+grantID, "grant.execute", body.Tool, "denied", "tool not allowed")
		http.Error(w, "tool is not allowed by grant", http.StatusForbidden)
		return
	}
	if err := validateManagedGrantConstraints(constraints, body.Input); err != nil {
		s.auditManagedTenant(tenantID, "grant:"+grantID, "grant.execute", body.Tool, "denied", err.Error())
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	result, err := s.executeManagedGrantConnection(connID, appSlug, body.Tool, body.Input)
	if err != nil {
		s.auditManagedTenant(tenantID, "grant:"+grantID, "grant.execute", body.Tool, "error", truncate(err.Error(), 500))
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"success": false, "data": err.Error()})
		return
	}
	s.auditManagedTenant(tenantID, "grant:"+grantID, "grant.execute", body.Tool, "success", "")
	writeJSON(w, result)
}

func (s *Server) executeManagedGrantConnection(connID int64, appSlug, toolName string, input map[string]any) (*ExecuteResult, error) {
	var userID int64
	if err := s.store.db.QueryRow(`SELECT user_id FROM connections WHERE id=?`, connID).Scan(&userID); err != nil {
		return nil, errors.New("parent connection not found")
	}
	conn, encrypted, err := s.store.GetConnection(userID, connID)
	if err != nil || conn.Status != "active" || conn.AppSlug != appSlug || conn.CredentialManagement != "app" || conn.CredentialExportPolicy != "never" {
		return nil, errors.New("parent managed connection is unavailable")
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		return nil, errors.New("integration catalog entry not found")
	}
	prefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	var tool *AppToolDef
	for i := range app.Tools {
		if app.Tools[i].Name == toolName || prefix+"_"+app.Tools[i].Name == toolName || conn.AppSlug+"_"+app.Tools[i].Name == toolName {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		return nil, errors.New("integration tool not found")
	}
	plain, err := Decrypt(s.secret, encrypted)
	if err != nil {
		return nil, errors.New("decrypt parent connection")
	}
	credentials := map[string]string{}
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		return nil, errors.New("decode parent connection")
	}
	ctx, err := s.resolveConnectionContext(userID, app, credentials, input)
	if err != nil {
		return nil, err
	}
	persistID := connID
	if ctx.MasterConnID != 0 {
		persistID = ctx.MasterConnID
	}
	persist := func(updated map[string]string) error {
		raw, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		enc, err := Encrypt(s.secret, string(raw))
		if err != nil {
			return err
		}
		return s.store.UpdateConnectionCredentials(persistID, enc)
	}
	if err := s.prepareIntegrationExternalFetch(ctx.App, tool, ctx.Credentials, ctx.Input); err != nil {
		return nil, err
	}
	result, err := executeIntegrationToolWithRefresh(ctx.App, tool, ctx.Credentials, ctx.Input, "", persist)
	s.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "managed-tenant", tool.Name, input, result, err))
	return result, err
}

func validateManagedGrantConstraints(c sdk.ManagedGrantConstraints, input map[string]any) error {
	for _, field := range c.DeniedFields {
		if _, ok := input[field]; ok {
			return fmt.Errorf("field %q is denied by grant", field)
		}
	}
	for field, want := range c.FixedInput {
		got, ok := input[field]
		if !ok {
			input[field] = want
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return fmt.Errorf("field %q must equal the controller-defined value", field)
		}
	}
	for field, allowed := range c.AllowedValues {
		got, ok := input[field]
		if !ok {
			continue
		}
		if !listContainsFold(allowed, fmt.Sprint(got)) {
			return fmt.Errorf("field %q is outside the grant allow-list", field)
		}
	}
	return nil
}

func (s *Server) auditManagedTenant(tenantID, actor, action, resource, status, detail string) {
	_, _ = s.store.db.Exec(`INSERT INTO managed_tenant_audit(tenant_id,actor,action,resource,status,detail) VALUES(?,?,?,?,?,?)`, tenantID, actor, action, resource, status, detail)
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		key := strings.ToLower(v)
		if v != "" && !seen[key] {
			seen[key] = true
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) normalizeManagedAllowedTools(appSlug string, requested []string) ([]string, error) {
	if s.catalog == nil {
		return nil, errors.New("integration catalog unavailable")
	}
	app := s.catalog.Get(appSlug)
	if app == nil {
		return nil, errors.New("integration catalog entry not found")
	}
	known := map[string]string{}
	for _, tool := range app.Tools {
		known[strings.ToLower(tool.Name)] = tool.Name
		known[strings.ToLower(appSlug+"_"+tool.Name)] = tool.Name
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range uniqueNonEmpty(requested) {
		canonical, ok := known[strings.ToLower(raw)]
		if !ok {
			return nil, fmt.Errorf("tool %q is not defined by integration %q", raw, appSlug)
		}
		if !seen[canonical] {
			seen[canonical] = true
			out = append(out, canonical)
		}
	}
	return out, nil
}

func parseManagedTime(raw string) time.Time { t, _ := parseTime(raw); return t }

// startManagedTenantReconciler activates only when apteva.yaml contains a
// managed.controller_url and either an enrollment ticket or saved identity.
func (s *Server) startManagedTenantReconciler(ctx context.Context, cfg aptevaManagedConfig) {
	if strings.TrimSpace(cfg.ControllerURL) == "" {
		return
	}
	go func() {
		interval := time.Duration(cfg.IntervalSeconds) * time.Second
		if interval < 10*time.Second {
			interval = 30 * time.Second
		}
		for {
			if err := s.reconcileManagedTenant(ctx, cfg); err != nil {
				log.Printf("[MANAGED] reconcile: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

func (s *Server) reconcileManagedTenant(ctx context.Context, cfg aptevaManagedConfig) error {
	identityPath := cfg.IdentityFile
	if identityPath == "" {
		identityPath = filepath.Join(s.dataDir, "managed-identity.json")
	} else if !filepath.IsAbs(identityPath) {
		identityPath = filepath.Join(s.dataDir, identityPath)
	}
	identity, err := readManagedIdentity(identityPath)
	if errors.Is(err, os.ErrNotExist) {
		ticketPath := cfg.EnrollmentTokenFile
		if ticketPath == "" {
			return errors.New("managed enrollment_token_file is required for first enrollment")
		}
		if !filepath.IsAbs(ticketPath) {
			ticketPath = filepath.Join(s.dataDir, ticketPath)
		}
		ticketRaw, readErr := os.ReadFile(ticketPath)
		if readErr != nil {
			return fmt.Errorf("read enrollment ticket: %w", readErr)
		}
		identity, err = enrollManagedTenant(ctx, cfg.ControllerURL, strings.TrimSpace(string(ticketRaw)))
		if err != nil {
			return err
		}
		if err := writeManagedIdentity(identityPath, identity); err != nil {
			return err
		}
		if err := os.Remove(ticketPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove consumed enrollment ticket: %w", err)
		}
	} else if err != nil {
		return err
	}
	controller := strings.TrimRight(cfg.ControllerURL, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, controller+"/api/managed/reconcile", nil)
	req.Header.Set("Authorization", "Bearer "+identity.Token)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("controller reconcile returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var desired managedReconcileResponse
	if err := json.NewDecoder(resp.Body).Decode(&desired); err != nil {
		return err
	}
	if desired.TenantID != identity.TenantID {
		return errors.New("controller returned a different tenant identity")
	}
	grantIDs := map[string]int64{}
	for _, grant := range desired.Grants {
		id, err := s.applyManagedGrant(identity.TenantID, grant)
		if err != nil {
			return err
		}
		grantIDs[grant.GrantID] = id
	}
	for _, grantID := range desired.RevokedGrants {
		if _, err := s.store.db.Exec(`UPDATE connections SET status='failed' WHERE external_id=? AND credential_management='controller'`, "managed:"+identity.TenantID+":"+grantID); err != nil {
			return err
		}
	}
	for _, bundle := range desired.Bundles {
		applyErr := s.applyManagedBundle(bundle, grantIDs)
		status, detail := "applied", ""
		if applyErr != nil {
			status, detail = "error", applyErr.Error()
		}
		if err := reportManagedBundle(ctx, controller, identity.Token, bundle.BundleID, bundle.Revision, status, detail); err != nil {
			return err
		}
		if applyErr != nil {
			return applyErr
		}
	}
	return nil
}

func enrollManagedTenant(ctx context.Context, controller, ticket string) (*managedTenantIdentity, error) {
	raw, _ := json.Marshal(map[string]string{"ticket": ticket})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(controller, "/")+"/api/managed/enroll", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("enrollment returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out managedTenantIdentity
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.TenantID == "" || out.Token == "" {
		return nil, errors.New("incomplete enrollment response")
	}
	return &out, nil
}

func readManagedIdentity(path string) (*managedTenantIdentity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out managedTenantIdentity
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.TenantID == "" || out.Token == "" {
		return nil, errors.New("managed identity is incomplete")
	}
	return &out, nil
}

func writeManagedIdentity(path string, identity *managedTenantIdentity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) applyManagedGrant(tenantID string, grant managedReconcileGrant) (int64, error) {
	if grant.TenantID != tenantID || grant.GrantID == "" || grant.ControllerToken == "" || grant.ControllerExecute == "" {
		return 0, errors.New("invalid managed grant")
	}
	userID, err := s.firstPlatformAdmin()
	if err != nil {
		return 0, err
	}
	if grant.ProjectID == "default" {
		grant.ProjectID = s.defaultManagedProject(userID)
	}
	fields := map[string]string{delegatedProviderMarker: "1", "grant_id": grant.GrantID, "resource": "provider.connection", "controller_execute_url": grant.ControllerExecute, "controller_token": grant.ControllerToken, "parent_connection_id": strconv.FormatInt(grant.ConnectionID, 10), "allowed_tools": strings.Join(grant.AllowedTools, ",")}
	constraints, _ := json.Marshal(grant.Constraints)
	fields["constraints"] = string(constraints)
	for k, v := range grant.PublicFields {
		if !strings.HasPrefix(k, "_") {
			fields[k] = v
		}
	}
	raw, _ := json.Marshal(fields)
	encrypted, err := Encrypt(s.secret, string(raw))
	if err != nil {
		return 0, err
	}
	externalID := "managed:" + tenantID + ":" + grant.GrantID
	var id int64
	err = s.store.db.QueryRow(`SELECT id FROM connections WHERE user_id=? AND external_id=?`, userID, externalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		appName := grant.AppSlug
		if s.catalog != nil {
			if app := s.catalog.Get(grant.AppSlug); app != nil && app.Name != "" {
				appName = app.Name
			}
		}
		conn, createErr := s.store.CreateConnectionExt(ConnectionInput{UserID: userID, AppSlug: grant.AppSlug, AppName: appName, Name: "Managed " + grant.AppSlug, AuthType: "delegated", EncryptedCreds: encrypted, ProjectID: grant.ProjectID, Source: "managed_controller", Status: "active", ExternalID: externalID, CreatedVia: "managed_controller", CredentialManagement: "controller", CredentialExportPolicy: "never"})
		if createErr != nil {
			return 0, createErr
		}
		return conn.ID, nil
	}
	if err != nil {
		return 0, err
	}
	_, err = s.store.db.Exec(`UPDATE connections SET encrypted_credentials=?,status='active',project_id=?,credential_management='controller',credential_export_policy='never' WHERE id=? AND user_id=?`, encrypted, grant.ProjectID, id, userID)
	return id, err
}

func (s *Server) firstPlatformAdmin() (int64, error) {
	var id int64
	err := s.store.db.QueryRow(`SELECT id FROM users WHERE COALESCE(role,'user')='admin' ORDER BY id LIMIT 1`).Scan(&id)
	return id, err
}

func (s *Server) defaultManagedProject(userID int64) string {
	var id string
	_ = s.store.db.QueryRow(`SELECT id FROM projects WHERE user_id=? ORDER BY created_at,id LIMIT 1`, userID).Scan(&id)
	return id
}

func (s *Server) applyManagedBundle(bundle sdk.ManagedTenantBundle, grantIDs map[string]int64) error {
	userID, err := s.firstPlatformAdmin()
	if err != nil {
		return err
	}
	for _, desired := range bundle.Apps {
		manifestRaw, err := s.fetchManifestBytes(desired.ManifestURL, desired.ManifestYAML)
		if err != nil {
			return fmt.Errorf("%s: fetch manifest: %w", desired.Key, err)
		}
		manifest, err := sdk.ParseManifest(manifestRaw)
		if err != nil {
			return fmt.Errorf("%s: parse manifest: %w", desired.Key, err)
		}
		projectID := desired.ProjectID
		if projectID == "default" || (projectID == "" && manifestAllowsScope(manifest, sdk.ScopeProject) && !manifestAllowsScope(manifest, sdk.ScopeGlobal)) {
			projectID = s.defaultManagedProject(userID)
		}
		bindings := map[string]any{}
		for role, grantID := range desired.Bindings {
			connID, ok := grantIDs[grantID]
			if !ok {
				return fmt.Errorf("%s: binding %s references unavailable grant %s", desired.Key, role, grantID)
			}
			bindings[role] = connID
		}
		var installID int64
		var version, currentBindings string
		err = s.store.db.QueryRow(`SELECT i.id,i.version,COALESCE(i.integration_bindings,'{}') FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE a.name=? AND i.project_id=? ORDER BY i.id DESC LIMIT 1`, manifest.Name, projectID).Scan(&installID, &version, &currentBindings)
		if err == nil {
			if version != manifest.Version {
				upgradeReq := httptest.NewRequest(http.MethodPost, "/apps/installs/"+strconv.FormatInt(installID, 10)+"/upgrade", strings.NewReader(`{"approve_new_permissions":true}`))
				upgradeReq.Header.Set("X-User-ID", strconv.FormatInt(userID, 10))
				upgradeRec := httptest.NewRecorder()
				s.handleUpgradeApp(upgradeRec, upgradeReq)
				if upgradeRec.Code/100 != 2 {
					return fmt.Errorf("%s: upgrade %s to %s failed (%d): %s", desired.Key, version, manifest.Version, upgradeRec.Code, strings.TrimSpace(upgradeRec.Body.String()))
				}
			}
			mergedBindings := map[string]any{}
			_ = json.Unmarshal([]byte(currentBindings), &mergedBindings)
			if mergedBindings == nil {
				mergedBindings = map[string]any{}
			}
			// Remove only stale bindings that point at this controller tenant;
			// app-dependency and operator bindings remain untouched.
			for role, rawID := range mergedBindings {
				if _, stillDesired := desired.Bindings[role]; stillDesired {
					continue
				}
				connID, ok := numericBindingID(rawID)
				if !ok {
					continue
				}
				var externalID string
				_ = s.store.db.QueryRow(`SELECT COALESCE(external_id,'') FROM connections WHERE id=?`, connID).Scan(&externalID)
				if strings.HasPrefix(externalID, "managed:"+bundle.TenantID+":") {
					delete(mergedBindings, role)
				}
			}
			for role, connID := range bindings {
				mergedBindings[role] = connID
			}
			bindingsRaw, _ := json.Marshal(mergedBindings)
			if string(bindingsRaw) != currentBindings {
				if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=?`, string(bindingsRaw), installID); err != nil {
					return err
				}
				if s.installedApps != nil {
					s.LoadInstalledApps()
				}
			}
			if err := s.applyManagedInstallConfig(installID, manifest, desired.Config); err != nil {
				return fmt.Errorf("%s: config: %w", desired.Key, err)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"manifest_url": desired.ManifestURL, "manifest_yaml": desired.ManifestYAML, "project_id": projectID, "config": desired.Config, "bindings": bindings})
		req := httptest.NewRequest(http.MethodPost, "/apps/install", bytes.NewReader(payload))
		req.Header.Set("X-User-ID", strconv.FormatInt(userID, 10))
		rec := httptest.NewRecorder()
		s.handleInstallApp(rec, req)
		if rec.Code/100 != 2 {
			return fmt.Errorf("%s: install failed (%d): %s", desired.Key, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	return nil
}

func numericBindingID(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), v > 0 && v == float64(int64(v))
	case int64:
		return v, v > 0
	case json.Number:
		id, err := v.Int64()
		return id, err == nil && id > 0
	default:
		return 0, false
	}
}

func (s *Server) applyManagedInstallConfig(installID int64, manifest *sdk.Manifest, desired map[string]string) error {
	if desired == nil {
		desired = map[string]string{}
	}
	known := map[string]bool{}
	for _, field := range manifest.ConfigSchema {
		known[field.Name] = true
	}
	for key := range desired {
		if !known[key] {
			return fmt.Errorf("unknown config key %q", key)
		}
	}
	raw, _ := json.Marshal(desired)
	var currentEncrypted string
	_ = s.store.db.QueryRow(`SELECT COALESCE(config_encrypted,'') FROM app_installs WHERE id=?`, installID).Scan(&currentEncrypted)
	if currentEncrypted != "" {
		if current, err := Decrypt(s.secret, currentEncrypted); err == nil && current == string(raw) {
			return nil
		}
	}
	encrypted, err := Encrypt(s.secret, string(raw))
	if err != nil {
		return err
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET config_encrypted=? WHERE id=?`, encrypted, installID); err != nil {
		return err
	}
	if s.localApps != nil && s.localApps.PID(installID) > 0 {
		if err := s.RespawnLocalInstall(installID); err != nil {
			return fmt.Errorf("config saved but app restart failed: %w", err)
		}
	}
	return nil
}

func reportManagedBundle(ctx context.Context, controller, token, bundleID string, revision int64, status, detail string) error {
	raw, _ := json.Marshal(map[string]any{"bundle_id": bundleID, "revision": revision, "status": status, "error": detail})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, controller+"/api/managed/reconcile/report", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("bundle report returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
