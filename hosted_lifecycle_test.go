package main

import (
	"context"
	"database/sql"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/internal/host"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestHostedIngressDoesNotTurnPublicClientsIntoLoopback(t *testing.T) {
	t.Setenv("APTEVA_HOST_PROXY_SECRET", "private-hop-key")
	var remote, scheme string
	handler := hostedIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote = r.RemoteAddr
		scheme = r.URL.Scheme
		if r.Header.Get("X-Apteva-Host-Token") != "" {
			t.Error("private key leaked to handler")
		}
		w.WriteHeader(204)
	}))
	for _, token := range []string{"", "spoofed"} {
		r := httptest.NewRequest("GET", "http://backend/api/apps/callback/test", nil)
		r.Header.Set("X-Apteva-Host-Token", token)
		r.Header.Set("X-Apteva-Host-Remote-Addr", "127.0.0.1:123")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("untrusted request status=%d", w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://backend/", nil)
	r.Header.Set("X-Apteva-Host-Token", "private-hop-key")
	r.Header.Set("X-Apteva-Host-Remote-Addr", "198.51.100.42:4555")
	r.Header.Set("X-Apteva-Host-Scheme", "https")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 204 || remote != "198.51.100.42:4555" || scheme != "https" {
		t.Fatalf("original peer/scheme lost: %d %s %s", w.Code, remote, scheme)
	}
}
func TestHostedStandbyDoesNotOpenDatabaseAndFencesOwnership(t *testing.T) {
	home, err := os.MkdirTemp(hostTestTempRoot(), "apteva-gate-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	dir := filepath.Join(home, "host")
	_ = os.Mkdir(dir, 0700)
	t.Setenv("APTEVA_HOME", home)
	t.Setenv("APTEVA_HOST_BACKEND_ADDR", "127.0.0.1:12345")
	socket := filepath.Join(dir, "candidate.sock")
	t.Setenv("APTEVA_HOST_SLOT_SOCKET", socket)
	type result struct {
		h   *hostedLifecycle
		err error
	}
	done := make(chan result, 1)
	go func() { h, err := enterHostedLifecycle(); done <- result{h, err} }()
	client := host.UnixClient(socket)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		var status map[string]any
		if host.Call(ctx, client, "GET", "/status", nil, &status) == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(home, "apteva.db")); !os.IsNotExist(err) {
		t.Fatal("standby touched the live database")
	}
	release, err := host.Lock(filepath.Join(dir, "runtime.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = host.Call(ctx, client, "POST", "/activate", nil, nil); err != nil {
		t.Fatal(err)
	}
	resultValue := <-done
	release()
	if resultValue.err == nil {
		resultValue.h.Close()
		t.Fatal("second controller acquired runtime ownership")
	}
}
func TestHostedSidecarAdoptionKeepsPIDAndPersistentData(t *testing.T) {

	bin := buildLocalSidecarFixture(t)
	old := NewLocalSupervisor(t.TempDir())
	spec := localSidecarSpec(t, 42, "keep-app", bin, 0, nil)
	if err := old.spawn(spec); err != nil {
		t.Fatal(err)
	}
	proc := old.currentProc(42)
	t.Cleanup(func() { _ = terminateProc(proc, time.Second) })
	if err := old.waitHealthy(42, spec.httpPort, "/health", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(old.cacheDir, "keep-app", "data", "42", "sentinel")
	if err := os.WriteFile(data, []byte("latest application data"), 0600); err != nil {
		t.Fatal(err)
	}
	next := NewLocalSupervisor(old.cacheDir)
	if err := next.Adopt(42, proc.cmd.Process.Pid, spec.httpPort, bin, &sdk.Manifest{Name: "keep-app", Runtime: sdk.Runtime{HealthCheck: "/health"}}); err != nil {
		t.Fatal(err)
	}
	if next.PID(42) != proc.cmd.Process.Pid {
		t.Fatal("adoption restarted the sidecar")
	}
	b, _ := os.ReadFile(data)
	if string(b) != "latest application data" {
		t.Fatal("adoption changed persistent data")
	}
	if err := next.Adopt(43, proc.cmd.Process.Pid, spec.httpPort, "/bin/not-this-process", &sdk.Manifest{Name: "keep-app"}); err == nil {
		t.Fatal("unrelated process was adopted")
	}
	if !processAlive(proc.cmd.Process.Pid) {
		t.Fatal("failed adoption killed the existing process")
	}
	_ = next.Stop(42)
}

func TestHostedPrivateCredentialsNeverReachSidecars(t *testing.T) {
	base := []string{"PATH=/bin", "APTEVA_HOST_PROXY_SECRET=secret", "APTEVA_HOST_SLOT_SOCKET=/private/slot", "APTEVA_HOST_SOCKET=/private/host", "APTEVA_HOST_BACKEND_ADDR=127.0.0.1:1234"}
	env := childEnvWithOverrides(base, map[string]string{"APTEVA_INSTALL_ID": "42"})
	for _, key := range []string{"APTEVA_HOST_PROXY_SECRET", "APTEVA_HOST_SLOT_SOCKET", "APTEVA_HOST_SOCKET", "APTEVA_HOST_BACKEND_ADDR"} {
		assertEnvMissing(t, env, key)
	}
	assertEnvValue(t, env, "PATH", "/bin")
	assertEnvValue(t, env, "APTEVA_INSTALL_ID", "42")
}

func TestHostedAgentAdoptionIncludesHelpersAndPreservesFailedRuntime(t *testing.T) {
	for _, uptime := range []int{20, 0} {
		t.Run(fmt.Sprintf("uptime-%d", uptime), func(t *testing.T) {
			t.Setenv("APTEVA_AGENT_BOOT_RESUME", "manual")
			s := newTestServer(t)
			u, err := s.store.CreateUser("handoff@test.local", "hash")
			if err != nil {
				t.Fatal(err)
			}
			a, err := s.store.CreateAgent(u.ID, "helper", "wait", "autonomous", "{}", "")
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command("/bin/sleep", "60")
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/health":
					w.WriteHeader(200)
				case "/config":
					if r.Method == "GET" {
						writeJSON(w, map[string]any{"directive": "current work", "mcp_servers": []any{}})
					} else {
						w.WriteHeader(200)
					}
				case "/status":
					writeJSON(w, map[string]any{"core_version": "1.2.3", "uptime_seconds": uptime})
				default:
					w.WriteHeader(200)
				}
			}))
			defer core.Close()
			a.Kind = "meta"
			a.Pid = child.Process.Pid
			a.Port = core.Listener.Addr().(*net.TCPAddr).Port
			a.CoreAPIKey = "test-core"
			a.Status = "running"
			if err := s.store.UpdateAgent(a); err != nil {
				t.Fatal(err)
			}
			defer func() {
				s.agents.mu.Lock()
				p := s.agents.processes[a.ID]
				delete(s.agents.processes, a.ID)
				s.agents.mu.Unlock()
				if p != nil && p.channels != nil {
					p.channels.Stop()
				}
			}()
			err = s.adoptHostedAgents()
			if (err == nil) != (uptime > 0) {
				t.Fatalf("adoption err=%v uptime=%d", err, uptime)
			}
			latest, err := s.store.GetAgentByID(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if latest.Status != "running" || latest.Pid != a.Pid || latest.CoreAPIKey != a.CoreAPIKey || !processAlive(a.Pid) {
				t.Fatal("handoff killed or cleared existing helper runtime")
			}
			if uptime > 0 && !s.agents.IsRunning(a.ID) {
				t.Fatal("manual boot policy prevented live helper adoption")
			}
		})
	}
}

func TestHostedFreezeRejectsOutstandingWorkAndBlocksAdmission(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.agentRollouts = newAgentRolloutCoordinator(nil)
	finish, err := s.beginHostedWork()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.freezeHostedWork(); err == nil {
		t.Fatal("accepted active transfer")
	}
	finish()
	if !s.localApps.acquireInstall(42) {
		t.Fatal("install admission")
	}
	if err := s.freezeHostedWork(); err == nil {
		t.Fatal("accepted active install")
	}
	s.localApps.releaseInstall(42)
	s.agentRollouts.status.State = "running"
	if err := s.freezeHostedWork(); err == nil {
		t.Fatal("accepted active core rollout")
	}
	s.agentRollouts.status.State = "idle"
	if err := s.freezeHostedWork(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.beginHostedWork(); err == nil {
		t.Fatal("transfer admitted during handoff")
	}
	manager := &appRuntimeManager{s: s}
	for _, automated := range []bool{false, true} {
		var err error
		if automated {
			_, err = manager.beginExpected(42, "restart", "frozen-recovery", runtimePlacement{}, nil, "observed-owner")
		} else {
			_, err = manager.begin(42, "restart", "frozen-manual", runtimePlacement{}, nil)
		}
		if err == nil || err.Error() != "platform handoff is in progress" {
			t.Fatalf("automated=%v: runtime operation bypassed handoff admission: %v", automated, err)
		}
	}
	if s.localApps.acquireInstall(42) {
		t.Fatal("install admitted during handoff")
	}
	if _, err := s.agentRollouts.start([]int64{1}, nil, "all", "", "", 0); err == nil {
		t.Fatal("rollout admitted during handoff")
	}
	s.unfreezeHostedWork()
	finish, err = s.beginHostedWork()
	if err != nil {
		t.Fatal(err)
	}
	finish()
}

func TestHostedExistingStoreAndKeyNeverReplaceMissingData(t *testing.T) {
	t.Setenv("SERVER_SECRET", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.db")
	if store, err := OpenExistingStore(path); err == nil {
		store.Close()
		t.Fatal("created missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing database was created")
	}
	if _, err := LoadExistingSecret(dir); err == nil {
		t.Fatal("created missing key")
	}
	if _, err := os.Stat(filepath.Join(dir, ".secret")); !os.IsNotExist(err) {
		t.Fatal("missing key was replaced")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE sentinel(value TEXT); INSERT INTO sentinel VALUES('latest data')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := updateDatabaseFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := updateDatabaseSchema(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenExistingStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	after, err := updateDatabaseFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	nextSchema, err := updateDatabaseSchema(path)
	if err != nil {
		t.Fatal(err)
	}
	if before != after || schema != nextSchema {
		t.Fatal("existing-schema open migrated or transformed data")
	}
	for _, suffix := range []string{".recovery.json", ".restored.marker"} {
		marker := path + suffix
		if err := os.WriteFile(marker, []byte("pending restore"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateHostedSchema(path, schema); err == nil {
			t.Fatal("accepted pending restore")
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("consumed restore marker")
		}
		os.Remove(marker)
	}
}

func hostTestTempRoot() string {
	if runtime.GOOS == "darwin" {
		return "/private/tmp"
	}
	return os.TempDir()
}

func TestHostedManagedRuntimeUsesDurableProcessIdentity(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	manifest := sdk.Manifest{Name: "managed-handoff", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "source", HealthCheck: "/health"}}
	id := seedRunningInstall(t, s, manifest.Name, "", manifest, nil)
	bin := buildLocalSidecarFixture(t)
	spec := localSidecarSpec(t, id, manifest.Name, bin, 0, nil)
	if err := s.localApps.spawn(spec); err != nil {
		t.Fatal(err)
	}
	p := s.localApps.currentProc(id)
	t.Cleanup(func() { terminateProc(p, time.Second) })
	if err := s.localApps.waitHealthy(id, spec.httpPort, "/health", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	m := s.appRuntimeManager()
	d := runtimeDeployment{ID: "durable-handoff", InstallID: id, Status: "active", Placement: runtimePlacement{Type: "local"}, Manifest: manifest, LocalBin: bin, LocalPort: spec.httpPort, LocalPID: p.cmd.Process.Pid}
	if err := m.saveDeployment(d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET runtime_managed=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	// A new supervisor has no legacy map entry for managed deployments.
	s.localApps.mu.Lock()
	delete(s.localApps.procs, id)
	s.localApps.mu.Unlock()
	pids, err := s.hostedRuntimePIDs()
	if err != nil {
		t.Fatal(err)
	}
	if pids["deployment:"+d.ID] != p.cmd.Process.Pid {
		t.Fatal("managed runtime absent from handoff attestation")
	}
	d.LocalBin = "/not-the-running-process"
	if err := m.saveDeployment(d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.hostedRuntimePIDs(); err == nil {
		t.Fatal("accepted unrelated managed process")
	}
	if !processAlive(p.cmd.Process.Pid) {
		t.Fatal("failed attestation killed managed app")
	}
}
