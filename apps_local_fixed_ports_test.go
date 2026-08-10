package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestHTTPOnlyActivationRemainsBlueGreen(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })
	bin := buildLocalSidecarFixture(t)
	events := filepath.Join(t.TempDir(), "events.log")

	oldSpec := localSidecarSpec(t, 1, "http-app", copyFixtureBinary(t, bin, "v1"), 0, map[string]string{
		"TEST_SIDECAR_ID":     "old",
		"TEST_SIDECAR_EVENTS": events,
	})
	if err := sup.activate(oldSpec, 5*time.Second); err != nil {
		t.Fatalf("start old: %v", err)
	}
	old := sup.currentProc(1)
	if old == nil {
		t.Fatal("old process not tracked")
	}

	newSpec := localSidecarSpec(t, 1, "http-app", copyFixtureBinary(t, bin, "v2"), 0, map[string]string{
		"TEST_SIDECAR_ID":     "new",
		"TEST_SIDECAR_EVENTS": events,
	})
	if err := sup.activate(newSpec, 5*time.Second); err != nil {
		t.Fatalf("activate new: %v", err)
	}

	sup.pendingMu.Lock()
	parked := sup.pending[1]
	sup.pendingMu.Unlock()
	if parked != old || !processAlive(old.cmd.Process.Pid) {
		t.Fatal("HTTP-only upgrade did not preserve the live old process for blue-green handoff")
	}
	if current := sup.currentProc(1); current == nil || current.spec.env["TEST_SIDECAR_ID"] != "new" {
		t.Fatal("new HTTP-only process is not active")
	}
	sup.RetireOld(1, 2*time.Second)
	waitLocalProcDone(t, old)
}

func TestFixedPortActivationStopsOldBeforeStartingNew(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })
	bin := buildLocalSidecarFixture(t)
	events := filepath.Join(t.TempDir(), "events.log")
	fixedPort := unusedTCPPort(t)

	oldSpec := localSidecarSpec(t, 11, "mqtt", copyFixtureBinary(t, bin, "v1"), fixedPort, map[string]string{
		"TEST_SIDECAR_ID":         "old",
		"TEST_SIDECAR_EVENTS":     events,
		"TEST_SIDECAR_FIXED_PORT": fmt.Sprint(fixedPort),
	})
	if err := sup.activate(oldSpec, 5*time.Second); err != nil {
		t.Fatalf("start old: %v", err)
	}
	old := sup.currentProc(11)

	newSpec := localSidecarSpec(t, 11, "mqtt", copyFixtureBinary(t, bin, "v2"), fixedPort, map[string]string{
		"TEST_SIDECAR_ID":         "new",
		"TEST_SIDECAR_EVENTS":     events,
		"TEST_SIDECAR_FIXED_PORT": fmt.Sprint(fixedPort),
	})
	if err := sup.activate(newSpec, 5*time.Second); err != nil {
		t.Fatalf("activate fixed-port new: %v", err)
	}
	waitLocalProcDone(t, old)

	got := readEventLines(t, events)
	assertOrderedEvents(t, got, "start old", "stop old", "start new")
	sup.pendingMu.Lock()
	parked := sup.pending[11]
	sup.pendingMu.Unlock()
	if parked != nil {
		t.Fatal("fixed-port upgrade incorrectly parked an old blue-green process")
	}
}

func TestFixedPortFailedUpgradeRestartsAndVerifiesOld(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })
	bin := buildLocalSidecarFixture(t)
	events := filepath.Join(t.TempDir(), "events.log")
	fixedPort := unusedTCPPort(t)

	oldSpec := localSidecarSpec(t, 21, "mqtt", copyFixtureBinary(t, bin, "v1"), fixedPort, map[string]string{
		"TEST_SIDECAR_ID":         "old",
		"TEST_SIDECAR_EVENTS":     events,
		"TEST_SIDECAR_FIXED_PORT": fmt.Sprint(fixedPort),
	})
	if err := sup.activate(oldSpec, 5*time.Second); err != nil {
		t.Fatalf("start old: %v", err)
	}

	badSpec := localSidecarSpec(t, 21, "mqtt", copyFixtureBinary(t, bin, "v2"), fixedPort, map[string]string{
		"TEST_SIDECAR_ID":          "new",
		"TEST_SIDECAR_EVENTS":      events,
		"TEST_SIDECAR_FIXED_PORT":  fmt.Sprint(fixedPort),
		"TEST_SIDECAR_FAIL_HEALTH": "1",
	})
	err := sup.activate(badSpec, 1200*time.Millisecond)
	if err == nil {
		t.Fatal("unhealthy fixed-port upgrade succeeded")
	}
	if !activationRollbackVerified(err) {
		t.Fatalf("rollback was not reported as verified: %v", err)
	}
	current := sup.currentProc(21)
	if current == nil || current.spec.env["TEST_SIDECAR_ID"] != "old" {
		t.Fatalf("old process was not restored: %#v", current)
	}
	if err := sup.waitReady(current.spec, 2*time.Second); err != nil {
		t.Fatalf("restored old process is not ready: %v", err)
	}
	got := readEventLines(t, events)
	assertOrderedEvents(t, got, "start old", "stop old", "start new", "stop new", "start old")
}

func TestFixedPortFailedRollbackIsNotReportedAsRunning(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })
	bin := buildLocalSidecarFixture(t)
	fixedPort := unusedTCPPort(t)

	oldSpec := localSidecarSpec(t, 22, "mqtt", copyFixtureBinary(t, bin, "v1"), fixedPort, map[string]string{
		"TEST_SIDECAR_ID":         "old",
		"TEST_SIDECAR_FIXED_PORT": fmt.Sprint(fixedPort),
	})
	if err := sup.activate(oldSpec, 5*time.Second); err != nil {
		t.Fatalf("start old: %v", err)
	}
	// The running OLD stays healthy, but its captured restart specification is
	// made unhealthy to model a previous binary that can no longer recover.
	sup.currentProc(22).spec.env["TEST_SIDECAR_FAIL_HEALTH"] = "1"

	badSpec := localSidecarSpec(t, 22, "mqtt", copyFixtureBinary(t, bin, "v2"), fixedPort, map[string]string{
		"TEST_SIDECAR_ID":          "new",
		"TEST_SIDECAR_FIXED_PORT":  fmt.Sprint(fixedPort),
		"TEST_SIDECAR_FAIL_HEALTH": "1",
	})
	err := sup.activate(badSpec, 500*time.Millisecond)
	if err == nil {
		t.Fatal("unhealthy upgrade unexpectedly succeeded")
	}
	if activationRollbackVerified(err) {
		t.Fatalf("failed rollback was incorrectly verified: %v", err)
	}
	if sup.PID(22) != 0 {
		t.Fatal("failed rollback left an unhealthy process tracked as running")
	}
}

func TestFixedPortReservationsRejectConflictsAndReleaseOnUninstall(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })
	bin := buildLocalSidecarFixture(t)
	fixedPort := unusedTCPPort(t)

	first := localSidecarSpec(t, 31, "MQTT Broker", copyFixtureBinary(t, bin, "one"), fixedPort, map[string]string{
		"TEST_SIDECAR_ID":         "one",
		"TEST_SIDECAR_FIXED_PORT": fmt.Sprint(fixedPort),
	})
	if err := sup.activate(first, 5*time.Second); err != nil {
		t.Fatalf("start first: %v", err)
	}
	second := localSidecarSpec(t, 32, "Other Broker", copyFixtureBinary(t, bin, "two"), fixedPort, map[string]string{
		"TEST_SIDECAR_ID":         "two",
		"TEST_SIDECAR_FIXED_PORT": fmt.Sprint(fixedPort),
	})
	err := sup.activate(second, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("TCP port %d is already reserved by MQTT Broker installation 31", fixedPort)) {
		t.Fatalf("unexpected conflict error: %v", err)
	}
	if sup.currentProc(32) != nil {
		t.Fatal("conflicting installation was started")
	}

	_ = sup.Stop(31)
	sup.ReleaseFixedPorts(31)
	if err := sup.activate(second, 5*time.Second); err != nil {
		t.Fatalf("port was not reusable after uninstall release: %v", err)
	}
}

func TestWaitFixedTCPPortsIgnoresUDPButChecksTCP(t *testing.T) {
	udpOnly := []fixedRuntimePort{{name: "discovery", protocol: "udp", hostPort: unusedTCPPort(t)}}
	if err := waitFixedTCPPorts("127.0.0.1", udpOnly, 50*time.Millisecond); err != nil {
		t.Fatalf("UDP received a false TCP readiness check: %v", err)
	}
	tcpPort := unusedTCPPort(t)
	err := waitFixedTCPPorts("127.0.0.1", []fixedRuntimePort{{name: "mqtt", protocol: "tcp", hostPort: tcpPort}}, 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(tcpPort)) {
		t.Fatalf("closed TCP listener was not rejected: %v", err)
	}
}

func TestRebuildLocalFixedPortReservationsBeforeResume(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	fixedPort := unusedTCPPort(t)
	manifest := sdk.Manifest{
		Name:    "mqtt",
		Version: "1.0.0",
		Runtime: sdk.Runtime{
			Kind:     "service",
			Binaries: map[string]string{localPlatform(): "https://example.invalid/mqtt"},
			Ports: []sdk.RuntimePort{{
				Name: "mqtt", ContainerPort: fixedPort, HostPort: fixedPort, Protocol: "tcp",
			}},
		},
	}
	firstID := seedRunningInstall(t, s, "mqtt", "", manifest, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET local_bin_path='/tmp/mqtt', local_port=9001 WHERE id=?`, firstID); err != nil {
		t.Fatal(err)
	}

	other := manifest
	other.Name = "other-broker"
	manifestJSON, _ := json.Marshal(other)
	res, err := s.store.db.Exec(`INSERT INTO apps (name, source, manifest_json) VALUES (?, 'registry', ?)`, other.Name, string(manifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(
		`INSERT INTO app_installs (app_id, status, version, manifest_json, source) VALUES (?, 'pending', ?, ?, 'registry')`,
		appID, other.Version, string(manifestJSON),
	)
	if err != nil {
		t.Fatal(err)
	}
	conflictingID, _ := res.LastInsertId()

	s.rebuildLocalFixedPortReservations()
	key := fixedPortKey{protocol: "tcp", port: fixedPort}
	s.localApps.portMu.Lock()
	owner := s.localApps.fixedPorts[key]
	s.localApps.portMu.Unlock()
	if owner.installID != firstID {
		t.Fatalf("reservation owner=%d, want running install %d", owner.installID, firstID)
	}
	var status, errorMessage string
	if err := s.store.db.QueryRow(`SELECT status, error_message FROM app_installs WHERE id=?`, conflictingID).Scan(&status, &errorMessage); err != nil {
		t.Fatal(err)
	}
	if status != "error" || !strings.Contains(errorMessage, fmt.Sprintf("TCP port %d", fixedPort)) {
		t.Fatalf("conflicting pending install status=%q error=%q", status, errorMessage)
	}

	// A second reconstruction models another server restart and must derive
	// the same ownership solely from persisted manifests.
	s.localApps.ResetFixedPortReservations()
	s.rebuildLocalFixedPortReservations()
	s.localApps.portMu.Lock()
	owner = s.localApps.fixedPorts[key]
	s.localApps.portMu.Unlock()
	if owner.installID != firstID {
		t.Fatalf("reconstructed owner=%d, want %d", owner.installID, firstID)
	}
}

func localSidecarSpec(t *testing.T, installID int64, appName, bin string, fixedPort int, env map[string]string) activationSpec {
	t.Helper()
	httpPort := unusedTCPPort(t)
	m := &sdk.Manifest{Name: appName, Runtime: sdk.Runtime{HealthCheck: "/health"}}
	if fixedPort > 0 {
		m.Runtime.Ports = []sdk.RuntimePort{{Name: "mqtt", ContainerPort: fixedPort, HostPort: fixedPort, Protocol: "tcp"}}
	}
	spec, err := newActivationSpec(installID, m, bin, httpPort, env)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func unusedTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func buildLocalSidecarFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := `package main
import (
  "fmt"
  "net"
  "net/http"
  "os"
  "os/signal"
  "syscall"
)
func event(kind string) {
  path := os.Getenv("TEST_SIDECAR_EVENTS")
  if path == "" { return }
  f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
  if err == nil { fmt.Fprintf(f, "%s %s\n", kind, os.Getenv("TEST_SIDECAR_ID")); f.Close() }
}
func main() {
  var fixed net.Listener
  if raw := os.Getenv("TEST_SIDECAR_FIXED_PORT"); raw != "" {
    var err error
    fixed, err = net.Listen("tcp", "127.0.0.1:"+raw)
    if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(23) }
    go func() { for { c, err := fixed.Accept(); if err != nil { return }; c.Close() } }()
  }
  mux := http.NewServeMux()
  mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
    if os.Getenv("TEST_SIDECAR_FAIL_HEALTH") == "1" { http.Error(w, "not ready", 503); return }
    w.WriteHeader(200)
  })
  server := &http.Server{Addr: "127.0.0.1:"+os.Getenv("APTEVA_APP_PORT"), Handler: mux}
  httpListener, err := net.Listen("tcp", server.Addr)
  if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(24) }
  event("start")
  go server.Serve(httpListener)
  signals := make(chan os.Signal, 1)
  signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
  <-signals
  event("stop")
  httpListener.Close()
  if fixed != nil { fixed.Close() }
}
`
	sourcePath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "sidecar")
	cmd := exec.Command("go", "build", "-o", bin, sourcePath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sidecar fixture: %v\n%s", err, output)
	}
	return bin
}

func copyFixtureBinary(t *testing.T, source, version string) string {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), version)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "bin")
	if err := os.WriteFile(target, data, 0755); err != nil {
		t.Fatal(err)
	}
	return target
}

func readEventLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.FieldsFunc(strings.TrimSpace(string(data)), func(r rune) bool { return r == '\n' || r == '\r' })
}

func assertOrderedEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	position := 0
	for _, event := range got {
		if position < len(want) && event == want[position] {
			position++
		}
	}
	if position != len(want) {
		t.Fatalf("events=%v, missing ordered sequence %v", got, want)
	}
}
