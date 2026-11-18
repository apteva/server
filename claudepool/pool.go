package claudepool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// mcpServerName is the MCP server name Claude sees; its tools appear as
// mcp__apteva__<tool>.
const mcpServerName = "apteva"

// Config configures a Pool.
type Config struct {
	CLI CLI
	// WorkDir is the empty working directory every claude process runs
	// in. Claude Code files sessions under a folder named after it, so it
	// must be used by nothing else.
	WorkDir string
	// StateDir holds per-session files (system prompt, MCP config).
	StateDir string
	// MCPURL is the pool's MCP endpoint as Claude Code reaches it.
	MCPURL string
	// ToolTimeout bounds how long a tool call may wait for its result.
	// Claude Code is given a slightly longer timeout so it never gives up
	// first: when it does, the model calls the tool again.
	ToolTimeout time.Duration
	Limits      Limits
	// MaxSessionsPerOwner bounds how many conversations are kept per owner.
	MaxSessionsPerOwner int
	// SessionTTL drops conversations unused for this long.
	SessionTTL time.Duration
	Logf       func(format string, args ...any)
}

// Pool shares a bounded number of claude processes between conversations.
type Pool struct {
	cfg Config

	mu       sync.Mutex
	limits   Limits
	sessions map[string]*session // by id
	byToken  map[string]*session
	reserved int // slots promised to processes being started
	waiters  map[chan struct{}]struct{}
	limit    *LimitInfo
	auth     *AuthState
	closed   bool

	// probeMu serializes the short-lived processes that ask Claude Code
	// for plan usage and models, and guards their cached answers.
	probeMu    sync.Mutex
	usage      *PlanUsage
	models     []ModelOption
	modelsAt   time.Time
	modelsFrom string // cliIdentity of the binary that listed them

	stop chan struct{}
}

// New creates a pool and removes sessions left by a previous run, which
// can no longer be matched to a conversation.
func New(cfg Config) (*Pool, error) {
	if cfg.WorkDir == "" || cfg.StateDir == "" || cfg.MCPURL == "" {
		return nil, errors.New("claudepool: WorkDir, StateDir and MCPURL are required")
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 10*time.Minute + 30*time.Second
	}
	if cfg.MaxSessionsPerOwner <= 0 {
		cfg.MaxSessionsPerOwner = 16
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 24 * time.Hour
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	cfg.Limits = normalizeLimits(cfg.Limits)
	for _, dir := range []string{cfg.WorkDir, cfg.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if real, err := filepath.EvalSymlinks(cfg.WorkDir); err == nil {
		cfg.WorkDir = real
	}
	p := &Pool{
		cfg:      cfg,
		limits:   cfg.Limits,
		sessions: map[string]*session{},
		byToken:  map[string]*session{},
		waiters:  map[chan struct{}]struct{}{},
		stop:     make(chan struct{}),
	}
	p.removeOrphanSessions()
	go p.reap()
	return p, nil
}

func normalizeLimits(l Limits) Limits {
	if l.MaxProcs <= 0 {
		l.MaxProcs = 4
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = 90 * time.Second
	}
	if l.QueueTimeout <= 0 {
		l.QueueTimeout = 60 * time.Second
	}
	return l
}

// SetLimits changes the pool limits; extra processes close as they go idle.
func (p *Pool) SetLimits(l Limits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limits = normalizeLimits(l)
	p.notifyLocked()
}

// SetCLI changes how claude is found and which login it uses.
func (p *Pool) SetCLI(cli CLI) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.CLI != cli {
		p.cfg.CLI = cli
		p.auth = nil
	}
}

// CLI returns the pool's CLI settings.
func (p *Pool) CLI() CLI {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.CLI
}

// Close stops every process.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	var procs []*process
	for _, sess := range p.sessions {
		if sess.proc != nil {
			procs = append(procs, sess.proc)
			sess.proc = nil
		}
		sess.failHeld("Apteva is shutting down.")
	}
	p.notifyLocked()
	p.mu.Unlock()
	// Sessions do not outlive the pool (New removes them), so there is
	// nothing to wind down gracefully.
	for _, proc := range procs {
		proc.kill()
	}
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for _, proc := range procs {
		select {
		case <-proc.done:
		case <-timeout.C:
			return
		}
	}
}

// ForgetAuth drops the cached login state, e.g. after sign-in or sign-out.
func (p *Pool) ForgetAuth() {
	p.mu.Lock()
	p.auth = nil
	p.mu.Unlock()
}

// Auth returns the login state, checking it when the cached copy is old.
func (p *Pool) Auth(ctx context.Context, maxAge time.Duration) (AuthState, error) {
	p.mu.Lock()
	cached := p.auth
	cli := p.cfg.CLI
	p.mu.Unlock()
	if cached != nil && time.Since(cached.CheckedAt) < maxAge {
		return *cached, nil
	}
	state, err := cli.AuthStatus(ctx)
	if err != nil {
		return AuthState{}, err
	}
	p.mu.Lock()
	p.auth = &state
	p.mu.Unlock()
	return state, nil
}

// Limit returns the latest usage limit state Claude Code reported.
func (p *Pool) Limit() *LimitInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.limit == nil {
		return nil
	}
	copy := *p.limit
	return &copy
}

func (p *Pool) noteLimit(info *rateLimitInfo) {
	if info == nil || info.Status == "" {
		return
	}
	limit := &LimitInfo{Status: info.Status, Type: info.RateLimitType, Utilization: info.Utilization, UpdatedAt: time.Now()}
	if info.ResetsAt > 0 {
		reset := time.Unix(info.ResetsAt, 0)
		limit.ResetsAt = &reset
	}
	p.mu.Lock()
	p.limit = limit
	p.mu.Unlock()
}

func (p *Pool) checkReady(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if p.limit != nil && p.limit.Status == "rejected" && p.limit.ResetsAt != nil && time.Now().Before(*p.limit.ResetsAt) {
		wait := time.Until(*p.limit.ResetsAt)
		p.mu.Unlock()
		return &RetryError{Err: ErrLimited, RetryAfter: wait}
	}
	p.mu.Unlock()
	maxAge := time.Minute
	p.mu.Lock()
	if p.auth != nil && !p.auth.LoggedIn {
		maxAge = 10 * time.Second
	}
	p.mu.Unlock()
	auth, err := p.Auth(ctx, maxAge)
	if err != nil {
		if errors.Is(err, ErrNotInstalled) {
			return err
		}
		return fmt.Errorf("check Claude Code login: %w", err)
	}
	if !auth.LoggedIn {
		return &RetryError{Err: ErrNotSignedIn, RetryAfter: time.Minute}
	}
	if !auth.Subscription() {
		return fmt.Errorf("Claude Code is signed in with %q, not a Claude subscription; sign in with `claude auth login --claudeai`", auth.AuthMethod)
	}
	return nil
}

// Run sends one request and streams the reply through emit.
func (p *Pool) Run(ctx context.Context, req Request, emit func(Event) error) error {
	if len(req.Messages) == 0 {
		return errors.New("no messages")
	}
	if err := p.checkReady(ctx); err != nil {
		return err
	}
	sess, err := p.claim(ctx, req)
	if err != nil {
		return err
	}
	return p.runTurn(ctx, sess, req, emit)
}

// claim finds the conversation req continues, or starts a new one, and
// returns it with its turn lock held.
func (p *Pool) claim(ctx context.Context, req Request) (*session, error) {
	for attempt := 0; attempt < 3; attempt++ {
		p.mu.Lock()
		sess := p.matchLocked(req)
		if sess == nil {
			sess = p.addSessionLocked(req)
		}
		p.mu.Unlock()

		select {
		case sess.turn <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.stop:
			return nil, ErrClosed
		}
		p.mu.Lock()
		if p.sessions[sess.id] == sess && (sess.dirty || matchHistory(sess.history, req.Messages)) {
			sess.turnHeld = true
			sess.lastUsed = time.Now()
			p.mu.Unlock()
			return sess, nil
		}
		p.mu.Unlock()
		<-sess.turn
	}
	return nil, errors.New("conversation changed while waiting")
}

func (p *Pool) matchLocked(req Request) *session {
	var best *session
	for _, sess := range p.sessions {
		if sess.owner != req.Owner || sess.dirty || len(sess.history) == 0 {
			continue
		}
		if !matchHistory(sess.history, req.Messages) {
			continue
		}
		if best == nil || len(sess.history) > len(best.history) ||
			(len(sess.history) == len(best.history) && sess.lastUsed.After(best.lastUsed)) {
			best = sess
		}
	}
	return best
}

func (p *Pool) addSessionLocked(req Request) *session {
	sess := newSession(req.Owner, req.OwnerLabel)
	p.sessions[sess.id] = sess
	p.byToken[sess.mcpToken] = sess
	return sess
}

func (p *Pool) releaseTurn(sess *session) {
	p.mu.Lock()
	sess.turnHeld = false
	sess.lastUsed = time.Now()
	if sess.retire && p.sessions[sess.id] == sess {
		p.retireLocked(sess)
	}
	p.notifyLocked()
	p.mu.Unlock()
	<-sess.turn
}

// runTurn delivers the request to the session and streams the reply.
// It is entered with the session's turn lock held.
func (p *Pool) runTurn(ctx context.Context, sess *session, req Request, emit func(Event) error) error {
	p.mu.Lock()
	var pl plan
	if sess.dirty || !sess.started || (len(sess.awaiting) > 0 && sess.proc == nil) {
		pl = plan{reseed: true}
	} else {
		pl = sess.planFor(req.Messages)
	}
	if pl.reseed && (sess.started || sess.dirty) {
		// Claude's copy of this conversation no longer matches; continue
		// in a fresh session seeded with the caller's history.
		old := sess
		sess = p.addSessionLocked(req)
		sess.turn <- struct{}{}
		sess.turnHeld = true
		p.retireLocked(old)
		p.mu.Unlock()
		p.releaseTurn(old)
		p.mu.Lock()
	}
	// A new system prompt or tool list restarts the process between
	// turns, never while tools are pending. The model is switched in
	// place at any time: callers pick a model per request, and most turns
	// leave tool calls pending.
	key := toolsKey(req.Tools)
	if len(sess.awaiting) == 0 {
		if sess.proc != nil && (sess.system != req.System || sess.toolsKey != key) {
			proc := sess.proc
			sess.proc = nil
			p.notifyLocked()
			defer proc.close()
		}
		sess.system, sess.toolsKey, sess.tools = req.System, key, append([]Tool(nil), req.Tools...)
	}
	switchModel := sess.proc != nil && sess.model != req.Model
	if sess.proc == nil {
		sess.model = req.Model
	}
	needProc := sess.proc == nil
	p.mu.Unlock()

	if needProc {
		if err := p.acquireSlot(ctx, sess); err != nil {
			p.releaseTurn(sess)
			return err
		}
		if err := p.spawn(sess); err != nil {
			p.mu.Lock()
			p.reserved--
			sess.dirty = true
			p.notifyLocked()
			p.mu.Unlock()
			p.releaseTurn(sess)
			return err
		}
	}

	p.mu.Lock()
	proc := sess.proc
	if proc == nil {
		sess.dirty = true
		p.mu.Unlock()
		p.releaseTurn(sess)
		return errors.New("Claude process exited before the turn started")
	}
	sess.busy = true
	sess.setState("replying", "")
	awaiting := sess.awaiting
	p.mu.Unlock()

	if switchModel {
		if err := p.switchModel(proc, req.Model); err != nil {
			p.failTurn(sess, proc)
			return err
		}
		p.mu.Lock()
		sess.model = req.Model
		p.mu.Unlock()
	}

	// Deliver the new input.
	var sendErr error
	switch {
	case pl.reseed:
		sendErr = proc.send(textBlocks([]string{renderTranscript(req.Messages)}))
	case len(awaiting) > 0:
		delivered := map[string]bool{}
		for i, msg := range pl.toolResults {
			text := msg.Text
			if i == len(pl.toolResults)-1 && len(pl.userText) > 0 {
				// Claude is mid-turn; messages that arrived with the
				// results travel with the last one.
				text += "\n\n<new_messages>\n" + strings.Join(pl.userText, "\n\n") + "\n</new_messages>"
			}
			sess.deliver(msg.ToolCallID, toolResult{text: text, isError: msg.IsError})
			delivered[msg.ToolCallID] = true
		}
		for _, call := range awaiting {
			if !delivered[call.ID] {
				sess.deliver(call.ID, toolResult{text: "No result was returned for this tool call.", isError: true})
			}
		}
	default:
		sendErr = proc.send(textBlocks(pl.userText))
	}
	if sendErr != nil {
		p.failTurn(sess, proc)
		return fmt.Errorf("send to Claude: %w", sendErr)
	}

	reply, finish, usage, model, err := p.readReply(ctx, sess, proc, emit)
	if err != nil {
		p.failTurn(sess, proc)
		return err
	}

	p.mu.Lock()
	sess.started = true
	sess.turns++
	sess.history = append(append([]Message(nil), req.Messages...), reply)
	if finish == "tool_calls" {
		sess.awaiting = reply.ToolCalls
		sess.waitMoves = proc.moves.Load()
		names := make([]string, 0, len(reply.ToolCalls))
		for _, call := range reply.ToolCalls {
			names = append(names, call.Name)
		}
		sess.setState("waiting_tool", strings.Join(names, ", "))
	} else {
		sess.awaiting = nil
		sess.setState("finishing", "")
	}
	p.mu.Unlock()

	if err := emit(Event{Kind: EventDone, FinishReason: finish, Usage: usage, Model: model}); err != nil {
		// The caller missed the end of the reply and will resend an
		// older conversation; this session can no longer match it.
		p.failTurn(sess, proc)
		return err
	}

	if finish == "tool_calls" {
		p.releaseTurn(sess)
		return nil
	}
	// Claude Code reports the turn's result about a second after the
	// reply ends; keep the session until then without making the caller
	// wait for it.
	go p.finishTurn(sess, proc)
	return nil
}

func (p *Pool) finishTurn(sess *session, proc *process) {
	timeout := time.NewTimer(2 * time.Minute)
	defer timeout.Stop()
	for done := false; !done; {
		select {
		case msg, ok := <-proc.msgs:
			if !ok {
				done = true
				break
			}
			switch msg.Type {
			case "rate_limit_event":
				p.noteLimit(msg.RateLimitInfo)
			case "result":
				done = true
			}
		case <-timeout.C:
			p.cfg.Logf("[CLAUDE-POOL] session %s: no result after the reply; closing its process", short(sess.id))
			p.mu.Lock()
			if sess.proc == proc {
				sess.proc = nil
			}
			p.mu.Unlock()
			proc.close()
			done = true
		}
	}
	p.mu.Lock()
	sess.busy = false
	sess.setState("idle", "")
	p.mu.Unlock()
	p.releaseTurn(sess)
}

// failTurn resets a session whose turn failed: its Claude copy may now
// differ from the caller's, so the next request starts a fresh session.
func (p *Pool) failTurn(sess *session, proc *process) {
	p.mu.Lock()
	sess.dirty = true
	sess.busy = false
	sess.awaiting = nil
	sess.setState("idle", "")
	if sess.proc == proc {
		sess.proc = nil
	}
	p.notifyLocked()
	p.mu.Unlock()
	sess.failHeld("The turn was interrupted.")
	proc.close()
	p.releaseTurn(sess)
}

func textBlocks(texts []string) []map[string]any {
	blocks := make([]map[string]any, 0, len(texts))
	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": "Continue."})
	}
	return blocks
}

// replyIdleTimeout bounds the silence between two stream messages.
var replyIdleTimeout = 5 * time.Minute

// toolGrace is how long a process waiting for tool results keeps its slot
// when another conversation needs one. Callers often run tools for seconds,
// but an agent may also pause for a long time before sending results.
var toolGrace = 30 * time.Second

// reapInterval is how often idle processes and old sessions are checked.
var reapInterval = 5 * time.Second

// toolIdleTimeout, when set, replaces the idle limit Claude Code gets for
// held tool calls (ToolTimeout plus a minute). Tests shorten it to make
// Claude give up on a call.
var toolIdleTimeout time.Duration

type blockState struct {
	kind     string
	toolIdx  int
	toolID   string
	toolName string
	args     strings.Builder
}

// readReply streams one model reply: it ends when the model stops,
// either to call tools or because it is done.
func (p *Pool) readReply(ctx context.Context, sess *session, proc *process, emit func(Event) error) (reply Message, finish string, usage Usage, model string, err error) {
	reply = Message{Role: "assistant"}
	var (
		text       strings.Builder
		blocks     = map[int]*blockState{}
		nextTool   int
		stopReason string
	)
	// Claude Code streams continuously while it works; a long silence
	// means the process is stuck.
	idle := time.NewTimer(replyIdleTimeout)
	defer idle.Stop()
	for {
		var msg streamMsg
		var ok bool
		select {
		case <-ctx.Done():
			return reply, "", usage, model, ctx.Err()
		case <-p.stop:
			return reply, "", usage, model, ErrClosed
		case <-idle.C:
			return reply, "", usage, model, fmt.Errorf("Claude sent nothing for %s", replyIdleTimeout)
		case msg, ok = <-proc.msgs:
		}
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(replyIdleTimeout)
		if !ok {
			return reply, "", usage, model, fmt.Errorf("Claude process exited: %s", proc.failure())
		}
		switch msg.Type {
		case "rate_limit_event":
			p.noteLimit(msg.RateLimitInfo)
			continue
		case "system":
			if msg.Subtype == "init" {
				for _, server := range msg.MCPServers {
					if server.Name == mcpServerName && server.Status != "connected" {
						p.cfg.Logf("[CLAUDE-POOL] session %s: tool server status %q", short(sess.id), server.Status)
					}
				}
			}
			continue
		case "result":
			// The turn ended without a reply: a failed sign-in, a usage
			// limit or another error.
			return reply, "", usage, model, p.resultError(msg)
		case "stream_event":
		default:
			continue
		}
		ev := msg.Event
		if ev == nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Model != "" {
				model = ev.Message.Model
			}
			if ev.Message != nil && ev.Message.Usage != nil {
				u := ev.Message.Usage
				usage.InputTokens += u.InputTokens
				usage.CacheReadTokens += u.CacheReadInputTokens
				usage.CacheWriteTokens += u.CacheCreationInputTokens
				usage.OutputTokens += u.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			b := &blockState{kind: ev.ContentBlock.Type}
			blocks[ev.Index] = b
			if b.kind == "tool_use" {
				b.toolIdx = nextTool
				nextTool++
				b.toolID = ev.ContentBlock.ID
				b.toolName = p.originalToolName(sess, ev.ContentBlock.Name)
				if err := emit(Event{Kind: EventToolStart, ToolIndex: b.toolIdx, ToolID: b.toolID, ToolName: b.toolName}); err != nil {
					return reply, "", usage, model, err
				}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			var err error
			switch ev.Delta.Type {
			case "text_delta":
				text.WriteString(ev.Delta.Text)
				err = emit(Event{Kind: EventText, Text: ev.Delta.Text})
			case "thinking_delta":
				err = emit(Event{Kind: EventThinking, Text: ev.Delta.Thinking})
			case "input_json_delta":
				if b := blocks[ev.Index]; b != nil && b.kind == "tool_use" {
					b.args.WriteString(ev.Delta.PartialJSON)
					err = emit(Event{Kind: EventToolArgs, ToolIndex: b.toolIdx, ToolID: b.toolID, ToolName: b.toolName, Text: ev.Delta.PartialJSON})
				}
			}
			if err != nil {
				return reply, "", usage, model, err
			}
		case "content_block_stop":
			if b := blocks[ev.Index]; b != nil && b.kind == "tool_use" {
				args := b.args.String()
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				reply.ToolCalls = append(reply.ToolCalls, ToolCall{ID: b.toolID, Name: b.toolName, Arguments: args})
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			reply.Text = text.String()
			switch {
			case stopReason == "tool_use" && len(reply.ToolCalls) > 0:
				return reply, "tool_calls", usage, model, nil
			case stopReason == "max_tokens":
				return reply, "length", usage, model, nil
			default:
				reply.ToolCalls = nil
				return reply, "stop", usage, model, nil
			}
		}
	}
}

var (
	authFailure  = regexp.MustCompile(`(?i)(authenticat|oauth|log ?in|logged out|credential|unauthori[sz]ed|401)`)
	limitFailure = regexp.MustCompile(`(?i)(usage limit|rate limit|limit reached|hit your limit|out of (extra )?usage)`)
)

func (p *Pool) resultError(msg streamMsg) error {
	text := strings.TrimSpace(msg.Result)
	if text == "" {
		text = msg.Subtype
	}
	switch {
	case authFailure.MatchString(text):
		p.ForgetAuth()
		return &RetryError{Err: ErrNotSignedIn, RetryAfter: time.Minute}
	case limitFailure.MatchString(text):
		wait := 5 * time.Minute
		p.mu.Lock()
		if p.limit != nil && p.limit.ResetsAt != nil && time.Now().Before(*p.limit.ResetsAt) {
			wait = time.Until(*p.limit.ResetsAt)
			p.limit.Status = "rejected"
		}
		p.mu.Unlock()
		return &RetryError{Err: ErrLimited, RetryAfter: wait}
	}
	return fmt.Errorf("Claude: %s", text)
}

// switchModel changes the model of a running process in place, so the
// conversation, its prompt cache and any pending tool calls carry on.
func (p *Pool) switchModel(proc *process, model string) error {
	var target any = model
	if model == "" {
		target = nil // the session default
	}
	id, err := proc.control(map[string]any{"subtype": "set_model", "model": target})
	if err != nil {
		return fmt.Errorf("switch Claude to %s: %w", model, err)
	}
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case msg, ok := <-proc.msgs:
			if !ok {
				return fmt.Errorf("switch Claude to %s: %s", model, proc.failure())
			}
			if msg.Type == "rate_limit_event" {
				p.noteLimit(msg.RateLimitInfo)
			}
			if msg.Type != "control_response" || msg.Response == nil || msg.Response.RequestID != id {
				continue
			}
			if msg.Response.Subtype == "error" {
				return fmt.Errorf("switch Claude to %s: %s", model, msg.Response.Error)
			}
			return nil
		case <-timeout.C:
			return fmt.Errorf("switch Claude to %s: no answer", model)
		}
	}
}

// acquireSlot reserves room for a new process: a free slot, the slot of the
// least recently used idle process, or that of the process that has waited
// longest for tool results beyond toolGrace. The process is closed.
func (p *Pool) acquireSlot(ctx context.Context, sess *session) error {
	deadline := time.Now().Add(p.limitsSnapshot().QueueTimeout)
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return ErrClosed
		}
		if p.runningLocked()+p.reserved < p.limits.MaxProcs {
			p.reserved++
			p.mu.Unlock()
			return nil
		}
		if victim := p.lruIdleLocked(sess); victim != nil {
			proc := victim.proc
			victim.proc = nil
			victim.setState("closed", "")
			p.reserved++
			p.mu.Unlock()
			proc.close()
			return nil
		}
		now := time.Now()
		parked, next := p.longestWaitingLocked(sess, now)
		if parked != nil {
			p.cfg.Logf("[CLAUDE-POOL] session %s: waited %s for tool results; closing it for another conversation", short(parked.id), now.Sub(parked.stateSince).Round(time.Second))
			p.parkLocked(parked)
			p.reserved++
			p.mu.Unlock()
			return nil
		}
		ch := make(chan struct{})
		p.waiters[ch] = struct{}{}
		sess.setState("waiting_slot", "")
		p.mu.Unlock()

		remaining := time.Until(deadline)
		wake := remaining
		if !next.IsZero() && next.Sub(now) < wake {
			wake = next.Sub(now)
		}
		timer := time.NewTimer(wake)
		var err error
		if remaining <= 0 {
			err = &RetryError{Err: ErrPoolBusy, RetryAfter: 30 * time.Second}
		} else {
			select {
			case <-ch:
			case <-ctx.Done():
				err = ctx.Err()
			case <-timer.C:
				if !time.Now().Before(deadline) {
					err = &RetryError{Err: ErrPoolBusy, RetryAfter: 30 * time.Second}
				}
			case <-p.stop:
				err = ErrClosed
			}
		}
		timer.Stop()
		p.mu.Lock()
		delete(p.waiters, ch)
		p.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

func (p *Pool) limitsSnapshot() Limits {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.limits
}

func (p *Pool) runningLocked() int {
	n := 0
	for _, sess := range p.sessions {
		if sess.proc != nil {
			n++
		}
	}
	return n
}

func (p *Pool) lruIdleLocked(except *session) *session {
	var victim *session
	for _, sess := range p.sessions {
		if sess == except || sess.proc == nil || sess.busy || sess.turnHeld {
			continue
		}
		if victim == nil || sess.lastUsed.Before(victim.lastUsed) {
			victim = sess
		}
	}
	return victim
}

// longestWaitingLocked returns the session that has waited longest for tool
// results beyond toolGrace. When none has yet, it returns when the first one
// will (zero if none is waiting).
func (p *Pool) longestWaitingLocked(except *session, now time.Time) (*session, time.Time) {
	var victim *session
	var next time.Time
	for _, sess := range p.sessions {
		if sess == except || sess.proc == nil || sess.turnHeld || !sess.busy || sess.state != "waiting_tool" {
			continue
		}
		if at := sess.stateSince.Add(toolGrace); at.After(now) {
			if next.IsZero() || at.Before(next) {
				next = at
			}
			continue
		}
		if victim == nil || sess.stateSince.Before(victim.stateSince) {
			victim = sess
		}
	}
	return victim, next
}

// parkLocked frees the slot of a session whose caller is still running
// tools. Claude is stopped before it sees any result, so when the results
// arrive the conversation continues in a new session seeded from the
// caller's history.
func (p *Pool) parkLocked(sess *session) {
	if sess.proc != nil {
		sess.proc.kill()
		sess.proc = nil
	}
	sess.busy = false
	sess.awaiting = nil
	p.retireLocked(sess)
}

// callDropped handles Claude Code giving up on a held tool call before the
// caller sent its result. Claude then carries on without that result,
// where nobody reads it, and its copy of the conversation no longer matches
// the caller's. The process is stopped, so the caller's next request
// continues in a new session seeded from its own history. This covers a
// call whose connection closed; Claude Code 2.1.327 leaves the connection
// open when it gives up, and the reaper sees that case in Claude's output.
func (p *Pool) callDropped(sess *session, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions[sess.id] != sess || sess.proc == nil || !sess.awaitsLocked(id) {
		return
	}
	if sess.turnHeld {
		// The results are being delivered right now; that turn's reply
		// cannot be trusted, so the next one starts over.
		p.cfg.Logf("[CLAUDE-POOL] session %s: Claude gave up on a tool call while its result was on the way", short(sess.id))
		sess.dirty = true
		return
	}
	p.cfg.Logf("[CLAUDE-POOL] session %s: Claude gave up waiting for a tool result after %s; closing its process", short(sess.id), time.Since(sess.stateSince).Round(time.Second))
	p.parkLocked(sess)
}

func (p *Pool) notifyLocked() {
	for ch := range p.waiters {
		close(ch)
		delete(p.waiters, ch)
	}
}

// spawn starts a process for the session, using a slot reserved by
// acquireSlot.
func (p *Pool) spawn(sess *session) error {
	p.mu.Lock()
	cli := p.cfg.CLI
	system, model, tools, started := sess.system, sess.model, sess.tools, sess.started
	p.mu.Unlock()

	bin, err := cli.Resolve()
	if err != nil {
		return err
	}
	dir := filepath.Join(p.cfg.StateDir, sess.id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	systemFile := filepath.Join(dir, "system.md")
	if err := os.WriteFile(systemFile, []byte(system), 0o600); err != nil {
		return err
	}
	servers := map[string]any{}
	if len(tools) > 0 {
		servers[mcpServerName] = map[string]any{
			"type":    "http",
			"url":     p.cfg.MCPURL,
			"headers": map[string]string{"Authorization": "Bearer " + sess.mcpToken},
		}
	}
	mcpConfig, _ := json.Marshal(map[string]any{"mcpServers": servers})
	mcpFile := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpFile, mcpConfig, 0o600); err != nil {
		return err
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--tools", "",
		"--strict-mcp-config",
		"--mcp-config", mcpFile,
		"--allowedTools", "mcp__" + mcpServerName,
		"--setting-sources", "project",
		"--system-prompt-file", systemFile,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if started {
		args = append(args, "--resume", sess.id)
	} else {
		args = append(args, "--session-id", sess.id)
	}
	// A held tool call lasts until the caller comes back, which for a
	// sleeping agent is minutes. The pool decides when to stop waiting
	// (ToolTimeout), so Claude Code's own limits, the total and the newer
	// idle one (5 minutes by default, checked every 30s), are raised past
	// it. The idle limit is also reset by the progress notifications the
	// pool sends while it holds a call (see callTool), so a release that
	// ignores this setting keeps the call too; if Claude still gives up,
	// the reaper sees it and closes the process.
	toolWait := msString(p.cfg.ToolTimeout + time.Minute)
	idleWait := toolWait
	if toolIdleTimeout > 0 {
		idleWait = msString(toolIdleTimeout)
	}
	env := cli.Env(
		"MCP_TOOL_TIMEOUT="+toolWait,
		"CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT="+idleWait,
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
	)
	proc, err := startProcess(bin, args, env, p.cfg.WorkDir)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.reserved--
	sess.proc = proc
	sess.setState("starting", "")
	p.mu.Unlock()
	p.cfg.Logf("[CLAUDE-POOL] started pid=%d session=%s owner=%s resume=%v", proc.pid, short(sess.id), sess.owner, started)
	go func() {
		<-proc.done
		p.mu.Lock()
		if sess.proc == proc {
			sess.proc = nil
			if sess.busy {
				sess.dirty = true
			}
		}
		p.notifyLocked()
		p.mu.Unlock()
	}()
	return nil
}

// retireLocked drops a session that a fresh one replaces.
func (p *Pool) retireLocked(sess *session) {
	delete(p.byToken, sess.mcpToken)
	delete(p.sessions, sess.id)
	if sess.proc != nil {
		proc := sess.proc
		sess.proc = nil
		go proc.close()
	}
	sess.failHeld("This conversation was restarted.")
	go p.removeSessionFiles(sess.id)
	p.notifyLocked()
}

// reap closes idle processes and forgets old conversations.
func (p *Pool) reap() {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
		}
		var closing []*process
		p.mu.Lock()
		now := time.Now()
		perOwner := map[string][]*session{}
		for _, sess := range p.sessions {
			perOwner[sess.owner] = append(perOwner[sess.owner], sess)
			if sess.turnHeld {
				continue
			}
			if sess.proc != nil && !sess.busy && now.Sub(sess.lastUsed) > p.limits.IdleTimeout {
				closing = append(closing, sess.proc)
				sess.proc = nil
				sess.setState("closed", "")
				continue
			}
			if sess.busy && sess.state == "waiting_tool" && sess.proc != nil && sess.proc.moves.Load() != sess.waitMoves {
				// Nothing reaches Claude while the pool holds its calls,
				// so it went on alone: it gave up on a call (Claude Code
				// aborts calls it thinks are stuck) and is answering
				// without the result, where nobody reads it. The caller's
				// next request starts again from its own history.
				p.cfg.Logf("[CLAUDE-POOL] session %s: Claude carried on without the tool result after %s; closing its process", short(sess.id), now.Sub(sess.stateSince).Round(time.Second))
				p.parkLocked(sess)
				continue
			}
			if sess.busy && sess.state == "waiting_tool" && now.Sub(sess.stateSince) > p.cfg.ToolTimeout {
				// The caller has not come back with the tool results.
				// Stop Claude rather than let it time the calls out
				// and carry on where nobody reads it.
				p.cfg.Logf("[CLAUDE-POOL] session %s: no tool results after %s; closing its process", short(sess.id), p.cfg.ToolTimeout)
				p.parkLocked(sess)
				continue
			}
			if sess.proc == nil && !sess.busy && now.Sub(sess.lastUsed) > p.cfg.SessionTTL {
				p.retireLocked(sess)
			}
		}
		for _, list := range perOwner {
			if len(list) <= p.cfg.MaxSessionsPerOwner {
				continue
			}
			sort.Slice(list, func(i, j int) bool { return list[i].lastUsed.After(list[j].lastUsed) })
			for _, sess := range list[p.cfg.MaxSessionsPerOwner:] {
				if sess.proc == nil && !sess.busy && !sess.turnHeld {
					p.retireLocked(sess)
				}
			}
		}
		if len(closing) > 0 {
			p.notifyLocked()
		}
		p.mu.Unlock()
		for _, proc := range closing {
			proc.close()
		}
	}
}

// Status returns a snapshot of the pool.
func (p *Pool) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{
		MaxProcs:     p.limits.MaxProcs,
		Waiting:      len(p.waiters),
		Sessions:     len(p.sessions),
		IdleTimeout:  p.limits.IdleTimeout,
		QueueTimeout: p.limits.QueueTimeout,
		Processes:    []ProcessStatus{},
	}
	for _, sess := range p.sessions {
		if sess.proc == nil {
			continue
		}
		st.Running++
		if sess.busy || sess.turnHeld {
			st.Busy++
		}
		st.Processes = append(st.Processes, ProcessStatus{
			Session:    short(sess.id),
			Owner:      sess.owner,
			Model:      sess.model,
			OwnerLabel: sess.ownerLabel,
			PID:        sess.proc.pid,
			State:      sess.state,
			Detail:     sess.detail,
			Since:      sess.stateSince,
			StartedAt:  sess.proc.startedAt,
			Turns:      sess.turns,
		})
	}
	sort.Slice(st.Processes, func(i, j int) bool { return st.Processes[i].StartedAt.Before(st.Processes[j].StartedAt) })
	if p.limit != nil {
		copy := *p.limit
		st.Limit = &copy
	}
	return st
}

// CloseOwner ends every conversation of an owner, e.g. a stopped agent.
func (p *Pool) CloseOwner(owner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sess := range p.sessions {
		if sess.owner != owner {
			continue
		}
		if sess.turnHeld {
			sess.retire = true
			continue
		}
		p.retireLocked(sess)
	}
}

// projectDir is where Claude Code files the sessions of WorkDir.
func (p *Pool) projectDir() string {
	base := p.cfg.CLI.ConfigDir
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "projects", projectDirName(p.cfg.WorkDir))
}

var nonProjectChars = regexp.MustCompile(`[^A-Za-z0-9-]`)

// projectDirName mirrors how Claude Code names a working directory's
// session folder.
func projectDirName(dir string) string {
	return nonProjectChars.ReplaceAllString(dir, "-")
}

func (p *Pool) removeSessionFiles(id string) {
	_ = os.RemoveAll(filepath.Join(p.cfg.StateDir, id))
	if dir := p.projectDir(); dir != "" {
		_ = os.Remove(filepath.Join(dir, id+".jsonl"))
		_ = os.RemoveAll(filepath.Join(dir, id))
	}
}

// removeOrphanSessions deletes sessions from earlier runs: the pool keeps
// the mapping from conversations to sessions in memory only.
func (p *Pool) removeOrphanSessions() {
	entries, _ := os.ReadDir(p.cfg.StateDir)
	for _, entry := range entries {
		p.removeSessionFiles(entry.Name())
	}
	if dir := p.projectDir(); dir != "" {
		files, _ := os.ReadDir(dir)
		for _, file := range files {
			if strings.HasSuffix(file.Name(), ".jsonl") {
				_ = os.Remove(filepath.Join(dir, file.Name()))
			}
		}
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
