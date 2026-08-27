package main

import (
	"context"
	"encoding/json"
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
	ProvidersScanned     int
	ProvidersRefreshed   int
	ProvidersFailed      int
	ConnectionsScanned   int
	ConnectionsRefreshed int
	ConnectionsFailed    int
	AgentsRestarted      int
}

type codexProviderRefresh struct {
	ProviderID   int64
	ConnectionID int64
	UserID       int64
	ProjectID    string
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

	providerRefreshes, providerScanned, providerFailed := s.refreshExpiringCodexProviderStates(ctx, skew)
	connectionRefreshes, connectionScanned, connectionFailed := s.refreshExpiringCodexConnectionStates(ctx, skew)
	refreshed := append(providerRefreshes, connectionRefreshes...)
	out.ProvidersScanned = providerScanned
	out.ProvidersRefreshed = len(providerRefreshes)
	out.ProvidersFailed = providerFailed
	out.ConnectionsScanned = connectionScanned
	out.ConnectionsRefreshed = len(connectionRefreshes)
	out.ConnectionsFailed = connectionFailed
	if len(refreshed) == 0 || !restartAgents {
		return out
	}
	out.AgentsRestarted = s.restartAgentsUsingCodexProviders(refreshed)
	return out
}

// refreshExpiringCodexConnectionStates is the Connections-era counterpart to
// the legacy providers-table refresher above. It uses a compare-and-swap write
// so a concurrent interactive re-auth always wins over a background refresh
// that started from older credentials.
func (s *Server) refreshExpiringCodexConnectionStates(ctx context.Context, skew time.Duration) ([]codexProviderRefresh, int, int) {
	type candidate struct {
		id        int64
		userID    int64
		projectID string
		encrypted string
	}
	rows, err := s.store.db.Query(`
		SELECT id, user_id, COALESCE(project_id,''), encrypted_credentials
		  FROM connections
		 WHERE COALESCE(status,'active') = 'active'
		   AND COALESCE(source,'local') = 'local'
		   AND app_slug = ?
		   AND auth_type = ?
	`, integrationOpenAICodexSlug, connectionAuthTypeDeviceCode)
	if err != nil {
		log.Printf("[PROVIDER-REFRESH] list OpenAI Codex connections: %v", err)
		return nil, 0, 1
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.userID, &item.projectID, &item.encrypted); err != nil {
			continue
		}
		candidates = append(candidates, item)
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil {
		log.Printf("[PROVIDER-REFRESH] scan OpenAI Codex connections: %v", rowsErr)
		return nil, len(candidates), 1
	}

	refreshed := make([]codexProviderRefresh, 0, len(candidates))
	failed := 0
	for _, item := range candidates {
		select {
		case <-ctx.Done():
			return refreshed, len(candidates), failed
		default:
		}
		changed, err := s.refreshOneCodexConnection(item.id, item.encrypted, skew)
		if err != nil {
			failed++
			log.Printf("[PROVIDER-REFRESH] OpenAI Codex connection=%d refresh failed: %v", item.id, err)
			continue
		}
		if changed {
			refreshed = append(refreshed, codexProviderRefresh{
				ConnectionID: item.id,
				UserID:       item.userID,
				ProjectID:    item.projectID,
			})
		}
	}
	if len(candidates) > 0 && (len(refreshed) > 0 || failed > 0) {
		log.Printf("[PROVIDER-REFRESH] OpenAI Codex connections scanned=%d refreshed=%d failed=%d", len(candidates), len(refreshed), failed)
	}
	return refreshed, len(candidates), failed
}

func (s *Server) refreshOneCodexConnection(connectionID int64, expectedEncrypted string, skew time.Duration) (bool, error) {
	plain, err := Decrypt(s.secret, expectedEncrypted)
	if err != nil {
		return false, err
	}
	credentials := map[string]string{}
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		return false, err
	}
	if !connectionOpenAICodexNeedsRefresh(credentials, skew) {
		return false, nil
	}
	if err := refreshIntegrationOpenAICodexCredentials(credentials); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return false, err
	}
	reEncrypted, err := Encrypt(s.secret, string(encoded))
	if err != nil {
		return false, err
	}
	result, err := s.store.db.Exec(`
		UPDATE connections
		   SET encrypted_credentials = ?
		 WHERE id = ? AND encrypted_credentials = ?`, reEncrypted, connectionID, expectedEncrypted)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if updated == 0 {
		// Interactive re-auth or another refresher replaced the row while the
		// network request was in flight. Preserve that newer credential.
		return false, nil
	}
	log.Printf("[PROVIDER-REFRESH] OpenAI Codex connection=%d refreshed", connectionID)
	return true, nil
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
	if err := s.updateAgentCore(context.Background(), inst.ID); err != nil {
		log.Printf("[PROVIDER-REFRESH] agent=%d restart failed: %v", inst.ID, err)
		return false
	}
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
		   AND (EXISTS (
		       SELECT 1
		         FROM providers p
		         JOIN provider_types pt ON pt.id = p.provider_type_id
		        WHERE p.user_id = a.user_id
		          AND (COALESCE(p.project_id,'') = '' OR COALESCE(p.project_id,'') = COALESCE(a.project_id,''))
		          AND (COALESCE(pt.auth_provider,'') = ?
		               OR (p.type = 'llm' AND lower(p.name) = 'openai codex')
		               OR lower(p.type) = ?)
		   ) OR EXISTS (
		       SELECT 1
		         FROM connections c
		        WHERE c.user_id = a.user_id
		          AND COALESCE(c.status,'active') = 'active'
		          AND COALESCE(c.source,'local') = 'local'
		          AND c.app_slug = ?
		          AND c.auth_type = ?
		          AND (COALESCE(c.project_id,'') = '' OR COALESCE(c.project_id,'') = COALESCE(a.project_id,''))
		   ))
	`, openAICodexAuthProvider, openAICodexAuthProvider, integrationOpenAICodexSlug, connectionAuthTypeDeviceCode).Scan(&count)
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
