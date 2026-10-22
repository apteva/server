package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/internal/host"
)

// Opt-in real-server test. The directory must contain freshly built binaries
// named apteva-bluegreen-{server,server-next,host,cli}; server versions are
// stamped 0.90.6 and 0.90.7. All state and credentials are disposable.
func TestHostedRealServerHandoff(t *testing.T) {
	binaries := os.Getenv("APTEVA_HOST_E2E_BIN_DIR")
	if binaries == "" {
		t.Skip("set APTEVA_HOST_E2E_BIN_DIR to freshly built test binaries")
	}
	home, err := os.MkdirTemp(hostTestTempRoot(), "apteva-real-handoff-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			os.RemoveAll(home)
		} else {
			t.Logf("isolated debug state: %s", home)
		}
	})
	copyFile := func(source, dest string) {
		t.Helper()
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, raw, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for version, source := range map[string]string{"0.90.6": "server", "0.90.7": "server-next"} {
		dir := filepath.Join(home, "versions", version)
		copyFile(filepath.Join(binaries, "apteva-bluegreen-"+source), filepath.Join(dir, "apteva-server"))
		copyFile(filepath.Join(binaries, "apteva-bluegreen-cli"), filepath.Join(dir, "apteva"))
	}
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../versions/0.90.6", filepath.Join(home, "bin/current")); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(home, "apteva.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seed := &Server{store: store}
	key := testPrivateAPIKey(t, seed)
	secret, err := LoadSecret(home)
	if err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Name: "handoff-fixture", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "source", HealthCheck: "/health"}}
	install := seedRunningInstall(t, seed, manifest.Name, "", manifest, nil)
	sidecar := filepath.Join(home, "apps", manifest.Name, "1.0.0", "bin")
	copyFile(buildLocalSidecarFixture(t), sidecar)
	appPort := unusedTCPPort(t)
	events := filepath.Join(home, "sidecar-events")
	config, _ := json.Marshal(map[string]string{"TEST_SIDECAR_EVENTS": events})
	encrypted, err := Encrypt(secret, string(config))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE app_installs SET local_bin_path=?,local_port=?,config_encrypted=? WHERE id=?`, sidecar, appPort, encrypted, install); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(home, "apps", manifest.Name, "data", fmt.Sprint(install))
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	appDB := filepath.Join(dataDir, "app.db")
	db, err := sql.Open("sqlite", appDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE sentinel(value TEXT); INSERT INTO sentinel VALUES('latest app data')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	sentinel, err := os.ReadFile(appDB)
	if err != nil {
		t.Fatal(err)
	}
	// A live core fixture models persisted evolving configuration and a helper
	// already doing work. Its OS process is real; no LLM/network credentials.
	coreProcess := exec.Command("/bin/sleep", "120")
	if err := coreProcess.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { coreProcess.Process.Kill(); coreProcess.Wait() }()
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/status":
			writeJSON(w, map[string]any{"core_version": "0.90.6", "uptime_seconds": 60})
		case "/config":
			if r.Method == "GET" {
				writeJSON(w, map[string]any{"directive": "latest evolved work", "mcp_servers": []any{}})
			} else {
				w.WriteHeader(200)
			}
		default:
			writeJSON(w, map[string]any{})
		}
	}))
	defer core.Close()
	agent, err := store.CreateAgent(1, "surviving-helper", "wait", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	agent.Status = "running"
	agent.Port = core.Listener.Addr().(*net.TCPAddr).Port
	agent.Pid = coreProcess.Process.Pid
	agent.CoreAPIKey = "disposable-core-key"
	if err := store.UpdateAgent(agent); err != nil {
		t.Fatal(err)
	}
	port := unusedTCPPort(t)
	env := childEnvWithOverrides(os.Environ(), map[string]string{"APTEVA_HOME": home, "DB_PATH": dbPath, "DATA_DIR": home, "PORT": fmt.Sprint(port), "APTEVA_BIND": "127.0.0.1", "APTEVA_SERVER_BIN": filepath.Join(home, "versions/0.90.6/apteva-server"), "APTEVA_AGENT_UPDATE_POLICY": "preserve", "APTEVA_AGENT_BOOT_RESUME": "auto", "APTEVA_BOOT_META_AGENTS": "", "SERVER_SECRET": "", "APTEVA_HTTP_LISTEN_ADDR": "", "APTEVA_HTTPS_LISTEN_ADDR": "", "APTEVA_INGRESS_ENABLED": "", "QUIET": "1"})
	logPath := filepath.Join(home, "host.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(filepath.Join(binaries, "apteva-bluegreen-host"))
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(40 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			raw, _ := os.ReadFile(logPath)
			t.Log(string(raw))
		}
	})
	client := &http.Client{Timeout: 20 * time.Second}
	defer client.CloseIdleConnections()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	request := func(method, path string, body []byte) ([]byte, error) {
		req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 && resp.StatusCode != 201 {
			return raw, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, raw)
		}
		return raw, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := request("GET", "/health", nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		time.Sleep(30 * time.Millisecond)
	}
	var originalPID int
	if err := store.db.QueryRow(`SELECT local_pid FROM app_installs WHERE id=?`, install).Scan(&originalPID); err != nil || originalPID <= 0 {
		t.Fatalf("sidecar startup: %d %v", originalPID, err)
	}
	if _, err := store.db.Exec(`UPDATE agents SET kind='platform_helper' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	writesDone := make(chan error, 1)
	writes := 0
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				writesDone <- nil
				return
			default:
			}
			if _, err := request("POST", "/api/projects", []byte(fmt.Sprintf(`{"name":"handoff-write-%d"}`, i))); err != nil {
				writesDone <- err
				return
			}
			writes++
			time.Sleep(10 * time.Millisecond)
		}
	}()
	control := host.UnixClient(host.SocketPath(home))
	defer control.CloseIdleConnections()
	var state host.State
	if err := host.Call(context.Background(), control, "POST", "/deploy", host.Candidate{Binary: filepath.Join(home, "versions/0.90.7/apteva-server"), Version: "0.90.7", AgentPolicy: "preserve"}, &state); err != nil {
		close(stop)
		<-writesDone
		t.Fatal(err)
	}
	deadline = time.Now().Add(60 * time.Second)
	for {
		if err := host.Call(context.Background(), control, "GET", "/status", nil, &state); err != nil {
			t.Fatal(err)
		}
		if state.Job != nil && host.Terminal(state.Job.Phase) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deployment timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	if err := <-writesDone; err != nil {
		t.Fatal(err)
	}
	if state.Job.Phase != "succeeded" {
		t.Fatalf("deployment failed: %+v", state.Job)
	}
	// Rollback uses the public CLI and retains writes committed under green.
	link := filepath.Join(home, "bin/current")
	os.Remove(link)
	if err := os.Symlink("../versions/0.90.7", link); err != nil {
		t.Fatal(err)
	}
	rollback := exec.Command(filepath.Join(binaries, "apteva-bluegreen-cli"), "rollback", "0.90.6")
	rollback.Env = env
	if output, err := rollback.CombinedOutput(); err != nil {
		t.Fatalf("CLI rollback: %v %s", err, output)
	}
	var count, distinct, pid int
	if err := store.db.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT name) FROM projects WHERE name LIKE 'handoff-write-%'`).Scan(&count, &distinct); err != nil {
		t.Fatal(err)
	}
	if count != writes || count != distinct || writes == 0 {
		t.Fatalf("writes: committed=%d rows=%d unique=%d", writes, count, distinct)
	}
	if err := store.db.QueryRow(`SELECT local_pid FROM app_installs WHERE id=?`, install).Scan(&pid); err != nil || pid != originalPID || !processAlive(pid) {
		t.Fatal("sidecar PID changed")
	}
	eventsRaw, _ := os.ReadFile(events)
	if strings.Count(string(eventsRaw), "start ") != 1 || strings.Contains(string(eventsRaw), "stop ") {
		t.Fatalf("sidecar restarted: %s", eventsRaw)
	}
	raw, _ := os.ReadFile(appDB)
	if !bytes.Equal(raw, sentinel) {
		t.Fatal("app database changed")
	}
	nextKey, err := LoadExistingSecret(home)
	if err != nil || !bytes.Equal(nextKey, secret) {
		t.Fatal("encryption key changed")
	}
	latest, err := store.GetAgentByID(agent.ID)
	if err != nil || latest.Pid != agent.Pid || latest.Kind != "platform_helper" || !processAlive(agent.Pid) {
		t.Fatal("helper process was replaced or cleared")
	}
	t.Logf("update + CLI rollback: %d writes, zero lost/duplicate writes; app and helper PIDs, app DB and encryption key retained", writes)
}
