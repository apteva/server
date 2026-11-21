package main

// Claude subscription provider: agent turns run through the owner's own,
// signed-in Claude Code CLI on this machine, so they draw on the owner's
// Claude plan instead of per-token API billing.
//
// Policy boundaries this file keeps:
//   - Apteva never reads, stores or refreshes Claude credentials. Sign-in
//     runs in Claude Code's own command and on Anthropic's page: the server
//     reads only the sign-in link from the command, passes on the one-time
//     code the user pastes, and asks `claude auth status` whether it worked.
//     Claude Code exchanges the code and stores the login itself.
//   - The provider is offered only on installs with a single active user,
//     so one person's plan never serves other people's agents.
//
// Core sees an ordinary OpenAI-style streaming endpoint; the server keeps a
// small pool of claude processes (see the claudepool package) shared by
// every agent thread.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apteva/server/claudepool"
)

const (
	claudeSubscriptionProviderKey = "claude-subscription"
	claudeSubscriptionSlug        = "claude-subscription"
	// connectionAuthTypeLocalCLI marks providers that use a CLI signed in
	// on the server machine; their connections hold no credentials.
	connectionAuthTypeLocalCLI = "local_cli"
)

var claudeSubscriptionDefaultModels = map[string]string{
	"large":  "opus",
	"medium": "sonnet",
	"small":  "haiku",
}

// claudeSubscriptionModels are the Claude Code model aliases, which always
// point at the newest model of each family the plan includes. They are
// what the tiers default to, and the list when Claude Code cannot be asked.
func claudeSubscriptionModels() []ModelInfo {
	text := []string{"text", "image"}
	return []ModelInfo{
		{ID: "opus", Name: "Claude Opus (latest)", Description: "Most capable model on your plan", ContextSize: 200000, Capabilities: ProviderModelCapabilities{ContextWindow: 200000, InputModalities: text}},
		{ID: "sonnet", Name: "Claude Sonnet (latest)", Description: "Fast and capable", ContextSize: 200000, Capabilities: ProviderModelCapabilities{ContextWindow: 200000, InputModalities: text}},
		{ID: "haiku", Name: "Claude Haiku (latest)", Description: "Fastest, uses the least of your plan", ContextSize: 200000, Capabilities: ProviderModelCapabilities{ContextWindow: 200000, InputModalities: text}},
	}
}

// claudeSubscriptionModelList is what Claude Code offers this login (its
// own model picker), plus any default alias it lists only as a variant.
func (s *Server) claudeSubscriptionModelList(ctx context.Context, refresh bool) []ModelInfo {
	aliases := claudeSubscriptionModels()
	pool, err := s.claudePool()
	if err != nil {
		return aliases
	}
	maxAge := 10 * time.Minute
	if refresh {
		maxAge = 0
	}
	options, err := pool.Models(ctx, maxAge)
	if err != nil {
		log.Printf("[CLAUDE-SUB] model list unavailable: %v", err)
		return aliases
	}
	var listed []ModelInfo
	seen := map[string]bool{}
	resolved := map[string]string{} // value → model it points at
	for _, option := range options {
		// "default" follows whatever Claude Code picks; an empty tier
		// already means that.
		if option.Value == "default" || seen[option.Value] {
			continue
		}
		seen[option.Value] = true
		resolved[option.Value] = option.ResolvedModel
		window := 200000
		if strings.HasSuffix(option.Value, "[1m]") || strings.HasSuffix(option.ResolvedModel, "[1m]") {
			window = 1000000
		}
		// Name the model the option points at today ("Sonnet 5"): the
		// display name alone ("Sonnet") does not say which version.
		name := option.DisplayName
		if product := claudeModelName(option.ResolvedModel); product != "" {
			name = product
			if window == 1000000 {
				name += " (1M context)"
			}
		}
		listed = append(listed, ModelInfo{
			ID: option.Value, Name: name, Description: option.Description, ContextSize: window,
			Capabilities: ProviderModelCapabilities{ContextWindow: window, InputModalities: []string{"text", "image"}},
		})
	}
	var models []ModelInfo
	for _, alias := range aliases {
		if seen[alias.ID] {
			continue
		}
		// An alias listed only as its 1M-context variant points at the
		// same model with the standard context.
		if product := claudeModelName(resolved[alias.ID+"[1m]"]); product != "" {
			alias.Name = product
		}
		models = append(models, alias)
	}
	return append(models, listed...)
}

var claudeModelIDPattern = regexp.MustCompile(`^claude-([a-z]+)((?:-[0-9]+)+)$`)

// claudeModelName turns a Claude model id into its product name:
// claude-sonnet-5 is "Sonnet 5", claude-haiku-4-5-20251001 is "Haiku 4.5".
// It returns "" for anything else.
func claudeModelName(id string) string {
	match := claudeModelIDPattern.FindStringSubmatch(strings.TrimSuffix(id, "[1m]"))
	if match == nil {
		return ""
	}
	version := strings.Split(strings.TrimPrefix(match[2], "-"), "-")
	if n := len(version); n > 1 && len(version[n-1]) == 8 {
		version = version[:n-1] // a snapshot date, not part of the version
	}
	if len(version) > 2 {
		return ""
	}
	return strings.ToUpper(match[1][:1]) + match[1][1:] + " " + strings.Join(version, ".")
}

type claudeSubscriptionService struct {
	once    sync.Once
	pool    *claudepool.Pool
	initErr error
	ready   atomic.Pointer[claudepool.Pool]
	login   claudepool.LoginRunner
}

// claudeSubscriptionCLI is set by the operator through the environment,
// never through the API: the server runs whatever binary this names.
func claudeSubscriptionCLI() claudepool.CLI {
	return claudepool.CLI{
		Binary:    strings.TrimSpace(os.Getenv("APTEVA_CLAUDE_BIN")),
		ConfigDir: strings.TrimSpace(os.Getenv("APTEVA_CLAUDE_CONFIG_DIR")),
	}
}

func (s *Server) claudePool() (*claudepool.Pool, error) {
	svc := &s.claudeSub
	svc.once.Do(func() {
		base := filepath.Join(s.dataDir, "claude-subscription")
		if s.dataDir == "" {
			base = filepath.Join(os.TempDir(), "apteva-claude-subscription")
		}
		svc.pool, svc.initErr = claudepool.New(claudepool.Config{
			CLI:      claudeSubscriptionCLI(),
			WorkDir:  filepath.Join(base, "work"),
			StateDir: filepath.Join(base, "sessions"),
			MCPURL:   "http://127.0.0.1:" + s.port + "/api/llm/claude-subscription/mcp",
			// Core allows connector calls up to 10 minutes.
			ToolTimeout: 10*time.Minute + 30*time.Second,
			// A successful done ends the thread: core sends no result.
			TerminalTools: []string{"done"},
			Limits:        s.claudeSubscriptionLimits(),
			Logf: func(format string, args ...any) {
				log.Printf(format, args...)
			},
		})
		if svc.initErr == nil {
			svc.ready.Store(svc.pool)
		}
	})
	return svc.pool, svc.initErr
}

// claudeSubscriptionSettings are the pool settings kept on the provider
// connection's runtime config.
type claudeSubscriptionSettings struct {
	PoolSize     int `json:"pool_size"`
	IdleSeconds  int `json:"idle_seconds"`
	QueueSeconds int `json:"queue_seconds"`
}

func defaultClaudeSubscriptionSettings() claudeSubscriptionSettings {
	return claudeSubscriptionSettings{PoolSize: 4, IdleSeconds: 90, QueueSeconds: 60}
}

func (c claudeSubscriptionSettings) validate() error {
	switch {
	case c.PoolSize < 1 || c.PoolSize > 16:
		return errors.New("pool_size must be between 1 and 16")
	case c.IdleSeconds < 10 || c.IdleSeconds > 3600:
		return errors.New("idle_seconds must be between 10 and 3600")
	case c.QueueSeconds < 5 || c.QueueSeconds > 150:
		// Core gives up on response headers after 180 seconds.
		return errors.New("queue_seconds must be between 5 and 150")
	}
	return nil
}

func (c claudeSubscriptionSettings) limits() claudepool.Limits {
	return claudepool.Limits{
		MaxProcs:     c.PoolSize,
		IdleTimeout:  time.Duration(c.IdleSeconds) * time.Second,
		QueueTimeout: time.Duration(c.QueueSeconds) * time.Second,
	}
}

// claudeSubscriptionConnection returns the oldest active Claude
// subscription connection, which holds the pool settings.
func (s *Server) claudeSubscriptionConnection() (int64, claudeSubscriptionSettings, bool) {
	settings := defaultClaudeSubscriptionSettings()
	var id int64
	var raw string
	err := s.store.dbQueryRow(`SELECT id, COALESCE(runtime_config,'{}') FROM connections
		WHERE app_slug=? AND status='active' ORDER BY id LIMIT 1`, claudeSubscriptionSlug).Scan(&id, &raw)
	if err != nil {
		return 0, settings, false
	}
	var stored map[string]any
	if json.Unmarshal([]byte(raw), &stored) == nil {
		for key, target := range map[string]*int{"pool_size": &settings.PoolSize, "idle_seconds": &settings.IdleSeconds, "queue_seconds": &settings.QueueSeconds} {
			if value, ok := stored[key].(float64); ok && value > 0 {
				*target = int(value)
			}
		}
	}
	if settings.validate() != nil {
		settings = defaultClaudeSubscriptionSettings()
	}
	return id, settings, true
}

func (s *Server) claudeSubscriptionLimits() claudepool.Limits {
	_, settings, _ := s.claudeSubscriptionConnection()
	return settings.limits()
}

// claudeSubscriptionAllowed reports whether this install may offer the
// provider: Claude plans are personal, so only single-user installs can.
func (s *Server) claudeSubscriptionAllowed() bool {
	var count int
	if err := s.store.dbQueryRow(`SELECT COUNT(*) FROM users WHERE disabled_at IS NULL`).Scan(&count); err != nil {
		return false
	}
	return count <= 1
}

// connectionUsesLocalCLI reports whether an app signs in through a CLI on
// the server machine rather than through stored credentials.
func connectionUsesLocalCLI(app *AppTemplate) bool {
	return app != nil && containsString(app.Auth.Types, connectionAuthTypeLocalCLI)
}

func (s *Server) claudeSubscriptionAgentStopped(agentID int64) {
	// Only a pool that is already running has anything to close.
	if pool := s.claudeSub.ready.Load(); pool != nil {
		pool.CloseOwner(claudeSubscriptionOwner(agentID))
	}
}

func claudeSubscriptionOwner(agentID int64) string {
	return "agent:" + strconv.FormatInt(agentID, 10)
}

// --- Chat completions ---------------------------------------------------

type claudeChatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
}

var claudeModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-\[\]]{0,99}$`)

// claudeContentText flattens OpenAI message content to text. Images are not
// passed to Claude yet.
func claudeContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text":
			out = append(out, part.Text)
		case "image_url", "input_image":
			out = append(out, "[An image was attached here, but this provider cannot show images to the model.]")
		case "input_audio", "audio_url":
			out = append(out, "[An audio clip was attached here, but this provider cannot pass audio to the model.]")
		}
	}
	return strings.Join(out, "\n")
}

func (req claudeChatRequest) toPool(owner, label string) (claudepool.Request, error) {
	out := claudepool.Request{Owner: owner, OwnerLabel: label, Model: req.Model}
	var system []string
	leading := true
	for _, msg := range req.Messages {
		text := claudeContentText(msg.Content)
		switch msg.Role {
		case "system", "developer":
			if leading {
				system = append(system, text)
				continue
			}
			out.Messages = append(out.Messages, claudepool.Message{Role: "user", Text: "[system]\n" + text})
		case "user":
			out.Messages = append(out.Messages, claudepool.Message{Role: "user", Text: text})
		case "assistant":
			m := claudepool.Message{Role: "assistant", Text: text}
			for _, call := range msg.ToolCalls {
				m.ToolCalls = append(m.ToolCalls, claudepool.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
			}
			out.Messages = append(out.Messages, m)
		case "tool":
			out.Messages = append(out.Messages, claudepool.Message{Role: "tool", ToolCallID: msg.ToolCallID, Text: text})
		default:
			return out, fmt.Errorf("unsupported message role %q", msg.Role)
		}
		leading = false
	}
	out.System = strings.Join(system, "\n\n")
	for _, tool := range req.Tools {
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		out.Tools = append(out.Tools, claudepool.Tool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters})
	}
	if len(out.Messages) == 0 {
		return out, errors.New("no messages")
	}
	return out, nil
}

func claudeErrorBody(kind, message string) map[string]any {
	return map[string]any{"error": map[string]any{"type": kind, "message": message}}
}

// claudeErrorStatus maps a pool error to an HTTP status and error type.
func claudeErrorStatus(err error) (int, string, time.Duration) {
	var retry *claudepool.RetryError
	after := time.Duration(0)
	if errors.As(err, &retry) {
		after = retry.RetryAfter
	}
	switch {
	case errors.Is(err, claudepool.ErrNotSignedIn):
		return http.StatusServiceUnavailable, "not_signed_in", after
	case errors.Is(err, claudepool.ErrNotInstalled):
		return http.StatusServiceUnavailable, "not_installed", 0
	case errors.Is(err, claudepool.ErrLimited):
		return http.StatusTooManyRequests, "usage_limit", after
	case errors.Is(err, claudepool.ErrPoolBusy):
		return http.StatusServiceUnavailable, "pool_busy", after
	case errors.Is(err, context.Canceled):
		return 499, "cancelled", 0
	default:
		return http.StatusBadGateway, "claude_error", 0
	}
}

// claudeStream writes OpenAI-style chat completion chunks. Headers go out
// with the first chunk, so errors before any output keep a real status;
// a request still queued after headerDelay commits to 200 and keeps the
// connection alive with SSE comments.
type claudeStream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
	failed  error
	last    time.Time
	id      string
	model   string
	created int64
}

func (cs *claudeStream) startLocked() {
	if cs.started {
		return
	}
	h := cs.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	cs.w.WriteHeader(http.StatusOK)
	cs.started = true
}

func (cs *claudeStream) writeLocked(data string) error {
	if cs.failed != nil {
		return cs.failed
	}
	if _, err := io.WriteString(cs.w, data); err != nil {
		cs.failed = err
		return err
	}
	if cs.flusher != nil {
		cs.flusher.Flush()
	}
	cs.last = time.Now()
	return nil
}

func (cs *claudeStream) chunk(delta map[string]any, finish string, usage map[string]any) error {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	payload := map[string]any{
		"id": cs.id, "object": "chat.completion.chunk", "created": cs.created, "model": cs.model,
		"choices": []any{choice},
	}
	if usage != nil {
		payload["choices"] = []any{}
		payload["usage"] = usage
	}
	raw, _ := json.Marshal(payload)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.startLocked()
	return cs.writeLocked("data: " + string(raw) + "\n\n")
}

func (cs *claudeStream) keepAlive(stop <-chan struct{}, headerDelay time.Duration) {
	began := time.Now()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		cs.mu.Lock()
		if !cs.started && time.Since(began) >= headerDelay {
			cs.startLocked()
			_ = cs.writeLocked(": waiting for a Claude process\n\n")
		} else if cs.started && time.Since(cs.last) >= 15*time.Second {
			_ = cs.writeLocked(": keep-alive\n\n")
		}
		cs.mu.Unlock()
	}
}

func usagePayload(u claudepool.Usage) map[string]any {
	prompt := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      prompt + u.OutputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      u.CacheReadTokens,
			"cache_write_tokens": u.CacheWriteTokens,
		},
	}
}

// claudeSubscriptionAgent resolves the agent behind a core key. Core starts
// thinking before its key reaches the database, so runningCoreAgent checks
// live processes before the stored row.
func (s *Server) claudeSubscriptionAgent(ctx context.Context, token string) (*Agent, bool) {
	agent, err := s.runningCoreAgent(ctx, token)
	if err != nil || agent == nil {
		return nil, false
	}
	return agent, true
}

// handleClaudeSubscriptionChat serves core's model calls. Like the managed
// gateway it authenticates with the agent's own core_ key.
func (s *Server) handleClaudeSubscriptionChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !strings.HasPrefix(token, "core_") {
		writeJSONStatus(w, http.StatusUnauthorized, claudeErrorBody("invalid_agent_credential", "invalid agent credential"))
		return
	}
	agent, ok := s.claudeSubscriptionAgent(r.Context(), token)
	if !ok {
		writeJSONStatus(w, http.StatusUnauthorized, claudeErrorBody("invalid_agent_credential", "invalid agent credential"))
		return
	}
	if !s.claudeSubscriptionAllowed() {
		writeJSONStatus(w, http.StatusForbidden, claudeErrorBody("single_user_only", "The Claude subscription provider is only available on single-user installs."))
		return
	}
	if !s.claudeSubscriptionConnectedFor(agent.UserID, agent.ProjectID) {
		writeJSONStatus(w, http.StatusServiceUnavailable, claudeErrorBody("not_connected", "No Claude subscription provider is connected."))
		return
	}
	var body claudeChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, claudeErrorBody("invalid_request", "invalid JSON body"))
		return
	}
	if body.Model != "" && !claudeModelPattern.MatchString(body.Model) {
		writeJSONStatus(w, http.StatusBadRequest, claudeErrorBody("invalid_request", "invalid model name"))
		return
	}
	var label string
	_ = s.store.dbQueryRow(`SELECT COALESCE(name,'') FROM agents WHERE id=?`, agent.ID).Scan(&label)
	req, err := body.toPool(claudeSubscriptionOwner(agent.ID), label)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, claudeErrorBody("invalid_request", err.Error()))
		return
	}
	pool, err := s.claudePool()
	if err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, claudeErrorBody("unavailable", err.Error()))
		return
	}
	// Model calls can outlast any default server deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	model := body.Model
	if model == "" {
		model = "default"
	}
	created := time.Now()
	id := "chatcmpl-claude-" + strconv.FormatInt(created.UnixNano(), 36)

	if !body.Stream {
		s.claudeSubscriptionComplete(w, r, pool, req, id, model, created)
		return
	}

	flusher, _ := w.(http.Flusher)
	cs := &claudeStream{w: w, flusher: flusher, id: id, model: model, created: created.Unix()}
	stop := make(chan struct{})
	var keepAliveDone sync.WaitGroup
	keepAliveDone.Add(1)
	go func() {
		defer keepAliveDone.Done()
		cs.keepAlive(stop, 20*time.Second)
	}()
	defer func() {
		close(stop)
		keepAliveDone.Wait()
	}()

	sentRole := false
	err = pool.Run(r.Context(), req, func(ev claudepool.Event) error {
		delta := map[string]any{}
		if !sentRole {
			delta["role"] = "assistant"
			sentRole = true
		}
		switch ev.Kind {
		case claudepool.EventText:
			delta["content"] = ev.Text
		case claudepool.EventThinking:
			delta["reasoning_content"] = ev.Text
		case claudepool.EventToolStart:
			delta["tool_calls"] = []any{map[string]any{
				"index": ev.ToolIndex, "id": ev.ToolID, "type": "function",
				"function": map[string]any{"name": ev.ToolName, "arguments": ""},
			}}
		case claudepool.EventToolArgs:
			delta["tool_calls"] = []any{map[string]any{
				"index": ev.ToolIndex, "function": map[string]any{"arguments": ev.Text},
			}}
		case claudepool.EventDone:
			if err := cs.chunk(delta, ev.FinishReason, nil); err != nil {
				return err
			}
			if err := cs.chunk(nil, "", usagePayload(ev.Usage)); err != nil {
				return err
			}
			cs.mu.Lock()
			defer cs.mu.Unlock()
			return cs.writeLocked("data: [DONE]\n\n")
		}
		return cs.chunk(delta, "", nil)
	})
	if err == nil {
		return
	}
	status, kind, after := claudeErrorStatus(err)
	if status != 499 {
		log.Printf("[CLAUDE-SUB] agent=%d turn failed: %v", agent.ID, err)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.started {
		raw, _ := json.Marshal(claudeErrorBody(kind, err.Error()))
		_ = cs.writeLocked("data: " + string(raw) + "\n\n")
		return
	}
	if status == 499 {
		return
	}
	if after > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(after.Round(time.Second)/time.Second)+1))
	}
	writeJSONStatus(w, status, claudeErrorBody(kind, err.Error()))
	cs.started = true
}

func (s *Server) claudeSubscriptionComplete(w http.ResponseWriter, r *http.Request, pool *claudepool.Pool, req claudepool.Request, id, model string, created time.Time) {
	var text, reasoning strings.Builder
	var calls []map[string]any
	var args []strings.Builder
	var finish string
	var usage claudepool.Usage
	err := pool.Run(r.Context(), req, func(ev claudepool.Event) error {
		switch ev.Kind {
		case claudepool.EventText:
			text.WriteString(ev.Text)
		case claudepool.EventThinking:
			reasoning.WriteString(ev.Text)
		case claudepool.EventToolStart:
			calls = append(calls, map[string]any{"id": ev.ToolID, "type": "function", "function": map[string]any{"name": ev.ToolName}})
			args = append(args, strings.Builder{})
		case claudepool.EventToolArgs:
			if ev.ToolIndex < len(args) {
				args[ev.ToolIndex].WriteString(ev.Text)
			}
		case claudepool.EventDone:
			finish, usage = ev.FinishReason, ev.Usage
		}
		return nil
	})
	if err != nil {
		status, kind, after := claudeErrorStatus(err)
		if status == 499 {
			return
		}
		if after > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(after.Round(time.Second)/time.Second)+1))
		}
		writeJSONStatus(w, status, claudeErrorBody(kind, err.Error()))
		return
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		for i := range calls {
			calls[i]["function"].(map[string]any)["arguments"] = args[i].String()
		}
		message["tool_calls"] = calls
	}
	writeJSON(w, map[string]any{
		"id": id, "object": "chat.completion", "created": created.Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   usagePayload(usage),
	})
}

// claudeSubscriptionConnectedFor reports whether the user has an active
// Claude subscription connection visible to the project.
func (s *Server) claudeSubscriptionConnectedFor(userID int64, projectID string) bool {
	var count int
	err := s.store.dbQueryRow(`SELECT COUNT(*) FROM connections
		WHERE app_slug=? AND user_id=? AND status='active' AND (COALESCE(project_id,'')='' OR project_id=?)`,
		claudeSubscriptionSlug, userID, projectID).Scan(&count)
	return err == nil && count > 0
}

// handleClaudeSubscriptionMCP serves the tool bridge to local claude
// processes only; each session carries its own bearer token.
func (s *Server) handleClaudeSubscriptionMCP(w http.ResponseWriter, r *http.Request) {
	if !requestFromLoopback(r) || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	pool, err := s.claudePool()
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	pool.ServeHTTP(w, r)
}

// --- Status, sign-in and settings ----------------------------------------

type claudeProcessView struct {
	claudepool.ProcessStatus
	MemoryKB int64 `json:"memory_kb,omitempty"`
}

// processMemory returns each pid's memory use in KiB. On macOS it is the
// physical footprint, what Activity Monitor shows: the resident size
// there leaves out the compressed pages of an idle process, so it reads
// far too low for a Claude process waiting on a tool. Elsewhere it is the
// resident size.
func processMemory(pids []int) map[int]int64 {
	if len(pids) == 0 {
		return map[int]int64{}
	}
	if runtime.GOOS == "darwin" {
		if out := processFootprint(pids); len(out) > 0 {
			return out
		}
	}
	return processRSS(pids)
}

var footprintLine = regexp.MustCompile(`(?m)\[([0-9]+)\]:.*\bFootprint:\s*([0-9]+) B\b`)

func processFootprint(pids []int) map[int]int64 {
	bin := "/usr/bin/footprint"
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	args := []string{"-f", "bytes"}
	for _, pid := range pids {
		args = append(args, "-p", strconv.Itoa(pid))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// A pid that exited makes footprint fail; the others still print.
	raw, _ := exec.CommandContext(ctx, bin, args...).Output()
	return parseFootprint(raw)
}

// parseFootprint reads `footprint -f bytes` output, one
// "name [pid]: 64-bit    Footprint: <n> B" line per process.
func parseFootprint(raw []byte) map[int]int64 {
	out := map[int]int64{}
	for _, match := range footprintLine.FindAllSubmatch(raw, -1) {
		pid, err1 := strconv.Atoi(string(match[1]))
		bytes, err2 := strconv.ParseInt(string(match[2]), 10, 64)
		if err1 == nil && err2 == nil {
			out[pid] = bytes / 1024
		}
	}
	return out
}

// processRSS returns the resident memory of each pid in KiB.
func processRSS(pids []int) map[int]int64 {
	out := map[int]int64{}
	if len(pids) == 0 {
		return out
	}
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "ps", "-o", "pid=,rss=", "-p", strings.Join(list, ",")).Output()
	if err != nil && len(raw) == 0 {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		rss, err2 := strconv.ParseInt(fields[1], 10, 64)
		if err1 == nil && err2 == nil {
			out[pid] = rss
		}
	}
	return out
}

func (s *Server) handleClaudeSubscription(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/claude-subscription"), "/")
	switch {
	case path == "" && r.Method == http.MethodGet:
		s.handleClaudeSubscriptionStatus(w, r)
	case path == "/login" && r.Method == http.MethodPost:
		s.handleClaudeSubscriptionLogin(w, r)
	case path == "/login/code" && r.Method == http.MethodPost:
		s.handleClaudeSubscriptionLoginCode(w, r)
	case path == "/login/cancel" && r.Method == http.MethodPost:
		s.handleClaudeSubscriptionLoginCancel(w, r)
	case path == "/logout" && r.Method == http.MethodPost:
		s.handleClaudeSubscriptionLogout(w, r)
	case path == "/settings" && r.Method == http.MethodPut:
		s.handleClaudeSubscriptionSettings(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handleClaudeSubscriptionStatus(w http.ResponseWriter, r *http.Request) {
	cli := claudeSubscriptionCLI()
	out := map[string]any{
		"available":       s.claudeSubscriptionAllowed(),
		"installed":       false,
		"login":           s.claudeSub.login.State(),
		"sign_in_command": "claude auth login",
	}
	if !s.claudeSubscriptionAllowed() {
		out["unavailable_reason"] = "This install has more than one user. A Claude plan is personal, so this provider is only offered on single-user installs."
	}
	if current, err := user.Current(); err == nil {
		out["server_user"] = current.Username
	}
	if cli.ConfigDir != "" {
		out["config_dir"] = cli.ConfigDir
		out["sign_in_command"] = "CLAUDE_CONFIG_DIR=" + shellQuote(cli.ConfigDir) + " claude auth login"
	}
	connID, settings, connected := s.claudeSubscriptionConnection()
	out["connected"] = connected
	if connected {
		out["connection_id"] = connID
	}
	out["settings"] = settings
	if bin, err := cli.Resolve(); err == nil {
		out["installed"] = true
		out["binary"] = bin
		if version, err := cli.Version(r.Context()); err == nil {
			out["version"] = version
		}
	}
	if pool, err := s.claudePool(); err == nil {
		if out["installed"] == true {
			maxAge := 30 * time.Second
			if r.URL.Query().Get("refresh") == "1" {
				maxAge = 0
			}
			if auth, err := pool.Auth(r.Context(), maxAge); err == nil {
				out["auth"] = map[string]any{
					"logged_in":         auth.LoggedIn,
					"auth_method":       auth.AuthMethod,
					"subscription_type": auth.SubscriptionType,
					"subscription":      auth.Subscription(),
					"checked_at":        auth.CheckedAt,
				}
			} else {
				out["auth_error"] = err.Error()
			}
		}
		status := pool.Status()
		pids := make([]int, 0, len(status.Processes))
		for _, proc := range status.Processes {
			pids = append(pids, proc.PID)
		}
		memory := processMemory(pids)
		procs := make([]claudeProcessView, 0, len(status.Processes))
		var total int64
		for _, proc := range status.Processes {
			procs = append(procs, claudeProcessView{ProcessStatus: proc, MemoryKB: memory[proc.PID]})
			total += memory[proc.PID]
		}
		out["pool"] = map[string]any{
			"max_processes": status.MaxProcs,
			"running":       status.Running,
			"busy":          status.Busy,
			"waiting":       status.Waiting,
			"sessions":      status.Sessions,
			"idle_seconds":  int(status.IdleTimeout / time.Second),
			"queue_seconds": int(status.QueueTimeout / time.Second),
			"memory_kb":     total,
			"processes":     procs,
		}
		if status.Limit != nil {
			out["limit"] = status.Limit
		}
	}
	writeJSON(w, out)
}

func shellQuote(value string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_./~-]+$`).MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// handleClaudeSubscriptionLogin starts Claude Code's own sign-in, replacing
// one in progress. Claude Code prints Anthropic's sign-in link, which the
// status reports as login.url; the user opens it, approves, and pastes the
// code Anthropic shows into /login/code. Where Apteva runs on the user's own
// computer, Claude Code may also open the page in a browser itself.
func (s *Server) handleClaudeSubscriptionLogin(w http.ResponseWriter, r *http.Request) {
	if !s.claudeSubscriptionLoginAllowed(w, r) {
		return
	}
	err := s.claudeSub.login.Start(claudeSubscriptionCLI(), 10*time.Minute, s.claudeSubscriptionLoginEnded)
	switch {
	case errors.Is(err, claudepool.ErrNotInstalled):
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "not_installed"})
		return
	case err != nil:
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.claudeSubscriptionLoginEnded()
	writeJSON(w, map[string]any{"started": true, "login": s.claudeSub.login.State()})
}

// handleClaudeSubscriptionLoginCode passes the one-time code from
// Anthropic's page to the running sign-in. The code is neither logged nor
// kept.
func (s *Server) handleClaudeSubscriptionLoginCode(w http.ResponseWriter, r *http.Request) {
	if !s.claudeSubscriptionLoginAllowed(w, r) {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_code"})
		return
	}
	err := s.claudeSub.login.SubmitCode(body.Code)
	switch {
	case errors.Is(err, claudepool.ErrInvalidCode):
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_code", "login": s.claudeSub.login.State()})
	case errors.Is(err, claudepool.ErrLoginNotRunning):
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "not_running", "login": s.claudeSub.login.State()})
	case errors.Is(err, claudepool.ErrCodeSubmitted):
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "code_submitted", "login": s.claudeSub.login.State()})
	case err != nil:
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "submit_failed"})
	default:
		writeJSON(w, map[string]any{"login": s.claudeSub.login.State()})
	}
}

// handleClaudeSubscriptionLoginCancel stops the sign-in in progress.
func (s *Server) handleClaudeSubscriptionLoginCancel(w http.ResponseWriter, r *http.Request) {
	if !s.claudeSubscriptionLoginAllowed(w, r) {
		return
	}
	canceled := s.claudeSub.login.Cancel()
	writeJSON(w, map[string]any{"canceled": canceled, "login": s.claudeSub.login.State()})
}

func (s *Server) claudeSubscriptionLoginAllowed(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireCapability(w, r, "provider_management") {
		return false
	}
	if !s.claudeSubscriptionAllowed() {
		writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "single_user_only"})
		return false
	}
	return true
}

// claudeSubscriptionLoginEnded makes the next status read ask Claude Code
// for its login again.
func (s *Server) claudeSubscriptionLoginEnded() {
	if pool, err := s.claudePool(); err == nil {
		pool.ForgetAuth()
	}
}

// handleClaudeSubscriptionLogout signs Claude Code out on this machine.
func (s *Server) handleClaudeSubscriptionLogout(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapability(w, r, "provider_management") {
		return
	}
	if err := claudeSubscriptionCLI().Logout(r.Context()); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if pool, err := s.claudePool(); err == nil {
		pool.ForgetAuth()
	}
	writeJSON(w, map[string]any{"signed_out": true})
}

func (s *Server) handleClaudeSubscriptionSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapability(w, r, "provider_management") {
		return
	}
	connID, settings, ok := s.claudeSubscriptionConnection()
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "not_connected"})
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	for key, target := range map[string]*int{"pool_size": &settings.PoolSize, "idle_seconds": &settings.IdleSeconds, "queue_seconds": &settings.QueueSeconds} {
		if raw, present := body[key]; present {
			if err := json.Unmarshal(raw, target); err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": key + " must be a whole number"})
				return
			}
		}
	}
	if err := settings.validate(); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	state := map[string]any{}
	_ = json.Unmarshal([]byte(s.store.GetConnectionRuntimeConfigAny(connID)), &state)
	state["pool_size"], state["idle_seconds"], state["queue_seconds"] = settings.PoolSize, settings.IdleSeconds, settings.QueueSeconds
	encoded, _ := json.Marshal(state)
	if err := s.store.UpdateConnectionRuntimeConfig(connID, string(encoded)); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "could not save settings"})
		return
	}
	if pool, err := s.claudePool(); err == nil {
		pool.SetLimits(settings.limits())
	}
	writeJSON(w, map[string]any{"settings": settings})
}

// claudeSubscriptionUsage reports the plan usage meter. Claude Code reads
// it with its own login (its get_usage request); when that fails, the
// limit state the latest turn reported is shown instead.
func (s *Server) claudeSubscriptionUsage(ctx context.Context, connID int64, refresh bool) ProviderUsageSnapshot {
	snapshot := ProviderUsageSnapshot{Supported: true, ProviderID: connID, Kind: "subscription_quota"}
	pool, err := s.claudePool()
	if err != nil {
		return snapshot
	}
	auth, err := pool.Auth(ctx, 5*time.Minute)
	if err != nil {
		return snapshot
	}
	snapshot.Plan = auth.SubscriptionType
	if !auth.Subscription() {
		return snapshot
	}
	maxAge := providerUsageFreshTTL
	if refresh {
		maxAge = providerUsageManualRefreshMin
	}
	usage, err := pool.PlanUsage(ctx, maxAge)
	if usage.CheckedAt.IsZero() {
		if err != nil {
			log.Printf("[CLAUDE-SUB] plan usage unavailable: %v", err)
		}
		return claudeSubscriptionTurnUsage(snapshot, pool.Limit())
	}
	if !usage.Available {
		return snapshot
	}
	if usage.SubscriptionType != "" {
		snapshot.Plan = usage.SubscriptionType
	}
	snapshot.FetchedAt = usage.CheckedAt
	snapshot.Stale = err != nil || time.Since(usage.CheckedAt) > providerUsageStaleTTL
	// A turn may have run into a limit since the meter was read.
	turn := pool.Limit()
	plan := ProviderUsageLimit{ID: "claude", Label: "Claude plan"}
	var others []ProviderUsageLimit
	for _, w := range usage.Windows {
		window := ProviderUsageWindow{ID: w.ID, DurationMinutes: 7 * 24 * 60, ResetsAt: w.ResetsAt}
		if w.ID == "five_hour" {
			window.DurationMinutes = 300
		}
		reached := false
		if w.Utilization != nil {
			window.UsedPercent = int(math.Round(*w.Utilization))
			reached = *w.Utilization >= 100
		}
		if turn != nil && turn.Status == "rejected" && turn.Type == w.ID && turn.UpdatedAt.After(usage.CheckedAt) {
			window.UsedPercent, reached = 100, true
		}
		if w.Utilization == nil && !reached {
			continue
		}
		if reached && snapshot.RateLimitReachedType == "" {
			snapshot.RateLimitReachedType = w.ID
		}
		if w.ID == "five_hour" || w.ID == "seven_day" {
			plan.Windows = append(plan.Windows, window)
			plan.Reached = plan.Reached || reached
			continue
		}
		others = append(others, ProviderUsageLimit{ID: w.ID, Label: w.Label + " (weekly)", Reached: reached, Windows: []ProviderUsageWindow{window}})
	}
	snapshot.Limits = append([]ProviderUsageLimit{plan}, others...)
	return snapshot
}

// claudeSubscriptionTurnUsage shows the limit state the latest turn
// reported. Claude Code reports a percentage only near the limit.
func claudeSubscriptionTurnUsage(snapshot ProviderUsageSnapshot, limit *claudepool.LimitInfo) ProviderUsageSnapshot {
	if limit == nil {
		return snapshot
	}
	snapshot.FetchedAt = limit.UpdatedAt
	snapshot.Stale = time.Since(limit.UpdatedAt) > providerUsageStaleTTL
	reached := limit.Status == "rejected"
	window := ProviderUsageWindow{ID: limit.Type, ResetsAt: limit.ResetsAt}
	switch limit.Type {
	case "five_hour":
		window.DurationMinutes = 300
	case "seven_day", "seven_day_opus", "seven_day_sonnet":
		window.DurationMinutes = 7 * 24 * 60
	}
	switch {
	case limit.Utilization != nil:
		window.UsedPercent = int(*limit.Utilization*100 + 0.5)
	case reached:
		window.UsedPercent = 100
	default:
		window.UsedPercent = -1
	}
	if reached {
		snapshot.RateLimitReachedType = limit.Type
	}
	usageLimit := ProviderUsageLimit{ID: "claude", Label: "Claude plan", Reached: reached}
	if window.UsedPercent >= 0 {
		usageLimit.Windows = []ProviderUsageWindow{window}
	}
	snapshot.Limits = []ProviderUsageLimit{usageLimit}
	return snapshot
}
