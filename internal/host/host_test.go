package host

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Real subprocess fixture: exercises private standby/activation, readiness,
// version checking, request forwarding, process exit and crash recovery paths.
func TestHostProcess(t *testing.T) {
	if os.Getenv("APTEVA_HOST_TEST_HELPER") != "1" {
		return
	}
	binary := os.Getenv("APTEVA_TEST_BINARY")
	version := filepath.Base(filepath.Dir(binary))
	for _, arg := range os.Args {
		if arg == "--host-protocol" {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": Protocol, "version": version, "existing_schema": true})
			os.Exit(0)
		}
		if arg == "--host-check-schema" {
			_ = json.NewEncoder(os.Stdout).Encode(DatabaseIdentity{Version: version, SchemaHash: "fake-schema", SecretHash: "fake-key"})
			os.Exit(0)
		}
		if arg == "--preflight" {
			safe := true
			if _, err := os.Stat(binary + ".incompatible"); err == nil {
				safe = false
			}
			p := os.Getenv("APTEVA_UPDATE_PREFLIGHT")
			_ = os.WriteFile(p, []byte("disposable snapshot"), 0600)
			b, _ := json.Marshal(map[string]any{"version": version, "schema_hash": "fake-schema", "rollback_safe": safe})
			_ = os.WriteFile(p+".json", b, 0600)
			os.Exit(0)
		}
	}
	socket := os.Getenv("APTEVA_HOST_SLOT_SOCKET")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(3)
	}
	activate := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte(os.Getenv("APTEVA_HOST_PROXY_SECRET"))))
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": Protocol, "version": version, "pid": os.Getpid(), "proxy_identity": identity})
	})
	mux.HandleFunc("POST /prepare", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /identity", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DatabaseIdentity{Version: version, SchemaHash: "fake-schema", SecretHash: "fake-key"})
	})
	mux.HandleFunc("POST /activate", func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(activate) }); w.WriteHeader(204) })
	mux.HandleFunc("POST /retire", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("POST /freeze", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "{}") })
	mux.HandleFunc("POST /unfreeze", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /runtimes", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "{}") })
	go http.Serve(listener, mux)
	<-activate
	if _, err = os.Stat(binary + ".fail-activation"); err == nil {
		os.Exit(9)
	}
	backend, err := net.Listen("tcp", os.Getenv("APTEVA_HOST_BACKEND_ADDR"))
	if err != nil {
		os.Exit(4)
	}
	requests := http.NewServeMux()
	ready := make(chan struct{})
	requests.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ready:
		default:
			http.Error(w, "startup callbacks pending", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "cli": version})
	})
	requests.HandleFunc("/api/apps/callback/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-app-token" || r.Header.Get("X-Apteva-Host-Token") != os.Getenv("APTEVA_HOST_PROXY_SECRET") {
			http.Error(w, "invalid app callback", http.StatusUnauthorized)
			return
		}
		_ = os.WriteFile(binary+".callback-seen", []byte(strconv.Itoa(os.Getpid())), 0600)
		w.WriteHeader(http.StatusNoContent)
	})
	requests.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			f, e := os.OpenFile(os.Getenv("DB_PATH"), os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			_, _ = f.Write(append(b, '\n'))
			_ = f.Sync()
			f.Close()
		}
		io.WriteString(w, version)
	})
	go http.Serve(backend, requests)
	if _, err := os.Stat(binary + ".startup-callback"); err == nil {
		// Model OnMount -> platform.WhoAmI through the stable public listener,
		// while /health remains unsuccessful until that callback completes.
		client := &http.Client{Timeout: 2 * time.Second}
		request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+os.Getenv("PORT")+"/api/apps/callback/whoami", nil)
		request.Header.Set("Authorization", "Bearer fixture-app-token")
		response, err := client.Do(request)
		if err != nil {
			os.Exit(10)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			os.Exit(11)
		}
		if _, err := os.Stat(binary + ".fail-after-callback"); err == nil {
			os.Exit(12)
		}
		for {
			if _, err := os.Stat(binary + ".hold-readiness"); err != nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	close(ready)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	<-sig
	os.Exit(0)
}

func fixture(t *testing.T, beforeStart ...func(*Host)) (*Host, *http.Client, string, Candidate) {
	t.Helper()
	h, c, url, candidate, _ := fixtureWithStopped(t, beforeStart...)
	return h, c, url, candidate
}
func fixtureWithStopped(t *testing.T, beforeStart ...func(*Host)) (*Host, *http.Client, string, Candidate, <-chan struct{}) {
	t.Helper()
	home, err := os.MkdirTemp(hostTestTempRoot(), "apteva-host-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	db := filepath.Join(home, "apteva.db")
	if err := os.WriteFile(db, []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binaries := map[string]string{}
	for _, v := range []string{"1.0.0", "2.0.0"} {
		p := filepath.Join(home, "versions", v, "apteva-server")
		_ = os.MkdirAll(filepath.Dir(p), 0700)
		// os.Args[0] and the generated temporary path are shell-quoted here; no
		// user-provided data or environment values are evaluated by this fixture.
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
		script := fmt.Sprintf("#!/bin/sh\nAPTEVA_HOST_TEST_HELPER=1 APTEVA_TEST_BINARY=%s exec %s -test.run='^TestHostProcess$' -- \"$@\"\n", quote(p), quote(os.Args[0]))
		if err := os.WriteFile(p, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		binaries[v] = p
	}
	reserve, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := reserve.Addr().String()
	reserve.Close()
	h, err := New(Config{Home: home, DBPath: db, ListenAddr: addr, ServerBinary: binaries["1.0.0"], StartupTimeout: 3 * time.Second, DrainTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, configure := range beforeStart {
		configure(h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() { defer close(stopped); done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("host failed to stop")
		}
	})
	c := UnixClient(SocketPath(home))
	t.Cleanup(c.CloseIdleConnections)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var s State
		if Call(context.Background(), c, "GET", "/status", nil, &s) == nil && s.Version == "1.0.0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h, c, "http://" + addr, Candidate{Binary: binaries["2.0.0"], Version: "2.0.0"}, stopped
}
func waitJob(t *testing.T, c *http.Client) State {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var s State
		if err := Call(context.Background(), c, "GET", "/status", nil, &s); err != nil {
			t.Fatal(err)
		}
		if s.Job != nil && Terminal(s.Job.Phase) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not finish: %+v", s.Job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestDeploymentPreservesConcurrentWrites(t *testing.T) {
	h, c, url, candidate := fixture(t)
	writerDone := make(chan error, 1)
	stopWriter := make(chan struct{})
	written := 0
	versions := map[string]bool{}
	var mu sync.Mutex
	go func() {
		client := &http.Client{Timeout: 8 * time.Second}
		defer client.CloseIdleConnections()
		for i := 0; ; i++ {
			select {
			case <-stopWriter:
				writerDone <- nil
				return
			default:
			}
			resp, err := client.Post(url+"/write", "text/plain", strings.NewReader(strconv.Itoa(i)))
			if err != nil {
				writerDone <- err
				return
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				writerDone <- fmt.Errorf("write status %d: %s", resp.StatusCode, data)
				return
			}
			mu.Lock()
			versions[string(data)] = true
			written++
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "succeeded" || state.Version != "2.0.0" {
		t.Fatalf("deployment: %+v", state)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		seen := versions["2.0.0"]
		mu.Unlock()
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new version did not serve writes")
		}
		time.Sleep(time.Millisecond)
	}
	close(stopWriter)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(h.cfg.DBPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != written+1 || lines[0] != "initial" {
		t.Fatalf("data was lost or duplicated: %d rows", len(lines))
	}
	for i, line := range lines[1:] {
		if line != strconv.Itoa(i) {
			t.Fatalf("row %d=%q", i, line)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !versions["1.0.0"] || !versions["2.0.0"] {
		t.Fatalf("did not exercise both versions: %v", versions)
	}
}
func TestIncompatibleCandidateLeavesActiveServerAndData(t *testing.T) {
	h, c, url, candidate := fixture(t)
	before, _ := os.ReadFile(h.cfg.DBPath)
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "failed" || state.Version != "1.0.0" {
		t.Fatalf("unsafe activation: %+v", state)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "1.0.0" {
		t.Fatal(string(b))
	}
	after, _ := os.ReadFile(h.cfg.DBPath)
	if string(before) != string(after) {
		t.Fatal("preflight modified live data")
	}
}
func TestFailedActivationRollsBackWithoutRestoringSnapshot(t *testing.T) {
	h, c, url, candidate := fixture(t)
	_ = os.WriteFile(candidate.Binary+".fail-activation", nil, 0600)
	resp, err := http.Post(url, "text/plain", strings.NewReader("latest-write"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err = Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "rolled_back" || state.Version != "1.0.0" {
		t.Fatalf("recovery: %+v", state)
	}
	data, _ := os.ReadFile(h.cfg.DBPath)
	if string(data) != "initial\nlatest-write\n" {
		t.Fatalf("rollback lost current writes: %q", data)
	}
}
func TestRouterWaitsForInflightWorkAndDoesNotReplay(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		close(entered)
		<-finish
		io.WriteString(w, "old")
	}))
	defer old.Close()
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "new") }))
	defer fresh.Close()
	router := NewRouter()
	blue, _ := newBackend(old.URL)
	green, _ := newBackend(fresh.URL)
	router.Publish(blue)
	frontend := httptest.NewServer(router)
	defer frontend.Close()
	done := make(chan string, 1)
	go func() {
		resp, err := http.Post(frontend.URL, "text/plain", strings.NewReader("one side effect"))
		if err != nil {
			done <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- string(b)
	}()
	<-entered
	router.Pause()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := router.Drain(ctx, blue); err == nil {
		t.Fatal("active work was not drained")
	}
	queued := make(chan string, 1)
	go func() {
		resp, err := http.Get(frontend.URL)
		if err != nil {
			queued <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		queued <- string(b)
	}()
	close(finish)
	if got := <-done; got != "old" {
		t.Fatal(got)
	}
	router.Publish(green)
	if got := <-queued; got != "new" {
		t.Fatal(got)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("mutating request replayed %d times", calls)
	}
}
func TestCandidateCannotEscapeInstallation(t *testing.T) {
	h, _, _, candidate := fixture(t)
	for _, c := range []Candidate{{Binary: candidate.Binary, Version: "../escape"}, {Binary: "/bin/sh", Version: "2.0.0"}} {
		if err := h.validateCandidate(c); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestHostOwnedSidecarSurvivesSlotChangeAndDeduplicatesStart(t *testing.T) {
	h, c, _, candidate := fixture(t)
	logPath := filepath.Join(h.cfg.Home, "sidecar.log")
	req := ProcessRequest{ID: "sidecar-start-once", Args: []string{"/bin/sh", "-c", "while :; do echo alive; sleep 0.05; done"}, Directory: h.cfg.Home, LogPath: logPath, Env: []string{"PATH=/bin:/usr/bin"}}
	first, err := h.startProcess(req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.startProcess(req)
	if err != nil || again.PID != first.PID {
		t.Fatalf("duplicate spawned another sidecar: %v", err)
	}
	different := req
	different.Args = []string{"/bin/false"}
	if _, err := h.startProcess(different); err == nil {
		t.Fatal("reused start identity with different command")
	}
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, c); s.Job.Phase != "succeeded" {
		t.Fatalf("handoff: %+v", s.Job)
	}
	before, _ := os.ReadFile(logPath)
	deadline := time.Now().Add(2 * time.Second)
	for {
		after, _ := os.ReadFile(logPath)
		if len(after) > len(before) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sidecar logs stopped with retired management slot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	final, err := h.startProcess(req)
	if err != nil || !final.Running || final.PID != first.PID {
		t.Fatalf("sidecar restarted: %+v %v", final, err)
	}
}

func TestHostRejectsUnattestedRecoveryAndRecoversControllerCrash(t *testing.T) {
	h, _, url, _ := fixture(t)
	record := h.Snapshot().ActiveSlot
	invalid := *record
	invalid.PID = os.Getpid()
	if _, err := h.adoptSlot(context.Background(), &invalid); err == nil {
		t.Fatal("adopted unrelated PID")
	}
	invalid = *record
	invalid.ProxyKey = "wrong-slot-key"
	if _, err := h.adoptSlot(context.Background(), &invalid); err == nil {
		t.Fatal("adopted slot with unverified ingress key")
	}
	invalid = *record
	invalid.Socket = filepath.Join(h.cfg.Home, "outside.sock")
	if _, err := h.adoptSlot(context.Background(), &invalid); err == nil {
		t.Fatal("adopted untrusted socket")
	}
	restored, err := h.adoptSlot(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if restored.cmd.Process.Pid != record.PID {
		t.Fatal("attested adoption changed PID")
	}
	if err := h.active.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(7 * time.Second)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	for {
		state := h.Snapshot()
		if state.ActiveSlot != nil && state.ActiveSlot.PID != record.PID {
			resp, err := client.Get(url + "/health")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("controller crash was not recovered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(h.cfg.DBPath)
	if string(data) != "initial\n" {
		t.Fatal("recovery replaced live data")
	}
}

func TestUnsupportedServerProbeUsesOnlyDisposablePaths(t *testing.T) {
	h, _, _, candidate := fixture(t)
	script := `#!/bin/sh
printf '%s\n' "$APTEVA_HOME" "$DB_PATH" "$DATA_DIR" > "$APTEVA_PROBE_OBSERVATION"
echo unaware
exit 1
`
	if err := os.WriteFile(candidate.Binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	observed := filepath.Join(h.cfg.Home, "observed-paths")
	t.Setenv("APTEVA_PROBE_OBSERVATION", observed)
	if err := h.probeProtocol(context.Background(), candidate); err == nil {
		t.Fatal("accepted unsupported server")
	}
	raw, err := os.ReadFile(observed)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Fields(string(raw)) {
		if !strings.HasPrefix(p, filepath.Join(h.cfg.Home, "host", "probe-")) {
			t.Fatalf("probe received live path: %s", p)
		}
	}
	data, _ := os.ReadFile(h.cfg.DBPath)
	if string(data) != "initial\n" {
		t.Fatal("probe touched live data")
	}
}

func hostTestTempRoot() string {
	if runtime.GOOS == "darwin" {
		return "/private/tmp"
	}
	return os.TempDir()
}

func TestCleanHostRestartUsesCommittedVersion(t *testing.T) {
	h, c, url, candidate, stopped := fixtureWithStopped(t)
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	if state := waitJob(t, c); state.Job.Phase != "succeeded" {
		t.Fatalf("deploy: %+v", state.Job)
	}
	h.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := ReadState(h.cfg.Home)
		if err == nil && state.CleanStop {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("clean stop was not journaled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// CleanStop is journaled before Run's final lock-release defer. A supervisor
	// restarts only once the previous host has exited, so wait for that boundary.
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("previous host did not exit")
	}
	// The launch command can still point at an older symlink after a client
	// crash. The committed host journal remains authoritative on restart.
	replacement, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- replacement.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline = time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get(url + "/health")
		if err == nil {
			var health map[string]any
			json.NewDecoder(response.Body).Decode(&health)
			response.Body.Close()
			if health["cli"] == candidate.Version {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("committed version did not resume")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if replacement.Snapshot().CleanStop {
		t.Fatal("running host retained clean-stop marker")
	}
}
