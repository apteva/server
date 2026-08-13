package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type publicClientScope struct {
	Type    string   `json:"type"`
	App     string   `json:"app"`
	Actions []string `json:"actions"`
}

type publicClientRateBucket struct {
	window int64
	count  int
}

var (
	publicClientRateMu      sync.Mutex
	publicClientRateBuckets = map[int64]publicClientRateBucket{}
)

func (s *Server) tryHandlePublicClientAppMCP(w http.ResponseWriter, r *http.Request) bool {
	appName, tail, ok := parseAppRuntimePath(r.URL.Path)
	if !ok || tail != "/mcp" || r.Method != http.MethodPost {
		return false
	}
	token := requestAPIKeyToken(r)
	if token == "" || !strings.HasPrefix(token, "pk_") {
		return false
	}

	key, err := s.store.GetPublicClientAPIKey(HashAPIKey(token))
	if err != nil {
		http.Error(w, "invalid public client key", http.StatusUnauthorized)
		return true
	}
	if key.ProjectID == "" {
		http.Error(w, "public client key is not project scoped", http.StatusForbidden)
		return true
	}
	if !publicClientOriginAllowed(key.AllowedOrigins, r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return true
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return true
	}
	_ = r.Body.Close()
	action, scopedBody, err := scopePublicClientMCPRequest(body, key.ProjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	restoreRequestBody(r, scopedBody)
	if !publicClientScopeAllows(key.Scopes, appName, action) {
		http.Error(w, "public client key is not allowed to call this app action", http.StatusForbidden)
		return true
	}
	if !publicClientRateAllowed(key.ID, key.RateLimitPerMinute) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return true
	}

	entry := s.installedApps.GetByNameAndProject(appName, key.ProjectID)
	if entry == nil {
		http.Error(w, "app not installed for public client key project: "+appName, http.StatusNotFound)
		return true
	}

	q := r.URL.Query()
	q.Set("install_id", strconv.FormatInt(entry.InstallID, 10))
	q.Set("project_id", key.ProjectID)
	q.Del("api_key")
	r.URL.RawQuery = q.Encode()
	r.Header.Del("Authorization")
	r.Header.Del("X-API-Key")
	r.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(entry.InstallID, 10))

	s.store.MarkAPIKeyUsed(key.ID, requestClientIP(r))
	s.handleAppProxy(w, r)
	return true
}

// scopePublicClientMCPRequest validates the only JSON-RPC shape public client
// keys may invoke and stamps the key's trusted tenant scope into the forwarded
// arguments. The browser can neither omit the project context nor substitute
// another project when the selected app is a shared global installation.
func scopePublicClientMCPRequest(body []byte, projectID string) (string, []byte, error) {
	var rpc map[string]json.RawMessage
	if err := json.Unmarshal(body, &rpc); err != nil {
		return "", nil, err
	}
	var method string
	if err := json.Unmarshal(rpc["method"], &method); err != nil || method != "tools/call" {
		return "", nil, errPublicClientMCPMethod
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(rpc["params"], &params); err != nil || params == nil {
		return "", nil, errPublicClientMCPParams
	}
	var action string
	if err := json.Unmarshal(params["name"], &action); err != nil || strings.TrimSpace(action) == "" {
		return "", nil, errPublicClientMCPToolName
	}
	var arguments map[string]any
	if err := json.Unmarshal(params["arguments"], &arguments); err != nil || arguments == nil {
		return "", nil, errPublicClientMCPArguments
	}
	arguments["_project_id"] = projectID
	argumentsJSON, err := json.Marshal(arguments)
	if err != nil {
		return "", nil, err
	}
	params["arguments"] = argumentsJSON
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return "", nil, err
	}
	rpc["params"] = paramsJSON
	rewritten, err := json.Marshal(rpc)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(action), rewritten, nil
}

func parseAppRuntimePath(path string) (string, string, bool) {
	rest := strings.TrimPrefix(path, "/apps/")
	if rest == path || rest == "" {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if parts[0] == "" {
		return "", "", false
	}
	tail := ""
	if len(parts) == 2 {
		tail = "/" + parts[1]
	}
	return parts[0], tail, true
}

func requestAPIKeyToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	return strings.TrimSpace(r.URL.Query().Get("api_key"))
}

func publicClientOriginAllowed(rawAllowed, origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	var allowed []string
	if err := json.Unmarshal([]byte(rawAllowed), &allowed); err != nil {
		return false
	}
	for _, item := range allowed {
		item = strings.TrimSpace(item)
		if item == origin || item == "*" {
			return true
		}
	}
	return false
}

func publicClientMCPToolName(body []byte) (string, error) {
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return "", err
	}
	if rpc.Method != "tools/call" {
		return "", errPublicClientMCPMethod
	}
	if strings.TrimSpace(rpc.Params.Name) == "" {
		return "", errPublicClientMCPToolName
	}
	return rpc.Params.Name, nil
}

var (
	errPublicClientMCPMethod    = publicClientError("public client keys only support tools/call")
	errPublicClientMCPParams    = publicClientError("MCP params must be an object")
	errPublicClientMCPToolName  = publicClientError("missing MCP tool name")
	errPublicClientMCPArguments = publicClientError("MCP params.arguments must be an object")
)

type publicClientError string

func (e publicClientError) Error() string { return string(e) }

func publicClientScopeAllows(rawScopes, appName, action string) bool {
	var scopes []publicClientScope
	if err := json.Unmarshal([]byte(rawScopes), &scopes); err != nil {
		return false
	}
	for _, scope := range scopes {
		if scope.Type != "app_action" || scope.App != appName {
			continue
		}
		for _, allowed := range scope.Actions {
			if allowed == action || allowed == "*" {
				return true
			}
		}
	}
	return false
}

func publicClientRateAllowed(keyID int64, limit int) bool {
	if limit <= 0 {
		limit = 60
	}
	nowWindow := time.Now().Unix() / 60
	publicClientRateMu.Lock()
	defer publicClientRateMu.Unlock()
	bucket := publicClientRateBuckets[keyID]
	if bucket.window != nowWindow {
		bucket = publicClientRateBucket{window: nowWindow}
	}
	if bucket.count >= limit {
		publicClientRateBuckets[keyID] = bucket
		return false
	}
	bucket.count++
	publicClientRateBuckets[keyID] = bucket
	return true
}

func restoreRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func requestClientIP(r *http.Request) string {
	return clientIP(r)
}
