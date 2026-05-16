package main

// eval_sandbox.go — generic test environment for evals.
//
// The sandbox is one primitive: every spawned process in an eval
// (apteva-core for the agent, apteva-core for the judge, real
// apteva-server, real app sidecars) talks through a single HTTP
// intercept proxy. The proxy has three modes per host:
//
//   allow   — passes through unchanged. Used for LLM endpoints
//             (opencode.ai, anthropic.com, ...) and 127.0.0.1.
//   mock    — match the request against eval-declared HTTPMocks and
//             return a canned response. Used for third-party
//             integration APIs (Twitter, HubSpot, ...).
//   block   — return 502 + record the call to the trajectory. Used
//             as the default for everything not otherwise classified.
//
// Setting HTTP_PROXY=http://127.0.0.1:<port> on every spawned process
// routes their HTTP traffic through this proxy. Go's net/http honors
// it for free, so apps + apteva-server + apteva-core all get sandboxed
// without code changes anywhere.
//
// This file is package-internal but only intended to be wired up by
// the eval runner — production paths don't touch it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HTTPMock is one declared response in the sandbox's rule table.
// Match precedence: the first mock whose Host + Path + Method match
// the request wins. Path is matched as a prefix (so "/2/tweets"
// matches "/2/tweets" and "/2/tweets/123") to keep matchers terse;
// callers wanting an exact match include a trailing space-or-tab
// (won't match any real URL).
type HTTPMock struct {
	Host       string            `json:"host"`        // e.g. "api.twitter.com"
	Path       string            `json:"path"`        // e.g. "/2/tweets"
	Method     string            `json:"method"`      // GET/POST/...
	Status     int               `json:"status"`      // defaults to 200
	Headers    map[string]string `json:"headers"`     // optional response headers
	Body       json.RawMessage   `json:"body"`        // canned response body
}

// InterceptedCall captures one proxy-handled request for the eval
// trajectory. Apps don't see it; the test does, so the judge can
// grade whether the agent caused an outbound call.
type InterceptedCall struct {
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	Method    string    `json:"method"`
	Mocked    bool      `json:"mocked"`     // true if a mock matched, false on block/allow
	Allowed   bool      `json:"allowed"`    // true if passed through (LLM, loopback)
	Blocked   bool      `json:"blocked"`    // true if no allow/mock matched
	ReqBody   string    `json:"req_body,omitempty"`
	RespBody  string    `json:"resp_body,omitempty"`
	Status    int       `json:"status"`
	Timestamp time.Time `json:"ts"`
}

// SandboxPolicy is the proxy's per-host classification table. Hosts
// are matched by suffix ("opencode.ai" matches "api.opencode.ai" too)
// so callers don't have to enumerate every subdomain.
type SandboxPolicy struct {
	// AllowHostSuffixes — anything matching one of these passes through
	// the proxy unchanged. Use for LLM hosts, package mirrors, and
	// loopback (127.0.0.1) for sibling-sidecar calls.
	AllowHostSuffixes []string
	// Mocks — checked in order; first match wins.
	Mocks []HTTPMock
}

// defaultAllowSuffixes is the conservative default: LLM endpoints we
// actively use + loopback for inter-sidecar HTTP. Any new host the
// eval depends on must be added here OR mocked. Anything else is
// blocked, which is the safe default — better to fail loud than to
// silently hit a real third-party API mid-test.
var defaultAllowSuffixes = []string{
	"127.0.0.1",
	"localhost",
	"opencode.ai",         // the workspace's LLM provider
	"api.anthropic.com",
	"api.openai.com",
}

// sandboxProxy is the live intercept HTTP server. One per eval run.
type sandboxProxy struct {
	listener net.Listener
	server   *http.Server
	policy   SandboxPolicy

	mu    sync.Mutex
	calls []InterceptedCall
}

// startSandboxProxy binds a random loopback port and starts serving.
// Returns the proxy + the address spawned processes should set as
// HTTP_PROXY/HTTPS_PROXY. Caller calls Stop() on cleanup.
func startSandboxProxy(policy SandboxPolicy) (*sandboxProxy, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("listen: %w", err)
	}
	// Combine the caller's allowlist with the conservative defaults.
	// Duplicates are fine — matching is short-circuit suffix.
	policy.AllowHostSuffixes = append(append([]string{}, defaultAllowSuffixes...), policy.AllowHostSuffixes...)

	p := &sandboxProxy{listener: listener, policy: policy}
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handle)
	p.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go p.server.Serve(listener) //nolint:errcheck

	return p, "http://" + listener.Addr().String(), nil
}

func (p *sandboxProxy) Stop() {
	if p.server != nil {
		_ = p.server.Close()
	}
}

// Calls returns a snapshot of every request the proxy has seen so far.
// Safe to call concurrently.
func (p *sandboxProxy) Calls() []InterceptedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]InterceptedCall, len(p.calls))
	copy(out, p.calls)
	return out
}

// handle dispatches each request through the policy table.
//
// Go's HTTP client sends requests to a proxy with the full URL in the
// request line (RFC 7230 §5.3.2 — "absolute-form"). For HTTPS it
// would normally send CONNECT to establish a tunnel; we accept HTTP
// only for now because all of our intercepted endpoints
// (twitter, slack, etc.) are reachable over either HTTPS or HTTP-
// equivalent test fixtures and CONNECT-tunnel rewriting adds real
// complexity we don't need yet. HTTPS hosts in HTTPMocks get matched
// the same way — the request comes through CONNECT, we hijack and
// stub at the application layer.
//
// For v1, we serve HTTP forward-proxy semantics. The downside: an
// app explicitly using HTTPS to an unmocked host will fail to even
// CONNECT. That's actually what we want — we'd rather have the test
// fail loud than the agent quietly hit a real Twitter.
func (p *sandboxProxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	host := r.URL.Hostname()
	if host == "" {
		// Some HTTP/1.1 proxy clients put the host in Host header
		// instead. Fall back to Host so we don't misclassify.
		host = strings.SplitN(r.Host, ":", 2)[0]
	}
	path := r.URL.Path
	method := r.Method

	// Drain body once; we may use it for mock matching and need it for
	// the passthrough request.
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}

	rec := InterceptedCall{
		Host: host, Path: path, Method: method,
		ReqBody: truncate(string(bodyBytes), 1000),
		Timestamp: time.Now(),
	}

	// 1. Allow-list passthrough.
	if hostMatchesSuffix(host, p.policy.AllowHostSuffixes) {
		p.proxyThrough(w, r, bodyBytes, &rec)
		return
	}

	// 2. Mock match.
	for _, m := range p.policy.Mocks {
		if !mockMatches(m, host, path, method) {
			continue
		}
		rec.Mocked = true
		rec.Status = m.Status
		if rec.Status == 0 {
			rec.Status = 200
		}
		for k, v := range m.Headers {
			w.Header().Set(k, v)
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(rec.Status)
		body := m.Body
		if len(body) == 0 {
			body = json.RawMessage(`{}`)
		}
		_, _ = w.Write(body)
		rec.RespBody = truncate(string(body), 1000)
		p.record(rec)
		return
	}

	// 3. Block — neither allowed nor mocked. Return 502 with a body
	// that names the offending host so the test failure is obvious.
	rec.Blocked = true
	rec.Status = http.StatusBadGateway
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.Status)
	msg := fmt.Sprintf(`{"error":"sandbox: blocked outbound call to %q (not in allowlist and no mock declared)"}`, host)
	_, _ = w.Write([]byte(msg))
	rec.RespBody = msg
	p.record(rec)
}

// handleConnect responds to the HTTPS tunnel request from clients.
// We refuse with 502 unless the target host is allowlisted (in which
// case we'd need to actually open a tunnel, which is out of scope
// for v1 — see file header comment). Records the attempt so the
// trajectory shows it.
func (p *sandboxProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := strings.SplitN(r.Host, ":", 2)[0]
	rec := InterceptedCall{
		Host: host, Path: r.URL.Path, Method: "CONNECT",
		Timestamp: time.Now(),
	}
	if hostMatchesSuffix(host, p.policy.AllowHostSuffixes) {
		// Allow CONNECT to LLM hosts — open a tunnel.
		dest, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, "dial: "+err.Error(), http.StatusBadGateway)
			rec.Blocked = true
			rec.Status = http.StatusBadGateway
			p.record(rec)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			dest.Close()
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			dest.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		rec.Allowed = true
		rec.Status = 200
		p.record(rec)
		go func() { _, _ = io.Copy(dest, client); dest.Close() }()
		go func() { _, _ = io.Copy(client, dest); client.Close() }()
		return
	}
	// Block.
	rec.Blocked = true
	rec.Status = http.StatusBadGateway
	http.Error(w, "sandbox: blocked CONNECT to "+host, http.StatusBadGateway)
	p.record(rec)
}

// proxyThrough forwards the request to the real target. Allowlist
// path — we trust the caller's policy. Body is replayed verbatim.
func (p *sandboxProxy) proxyThrough(w http.ResponseWriter, r *http.Request, body []byte, rec *InterceptedCall) {
	rec.Allowed = true
	target := *r.URL
	if target.Scheme == "" {
		target.Scheme = "http"
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = &target
			req.Host = target.Host
			if len(body) > 0 {
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
			}
		},
	}
	rw := &recordingResponseWriter{ResponseWriter: w}
	proxy.ServeHTTP(rw, r)
	rec.Status = rw.status
	rec.RespBody = truncate(string(rw.body), 1000)
	p.record(*rec)
}

func (p *sandboxProxy) record(c InterceptedCall) {
	p.mu.Lock()
	p.calls = append(p.calls, c)
	p.mu.Unlock()
}

// hostMatchesSuffix is the suffix-matcher used by the allowlist.
// "opencode.ai" matches "opencode.ai", "api.opencode.ai", and
// "zen.opencode.ai" — but not "openyourcode.ai" because we anchor
// on a leading dot.
func hostMatchesSuffix(host string, suffixes []string) bool {
	if host == "" {
		return false
	}
	for _, s := range suffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

func mockMatches(m HTTPMock, host, path, method string) bool {
	if m.Method != "" && !strings.EqualFold(m.Method, method) {
		return false
	}
	if m.Host != "" && m.Host != host {
		return false
	}
	if m.Path != "" && !strings.HasPrefix(path, m.Path) {
		return false
	}
	return true
}

// recordingResponseWriter is the tiny shim that lets proxyThrough
// capture the status + body without breaking httputil.ReverseProxy.
type recordingResponseWriter struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (r *recordingResponseWriter) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recordingResponseWriter) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	// Cap the captured copy so a 100MB download doesn't balloon memory.
	if len(r.body) < 4096 {
		need := 4096 - len(r.body)
		if need > len(p) {
			need = len(p)
		}
		r.body = append(r.body, p[:need]...)
	}
	return r.ResponseWriter.Write(p)
}

// httpProxyURL is the value to set as HTTP_PROXY / HTTPS_PROXY on
// any process the eval spawns. Convenience wrapper so callers don't
// have to massage the listener address themselves.
func (p *sandboxProxy) ProxyURL() string {
	return "http://" + p.listener.Addr().String()
}

// ─── Sandboxed app sidecar spawner ─────────────────────────────────

// SandboxApp is one app the eval wants spawned alongside the agent.
// Name is the app's manifest name (e.g. "status"). The runner
// resolves the binary path, allocates a port, and spawns it with a
// tmp data dir + the proxy env vars. The app's MCP endpoint is
// wired into the agent core's mcp_servers config so the agent can
// discover its tools naturally.
type SandboxApp struct {
	Name        string // matches the app's manifest.name
	BinaryPath  string // override; otherwise the runner searches conventional paths
	Migrations  string // override; otherwise the runner derives from BinaryPath
	GatewayURL  string // optional override for APTEVA_GATEWAY_URL
	ExtraEnv    map[string]string
}

// SandboxAppInstance is the live result of spawning one app: enough
// for the eval-runner to add an mcp_servers entry pointing at it and
// to tear it down later.
type SandboxAppInstance struct {
	Name    string
	Port    int
	URL     string // http://127.0.0.1:<port>
	MCPURL  string // http://127.0.0.1:<port>/mcp
	DataDir string
	Cmd     *exec.Cmd
}

// Stop terminates the sandboxed app sidecar. Idempotent.
func (a *SandboxAppInstance) Stop() {
	if a == nil || a.Cmd == nil || a.Cmd.Process == nil {
		return
	}
	_ = a.Cmd.Process.Kill()
	_, _ = a.Cmd.Process.Wait()
}

// SpawnSandboxedApp boots one app sidecar inside the sandbox: tmp
// data dir, HTTP_PROXY pointing at the intercept proxy, gateway URL
// pointing at the test apteva-server. Waits for /health to respond
// before returning so callers can immediately wire the app's MCP
// URL into an agent config.
//
// Returns a *SandboxAppInstance the caller must Stop() on cleanup
// (t.Cleanup wraps this nicely in tests).
func SpawnSandboxedApp(spec SandboxApp, proxyURL, gatewayURL string, healthBudget time.Duration) (*SandboxAppInstance, error) {
	if spec.BinaryPath == "" {
		return nil, fmt.Errorf("SandboxApp.BinaryPath is required (no auto-discovery yet)")
	}
	port, err := allocFreePort()
	if err != nil {
		return nil, fmt.Errorf("alloc port: %w", err)
	}
	dataDir, err := os.MkdirTemp("", "apteva-sandbox-app-"+spec.Name+"-")
	if err != nil {
		return nil, fmt.Errorf("tmp dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, spec.Name+".db")

	effectiveGateway := gatewayURL
	if spec.GatewayURL != "" {
		effectiveGateway = spec.GatewayURL
	}

	env := []string{
		// Standard Go HTTP proxy env vars — every spawned subprocess's
		// net/http calls go through the intercept.
		"HTTP_PROXY=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL,
		"NO_PROXY=", // make sure nothing's pre-set to bypass us
		// App-sdk wiring.
		fmt.Sprintf("APTEVA_APP_PORT=%d", port),
		"APTEVA_DATA_DIR=" + dataDir,
		"DB_PATH=" + dbPath,
		"APTEVA_GATEWAY_URL=" + effectiveGateway,
		// No APTEVA_APP_TOKEN: app-sdk's withTokenAuth treats empty as
		// "dev mode" and serves every request without auth. Fine on
		// loopback in a sandbox.
		// Preserve user's PATH so the sidecar can find tools (ffmpeg
		// etc.) if it needs them.
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	if spec.Migrations != "" {
		env = append(env, "APTEVA_MIGRATIONS_DIR="+spec.Migrations)
	}
	for k, v := range spec.ExtraEnv {
		env = append(env, k+"="+v)
	}

	cmd := exec.Command(spec.BinaryPath)
	cmd.Env = env
	cmd.Dir = dataDir
	// Sidecar logs go through to test stderr so a developer running
	// `go test -v` sees the app's boot output and any errors.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", spec.Name, err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	instance := &SandboxAppInstance{
		Name:    spec.Name,
		Port:    port,
		URL:     url,
		MCPURL:  url + "/mcp",
		DataDir: dataDir,
		Cmd:     cmd,
	}

	// Poll /health until it responds 200 or the budget expires.
	deadline := time.Now().Add(healthBudget)
	client := &http.Client{Timeout: 1 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return instance, nil
			}
		}
		// Also fail fast if the process died.
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return nil, fmt.Errorf("sandbox app %s exited during boot: %v", spec.Name, cmd.ProcessState)
		}
		time.Sleep(100 * time.Millisecond)
	}
	instance.Stop()
	return nil, fmt.Errorf("sandbox app %s never became healthy within %s", spec.Name, healthBudget)
}

// allocFreePort binds 127.0.0.1:0 and returns the kernel-assigned port.
// Same trick AgentManager.allocPort uses; tiny race window between
// close and the sandbox process binding the port, but in practice it
// never collides on the high-port range.
func allocFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

