package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// blobScope comes from signed runtime transport or validated app callback
// scope. A model-supplied project/thread/owner is never used for authorization.
// An app-owned thread must retain access to its owning app, but automatic
// tool-output storage does not require that app to use the explicit file API.
// App callback uploads enforce platform.files.references at their entry point.
func (s *Server) blobScope(caller fileCaller) (userID int64, project string, installID int64, err error) {
	if caller.agentID <= 0 || !validTrustedMCPIdentity(caller.threadID) {
		return 0, "", 0, fileProblem(403, "file_context_required", "trusted agent and thread required")
	}
	agent, err := s.store.GetAgentByID(caller.agentID)
	if err != nil || agent == nil {
		return 0, "", 0, fileProblem(403, "file_inaccessible", "agent unavailable")
	}
	project = agent.ProjectID
	var scopedProject string
	err = s.store.db.QueryRow(`SELECT project_id,source_install_id FROM agent_thread_scopes WHERE agent_id=? AND thread_id=?`, caller.agentID, caller.threadID).Scan(&scopedProject, &installID)
	if err == nil {
		if project != "" && project != scopedProject {
			return 0, "", 0, fileProblem(403, "file_inaccessible", "thread project differs from agent")
		}
		project = scopedProject
	} else if err != sql.ErrNoRows {
		return 0, "", 0, err
	}
	if caller.installID != 0 && caller.installID != installID {
		return 0, "", 0, fileProblem(403, "file_inaccessible", "thread is not owned by this installation")
	}
	if project != "" && s.store.GetPlatformRole(agent.UserID) != PlatformAdmin && s.effectiveRoleOnProject(agent.UserID, project).Rank() < ProjectViewer.Rank() {
		return 0, "", 0, fileProblem(403, "file_inaccessible", "agent owner no longer has project access")
	}
	if installID != 0 {
		if _, err := s.authorizeFileAppAccess(installID, project, caller.agentID); err != nil {
			return 0, "", 0, err
		}
	}
	return agent.UserID, project, installID, nil
}

func (s *Server) storeServerBlob(ctx context.Context, caller fileCaller, filename, mimeType string, data []byte) (sdk.FileHandle, error) {
	var empty sdk.FileHandle
	if int64(len(data)) > sdk.MaxFileReferenceBytes {
		return empty, fileProblem(413, "file_too_large", "blob exceeds 25 MiB")
	}
	if data == nil {
		data = []byte{}
	}
	media, _, mimeErr := mime.ParseMediaType(mimeType)
	if mimeErr != nil || media != mimeType || len(mimeType) > 255 || len(filename) > 255 || strings.ContainsAny(filename, "/\\\x00\r\n") {
		return empty, fileProblem(400, "invalid_file_metadata", "valid MIME type and filename required")
	}
	userID, project, installID, err := s.blobScope(caller)
	if err != nil {
		return empty, err
	}
	digest := sha256.Sum256(data)
	checksum := hex.EncodeToString(digest[:])
	var random [24]byte
	if _, err = rand.Read(random[:]); err != nil {
		return empty, err
	}
	id := hex.EncodeToString(random[:])
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO server_blobs(id,owner_user_id,source_install_id,project_id,filename,mime_type,size,sha256,data) VALUES(?,?,?,?,?,?,?,?,?)
 ON CONFLICT(owner_user_id,source_install_id,project_id,filename,mime_type,sha256) DO NOTHING`, id, userID, installID, project, filename, mimeType, len(data), checksum, data)
	if err != nil {
		return empty, err
	}
	var revoked bool
	if err = tx.QueryRowContext(ctx, `SELECT id,revoked FROM server_blobs WHERE owner_user_id=? AND source_install_id=? AND project_id=? AND filename=? AND mime_type=? AND sha256=?`, userID, installID, project, filename, mimeType, checksum).Scan(&id, &revoked); err != nil {
		return empty, err
	}
	if revoked {
		return empty, fileProblem(410, "file_revoked", "blob was revoked")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO server_blob_grants(file_id,agent_id,thread_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`, id, caller.agentID, caller.threadID); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return sdk.FileHandle{File: true, Ref: sdk.FileReferencePrefix + id, Filename: filename, MIMEType: mimeType, Size: int64(len(data))}, nil
}

func (s *Server) authorizeServerBlob(f *storedFileReference, caller fileCaller) error {
	userID, project, installID, err := s.blobScope(caller)
	if err != nil {
		return err
	}
	if f.ownerUserID != userID || f.ProjectID != project || f.installID != installID {
		return fileProblem(403, "file_inaccessible", "blob belongs to another owner, installation or project")
	}
	var n int
	if err = s.store.db.QueryRow(`SELECT COUNT(*) FROM server_blob_grants WHERE file_id=? AND agent_id=? AND thread_id=?`, f.id, caller.agentID, caller.threadID).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fileProblem(403, "file_inaccessible", "blob is not granted to this thread")
	}
	var revoked bool
	if err = s.store.db.QueryRow(`SELECT revoked FROM server_blobs WHERE id=?`, f.id).Scan(&revoked); err != nil {
		return err
	}
	if revoked {
		return fileProblem(410, "file_revoked", "blob was revoked")
	}
	return nil
}

func (s *Server) handleCallbackBlobs(w http.ResponseWriter, r *http.Request) {
	principal, ok := r.Context().Value(appCallbackPrincipalKey{}).(appCallbackPrincipal)
	if !ok || r.Method != http.MethodPost {
		writeFileProblem(w, fileProblem(403, "file_context_required", "authenticated app POST required"))
		return
	}
	if !installHasPermission(s, principal.installID, sdk.PermFileReferences) {
		writeFileProblem(w, fileProblem(403, "permission_denied", "missing platform.files.references permission"))
		return
	}
	var body sdk.StoreBlobRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 36<<20))
	if err := decoder.Decode(&body); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeFileProblem(w, fileProblem(400, "invalid_request", "valid blob upload required"))
		return
	}
	caller, ok := s.callbackFileCaller(w, r, principal.installID, &body.Scope)
	if !ok {
		return
	}
	handle, err := s.storeServerBlob(r.Context(), caller, body.Filename, body.MIMEType, body.Data)
	if err != nil {
		writeFileProblem(w, err)
		return
	}
	writeJSON(w, handle)
}

// Replace tool-produced _binary envelopes before they reach Core or usage
// history. Existing callers without signed runtime identity use the old path.
func (s *Server) storeToolBlobValue(ctx context.Context, value any, caller fileCaller) (any, error) {
	if !containsBinaryOutput(value) {
		return value, nil
	}
	var total int64
	count := 0
	stored := make(map[string]sdk.FileHandle)
	var visit func(any, int) (any, error)
	visit = func(value any, depth int) (any, error) {
		if depth > 24 {
			return nil, fileProblem(400, "invalid_file_output", "tool result nesting exceeds limit")
		}
		switch v := value.(type) {
		case map[string]any:
			if v["_binary"] == true {
				encoded, ok := v["base64"].(string)
				if !ok || int64(base64.StdEncoding.DecodedLen(len(encoded))) > sdk.MaxFileReferenceBytes+2 {
					return nil, fileProblem(413, "file_too_large", "invalid or oversized binary output")
				}
				data, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return nil, fileProblem(400, "invalid_file_output", "invalid base64 in binary output")
				}
				filename, _ := v["filename"].(string)
				media, _ := v["mimeType"].(string)
				if media == "" {
					media, _ = v["mime_type"].(string)
				}
				if media == "" {
					media = "application/octet-stream"
				}
				key := fmt.Sprintf("%s\x00%s\x00%x", filename, media, sha256.Sum256(data))
				if handle, ok := stored[key]; ok {
					return handle, nil
				}
				count++
				total += int64(len(data))
				if count > 16 || total > sdk.MaxFileReferenceBytes {
					return nil, fileProblem(413, "file_too_large", "combined tool files exceed limit")
				}
				handle, err := s.storeServerBlob(ctx, caller, filename, media, data)
				if err != nil {
					return nil, err
				}
				stored[key] = handle
				return handle, nil
			}
			out := make(map[string]any, len(v))
			for key, child := range v {
				next, err := visit(child, depth+1)
				if err != nil {
					return nil, err
				}
				out[key] = next
			}
			return out, nil
		case []any:
			out := make([]any, len(v))
			for i, child := range v {
				next, err := visit(child, depth+1)
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
	return visit(value, 0)
}

func containsBinaryOutput(value any) bool {
	pending := []any{value}
	for len(pending) > 0 {
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch v := item.(type) {
		case map[string]any:
			if v["_binary"] == true {
				return true
			}
			for _, child := range v {
				pending = append(pending, child)
			}
		case []any:
			pending = append(pending, v...)
		}
	}
	return false
}

func (s *Server) blobCallerFromRequest(r *http.Request) (fileCaller, error) {
	thread := r.Header.Get(sdk.HeaderFileReferenceThread)
	if thread == "" {
		thread = r.Header.Get("X-Apteva-Caller-Thread")
	}
	return s.fileReferenceCaller(r, thread)
}

func (s *Server) storeAppBlobResponse(r *http.Request, resp *http.Response) error {
	caller, err := s.blobCallerFromRequest(r)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return nil // Legacy or streaming transport retains its existing behavior.
	}
	raw, release, err := s.readBlobToolResponse(resp.Body)
	defer release()
	resp.Body.Close()
	if err != nil {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if r.GetBody != nil {
			if body, openErr := r.GetBody(); openErr == nil {
				_ = json.NewDecoder(io.LimitReader(body, 50<<20)).Decode(&request)
				body.Close()
			}
		}
		detail, _ := json.Marshal(fileProblemFromError(err))
		failure, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": string(detail)}}}})
		resp.Body = io.NopCloser(bytes.NewReader(failure))
		resp.ContentLength = int64(len(failure))
		resp.Header.Set("Content-Length", strconv.Itoa(len(failure)))
		return nil
	}
	restore := func(data []byte) {
		resp.Body = io.NopCloser(bytes.NewReader(data))
		resp.ContentLength = int64(len(data))
		resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
	}
	restore(raw)
	var rpc map[string]any
	if decodeMCPProxyRequest(raw, &rpc) != nil {
		return nil
	}
	result, _ := rpc["result"].(map[string]any)
	if result["isError"] == true {
		return nil
	}
	content, _ := result["content"].([]any)
	// MCP may include the same file in text and structuredContent. Rewrite
	// both before Core sees the response, with one combined batch limit.
	var values []any
	var textBlocks []map[string]any
	for _, item := range content {
		block, _ := item.(map[string]any)
		text, _ := block["text"].(string)
		if block["type"] != "text" || !strings.Contains(text, `"_binary"`) {
			continue
		}
		var value any
		if decodeMCPProxyRequest([]byte(text), &value) != nil {
			continue
		}
		if !containsBinaryOutput(value) {
			continue
		}
		values = append(values, value)
		textBlocks = append(textBlocks, block)
	}
	structured := result["structuredContent"]
	hasStructuredBinary := containsBinaryOutput(structured)
	if hasStructuredBinary {
		values = append(values, structured)
	}
	if len(values) > 0 {
		next, err := s.storeToolBlobValue(r.Context(), values, caller)
		if err != nil {
			detail, _ := json.Marshal(fileProblemFromError(err))
			rpc["result"] = map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": string(detail)}}}
		} else {
			rewritten := next.([]any)
			for i, block := range textBlocks {
				encoded, _ := json.Marshal(rewritten[i])
				block["text"] = string(encoded)
			}
			if hasStructuredBinary {
				result["structuredContent"] = rewritten[len(textBlocks)]
			}
		}
		encoded, err := json.Marshal(rpc)
		if err != nil {
			return err
		}
		restore(encoded)
	}
	return nil
}

// MCP can repeat a 25 MiB file in text and structuredContent. Allow both wire
// representations, while bounding concurrent parsing memory separately from
// the unique-file limit applied during conversion.
func (s *Server) readBlobToolResponse(body io.Reader) ([]byte, func(), error) {
	runtime := s.automaticRuntime()
	var reserved int64
	release := func() { runtime.mu.Lock(); runtime.fileBytes -= reserved; reserved = 0; runtime.mu.Unlock() }
	var out bytes.Buffer
	chunk := make([]byte, 64<<10)
	for {
		n, err := body.Read(chunk)
		if n > 0 {
			if out.Len()+n > 72<<20 {
				return nil, release, fileProblem(413, "file_too_large", "tool response exceeds 72 MiB")
			}
			estimate := 3 * int64(n)
			runtime.mu.Lock()
			if runtime.fileBytes+estimate > maxFileMemoryBytes {
				runtime.mu.Unlock()
				return nil, release, fileProblem(429, "file_resolution_busy", "file response memory budget occupied; retry later")
			}
			runtime.fileBytes += estimate
			reserved += estimate
			runtime.mu.Unlock()
			_, _ = out.Write(chunk[:n])
		}
		if err == io.EOF {
			return out.Bytes(), release, nil
		}
		if err != nil {
			return nil, release, fileProblem(502, "file_read_failed", "tool output transfer incomplete")
		}
	}
}
