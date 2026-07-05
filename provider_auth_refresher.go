package main

import (
	"context"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	providerAuthRefreshInterval = 5 * time.Minute
	codexProviderRefreshSkew    = 60 * time.Minute
	codexAgentRestartDelay      = 2 * time.Second
)

type codexProviderRefreshResult struct {
	ProvidersScanned   int
	ProvidersRefreshed int
	ProvidersFailed    int
	AgentsRestarted    int
}

type codexProviderRefresh struct {
	ProviderID int64
	UserID     int64
	ProjectID  string
}

var codexProviderRefreshInFlight atomic.Bool

// startProviderAuthRefresher owns platform-level provider refresh. It is not an
// app/job because provider auth must stay healthy even when apps are still
// mounting or unavailable.
func (s *Server) startProviderAuthRefresher(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	log.Printf("[PROVIDER-REFRESH] OpenAI Codex refresher enabled interval=%s skew=%s", providerAuthRefreshInterval, codexProviderRefreshSkew)
	go func() {
		s.refreshExpiringCodexProviders(ctx, codexProviderRefreshSkew, true)
		ticker := time.NewTicker(providerAuthRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshExpiringCodexProviders(ctx, codexProviderRefreshSkew, true)
			}
		}
	}()
}

func (s *Server) refreshExpiringCodexProviders(ctx context.Context, skew time.Duration, restartAgents bool) codexProviderRefreshResult {
	var out codexProviderRefreshResult
	if s == nil || s.store == nil {
		return out
	}
	if !codexProviderRefreshInFlight.CompareAndSwap(false, true) {
		log.Printf("[PROVIDER-REFRESH] OpenAI Codex refresh already running; skipping")
		return out
	}
	defer codexProviderRefreshInFlight.Store(false)

	refreshed, scanned, failed := s.refreshExpiringCodexProviderStates(ctx, skew)
	out.ProvidersScanned = scanned
	out.ProvidersRefreshed = len(refreshed)
	out.ProvidersFailed = failed
	if len(refreshed) == 0 || !restartAgents {
		return out
	}
	out.AgentsRestarted = s.restartAgentsUsingCodexProviders(refreshed)
	return out
}

func (s *Server) refreshExpiringCodexProviderStates(ctx context.Context, skew time.Duration) ([]codexProviderRefresh, int, int) {
	rows, err := s.store.db.Query(`
		SELECT p.id, p.user_id, COALESCE(p.project_id,''), p.encrypted_data
		  FROM providers p
		  JOIN provider_types pt ON pt.id = p.provider_type_id
		 WHERE COALESCE(p.status,'active') = 'active'
		   AND (COALESCE(pt.auth_provider,'') = ?
		        OR (p.type = 'llm' AND lower(p.name) = 'openai codex')
		        OR lower(p.type) = ?)
	`, openAICodexAuthProvider, openAICodexAuthProvider)
	if err != nil {
		log.Printf("[PROVIDER-REFRESH] list OpenAI Codex providers: %v", err)
		return nil, 0, 1
	}
	defer rows.Close()

	var refreshed []codexProviderRefresh
	scanned := 0
	failed := 0
	for rows.Next() {
		select {
		case <-ctx.Done():
			return refreshed, scanned, failed
		default:
		}
		var providerID, userID int64
		var projectID, encrypted string
		if err := rows.Scan(&providerID, &userID, &projectID, &encrypted); err != nil {
			failed++
			continue
		}
		scanned++
		changed, err := s.refreshOneCodexProvider(ctx, providerID, encrypted, skew)
		if err != nil {
			failed++
			log.Printf("[PROVIDER-REFRESH] OpenAI Codex provider=%d refresh failed: %v", providerID, err)
			continue
		}
		if changed {
			refreshed = append(refreshed, codexProviderRefresh{ProviderID: providerID, UserID: userID, ProjectID: projectID})
		}
	}
	if err := rows.Err(); err != nil {
		failed++
		log.Printf("[PROVIDER-REFRESH] scan OpenAI Codex providers: %v", err)
	}
	if scanned > 0 && (len(refreshed) > 0 || failed > 0) {
		log.Printf("[PROVIDER-REFRESH] OpenAI Codex scanned=%d refreshed=%d failed=%d", scanned, len(refreshed), failed)
	}
	return refreshed, scanned, failed
}

func (s *Server) refreshOneCodexProvider(ctx context.Context, providerID int64, encrypted string, skew time.Duration) (bool, error) {
	_, changed, err := s.store.RefreshOpenAICodexProviderState(providerID, 0, s.secret, skew, false, "server_background_refresh")
	if changed {
		log.Printf("[PROVIDER-REFRESH] OpenAI Codex provider=%d refreshed", providerID)
	}
	return changed, err
}

func (s *Server) restartAgentsUsingCodexProviders(refreshed []codexProviderRefresh) int {
	if len(refreshed) == 0 || s == nil || s.store == nil || s.agents == nil {
		return 0
	}
	rows, err := s.store.ListAgentsByStatus("running")
	if err != nil {
		log.Printf("[PROVIDER-REFRESH] list running agents for restart: %v", err)
		return 0
	}
	restarted := 0
	seen := map[int64]bool{}
	for i := range rows {
		inst := &rows[i]
		if seen[inst.ID] || (inst.Kind != "" && inst.Kind != "user") {
			continue
		}
		if !agentUsesRefreshedCodexProvider(inst, refreshed) {
			continue
		}
		seen[inst.ID] = true
		if s.restartAgentAfterProviderRefresh(inst) {
			restarted++
			time.Sleep(codexAgentRestartDelay)
		}
	}
	if restarted > 0 {
		log.Printf("[PROVIDER-REFRESH] restarted %d running agent(s) after OpenAI Codex refresh", restarted)
	}
	return restarted
}

func agentUsesRefreshedCodexProvider(inst *Agent, refreshed []codexProviderRefresh) bool {
	if inst == nil {
		return false
	}
	for _, p := range refreshed {
		if p.UserID != inst.UserID {
			continue
		}
		if p.ProjectID == "" || p.ProjectID == inst.ProjectID {
			return true
		}
	}
	return false
}

func (s *Server) restartAgentAfterProviderRefresh(inst *Agent) bool {
	log.Printf("[PROVIDER-REFRESH] restarting agent=%d after OpenAI Codex token refresh", inst.ID)
	s.agents.Stop(inst.ID)
	providerEnv, err := s.store.GetAllProviderEnvVars(inst.UserID, s.secret, inst.ProjectID)
	if err != nil {
		log.Printf("[PROVIDER-REFRESH] agent=%d restart skipped: provider env failed: %v", inst.ID, err)
		return false
	}
	pool := s.GetProviderPool(inst.UserID, inst.ProjectID)
	if len(pool) == 0 {
		log.Printf("[PROVIDER-REFRESH] agent=%d restart skipped: no provider pool", inst.ID)
		return false
	}
	if err := s.agents.Start(inst, providerEnv, s.port, pool, s.instanceSecret, s.loadChannelConfigs(inst.ID)...); err != nil {
		if strings.Contains(err.Error(), "already running") {
			log.Printf("[PROVIDER-REFRESH] agent=%d already running during restart", inst.ID)
			return false
		}
		log.Printf("[PROVIDER-REFRESH] agent=%d restart failed: %v", inst.ID, err)
		return false
	}
	s.store.UpdateAgent(inst)
	s.restoreSlackForInstance(inst)
	s.restoreEmailForInstance(inst)
	s.notifyAgentSubscriptionStartup(inst)
	return true
}

func (s *Store) RunningAgentsUseCodexProvider() bool {
	if s == nil || s.db == nil {
		return false
	}
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(1)
		  FROM agents a
		 WHERE a.status = 'running'
		   AND COALESCE(a.kind,'user') = 'user'
		   AND EXISTS (
		       SELECT 1
		         FROM providers p
		         JOIN provider_types pt ON pt.id = p.provider_type_id
		        WHERE p.user_id = a.user_id
		          AND (COALESCE(p.project_id,'') = '' OR COALESCE(p.project_id,'') = COALESCE(a.project_id,''))
		          AND (COALESCE(pt.auth_provider,'') = ?
		               OR (p.type = 'llm' AND lower(p.name) = 'openai codex')
		               OR lower(p.type) = ?)
		   )
	`, openAICodexAuthProvider, openAICodexAuthProvider).Scan(&count)
	return err == nil && count > 0
}

func providerAuthRefreshEnvEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("APTEVA_PROVIDER_AUTH_REFRESH"))
	if raw == "" {
		raw = "1"
	}
	switch strings.ToLower(raw) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func disableCoreReattachForCodexRefresh() bool {
	raw := strings.TrimSpace(os.Getenv("APTEVA_CODEX_REFRESH_DISABLE_REATTACH"))
	if raw == "" {
		raw = "1"
	}
	switch strings.ToLower(raw) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
