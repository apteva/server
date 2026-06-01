package main

// connections_scope.go — `PATCH /api/connections/:id/scope` moves a
// connection between project and global scope.
//
// Symmetry note: v0.14.5 introduced the same shape for app installs
// (server/apps_scope.go). This is the connection-side mirror so an
// operator can have a global storage install bound to a global R2
// connection — no hidden cross-project dependencies. Without this,
// you could globalise an install but its bound connections stayed
// trapped in the project they were minted in, which surfaces as
// "global app, but the credentials belong to project X" confusion.
//
// What changes vs. what's left alone:
//
//   CHANGE  connections.project_id          (source of truth)
//   CHANGE  mcp_servers.project_id WHERE
//             connection_id = <id>          (auto-MCP follows its
//                                            owning connection)
//
//   LEAVE   integration_bindings JSON       (references connection_id;
//                                            cross-app calls work)
//   LEAVE   encrypted_credentials           (the secret material
//                                            itself is scope-agnostic)
//   LEAVE   subscriptions, webhook routes   (project-scoped routing
//                                            stays — even on a global
//                                            connection, each project
//                                            subscribes independently)
//
// Safety: single SQL transaction, zero DELETEs, owner-only auth.
// Refuses for composio-source connections because Composio's hosted
// `connected_accounts` are bound to a project on their side too and
// we don't yet propagate scope changes through their API.

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

type connectionScopeResult struct {
	ConnectionID int64  `json:"connection_id"`
	OldProjectID string `json:"old_project_id"`
	NewProjectID string `json:"new_project_id"`
	MCPsMigrated int    `json:"mcp_servers_migrated"`
}

func (s *Server) handleSetConnectionScope(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/connections/")
	idStr := strings.TrimSuffix(rest, "/scope")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid connection id", http.StatusBadRequest)
		return
	}

	var body struct {
		ProjectID string `json:"project_id"` // "" = global
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	target := body.ProjectID

	userID := getUserID(r)

	// Load the connection. We need (a) the current project_id for the
	// summary, (b) the source — composio gets refused — and (c) the
	// owner check (GetConnection's user_id WHERE clause covers it but
	// returning a clearer error here is friendlier than a 404).
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	if conn.Source == "composio" {
		http.Error(w, "composio connections can't be re-scoped — Composio's hosted account is bound to a project on their side", http.StatusBadRequest)
		return
	}
	if conn.ProjectID == target {
		writeJSON(w, connectionScopeResult{
			ConnectionID: connID,
			OldProjectID: conn.ProjectID,
			NewProjectID: target,
		})
		return
	}

	tx, err := s.store.db.Begin()
	if err != nil {
		http.Error(w, "begin tx: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.Exec(
		`UPDATE connections SET project_id = ? WHERE id = ? AND user_id = ?`,
		target, connID, userID,
	); err != nil {
		http.Error(w, "update connection: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The auto-MCP row created at connection time inherits the
	// connection's project_id (see CreateMCPServerFromConnection in
	// store.go). It must follow the move; otherwise the dashboard's
	// MCP list would show the row under the wrong scope.
	mcpRes, err := tx.Exec(
		`UPDATE mcp_servers SET project_id = ? WHERE connection_id = ?`,
		target, connID,
	)
	if err != nil {
		http.Error(w, "update mcp_servers: "+err.Error(), http.StatusInternalServerError)
		return
	}
	mcpsMoved, _ := mcpRes.RowsAffected()

	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tx = nil

	log.Printf("[CONN-SCOPE] connection=%d (%s/%s) moved from %q to %q (mcps migrated: %d)",
		connID, conn.AppSlug, conn.Name, conn.ProjectID, target, mcpsMoved)

	writeJSON(w, connectionScopeResult{
		ConnectionID: connID,
		OldProjectID: conn.ProjectID,
		NewProjectID: target,
		MCPsMigrated: int(mcpsMoved),
	})
}
