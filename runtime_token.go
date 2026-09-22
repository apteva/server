package main

// runtime_token.go — keeps /api/providers/:id/auth/runtime-token working
// after the providers table goes away.
//
// apteva-core builds this URL itself, from an env var we hand it at spawn:
//
//	SERVER_URL + "/api/providers/" + OPENAI_CODEX_PROVIDER_ID + "/auth/runtime-token"
//
// (core/provider_openai_native.go). Core calls it before a request and
// force-calls it on 401/403, which is why the server never had to push
// refreshed tokens into a running agent. Two consequences for the
// providers/connections fusion:
//
//   1. The route can never be deleted, whatever happens to the providers
//      table — cores in the field will keep calling it.
//   2. Cores spawned before the migration hold the OLD providers.id in
//      their environment for the lifetime of the process. So the id in
//      the URL has to resolve as either a provider row or a migrated
//      connection, and it stays that way until every such core restarts.
//
// resolveRuntimeTokenConnection handles (2) via connections.legacy_provider_id.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// handleRuntimeToken serves the token core asks for, from whichever
// store still owns the credential.
//
// A migrated connection owns its legacy provider ID even while the old row
// remains. Interactive reauth updates the connection, so consulting the old
// provider first would restore stale credentials on the next core request.
// Unmigrated providers still take precedence over unrelated connection IDs.
func (s *Server) handleRuntimeToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idPart := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/providers/"), "/auth/runtime-token")
	id, err := atoi64(strings.Trim(idPart, "/"))
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}

	migratedID, err := s.store.connectionForLegacyProvider(userID, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "could not resolve provider migration", http.StatusInternalServerError)
		return
	}
	conn, ok := s.resolveRuntimeTokenConnection(userID, id)
	if err == nil {
		// An inactive or unavailable migrated connection must not fall back to
		// the legacy credential, or to an unrelated connection with this ID.
		if !ok || conn.ID != migratedID || conn.LegacyProviderID != id {
			http.Error(w, "provider not found", http.StatusNotFound)
			return
		}
		s.writeConnectionRuntimeToken(w, conn, r.URL.Query().Get("force") == "1")
		return
	}

	if _, _, err := s.store.GetProvider(userID, id); err == nil {
		s.handleProviderAuthAction(w, r, "runtime-token")
		return
	}

	if !ok {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}
	s.writeConnectionRuntimeToken(w, conn, r.URL.Query().Get("force") == "1")
}

// resolveRuntimeTokenConnection finds the connection that owns the
// credential a given id refers to. Tries legacy_provider_id first: a
// migrated row answers to the id the core in the field already holds,
// and only then to its own.
func (s *Server) resolveRuntimeTokenConnection(userID, id int64) (runtimeConnection, bool) {
	conns, err := s.store.ListRuntimeConnections(userID)
	if err != nil {
		return runtimeConnection{}, false
	}
	var byOwnID *runtimeConnection
	for i := range conns {
		if s.runtimeAppFor(conns[i]) == nil {
			continue
		}
		if conns[i].LegacyProviderID == id {
			return conns[i], true
		}
		if conns[i].ID == id && byOwnID == nil {
			byOwnID = &conns[i]
		}
	}
	if byOwnID != nil {
		return *byOwnID, true
	}
	return runtimeConnection{}, false
}

// writeConnectionRuntimeToken refreshes session credentials if needed and
// returns the provider driver's runtime payload. The OpenAI Codex driver keeps
// the exact legacy shape core parses ({access_token, account_id}); newer
// session providers can add metadata without changing that compatibility path.
func (s *Server) writeConnectionRuntimeToken(w http.ResponseWriter, conn runtimeConnection, force bool) {
	plaintext, err := Decrypt(s.secret, conn.EncryptedCreds)
	if err != nil {
		http.Error(w, "could not read connection credentials", http.StatusInternalServerError)
		return
	}
	credentials := map[string]string{}
	if err := json.Unmarshal([]byte(plaintext), &credentials); err != nil {
		http.Error(w, "could not decode connection credentials", http.StatusInternalServerError)
		return
	}

	driver := connectionSessionAuthDriverFor(conn.AppSlug)
	if driver == nil {
		http.Error(w, "provider does not use session authentication", http.StatusBadRequest)
		return
	}
	if force || driver.NeedsRefresh(credentials, 10*time.Minute) {
		if err := s.refreshConnectionCredentials(conn.ID, credentials, func(current map[string]string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return driver.Refresh(ctx, current)
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	payload, err := driver.RuntimeToken(credentials)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, payload)
}
