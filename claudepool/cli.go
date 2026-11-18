package claudepool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CLI runs the claude binary's own commands. It never reads Claude's
// credential store: login state comes only from `claude auth status`.
type CLI struct {
	// Binary is an explicit path; empty means search PATH and the usual
	// install locations.
	Binary string
	// ConfigDir, when set, becomes CLAUDE_CONFIG_DIR so Apteva can use a
	// login separate from the user's own Claude Code.
	ConfigDir string
}

// Resolve returns the claude binary path.
func (c CLI) Resolve() (string, error) {
	if c.Binary != "" {
		if info, err := os.Stat(c.Binary); err == nil && !info.IsDir() {
			return c.Binary, nil
		}
		return "", ErrNotInstalled
	}
	if path, err := exec.LookPath("claude"); err == nil {
		return path, nil
	}
	// Services (launchd, systemd) often run with a short PATH that misses
	// the per-user install locations.
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", "claude"),
			filepath.Join(home, ".claude", "local", "claude"),
			filepath.Join(home, ".npm-global", "bin", "claude"),
			filepath.Join(home, ".bun", "bin", "claude"),
		)
	}
	candidates = append(candidates, "/opt/homebrew/bin/claude", "/usr/local/bin/claude", "/usr/bin/claude")
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", ErrNotInstalled
}

// Env returns the environment for claude processes: the server's own
// environment without anything that could select a different credential
// or billing path, plus extra.
func (c CLI) Env(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		switch {
		case strings.HasPrefix(upper, "ANTHROPIC_"),
			upper == "CLAUDE_CODE_OAUTH_TOKEN",
			upper == "CLAUDE_CODE_USE_BEDROCK",
			upper == "CLAUDE_CODE_USE_VERTEX",
			upper == "CLAUDE_CODE_USE_FOUNDRY",
			upper == "CLAUDECODE",
			upper == "CLAUDE_CODE_ENTRYPOINT",
			upper == "CLAUDE_CONFIG_DIR",
			upper == "MCP_TOOL_TIMEOUT",
			upper == "CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT":
			continue
		}
		env = append(env, kv)
	}
	// Services often run with a PATH that lacks the binary's directory;
	// Claude Code itself is fine without it, but keep a sane default.
	if os.Getenv("PATH") == "" {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	if c.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+c.ConfigDir)
	}
	return append(env, extra...)
}

// Version returns `claude --version`, e.g. "2.1.281 (Claude Code)".
func (c CLI) Version(ctx context.Context) (string, error) {
	bin, err := c.Resolve()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = c.Env()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// AuthState is what `claude auth status --json` reports. It carries no
// credential material.
type AuthState struct {
	LoggedIn         bool      `json:"logged_in"`
	AuthMethod       string    `json:"auth_method,omitempty"`
	SubscriptionType string    `json:"subscription_type,omitempty"`
	APIProvider      string    `json:"api_provider,omitempty"`
	CheckedAt        time.Time `json:"checked_at"`
}

// Subscription reports whether the login draws on a Claude subscription
// rather than an API key or a cloud provider.
func (a AuthState) Subscription() bool {
	return a.LoggedIn && a.AuthMethod == "claude.ai"
}

// AuthStatus runs `claude auth status --json`.
func (c CLI) AuthStatus(ctx context.Context) (AuthState, error) {
	bin, err := c.Resolve()
	if err != nil {
		return AuthState{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "status", "--json")
	cmd.Env = c.Env()
	// A logged-out status exits non-zero but still prints JSON.
	out, runErr := cmd.Output()
	var raw struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		SubscriptionType string `json:"subscriptionType"`
		APIProvider      string `json:"apiProvider"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		if runErr != nil {
			return AuthState{}, runErr
		}
		return AuthState{}, err
	}
	return AuthState{
		LoggedIn:         raw.LoggedIn,
		AuthMethod:       raw.AuthMethod,
		SubscriptionType: raw.SubscriptionType,
		APIProvider:      raw.APIProvider,
		CheckedAt:        time.Now(),
	}, nil
}

// Logout runs `claude auth logout`.
func (c CLI) Logout(ctx context.Context) error {
	bin, err := c.Resolve()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "logout")
	cmd.Env = c.Env()
	return cmd.Run()
}

// LoginRunner starts Claude Code's own sign-in command. Sign-in completes
// in the browser on Anthropic's page and Claude Code stores the result
// itself; the runner only tracks whether the command is still running.
type LoginRunner struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	lastErr   string
}

// LoginState reports the sign-in command's progress.
type LoginState struct {
	Running   bool       `json:"running"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// State returns the current sign-in progress.
func (l *LoginRunner) State() LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := LoginState{Running: l.running, LastError: l.lastErr}
	if !l.startedAt.IsZero() {
		started := l.startedAt
		state.StartedAt = &started
	}
	return state
}

// ErrLoginRunning means a sign-in is already in progress.
var ErrLoginRunning = errors.New("a sign-in is already in progress")

// Start runs `claude auth login --claudeai`, which opens Anthropic's
// sign-in page in the browser. The command's output is discarded: Apteva
// must not sit in the middle of the sign-in.
func (l *LoginRunner) Start(c CLI, timeout time.Duration) error {
	bin, err := c.Resolve()
	if err != nil {
		return err
	}
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return ErrLoginRunning
	}
	l.running = true
	l.startedAt = time.Now()
	l.lastErr = ""
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, bin, "auth", "login", "--claudeai")
	cmd.Env = c.Env()
	if err := cmd.Start(); err != nil {
		cancel()
		l.finish(err)
		return err
	}
	go func() {
		defer cancel()
		err := cmd.Wait()
		if ctx.Err() == context.DeadlineExceeded {
			err = errors.New("sign-in was not completed in time")
		}
		l.finish(err)
	}()
	return nil
}

func (l *LoginRunner) finish(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = false
	if err != nil {
		l.lastErr = err.Error()
	}
}
