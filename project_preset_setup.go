package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	semver "github.com/Masterminds/semver/v3"
)

// Setup uses the app's existing public tools. It is provisioning, not a workflow engine.
type ProjectPresetSetupStep struct {
	MinAppVersion    string         `json:"min_app_version,omitempty"`
	RequiresOperator bool           `json:"requires_operator,omitempty"`
	Description      string         `json:"description,omitempty"`
	Key              string         `json:"key"`
	App              string         `json:"app"`
	Tool             string         `json:"tool"`
	Title            string         `json:"title,omitempty"`
	Input            map[string]any `json:"input,omitempty"`
}

type ProjectPresetSetupProgress struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Bounded striped locks avoid retaining a mutex for every project ever seen.
var presetApplyLocks [64]sync.Mutex

func presetApplyLock(projectID string) *sync.Mutex {
	sum := sha256.Sum256([]byte(projectID))
	return &presetApplyLocks[int(sum[0])%len(presetApplyLocks)]
}

func validatePresetSetup(preset ProjectPreset) error {
	if len(preset.Setup) > 50 {
		return errors.New("preset may contain at most 50 setup steps")
	}
	agents := map[string]bool{}
	for _, a := range preset.Agents {
		agents[a.Key] = true
	}
	prior := map[string]bool{}
	for _, step := range preset.Setup {
		if !validPresetIdentifier(step.Key) || prior[step.Key] || !validPresetIdentifier(step.App) || strings.TrimSpace(step.Tool) == "" || len(step.Tool) > 128 || len(step.Title) > 160 || len(step.Description) > 1000 {
			return fmt.Errorf("invalid preset setup step %q", step.Key)
		}
		if step.MinAppVersion != "" {
			if _, err := semver.StrictNewVersion(step.MinAppVersion); err != nil {
				return fmt.Errorf("setup step %s has invalid min_app_version", step.Key)
			}
		}
		raw, err := json.Marshal(step.Input)
		if err != nil || len(raw) > 64<<10 {
			return fmt.Errorf("setup step %s input exceeds 64 KiB or is invalid", step.Key)
		}
		_, err = mapPresetSetupRefs(step.Input, func(ref string) (any, error) {
			p := strings.Split(ref, ".")
			if ref == "project.id" || ref == "preset.id" || ref == "step.key" || ref == "step.idempotency_key" {
				return nil, nil
			}
			if len(p) == 2 && p[0] == "agents" && agents[p[1]] {
				return nil, nil
			}
			if len(p) >= 2 && p[0] == "steps" && prior[p[1]] {
				return nil, nil
			}
			return nil, fmt.Errorf("invalid reference %q (steps must refer to earlier steps)", ref)
		}, 0)
		if err != nil {
			return fmt.Errorf("setup step %s: %w", step.Key, err)
		}
		// Scope/identity fields are owned by the server, never by a preset.
		for key := range step.Input {
			if strings.HasPrefix(key, "_") || key == "$ref" {
				return fmt.Errorf("setup step %s: reserved input %q", step.Key, key)
			}
		}
		prior[step.Key] = true
	}
	return nil
}

func mapPresetSetupRefs(value any, resolve func(string) (any, error), depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("setup input nesting exceeds 64 levels")
	}
	switch v := value.(type) {
	case map[string]any:
		if ref, exists := v["$ref"]; exists {
			name, ok := ref.(string)
			if !ok || name == "" || len(v) != 1 {
				return nil, errors.New("a reference must contain only a string $ref")
			}
			return resolve(name)
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			mapped, err := mapPresetSetupRefs(item, resolve, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = mapped
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			mapped, err := mapPresetSetupRefs(item, resolve, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = mapped
		}
		return out, nil
	default:
		return value, nil
	}
}

func resolvePresetSetupRef(ref, projectID, presetID, stepKey string, agents map[string]int64, results map[string]any) (any, error) {
	switch ref {
	case "preset.id":
		return presetID, nil
	case "step.key":
		return stepKey, nil
	case "step.idempotency_key":
		return "preset:" + presetID + ":" + stepKey, nil
	}
	p := strings.Split(ref, ".")
	if ref == "project.id" {
		return projectID, nil
	}
	if len(p) == 2 && p[0] == "agents" {
		if id := agents[p[1]]; id > 0 {
			return id, nil
		}
	}
	if len(p) >= 2 && p[0] == "steps" {
		value, ok := results[p[1]]
		if !ok {
			return nil, fmt.Errorf("step result %q is unavailable", p[1])
		}
		for _, key := range p[2:] {
			switch v := value.(type) {
			case map[string]any:
				value, ok = v[key]
			case []any:
				i, e := strconv.Atoi(key)
				ok = e == nil && i >= 0 && i < len(v)
				if ok {
					value = v[i]
				}
			default:
				ok = false
			}
			if !ok {
				return nil, fmt.Errorf("reference %q does not exist", ref)
			}
		}
		return value, nil
	}
	return nil, fmt.Errorf("reference %q is unavailable", ref)
}

// Results retain JSON number precision when passing IDs from one tool to another.
func decodePresetSetupJSON(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected data after JSON result")
	}
	return nil
}

func (s *Server) applyPresetSetup(r *http.Request, projectID string, preset ProjectPreset, agents map[string]int64, retryKeys []string) ([]ProjectPresetSetupProgress, error) {
	progress := make([]ProjectPresetSetupProgress, len(preset.Setup))
	retry := map[string]bool{}
	for _, key := range retryKeys {
		retry[key] = true
	}
	for i, step := range preset.Setup {
		progress[i] = ProjectPresetSetupProgress{Key: step.Key, Status: "pending"}
	}
	results := map[string]any{}
	visible, err := s.visibleProjectPresetApps(projectID)
	if err != nil && len(preset.Setup) > 0 {
		return progress, fmt.Errorf("load setup apps: %w", err)
	}
	fail := func(i int, status string, err error) ([]ProjectPresetSetupProgress, error) {
		progress[i].Status = status
		progress[i].Error = err.Error()
		return progress, fmt.Errorf("Setup step %s: %w", preset.Setup[i].Key, err)
	}
	for i, step := range preset.Setup {
		if err := r.Context().Err(); err != nil {
			return fail(i, "blocked", err)
		}
		input, err := mapPresetSetupRefs(step.Input, func(ref string) (any, error) {
			return resolvePresetSetupRef(ref, projectID, preset.ID, step.Key, agents, results)
		}, 0)
		if err != nil {
			return fail(i, "blocked", err)
		}
		if input != nil {
			args, ok := input.(map[string]any)
			if !ok {
				return fail(i, "blocked", errors.New("tool input must be an object"))
			}
			for key := range args {
				if strings.HasPrefix(key, "_") {
					return fail(i, "blocked", fmt.Errorf("reserved input %q", key))
				}
			}
		}
		installID := visible[step.App].InstallID
		if installID == 0 || s.installedApps == nil {
			return fail(i, "blocked", fmt.Errorf("app %s must be installed and running", step.App))
		}
		entry := s.installedApps.Get(installID)
		if entry == nil || entry.SidecarURL == "" {
			return fail(i, "blocked", fmt.Errorf("app %s is unavailable", step.App))
		}
		if err := presetSetupVersionRequirement(step, entry.Manifest.Version); err != nil {
			return fail(i, "blocked", err)
		}
		if step.RequiresOperator && !isPresetOperator(r) {
			return fail(i, "blocked", errors.New("this setup step requires a signed-in operator or operator API key"))
		}
		if isPlatformBackupApp(entry) && !s.isAdmin(getUserID(r)) {
			return fail(i, "blocked", errors.New("platform administrator required for this app"))
		}
		if asyncResultSpecForTool(entry, step.Tool) != nil {
			return fail(i, "blocked", errors.New("setup requires a synchronous tool; async jobs are not supported"))
		}
		// Restrict to the same public manifest surface available to agents. In
		// particular, app-only tools cannot be invoked by a user-authored preset.
		allowed := false
		for _, tool := range agentVisibleMCPTools(entry.Manifest.Provides.MCPTools) {
			if tool.Name == step.Tool {
				allowed = true
				break
			}
		}
		if !allowed {
			return fail(i, "blocked", fmt.Errorf("public tool %s is not declared by %s", step.Tool, step.App))
		}
		// A changed definition, resolved input, or app installation is not a safe retry.
		raw, err := json.Marshal(map[string]any{"app": step.App, "tool": step.Tool, "input": input, "install_id": installID})
		if err != nil {
			return fail(i, "blocked", err)
		}
		if len(raw) > 256<<10 {
			return fail(i, "blocked", errors.New("resolved setup input exceeds 256 KiB"))
		}
		fingerprint := fmt.Sprintf("%x", sha256.Sum256(raw))
		var savedHash, status, result, detail string
		err = s.store.db.QueryRow(`SELECT fingerprint,status,result_json,error FROM preset_setup_steps WHERE project_id=? AND preset_id=? AND step_key=?`, projectID, preset.ID, step.Key).Scan(&savedHash, &status, &result, &detail)
		switch {
		case err == nil:
			if savedHash != fingerprint {
				return fail(i, "blocked", errors.New("this step was already attempted with different inputs or installation; reconcile its app data before changing the step key"))
			}
			if status == "completed" {
				var value any
				if err := decodePresetSetupJSON([]byte(result), &value); err != nil {
					return fail(i, "blocked", errors.New("saved result cannot be read"))
				}
				results[step.Key] = value
				progress[i].Status = "completed"
				continue
			}
			if status != "uncertain" || !retry[step.Key] {
				if detail == "" {
					detail = "The previous call may have made changes. Check the app before explicitly retrying this step."
				}
				return fail(i, status, errors.New(detail))
			}
			res, e := s.store.db.Exec(`UPDATE preset_setup_steps SET status='running',error='',updated_at=CURRENT_TIMESTAMP WHERE project_id=? AND preset_id=? AND step_key=? AND status='uncertain'`, projectID, preset.ID, step.Key)
			if e != nil {
				return fail(i, "blocked", e)
			}
			count, e := res.RowsAffected()
			if e != nil || count != 1 {
				return fail(i, "blocked", errors.New("setup step was claimed by another apply"))
			}
		case errors.Is(err, sql.ErrNoRows):
			_, err = s.store.db.Exec(`INSERT INTO preset_setup_steps(project_id,preset_id,step_key,fingerprint,status) VALUES(?,?,?,?,'running')`, projectID, preset.ID, step.Key, fingerprint)
			if err != nil {
				return fail(i, "blocked", err)
			}
		default:
			return fail(i, "blocked", err)
		}
		value, callErr := s.callPresetSetupTool(r, projectID, installID, step, input)
		if callErr != nil {
			// Even a reported tool error can follow a partial write. Never replay it automatically.
			detail = "Check the app before retrying; this call may have made changes: " + callErr.Error()
			_, saveErr := s.store.db.Exec(`UPDATE preset_setup_steps SET status='uncertain',error=?,updated_at=CURRENT_TIMESTAMP WHERE project_id=? AND preset_id=? AND step_key=?`, detail, projectID, preset.ID, step.Key)
			if saveErr != nil {
				detail += " (could not persist the failure; restart recovery will retain the uncertainty)"
			}
			return fail(i, "uncertain", errors.New(detail))
		}
		raw, err = json.Marshal(value)
		if err != nil {
			return fail(i, "uncertain", errors.New("tool finished but its result could not be encoded"))
		}
		_, err = s.store.db.Exec(`UPDATE preset_setup_steps SET status='completed',result_json=?,error='',updated_at=CURRENT_TIMESTAMP WHERE project_id=? AND preset_id=? AND step_key=?`, string(raw), projectID, preset.ID, step.Key)
		if err != nil {
			return fail(i, "uncertain", errors.New("tool finished but its result could not be saved; check the app before retrying"))
		}
		results[step.Key] = value
		progress[i].Status = "completed"
	}
	return progress, nil
}

// The operator-facing proxy preserves authorization, project injection, trusted
// principal headers, file handling and the ordinary app admission controls.
func (s *Server) callPresetSetupTool(parent *http.Request, projectID string, installID int64, step ProjectPresetSetupStep, input any) (result any, callErr error) {
	// ReverseProxy aborts on a truncated upstream body. Preserve uncertainty
	// instead of losing the apply response and leaving a running record behind.
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				result = nil
				callErr = errors.New("app response was interrupted")
				return
			}
			panic(recovered)
		}
	}()
	if input == nil {
		input = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": step.Tool, "arguments": input}})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent.Context(), 2*time.Minute)
	defer cancel()
	target := fmt.Sprintf("/apps/%s/_install/%d/mcp?project_id=%s", step.App, installID, url.QueryEscape(projectID))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header = parent.Header.Clone()
	// Mint an operator subject only from the identity set by authMiddleware.
	// The normal app proxy signs it after enforcing project access. Delegated
	// application users and agent callers never acquire operator authority.
	if isPresetOperator(parent) {
		request.Header.Set("X-Apteva-Subject-Type", "preset_operator")
		request.Header.Set("X-Apteva-Subject-ID", parent.Header.Get("X-Apteva-Operator-ID"))
		request.Header.Set("X-Apteva-Project-ID", projectID)
		request.Header.Del("X-Apteva-Caller-Agent")
		request.Header.Del("X-Apteva-Caller-Instance")
		request.Header.Del("X-Apteva-Caller-Thread")
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Del("Content-Length")
	request.Header.Del("Content-Encoding")
	request.Header.Del("Accept-Encoding")
	request.Header.Del("Connection")
	request.Header.Del("Upgrade")
	recorder := &presetSetupResponse{header: make(http.Header)}
	s.handleAppProxy(recorder, request)
	if recorder.overflow {
		return nil, errors.New("tool response exceeds 1 MiB")
	}
	if recorder.status < 200 || recorder.status >= 300 {
		return nil, fmt.Errorf("app call returned HTTP %d", recorder.status)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := decodePresetSetupJSON(recorder.body.Bytes(), &rpc); err != nil {
		return nil, errors.New("tool returned an invalid JSON-RPC response")
	}
	if len(rpc.Error) > 0 && string(rpc.Error) != "null" {
		var detail struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rpc.Error, &detail)
		return nil, fmt.Errorf("app returned a JSON-RPC error: %s", cleanPresetText(detail.Message, 500))
	}
	var value map[string]any
	if len(rpc.Result) == 0 || decodePresetSetupJSON(rpc.Result, &value) != nil || value == nil {
		return nil, errors.New("tool returned no usable result")
	}
	if failed, _ := value["isError"].(bool); failed {
		message := "app tool reported an error"
		if content, ok := value["content"].([]any); ok {
			for _, part := range content {
				if item, ok := part.(map[string]any); ok {
					if text, ok := item["text"].(string); ok && text != "" {
						message += ": " + cleanPresetText(text, 500)
						break
					}
				}
			}
		}
		return nil, errors.New(message)
	}
	if structured, ok := value["structuredContent"]; ok && structured != nil {
		return structured, nil
	}
	if content, ok := value["content"].([]any); ok && len(content) == 1 {
		item, _ := content[0].(map[string]any)
		if item["type"] == "text" {
			if text, ok := item["text"].(string); ok {
				var decoded any
				if decodePresetSetupJSON([]byte(text), &decoded) == nil {
					return decoded, nil
				}
			}
		}
	}
	return value, nil
}

// Bound the internal proxy response without retaining arbitrarily large tool output.
type presetSetupResponse struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *presetSetupResponse) Header() http.Header { return w.header }
func (w *presetSetupResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *presetSetupResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(p) > 1<<20 {
		w.overflow = true
		return 0, errors.New("preset tool response too large")
	}
	return w.body.Write(p)
}
func (w *presetSetupResponse) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}

// Preview reads only statuses, never prior tool data or app credentials.
func (s *Server) presetSetupProgress(projectID string, preset ProjectPreset) ([]ProjectPresetSetupProgress, error) {
	if len(preset.Setup) == 0 {
		return nil, nil
	}
	rows, err := s.store.db.Query(`SELECT step_key,status,error FROM preset_setup_steps WHERE project_id=? AND preset_id=?`, projectID, preset.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	saved := map[string]ProjectPresetSetupProgress{}
	for rows.Next() {
		var item ProjectPresetSetupProgress
		if err := rows.Scan(&item.Key, &item.Status, &item.Error); err != nil {
			return nil, err
		}
		saved[item.Key] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ProjectPresetSetupProgress, 0, len(preset.Setup))
	for _, step := range preset.Setup {
		item, ok := saved[step.Key]
		if !ok {
			item = ProjectPresetSetupProgress{Key: step.Key, Status: "pending"}
		}
		out = append(out, item)
	}
	return out, nil
}

func isPresetOperator(r *http.Request) bool {
	id := getUserID(r)
	return id > 0 && r.Header.Get("X-Apteva-Operator-ID") == itoa(id) && r.Header.Get("X-Apteva-App-Install-ID") == "" && r.Header.Get("X-Apteva-Subject-Type") == ""
}

func presetSetupVersionRequirement(step ProjectPresetSetupStep, version string) error {
	if step.MinAppVersion == "" {
		return nil
	}
	required, err := semver.StrictNewVersion(step.MinAppVersion)
	if err != nil {
		return fmt.Errorf("invalid minimum app version for %s", step.App)
	}
	current, err := semver.StrictNewVersion(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	if err != nil || current.LessThan(required) {
		return fmt.Errorf("update %s to %s or newer before applying %q (installed version: %s)", step.App, step.MinAppVersion, step.Title, version)
	}
	return nil
}
