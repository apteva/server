package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sort"
	"time"
)

// A core receives provider credentials only as environment variables when it
// starts. Two things follow:
//
//   - Switching a running agent to a provider whose credentials it never
//     received cannot take effect live. Core reports that choice as
//     provider_unavailable instead of running on another provider, and the
//     server restarts the agent so it starts with them.
//   - Credentials saved after the start (a key replaced, a connection signed in
//     again or removed) don't reach the running core either. The agent is
//     marked as needing a restart; nothing restarts it on its own.

// Restart reasons reported on the agent and in config responses.
const (
	restartReasonProviderUnavailable = "provider_unavailable"
	restartReasonCredentialsChanged  = "provider_credentials_changed"
)

// coreProviderUnavailable is core's report that the agent's chosen provider
// cannot run in the current process.
type coreProviderUnavailable struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// rotatingProviderEnv are short-lived tokens the server refreshes by itself.
// Cores fetch the current token by provider id, so a new value doesn't leave
// the running core behind. Only their presence is compared.
var rotatingProviderEnv = map[string]bool{
	"OPENAI_CODEX_ACCESS_TOKEN": true,
	"GROK_BUILD_ACCESS_TOKEN":   true,
}

// providerEnvFingerprint records what a core was started with without keeping
// the credentials themselves.
func providerEnvFingerprint(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for name, value := range env {
		if rotatingProviderEnv[name] {
			out[name] = ""
			continue
		}
		sum := sha256.Sum256([]byte(value))
		out[name] = hex.EncodeToString(sum[:12])
	}
	return out
}

// staleProviderEnv lists the variables a core started with that are now
// different or gone. Variables added since don't matter to the running agent
// until it switches to them, and that switch restarts it.
func staleProviderEnv(started, current map[string]string) []string {
	var stale []string
	for name, fingerprint := range started {
		now, ok := current[name]
		if !ok || now != fingerprint {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	return stale
}

// The agent page reloads every few seconds; reading every saved credential
// that often isn't needed to notice a change.
var providerCredentialCheckTTL = 30 * time.Second

// credentialsChanged reports whether the credentials saved now differ from
// those the agent's core started with. A core adopted from an earlier server
// process has no record and is never reported.
func (im *AgentManager) credentialsChanged(id int64, compare func(started map[string]string) bool) bool {
	im.mu.RLock()
	ri := im.processes[id]
	im.mu.RUnlock()
	if ri == nil || !ri.isRunning() || ri.providerEnv == nil {
		return false
	}
	ri.runtimeMu.Lock()
	if !ri.credentialsAt.IsZero() && time.Since(ri.credentialsAt) < providerCredentialCheckTTL {
		stale := ri.credentialsStale
		ri.runtimeMu.Unlock()
		return stale
	}
	ri.runtimeMu.Unlock()
	stale := compare(ri.providerEnv)
	ri.runtimeMu.Lock()
	ri.credentialsAt, ri.credentialsStale = time.Now(), stale
	ri.runtimeMu.Unlock()
	return stale
}

// markProviderRestart reports, on an agent already enriched with its runtime
// state, whether it needs a restart to use its provider credentials. Reading
// the saved credentials is not free, so only the single-agent view asks.
func (s *Server) markProviderRestart(inst *Agent, checkCredentials bool) {
	if inst == nil || inst.Status != "running" {
		return
	}
	if inst.ProviderUnavailable != nil {
		inst.NeedsRestart = true
		inst.RestartReason = restartReasonProviderUnavailable
		return
	}
	if !checkCredentials {
		return
	}
	changed := s.agents.credentialsChanged(inst.ID, func(started map[string]string) bool {
		current, err := s.GetAllProviderEnvVars(inst.UserID, inst.ProjectID)
		if err != nil {
			log.Printf("[PROVIDERS] agent=%d compare credentials: %v", inst.ID, err)
			return false
		}
		return len(staleProviderEnv(started, providerEnvFingerprint(current))) > 0
	})
	if changed {
		inst.NeedsRestart = true
		inst.RestartReason = restartReasonCredentialsChanged
	}
}

// providerSwitchRestart restarts a running agent whose core just reported
// that its newly chosen provider cannot run. The caller must not hold the
// agent's config lock: the restart takes it.
func (s *Server) providerSwitchRestart(ctx context.Context, inst *Agent, unavailable *coreProviderUnavailable) map[string]any {
	log.Printf("[PROVIDERS] agent=%d restarting: provider %q needs credentials the running core doesn't have (%s)", inst.ID, unavailable.Name, unavailable.Reason)
	started := time.Now()
	restart := s.updateAgentCore
	if s.providerSwitchRestarter != nil {
		restart = s.providerSwitchRestarter
	}
	if err := restart(ctx, inst.ID); err != nil {
		log.Printf("[PROVIDERS] agent=%d restart for provider %q failed: %v", inst.ID, unavailable.Name, err)
		return map[string]any{"restarted": false, "restart_reason": restartReasonProviderUnavailable, "restart_error": err.Error()}
	}
	log.Printf("[PROVIDERS] agent=%d restarted on provider %q in %s", inst.ID, unavailable.Name, time.Since(started).Round(time.Millisecond))
	// The restarted core has the credentials; the report was about the old one.
	return map[string]any{"restarted": true, "restart_reason": restartReasonProviderUnavailable, "provider_unavailable": nil}
}
