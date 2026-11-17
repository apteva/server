package host

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apteva/server/internal/sshwatch"
)

// testSecurityConfig keeps host tests off the machine: a private collector
// directory, no journalctl, no system log files and no authorized_keys scan.
func testSecurityConfig(home string) *sshwatch.Config {
	return &sshwatch.Config{
		Dir:                  filepath.Join(home, "host", "security"),
		JournalctlPath:       filepath.Join(home, "no-journalctl"),
		LogFiles:             []string{},
		AuthorizedKeysPaths:  []string{},
		MachineIDPath:        filepath.Join(home, "no-machine-id"),
		LockRetryInterval:    50 * time.Millisecond,
		HeartbeatInterval:    50 * time.Millisecond,
		FlushInterval:        10 * time.Millisecond,
		DeliveryInterval:     20 * time.Millisecond,
		PolicyReloadInterval: 50 * time.Millisecond,
		SourceRetryInterval:  50 * time.Millisecond,
		Logf:                 func(string, ...any) {},
	}
}

// rawControlClient talks to the control socket without the automatic token.
func rawControlClient(socket string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}

func controlStatus(t *testing.T, client *http.Client, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://host/status", nil)
	if token != "" {
		req.Header.Set(ControlTokenHeader, token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestControlSocketRequiresToken(t *testing.T) {
	h, c, _, _ := fixture(t)
	raw := rawControlClient(SocketPath(h.cfg.Home))
	defer raw.CloseIdleConnections()
	if code := controlStatus(t, raw, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: HTTP %d", code)
	}
	if code := controlStatus(t, raw, strings.Repeat("0", 64)); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: HTTP %d", code)
	}
	fi, err := os.Stat(ControlTokenPath(h.cfg.Home))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if code := controlStatus(t, raw, h.controlToken); code != http.StatusOK {
		t.Fatalf("valid token: HTTP %d", code)
	}
	// UnixClient attaches the token from control.token next to the socket.
	var state State
	if err := Call(context.Background(), c, "GET", "/status", nil, &state); err != nil || state.PID != os.Getpid() {
		t.Fatalf("status with UnixClient: %+v %v", state, err)
	}
	// ...or from the environment, as server slots receive it.
	t.Setenv(ControlTokenEnv, "not-the-token")
	if err := Call(context.Background(), UnixClient(SocketPath(h.cfg.Home)), "GET", "/status", nil, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("env token must take precedence: %v", err)
	}
	t.Setenv(ControlTokenEnv, h.controlToken)
	if err := Call(context.Background(), UnixClient(SocketPath(h.cfg.Home)), "GET", "/status", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Every route is covered, including mutating ones.
	for _, route := range []string{"POST /deploy", "POST /listeners", "POST /process/start", "GET /process/status", "POST /validate", "GET /security/health", "PUT /security/policy"} {
		method, path, _ := strings.Cut(route, " ")
		req, _ := http.NewRequest(method, "http://host"+path, strings.NewReader("{}"))
		resp, err := raw.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without token: HTTP %d", route, resp.StatusCode)
		}
	}
}

func TestControlSlotReceivesTokenAndProbesDoNot(t *testing.T) {
	h, _, _, _ := fixture(t)
	if !validControlToken(h.controlToken) {
		t.Fatalf("token %q", h.controlToken)
	}
	// The fixture server records the token it was started with.
	b, err := os.ReadFile(h.cfg.ServerBinary + ".control-token")
	if err != nil || string(b) != h.controlToken {
		t.Fatalf("slot token %q (%v), host token %q", b, err, h.controlToken)
	}
	env := overrides([]string{ControlTokenEnv + "=stale", "PATH=/bin"}, map[string]string{ControlTokenEnv: ""})
	for _, v := range env {
		if strings.HasPrefix(v, ControlTokenEnv) {
			t.Fatalf("probe/preflight env keeps the control token: %v", env)
		}
	}
}

func TestPeerCredentialCheck(t *testing.T) {
	if !peerAllowed(501, 501) || !peerAllowed(0, 501) || peerAllowed(502, 501) || peerAllowed(501, 0) {
		t.Fatal("peerAllowed")
	}
	dir, err := os.MkdirTemp(hostTestTempRoot(), "peercred-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	var cred peerCred
	var credErr error
	go func() {
		defer wg.Done()
		c, err := l.Accept()
		if err != nil {
			credErr = err
			return
		}
		defer c.Close()
		cred, credErr = peerCredentials(c.(*net.UnixConn))
	}()
	c, err := net.Dial("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	c.Close()
	if credErr != nil || cred.uid != uint32(os.Geteuid()) {
		t.Fatalf("peer credentials %+v %v", cred, credErr)
	}
	// Requests without kernel credentials in their context fail closed.
	h := &Host{controlToken: strings.Repeat("a", 64)}
	rec := &statusRecorder{header: http.Header{}}
	req, _ := http.NewRequest("GET", "http://host/status", nil)
	req.Header.Set(ControlTokenHeader, h.controlToken)
	h.authorizeControl(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
	if rec.code != http.StatusForbidden {
		t.Fatalf("no peer credentials: HTTP %d", rec.code)
	}
}

type statusRecorder struct {
	header http.Header
	code   int
}

func (r *statusRecorder) Header() http.Header         { return r.header }
func (r *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *statusRecorder) WriteHeader(code int)        { r.code = code }

func TestProcessStartRejectsPrivilegedIdentityAndSecrets(t *testing.T) {
	h, c, _, _ := fixture(t)
	u := func(v uint32) *uint32 { return &v }
	base := ProcessRequest{Args: []string{"/bin/sh", "-c", "exit 0"}, Directory: h.cfg.Home, LogPath: filepath.Join(h.cfg.Home, "p.log"), Env: []string{"PATH=/bin"}}
	cases := []struct {
		name   string
		mutate func(*ProcessRequest)
		want   string
	}{
		{"uid 0", func(r *ProcessRequest) { r.UID = u(0) }, "uid or gid 0"},
		{"gid 0", func(r *ProcessRequest) { r.UID, r.GID = u(DefaultAppUIDBase+1), u(0) }, "uid or gid 0"},
		{"uid below base", func(r *ProcessRequest) { r.UID = u(1000) }, "APTEVA_APP_UID_BASE"},
		{"gid below base", func(r *ProcessRequest) { r.UID, r.GID = u(DefaultAppUIDBase+1), u(27) }, "APTEVA_APP_UID_BASE"},
		{"uid -1 sentinel", func(r *ProcessRequest) { r.UID = u(^uint32(0)) }, "APTEVA_APP_UID_BASE"},
		{"gid without uid", func(r *ProcessRequest) { r.GID = u(DefaultAppUIDBase + 1) }, "gid requires uid"},
		{"server secret", func(r *ProcessRequest) { r.Env = append(r.Env, "SERVER_SECRET=abc") }, "credentials"},
		{"host env", func(r *ProcessRequest) { r.Env = append(r.Env, ControlTokenEnv+"=abc") }, "credentials"},
	}
	for i, tc := range cases {
		req := base
		req.Env = append([]string(nil), base.Env...)
		req.ID = "reject-" + string(rune('a'+i))
		tc.mutate(&req)
		err := Call(context.Background(), c, "POST", "/process/start", req, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	// A valid UID is still refused unless the host is root on Linux.
	if os.Geteuid() != 0 {
		req := base
		req.ID = "valid-uid-non-root"
		req.UID = u(DefaultAppUIDBase + 5)
		if err := Call(context.Background(), c, "POST", "/process/start", req, nil); err == nil || !strings.Contains(err.Error(), "root on Linux") {
			t.Fatalf("non-root host accepted uid: %v", err)
		}
	}
	// Unset UID/GID keeps the previous behaviour.
	req := base
	req.ID = "plain"
	var status ProcessStatus
	if err := Call(context.Background(), c, "POST", "/process/start", req, &status); err != nil || status.PID < 1 {
		t.Fatalf("plain start: %+v %v", status, err)
	}
	t.Setenv("APTEVA_APP_UID_BASE", "300000")
	if AppUIDBase() != 300000 {
		t.Fatal("APTEVA_APP_UID_BASE ignored")
	}
	t.Setenv("APTEVA_APP_UID_BASE", "10")
	if AppUIDBase() != DefaultAppUIDBase {
		t.Fatal("system-account base accepted")
	}
}

func TestHostIdentityAllowsBindChangeOnSamePort(t *testing.T) {
	h, _, _, _ := fixture(t)
	_, port, _ := net.SplitHostPort(h.cfg.ListenAddr)
	cfg := h.cfg
	cfg.ListenAddr = net.JoinHostPort("0.0.0.0", port)
	if _, err := New(cfg); err != nil {
		t.Fatalf("bind change refused: %v", err)
	}
	cfg.ListenAddr = "127.0.0.1:1"
	if _, err := New(cfg); err == nil {
		t.Fatal("port change accepted")
	}
}

// --- security routes -------------------------------------------------------

type lineSource struct{ lines []sshwatch.Line }

func (s *lineSource) Name() string { return "test" }
func (s *lineSource) Run(ctx context.Context, cursor string, sink sshwatch.Sink) error {
	for _, l := range s.lines {
		if l.Cursor == cursor {
			continue
		}
		if err := sink.Line(ctx, l); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestSecurityRoutes(t *testing.T) {
	fp := "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"
	_, c, _, _ := fixture(t, func(h *Host) {
		h.cfg.Security.Source = &lineSource{lines: []sshwatch.Line{
			{Cursor: "1", Ident: "sshd", Time: time.Now(), Message: "Accepted publickey for root from 198.51.100.7 port 4000 ssh2: ED25519 " + fp},
			{Cursor: "2", Ident: "sshd", Time: time.Now(), Message: "Failed password for invalid user admin from 203.0.113.9 port 4001 ssh2"},
		}}
	})
	ctx := context.Background()
	var page sshwatch.EventsPage
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		if page, err = SecurityEvents(ctx, c, 0, 0); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range page.Events {
			if e.Kind == sshwatch.KindSSHAuth {
				n++
			}
		}
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events %+v", page)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var accepted sshwatch.Event
	for _, e := range page.Events {
		if e.Result == sshwatch.ResultAccepted {
			accepted = e
		}
	}
	if accepted.User != "root" || accepted.Fingerprint != fp || len(accepted.Alerts) != 3 {
		t.Fatalf("accepted event %+v", accepted)
	}
	if next, err := SecurityEvents(ctx, c, page.NextAfter, 10); err != nil || len(next.Events) != 0 || next.NextAfter != page.NextAfter {
		t.Fatalf("paging %+v %v", next, err)
	}
	if one, err := SecurityEvents(ctx, c, 0, 1); err != nil || len(one.Events) != 1 {
		t.Fatalf("limit %+v %v", one, err)
	}
	if err := Call(ctx, c, "GET", "/security/events?after=-1", nil, nil); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("negative after: %v", err)
	}
	health, err := SecurityHealth(ctx, c)
	if err != nil || health.Role != sshwatch.RoleCollector || !health.LockHeld || health.Source != "test" || health.Cursor != "2" || health.LastSeq == 0 {
		t.Fatalf("health %+v %v", health, err)
	}
	if _, err := PutSecurityPolicy(ctx, c, sshwatch.Policy{ApprovedSources: []string{"300.0.0.0/8"}}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("invalid policy: %v", err)
	}
	applied, err := PutSecurityPolicy(ctx, c, sshwatch.Policy{ApprovedSources: []string{"198.51.100.0/24"}, ApprovedFingerprints: []string{fp}, RootLoginAllowed: true, AlertWebhookURL: "https://alerts.example/ssh", AlertWebhookSecret: "s"})
	if err != nil || applied.AlertWebhookSecret != "" || !applied.AlertWebhookSecretSet {
		t.Fatalf("put %+v %v", applied, err)
	}
	got, err := GetSecurityPolicy(ctx, c)
	if err != nil || len(got.ApprovedSources) != 1 || got.ApprovedSources[0] != "198.51.100.0/24" || got.AlertWebhookSecret != "" || !got.AlertWebhookSecretSet || !got.RootLoginAllowed {
		t.Fatalf("get %+v %v", got, err)
	}
}

func TestSecurityRoutesWhenDisabled(t *testing.T) {
	_, c, _, _ := fixture(t, func(h *Host) { h.cfg.SecurityCollector = SecurityCollectorOff })
	_, err := SecurityHealth(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("disabled collector: %v", err)
	}
}

func TestProcessStartRunsAsAppUIDWhenRootOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires apteva-host running as root on Linux")
	}
	h, c, _, _ := fixture(t)
	uid := uint32(DefaultAppUIDBase + 7)
	logPath := filepath.Join(h.cfg.Home, "uid.log")
	req := ProcessRequest{ID: "as-app-uid", Args: []string{"/bin/sh", "-c", "echo uid=$(id -u) gid=$(id -g) groups=$(id -G)"}, Directory: "/", LogPath: logPath, Env: []string{"PATH=/usr/bin:/bin"}, UID: &uid}
	if err := Call(context.Background(), c, "POST", "/process/start", req, nil); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("uid=%d gid=%d groups=%d", uid, uid, uid)
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(logPath)
		if strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process identity %q, want %q", b, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
