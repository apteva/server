package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const maxFileMemoryBytes = 256 << 20

func (s *Store) migrateFileReferences() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS app_file_references (
 id TEXT PRIMARY KEY, install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL, attachment_id TEXT NOT NULL, version TEXT NOT NULL,
 filename TEXT NOT NULL, mime_type TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL,
 expires_at INTEGER NOT NULL DEFAULT 0, revoked INTEGER NOT NULL DEFAULT 0,
 UNIQUE(install_id,project_id,attachment_id,version));
 CREATE TABLE IF NOT EXISTS app_file_grants (
 file_id TEXT NOT NULL REFERENCES app_file_references(id) ON DELETE CASCADE,
 agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
 thread_id TEXT NOT NULL, PRIMARY KEY(file_id,agent_id,thread_id));
 CREATE TRIGGER IF NOT EXISTS revoke_thread_file_grants AFTER DELETE ON agent_thread_scopes BEGIN
 DELETE FROM app_file_grants WHERE agent_id=OLD.agent_id AND thread_id=OLD.thread_id; END;
 CREATE TRIGGER IF NOT EXISTS revoke_rebound_thread_file_grants AFTER UPDATE OF project_id,source_install_id ON agent_thread_scopes
 WHEN OLD.project_id<>NEW.project_id OR OLD.source_install_id<>NEW.source_install_id BEGIN
 DELETE FROM app_file_grants WHERE agent_id=OLD.agent_id AND thread_id=OLD.thread_id; END;
 CREATE TABLE IF NOT EXISTS server_blobs (
 id TEXT PRIMARY KEY, owner_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 source_install_id INTEGER NOT NULL DEFAULT 0, project_id TEXT NOT NULL,
 filename TEXT NOT NULL, mime_type TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL,
 data BLOB NOT NULL, revoked INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
 UNIQUE(owner_user_id,source_install_id,project_id,filename,mime_type,sha256));
 CREATE TABLE IF NOT EXISTS server_blob_grants (
 file_id TEXT NOT NULL REFERENCES server_blobs(id) ON DELETE CASCADE,
 agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
 thread_id TEXT NOT NULL, PRIMARY KEY(file_id,agent_id,thread_id));
 CREATE TRIGGER IF NOT EXISTS revoke_thread_blob_grants AFTER DELETE ON agent_thread_scopes BEGIN
 DELETE FROM server_blob_grants WHERE agent_id=OLD.agent_id AND thread_id=OLD.thread_id; END;
 CREATE TRIGGER IF NOT EXISTS revoke_rebound_thread_blob_grants AFTER UPDATE OF project_id,source_install_id ON agent_thread_scopes
 WHEN OLD.project_id<>NEW.project_id OR OLD.source_install_id<>NEW.source_install_id BEGIN
 DELETE FROM server_blob_grants WHERE agent_id=OLD.agent_id AND thread_id=OLD.thread_id; END;`)
	return err
}
func fileProblem(status int, code, message string) *sdk.FileReferenceError {
	return &sdk.FileReferenceError{StatusCode: status, Code: code, Message: message}
}
func fileProblemFromError(err error) *sdk.FileReferenceError {
	var p *sdk.FileReferenceError
	if !errors.As(err, &p) {
		return fileProblem(500, "file_reference_error", "file reference operation failed")
	}
	return p
}
func writeFileProblem(w http.ResponseWriter, err error) {
	p := fileProblemFromError(err)
	writeJSONStatus(w, p.StatusCode, p)
}

func fileID(ref string) (string, error) {
	if !sdk.IsFileReference(ref) {
		return "", fileProblem(400, "invalid_file_reference", "expected an Apteva file reference")
	}
	id := strings.TrimPrefix(ref, sdk.FileReferencePrefix)
	id = strings.TrimPrefix(id, sdk.LegacyFileReferencePrefix)
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 24 {
		return "", fileProblem(400, "invalid_file_reference", "invalid file reference")
	}
	return id, nil
}

type storedFileReference struct {
	sdk.FileReference
	id          string
	installID   int64
	revoked     bool
	serverBlob  bool
	ownerUserID int64
}

func (s *Server) loadFileReference(ref string) (*storedFileReference, error) {
	id, err := fileID(ref)
	if err != nil {
		return nil, err
	}
	f := &storedFileReference{id: id}
	var expiry int64
	err = s.store.db.QueryRow(`SELECT install_id,project_id,attachment_id,version,filename,mime_type,size,sha256,expires_at,revoked FROM app_file_references WHERE id=?`, id).Scan(&f.installID, &f.ProjectID, &f.AttachmentID, &f.Version, &f.Filename, &f.MIMEType, &f.Size, &f.SHA256, &expiry, &f.revoked)
	if err == sql.ErrNoRows {
		f.serverBlob = true
		err = s.store.db.QueryRow(`SELECT source_install_id,owner_user_id,project_id,filename,mime_type,size,sha256,revoked FROM server_blobs WHERE id=?`, id).Scan(&f.installID, &f.ownerUserID, &f.ProjectID, &f.Filename, &f.MIMEType, &f.Size, &f.SHA256, &f.revoked)
		if err == sql.ErrNoRows {
			return nil, fileProblem(404, "file_not_found", "file reference not found")
		}
	}
	if err != nil {
		return nil, err
	}
	f.Ref = sdk.FileReferencePrefix + id
	f.File = true
	f.Revoked = f.revoked
	if expiry != 0 {
		t := time.Unix(expiry, 0).UTC()
		f.ExpiresAt = &t
	}
	return f, nil
}
func fileTextOK(value string, max int) bool {
	return value != "" && len(value) <= max && !strings.ContainsAny(value, "\x00\r\n")
}
func (s *Server) handleCallbackFileReferences(w http.ResponseWriter, r *http.Request, parts []string) {
	principal, ok := r.Context().Value(appCallbackPrincipalKey{}).(appCallbackPrincipal)
	if !ok || principal.installID <= 0 {
		writeFileProblem(w, fileProblem(401, "file_inaccessible", "app authentication required"))
		return
	}
	installID := principal.installID
	if !installHasPermission(s, installID, sdk.PermFileReferences) {
		writeFileProblem(w, fileProblem(403, "permission_denied", "missing platform.files.references permission"))
		return
	}
	if r.Method != http.MethodPost || len(parts) != 1 {
		writeFileProblem(w, fileProblem(405, "invalid_request", "POST action required"))
		return
	}
	var body struct {
		sdk.RegisterFileReferenceRequest
		Ref      string                  `json:"ref"`
		AgentID  int64                   `json:"agent_id"`
		ThreadID string                  `json:"thread_id"`
		Scope    *sdk.FileReferenceScope `json:"scope"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		writeFileProblem(w, fileProblem(400, "invalid_request", "invalid JSON"))
		return
	}
	if parts[0] == "register" {
		_, project, ok := s.runtimeCallerProject(w, r, installID, body.ProjectID, ProjectEditor)
		if !ok {
			return
		}
		body.ProjectID = project
		req := body.RegisterFileReferenceRequest
		digest, err := hex.DecodeString(req.SHA256)
		media, _, mimeErr := mime.ParseMediaType(req.MIMEType)
		if !fileTextOK(req.AttachmentID, 256) || !fileTextOK(req.Version, 128) || !fileTextOK(req.Filename, 255) || strings.ContainsAny(req.Filename, "/\\") || mimeErr != nil || media != req.MIMEType || len(req.MIMEType) > 255 || err != nil || len(digest) != 32 || req.Size < 0 {
			writeFileProblem(w, fileProblem(400, "invalid_request", "immutable attachment ID, version, filename, MIME type, size and SHA-256 required"))
			return
		}
		if req.Size > sdk.MaxFileReferenceBytes {
			writeFileProblem(w, fileProblem(413, "file_too_large", "attachment exceeds 25 MiB"))
			return
		}
		var expiry int64
		if req.ExpiresAt != nil {
			expiry = req.ExpiresAt.Unix()
			if expiry <= time.Now().Unix() {
				writeFileProblem(w, fileProblem(410, "file_expired", "expiry must be in the future"))
				return
			}
		}
		req.SHA256 = strings.ToLower(req.SHA256)
		var random [24]byte
		if _, err = rand.Read(random[:]); err != nil {
			writeFileProblem(w, err)
			return
		}
		id := hex.EncodeToString(random[:])
		_, err = s.store.db.Exec(`INSERT INTO app_file_references(id,install_id,project_id,attachment_id,version,filename,mime_type,size,sha256,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(install_id,project_id,attachment_id,version) DO NOTHING`, id, installID, project, req.AttachmentID, req.Version, req.Filename, req.MIMEType, req.Size, req.SHA256, expiry)
		if err != nil {
			writeFileProblem(w, err)
			return
		}
		if err = s.store.db.QueryRow(`SELECT id FROM app_file_references WHERE install_id=? AND project_id=? AND attachment_id=? AND version=?`, installID, project, req.AttachmentID, req.Version).Scan(&id); err != nil {
			writeFileProblem(w, err)
			return
		}
		f, err := s.loadFileReference(sdk.FileReferencePrefix + id)
		if err != nil {
			writeFileProblem(w, err)
			return
		}
		oldExpiry := int64(0)
		if f.ExpiresAt != nil {
			oldExpiry = f.ExpiresAt.Unix()
		}
		if f.Filename != req.Filename || f.MIMEType != req.MIMEType || f.Size != req.Size || f.SHA256 != req.SHA256 || oldExpiry != expiry {
			writeFileProblem(w, fileProblem(409, "file_version_conflict", "attachment version already registered with different metadata"))
			return
		}
		if f.revoked {
			writeFileProblem(w, fileProblem(410, "file_revoked", "attachment reference revoked; register a new version"))
			return
		}
		writeJSON(w, f.FileReference)
		return
	}
	f, err := s.loadFileReference(body.Ref)
	if err != nil {
		writeFileProblem(w, err)
		return
	}
	if f.installID != installID {
		writeFileProblem(w, fileProblem(403, "file_inaccessible", "reference belongs to another installation"))
		return
	}
	if _, _, ok := s.runtimeCallerProject(w, r, installID, f.ProjectID, ProjectEditor); !ok {
		return
	}
	switch parts[0] {
	case "get":
		writeJSON(w, f.FileReference)
		return
	case "read":
		if body.Scope == nil || body.Scope.AgentID <= 0 || body.Scope.ThreadID == "" {
			writeFileProblem(w, fileProblem(400, "file_context_required", "agent and thread scope required"))
			return
		}
		caller, ok := s.callbackFileCaller(w, r, installID, body.Scope)
		if !ok {
			return
		}
		if err = s.authorizeFileRead(f, caller); err != nil {
			writeFileProblem(w, err)
			return
		}
		var raw []byte
		if f.serverBlob {
			err = s.store.db.QueryRowContext(r.Context(), `SELECT data FROM server_blobs WHERE id=?`, f.id).Scan(&raw)
		} else {
			var resolved map[string]any
			resolved, err = s.readReferencedFile(r.Context(), f, caller)
			if err == nil {
				encoded, _ := resolved["base64"].(string)
				raw, err = base64.StdEncoding.DecodeString(encoded)
			}
		}
		if err != nil {
			writeFileProblem(w, err)
			return
		}
		writeJSON(w, sdk.FileReferenceReadResponse{FileReference: f.FileReference, Data: raw})
		return
	case "revoke":
		if f.serverBlob {
			_, err = s.store.db.Exec(`UPDATE server_blobs SET revoked=1 WHERE id=?`, f.id)
		} else {
			_, err = s.store.db.Exec(`UPDATE app_file_references SET revoked=1 WHERE id=?`, f.id)
		}
	case "grant":
		if f.revoked {
			writeFileProblem(w, fileProblem(410, "file_revoked", "reference revoked"))
			return
		}
		if f.ExpiresAt != nil && !f.ExpiresAt.After(time.Now()) {
			writeFileProblem(w, fileProblem(410, "file_expired", "reference expired"))
			return
		}
		if _, err = s.callbackAgentForInstall(r, installID, body.AgentID); err != nil {
			writeFileProblem(w, fileProblem(403, "file_inaccessible", "agent outside installation scope"))
			return
		}
		if f.serverBlob {
			target, lookupErr := s.store.GetAgentByID(body.AgentID)
			if lookupErr != nil || target == nil || target.UserID != f.ownerUserID {
				writeFileProblem(w, fileProblem(403, "file_inaccessible", "blob grant requires an agent of the same owner"))
				return
			}
		}
		var result sql.Result
		grantTable := "app_file_grants"
		if f.serverBlob {
			grantTable = "server_blob_grants"
		}
		result, err = s.store.db.Exec(`INSERT INTO `+grantTable+`(file_id,agent_id,thread_id)
   SELECT ?,ats.agent_id,ats.thread_id FROM agent_thread_scopes ats JOIN app_agent_bindings b ON b.agent_id=ats.agent_id AND b.install_id=ats.source_install_id
   WHERE ats.agent_id=? AND ats.thread_id=? AND ats.project_id=? AND ats.source_install_id=? AND b.enabled=1
   ON CONFLICT(file_id,agent_id,thread_id) DO UPDATE SET file_id=excluded.file_id`, f.id, body.AgentID, body.ThreadID, f.ProjectID, installID)
		if err == nil {
			n, _ := result.RowsAffected()
			if n == 0 {
				err = fileProblem(403, "file_scope_required", "grant requires an attached agent and a thread owned by this app in this project")
			}
		}
	case "revoke-grant":
		if f.serverBlob {
			_, err = s.store.db.Exec(`DELETE FROM server_blob_grants WHERE file_id=? AND agent_id=? AND thread_id=?`, f.id, body.AgentID, body.ThreadID)
		} else {
			_, err = s.store.db.Exec(`DELETE FROM app_file_grants WHERE file_id=? AND agent_id=? AND thread_id=?`, f.id, body.AgentID, body.ThreadID)
		}
	default:
		err = fileProblem(404, "invalid_request", "unknown file reference action")
	}
	if err != nil {
		writeFileProblem(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// Runtime URLs carry a server-signed agent identity. Raw caller headers and
// model arguments cannot choose an agent. Existing route auth remains required.
func fileCallerSignature(secret string, agentID int64, path, install string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "file-caller-v1:%d:%s:%s", agentID, path, install)
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Server) authorizeFileReferenceMCPConfig(agent *Agent, cfg map[string]any) {
	if agent == nil || s.instanceSecret == "" {
		return
	}
	raw, _ := cfg["url"].(string)
	u, err := url.Parse(raw)
	port := s.port
	if port == "" {
		port = localServerPort()
	}
	if err != nil || u.Port() != port || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return
	}
	if !(strings.HasPrefix(u.Path, "/api/apps/") && strings.HasSuffix(u.Path, "/mcp")) && !strings.HasPrefix(u.Path, "/mcp/") {
		return
	}
	q := u.Query()
	q.Set("file_agent", strconv.FormatInt(agent.ID, 10))
	q.Set("file_auth", fileCallerSignature(s.instanceSecret, agent.ID, u.Path, q.Get("install_id")))
	u.RawQuery = q.Encode()
	cfg["url"] = u.String()
}

type fileCaller struct {
	installID int64
	agentID   int64
	threadID  string
}

func (s *Server) fileReferenceAgent(r *http.Request) (int64, error) {
	q := r.URL.Query()
	id, err := strconv.ParseInt(q.Get("file_agent"), 10, 64)
	signed := q.Get("file_auth")
	valid := hmac.Equal([]byte(signed), []byte(fileCallerSignature(s.instanceSecret, id, r.URL.Path, q.Get("install_id"))))
	// /api is stripped before app routing, while runtime URLs are signed
	// using their public /api/apps/... path. Both identify the same route.
	if !valid && strings.HasPrefix(r.URL.Path, "/apps/") {
		valid = hmac.Equal([]byte(signed), []byte(fileCallerSignature(s.instanceSecret, id, "/api"+r.URL.Path, q.Get("install_id"))))
	}
	if err != nil || id <= 0 || s.instanceSecret == "" || !valid {
		return 0, fileProblem(403, "file_context_required", "file resolution requires a server-authorized agent dispatch")
	}
	return id, nil
}
func (s *Server) fileReferenceCaller(r *http.Request, threadID string) (fileCaller, error) {
	id, err := s.fileReferenceAgent(r)
	if err != nil {
		return fileCaller{}, err
	}
	if !validTrustedMCPIdentity(threadID) {
		return fileCaller{}, fileProblem(403, "file_context_required", "file resolution requires trusted thread context")
	}
	return fileCaller{agentID: id, threadID: threadID}, nil
}

// Source access is identical for app-owned bytes and server blobs associated
// with an app installation. Moving the bytes must not bypass revocation.
func (s *Server) authorizeFileSource(installID int64, project string, agentID int64) error {
	metadata, err := s.appMetadata(installID)
	if err != nil || metadata.status != "running" || !metadata.permissions[sdk.PermFileReferences] || (metadata.project != "" && metadata.project != project) {
		return fileProblem(403, "file_inaccessible", "owning app is inaccessible or permission was revoked")
	}
	if s.store.GetPlatformRole(metadata.owner) != PlatformAdmin && s.effectiveRoleOnProject(metadata.owner, project).Rank() < ProjectViewer.Rank() {
		return fileProblem(403, "file_inaccessible", "owning app no longer has project access")
	}
	var enabled int
	if err = s.store.db.QueryRow(`SELECT enabled FROM app_agent_bindings WHERE install_id=? AND agent_id=?`, installID, agentID).Scan(&enabled); err != nil || enabled != 1 {
		return fileProblem(403, "file_inaccessible", "owning app is no longer attached to this agent")
	}
	return nil
}

func (s *Server) authorizeFileRead(f *storedFileReference, caller fileCaller) error {
	if f.serverBlob {
		return s.authorizeServerBlob(f, caller)
	}
	if caller.installID != 0 && caller.installID != f.installID {
		return fileProblem(403, "file_inaccessible", "file belongs to another app installation")
	}
	// Check grants before disclosing lifecycle state to an unauthorized caller.
	var n int
	err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_file_grants g
 JOIN agent_thread_scopes ats ON ats.agent_id=g.agent_id AND ats.thread_id=g.thread_id
 JOIN app_agent_bindings b ON b.agent_id=g.agent_id AND b.install_id=ats.source_install_id
 JOIN agents a ON a.id=g.agent_id
 WHERE g.file_id=? AND g.agent_id=? AND g.thread_id=? AND ats.source_install_id=? AND ats.project_id=? AND b.enabled=1
 AND (COALESCE(a.project_id,'')='' OR a.project_id=ats.project_id)`, f.id, caller.agentID, caller.threadID, f.installID, f.ProjectID).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return fileProblem(403, "file_inaccessible", "attachment is not granted to this agent and thread")
	}
	agent, err := s.store.GetAgentByID(caller.agentID)
	if err != nil || agent == nil || (s.store.GetPlatformRole(agent.UserID) != PlatformAdmin && s.effectiveRoleOnProject(agent.UserID, f.ProjectID).Rank() < ProjectViewer.Rank()) {
		return fileProblem(403, "file_inaccessible", "agent owner no longer has project access")
	}
	if err := s.authorizeFileSource(f.installID, f.ProjectID, caller.agentID); err != nil {
		return err
	}

	// Reload revocation from durable storage on every resolution, including after IO.
	var revoked bool
	err = s.store.db.QueryRow(`SELECT revoked FROM app_file_references WHERE id=?`, f.id).Scan(&revoked)
	if err == sql.ErrNoRows {
		return fileProblem(410, "file_deleted", "reference deleted")
	}
	if err != nil {
		return err
	}
	if revoked {
		return fileProblem(410, "file_revoked", "reference revoked")
	}
	if f.ExpiresAt != nil && !f.ExpiresAt.After(time.Now()) {
		return fileProblem(410, "file_expired", "reference expired")
	}
	if f.Size > sdk.MaxFileReferenceBytes {
		return fileProblem(413, "file_too_large", "attachment exceeds 25 MiB")
	}
	return nil
}
func (s *Server) readReferencedFile(ctx context.Context, f *storedFileReference, caller fileCaller) (map[string]any, error) {
	if err := s.authorizeFileRead(f, caller); err != nil {
		return nil, err
	}
	if f.serverBlob {
		var raw []byte
		if err := s.store.db.QueryRowContext(ctx, `SELECT data FROM server_blobs WHERE id=?`, f.id).Scan(&raw); err != nil {
			return nil, err
		}
		digest := sha256.Sum256(raw)
		if int64(len(raw)) != f.Size || hex.EncodeToString(digest[:]) != f.SHA256 {
			return nil, fileProblem(409, "file_changed", "blob no longer matches its immutable metadata")
		}
		if err := s.authorizeFileRead(f, caller); err != nil {
			return nil, err
		}
		return map[string]any{"_binary": true, "base64": base64.StdEncoding.EncodeToString(raw), "mimeType": f.MIMEType, "size": int64(len(raw)), "filename": f.Filename}, nil
	}
	if s.installedApps == nil {
		return nil, fileProblem(503, "file_unavailable", "owning app unavailable")
	}
	owner := s.installedApps.Get(f.installID)
	if owner == nil || owner.SidecarURL == "" {
		return nil, fileProblem(503, "file_unavailable", "owning app unavailable")
	}
	token, err := s.appInstallToken(f.installID)
	if err != nil {
		return nil, fileProblem(503, "file_unavailable", "owning app unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	body, _ := json.Marshal(sdk.FileReadRequest{FileReference: f.FileReference, AgentID: caller.agentID, ThreadID: caller.threadID, Deadline: deadline})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(owner.SidecarURL, "/")+sdk.FileReferenceReadPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sdk.HeaderFileReadSignature, sdk.FileReadSignature(token, body))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fileProblem(503, "file_unavailable", "owning app could not supply the attachment")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var p sdk.FileReferenceError
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&p)
		switch p.Code {
		case "file_deleted", "file_expired":
			return nil, fileProblem(410, p.Code, "attachment no longer available")
		case "file_inaccessible":
			return nil, fileProblem(403, p.Code, "attachment inaccessible")
		}
		if resp.StatusCode == 404 {
			return nil, fileProblem(503, "file_source_unsupported", "owning app has no file reference reader")
		}
		return nil, fileProblem(503, "file_unavailable", "owning app could not supply the attachment")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, f.Size+1))
	if err != nil {
		return nil, fileProblem(503, "file_read_failed", "attachment transfer incomplete")
	}
	if int64(len(raw)) > f.Size {
		return nil, fileProblem(413, "file_too_large", "attachment exceeded registered size")
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, fileProblem(409, "file_changed", "attachment no longer matches its immutable version")
	}
	if err = s.authorizeFileRead(f, caller); err != nil {
		return nil, err
	}
	return map[string]any{"_binary": true, "base64": base64.StdEncoding.EncodeToString(raw), "mimeType": f.MIMEType, "size": f.Size, "filename": f.Filename}, nil
}

func referenceValue(value any) string {
	switch v := value.(type) {
	case string:
		if sdk.IsFileReference(v) {
			return v
		}
	case map[string]any:
		if ref, ok := v["_file_ref"].(string); ok && sdk.IsFileReference(ref) {
			return ref
		}
		if v["_file"] == true {
			if ref, ok := v["ref"].(string); ok && sdk.IsFileReference(ref) {
				return ref
			}
		}
	}
	return ""
}
func containsFileReference(value any) bool {
	if referenceValue(value) != "" {
		return true
	}
	switch v := value.(type) {
	case map[string]any:
		for _, item := range v {
			if containsFileReference(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if containsFileReference(item) {
				return true
			}
		}
	}
	return false
}

// resolveFileArguments returns a new argument tree, preserving the original
// compact references for usage/audit records. Reservations last through dispatch.
func (s *Server) resolveFileArguments(ctx context.Context, args map[string]any, schema map[string]any, caller fileCaller) (map[string]any, func(), error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var reserved, total int64
	count := 0
	runtime := s.automaticRuntime()
	release := func() { runtime.mu.Lock(); runtime.fileBytes -= reserved; reserved = 0; runtime.mu.Unlock() }
	var visit func(any, map[string]any, int) (any, error)
	visit = func(value any, shape map[string]any, depth int) (any, error) {
		if depth > 24 {
			return nil, fileProblem(400, "invalid_file_argument", "file argument nesting exceeds limit")
		}
		if ref := referenceValue(value); ref != "" {
			passthrough := shape[sdk.FileReferencePassthroughSchemaKey] == true
			if shape[sdk.FileReferenceSchemaKey] != true && !passthrough {
				return nil, fileProblem(400, "unsupported_file_argument", "this tool argument does not declare file support")
			}
			count++
			if count > 16 {
				return nil, fileProblem(413, "file_too_large", "too many file references in one call")
			}
			f, err := s.loadFileReference(ref)
			if err != nil {
				return nil, err
			}
			if err = s.authorizeFileRead(f, caller); err != nil {
				return nil, err
			}
			total += f.Size
			if total > sdk.MaxFileReferenceBytes {
				return nil, fileProblem(413, "file_too_large", "combined attachments exceed 25 MiB")
			}
			if passthrough {
				// Authorize and canonicalize metadata, but leave the bytes in the
				// platform-owned store for the destination app to retain as a
				// reference. This is used by presentation-oriented tools such as
				// Conversations message attachments.
				return f.FileReference.Handle(), nil
			}
			// Budget covers raw bytes, base64, and encoded proxy copies. No durable
			// bytes are stored here, and concurrent calls cannot exhaust host RAM.
			estimate := 6*f.Size + 1024
			runtime.mu.Lock()
			if runtime.fileBytes+estimate > maxFileMemoryBytes {
				runtime.mu.Unlock()
				return nil, fileProblem(429, "file_resolution_busy", "file transfer memory budget occupied; retry later")
			}
			runtime.fileBytes += estimate
			reserved += estimate
			runtime.mu.Unlock()
			return s.readReferencedFile(ctx, f, caller)
		}
		switch v := value.(type) {
		case map[string]any:
			props, _ := shape["properties"].(map[string]any)
			out := make(map[string]any, len(v))
			for key, item := range v {
				child, _ := props[key].(map[string]any)
				next, err := visit(item, child, depth+1)
				if err != nil {
					return nil, err
				}
				out[key] = next
			}
			return out, nil
		case []any:
			child, _ := shape["items"].(map[string]any)
			out := make([]any, len(v))
			for i, item := range v {
				next, err := visit(item, child, depth+1)
				if err != nil {
					return nil, err
				}
				out[i] = next
			}
			return out, nil
		default:
			return value, nil
		}
	}
	out, err := visit(args, schema, 0)
	code := "ok"
	if err != nil {
		code = "file_reference_error"
		var problem *sdk.FileReferenceError
		if errors.As(err, &problem) {
			code = problem.Code
		}
	}
	log.Printf("[FILE-RESOLVE] agent=%d files=%d bytes=%d duration_ms=%d status=%s", caller.agentID, count, total, time.Since(started).Milliseconds(), code)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return out.(map[string]any), release, nil
}

// Called after app routing/authorization, before the reverse proxy forwards
// bytes. Ordinary calls retain their existing transport and incur no lookup.
func (s *Server) resolveAppFileRequest(r *http.Request, entry *InstalledApp) (func(), error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return func() {}, err
	}
	restoreRequestBody(r, body)
	var rpc map[string]any
	if decodeMCPProxyRequest(body, &rpc) != nil || rpc["method"] != "tools/call" {
		return func() {}, nil
	}
	params, _ := rpc["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	if !containsFileReference(args) {
		return func() {}, nil
	}
	caller, err := s.blobCallerFromRequest(r)
	if err != nil {
		return func() {}, err
	}
	_, project, _, err := s.blobScope(caller)
	if err != nil {
		return func() {}, err
	}
	if (entry.ProjectID != "" && entry.ProjectID != project) || (r.URL.Query().Get("project_id") != "" && r.URL.Query().Get("project_id") != project) {
		return func() {}, fileProblem(403, "file_inaccessible", "destination app project does not match thread")
	}
	var attached int
	err = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE agent_id=? AND install_id=? AND enabled=1`, caller.agentID, entry.InstallID).Scan(&attached)
	if err != nil {
		return func() {}, err
	}
	if attached != 1 {
		return func() {}, fileProblem(403, "file_inaccessible", "destination app is not attached to this agent")
	}
	name, _ := params["name"].(string)
	allowed := false
	for _, tool := range agentVisibleMCPTools(entry.Manifest.Provides.MCPTools) {
		if tool.Name == name {
			allowed = true
			break
		}
	}
	if !allowed {
		return func() {}, fileProblem(403, "file_inaccessible", "destination tool is not agent-visible")
	}
	token, err := s.appInstallToken(entry.InstallID)
	if err != nil {
		return func() {}, err
	}
	// Fetch the authoritative current schema only for calls carrying references.
	// Bound both the response size and lifetime, and never follow redirects.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(entry.SidecarURL, "/")+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		return func() {}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return func() {}, fileProblem(503, "file_unavailable", "destination tool schema unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	var listing struct {
		Result struct {
			Tools []installMCPToolInfo `json:"tools"`
		} `json:"result"`
	}
	if err != nil || resp.StatusCode != 200 || len(raw) > 2<<20 || json.Unmarshal(raw, &listing) != nil {
		return func() {}, fileProblem(503, "file_unavailable", "destination tool schema unavailable")
	}
	var schema map[string]any
	for _, tool := range listing.Result.Tools {
		if tool.Name == name {
			schema = tool.InputSchema
			break
		}
	}
	if schema == nil {
		return func() {}, fileProblem(400, "unsupported_file_argument", "destination tool has no file schema")
	}
	next, release, err := s.resolveFileArguments(r.Context(), args, schema, caller)
	if err != nil {
		return release, err
	}
	params["arguments"] = next
	encoded, err := json.Marshal(rpc)
	if err != nil {
		release()
		return func() {}, err
	}
	restoreRequestBody(r, encoded)
	return release, nil
}

// File failures are the result of the original tool call, never a second
// model-facing tool invocation. Do not put resolved payloads into activity.
func writeFileToolFailure(w http.ResponseWriter, r *http.Request, err error) {
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 50<<20)).Decode(&request)
	}
	var p *sdk.FileReferenceError
	if !errors.As(err, &p) {
		p = fileProblem(500, "file_reference_error", "file resolution failed")
	}
	detail, _ := json.Marshal(p)
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": string(detail)}}}})
}

func (s *Server) callbackFileCaller(w http.ResponseWriter, r *http.Request, installID int64, scope *sdk.FileReferenceScope) (fileCaller, bool) {
	principal, ok := r.Context().Value(appCallbackPrincipalKey{}).(appCallbackPrincipal)
	if !ok || principal.installID != installID || scope == nil || !installHasPermission(s, installID, sdk.PermFileReferences) {
		writeFileProblem(w, fileProblem(403, "file_context_required", "authenticated app file_scope and platform.files.references required"))
		return fileCaller{}, false
	}
	_, project, ok := s.runtimeCallerProject(w, r, installID, scope.ProjectID, ProjectEditor)
	if !ok {
		return fileCaller{}, false
	}
	var n int
	err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agent_thread_scopes ats JOIN app_agent_bindings b ON b.agent_id=ats.agent_id AND b.install_id=ats.source_install_id WHERE ats.agent_id=? AND ats.thread_id=? AND ats.project_id=? AND ats.source_install_id=? AND b.enabled=1`, scope.AgentID, scope.ThreadID, project, installID).Scan(&n)
	if err != nil || n != 1 {
		writeFileProblem(w, fileProblem(403, "file_inaccessible", "thread does not belong to this installation and project"))
		return fileCaller{}, false
	}
	scope.ProjectID = project
	return fileCaller{agentID: scope.AgentID, threadID: scope.ThreadID, installID: installID}, true
}

// Catalog-declared binary body and multipart file inputs already define which
// integration arguments accept bytes. No catalog migration is required.
func integrationFileSchema(tool *AppToolDef) map[string]any {
	props := map[string]any{}
	if tool.BodyBinaryParam != "" {
		props[tool.BodyBinaryParam] = map[string]any{sdk.FileReferenceSchemaKey: true}
	}
	if tool.MultipartForm != nil {
		for input := range tool.MultipartForm.FileFields {
			props[input] = map[string]any{sdk.FileReferenceSchemaKey: true, "items": map[string]any{sdk.FileReferenceSchemaKey: true}}
		}
	}
	return map[string]any{"type": "object", "properties": props}
}
