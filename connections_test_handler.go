package main

// connections_test_handler.go — `POST /api/connections/:id/test` and
// the shared health-check runner.
//
// Pre-v0.13 the only validation a connection ever got was the
// reactive 401 → refresh → retry loop on a real tool call: an
// operator could type a wrong API key, see "active" instantly, and
// only learn it was bogus when an agent failed days later. The
// runner below uses each app's catalog-declared `health_check` to
// run a cheap probe (Slack auth.test, S3 list_buckets, /me on
// most REST APIs) under the same execution path a real tool call
// uses — credential templating, AWS-SigV4 signing, header building.
//
// Returns are stable shape so the dashboard can render either a
// green tick or a red X with the upstream's error message:
//
//   { "ok": true,  "latency_ms": 142, "status_code": 200 }
//   { "ok": false, "latency_ms":  87, "status_code": 403, "error": "InvalidAccessKeyId" }
//   { "ok": true,  "skipped": true,  "reason": "no health_check in catalog" }
//
// Same helper feeds the pre-flight check in handleCreateConnection's
// non-OAuth branch — if the user types in bad credentials, we
// refuse to save and surface the error in the form rather than
// pretending success.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// ConnectionTestResult is the JSON shape returned by the test
// endpoint AND surfaced to the dashboard creation form on
// pre-flight failure.
type ConnectionTestResult struct {
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped,omitempty"`
	Reason     string `json:"reason,omitempty"` // why skipped (e.g. "no health_check in catalog")
	LatencyMS  int64  `json:"latency_ms"`
	StatusCode int    `json:"status_code,omitempty"` // upstream HTTP status
	Error      string `json:"error,omitempty"`       // human-readable failure message
}

// handleTestConnection runs the configured health_check probe
// against the stored credentials of a single connection. Returns
// 200 with ConnectionTestResult either way — the OK field tells
// the dashboard whether to show success or failure. We don't 4xx
// on probe failure because a 4xx return + JSON body would
// double-confuse the dashboard's fetch wrapper.
func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/connections/"), "/test")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		writeJSON(w, ConnectionTestResult{
			OK:      false,
			Skipped: true,
			Reason:  fmt.Sprintf("app %q not in catalog", conn.AppSlug),
		})
		return
	}

	res := s.runHealthCheck(app, encCreds)
	writeJSON(w, res)
}

// runHealthCheck executes the configured probe and returns a
// ConnectionTestResult. Shared between the test endpoint and the
// pre-flight branch in handleCreateConnection. encCreds is the
// already-encrypted credentials blob from the DB; the caller does
// the lookup so this function is testable with a synthetic
// AppTemplate + plaintext creds.
func (s *Server) runHealthCheck(app *AppTemplate, encCreds string) ConnectionTestResult {
	hc := app.HealthCheck
	if hc == nil || (hc.Tool == "" && hc.Path == "") {
		return ConnectionTestResult{
			OK:      true,
			Skipped: true,
			Reason:  "no health_check in catalog",
		}
	}

	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		return ConnectionTestResult{
			OK:    false,
			Error: "decrypt credentials: " + err.Error(),
		}
	}
	var credentials map[string]string
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		return ConnectionTestResult{
			OK:    false,
			Error: "parse credentials: " + err.Error(),
		}
	}
	normalizeCredentials(credentials)

	// Resolve the AppToolDef the probe will run. Two flavors:
	//
	//   - hc.Tool != ""  → look up a real tool from app.Tools by
	//     name and reuse its definition. DRY: any fix to the
	//     tool's path/method/query_params benefits the probe too.
	//
	//   - hc.Tool == ""  → build a synthetic AppToolDef from
	//     hc.Method + hc.Path. Used when the probe URL doesn't
	//     match any exposed tool (a dedicated /healthz, a
	//     different host).
	//
	// In either flavor we hand the result to executeIntegrationTool
	// unmodified, which means AWS-SigV4 signing, {{region}}
	// templating in base_url, content-type defaulting, etc. all
	// behave identically to a real tool call.
	var tool *AppToolDef
	var input map[string]any
	if hc.Tool != "" {
		for i := range app.Tools {
			if app.Tools[i].Name == hc.Tool {
				t := app.Tools[i]
				tool = &t
				break
			}
		}
		if tool == nil {
			return ConnectionTestResult{
				OK:    false,
				Error: fmt.Sprintf("health_check.tool %q not in app.tools[]", hc.Tool),
			}
		}
		// Override timeout for the probe even when reusing a tool
		// — a normal tool call might tolerate 60s, but a health
		// check that takes 60s isn't useful as a "is this alive"
		// signal. Don't mutate the catalog tool; clone first.
		probeTool := *tool
		probeTool.TimeoutMS = 10000
		tool = &probeTool
		input = hc.Input
		if input == nil {
			input = map[string]any{}
		}
	} else {
		tool = &AppToolDef{
			Name:        "_health_check",
			Description: "internal: health check probe",
			Method:      hc.Method,
			Path:        hc.Path,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			TimeoutMS:   10000,
		}
		input = map[string]any{}
	}

	// Allow per-probe BaseURL override. SHALLOW copy of
	// AppTemplate so executeIntegrationTool reads our value
	// without mutating the catalog (a shared in-memory map; a
	// mutation would leak the override across future tool calls).
	probeApp := *app
	if hc.BaseURL != "" {
		probeApp.BaseURL = hc.BaseURL
	}

	expect := hc.ExpectStatus
	if len(expect) == 0 {
		expect = []int{200}
	}

	t0 := time.Now()
	result, execErr := executeIntegrationTool(&probeApp, tool, credentials, input, "")
	latency := time.Since(t0).Milliseconds()

	if execErr != nil {
		return ConnectionTestResult{
			OK:        false,
			LatencyMS: latency,
			Error:     execErr.Error(),
		}
	}
	for _, want := range expect {
		if result.Status == want {
			return ConnectionTestResult{
				OK:         true,
				LatencyMS:  latency,
				StatusCode: result.Status,
			}
		}
	}

	// Body-pattern fallback. Some upstreams return 4xx as their
	// "auth understood, permission denied" answer — for S3-compat,
	// a bucket-scoped token gets `403 AccessDenied` on ListBuckets
	// even though the token itself is perfectly valid. The catalog
	// lists the strings that prove "auth was at least accepted"
	// (e.g. "AccessDenied"); when status is 401/403 AND body
	// contains one of those patterns we treat the probe as OK.
	// Bad credentials still fail — they produce different bodies
	// (`InvalidAccessKeyId`, `SignatureDoesNotMatch`) that aren't
	// in the allowlist.
	body := previewBody(result.Data)
	if (result.Status == 401 || result.Status == 403) && len(hc.AuthOKWhenBodyContains) > 0 {
		// Use the FULL body for matching (truncation would lose
		// the discriminator string), and the truncated previewBody
		// only for the error/log output below.
		fullBody := stringifyData(result.Data)
		for _, needle := range hc.AuthOKWhenBodyContains {
			if needle != "" && strings.Contains(fullBody, needle) {
				log.Printf("[CONN-TEST] app=%s status=%d auth-ok-by-body match=%q",
					app.Slug, result.Status, needle)
				return ConnectionTestResult{
					OK:         true,
					LatencyMS:  latency,
					StatusCode: result.Status,
					Reason: fmt.Sprintf(
						"credentials valid (HTTP %d %s — token lacks scope for this probe but is otherwise authentic)",
						result.Status, needle),
				}
			}
		}
	}

	log.Printf("[CONN-TEST] app=%s status=%d body=%q", app.Slug, result.Status, body)
	return ConnectionTestResult{
		OK:         false,
		LatencyMS:  latency,
		StatusCode: result.Status,
		Error:      fmt.Sprintf("HTTP %d: %s", result.Status, body),
	}
}

// stringifyData turns ExecuteResult.Data into a single string for
// substring matching. Mirrors previewBody but without the 240-char
// truncation, since the discriminator pattern we're hunting for
// might appear past that boundary in a verbose error response.
func stringifyData(data any) string {
	if data == nil {
		return ""
	}
	switch v := data.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// previewBody renders ExecuteResult.Data as a short string for the
// operator-facing error preview. Strings come back verbatim;
// everything else gets JSON-marshalled. Truncated at 240 chars.
func previewBody(data any) string {
	if data == nil {
		return ""
	}
	var s string
	switch v := data.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		s = string(b)
	}
	s = strings.TrimSpace(s)
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}
