package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const agentLifecycleIntentFile = "shutdown-intent.json"

type agentLifecycleIntent struct {
	Reason    string    `json:"reason"`
	Policy    string    `json:"agent_policy,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) lifecycleIntentPath() string {
	return filepath.Join(s.dataDir, agentLifecycleIntentFile)
}

func (s *Server) readLifecycleIntent(remove bool) agentLifecycleIntent {
	path := s.lifecycleIntentPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return agentLifecycleIntent{}
	}
	var intent agentLifecycleIntent
	if json.Unmarshal(raw, &intent) != nil || intent.ExpiresAt.IsZero() || time.Now().After(intent.ExpiresAt) {
		_ = os.Remove(path)
		return agentLifecycleIntent{}
	}
	if remove {
		_ = os.Remove(path)
	}
	intent.Reason = strings.ToLower(strings.TrimSpace(intent.Reason))
	if strings.TrimSpace(intent.Policy) != "" {
		intent.Policy = normalizeAgentUpdatePolicy(intent.Policy)
	}
	return intent
}

func normalizeAgentUpdatePolicy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "preserve", "detach", "reattach":
		return "preserve"
	case "rolling", "roll":
		return "rolling"
	case "restart", "stop", "respawn", "":
		return "restart"
	default:
		return ""
	}
}

func (s *Server) agentUpdatePolicy() string {
	raw := strings.TrimSpace(os.Getenv("APTEVA_AGENT_UPDATE_POLICY"))
	if raw == "" && s != nil && s.store != nil {
		raw = strings.TrimSpace(s.store.GetSetting("agent_update_policy"))
	}
	if raw != "" {
		if policy := normalizeAgentUpdatePolicy(raw); policy != "" {
			return policy
		}
		log.Printf("[LIFECYCLE] unknown agent update policy %q; using restart", raw)
		return "restart"
	}
	// Preserve compatibility with the original detach switch.
	if s.agentShutdownPolicy() == "detach" {
		return "preserve"
	}
	return "restart"
}

func (s *Server) resolvedShutdownPolicy(intent agentLifecycleIntent) string {
	// An undeclared signal is treated as a real shutdown. This keeps Ctrl+C,
	// service stops, and supervisor failures from silently orphaning cores.
	if intent.Reason != "restart" && intent.Reason != "update" {
		return "restart"
	}
	if intent.Policy != "" {
		return intent.Policy
	}
	return s.agentUpdatePolicy()
}

func (s *Server) agentRolloutDelay() time.Duration {
	raw := strings.TrimSpace(os.Getenv("APTEVA_AGENT_ROLLOUT_DELAY"))
	if raw == "" && s != nil && s.store != nil {
		raw = strings.TrimSpace(s.store.GetSetting("agent_rollout_delay"))
	}
	if raw == "" {
		return 15 * time.Second
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	return 15 * time.Second
}

type agentRolloutStatus struct {
	ID            string            `json:"id,omitempty"`
	State         string            `json:"state"`
	Scope         string            `json:"scope,omitempty"`
	ProjectID     string            `json:"project_id,omitempty"`
	Total         int               `json:"total"`
	Completed     int               `json:"completed"`
	Failed        int               `json:"failed"`
	CurrentAgent  int64             `json:"current_agent_id,omitempty"`
	CurrentName   string            `json:"current_agent_name,omitempty"`
	DelaySeconds  int               `json:"delay_seconds"`
	Errors        map[string]string `json:"errors,omitempty"`
	StartedAt     *time.Time        `json:"started_at,omitempty"`
	FinishedAt    *time.Time        `json:"finished_at,omitempty"`
	TargetVersion string            `json:"target_core_version,omitempty"`
	agentIDs      []int64
}

type agentRolloutCoordinator struct {
	mu     sync.RWMutex
	status agentRolloutStatus
	cancel context.CancelFunc
	runOne func(context.Context, int64) error
}

func newAgentRolloutCoordinator(runOne func(context.Context, int64) error) *agentRolloutCoordinator {
	return &agentRolloutCoordinator{status: agentRolloutStatus{State: "idle"}, runOne: runOne}
}

func (r *agentRolloutCoordinator) snapshot() agentRolloutStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := r.status
	if r.status.Errors != nil {
		out.Errors = make(map[string]string, len(r.status.Errors))
		for key, value := range r.status.Errors {
			out.Errors[key] = value
		}
	}
	return out
}

func (r *agentRolloutCoordinator) start(ids []int64, names map[int64]string, scope, projectID, target string, delay time.Duration) (agentRolloutStatus, error) {
	if len(ids) == 0 {
		return r.snapshot(), errors.New("no running agents require a core update")
	}
	r.mu.Lock()
	if r.status.State == "running" {
		r.mu.Unlock()
		return agentRolloutStatus{}, errors.New("a core rollout is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	r.cancel = cancel
	r.status = agentRolloutStatus{
		ID: fmt.Sprintf("rollout-%d", now.UnixNano()), State: "running", Scope: scope,
		ProjectID: projectID, Total: len(ids), DelaySeconds: int(delay.Seconds()),
		Errors: map[string]string{}, StartedAt: &now, TargetVersion: target,
		agentIDs: append([]int64(nil), ids...),
	}
	initial := r.status
	r.mu.Unlock()

	go func() {
		for index, id := range ids {
			select {
			case <-ctx.Done():
				r.finish("cancelled")
				return
			default:
			}
			r.mu.Lock()
			r.status.CurrentAgent = id
			r.status.CurrentName = names[id]
			r.mu.Unlock()
			if err := r.runOne(ctx, id); err != nil {
				r.mu.Lock()
				r.status.Failed++
				r.status.Errors[strconv.FormatInt(id, 10)] = err.Error()
				r.mu.Unlock()
			} else {
				r.mu.Lock()
				r.status.Completed++
				r.mu.Unlock()
			}
			if index < len(ids)-1 && delay > 0 {
				select {
				case <-ctx.Done():
					r.finish("cancelled")
					return
				case <-time.After(delay):
				}
			}
		}
		r.finish("completed")
	}()
	return initial, nil
}

func (r *agentRolloutCoordinator) finish(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	r.status.State = state
	r.status.CurrentAgent = 0
	r.status.CurrentName = ""
	r.status.FinishedAt = &now
	r.cancel = nil
}

func (r *agentRolloutCoordinator) stop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.State != "running" || r.cancel == nil {
		return false
	}
	r.cancel()
	return true
}

func (s *Server) updateAgentCore(ctx context.Context, agentID int64) error {
	inst, err := s.store.GetAgentByID(agentID)
	if err != nil {
		return fmt.Errorf("agent not found: %w", err)
	}
	if !s.agents.IsRunning(agentID) {
		return errors.New("agent is not running")
	}
	providerEnv, err := s.store.GetAllProviderEnvVars(inst.UserID, s.secret, inst.ProjectID)
	if err != nil {
		return fmt.Errorf("provider environment: %w", err)
	}
	pool := s.GetProviderPool(inst.UserID, inst.ProjectID)
	if len(pool) == 0 {
		return errors.New("no LLM provider configured")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	// Once replacement starts it is atomic from the rollout's perspective:
	// cancellation prevents the next agent, but never strands this one between
	// Stop and Start.
	s.agents.Stop(inst.ID)
	info, err := s.startManagedAgent(inst, providerEnv, pool, s.loadChannelConfigs(inst.ID)...)
	if err != nil {
		return fmt.Errorf("start updated core: %w", err)
	}
	_ = s.store.UpdateAgent(inst)
	s.restoreSlackForInstance(inst)
	s.restoreEmailForInstance(inst)
	s.notifyAgentSubscriptionStartup(inst)

	if CoreVersion != "dev" && info.Version != "" && info.Version != CoreVersion {
		return fmt.Errorf("updated core reported version %s, target is %s", info.Version, CoreVersion)
	}
	return nil
}

func (s *Server) waitForAgentCoreHealthy(ctx context.Context, agentID int64, timeout time.Duration) (coreRuntimeInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return coreRuntimeInfo{}, ctx.Err()
		default:
		}
		if info, ok := s.agents.CoreRuntimeInfo(agentID); ok {
			return info, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return coreRuntimeInfo{}, fmt.Errorf("agent %d core did not become healthy within %s", agentID, timeout)
}

const agentRuntimeStartupTimeout = 30 * time.Second

func coreRuntimeStartedAt(info coreRuntimeInfo) time.Time {
	startedAt := time.Now().UTC()
	if info.UptimeSeconds > 0 {
		startedAt = startedAt.Add(-time.Duration(info.UptimeSeconds) * time.Second)
	}
	return startedAt
}

// syncAgentRuntime makes process activation durable only after the core's
// authenticated status endpoint reports its real build metadata. The store
// writes the complete runtime snapshot atomically.
func (s *Server) syncAgentRuntime(inst *Agent, timeout time.Duration, requireUptime bool) (coreRuntimeInfo, error) {
	if inst == nil {
		return coreRuntimeInfo{}, errors.New("agent is nil")
	}
	info, err := s.waitForAgentCoreHealthy(context.Background(), inst.ID, timeout)
	if err != nil {
		return coreRuntimeInfo{}, err
	}
	if strings.TrimSpace(info.Version) == "" {
		return coreRuntimeInfo{}, fmt.Errorf("agent %d core status omitted core_version", inst.ID)
	}
	if requireUptime && info.UptimeSeconds <= 0 {
		return coreRuntimeInfo{}, fmt.Errorf("agent %d adopted core status omitted uptime", inst.ID)
	}
	inst.CoreVersion = info.Version
	inst.CoreBuildTime = info.BuildTime
	if err := s.store.SetAgentRuntimeRunning(inst, coreRuntimeStartedAt(info)); err != nil {
		return coreRuntimeInfo{}, fmt.Errorf("persist agent %d runtime: %w", inst.ID, err)
	}
	return info, nil
}

func (s *Server) abandonUnsyncedAgentRuntime(inst *Agent) {
	if inst == nil {
		return
	}
	s.agents.Stop(inst.ID)
	if err := s.store.SetAgentRuntimeStopped(inst); err != nil {
		log.Printf("[RUNTIME] clear unsynced agent=%d: %v", inst.ID, err)
	}
}

// startManagedAgent is the only server-owned path for launching a core. It
// does not report success while the process exists only in memory.
func (s *Server) ensureAgentDefaultProvider(inst *Agent, pool []ProviderInfo) (string, error) {
	configuredDefault := configuredAgentDefaultProvider(inst.Config)
	effectiveDefault := effectiveProviderDefault(pool, configuredDefault)
	if effectiveDefault == "" {
		return "", errors.New("no LLM provider configured")
	}
	if configuredDefault != effectiveDefault {
		var config map[string]any
		if strings.TrimSpace(inst.Config) != "" {
			if err := json.Unmarshal([]byte(inst.Config), &config); err != nil {
				return "", fmt.Errorf("decode agent provider config: %w", err)
			}
		}
		if config == nil {
			config = map[string]any{}
		}
		config["default_provider"] = effectiveDefault
		encoded, err := json.Marshal(config)
		if err != nil {
			return "", fmt.Errorf("encode agent provider config: %w", err)
		}
		inst.Config = string(encoded)
		if err := s.store.UpdateAgent(inst); err != nil {
			return "", fmt.Errorf("persist agent default provider: %w", err)
		}
		log.Printf("[PROVIDERS] agent=%d default=%s previous=%q", inst.ID, effectiveDefault, configuredDefault)
	}
	return effectiveDefault, nil
}

func (s *Server) startManagedAgent(inst *Agent, providerEnv map[string]string, pool []ProviderInfo, channelConfigs ...ChannelConfig) (coreRuntimeInfo, error) {
	if _, err := s.ensureAgentDefaultProvider(inst, pool); err != nil {
		return coreRuntimeInfo{}, err
	}
	if err := s.agents.Start(inst, providerEnv, s.port, pool, s.instanceSecret, channelConfigs...); err != nil {
		return coreRuntimeInfo{}, err
	}
	info, err := s.syncAgentRuntime(inst, agentRuntimeStartupTimeout, false)
	if err != nil {
		s.abandonUnsyncedAgentRuntime(inst)
		return coreRuntimeInfo{}, err
	}
	return info, nil
}

// reattachManagedAgent applies the same durable activation contract when a
// replacement server adopts a core that survived the old server process.
func (s *Server) reattachManagedAgent(inst *Agent, channelConfigs ...ChannelConfig) (coreRuntimeInfo, error) {
	if err := s.agents.Reattach(inst, s.port, channelConfigs...); err != nil {
		return coreRuntimeInfo{}, err
	}
	info, err := s.syncAgentRuntime(inst, agentRuntimeStartupTimeout, true)
	if err != nil {
		s.abandonUnsyncedAgentRuntime(inst)
		return coreRuntimeInfo{}, err
	}
	return info, nil
}

func (s *Server) rolloutCandidates(projectID string, all bool, ids []int64) ([]int64, map[int64]string, error) {
	rows, err := s.store.ListAgentsByStatus("running")
	if err != nil {
		return nil, nil, err
	}
	wanted := map[int64]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []int64
	names := map[int64]string{}
	for i := range rows {
		agent := &rows[i]
		if agent.Kind != "" && agent.Kind != "user" {
			continue
		}
		if len(wanted) > 0 && !wanted[agent.ID] {
			continue
		}
		if len(wanted) == 0 && !all && projectID != "" && agent.ProjectID != projectID {
			continue
		}
		s.enrichAgentRuntime(agent)
		if len(wanted) == 0 && !agent.CoreUpdateAvailable {
			continue
		}
		out = append(out, agent.ID)
		names[agent.ID] = agent.Name
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, names, nil
}

func (s *Server) startCoreRollout(ids []int64, projectID, scope string, all bool, delay time.Duration) (agentRolloutStatus, error) {
	candidates, names, err := s.rolloutCandidates(projectID, all, ids)
	if err != nil {
		return agentRolloutStatus{}, err
	}
	return s.agentRollouts.start(candidates, names, scope, projectID, CoreVersion, delay)
}

func (s *Server) handleCoreRollout(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		status := s.agentRollouts.snapshot()
		if !s.authorizeCoreRollout(w, r, status, ProjectViewer) {
			return
		}
		writeJSON(w, status)
	case http.MethodDelete:
		if !s.authorizeCoreRollout(w, r, s.agentRollouts.snapshot(), ProjectEditor) {
			return
		}
		writeJSON(w, map[string]bool{"cancel_requested": s.agentRollouts.stop()})
	case http.MethodPost:
		var body struct {
			ProjectID    string  `json:"project_id"`
			All          bool    `json:"all"`
			AgentIDs     []int64 `json:"agent_ids"`
			DelaySeconds *int    `json:"delay_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body.All {
			if !s.isAdmin(getUserID(r)) {
				http.Error(w, "admin access required", http.StatusForbidden)
				return
			}
		} else if body.ProjectID != "" {
			if _, _, ok := s.requireProjectAccess(w, r, body.ProjectID, ProjectEditor); !ok {
				return
			}
		} else if len(body.AgentIDs) > 0 {
			for _, agentID := range body.AgentIDs {
				if _, ok := s.requireAgentAccess(w, r, agentID, ProjectEditor); !ok {
					return
				}
			}
		} else if len(body.AgentIDs) == 0 {
			http.Error(w, "project_id, all, or agent_ids is required", http.StatusBadRequest)
			return
		}
		delay := s.agentRolloutDelay()
		if body.DelaySeconds != nil {
			if *body.DelaySeconds < 0 || *body.DelaySeconds > 3600 {
				http.Error(w, "delay_seconds must be between 0 and 3600", http.StatusBadRequest)
				return
			}
			delay = time.Duration(*body.DelaySeconds) * time.Second
		}
		status, err := s.startCoreRollout(body.AgentIDs, body.ProjectID, agentRolloutScope(body.All, body.AgentIDs), body.All, delay)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, status)
	default:
		http.Error(w, "GET, POST, or DELETE", http.StatusMethodNotAllowed)
	}
}

func agentRolloutScope(all bool, agentIDs []int64) string {
	if all {
		return "all"
	}
	if len(agentIDs) == 1 {
		return "agent"
	}
	if len(agentIDs) > 1 {
		return "agents"
	}
	return "project"
}

func (s *Server) authorizeCoreRollout(w http.ResponseWriter, r *http.Request, status agentRolloutStatus, need ProjectRole) bool {
	if status.State == "idle" || status.ID == "" {
		return true
	}
	if status.Scope == "all" {
		if !s.isAdmin(getUserID(r)) {
			http.Error(w, "admin access required", http.StatusForbidden)
			return false
		}
		return true
	}
	if status.ProjectID != "" {
		_, _, ok := s.requireProjectAccess(w, r, status.ProjectID, need)
		return ok
	}
	if len(status.agentIDs) > 0 {
		_, ok := s.requireAgentAccess(w, r, status.agentIDs[0], need)
		return ok
	}
	http.Error(w, "rollout access unavailable", http.StatusForbidden)
	return false
}

func (s *Server) handleAgentCoreUpdate(w http.ResponseWriter, r *http.Request, agentID int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	status, err := s.startCoreRollout([]int64{agentID}, "", "agent", false, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, status)
}
