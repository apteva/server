package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// HTTPMock is one declared response in an environment edge rule table.
type HTTPMock struct {
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Method  string            `json:"method"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// InterceptedCall captures one request handled by an environment edge.
type InterceptedCall struct {
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	Method    string    `json:"method"`
	Mocked    bool      `json:"mocked"`
	Allowed   bool      `json:"allowed"`
	Blocked   bool      `json:"blocked"`
	Recorded  bool      `json:"recorded"`
	ReqBody   string    `json:"req_body,omitempty"`
	RespBody  string    `json:"resp_body,omitempty"`
	Status    int       `json:"status"`
	Timestamp time.Time `json:"ts"`
}

// SandboxPolicy classifies outbound hosts and supplies deterministic mocks.
type SandboxPolicy struct {
	AllowHostSuffixes []string
	Mocks             []HTTPMock
}

var defaultAllowSuffixes = []string{
	"127.0.0.1",
	"localhost",
	"opencode.ai",
	"api.anthropic.com",
	"api.openai.com",
	"chatgpt.com",
}

// llmGatewayHost extracts the hostname from an operator-supplied
// OPENAI_BASE_URL value. Returns "" for blank or unparseable input so
// callers can append the result unconditionally after checking.
func llmGatewayHost(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// llmAllowSuffixes returns defaultAllowSuffixes plus the host of an
// OPENAI_BASE_URL configured on the server process itself (container
// env). Without this an operator who points OpenAI at a compatible
// gateway gets their inference blocked by the edge even though the
// provider is configured correctly — the allowlist would still only
// name api.openai.com.
//
// This covers process-level configuration only. A gateway configured
// per-connection (runtime_config.base_url) is admitted at agent attach
// time instead — see EnvironmentEdge.AllowHost and its call in
// SpawnAgentInEnvironment — because the edge cannot see any user's
// connection rows at edge start.
func llmAllowSuffixes() []string {
	suffixes := append([]string{}, defaultAllowSuffixes...)
	if host := llmGatewayHost(os.Getenv("OPENAI_BASE_URL")); host != "" {
		suffixes = append(suffixes, host)
	}
	return suffixes
}

func hostMatchesSuffix(host string, suffixes []string) bool {
	if host == "" {
		return false
	}
	for _, suffix := range suffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func mockMatches(mock HTTPMock, host, path, method string) bool {
	if mock.Method != "" && !strings.EqualFold(mock.Method, method) {
		return false
	}
	if mock.Host != "" && mock.Host != host {
		return false
	}
	if mock.Path != "" && !strings.HasPrefix(path, mock.Path) {
		return false
	}
	return true
}

// SandboxApp describes an app sidecar started inside an isolated environment.
type SandboxApp struct {
	Name          string
	BinaryPath    string
	Migrations    string
	GatewayURL    string
	ExtraEnv      map[string]string
	DataDir       string
	EnvironmentID string
}

// SandboxAppInstance is a live isolated app sidecar.
type SandboxAppInstance struct {
	Name    string
	Port    int
	URL     string
	MCPURL  string
	DataDir string
	Cmd     *exec.Cmd
}

func (a *SandboxAppInstance) Stop() {
	if a == nil || a.Cmd == nil || a.Cmd.Process == nil {
		return
	}
	_ = a.Cmd.Process.Kill()
	_, _ = a.Cmd.Process.Wait()
}

// SpawnSandboxedApp starts an app sidecar with isolated storage and edge proxy settings.
func SpawnSandboxedApp(spec SandboxApp, proxyURL, gatewayURL string, healthBudget time.Duration) (*SandboxAppInstance, error) {
	if spec.BinaryPath == "" {
		return nil, fmt.Errorf("SandboxApp.BinaryPath is required")
	}
	port, err := allocFreePort()
	if err != nil {
		return nil, fmt.Errorf("alloc port: %w", err)
	}
	dataDir := spec.DataDir
	if dataDir == "" {
		dataDir, err = os.MkdirTemp("", "apteva-environment-app-"+spec.Name+"-")
		if err != nil {
			return nil, fmt.Errorf("tmp dir: %w", err)
		}
	} else if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, spec.Name+".db")

	effectiveGateway := gatewayURL
	if spec.GatewayURL != "" {
		effectiveGateway = spec.GatewayURL
	}
	env := []string{
		"HTTP_PROXY=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL,
		"NO_PROXY=",
		fmt.Sprintf("APTEVA_APP_PORT=%d", port),
		"APTEVA_DATA_DIR=" + dataDir,
		"DB_PATH=" + dbPath,
		"APTEVA_GATEWAY_URL=" + effectiveGateway,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	if spec.Migrations != "" {
		env = append(env, "APTEVA_MIGRATIONS_DIR="+spec.Migrations)
	}
	if spec.EnvironmentID != "" {
		env = append(env, "APTEVA_ENVIRONMENT_ID="+spec.EnvironmentID)
	}
	for key, value := range spec.ExtraEnv {
		env = append(env, key+"="+value)
	}

	cmd := exec.Command(spec.BinaryPath)
	cmd.Env = env
	cmd.Dir = dataDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", spec.Name, err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	instance := &SandboxAppInstance{
		Name: spec.Name, Port: port, URL: url, MCPURL: url + "/mcp", DataDir: dataDir, Cmd: cmd,
	}
	deadline := time.Now().Add(healthBudget)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return instance, nil
			}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return nil, fmt.Errorf("environment app %s exited during boot: %v", spec.Name, cmd.ProcessState)
		}
		time.Sleep(100 * time.Millisecond)
	}
	instance.Stop()
	return nil, fmt.Errorf("environment app %s never became healthy within %s", spec.Name, healthBudget)
}

func allocFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
