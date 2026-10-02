package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func newBlobTestCaller(t *testing.T, s *Server) fileCaller {
	t.Helper()
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "blob-test", "handle files", "autonomous", "{}", "blob-project")
	if err != nil {
		t.Fatal(err)
	}
	return fileCaller{agentID: agent.ID, threadID: "main"}
}

func TestServerBlobRoundTripAndAccess(t *testing.T) {
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	raw := []byte("durable uploaded content")
	handle, err := s.storeServerBlob(context.Background(), caller, "report.txt", "text/plain", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !handle.File || !strings.HasPrefix(handle.Ref, "blobref://") || handle.Size != int64(len(raw)) {
		t.Fatalf("unexpected handle: %#v", handle)
	}
	again, err := s.storeServerBlob(context.Background(), caller, "report.txt", "text/plain", raw)
	if err != nil || again.Ref != handle.Ref {
		t.Fatalf("repeated upload changed handle: %#v %v", again, err)
	}
	// A fresh Server reads metadata and bytes exclusively from durable storage.
	restarted := &Server{store: s.store}
	file, err := restarted.loadFileReference(handle.Ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{handle.Ref, map[string]any{"_file_ref": handle.Ref}, map[string]any{"_file": true, "ref": handle.Ref}} {
		args := map[string]any{"file": value, "other": "unchanged"}
		resolved, release, err := restarted.resolveFileArguments(context.Background(), args, map[string]any{"properties": map[string]any{"file": sdk.FileArgumentSchema("File")}}, caller)
		if err != nil {
			t.Fatal(err)
		}
		release()
		envelope, ok := resolved["file"].(map[string]any)
		if !ok || envelope["_binary"] != true || envelope["base64"] != base64.StdEncoding.EncodeToString(raw) || envelope["filename"] != "report.txt" || resolved["other"] != "unchanged" {
			t.Fatalf("resolved input: %#v", resolved)
		}
		if referenceValue(args["file"]) != handle.Ref {
			t.Fatal("resolver modified compact audit arguments")
		}
	}
	if err := restarted.authorizeFileRead(file, fileCaller{agentID: caller.agentID, threadID: "another-thread"}); err == nil {
		t.Fatal("ungranted thread read the blob")
	}
	other := newBlobTestCaller(t, s)
	if err := restarted.authorizeFileRead(file, other); err == nil {
		t.Fatal("another agent read the blob")
	}
	if _, err = s.store.db.Exec(`UPDATE agents SET project_id='another-project' WHERE id=?`, caller.agentID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.authorizeFileRead(file, caller); err == nil {
		t.Fatal("project reassignment retained access")
	}
	if _, err = s.store.db.Exec(`UPDATE agents SET project_id='blob-project' WHERE id=?`, caller.agentID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.db.Exec(`UPDATE server_blobs SET revoked=1 WHERE id=?`, file.id); err != nil {
		t.Fatal(err)
	}
	if err := restarted.authorizeFileRead(file, caller); err == nil {
		t.Fatal("revoked blob remained accessible")
	}
}

func TestServerBlobEmptyAndUnsupportedArgument(t *testing.T) {
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	handle, err := s.storeServerBlob(context.Background(), caller, "empty.txt", "text/plain", nil)
	if err != nil || handle.Size != 0 {
		t.Fatalf("empty blob: %#v %v", handle, err)
	}
	_, release, err := s.resolveFileArguments(context.Background(), map[string]any{"text": handle.Ref}, nil, caller)
	release()
	if err == nil || !strings.Contains(err.Error(), "unsupported_file_argument") {
		t.Fatalf("ordinary text input accepted a blob: %v", err)
	}
}

func signedBlobRequest(s *Server, caller fileCaller) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/apps/files/mcp", nil)
	query := url.Values{"file_agent": []string{itoa64(caller.agentID)}}
	query.Set("file_auth", fileCallerSignature(s.instanceSecret, caller.agentID, r.URL.Path, ""))
	r.URL.RawQuery = query.Encode()
	r.Header.Set(sdk.HeaderFileReferenceThread, caller.threadID)
	return r
}

func TestServerBlobToolOutputToNextInput(t *testing.T) {
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	raw := []byte("tool output bytes")
	binary := map[string]any{"_binary": true, "base64": base64.StdEncoding.EncodeToString(raw), "mimeType": "text/plain", "filename": "result.txt", "size": len(raw)}
	text, _ := json.Marshal(binary)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.Number("9007199254740993"), "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}})
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
	if err := s.storeAppBlobResponse(signedBlobRequest(s, caller), resp); err != nil {
		t.Fatal(err)
	}
	encoded, _ := io.ReadAll(resp.Body)
	if bytes.Contains(encoded, []byte("base64")) || !bytes.Contains(encoded, []byte("9007199254740993")) {
		t.Fatalf("bytes leaked or RPC ID changed: %s", encoded)
	}
	var rpc struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(encoded, &rpc); err != nil {
		t.Fatal(err)
	}
	var handle sdk.FileHandle
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &handle); err != nil || !handle.File {
		t.Fatalf("output did not become a handle: %#v %v", handle, err)
	}
	resolved, release, err := s.resolveFileArguments(context.Background(), map[string]any{"file": handle.Ref}, map[string]any{"properties": map[string]any{"file": sdk.FileArgumentSchema("File")}}, caller)
	defer release()
	if err != nil || resolved["file"].(map[string]any)["base64"] != binary["base64"] {
		t.Fatalf("output-to-input failed: %#v %v", resolved, err)
	}
	legacy := sdk.LegacyFileReferencePrefix + strings.TrimPrefix(handle.Ref, sdk.FileReferencePrefix)
	loaded, err := s.loadFileReference(legacy)
	if err != nil || loaded.Ref != handle.Ref {
		t.Fatalf("legacy alias did not resolve the same blob: %#v %v", loaded, err)
	}
}

func TestServerBlobUnsignedOutputKeepsLegacyBehavior(t *testing.T) {
	s := newTestServer(t)
	raw := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"_binary\":true,\"base64\":\"AQI=\"}"}]}}`
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(raw))}
	request := httptest.NewRequest(http.MethodPost, "/api/apps/files/mcp", nil)
	request.Header.Set(sdk.HeaderFileReferenceThread, "forged-thread")
	if err := s.storeAppBlobResponse(request, resp); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != raw {
		t.Fatal("unsigned legacy transport changed")
	}
}

func TestServerBlobProxyOutputToFileTool(t *testing.T) {
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	installID := seedAppWithTools(t, s, "blob-flow", "blob-project", []string{"produce", "consume"})
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, installID, caller.agentID); err != nil {
		t.Fatal(err)
	}
	raw := []byte("output handed to the next tool")
	binary := map[string]any{"_binary": true, "base64": base64.StdEncoding.EncodeToString(raw), "mimeType": "text/plain", "filename": "result.txt", "size": len(raw)}
	schema := map[string]any{"type": "object", "properties": map[string]any{"file": sdk.FileArgumentSchema("File")}}
	consumed := make(chan map[string]any, 1)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch {
		case rpc.Method == "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "consume", "inputSchema": schema}}}
		case rpc.Params.Name == "produce":
			text, _ := json.Marshal(binary)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "structuredContent": map[string]any{"file": binary}}
		case rpc.Params.Name == "consume":
			consumed <- rpc.Params.Arguments
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "accepted"}}}
		default:
			t.Errorf("unexpected sidecar call: %s", rpc.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	defer sidecar.Close()
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{InstallID: installID, AppName: "blob-flow", ProjectID: "blob-project", SidecarURL: sidecar.URL,
		Manifest: sdk.Manifest{Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{{Name: "produce"}, {Name: "consume"}}}}})
	call := func(name string, args map[string]any) *httptest.ResponseRecorder {
		path := "/apps/blob-flow/mcp"
		query := url.Values{"file_agent": []string{itoa64(caller.agentID)}, "install_id": []string{itoa64(installID)}}
		query.Set("file_auth", fileCallerSignature(s.instanceSecret, caller.agentID, "/api"+path, itoa64(installID)))
		request := authedRequest(t, http.MethodPost, path+"?"+query.Encode(), "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		request.Header.Set(sdk.HeaderFileReferenceThread, caller.threadID)
		response := httptest.NewRecorder()
		s.handleAppProxy(response, request)
		return response
	}
	output := call("produce", map[string]any{})
	if output.Code != http.StatusOK || strings.Contains(output.Body.String(), "base64") {
		t.Fatalf("gateway output exposed bytes: %d %s", output.Code, output.Body.String())
	}
	var rpc struct {
		Result struct {
			Content    []struct{ Text string } `json:"content"`
			Structured struct {
				File sdk.FileHandle `json:"file"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output.Body.Bytes(), &rpc); err != nil || len(rpc.Result.Content) != 1 {
		t.Fatalf("invalid gateway output: %s (%v)", output.Body.String(), err)
	}
	var handle sdk.FileHandle
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &handle); err != nil || !handle.File || handle.Ref != rpc.Result.Structured.File.Ref {
		t.Fatalf("text and structured output handles differ: %#v %#v (%v)", handle, rpc.Result.Structured.File, err)
	}
	// This is the same metadata-only argument Core sends back unchanged.
	input := call("consume", map[string]any{"file": handle})
	if input.Code != http.StatusOK || !strings.Contains(input.Body.String(), "accepted") {
		t.Fatalf("file tool call failed: %d %s", input.Code, input.Body.String())
	}
	select {
	case args := <-consumed:
		file, ok := args["file"].(map[string]any)
		if !ok || file["_binary"] != true || file["base64"] != binary["base64"] || file["filename"] != "result.txt" {
			t.Fatalf("recipient did not get original bytes: %#v", args)
		}
	default:
		t.Fatal("file tool was not called")
	}
}

func TestServerBlobAppFileInputFromMainThread(t *testing.T) {
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	installID := seedAppWithTools(t, s, "blob-destination", "blob-project", []string{"files_put"})
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, installID, caller.agentID); err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"file": sdk.FileArgumentSchema("File")}}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{map[string]any{"name": "files_put", "inputSchema": schema}}}})
	}))
	defer sidecar.Close()
	entry := &InstalledApp{InstallID: installID, AppName: "blob-destination", ProjectID: "blob-project", SidecarURL: sidecar.URL,
		Manifest: sdk.Manifest{Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{{Name: "files_put"}}}}}
	handle, err := s.storeServerBlob(context.Background(), caller, "report.txt", "text/plain", []byte("uploaded"))
	if err != nil {
		t.Fatal(err)
	}
	// Main has no app-owned thread scope. Its server-validated agent project
	// still permits the shared blob to reach an attached Files tool.
	request := signedBlobRequest(s, caller)
	request.URL.Path = "/apps/files/mcp" // /api stripped by the router.
	text, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "files_put", "arguments": map[string]any{"file": handle}}})
	restoreRequestBody(request, text)
	release, err := s.resolveAppFileRequest(request, entry)
	defer release()
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := io.ReadAll(request.Body)
	if !bytes.Contains(resolved, []byte(`"_binary":true`)) || bytes.Contains(resolved, []byte("blobref://")) {
		t.Fatalf("Files tool did not receive resolved bytes: %s", resolved)
	}
}

func TestServerBlobCallbackUploadRequiresAppOwnedScope(t *testing.T) {
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "blob-source"}
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermFileReferences}
	installID := seedInstallWithBindings(t, s, "blob-source", manifest, nil)
	if _, err := s.store.db.Exec(`UPDATE agents SET project_id='proj-1' WHERE id=?`, caller.agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, installID, caller.agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.BindAgentThreadScope(caller.agentID, "upload-thread", "proj-1", installID); err != nil {
		t.Fatal(err)
	}
	upload := sdk.StoreBlobRequest{Scope: sdk.FileReferenceScope{ProjectID: "proj-1", AgentID: caller.agentID, ThreadID: "upload-thread"}, Filename: "input.txt", MIMEType: "text/plain", Data: []byte("upload bytes")}
	post := func(scope sdk.FileReferenceScope) *httptest.ResponseRecorder {
		upload.Scope = scope
		body, _ := json.Marshal(upload)
		request := httptest.NewRequest(http.MethodPost, "/apps/callback/blobs", bytes.NewReader(body))
		request.Header.Set("X-User-ID", "1")
		request = request.WithContext(context.WithValue(request.Context(), appCallbackPrincipalKey{}, appCallbackPrincipal{installID: installID, userID: 1}))
		rec := httptest.NewRecorder()
		s.handleCallbackBlobs(rec, request)
		return rec
	}
	response := post(upload.Scope)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "blobref://") || strings.Contains(response.Body.String(), "upload bytes") {
		t.Fatalf("upload response: %d %s", response.Code, response.Body.String())
	}
	forged := upload.Scope
	forged.ThreadID = "unowned-thread"
	if response := post(forged); response.Code != http.StatusForbidden {
		t.Fatalf("unowned thread accepted: %d %s", response.Code, response.Body.String())
	}
}
