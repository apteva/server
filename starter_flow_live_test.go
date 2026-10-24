package main

// Opt-in end-to-end smoke: real Core and locally cached app executables, all
// copied into a temporary installation with fresh databases. Existing projects
// are never opened for writing. Uses the locally configured Codex provider.
import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStarterFlowLive(t *testing.T) {
	if os.Getenv("APTEVA_RUN_STARTER_FLOW") != "1" {
		t.Skip("set APTEVA_RUN_STARTER_FLOW=1 for isolated real Core/app smoke")
	}
	state := loadOpenAICodexProviderState(t)
	s := newTestServer(t)
	s.dataDir = t.TempDir()
	seedPresetProject(t, s, "starter-live")
	s.agents = NewAgentManager(filepath.Join(s.dataDir, "agents"), findCoreBinary(t))
	s.agents.internalMCPSecret = s.instanceSecret
	s.agents.AuthorizeMCPConfig = s.authorizeAgentMCPConfig
	t.Cleanup(func() {
		rows, _ := s.store.ListAgentsInProject("starter-live")
		for _, a := range rows {
			s.agents.Stop(a.ID)
		}
	})
	s.localApps = NewLocalSupervisor(filepath.Join(s.dataDir, "app-cache"))
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	s.installedApps = NewInstalledAppsRegistry()
	s.appBus = NewAppEventBus()
	s.appEventDispatcher = NewAppEventDispatcher(s)
	s.appEventDispatcher.Start()
	t.Cleanup(s.appEventDispatcher.Stop)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.port = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	t.Setenv("PORT", s.port)
	api := http.NewServeMux()
	api.HandleFunc(strings.TrimPrefix(platformMCPPath, "/api"), s.handlePlatformMCP)
	s.registerAppRuntimeRoutes(api)
	registerRealManagementRoutes(s, api)
	api.HandleFunc("/telemetry/live", s.handleLiveTelemetry)
	api.HandleFunc("/telemetry", s.handleIngestTelemetry)
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", api))
	mux.HandleFunc("/mcp/", s.handleMCPEndpoint)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })
	rootURL := "http://127.0.0.1:" + s.port
	encrypted, _ := Encrypt(s.secret, string(mustJSON(state)))
	if _, err = s.store.CreateProvider(1, 15, "llm", "OpenAI Codex", encrypted); err != nil {
		t.Fatal(err)
	}
	key := "apt_starter_isolated_test"
	if _, err = s.store.CreateAPIKey(1, "starter-flow", HashAPIKey(key), key[:8]); err != nil {
		t.Fatal(err)
	}
	// Reuse verified local releases without rebuilding or sharing writable data.
	home, _ := os.UserHomeDir()
	localDB, err := sql.Open("sqlite", "file:"+filepath.Join(home, ".apteva", "apteva.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer localDB.Close()
	catalog, _ := loadProjectPresetCatalog()
	preset := catalog.ByID["personal-assistant"]
	for _, name := range projectPresetApps(preset) {
		var raw, bin string
		err = localDB.QueryRow(`SELECT COALESCE(NULLIF(i.manifest_json,''),a.manifest_json),i.local_bin_path FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE a.name=? AND i.local_bin_path<>'' ORDER BY i.id DESC LIMIT 1`, name).Scan(&raw, &bin)
		if err != nil {
			matches, _ := filepath.Glob(filepath.Join(home, ".apteva", "apps", name, "*", "src", "mcp", name, "apteva.yaml"))
			if len(matches) == 0 {
				t.Fatalf("cached %s release required: %v", name, err)
			}
			manifestPath := matches[len(matches)-1]
			data, e := os.ReadFile(manifestPath)
			if e != nil {
				t.Fatal(e)
			}
			parsed, e := sdk.ParseManifest(data)
			if e != nil {
				t.Fatal(e)
			}
			raw = string(mustJSON(parsed))
			bin = filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(manifestPath)))), "bin")
		}
		var manifest sdk.Manifest
		if err = json.Unmarshal([]byte(raw), &manifest); err != nil {
			t.Fatal(err)
		}
		res, err := s.store.db.Exec(`INSERT INTO apps(name,source,repo,ref,manifest_json) VALUES(?,'git','','',?)`, name, raw)
		if err != nil {
			t.Fatal(err)
		}
		appID, _ := res.LastInsertId()
		permissions, _ := json.Marshal(manifest.Requires.Permissions)
		res, err = s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,installed_by,version,manifest_json,permissions_json) VALUES(?,'starter-live','running',1,?,?,?)`, appID, manifest.Version, raw, string(permissions))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		appDir := filepath.Join(s.dataDir, "packages", name)
		os.MkdirAll(appDir, 0700)
		bytes, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		copiedBin := filepath.Join(appDir, "bin")
		if err = os.WriteFile(copiedBin, bytes, 0700); err != nil {
			t.Fatal(err)
		}
		// Installed source entries carry migrations/UI, referenced read-only.
		var src string
		candidates := []string{filepath.Join(filepath.Dir(bin), "src", "mcp", name), filepath.Join(filepath.Dir(bin), "src")}
		for _, candidate := range candidates {
			if _, err := os.Stat(filepath.Join(candidate, "migrations")); err == nil {
				src = candidate
				break
			}
		}
		port, err := freePort()
		if err != nil {
			t.Fatal(err)
		}
		token, err := s.appInstallToken(id)
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"APTEVA_GATEWAY_URL": rootURL, "APTEVA_APP_TOKEN": token, "APTEVA_INSTALL_ID": itoa64(id), "APTEVA_PROJECT_ID": "starter-live", "APTEVA_BIND_HOST": "127.0.0.1", "APTEVA_MIGRATIONS_DIR": filepath.Join(src, "migrations"), "APTEVA_UI_DIR": filepath.Join(src, "ui")}
		spec, err := newActivationSpec(id, &manifest, copiedBin, port, env)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.localApps.spawn(spec); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			resp, e := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
			if e == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					ready = true
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !ready {
			t.Fatalf("%s did not become ready; see %s", name, appDir)
		}
		s.store.db.Exec(`UPDATE app_installs SET local_port=?,sidecar_url_override=? WHERE id=?`, port, fmt.Sprintf("http://127.0.0.1:%d", port), id)
		entry := &InstalledApp{InstallID: id, AppName: name, ProjectID: "starter-live", Manifest: manifest, SidecarURL: fmt.Sprintf("http://127.0.0.1:%d", port), Token: token, Permissions: manifest.Requires.Permissions}
		s.installedApps.Add(entry)
		if err = s.registerAppMCP(id); err != nil {
			t.Fatal(err)
		}
	}
	s.dbPath = filepath.Join(s.dataDir, "test.db") // newTestServer may use a separate store path; derive the actual path.
	s.store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&s.dbPath)
	serverBin := findServerBinary(t)
	s.platformGatewayExec = func(ctx context.Context, userID int64, project string, body []byte) ([]byte, error) {
		cmd := exec.CommandContext(ctx, serverBin, "--mcp-gateway", "--user-id="+itoa64(userID))
		cmd.Env = childEnvWithOverrides(os.Environ(), map[string]string{"DB_PATH": s.dbPath, "DATA_DIR": s.dataDir, "PORT": s.port, "PROJECT_ID": project, "INSTANCE_SECRET": s.instanceSecret, "AGENT_SECRET": s.instanceSecret, "SERVER_SECRET": hex.EncodeToString(s.secret), "APTEVA_INTERNAL_SERVER_URL": rootURL})
		cmd.Stdin = bytes.NewReader(append(body, '\n'))
		out, err := cmd.Output()
		return bytes.TrimSpace(out), err
	}
	req := authedRequest(t, "POST", "/projects/starter-live/setup/apply", "", map[string]any{"preset_id": preset.ID, "description": "Isolated local acceptance: respond only to direct requests; create specialists only when explicitly requested."})
	req.Header.Set("X-Apteva-Operator-ID", "1")
	rec := httptest.NewRecorder()
	s.handleProject(rec, req)
	if rec.Code != 200 {
		t.Fatalf("apply status=%d", rec.Code)
	}
	rows, _ := s.store.ListAgentsInProject("starter-live")
	if len(rows) != 1 {
		t.Fatal("starter not created")
	}
	starter, _ := s.store.GetAgentByID(rows[0].ID)
	if starter.Status != "running" {
		t.Fatalf("starter status=%s", starter.Status)
	}
	request := func(method, path string, body any) []byte {
		t.Helper()
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(mustJSON(body))
		}
		req, _ := http.NewRequest(method, rootURL+"/api/apps/conversations/"+path+"&project_id=starter-live", reader)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("Conversations %s: HTTP %d %s", path, resp.StatusCode, data)
		}
		return data
	}
	var chat struct {
		ID string `json:"id"`
	}
	json.Unmarshal(request("POST", "chats?flow=starter", map[string]any{"agent_id": starter.ID, "project_id": "starter-live", "title": "Starter acceptance", "audience": "operator", "conversation_key": "starter-acceptance"}), &chat)
	if chat.ID == "" {
		t.Fatal("no conversation")
	}
	request("POST", "messages?flow=starter", map[string]any{"chat_id": chat.ID, "client_id": "create-specialist-once", "content": "Create and start one agent named Local Research Helper in this project. Its job is to answer my questions and coordinate with you when explicitly assigned. Attach only Agent to Agent, Conversations, Notes and Tasks. Do not copy your management permissions. Use idempotency key local-research-helper-v1. Do not ask for another confirmation: this is my explicit request. Tell me when it is ready and give me the link to converse with it."})
	var child *Agent
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); {
		rows, _ := s.store.ListAgentsInProject("starter-live")
		for _, row := range rows {
			if row.ID != starter.ID {
				child, _ = s.store.GetAgentByID(row.ID)
			}
		}
		if child != nil && child.Status == "running" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if child == nil || child.Status != "running" {
		t.Fatal("starter did not create/start specialist before deadline")
	}
	t.Logf("Created specialist mode=%s", child.Mode)
	if platformMCPAttached(child) {
		t.Fatal("specialist inherited management")
	}
	var childChat struct {
		ID string `json:"id"`
	}
	json.Unmarshal(request("POST", "chats?flow=child", map[string]any{"agent_id": child.ID, "project_id": "starter-live", "title": "Child acceptance", "audience": "operator", "conversation_key": "child-acceptance"}), &childChat)
	request("POST", "messages?flow=child", map[string]any{"chat_id": childChat.ID, "client_id": "child-greeting", "content": "Reply exactly: Ready for research."})
	waitTranscript := func(chatID, needle string) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
			raw := request("GET", "messages?chat_id="+chatID, nil)
			var messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			json.Unmarshal(raw, &messages)
			for _, m := range messages {
				if (m.Role == "agent" || m.Role == "assistant") && strings.Contains(strings.ToLower(m.Content), strings.ToLower(needle)) {
					return
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("missing assistant response containing %q", needle)
	}
	waitTranscript(childChat.ID, "Ready for research")
	request("POST", "messages?flow=delegation", map[string]any{"chat_id": chat.ID, "client_id": "bounded-a2a", "content": "Ask Local Research Helper through Agent to Agent for a short acknowledgement that it is ready for bounded research assignments. Do not do any research or create a task in Tasks for this tiny exchange. Wait for its reply, then tell me the result."})
	var a2aID int64
	s.store.db.QueryRow(`SELECT i.id FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE a.name='a2a'`).Scan(&a2aID)
	ledger, err := sql.Open("sqlite", "file:"+filepath.Join(s.localApps.cacheDir, "a2a", "data", itoa64(a2aID), "app.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	complete := false
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); {
		var count int
		ledger.QueryRow(`SELECT COUNT(*) FROM a2a_tasks WHERE kind='ask' AND status='completed'`).Scan(&count)
		if count > 0 {
			complete = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !complete {
		t.Fatal("A2A exchange did not complete")
	}
	rows, _ = s.store.ListAgentsInProject("starter-live")
	if len(rows) != 2 {
		t.Fatalf("unexpected agent count %d", len(rows))
	}
	t.Log("PASS: preset -> starter conversation -> create/start one specialist -> direct specialist conversation -> completed A2A exchange; all databases isolated")
}
