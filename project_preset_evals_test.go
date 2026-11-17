package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

var presetEvalTargets = map[string]string{
	"work-sales":                   "sales",
	"business-lead-generation":     "lead-ops",
	"work-support":                 "support",
	"development-software":         "engineer",
	"development-qa":               "qa",
	"development-engineering-team": "maintainer",
	"development-product-team":     "product-lead",
}

func presetEvalSteps(p ProjectPreset) ProjectPreset {
	steps := []ProjectPresetSetupStep{}
	for _, step := range p.Setup {
		if step.App == "evals" {
			steps = append(steps, step)
		}
	}
	p.Setup = steps
	return p
}

// These recipes are executable public-tool inputs, so check their safety and
// reference graph rather than comparing a snapshot of their presentation text.
func TestProjectPresetEvalsRecipes(t *testing.T) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	found := 0
	for _, p := range catalog.Presets {
		evals := presetEvalSteps(p)
		target, selected := presetEvalTargets[p.ID]
		if !selected {
			if len(evals.Setup) != 0 {
				t.Fatalf("unexpected Evals setup in %s", p.ID)
			}
			continue
		}
		found++
		t.Run(p.ID, func(t *testing.T) {
			if err := validateProjectPreset(p); err != nil {
				t.Fatal(err)
			}
			if !containsString(projectPresetApps(p), "evals") {
				t.Fatal("setup must install Evals through the ordinary resolver")
			}
			if len(evals.Setup) != 4 {
				t.Fatalf("want one suite and three cases, got %d steps", len(evals.Setup))
			}
			suite := evals.Setup[0]
			if suite.Tool != "eval_suite_create" || suite.Input["schedule_minutes"] != float64(0) || suite.Input["judge_model"] != nil {
				t.Fatal("suite must be manual and provider independent")
			}
			wantTargets := []any{map[string]any{"agent_id": map[string]any{"$ref": "agents." + target}}}
			if !reflect.DeepEqual(suite.Input["continuous_targets"], wantTargets) {
				t.Fatalf("incorrect target: %#v", suite.Input["continuous_targets"])
			}
			var targetApps []string
			for _, agent := range p.Agents {
				if containsString(agent.Apps, "evals") {
					t.Fatal("Evals is a project service, not an agent tool attachment")
				}
				if agent.Key == target {
					targetApps = agent.Apps
				}
			}
			for i, step := range evals.Setup {
				if step.MinAppVersion != "0.9.5" || !step.RequiresOperator {
					t.Fatalf("missing compatibility/operator requirement: %s", step.Key)
				}
				id, ok := step.Input["id"].(string)
				if !ok || len(id) > 128 || strings.ContainsAny(id, ":/") || ids[id] {
					t.Fatalf("invalid or duplicate app record ID: %q", id)
				}
				ids[id] = true
				if i == 0 {
					continue
				}
				if step.Tool != "eval_case_create" || !reflect.DeepEqual(step.Input["suite_id"], map[string]any{"$ref": "steps." + suite.Key + ".id"}) {
					t.Fatalf("case must use suite tool result: %s", step.Key)
				}
				input := step.Input
				if input["rating_profile"] != "agentic-quality-v2" || input["timeout_seconds"].(float64) > 300 || input["max_turns"].(float64) > 20 {
					t.Fatal("missing bounded weighted evaluation")
				}
				env := input["environment"].(map[string]any)
				if env["version"] != float64(1) || env["network_mode"] != "block" || env["integration_mode"] != "mock" || env["ttl_seconds"].(float64) > 900 {
					t.Fatal("case must have a bounded isolated environment")
				}
				apps := map[string]bool{}
				for _, raw := range env["apps"].([]any) {
					app := raw.(string)
					if app != "tasks" && app != "crm" && app != "notes" && app != "tickets" {
						t.Fatalf("starter must not access host repositories or external services: %s", app)
					}
					if !containsString(targetApps, app) {
						t.Fatalf("target %s does not use fixture app %s", target, app)
					}
					apps[app] = true
				}
				for key := range env {
					switch key {
					case "version", "apps", "network_mode", "integration_mode", "ttl_seconds", "seeds":
					default:
						t.Fatalf("unexpected environment authority: %s", key)
					}
				}
				if seeds, ok := env["seeds"].([]any); ok {
					for _, raw := range seeds {
						seed := raw.(map[string]any)
						if !apps[seed["app"].(string)] {
							t.Fatal("seed app is outside isolated environment")
						}
					}
				}
				for _, raw := range input["assertions"].([]any) {
					a := raw.(map[string]any)
					app, _ := a["app"].(string)
					if app == "" {
						app, _ = a["mcp"].(string)
					}
					if !apps[app] || a["critical"] != true {
						t.Fatalf("assertion must be critical and scoped to fixture app: %#v", a)
					}
				}
				critical, flexible := false, false
				for _, raw := range input["goals"].([]any) {
					g := raw.(map[string]any)
					critical = critical || g["critical"] == true
					flexible = flexible || (g["critical"] == false && g["category"] == "communication")
				}
				if !critical || !flexible {
					t.Fatal("outcomes must be critical and presentation flexible")
				}
			}
		})
	}
	if found != 7 {
		t.Fatalf("got %d presets, want seven", found)
	}
}

func seedPresetEvalsApp(t *testing.T, s *Server, projectID, sidecarURL string) *InstalledApp {
	t.Helper()
	id := seedAppWithTools(t, s, "evals", projectID, []string{"eval_suite_create", "eval_case_create"})
	entry := &InstalledApp{InstallID: id, AppName: "evals", ProjectID: projectID, SidecarURL: sidecarURL, Token: "preset-evals-fixture-token", Manifest: sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "evals", Version: "0.9.5", Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{{Name: "eval_suite_create"}, {Name: "eval_case_create"}}}}}
	if s.installedApps == nil {
		s.installedApps = NewInstalledAppsRegistry()
	}
	s.installedApps.Add(entry)
	return entry
}

// Keep the fake at the HTTP app boundary: the real setup runner still performs
// access checks, proxying, typed references, receipts, and result decoding.
func presetEvalsFixture(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		calls.Add(1)
		result := rpc.Params.Arguments
		if rpc.Params.Name == "eval_suite_create" {
			targets := result["continuous_targets"].([]any)
			if _, ok := targets[0].(map[string]any)["agent_id"].(float64); !ok {
				t.Error("agent reference did not remain numeric")
			}
		} else if rpc.Params.Name != "eval_case_create" {
			t.Errorf("unexpected setup tool %s", rpc.Params.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"structuredContent": result}})
	}))
	t.Cleanup(sidecar.Close)
	return sidecar, calls
}

func TestProjectPresetEvalsSetupPreservesCompletedContent(t *testing.T) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for id, agent := range presetEvalTargets {
		t.Run(id, func(t *testing.T) {
			s := newTestServer(t)
			const projectID = "preset-evals-test"
			seedPresetProject(t, s, projectID)
			sidecar, calls := presetEvalsFixture(t)
			entry := seedPresetEvalsApp(t, s, projectID, sidecar.URL)
			p := presetEvalSteps(catalog.ByID[id])
			req := httptest.NewRequest(http.MethodPost, "/projects/"+projectID+"/setup/apply", nil)
			req.Header.Set("X-User-ID", "1")
			if _, err := s.applyPresetSetup(req, projectID, p, map[string]int64{agent: 123}, nil); err == nil || !strings.Contains(err.Error(), "requires a signed-in operator") {
				t.Fatalf("missing operator identity must block setup: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("setup called an app without operator identity")
			}
			req.Header.Set("X-Apteva-Operator-ID", "1")
			apply := func() {
				t.Helper()
				progress, err := s.applyPresetSetup(req, projectID, p, map[string]int64{agent: 123}, nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, step := range progress {
					if step.Status != "completed" {
						t.Fatalf("incomplete setup: %+v", progress)
					}
				}
			}
			apply()
			if calls.Load() != 4 {
				t.Fatalf("calls=%d, want four", calls.Load())
			}
			// Reconstruct the registry, as on restart. Completed calls must not replay
			// or overwrite user-edited app records, even on explicit retry.
			s.installedApps = NewInstalledAppsRegistry()
			s.installedApps.Add(entry)
			apply()
			if _, err := s.applyPresetSetup(req, projectID, p, map[string]int64{agent: 123}, []string{p.Setup[0].Key}); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 4 {
				t.Fatalf("reapply replayed %d completed calls", calls.Load()-4)
			}
			changed := p
			changed.Setup = append([]ProjectPresetSetupStep(nil), p.Setup...)
			changed.Setup[0].Input = map[string]any{"id": "changed", "name": "Changed recipe"}
			if _, err := s.applyPresetSetup(req, projectID, changed, map[string]int64{agent: 123}, nil); err == nil || !strings.Contains(err.Error(), "different inputs") {
				t.Fatalf("changing attempted setup must require reconciliation: %v", err)
			}
			entry.Manifest.Version = "0.9.4"
			if _, err := s.applyPresetSetup(req, projectID, p, map[string]int64{agent: 123}, nil); err == nil || !strings.Contains(err.Error(), "update evals to 0.9.5") {
				t.Fatalf("old Evals must be rejected: %v", err)
			}
			if calls.Load() != 4 {
				t.Fatal("old app received setup call")
			}
		})
	}
}

// Optional contract smoke test against a real released Evals executable, using
// only a temporary database and a fixture gateway. No agent/model is started.
// APTEVA_PRESET_EVALS_BINARY and APTEVA_PRESET_EVALS_MIGRATIONS must refer to
// the same supporting release (>=0.9.5).
func TestProjectPresetEvalsRealApp(t *testing.T) {
	binary := os.Getenv("APTEVA_PRESET_EVALS_BINARY")
	if binary == "" {
		t.Skip("set APTEVA_PRESET_EVALS_BINARY for the real app contract test")
	}
	migrations := os.Getenv("APTEVA_PRESET_EVALS_MIGRATIONS")
	if migrations == "" {
		t.Fatal("APTEVA_PRESET_EVALS_MIGRATIONS is required")
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Tool string `json:"tool"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		if r.URL.Path != "/api/apps/callback/apps/environments/call" || input.Tool != "environment_catalog" {
			t.Errorf("unexpected platform action %s %s", r.URL.Path, input.Tool)
			http.Error(w, "unexpected platform action", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(sdk.HeaderAppResultFormat, sdk.AppResultJSON)
		_ = json.NewEncoder(w).Encode(map[string]any{"assertion_types": []string{"app_state", "mcp_tool_call"}})
	}))
	defer gateway.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	dir := t.TempDir()
	logFile, err := os.Create(filepath.Join(dir, "evals.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(binary)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "APTEVA_") && !strings.HasPrefix(value, "DB_PATH=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "APTEVA_APP_TOKEN=preset-evals-fixture-token", "APTEVA_PROJECT_ID=preset-evals-test", "APTEVA_INSTALL_ID=1", "APTEVA_GATEWAY_URL="+gateway.URL, "APTEVA_BIND_HOST=127.0.0.1", fmt.Sprintf("APTEVA_APP_PORT=%d", port), "DB_PATH="+filepath.Join(dir, "evals.db"), "APTEVA_MIGRATIONS_DIR="+migrations)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		data, _ := os.ReadFile(logFile.Name())
		t.Fatalf("Evals did not become ready: %s", data)
	}
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	seedPresetProject(t, s, "preset-evals-test")
	seedPresetEvalsApp(t, s, "preset-evals-test", base)
	req := httptest.NewRequest(http.MethodPost, "/projects/preset-evals-test/setup/apply", nil)
	req.Header.Set("X-User-ID", "1")
	req.Header.Set("X-Apteva-Operator-ID", "1")
	for id, target := range presetEvalTargets {
		p := presetEvalSteps(catalog.ByID[id])
		if _, err := s.applyPresetSetup(req, "preset-evals-test", p, map[string]int64{target: 123}, nil); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	get := func(path string, out any) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, base+path, nil)
		request.Header.Set("Authorization", "Bearer preset-evals-fixture-token")
		resp, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: HTTP %d", path, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var suites []struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
		Schedule int    `json:"schedule_minutes"`
		Cases    []struct {
			Revision int `json:"revision"`
		} `json:"cases"`
	}
	get("/api/suites", &suites)
	if len(suites) != 7 {
		t.Fatalf("suites=%d, want seven", len(suites))
	}
	for _, suite := range suites {
		if suite.Revision != 4 || suite.Schedule != 0 || len(suite.Cases) != 3 {
			t.Fatalf("suite changed on reapply or has wrong cases/schedule: %+v", suite)
		}
		for _, c := range suite.Cases {
			if c.Revision != 1 {
				t.Fatal("case overwritten on reapply")
			}
		}
	}
	// Model an operator customizing a case, then reapply every preset. Neither
	// that customization nor any suite/case revision should be overwritten.
	var saved []map[string]any
	get("/api/suites", &saved)
	custom := saved[0]["cases"].([]any)[0].(map[string]any)
	custom["name"] = "Operator customized evaluation"
	raw, err := json.Marshal(custom)
	if err != nil {
		t.Fatal(err)
	}
	update, err := http.NewRequest(http.MethodPut, base+"/api/cases/"+custom["id"].(string), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	update.Header.Set("Authorization", "Bearer preset-evals-fixture-token")
	update.Header.Set("Content-Type", "application/json")
	response, err := client.Do(update)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("customize case: HTTP %d", response.StatusCode)
	}
	get("/api/suites", &saved)
	for id, target := range presetEvalTargets {
		if _, err := s.applyPresetSetup(req, "preset-evals-test", presetEvalSteps(catalog.ByID[id]), map[string]int64{target: 123}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var after []map[string]any
	get("/api/suites", &after)
	if !reflect.DeepEqual(saved, after) {
		t.Fatal("reapply changed customized content or revisions")
	}
	var experiments []any
	get("/api/experiments", &experiments)
	if len(experiments) != 0 {
		t.Fatal("setup must not start live experiments")
	}
}
