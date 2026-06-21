package main

// eval_streaming.go — server-sent events transport for the eval
// runner's per-iteration pause loop.
//
// Architecture:
//   1. Client POSTs to /api/evals/preview/stream or
//      /api/agents/:id/evals/:evalId/run/stream with the same body
//      the synchronous endpoints accept.
//   2. Handler generates a run_id, registers a per-run control
//      channel in evalControlByRun, and starts streaming SSE.
//   3. First SSE event is event:"run_id" carrying the generated id.
//   4. Handler calls runRealEvalCore with a StepCallback that, after
//      each iteration, emits event:"iteration" with the verdict +
//      running suggestions rollup + trajectory snapshot, then blocks
//      on the control channel (unless this is the final iteration,
//      in which case it returns immediately so the runner can finalize).
//   5. Client POSTs /api/eval-runs/:run_id/step with
//      {"action": "continue"|"stop"} to release the runner.
//   6. When the runner finishes, handler emits event:"done" carrying
//      the final EvalRun, then closes the stream.
//
// Batch callers (the existing synchronous /run and /preview routes)
// pass nil StepCallback and aren't affected — runRealEvalCore
// auto-continues exactly as before.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// StepDecision is the operator's per-iteration choice.
type StepDecision int

const (
	StepContinue StepDecision = iota
	StepStop
)

// IterationResult is what the runner emits to the step callback
// after each iteration's judge call.
//
// Final=true means the runner is about to break out of the loop
// (pass, max reached, strict violation, or judge had nothing
// actionable). The streaming callback returns immediately in that
// case so the final event:"done" can ship the persisted EvalRun.
type IterationResult struct {
	Iteration     int             `json:"iteration"`
	MaxIterations int             `json:"max_iterations"`
	Verdict       *JudgeVerdict   `json:"verdict,omitempty"`
	Suggestions   *RunSuggestions `json:"suggestions,omitempty"`
	Trajectory    Trajectory      `json:"trajectory"`
	Final         bool            `json:"final"`
}

// StepCallback is the per-iteration hook on the runner's hot path.
// nil = batch mode. Synchronous: the runner blocks until it returns.
type StepCallback func(IterationResult) StepDecision

// evalControlByRun maps run_id → that run's control channel. The
// streaming handler registers on entry, unregisters on exit; the
// step control endpoint POSTs an action into the channel.
var (
	evalControlMu    sync.RWMutex
	evalControlByRun = map[string]chan StepDecision{}
)

func registerEvalControl(runID string, ch chan StepDecision) {
	evalControlMu.Lock()
	defer evalControlMu.Unlock()
	evalControlByRun[runID] = ch
}
func unregisterEvalControl(runID string) {
	evalControlMu.Lock()
	defer evalControlMu.Unlock()
	delete(evalControlByRun, runID)
}
func lookupEvalControl(runID string) chan StepDecision {
	evalControlMu.RLock()
	defer evalControlMu.RUnlock()
	return evalControlByRun[runID]
}

func newEvalRunID() string {
	return fmt.Sprintf("er-%d", time.Now().UnixNano())
}

// sseWriter is the framing helper shared by both stream entrypoints.
// SSE frames are "event: <name>\ndata: <json>\n\n"; flush after each
// so the client's reader doesn't sit on a buffered payload.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported by ResponseWriter")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &sseWriter{w: w, flusher: flusher}, nil
}

func (s *sseWriter) Send(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// streamingStepCallback binds an SSE writer + control channel to the
// runner's per-iteration hook. ctx is the HTTP request context so a
// client disconnect (closed tab) cleanly breaks the loop instead of
// leaving the runner blocked forever on the control channel.
func streamingStepCallback(ctx context.Context, sse *sseWriter, control chan StepDecision) StepCallback {
	return func(it IterationResult) StepDecision {
		if err := sse.Send("iteration", it); err != nil {
			return StepStop
		}
		if it.Final {
			return StepContinue
		}
		select {
		case d := <-control:
			return d
		case <-ctx.Done():
			return StepStop
		}
	}
}

// handleEvalStepControl receives the operator's continue/stop choice
// for an in-flight streaming run. Path:
//
//	POST /api/eval-runs/:run_id/step  body: {"action": "continue"|"stop"}
//
// The run_id is generated inside this process and only handed back
// to the operator who opened the stream, so the id itself is the
// reference — no additional ownership check needed beyond the
// standard auth middleware on the route.
func (s *Server) handleEvalStepControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/eval-runs/"), "/")
	if len(parts) < 2 || parts[1] != "step" {
		http.NotFound(w, r)
		return
	}
	runID := parts[0]
	if runID == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var decision StepDecision
	switch body.Action {
	case "continue":
		decision = StepContinue
	case "stop":
		decision = StepStop
	default:
		http.Error(w, "action must be continue or stop", http.StatusBadRequest)
		return
	}
	ch := lookupEvalControl(runID)
	if ch == nil {
		http.Error(w, "no in-flight run with that id", http.StatusNotFound)
		return
	}
	select {
	case ch <- decision:
		writeJSON(w, map[string]string{"status": "accepted"})
	case <-time.After(5 * time.Second):
		http.Error(w, "runner not waiting on control", http.StatusConflict)
	case <-r.Context().Done():
		http.Error(w, "request cancelled", http.StatusRequestTimeout)
	}
}

// handleEvalRunStream is the agent-scoped streaming entrypoint:
//
//	POST /api/agents/:agentId/evals/:evalId/run/stream
//
// Body is the same RunOptions JSON the synchronous /run handler
// accepts. Dispatch is from handleAgentEvals so the agentId +
// evalId ownership checks are already done by the caller.
func (s *Server) handleEvalRunStream(w http.ResponseWriter, r *http.Request, userID int64, agent *Agent, ev *Eval) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	opts := RunOptions{MaxIterations: 5}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&opts)
	}
	s.runEvalSSE(w, r, userID, agent, ev, opts, false)
}

// handleEvalPreviewStream is the draft-agent streaming counterpart
// of handleEvalPreview. Same request body, same defaults; the
// transport is the only difference.
func (s *Server) handleEvalPreviewStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	var body struct {
		Directive string      `json:"directive"`
		Name      string      `json:"name"`
		ProjectID string      `json:"project_id"`
		Eval      Eval        `json:"eval"`
		Options   *RunOptions `json:"options,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Directive) == "" {
		http.Error(w, "directive required for preview", http.StatusBadRequest)
		return
	}
	if len(body.Eval.Goals) == 0 {
		http.Error(w, "at least one goal required", http.StatusBadRequest)
		return
	}
	if body.Eval.MaxTurns <= 0 {
		body.Eval.MaxTurns = 5
	}
	if body.Eval.Description == "" && body.Eval.Trigger.Type != "" {
		body.Eval.Description = triggerToText(body.Eval.Trigger)
	}
	if strings.TrimSpace(body.Eval.Description) == "" {
		http.Error(w, "description required for preview", http.StatusBadRequest)
		return
	}
	opts := RunOptions{MaxIterations: 5}
	if body.Options != nil {
		opts = *body.Options
		if opts.MaxIterations <= 0 {
			opts.MaxIterations = 5
		}
	}
	draft := &Agent{
		Name:      body.Name,
		Directive: body.Directive,
		ProjectID: body.ProjectID,
		UserID:    userID,
	}
	s.runEvalSSE(w, r, userID, draft, &body.Eval, opts, true)
}

// runEvalSSE is the shared body for the two stream entrypoints. Sets
// up SSE framing, generates a run id, registers the control channel,
// runs the eval with a streaming step callback, then ships the final
// EvalRun as event:"done".
//
// Timeout budget: each iteration's headroom is 90s of execution + 10
// minutes of operator pause. Cap so an abandoned tab can't pin a
// runner forever; generous so a backgrounded tab survives a coffee
// break.
func (s *Server) runEvalSSE(w http.ResponseWriter, r *http.Request, userID int64, agent *Agent, ev *Eval, opts RunOptions, preview bool) {
	sse, err := newSSEWriter(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 1
	}
	if opts.MaxIterations > 10 {
		opts.MaxIterations = 10
	}

	runID := newEvalRunID()
	control := make(chan StepDecision, 1)
	registerEvalControl(runID, control)
	defer unregisterEvalControl(runID)

	if err := sse.Send("run_id", map[string]string{"run_id": runID}); err != nil {
		return
	}

	budget := time.Duration(opts.MaxIterations) * (90*time.Second + 10*time.Minute)
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()

	stepCb := streamingStepCallback(ctx, sse, control)
	run, err := s.runRealEvalCore(ctx, userID, agent, ev, preview, opts, stepCb)
	if err != nil {
		_ = sse.Send("error", map[string]string{"error": err.Error()})
		return
	}
	_ = sse.Send("done", run)
}
