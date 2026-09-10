package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const behaviorVersion = 3
const behaviorStart = "<!-- apteva:behavior:v1:start -->"
const behaviorEnd = "<!-- apteva:behavior:end -->"

var behaviorSection = regexp.MustCompile(`(?s)<!-- apteva:behavior:v[0-9]+:start -->.*?<!-- apteva:behavior:end -->`)

func validAgentMode(mode string) bool {
	return mode == "autonomous" || mode == "cautious" || mode == "learn"
}

func agentMode(mode string) string {
	if validAgentMode(mode) {
		return mode
	}
	return "autonomous"
}

// Only our delimited sections are replaced. Never infer ownership from prose
// or headings: those may be instructions written by the user or by evolve.
func withAgentBehavior(directive, mode string, proactivity ...int) string {
	var rule string
	switch agentMode(mode) {
	case "cautious":
		rule = "Cautious: you may inspect and perform read-only work within your assigned scope. Before an action that changes state or has external effects, explain the action, request approval through an available user communication channel, and wait for approval. If you cannot obtain approval, leave that action pending."
	case "learn":
		rule = "Learn: before using an unfamiliar combination of tool and scope, including read-only actions, explain what you intend to do and request approval through an available user communication channel. Wait for approval. You may reuse approval only for the same tool and approved scope when that approval is still available in your context; ask again if uncertain. This instruction does not provide a dedicated safety-profile memory store."
	default:
		rule = "Autonomous: proceed independently within the assigned scope and available permissions. Clarify material uncertainty and respect explicit approval requirements and user constraints."
	}
	section := behaviorStart + "\nServer-managed behavior instructions (" + agentMode(mode) + "). These are instructions, not enforced approval gates.\n" + rule + "\n" + agentProactivityInstructions(proactivityValue(proactivity)) + "\nWhen delegating, include these applicable behavior rules in each worker's directive. Delegation must not bypass approval requirements.\n" + behaviorEnd
	// Reuse the first section's position and remove any duplicate sections.
	first := true
	out := behaviorSection.ReplaceAllStringFunc(directive, func(string) string {
		if !first {
			return ""
		}
		first = false
		return section
	})
	if !first {
		return out
	}
	if directive == "" {
		return section
	}
	return directive + "\n\n" + section
}

func withoutCoreMode(config string) string {
	var cfg map[string]any
	if json.Unmarshal([]byte(config), &cfg) != nil || cfg == nil {
		return config
	}
	_, hasMode := cfg["mode"]
	_, hasProactivity := cfg["proactivity"]
	if !hasMode && !hasProactivity {
		return config
	}
	delete(cfg, "mode")
	delete(cfg, "proactivity")
	data, _ := json.Marshal(cfg)
	return string(data)
}

type agentBehaviorState struct {
	Revision        int64  `json:"revision"`
	MainRevision    int64  `json:"main_revision"`
	AppliedRevision int64  `json:"applied_revision"`
	Version         int    `json:"version"`
	Pending         bool   `json:"pending"`
	Error           string `json:"error,omitempty"`
}

func (s *Store) agentBehaviorState(id int64) (agentBehaviorState, error) {
	var state agentBehaviorState
	err := s.db.QueryRow(`SELECT revision,main_revision,applied_revision,version,last_error FROM agent_behavior_state WHERE agent_id=?`, id).Scan(&state.Revision, &state.MainRevision, &state.AppliedRevision, &state.Version, &state.Error)
	state.Pending = state.Version != behaviorVersion || state.AppliedRevision < state.Revision
	return state, err
}

func (s *Server) behaviorConfig(ctx context.Context, inst *Agent, port int) (map[string]any, error) {
	var cfg map[string]any
	if port > 0 {
		if err := s.behaviorCoreJSON(ctx, inst, http.MethodGet, "/config", nil, &cfg); err != nil {
			return nil, err
		}
	} else {
		data, err := os.ReadFile(filepath.Join(s.agents.instanceDir(inst.ID), "config.json"))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && json.Unmarshal(data, &cfg) != nil {
			return nil, fmt.Errorf("invalid saved agent config")
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

// Calls are bounded and cancelable. Reconciliation writes only the directive
// and the current MCP list (for compatibility with older PUT semantics).
func (s *Server) behaviorCoreJSON(ctx context.Context, inst *Agent, method, path string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	port := s.agents.GetPort(inst.ID)
	if port == 0 {
		return errInstanceNotRunning
	}
	resp, err := s.coreDoWithBootWaitContext(ctx, inst.ID, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), data, s.agents.GetCoreAPIKey(inst.ID), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<20))
	return err
}

func applyBehaviorToSavedWorkers(cfg map[string]any, mode string, proactivity ...int) {
	threads, _ := cfg["threads"].([]any)
	for _, entry := range threads {
		t, ok := entry.(map[string]any)
		if !ok || t["system"] == true || t["id"] == "main" || t["id"] == "unconscious" {
			continue
		}
		directive, _ := t["directive"].(string)
		t["directive"] = withAgentBehavior(directive, mode, proactivity...)
	}
}

// Caller holds lockAgentConfig. Pending desired state wins over disk; otherwise
// migrate the latest core-authored directive. This is also used before startup.
func (s *Server) reconcileAgentBehavior(ctx context.Context, inst *Agent, port int) (err error) {
	defer func() {
		if err != nil {
			_, _ = s.store.db.Exec(`UPDATE agent_behavior_state SET last_error=? WHERE agent_id=?`, err.Error(), inst.ID)
		}
	}()
	state, err := s.store.agentBehaviorState(inst.ID)
	if err != nil {
		return err
	}
	cfg, err := s.behaviorConfig(ctx, inst, port)
	if err != nil {
		return err
	}
	directive, ok := cfg["directive"].(string)
	if !ok || state.Revision > state.MainRevision {
		directive = inst.Directive
	}
	inst.Mode = agentMode(inst.Mode)
	inst.Directive = withAgentBehavior(directive, inst.Mode, inst.Proactivity)
	inst.Config = withoutCoreMode(inst.Config)
	if err = s.store.UpdateAgent(inst); err != nil {
		return err
	}
	// Even an already composed legacy row needs a first reconciliation.
	if _, err = s.store.db.Exec(`UPDATE agent_behavior_state SET revision=revision+1,version=? WHERE agent_id=? AND version<>?`, behaviorVersion, inst.ID, behaviorVersion); err != nil {
		return err
	}
	state, err = s.store.agentBehaviorState(inst.ID)
	if err != nil {
		return err
	}
	if port == 0 {
		err = s.writeStoppedConfigAtomic(inst.ID, func(saved map[string]any) error {
			delete(saved, "mode")
			delete(saved, "proactivity")
			saved["directive"] = inst.Directive
			applyBehaviorToSavedWorkers(saved, inst.Mode, inst.Proactivity)
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		if current, _ := cfg["directive"].(string); current != inst.Directive {
			mcp := cfg["mcp_servers"]
			if mcp == nil {
				mcp = []any{}
			}
			if err = s.behaviorCoreJSON(ctx, inst, http.MethodPut, "/config", map[string]any{"directive": inst.Directive, "mcp_servers": mcp}, nil); err != nil {
				return err
			}
		}
		if _, err = s.store.db.Exec(`UPDATE agent_behavior_state SET main_revision=? WHERE agent_id=? AND revision=?`, state.Revision, inst.ID, state.Revision); err != nil {
			return err
		}
		if err = s.applyBehaviorToLiveWorkers(ctx, inst); err != nil {
			return err
		}
	}
	_, err = s.store.db.Exec(`UPDATE agent_behavior_state SET main_revision=?,applied_revision=?,last_error='' WHERE agent_id=? AND revision=?`, state.Revision, state.Revision, inst.ID, state.Revision)
	return err
}

func (s *Server) applyBehaviorToLiveWorkers(ctx context.Context, inst *Agent) error {
	var threads []map[string]any
	if err := s.behaviorCoreJSON(ctx, inst, http.MethodGet, "/threads", nil, &threads); err != nil {
		return err
	}
	var failures []string
	for _, t := range threads {
		id, _ := t["id"].(string)
		if id == "" || id == "main" || id == "unconscious" || t["system"] == true {
			continue
		}
		directive, _ := t["directive"].(string)
		next := withAgentBehavior(directive, inst.Mode, inst.Proactivity)
		if next == directive {
			continue
		}
		// No tools, history, or restart_realtime changes. Core returns 409 if
		// an active voice session needs restart; keep it running and retry later.
		if err := s.behaviorCoreJSON(ctx, inst, http.MethodPut, "/threads/"+url.PathEscape(id), map[string]any{"directive": next}, nil); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("worker behavior pending: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (s *Server) behaviorMetadata(inst *Agent, out map[string]any) {
	out["mode"] = agentMode(inst.Mode)
	out["proactivity"] = inst.Proactivity
	if state, err := s.store.agentBehaviorState(inst.ID); err == nil {
		out["behavior_sync"] = state
	}
}

func (s *Server) writeBehaviorResponse(w http.ResponseWriter, resp *http.Response, inst *Agent) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		http.Error(w, "invalid core response", http.StatusBadGateway)
		return
	}
	var out map[string]any
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && json.Unmarshal(data, &out) == nil && out != nil {
		s.behaviorMetadata(inst, out)
		if _, isConfig := out["directive"]; isConfig {
			if state, err := s.store.agentBehaviorState(inst.ID); err == nil && state.Revision > state.MainRevision {
				out["directive"] = inst.Directive
			}
		}
		data, _ = json.Marshal(out)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.Header().Del("Content-Length")
	w.Header().Del("ETag")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

// Only pending agents are read. No polling of healthy cores or writes on the
// real-time event path. Startup/reattach use the same reconciliation directly.
func (s *Server) runAgentBehaviorReconciler(ctx context.Context) {
	run := func() {
		rows, err := s.store.db.Query(`SELECT agent_id FROM agent_behavior_state WHERE version<>? OR applied_revision<revision ORDER BY agent_id`, behaviorVersion)
		if err != nil {
			log.Printf("[BEHAVIOR] scan: %v", err)
			return
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			unlock := s.lockAgentConfig(id)
			inst, err := s.store.GetAgentByID(id)
			if err == nil {
				port := s.agents.GetPort(id)
				// A running row without a tracked core may be awaiting reattach.
				// Never overwrite a potentially live core's config file.
				if port > 0 || inst.Status == "stopped" {
					err = s.reconcileAgentBehavior(ctx, inst, port)
				}
			}
			unlock()
			if err != nil {
				log.Printf("[BEHAVIOR] agent=%d pending: %v", id, err)
			}
		}
	}
	run()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (s *Server) behaviorStatus(id int64) agentBehaviorState {
	state, err := s.store.agentBehaviorState(id)
	if err != nil {
		state.Pending = true
		state.Error = "behavior synchronization state unavailable"
	}
	return state
}
