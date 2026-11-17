package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

var catalogHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Store) migrateToolCatalogs() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS tool_catalogs (
 hash TEXT PRIMARY KEY,format TEXT NOT NULL,data TEXT NOT NULL,tool_count INTEGER NOT NULL,alias_count INTEGER NOT NULL,
 first_seen TEXT NOT NULL,last_seen TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS tool_catalog_agents (
 catalog_hash TEXT NOT NULL,agent_id INTEGER NOT NULL,last_seen TEXT NOT NULL,PRIMARY KEY(catalog_hash,agent_id));
 CREATE INDEX IF NOT EXISTS ix_tool_catalog_agents_agent ON tool_catalog_agents(agent_id,catalog_hash);`)
	return err
}

// Both old Core snapshots and new Core references use the same ingest path.
// Full schemas are stored only in the content-addressed table, never per event.
func compactToolCatalog(tx *sql.Tx, event *TelemetryEvent) error {
	if event.Type == "tool.manifest" {
		var ref struct {
			Hash string `json:"catalog_hash"`
		}
		_ = json.Unmarshal(event.Data, &ref)
		if !catalogHashPattern.MatchString(ref.Hash) {
			return nil
		}
		return touchToolCatalog(tx, ref.Hash, event)
	}
	if event.Type != "tool.catalog" {
		return nil
	}
	// A malformed catalog event is stored unchanged, as before content
	// addressing, so it can't make the rest of its batch fail.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(event.Data, &fields); err != nil || fields == nil {
		return nil
	}
	var hash string
	var tools, aliases int
	if _, full := fields["tools"]; full {
		var body json.RawMessage
		var err error
		hash, body, tools, aliases, err = canonicalToolCatalog(event.Data)
		if err != nil {
			return nil
		}
		at := event.Time.UTC().Format(time.RFC3339Nano)
		if _, err = tx.Exec(`INSERT INTO tool_catalogs(hash,format,data,tool_count,alias_count,first_seen,last_seen) VALUES(?,?,?,?,?,?,?) ON CONFLICT(hash) DO UPDATE SET last_seen=MAX(last_seen,excluded.last_seen)`, hash, toolCatalogFormat, string(body), tools, aliases, at, at); err != nil {
			return err
		}
	} else {
		if json.Unmarshal(fields["catalog_hash"], &hash) != nil || !catalogHashPattern.MatchString(hash) {
			return nil
		}
		// References may arrive before a retried snapshot; keep them so its eventual
		// arrival restores debugging lookup without losing the refresh event.
		err := tx.QueryRow(`SELECT tool_count,alias_count FROM tool_catalogs WHERE hash=?`, hash).Scan(&tools, &aliases)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec(`UPDATE tool_catalogs SET last_seen=MAX(last_seen,?) WHERE hash=?`, event.Time.UTC().Format(time.RFC3339Nano), hash); err != nil {
			return err
		}
	}
	if err := touchToolCatalog(tx, hash, event); err != nil {
		return err
	}

	delete(fields, "tools")
	delete(fields, "aliases")
	add := func(k string, v any) { b, _ := json.Marshal(v); fields[k] = b }
	add("catalog_hash", hash)
	add("catalog_format", toolCatalogFormat)
	add("catalog_reference", true)
	add("tool_count", tools)
	add("alias_count", aliases)
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	event.Data = data
	return nil
}

func (s *Store) compactLiveToolCatalogs(events []TelemetryEvent) error {
	needed := false
	for _, e := range events {
		needed = needed || e.Type == "tool.catalog" || e.Type == "tool.manifest"
	}
	if !needed {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range events {
		if err = compactToolCatalog(tx, &events[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) readToolCatalog(ctx context.Context, hash string, agentID int64) (json.RawMessage, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT c.data FROM tool_catalogs c JOIN tool_catalog_agents a ON a.catalog_hash=c.hash WHERE c.hash=? AND a.agent_id=?`, hash, agentID).Scan(&body)
	return json.RawMessage(body), err
}

func (s *Server) handleToolCatalogRead(w http.ResponseWriter, r *http.Request, p telemetryPrincipal, suffix string) {
	if !p.content {
		http.Error(w, "catalog content permission required", 403)
		return
	}
	hash := strings.TrimPrefix(suffix, "/catalogs/")
	agentID, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
	if err != nil || agentID <= 0 || !catalogHashPattern.MatchString(hash) {
		http.Error(w, "agent_id and valid catalog hash required", 400)
		return
	}
	agents, err := s.telemetryAgents(p, sdk.TelemetryQuery{AgentID: agentID, ProjectID: p.project, Projection: "content"})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := agents[agentID]; !ok {
		http.NotFound(w, r)
		return
	}
	body, err := s.store.readToolCatalog(r.Context(), hash, agentID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]any{"catalog_hash": hash, "catalog": body})
}

func (s *Store) cleanToolCatalogs(cutoff string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM tool_catalog_agents WHERE last_seen<?`, cutoff); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM tool_catalogs WHERE last_seen<? AND NOT EXISTS(SELECT 1 FROM tool_catalog_agents a WHERE a.catalog_hash=tool_catalogs.hash)`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

func touchToolCatalog(tx *sql.Tx, hash string, event *TelemetryEvent) error {
	at := event.Time.UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tool_catalogs SET last_seen=MAX(last_seen,?) WHERE hash=?`, at, hash); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO tool_catalog_agents(catalog_hash,agent_id,last_seen) VALUES(?,?,?) ON CONFLICT(catalog_hash,agent_id) DO UPDATE SET last_seen=MAX(last_seen,excluded.last_seen)`, hash, event.AgentID, at)
	return err
}
func (s *Store) missingToolCatalogs(events []TelemetryEvent) []string {
	missing := []string{}
	seen := map[string]bool{}
	for _, e := range events {
		if e.Type != "tool.catalog" && e.Type != "tool.manifest" {
			continue
		}
		var ref struct {
			Hash string `json:"catalog_hash"`
		}
		if json.Unmarshal(e.Data, &ref) != nil || !catalogHashPattern.MatchString(ref.Hash) || seen[ref.Hash] {
			continue
		}
		seen[ref.Hash] = true
		var found int
		if s.db.QueryRow(`SELECT 1 FROM tool_catalogs WHERE hash=?`, ref.Hash).Scan(&found) == sql.ErrNoRows {
			missing = append(missing, ref.Hash)
		}
		if len(missing) >= 64 {
			break
		}
	}
	return missing
}
