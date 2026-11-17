package main

// Startup reconciler: create mcp_servers rows for any local
// connections that don't have one yet. Exists to heal the
// pre-auto-MCP state left behind by suite fan-outs created before
// suites_handlers.go and mcp_gateway.go started calling
// CreateMCPServerFromConnection. Safe to run on every boot: a single
// query skips rows that already have an MCP, and
// CreateMCPServerFromConnection is idempotent-ish (it appends a
// suffix if the name collides, so even a mid-flight duplicate is
// survivable).

import (
	"database/sql"
	"encoding/json"
	"log"
)

// BackfillMissingMCPServers walks the connections table and creates
// an mcp_servers row for every local connection that doesn't already
// have one. Master rows (slug prefix `_group:`) are skipped — they
// are credential storage, not tool surfaces. Also renames suite-child
// MCP rows whose slug was derived from the project label alone (the
// old bug) to the new `<app_slug>-<project>` form so tool prefixes
// are distinct across fan-outs.
func (s *Server) BackfillMissingMCPServers() {
	s.backfillMissingMCPRows()
	s.backfillSuiteChildMCPSlugs()
}

func (s *Server) backfillMissingMCPRows() {
	rows, err := s.store.dbQuery(`
		SELECT c.id, c.user_id, c.app_slug, c.app_name, c.name, c.auth_type,
		       COALESCE(c.source,'local'), c.status, COALESCE(c.project_id,''), c.encrypted_credentials
		FROM connections c
		LEFT JOIN mcp_servers m ON m.connection_id = c.id
		WHERE m.id IS NULL
		  AND COALESCE(c.source,'local') = 'local'
		  AND c.app_slug NOT LIKE '_group:%'`)
	if err != nil {
		log.Printf("[BACKFILL-MCP] query failed: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	type databaseRecord struct {
		conn      Connection
		source    string
		projectID string
		encCreds  string
	}
	readRecords, err := collectSQLRows(rows, func(scanner rowScanner) (databaseRecord, error) {
		var record databaseRecord
		if err := scanner.Scan(&record.conn.ID, &record.conn.UserID, &record.conn.AppSlug, &record.conn.AppName, &record.conn.Name, &record.conn.AuthType, &record.source, &record.conn.Status, &record.projectID, &record.encCreds); err != nil {
			return record, err
		}
		return record, nil
	})
	if err != nil {
		return
	}
	for _, record := range readRecords {
		conn := record.conn
		source := record.source
		projectID := record.projectID
		encCreds := record.encCreds

		conn.Source = source
		conn.ProjectID = projectID
		toolCount := 0
		if app := s.catalog.Get(conn.AppSlug); app != nil {
			toolCount = len(app.Tools)
		}
		// Apply the new suite-aware slug rule when the connection is
		// a child: base = "<app_slug>-<project_label>". Detected by
		// peeking at the decrypted blob for `_type == "child"`.
		slugBase := ""
		if plain, err := Decrypt(s.secret, encCreds); err == nil {
			var blob map[string]string
			if json.Unmarshal([]byte(plain), &blob) == nil && blob[credKeyType] == "child" {
				slugBase = conn.AppSlug + "-" + conn.Name
			}
		}
		if mcpID, err := s.store.CreateMCPServerFromConnectionWithSlug(conn.UserID, &conn, toolCount, slugBase); err != nil {
			if err != sql.ErrNoRows {
				log.Printf("[BACKFILL-MCP] create failed conn=%d slug=%s: %v", conn.ID, conn.AppSlug, err)
			}
		} else {
			log.Printf("[BACKFILL-MCP] recovered conn=%d (%s/%s) → mcp=%d", conn.ID, conn.AppSlug, conn.Name, mcpID)
			count++
		}
	}

	if count > 0 {
		log.Printf("[BACKFILL-MCP] healed %d orphaned connections", count)
	}
}

// backfillSuiteChildMCPSlugs renames MCP rows whose slug was derived
// from just the project label (early bug) to the new
// "<app_slug>-<project_label>" form. Only touches rows whose
// connection is a suite child AND whose current MCP name doesn't
// already contain the app slug — so repeated runs are idempotent.
func (s *Server) backfillSuiteChildMCPSlugs() {
	rows, err := s.store.dbQuery(`
		SELECT c.id, c.user_id, c.app_slug, c.name, c.encrypted_credentials,
		       m.id, m.name, COALESCE(c.project_id,'')
		FROM connections c
		JOIN mcp_servers m ON m.connection_id = c.id
		WHERE COALESCE(c.source,'local') = 'local'
		  AND c.app_slug NOT LIKE '_group:%'`)
	if err != nil {
		return
	}
	defer rows.Close()

	renamed := 0
	type databaseRecord struct {
		connID    int64
		userID    int64
		mcpID     int64
		appSlug   string
		connName  string
		encCreds  string
		mcpName   string
		projectID string
	}
	readRecords, err := collectSQLRows(rows, func(scanner rowScanner) (databaseRecord, error) {
		var record databaseRecord
		if err := scanner.Scan(&record.connID, &record.userID, &record.appSlug, &record.connName, &record.encCreds, &record.mcpID, &record.mcpName, &record.projectID); err != nil {
			return record, err
		}
		return record, nil
	})
	if err != nil {
		return
	}
	for _, record := range readRecords {
		connID := record.connID
		userID := record.userID
		mcpID := record.mcpID
		appSlug := record.appSlug
		connName := record.connName
		encCreds := record.encCreds
		mcpName := record.mcpName
		projectID := record.projectID

		plain, err := Decrypt(s.secret, encCreds)
		if err != nil {
			continue
		}
		var blob map[string]string
		if json.Unmarshal([]byte(plain), &blob) != nil {
			continue
		}
		if blob[credKeyType] != "child" {
			continue
		}
		// Already contains the app slug → leave it alone.
		if len(mcpName) >= len(appSlug) && mcpName[:len(appSlug)] == appSlug {
			continue
		}
		desired := s.store.uniqueMCPName(userID, projectID, slugify(appSlug+"-"+connName), connID)
		if desired == mcpName {
			continue
		}
		if _, err := s.store.dbExec("UPDATE mcp_servers SET name = ? WHERE id = ?", desired, mcpID); err == nil {
			log.Printf("[BACKFILL-MCP] renamed mcp=%d %q → %q", mcpID, mcpName, desired)
			renamed++
		}
	}

	if renamed > 0 {
		log.Printf("[BACKFILL-MCP] renamed %d suite-child MCP slugs", renamed)
	}
}
