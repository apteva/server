package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type User struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	PasswordHash string `json:"-"`
	// Role is the platform-level role: 'user' (default) or 'admin'.
	// Admin is an implicit owner on every project — see
	// requireProjectAccess in authz.go.
	Role               string     `json:"role"`
	CreatedAt          time.Time  `json:"created_at"`
	OnboardedAt        *time.Time `json:"onboarded_at,omitempty"`
	MFAType            string     `json:"mfa_type,omitempty"`
	MFASecretEncrypted string     `json:"-"`
	MFARecoveryHashes  string     `json:"-"`
	MFALastCounter     int64      `json:"-"`
	MFAEnabledAt       *time.Time `json:"mfa_enabled_at,omitempty"`
}

type APIKey struct {
	ID                 int64     `json:"id"`
	UserID             int64     `json:"user_id"`
	Name               string    `json:"name"`
	KeyPrefix          string    `json:"key_prefix"` // first chars for display
	KeyHash            string    `json:"-"`
	Kind               string    `json:"kind"`
	ProjectID          string    `json:"project_id,omitempty"`
	Scopes             string    `json:"scopes,omitempty"`
	AllowedOrigins     string    `json:"allowed_origins,omitempty"`
	RateLimitPerMinute int       `json:"rate_limit_per_minute,omitempty"`
	ExpiresAt          string    `json:"expires_at,omitempty"`
	RevokedAt          string    `json:"revoked_at,omitempty"`
	LastUsed           string    `json:"last_used,omitempty"`
	LastUsedIP         string    `json:"last_used_ip,omitempty"`
	IssuerApp          string    `json:"issuer_app,omitempty"`
	IssuerInstallID    int64     `json:"issuer_install_id,omitempty"`
	SubjectType        string    `json:"subject_type,omitempty"`
	SubjectID          string    `json:"subject_id,omitempty"`
	SubjectEmail       string    `json:"subject_email,omitempty"`
	OrganizationID     string    `json:"organization_id,omitempty"`
	OrganizationSlug   string    `json:"organization_slug,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type Project struct {
	ID          string    `json:"id"`
	UserID      int64     `json:"user_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Color       string    `json:"color"`
	CreatedAt   time.Time `json:"created_at"`
}

type EnvironmentRecord struct {
	ID            string
	ProjectID     string
	Name          string
	Mode          string
	Status        string
	SpecJSON      string
	CreatedBy     int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastStartedAt *time.Time
	LastStoppedAt *time.Time
	ErrorMessage  string
}

type Agent struct {
	ID                  int64     `json:"id"`
	UserID              int64     `json:"user_id"`
	Name                string    `json:"name"`
	Directive           string    `json:"directive"`
	Mode                string    `json:"mode"`   // "autonomous" | "cautious" | "learn"
	Config              string    `json:"config"` // JSON blob
	Port                int       `json:"port"`
	Pid                 int       `json:"pid"`
	CoreAPIKey          string    `json:"-"`
	Status              string    `json:"status"` // running, stopped
	ProjectID           string    `json:"project_id,omitempty"`
	Kind                string    `json:"kind,omitempty"`
	CoreVersion         string    `json:"core_version,omitempty"`
	CoreBuildTime       string    `json:"core_build_time,omitempty"`
	CoreStartedAt       string    `json:"core_started_at,omitempty"`
	TargetCoreVersion   string    `json:"target_core_version,omitempty"`
	CoreUpdateAvailable bool      `json:"core_update_available,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

type Store struct{ db *sql.DB }

const (
	sqliteBusyTimeoutMS = 30000
	sqliteMaxOpenConns  = 8
)

func NewStore(path string) (*Store, error) {
	// _pragma= in the DSN applies to EVERY new connection
	// modernc.org/sqlite opens, not just the one that happens to
	// run an initial PRAGMA Exec. Pre-v0.12.4 the bare PRAGMA
	// statements below only configured whichever connection from
	// the database/sql pool ran them; a SECOND connection (and
	// the pool always opens a second under any concurrency) had
	// busy_timeout=0 and would return SQLITE_BUSY immediately
	// rather than waiting for the lock. Symptom: during a
	// supervisor restart, the boot's "[APPS] seed builtin
	// channel-chat" insert would race the prior process's WAL
	// settle and return "database is locked (5)" instead of
	// blocking for the full timeout the timeout was supposed to
	// grant. Soft race: the next read found the row and the boot
	// continued, but on a slower box or larger DB it could fail
	// loud. URI form fixes it for every pooled connection.
	dsn := path
	if !strings.Contains(dsn, "?") {
		dsn += "?"
	} else {
		dsn += "&"
	}
	// Every explicit transaction in the server is a write transaction. Start
	// those transactions with BEGIN IMMEDIATE so SQLite queues for the single
	// writer reservation before any reads occur. A deferred transaction that
	// reads and then upgrades to a writer can return SQLITE_BUSY immediately
	// when a parallel write wins the reservation, bypassing busy_timeout.
	dsn += fmt.Sprintf("_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate", sqliteBusyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Keep the server pool bounded, but do not force it to one
	// connection. The server has request paths that consume one result
	// set and then perform follow-up DB reads before the handler
	// returns; a single pooled connection turns any late rows.Close into
	// a self-deadlock. WAL + per-connection busy_timeout handles normal
	// SQLite writer contention, while this cap prevents an unbounded
	// pile-up of pooled connections under dashboard reload bursts.
	db.SetMaxOpenConns(sqliteMaxOpenConns)

	// Belt-and-suspenders: also fire the pragmas via Exec so the
	// initial connection has them even if the DSN parsing changes
	// in some future modernc.org/sqlite version. No-op for an
	// already-configured connection.
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", sqliteBusyTimeoutMS))
	db.Exec("PRAGMA foreign_keys=ON")
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// execWithBusyRetry wraps db.Exec with a small retry budget for
// SQLITE_BUSY (5). The DSN already configures busy_timeout
// per-connection, which usually absorbs lock contention inside the
// driver. This helper exists for the boot-time-sensitive paths
// (apps seed, migrations) where:
//
//   - the prior process's WAL is still settling at supervisor
//     restart, AND
//   - we'd rather block 1-2 seconds than log a soft error and
//     potentially miss the seed.
//
// 3 attempts x 250ms backoff = at most 750ms of additional wait
// on top of the busy_timeout. Cheap insurance for boot. Errors
// other than SQLITE_BUSY return immediately.
func execWithBusyRetry(db *sql.DB, query string, args ...any) error {
	const maxAttempts = 3
	const backoff = 250 * time.Millisecond
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		_, err := db.Exec(query, args...)
		if err == nil {
			return nil
		}
		lastErr = err
		// modernc.org/sqlite returns the SQLITE_BUSY message in the
		// error string. We don't unwrap to its concrete type here
		// because that would leak driver internals into a helper
		// that's already pretty mechanical; substring match is
		// good enough — false positives just retry harmlessly.
		s := err.Error()
		if !strings.Contains(s, "database is locked") && !strings.Contains(s, "SQLITE_BUSY") {
			return err
		}
		time.Sleep(backoff)
	}
	return lastErr
}

// renameInstanceTablesToAgents migrates schemas that pre-date the
// instance → agent rename (Phase 1 — server-internal only). Runs
// before the CREATE TABLE block so the IF NOT EXISTS clauses don't
// accidentally create a second, empty `agents` table while the data
// stays in `instances`.
//
// Idempotent — every step checks the source artifact still exists.
// Wire-facing names (JSON keys, env vars, HTTP query params, the
// X-Instance-Secret header) are deliberately NOT touched; those are
// Phase 2 work coordinated with apteva-core + apps.
//
// SQLite ≥ 3.25 RENAME COLUMN auto-updates indexes that reference
// the renamed column; we still DROP + CREATE on the index name itself
// since renaming an index isn't supported in the same statement.
func (s *Store) renameInstanceTablesToAgents() error {
	exec := func(query string) error {
		_, err := s.db.Exec(query)
		return err
	}
	// Rename the main agent table.
	if tableExists(s.db, "instances") && !tableExists(s.db, "agents") {
		if err := exec("ALTER TABLE instances RENAME TO agents"); err != nil {
			return fmt.Errorf("rename instances table: %w", err)
		}
	}
	// Rename FK columns in dependent tables.
	for _, t := range []string{"subscriptions", "channels", "telemetry", "app_grants"} {
		if columnExists(s.db, t, "instance_id") && !columnExists(s.db, t, "agent_id") {
			if err := exec("ALTER TABLE " + t + " RENAME COLUMN instance_id TO agent_id"); err != nil {
				return fmt.Errorf("rename %s.instance_id: %w", t, err)
			}
		}
	}
	// The binding table itself was renamed.
	if tableExists(s.db, "app_instance_bindings") && !tableExists(s.db, "app_agent_bindings") {
		if err := exec("ALTER TABLE app_instance_bindings RENAME TO app_agent_bindings"); err != nil {
			return fmt.Errorf("rename app instance bindings: %w", err)
		}
	}
	if columnExists(s.db, "app_agent_bindings", "instance_id") && !columnExists(s.db, "app_agent_bindings", "agent_id") {
		if err := exec("ALTER TABLE app_agent_bindings RENAME COLUMN instance_id TO agent_id"); err != nil {
			return fmt.Errorf("rename app bindings instance column: %w", err)
		}
	}
	// Index name swap. RENAME COLUMN keeps the old index alive
	// (pointing at the new column) — the rename below just gets the
	// name consistent with the new conventions, no data movement.
	if err := exec("DROP INDEX IF EXISTS idx_telem_instance_time"); err != nil {
		return fmt.Errorf("drop legacy telemetry index: %w", err)
	}
	return nil
}

// repairLegacyAgentForeignKeys handles databases that were upgraded by an
// early instance-to-agent migration. Those databases can already have the
// agents table while dependent table definitions still reference instances,
// which makes otherwise unrelated DELETE statements fail at prepare time.
func (s *Store) repairLegacyAgentForeignKeys() error {
	type tableRepair struct {
		name       string
		createSQL  string
		columns    string
		copyFilter string
	}
	repairs := []tableRepair{
		{
			name: "app_agent_bindings",
			createSQL: `CREATE TABLE app_agent_bindings (
				install_id INTEGER NOT NULL REFERENCES app_installs(id),
				agent_id INTEGER NOT NULL REFERENCES agents(id),
				enabled INTEGER NOT NULL DEFAULT 1,
				PRIMARY KEY (install_id, agent_id)
			)`,
			columns:    "install_id, agent_id, enabled",
			copyFilter: "EXISTS (SELECT 1 FROM app_installs i WHERE i.id=legacy.install_id) AND EXISTS (SELECT 1 FROM agents a WHERE a.id=legacy.agent_id)",
		},
		{
			name: "app_grants",
			createSQL: `CREATE TABLE app_grants (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
				agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
				effect TEXT NOT NULL CHECK (effect IN ('allow','deny')),
				permission TEXT NOT NULL,
				resource TEXT NOT NULL DEFAULT '*',
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				created_by TEXT NOT NULL DEFAULT '',
				UNIQUE(install_id, agent_id, effect, permission, resource)
			)`,
			columns:    "id, install_id, agent_id, effect, permission, resource, created_at, created_by",
			copyFilter: "EXISTS (SELECT 1 FROM app_installs i WHERE i.id=legacy.install_id) AND EXISTS (SELECT 1 FROM agents a WHERE a.id=legacy.agent_id)",
		},
		{
			name: "app_grant_defaults",
			createSQL: `CREATE TABLE app_grant_defaults (
				install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
				agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
				default_effect TEXT NOT NULL CHECK (default_effect IN ('allow','deny')),
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (install_id, agent_id)
			)`,
			columns:    "install_id, agent_id, default_effect, updated_at",
			copyFilter: "EXISTS (SELECT 1 FROM app_installs i WHERE i.id=legacy.install_id) AND EXISTS (SELECT 1 FROM agents a WHERE a.id=legacy.agent_id)",
		},
	}

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	needsRepair := false
	for _, repair := range repairs {
		var schema string
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='table' AND name=?`, repair.name).Scan(&schema); err == nil && strings.Contains(strings.ToLower(schema), "references instances") {
			needsRepair = true
			break
		}
	}
	if !needsRepair {
		return nil
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for agent repair: %w", err)
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, repair := range repairs {
		var schema string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='table' AND name=?`, repair.name).Scan(&schema); err != nil || !strings.Contains(strings.ToLower(schema), "references instances") {
			continue
		}
		legacyName := repair.name + "_legacy_agent_fk"
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+repair.name+" RENAME TO "+legacyName); err != nil {
			return fmt.Errorf("rename legacy %s: %w", repair.name, err)
		}
		if _, err := tx.ExecContext(ctx, repair.createSQL); err != nil {
			return fmt.Errorf("recreate %s: %w", repair.name, err)
		}
		copySQL := fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) SELECT %s FROM %s AS legacy WHERE %s", repair.name, repair.columns, repair.columns, legacyName, repair.copyFilter)
		if _, err := tx.ExecContext(ctx, copySQL); err != nil {
			return fmt.Errorf("copy %s: %w", repair.name, err)
		}
		if _, err := tx.ExecContext(ctx, "DROP TABLE "+legacyName); err != nil {
			return fmt.Errorf("drop legacy %s: %w", repair.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent foreign-key repair: %w", err)
	}
	_, err = conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_app_grants_lookup ON app_grants(install_id, agent_id)`)
	return err
}

// tableExists checks whether `name` is a table in the connected DB.
// Used by the rename migration to skip steps already applied.
func tableExists(db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&n)
	return n > 0
}

func (s *Store) migrate() error {
	// Phase 1 rename runs first so the CREATE TABLE IF NOT EXISTS
	// block below sees a freshly-renamed schema.
	if err := s.renameInstanceTablesToAgents(); err != nil {
		return err
	}

	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			mfa_type TEXT NOT NULL DEFAULT '',
			mfa_secret_encrypted TEXT NOT NULL DEFAULT '',
			mfa_recovery_hashes TEXT NOT NULL DEFAULT '[]',
			mfa_last_counter INTEGER NOT NULL DEFAULT -1,
			mfa_enabled_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			name TEXT NOT NULL,
			key_prefix TEXT NOT NULL,
			key_hash TEXT UNIQUE NOT NULL,
			kind TEXT NOT NULL DEFAULT 'private',
			project_id TEXT NOT NULL DEFAULT '',
			scopes TEXT NOT NULL DEFAULT '[]',
			allowed_origins TEXT NOT NULL DEFAULT '[]',
			rate_limit_per_minute INTEGER NOT NULL DEFAULT 60,
			expires_at DATETIME,
			revoked_at DATETIME,
			last_used_ip TEXT NOT NULL DEFAULT '',
			last_used DATETIME,
			issuer_app TEXT NOT NULL DEFAULT '',
			issuer_install_id INTEGER NOT NULL DEFAULT 0,
			subject_type TEXT NOT NULL DEFAULT '',
			subject_id TEXT NOT NULL DEFAULT '',
			subject_email TEXT NOT NULL DEFAULT '',
			organization_id TEXT NOT NULL DEFAULT '',
			organization_slug TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS delegated_access_policies (
			issuer_install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
			project_id TEXT NOT NULL,
			oauth_client_id TEXT NOT NULL,
			scopes TEXT NOT NULL,
			token_ttl_seconds INTEGER NOT NULL DEFAULT 3600,
			rate_limit_per_minute INTEGER NOT NULL DEFAULT 120,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (issuer_install_id, project_id, oauth_client_id)
		);
		CREATE INDEX IF NOT EXISTS idx_delegated_access_policy_lookup
			ON delegated_access_policies(issuer_install_id, project_id, oauth_client_id);
		CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id),
			expires_at DATETIME NOT NULL,
			auth_state TEXT NOT NULL DEFAULT 'active',
			mfa_attempts INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
			CREATE TABLE IF NOT EXISTS provider_types (
				id INTEGER PRIMARY KEY,
				type TEXT NOT NULL,
				name TEXT UNIQUE NOT NULL,
				description TEXT DEFAULT '',
				fields TEXT DEFAULT '[]',
				requires_credentials INTEGER DEFAULT 1,
				auth_type TEXT DEFAULT 'api_key',
				auth_provider TEXT DEFAULT '',
				runtime_status TEXT DEFAULT 'available',
				capabilities TEXT DEFAULT '[]',
				sort_order INTEGER DEFAULT 0
			);

		INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
			(1, 'llm', 'Fireworks', 'LLM inference via Fireworks AI', '["FIREWORKS_API_KEY"]', 1, 10),
			(2, 'llm', 'OpenAI', 'LLM inference and embeddings', '["OPENAI_API_KEY","OPENAI_BASE_URL"]', 1, 11),
			(3, 'llm', 'Anthropic', 'LLM inference via Anthropic', '["ANTHROPIC_API_KEY"]', 1, 12),
			(4, 'llm', 'Ollama', 'Local LLM inference', '["OLLAMA_HOST","OLLAMA_MODEL","OLLAMA_EMBED_MODEL","OLLAMA_EMBED_DIM"]', 1, 13),
			(5, 'integrations', 'Apteva Local', '200+ app integrations (GitHub, Slack, Stripe, etc.)', '[]', 0, 15),
			(6, 'embeddings', 'Voyage', 'Text embeddings', '["VOYAGE_API_KEY"]', 1, 20),
			(7, 'tts', 'ElevenLabs', 'Text-to-speech', '["ELEVENLABS_API_KEY"]', 1, 30),
			(8, 'browserbase', 'Browserbase', 'Cloud browser automation via Browserbase', '["BROWSERBASE_API_KEY","BROWSERBASE_PROJECT_ID"]', 1, 40),
			(10, 'llm', 'NVIDIA', 'LLM inference via NVIDIA NIM (integrate.api.nvidia.com)', '["NVIDIA_API_KEY"]', 1, 14),
			(11, 'steel', 'Steel', 'Cloud browser automation via Steel.dev', '["STEEL_API_KEY"]', 1, 41),
			(12, 'browser-engine', 'Browser Engine', 'Cloud browser automation via Browser Engine (self-hosted)', '["BROWSER_API_KEY","BROWSER_API_URL"]', 1, 42),
			(16, 'llm', 'Google', 'Gemini models via the Google Generative Language API', '["GOOGLE_API_KEY"]', 1, 13);

		-- Update existing Fireworks provider type to include model override fields
		UPDATE provider_types SET fields = '["FIREWORKS_API_KEY"]' WHERE id = 1;

		CREATE TABLE IF NOT EXISTS providers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			provider_type_id INTEGER DEFAULT 0,
			type TEXT NOT NULL,
			name TEXT NOT NULL,
			encrypted_data TEXT NOT NULL,
			status TEXT DEFAULT 'active',
			project_id TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS connections (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			app_slug TEXT NOT NULL,
			app_name TEXT NOT NULL,
			name TEXT NOT NULL,
			auth_type TEXT NOT NULL,
			encrypted_credentials TEXT NOT NULL,
			status TEXT DEFAULT 'active',
			project_id TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS ingress_routes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			hostname TEXT NOT NULL UNIQUE,
			target TEXT NOT NULL,
			project_id TEXT NOT NULL DEFAULT '',
			owner_install_id INTEGER NOT NULL DEFAULT 0,
			owner_kind TEXT NOT NULL DEFAULT '',
			cert_fqdn TEXT NOT NULL DEFAULT '',
			allow_http INTEGER NOT NULL DEFAULT 0,
			tls_mode TEXT NOT NULL DEFAULT 'auto',
			status TEXT NOT NULL DEFAULT 'active',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_ingress_routes_project ON ingress_routes(project_id);
		CREATE INDEX IF NOT EXISTS idx_ingress_routes_owner ON ingress_routes(owner_install_id);

		CREATE TABLE IF NOT EXISTS mcp_servers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			name TEXT NOT NULL,
			command TEXT NOT NULL DEFAULT '',
			args TEXT DEFAULT '[]',
			encrypted_env TEXT DEFAULT '',
			description TEXT DEFAULT '',
			status TEXT DEFAULT 'stopped',
			tool_count INTEGER DEFAULT 0,
			pid INTEGER DEFAULT 0,
			source TEXT DEFAULT 'custom',
			connection_id INTEGER DEFAULT 0,
			project_id TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS subscriptions (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id),
			agent_id INTEGER NOT NULL DEFAULT 0,
			connection_id INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL,
			slug TEXT NOT NULL DEFAULT '',
			description TEXT DEFAULT '',
			webhook_path TEXT UNIQUE NOT NULL,
			encrypted_hmac_secret TEXT DEFAULT '',
			enabled INTEGER DEFAULT 1,
			notify_agent INTEGER DEFAULT 0,
			thread_id TEXT DEFAULT '',
			delivery TEXT NOT NULL DEFAULT 'webhook',
			poll_config_json TEXT NOT NULL DEFAULT '',
			poll_state_json TEXT NOT NULL DEFAULT '',
			last_run_at DATETIME,
			next_run_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			failure_count INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL DEFAULT 'user',
			match_json TEXT NOT NULL DEFAULT '',
			wait_group_id TEXT NOT NULL DEFAULT '',
			expires_at DATETIME,
			delete_on_match INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_sub_webhook ON subscriptions(webhook_path);

		CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			agent_id INTEGER NOT NULL DEFAULT 0,
			type TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			encrypted_config TEXT DEFAULT '',
			status TEXT DEFAULT 'active',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS telemetry (
			id TEXT PRIMARY KEY,
			agent_id INTEGER NOT NULL,
			thread_id TEXT NOT NULL DEFAULT 'main',
			type TEXT NOT NULL,
			time DATETIME NOT NULL,
			data TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_telem_agent_time ON telemetry(agent_id, time);
		CREATE INDEX IF NOT EXISTS idx_telem_type ON telemetry(type, time);
		CREATE INDEX IF NOT EXISTS idx_telem_agent_type_time ON telemetry(agent_id, type, time);

		CREATE TABLE IF NOT EXISTS agents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			name TEXT NOT NULL,
			directive TEXT DEFAULT '',
			mode TEXT DEFAULT 'autonomous',
			config TEXT DEFAULT '{}',
			port INTEGER DEFAULT 0,
			pid INTEGER DEFAULT 0,
			core_api_key TEXT NOT NULL DEFAULT '',
			core_version TEXT NOT NULL DEFAULT '',
			core_build_time TEXT NOT NULL DEFAULT '',
			core_started_at DATETIME,
			status TEXT DEFAULT 'stopped',
			project_id TEXT DEFAULT '',
			-- kind distinguishes platform-owned agents (the meta-agent,
			-- eventually onboarding helpers, classifier, etc.) from
			-- regular user agents. List endpoints filter 'user' by
			-- default so platform agents don't clutter the dashboard.
			-- Values: 'user' (default), 'platform_helper' (the meta-agent).
			kind TEXT NOT NULL DEFAULT 'user',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id),
			name TEXT NOT NULL,
			description TEXT DEFAULT '',
			color TEXT DEFAULT '#6366f1',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		-- agent_templates — pre-canned starter configs surfaced in the
		-- "build your first agent" wizard. Three sources, sharing one
		-- table:
		--   source='builtin'  — shipped by apteva, user_id NULL.
		--                       Seeded inline below. INSERT OR IGNORE
		--                       protects operator edits across upgrades;
		--                       new platform-wide updates ship under a
		--                       fresh id (e.g. 'personal-assistant-v2').
		--   source='app'      — contributed by an installed app via
		--                       its manifest. apps_loader upserts these
		--                       on install/upgrade; uninstall deletes
		--                       them. id convention '<app_slug>:<id>'.
		--   source='user'     — operator's own templates (save-from-agent
		--                       or hand-written). user_id set.
		CREATE TABLE IF NOT EXISTS agent_templates (
			id               TEXT PRIMARY KEY,
			user_id          INTEGER REFERENCES users(id),
			source           TEXT NOT NULL DEFAULT 'builtin',
			source_ref       TEXT NOT NULL DEFAULT '',
			name             TEXT NOT NULL,
			icon             TEXT NOT NULL DEFAULT '',
			description      TEXT NOT NULL DEFAULT '',
			directive        TEXT NOT NULL,
			mode             TEXT NOT NULL DEFAULT 'learn',
			unconscious      INTEGER NOT NULL DEFAULT 0,
			recommended_apps TEXT NOT NULL DEFAULT '[]',
			-- requirements: JSON array of typed entries declaring what
			-- the wizard's Setup step needs to install/configure.
			-- Single source of truth for both the wizard render and
			-- (eventually) the meta-agent's checklist.
			-- Shape: [{"kind":"app","slug":"storage","required":true},
			--        {"kind":"integration","compatible_slugs":["slack"],...}]
			requirements     TEXT NOT NULL DEFAULT '[]',
			sort_order       INTEGER NOT NULL DEFAULT 100,
			created_at       DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at       DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		-- Per-user hide list. A user who doesn't want "Outbound sales"
		-- on their wizard inserts a row here; the listing endpoint
		-- filters it out. Cheaper than forking + deleting per-user
		-- copies of every builtin.
		CREATE TABLE IF NOT EXISTS user_template_hidden (
			user_id     INTEGER NOT NULL REFERENCES users(id),
			template_id TEXT    NOT NULL,
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, template_id)
		);


		-- Generic audit trail for app-initiated agent changes. Product-specific
		-- state stays in the owning app; the server
		-- records only the platform mutation and its provenance.
		CREATE TABLE IF NOT EXISTS agent_change_history (
			id                    INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id              INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
			field                 TEXT NOT NULL,
			before_json           TEXT NOT NULL,
			after_json            TEXT NOT NULL,
			reason                TEXT NOT NULL DEFAULT '',
			source_app_install_id INTEGER NOT NULL DEFAULT 0,
			applied_by_user_id    INTEGER NOT NULL DEFAULT 0,
			created_at            DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_agent_change_history_agent
			ON agent_change_history(agent_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS environments (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'block',
			status TEXT NOT NULL DEFAULT 'stopped',
			spec_json TEXT NOT NULL DEFAULT '{}',
			created_by INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_started_at DATETIME,
			last_stopped_at DATETIME,
			error_message TEXT NOT NULL DEFAULT ''
		);
	`)
	if err != nil {
		return err
	}

	// Seed the builtin agent templates. Same INSERT-OR-IGNORE pattern
	// as provider_types — operators can edit shipped rows freely and
	// upgrades won't trample them. To roll out a new version of a
	// shipped template, give it a fresh id ('personal-assistant-v2').
	// Catch-up: rename emoji → icon for DBs created before the
	// rename. SQLite 3.25+ ALTER COLUMN. Idempotent — if icon
	// already exists the guard short-circuits.
	if columnExists(s.db, "agent_templates", "emoji") && !columnExists(s.db, "agent_templates", "icon") {
		s.db.Exec("ALTER TABLE agent_templates RENAME COLUMN emoji TO icon")
	}
	// Catch-up: requirements column for DBs created before the
	// requirements rollout. Fresh DBs already have it from the
	// CREATE above. Default '[]' keeps existing rows valid JSON.
	if !columnExists(s.db, "agent_templates", "requirements") {
		s.db.Exec("ALTER TABLE agent_templates ADD COLUMN requirements TEXT NOT NULL DEFAULT '[]'")
	}
	// Catch-up: kind column for DBs created before the meta-agent
	// rollout. Existing rows default to 'user' so dashboards keep
	// listing them as before.
	if !columnExists(s.db, "agents", "kind") {
		s.db.Exec("ALTER TABLE agents ADD COLUMN kind TEXT NOT NULL DEFAULT 'user'")
	}
	if !columnExists(s.db, "agents", "core_api_key") {
		s.db.Exec("ALTER TABLE agents ADD COLUMN core_api_key TEXT NOT NULL DEFAULT ''")
	}
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN kind TEXT NOT NULL DEFAULT 'private'")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN project_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN scopes TEXT NOT NULL DEFAULT '[]'")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN allowed_origins TEXT NOT NULL DEFAULT '[]'")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN rate_limit_per_minute INTEGER NOT NULL DEFAULT 60")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN expires_at DATETIME")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN revoked_at DATETIME")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN last_used_ip TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN issuer_app TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN issuer_install_id INTEGER NOT NULL DEFAULT 0")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN subject_type TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN subject_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN subject_email TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN organization_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE api_keys ADD COLUMN organization_slug TEXT NOT NULL DEFAULT ''")
	if !columnExists(s.db, "agents", "core_version") {
		s.db.Exec("ALTER TABLE agents ADD COLUMN core_version TEXT NOT NULL DEFAULT ''")
	}
	if !columnExists(s.db, "agents", "core_build_time") {
		s.db.Exec("ALTER TABLE agents ADD COLUMN core_build_time TEXT NOT NULL DEFAULT ''")
	}
	if !columnExists(s.db, "agents", "core_started_at") {
		s.db.Exec("ALTER TABLE agents ADD COLUMN core_started_at DATETIME")
	}
	s.db.Exec(`UPDATE agents
	              SET port=0, pid=0, core_api_key=''
	            WHERE status='stopped'
	              AND (port != 0 OR pid != 0 OR COALESCE(core_api_key,'') != '')`)

	// The canonical builtin set lives next to its Go types in
	// agent_templates.go. Operator edits to existing rows survive
	// (INSERT OR IGNORE); platform-owned shape (requirements,
	// sort_order, icon) gets reapplied each boot.
	seedBuiltinTemplates(s.db)

	// onboarded_at: NULL for users who haven't finished the welcome flow.
	// First-time deploy of this column backfills pre-existing users from
	// created_at so we don't trap them in onboarding on upgrade. The
	// PRAGMA guard ensures the backfill runs exactly once — on every
	// subsequent boot we mustn't overwrite NULLs of users who registered
	// between boots and haven't completed onboarding yet.
	if !columnExists(s.db, "users", "onboarded_at") {
		s.db.Exec("ALTER TABLE users ADD COLUMN onboarded_at DATETIME")
		s.db.Exec("UPDATE users SET onboarded_at = created_at WHERE onboarded_at IS NULL")
	}
	// Optional dashboard MFA. These columns intentionally live on users and
	// sessions so the first TOTP implementation needs no auxiliary table.
	// Existing sessions remain fully authenticated through the active default.
	if !columnExists(s.db, "users", "mfa_type") {
		s.db.Exec("ALTER TABLE users ADD COLUMN mfa_type TEXT NOT NULL DEFAULT ''")
	}
	if !columnExists(s.db, "users", "mfa_secret_encrypted") {
		s.db.Exec("ALTER TABLE users ADD COLUMN mfa_secret_encrypted TEXT NOT NULL DEFAULT ''")
	}
	if !columnExists(s.db, "users", "mfa_recovery_hashes") {
		s.db.Exec("ALTER TABLE users ADD COLUMN mfa_recovery_hashes TEXT NOT NULL DEFAULT '[]'")
	}
	if !columnExists(s.db, "users", "mfa_last_counter") {
		s.db.Exec("ALTER TABLE users ADD COLUMN mfa_last_counter INTEGER NOT NULL DEFAULT -1")
	}
	if !columnExists(s.db, "users", "mfa_enabled_at") {
		s.db.Exec("ALTER TABLE users ADD COLUMN mfa_enabled_at DATETIME")
	}
	if !columnExists(s.db, "sessions", "auth_state") {
		s.db.Exec("ALTER TABLE sessions ADD COLUMN auth_state TEXT NOT NULL DEFAULT 'active'")
	}
	if !columnExists(s.db, "sessions", "mfa_attempts") {
		s.db.Exec("ALTER TABLE sessions ADD COLUMN mfa_attempts INTEGER NOT NULL DEFAULT 0")
	}

	// Migrations for existing DBs — silently ignored if columns already exist
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN thread_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE connections ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE mcp_servers ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE agents ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE providers ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE channels ADD COLUMN project_id TEXT DEFAULT ''")
	// is_default removed — default is per-instance, stored in agents.config
	s.db.Exec("ALTER TABLE agents ADD COLUMN mode TEXT DEFAULT 'autonomous'")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN external_webhook_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN events TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN notify_agent INTEGER NOT NULL DEFAULT 0")
	// Source discriminator for the subscription. Default 'webhook'
	// preserves existing rows: token-keyed external delivery via
	// /webhooks/<token>. New value 'app_event' attaches the row to the
	// in-process AppEventBus instead, where slug carries
	// '<app_name>:<topic_pattern>' (e.g. 'tables:row.*'). Bridge
	// dispatcher fans bus events into the same core /event delivery
	// path webhooks already use.
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN source TEXT NOT NULL DEFAULT 'webhook'")
	// Highest bus seq we successfully delivered to the agent. On
	// apteva-server restart the dispatcher subscribes with
	// since=last_seq_delivered so events emitted while we were down
	// (within the bus's 256-event ring) replay automatically.
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN last_seq_delivered INTEGER NOT NULL DEFAULT 0")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN delivery TEXT NOT NULL DEFAULT 'webhook'")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN poll_config_json TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN poll_state_json TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN last_run_at DATETIME")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN next_run_at DATETIME")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN last_error TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN kind TEXT NOT NULL DEFAULT 'user'")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN match_json TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN wait_group_id TEXT NOT NULL DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN expires_at DATETIME")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN delete_on_match INTEGER NOT NULL DEFAULT 0")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sub_poll_due ON subscriptions(delivery, enabled, next_run_at)")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sub_ephemeral_wait ON subscriptions(kind, wait_group_id)")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_sub_ephemeral_expiry ON subscriptions(kind, expires_at)")
	// Older app-event / provider-trigger subscriptions used an empty
	// webhook_path because they do not expose a per-subscription public
	// webhook. The column is globally UNIQUE, so the first such row
	// blocked every later internal subscription. Give existing rows a
	// deterministic internal key; new rows use random internal keys.
	migrateEmptySubscriptionWebhookPaths(s.db)

	// Multi-connection support: dedupe any existing (user, project, name)
	// collisions in mcp_servers by suffixing all but the oldest row with
	// the row id, then enforce uniqueness with an index. Do the same for
	// connections keyed on (user, project, app_slug, name). Both are
	// idempotent on re-run because the suffixed names no longer collide.
	s.db.Exec(`
		UPDATE mcp_servers
		SET name = name || '-' || id
		WHERE id IN (
			SELECT m1.id FROM mcp_servers m1
			JOIN mcp_servers m2
			  ON m1.user_id = m2.user_id
			 AND COALESCE(m1.project_id,'') = COALESCE(m2.project_id,'')
			 AND m1.name = m2.name
			 AND m1.id > m2.id
		)
	`)
	s.db.Exec(`
		UPDATE connections
		SET name = name || '-' || id
		WHERE id IN (
			SELECT c1.id FROM connections c1
			JOIN connections c2
			  ON c1.user_id = c2.user_id
			 AND COALESCE(c1.project_id,'') = COALESCE(c2.project_id,'')
			 AND c1.app_slug = c2.app_slug
			 AND c1.name = c2.name
			 AND c1.id > c2.id
		)
	`)
	s.db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_servers_name ON mcp_servers(user_id, project_id, name)")
	s.db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_connections_name ON connections(user_id, project_id, app_slug, name)")

	// Unified connections + mcp_servers: source discriminator + hosted-provider refs
	s.db.Exec("ALTER TABLE connections ADD COLUMN source TEXT DEFAULT 'local'")
	s.db.Exec("ALTER TABLE connections ADD COLUMN provider_id INTEGER DEFAULT 0")
	s.db.Exec("ALTER TABLE connections ADD COLUMN external_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE mcp_servers ADD COLUMN transport TEXT DEFAULT 'stdio'")
	s.db.Exec("ALTER TABLE mcp_servers ADD COLUMN url TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE mcp_servers ADD COLUMN provider_id INTEGER DEFAULT 0")
	// Tool-level scoping. JSON array of allowed tool names. Empty string ('')
	// means "all tools exposed by the underlying source are enabled" — the
	// legacy behaviour we keep for existing rows. Populated means the MCP
	// endpoint only serves those specific tools and rejects any tools/call
	// targeting anything outside the list.
	s.db.Exec("ALTER TABLE mcp_servers ADD COLUMN allowed_tools TEXT NOT NULL DEFAULT ''")
	// upstream_id stores a durable identifier supplied by a remote MCP or app.
	// The local mcp_servers.id remains stable when an upstream identity changes.
	s.db.Exec("ALTER TABLE mcp_servers ADD COLUMN upstream_id TEXT NOT NULL DEFAULT ''")

	// Pending-OAuth state table for local catalog OAuth2 flows.
	s.db.Exec(`CREATE TABLE IF NOT EXISTS oauth_states (
		state TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		connection_id INTEGER NOT NULL,
		app_slug TEXT NOT NULL,
		pkce_verifier TEXT DEFAULT '',
		expires_at DATETIME NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	// App-initiated OAuth: when a sidecar app starts the dance via
	// platform.oauth.start, we record the install id + the URL to redirect
	// the browser to once the callback completes. Without these the
	// callback always lands on the dashboard's HTML success page; with
	// them set, we 302 the browser back into the app's panel so it can
	// pick up the dangling pending_account row by conn_id.
	s.db.Exec(`ALTER TABLE oauth_states ADD COLUMN app_install_id INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE oauth_states ADD COLUMN return_url TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE oauth_states ADD COLUMN purpose TEXT NOT NULL DEFAULT 'connect'`)
	// Connections gain owner_app_install_id so the platform can scope
	// list/disconnect operations and so the operator's Integrations admin
	// can hide app-owned connections (the app exposes them through its
	// own UI). Pre-existing rows have created_via='integration' and
	// owner_app_install_id=0 — the legacy meaning.
	s.db.Exec(`ALTER TABLE connections ADD COLUMN owner_app_install_id INTEGER NOT NULL DEFAULT 0`)
	// auto_mcp: when 1 (default), creating a connection via the
	// Integrations admin auto-spawns an mcp_servers row that exposes
	// every tool the integration declares to all agents in the
	// project. When 0, the connection exists but no MCP server is
	// created — the operator can still bind it to an app's
	// `requires.integrations` role, but agents won't see the tools
	// globally. Useful for "I want Facebook for the Social app, not
	// for every agent in the project." Operator can flip the flag
	// later via PATCH /connections/:id/expose.
	s.db.Exec(`ALTER TABLE connections ADD COLUMN auto_mcp INTEGER NOT NULL DEFAULT 1`)

	// ─── Runtime-backend columns (providers/connections fusion) ───
	//
	// runtime_config: non-secret runtime knobs — pinned model IDs, base
	// URL overrides, Ollama's model names. Deliberately NOT in
	// encrypted_credentials: these are preferences, not secrets, and the
	// pool resolver reads them on every agent boot.
	s.db.Exec(`ALTER TABLE connections ADD COLUMN runtime_config TEXT NOT NULL DEFAULT '{}'`)
	// is_primary: which connection supplies the credential when several
	// exist for the same app (e.g. three OpenCode Go keys). The old
	// providers table deduped implicitly by lowest id; making it explicit
	// lets the operator choose. Scope precedence is unchanged — a
	// project-scoped row still beats a global one; is_primary only breaks
	// ties *within* a scope.
	s.db.Exec(`ALTER TABLE connections ADD COLUMN is_primary INTEGER NOT NULL DEFAULT 0`)
	// legacy_provider_id: the providers.id this row was migrated from, or
	// 0. Cores built before the fusion construct their token-refresh URL
	// as /api/providers/<OPENAI_CODEX_PROVIDER_ID>/auth/runtime-token
	// from an env var we injected at spawn, so that id has to keep
	// resolving after the providers table is gone. See
	// resolveRuntimeTokenConnection in provider_auth.go.
	s.db.Exec(`ALTER TABLE connections ADD COLUMN legacy_provider_id INTEGER NOT NULL DEFAULT 0`)
	// At most one primary per user+project+app. Partial unique indexes are
	// supported by modernc.org/sqlite, so the invariant is enforced by the
	// DB rather than by handler code that can be bypassed.
	s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_connections_primary
		ON connections(user_id, project_id, app_slug) WHERE is_primary = 1`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_connections_legacy_provider
		ON connections(legacy_provider_id) WHERE legacy_provider_id != 0`)
	// Backfill: mark the lowest id in each group primary so the new
	// ORDER BY reproduces the old "first wins" behavior as a stated fact
	// rather than a coincidence. HAVING SUM(is_primary) = 0 makes this
	// idempotent — once an operator picks a primary, reboots leave it be.
	s.db.Exec(`UPDATE connections SET is_primary = 1 WHERE id IN (
		SELECT MIN(id) FROM connections
		GROUP BY user_id, project_id, app_slug
		HAVING SUM(is_primary) = 0
	)`)

	// Seed new provider types on existing DBs (idempotent). The initial
	// CREATE-TABLE seed above only fires on fresh schemas; this block
	// catches upgrades so new provider types show up in the dashboard's
	// "add provider" picker after a binary upgrade without requiring a
	// DB reset.
	s.db.Exec(`INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
		(10, 'llm', 'NVIDIA', 'LLM inference via NVIDIA NIM (integrate.api.nvidia.com)', '["NVIDIA_API_KEY"]', 1, 14)`)
	s.db.Exec(`INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
		(16, 'llm', 'Google', 'Gemini models via the Google Generative Language API', '["GOOGLE_API_KEY"]', 1, 13)`)
	s.db.Exec(`INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
		(11, 'browser', 'Local Browser', 'Local Chromium via chromedp (requires Chromium in the runtime image)', '[]', 0, 41)`)
	s.db.Exec(`INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
		(12, 'browser', 'Remote CDP', 'Connect to an existing Chrome over CDP (ws:// or http://)', '["CDP_URL"]', 1, 42)`)
	// Name is the user-visible label shown in the "Add provider" dropdown.
	// Lookup keys (createProviderByName / FetchModels) normalize the name
	// via providerKeyFromName below — lowercase + spaces→hyphens — so
	// pretty display names work without a separate column.
	s.db.Exec(`INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
			(13, 'llm', 'OpenCode Go', 'Flat-rate gateway ($10/mo) for Kimi K2.6, Qwen, GLM, MiMo, DeepSeek and more (opencode.ai/go)', '["OPENCODE_GO_API_KEY"]', 1, 15)`)
	s.db.Exec(`INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
				(14, 'llm', 'Venice', 'Privacy-focused inference gateway — Llama, Qwen, GLM, Mistral plus Claude / Grok / Gemini reseller variants (venice.ai)', '["VENICE_API_KEY"]', 1, 16)`)
	s.db.Exec(`UPDATE provider_types
			SET fields='["OLLAMA_HOST","OLLAMA_MODEL","OLLAMA_EMBED_MODEL","OLLAMA_EMBED_DIM"]'
			WHERE id=4 AND name='Ollama'`)
	// Provider auth metadata: older DBs only have fields/requires_credentials.
	// These columns let the dashboard render API-key, device-code, browser
	// OAuth, and future auth methods without provider-specific routes.
	s.db.Exec("ALTER TABLE provider_types ADD COLUMN auth_type TEXT DEFAULT 'api_key'")
	s.db.Exec("ALTER TABLE provider_types ADD COLUMN auth_provider TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE provider_types ADD COLUMN runtime_status TEXT DEFAULT 'available'")
	s.db.Exec("ALTER TABLE provider_types ADD COLUMN capabilities TEXT DEFAULT '[]'")
	s.db.Exec(`UPDATE provider_types
			SET auth_type='api_key',
			    auth_provider=lower(replace(name, ' ', '-')),
			    runtime_status='available'
			WHERE auth_type IS NULL OR auth_type=''`)
	s.db.Exec(`INSERT OR IGNORE INTO provider_types
			(id, type, name, description, fields, requires_credentials, auth_type, auth_provider, runtime_status, capabilities, sort_order)
			VALUES
			(15, 'llm', 'OpenAI Codex', 'ChatGPT subscription-backed Codex auth via the Codex Responses runtime.', '[]', 1, 'oauth_device_code', 'openai-codex', 'available', '["llm","subscription","subscription_usage","codex_responses","streaming","native_tools"]', 17)`)
	s.db.Exec(`UPDATE provider_types
			SET description='ChatGPT subscription-backed Codex auth via the Codex Responses runtime.',
			    runtime_status='available',
			    capabilities='["llm","subscription","subscription_usage","codex_responses","streaming","native_tools"]'
			WHERE id=15 AND auth_provider='openai-codex'`)
	s.db.Exec(`INSERT OR IGNORE INTO provider_types
			(id, type, name, description, fields, requires_credentials, auth_type, auth_provider, runtime_status, capabilities, sort_order)
			VALUES
			(17, 'llm', 'xAI', 'Grok language models through the xAI OpenAI-compatible API.', '["XAI_API_KEY"]', 1, 'api_key', 'xai', 'available', '["llm","streaming","native_tools","reasoning","vision"]', 18)`)
	s.db.Exec(`UPDATE provider_types
			SET description='Grok language models through the xAI OpenAI-compatible API.',
			    fields='["XAI_API_KEY"]',
			    auth_type='api_key',
			    auth_provider='xai',
			    runtime_status='available',
			    capabilities='["llm","streaming","native_tools","reasoning","vision"]',
			    sort_order=18
			WHERE id=17 AND name='xAI'`)

	// Fix historical row 8: it was seeded with type='browser' but its
	// fields / name describe Browserbase. Flip the type to 'browserbase'
	// on existing installs. Idempotent — re-running is a no-op once the
	// row has the correct type.
	s.db.Exec(`UPDATE provider_types
		SET type='browserbase',
		    description='Cloud browser automation via Browserbase',
		    fields='["BROWSERBASE_API_KEY","BROWSERBASE_PROJECT_ID"]'
		WHERE id = 8 AND type='browser'`)
	// And rewrite any providers rows already created against the broken
	// seed so they start working immediately. The encrypted_data still
	// holds valid Browserbase credentials — only the type column is wrong.
	s.db.Exec(`UPDATE providers
		SET type='browserbase'
		WHERE type='browser' AND provider_type_id=8`)

	// Browser automation is now delivered through apps/integrations, not
	// generic provider rows. Keep historical provider_types around so existing
	// saved providers can still be inspected/deactivated, but stop presenting
	// them as addable runtime providers.
	s.db.Exec(`UPDATE provider_types
		SET runtime_status='unsupported'
		WHERE type IN ('browser', 'browserbase', 'steel', 'browser-engine')
		   OR name IN ('Browserbase', 'Steel', 'Browser Engine', 'Local Browser', 'Remote CDP')`)

	// Server-wide settings table — simple key/value bag for things the
	// admin needs to configure from the dashboard, not just from env
	// vars at boot. Today: public_url. Tomorrow: anything else that
	// belongs at the server level rather than per-user/per-project.
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS server_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS user_preferences (
			user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			language TEXT NOT NULL DEFAULT '',
			ui_layout TEXT NOT NULL DEFAULT '{}',
			ui_layout_revision INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	s.db.Exec("ALTER TABLE user_preferences ADD COLUMN ui_layout TEXT NOT NULL DEFAULT '{}'")
	s.db.Exec("ALTER TABLE user_preferences ADD COLUMN ui_layout_revision INTEGER NOT NULL DEFAULT 0")
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS mobile_push_subscriptions (
			id TEXT PRIMARY KEY,
			installation_id TEXT NOT NULL UNIQUE,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			relay_device_id TEXT NOT NULL,
			relay_grant_encrypted TEXT NOT NULL,
			grant_expires_at DATETIME NOT NULL,
			platform TEXT NOT NULL DEFAULT 'ios'
				CHECK(platform IN ('ios', 'android')),
			bundle_id TEXT NOT NULL,
			environment TEXT NOT NULL CHECK(environment IN ('sandbox', 'production')),
			app_version TEXT NOT NULL DEFAULT '',
			device_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active'
				CHECK(status IN ('active', 'invalid', 'revoked')),
			last_inbox_message_id INTEGER NOT NULL DEFAULT 0,
			last_badge INTEGER NOT NULL DEFAULT 0,
			retry_count INTEGER NOT NULL DEFAULT 0,
			next_retry_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			last_success_at DATETIME,
			last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_mobile_push_subscriptions_delivery
			ON mobile_push_subscriptions(status, next_retry_at);
		CREATE INDEX IF NOT EXISTS idx_mobile_push_subscriptions_user
			ON mobile_push_subscriptions(user_id, status);
	`)
	// Upgrade servers that created the original iOS-only subscription table.
	// SQLite applies the DEFAULT to existing rows, preserving their behavior.
	s.db.Exec(`ALTER TABLE mobile_push_subscriptions
		ADD COLUMN platform TEXT NOT NULL DEFAULT 'ios'
		CHECK(platform IN ('ios', 'android'))`)

	// Migrate legacy local mcp_servers rows: the name was written as
	// conn.AppName (display name with spaces like "OmniKit Storage") but
	// it should be the slug (e.g. "omnikit-storage"). Sub-threads look
	// up MCP servers by exact-match name at spawn time, and the LLM
	// infers slug-form from tool prefixes — so display-name rows cause
	// silent "worker got 0 tools" bugs.
	//
	// This UPDATE rewrites every local row to use the linked connection's
	// app_slug as the name and keeps the pretty form in description.
	// Safe to re-run: idempotent because name = app_slug is the new
	// invariant, subsequent runs are a no-op.
	s.db.Exec(`
		UPDATE mcp_servers
		SET
			name = COALESCE((SELECT app_slug FROM connections WHERE id = mcp_servers.connection_id), name),
			description = COALESCE((SELECT app_name FROM connections WHERE id = mcp_servers.connection_id), description)
		WHERE source = 'local' AND connection_id > 0
	`)

	// Apps system — see apps_loader.go. Keeps the index of every
	// installed app, plus a per-install row for project/global scope
	// and an instance-binding table the agent runtime reads.
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS apps (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL UNIQUE,
			source        TEXT NOT NULL,        -- 'git' | 'registry' | 'builtin'
			repo          TEXT NOT NULL DEFAULT '',
			ref           TEXT NOT NULL DEFAULT '',
			manifest_json TEXT NOT NULL,
			registered_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS app_installs (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			app_id               INTEGER NOT NULL REFERENCES apps(id),
			project_id           TEXT DEFAULT '',  -- '' = global
			service_name         TEXT NOT NULL DEFAULT '',
			sidecar_url_override TEXT NOT NULL DEFAULT '',  -- literal URL for local dev / non-orchestrator deploys
			config_encrypted     TEXT DEFAULT '',
			status               TEXT NOT NULL DEFAULT 'pending', -- pending|running|error|disabled
			upgrade_policy       TEXT NOT NULL DEFAULT 'manual',  -- manual|auto-patch|auto-minor
			version              TEXT NOT NULL DEFAULT '',
			manifest_json        TEXT NOT NULL DEFAULT '',
			pending_manifest_json TEXT NOT NULL DEFAULT '',
			source               TEXT NOT NULL DEFAULT '',
			repo                 TEXT NOT NULL DEFAULT '',
			ref                  TEXT NOT NULL DEFAULT '',
			permissions_json     TEXT NOT NULL DEFAULT '[]',
			installed_at         DATETIME DEFAULT CURRENT_TIMESTAMP,
				installed_by         INTEGER DEFAULT 0,
				app_token_hash        TEXT NOT NULL DEFAULT '',
				app_token_encrypted   TEXT NOT NULL DEFAULT '',
				default_for_new_agents INTEGER NOT NULL DEFAULT 0,
				UNIQUE(app_id, project_id)
			)
		`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN app_token_hash TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN app_token_encrypted TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN default_for_new_agents INTEGER NOT NULL DEFAULT 0`)
	// Per-install source snapshot. The apps row tracks marketplace/latest
	// metadata; these fields describe what this project is actually running.
	// Backfill keeps upgrades from one project changing another project's
	// manifest, MCP surface, permissions, or restart behavior.
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN manifest_json TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN pending_manifest_json TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN source TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN repo TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN ref TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`UPDATE app_installs
		SET manifest_json = COALESCE((SELECT manifest_json FROM apps WHERE apps.id = app_installs.app_id), '')
		WHERE manifest_json = ''`)
	s.db.Exec(`UPDATE app_installs
		SET source = COALESCE((SELECT source FROM apps WHERE apps.id = app_installs.app_id), '')
		WHERE source = ''`)
	s.db.Exec(`UPDATE app_installs
		SET repo = COALESCE((SELECT repo FROM apps WHERE apps.id = app_installs.app_id), '')
		WHERE repo = ''`)
	s.db.Exec(`UPDATE app_installs
		SET ref = COALESCE((SELECT ref FROM apps WHERE apps.id = app_installs.app_id), '')
		WHERE ref = ''`)
	s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_app_installs_token_hash ON app_installs(app_token_hash) WHERE app_token_hash != ''`)
	// Forward-add the column for installs created before this field existed.
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN sidecar_url_override TEXT NOT NULL DEFAULT ''`)
	// Local-spawn supervisor state: PID of the running child + path to
	// the cached binary on disk. Empty for orchestrator-managed apps.
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN local_pid INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN local_bin_path TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN local_port INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`)
	// Live phase string written by the source/local supervisor while
	// status='pending' so the dashboard can show "Cloning…", "Building…",
	// "Starting sidecar…" instead of an opaque pending pill.
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN status_message TEXT NOT NULL DEFAULT ''`)
	// integration_bindings: JSON {role: connection_id|install_id|null}
	// Populated at install time from the manifest's requires.integrations.
	// null distinguishes "operator declined optional dep" from "manifest
	// added the role in a later version, never asked the operator".
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN integration_bindings TEXT NOT NULL DEFAULT '{}'`)
	// has_pending_options flag: set when a previously-unbinded optional
	// dep now has a compatible target available (e.g. user installed
	// the storage app after media-studio). Dashboard surfaces a
	// "configure" banner on the install detail page.
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN has_pending_options INTEGER NOT NULL DEFAULT 0`)
	// Provider webhook registrations owned by an app's bound integration.
	// Provider credentials stay on the connection; signing secrets returned
	// during registration are encrypted here and are only used by the
	// authenticated app callback verification path.
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS app_integration_webhooks (
			id                    INTEGER PRIMARY KEY AUTOINCREMENT,
			install_id            INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
			role                  TEXT NOT NULL,
			connection_id         INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
			provider_slug         TEXT NOT NULL,
			callback_path         TEXT NOT NULL,
			callback_url          TEXT NOT NULL,
			events_json           TEXT NOT NULL DEFAULT '[]',
			external_webhook_id   TEXT NOT NULL,
			secret_encrypted      TEXT NOT NULL,
			status                TEXT NOT NULL DEFAULT 'ready',
			last_error            TEXT NOT NULL DEFAULT '',
			registered_at         TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at            TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(install_id, role)
		)
	`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_app_integration_webhooks_connection ON app_integration_webhooks(connection_id)`)
	// created_via on connections: 'integration' (default — top-level
	// install via the Integrations page, auto-creates an mcp_servers
	// row) vs 'app_install' (created inside an app's dependency flow,
	// no auto-MCP).
	s.db.Exec(`ALTER TABLE connections ADD COLUMN created_via TEXT NOT NULL DEFAULT 'integration'`)
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS app_agent_bindings (
			install_id   INTEGER NOT NULL REFERENCES app_installs(id),
			agent_id  INTEGER NOT NULL REFERENCES agents(id),
			enabled      INTEGER NOT NULL DEFAULT 1,
			PRIMARY KEY (install_id, agent_id)
		)
	`)
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS delegated_provider_usage (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			grant_id             TEXT NOT NULL DEFAULT '',
			connection_id         INTEGER NOT NULL DEFAULT 0,
			parent_connection_id  INTEGER NOT NULL DEFAULT 0,
			child_install_id      INTEGER NOT NULL DEFAULT 0,
			app_slug              TEXT NOT NULL DEFAULT '',
			tool                  TEXT NOT NULL DEFAULT '',
			resource              TEXT NOT NULL DEFAULT 'provider.connection',
			quantity              INTEGER NOT NULL DEFAULT 1,
			status                TEXT NOT NULL DEFAULT '',
			error                 TEXT NOT NULL DEFAULT '',
			direction             TEXT NOT NULL DEFAULT '',
			created_at            DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_delegated_provider_usage_grant_time ON delegated_provider_usage(grant_id, created_at)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_delegated_provider_usage_conn_time ON delegated_provider_usage(connection_id, created_at)`)
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS integration_usage_events (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at           DATETIME DEFAULT CURRENT_TIMESTAMP,
			project_id           TEXT NOT NULL DEFAULT '',
			caller_install_id    INTEGER NOT NULL DEFAULT 0,
			caller_app_name      TEXT NOT NULL DEFAULT '',
			connection_id         INTEGER NOT NULL DEFAULT 0,
			parent_connection_id  INTEGER NOT NULL DEFAULT 0,
			app_slug              TEXT NOT NULL DEFAULT '',
			tool                  TEXT NOT NULL DEFAULT '',
			grant_id              TEXT NOT NULL DEFAULT '',
			grant_resource        TEXT NOT NULL DEFAULT '',
			child_install_id      INTEGER NOT NULL DEFAULT 0,
			child_connection_id   INTEGER NOT NULL DEFAULT 0,
			direction             TEXT NOT NULL DEFAULT 'local',
			quantity              INTEGER NOT NULL DEFAULT 1,
			unit                  TEXT NOT NULL DEFAULT 'request',
			status                TEXT NOT NULL DEFAULT '',
			error                 TEXT NOT NULL DEFAULT '',
			provider_request_id   TEXT NOT NULL DEFAULT '',
			metadata_json         TEXT NOT NULL DEFAULT '{}'
		)
	`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_integration_usage_time ON integration_usage_events(created_at)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_integration_usage_connection_time ON integration_usage_events(connection_id, created_at)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_integration_usage_grant_time ON integration_usage_events(grant_id, created_at)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_integration_usage_app_tool_time ON integration_usage_events(app_slug, tool, created_at)`)

	// Skills — markdown-bodied playbooks the agent will load on
	// demand. v1 stores + serves them; agent integration is a
	// separate task. Three sources:
	//   'app'     — shipped by an installed app via provides.skills
	//   'user'    — operator-authored in the dashboard
	//   'builtin' — registered at server boot (none yet, slot reserved)
	// install_id ties app-shipped skills to their install for cascade
	// delete; user/builtin rows leave it NULL. UNIQUE(project_id, slug)
	// enforces one logical row per project; UNIQUE(project_id, command)
	// where command != '' keeps slash commands collision-free.
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS skills (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			slug            TEXT NOT NULL,
			name            TEXT NOT NULL,
			description     TEXT NOT NULL,
			body            TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL,
			install_id      INTEGER REFERENCES app_installs(id) ON DELETE CASCADE,
			project_id      TEXT NOT NULL DEFAULT '',
			command         TEXT NOT NULL DEFAULT '',
			metadata_json   TEXT NOT NULL DEFAULT '{}',
			enabled         INTEGER NOT NULL DEFAULT 1,
			version         TEXT NOT NULL DEFAULT '1.0.0',
			created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (project_id, slug)
		)
	`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_skills_project ON skills(project_id)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_skills_install ON skills(install_id) WHERE install_id IS NOT NULL`)
	// Partial unique index — only enforces uniqueness when command is
	// set, so the empty-string default doesn't conflict across rows.
	s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_skills_command ON skills(project_id, command) WHERE command != ''`)

	// Per-(install, instance) authorization grants. Apps that opt in
	// via provides.permissions get scoped per-agent access; apps that
	// don't keep their pre-permissions full-access behavior because
	// default_effect defaults to 'allow' and the SDK's gate only
	// fires when manifest's mcp_tools[].requires is set.
	//
	// effect: 'allow' | 'deny'
	// resource: matcher-specific pattern (glob, id_set, ...) — '*' is
	//           always-match regardless of matcher.
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS app_grants (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			install_id   INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
			agent_id  INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
			effect       TEXT NOT NULL CHECK (effect IN ('allow','deny')),
			permission   TEXT NOT NULL,
			resource     TEXT NOT NULL DEFAULT '*',
			created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
			created_by   TEXT NOT NULL DEFAULT '',
			UNIQUE(install_id, agent_id, effect, permission, resource)
		)
	`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_app_grants_lookup ON app_grants(install_id, agent_id)`)
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS app_grant_defaults (
			install_id      INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
			agent_id        INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
			default_effect  TEXT NOT NULL CHECK (default_effect IN ('allow','deny')),
			updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (install_id, agent_id)
		)
	`)
	// default_effect — 'allow' (default, full back-compat) or 'deny'
	// for fail-closed installs. Per-install knob; the dashboard's
	// "Access" tab flips it.
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN default_effect TEXT NOT NULL DEFAULT 'allow'`)

	// ─── Multi-user + roles ───────────────────────────────────────────
	//
	// Adds two-tier role system:
	//   - users.role: platform-level role ('user' | 'admin'). Admin is
	//     an implicit owner on every project (the authz helper
	//     short-circuits via requireProjectAccess).
	//   - project_members: which users have explicit access to which
	//     project, with a per-project role ('viewer' | 'editor' | 'owner').
	//   - project_invites: pending invitations by email, accepted via
	//     a token that doubles as the invite-URL slug.
	//
	// Migration backfill (idempotent — re-running on an already-migrated
	// DB is a safe no-op thanks to OR-IGNORE):
	//   1. lowest-id user → role='admin' (the operator that ran setup;
	//      additional users stay 'user' and the admin can promote them
	//      manually from /admin/users).
	//   2. every existing project gets a project_members row with
	//      role='owner' for its projects.user_id, so the new authz
	//      helper finds an explicit ownership row matching the implicit
	//      single-user environment.
	s.db.Exec(`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'`)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS project_members (
		project_id TEXT    NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		user_id    INTEGER NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
		role       TEXT    NOT NULL CHECK (role IN ('viewer','editor','owner')),
		added_by   INTEGER REFERENCES users(id),
		added_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (project_id, user_id)
	)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members(user_id)`)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS project_invites (
		id          TEXT PRIMARY KEY,
		project_id  TEXT    NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		email       TEXT    NOT NULL,
		role        TEXT    NOT NULL CHECK (role IN ('viewer','editor','owner')),
		invited_by  INTEGER NOT NULL REFERENCES users(id),
		expires_at  DATETIME NOT NULL,
		accepted_at DATETIME,
		created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_project_invites_project ON project_invites(project_id)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_project_invites_email   ON project_invites(email)`)
	// Backfill: lowest-id user becomes admin (the setup operator). If
	// there are no users yet (fresh DB), the UPDATE matches 0 rows —
	// the first handleRegister will set role='admin' explicitly.
	s.db.Exec(`UPDATE users SET role = 'admin'
	            WHERE id = (SELECT MIN(id) FROM users)
	              AND role = 'user'`)
	// Backfill project_members from existing projects.user_id. The
	// INSERT OR IGNORE makes this idempotent across boots.
	s.db.Exec(`INSERT OR IGNORE INTO project_members (project_id, user_id, role, added_by)
	           SELECT id, user_id, 'owner', user_id FROM projects`)

	if err := s.repairLegacyAgentForeignKeys(); err != nil {
		return fmt.Errorf("repair legacy agent foreign keys: %w", err)
	}
	if err := s.removeLegacyHostedIntegrationProvider(); err != nil {
		return fmt.Errorf("remove legacy hosted integration provider: %w", err)
	}
	return s.validateMigratedSchema()
}

// removeLegacyHostedIntegrationProvider removes the retired provider-backed
// integration path. The ordinary catalog integration remains available and
// uses source=local like every other data-driven integration.
func (s *Store) removeLegacyHostedIntegrationProvider() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	legacyProviderIDs := `SELECT id FROM providers WHERE provider_type_id=9`
	legacyConnectionIDs := `SELECT id FROM connections WHERE source='composio' OR provider_id IN (` + legacyProviderIDs + `)`
	statements := []string{
		`DELETE FROM subscriptions WHERE connection_id IN (` + legacyConnectionIDs + `)`,
		`DELETE FROM mcp_servers WHERE connection_id IN (` + legacyConnectionIDs + `) OR provider_id IN (` + legacyProviderIDs + `)`,
		`DELETE FROM connections WHERE id IN (` + legacyConnectionIDs + `)`,
		`DELETE FROM providers WHERE provider_type_id=9`,
		`DELETE FROM provider_types WHERE id=9`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) validateMigratedSchema() error {
	requiredTables := []string{"users", "agents", "projects", "project_members", "app_installs", "delegated_access_policies", "skills", "telemetry", "mobile_push_subscriptions"}
	for _, table := range requiredTables {
		if !tableExists(s.db, table) {
			return fmt.Errorf("migration incomplete: required table %s is missing", table)
		}
	}
	requiredColumns := map[string][]string{
		"agents":       {"project_id", "core_api_key"},
		"app_installs": {"app_token_hash", "app_token_encrypted", "integration_bindings"},
		"api_keys":     {"kind", "scopes", "allowed_origins"},
	}
	for table, columns := range requiredColumns {
		for _, column := range columns {
			if !columnExists(s.db, table, column) {
				return fmt.Errorf("migration incomplete: required column %s.%s is missing", table, column)
			}
		}
	}
	return nil
}

func migrateEmptySubscriptionWebhookPaths(db *sql.DB) {
	if db == nil {
		return
	}
	db.Exec(`
		UPDATE subscriptions
		SET webhook_path =
			CASE
				WHEN COALESCE(source,'') = 'app_event' THEN 'app-event-' || id
				WHEN COALESCE(delivery,'') = 'poll' THEN 'poll-' || id
				WHEN COALESCE(external_webhook_id,'') != '' THEN 'external-' || id
				ELSE 'internal-' || id
			END
		WHERE webhook_path = ''
	`)
}

func (s *Store) Close() error {
	return s.db.Close()
}

// --- Users ---

func (s *Store) CreateUser(email, passwordHash string) (*User, error) {
	result, err := s.db.Exec(
		"INSERT INTO users (email, password_hash) VALUES (?, ?)",
		email, passwordHash,
	)
	if err != nil {
		return nil, fmt.Errorf("user exists or db error: %w", err)
	}
	id, _ := result.LastInsertId()
	return &User{ID: id, Email: email, CreatedAt: time.Now()}, nil
}

func (s *Store) HasUsers() bool {
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count > 0
}

func (s *Store) GetUserByEmail(email string) (*User, error) {
	var u User
	var createdAt string
	var onboardedAt sql.NullString
	var mfaEnabledAt sql.NullString
	err := s.db.QueryRow(
		`SELECT id, email, password_hash, COALESCE(role,'user'), created_at, onboarded_at,
		        COALESCE(mfa_type,''), COALESCE(mfa_secret_encrypted,''),
		        COALESCE(mfa_recovery_hashes,'[]'), COALESCE(mfa_last_counter,-1), mfa_enabled_at
		   FROM users WHERE email = ?`, email,
	).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Role, &createdAt, &onboardedAt,
		&u.MFAType, &u.MFASecretEncrypted, &u.MFARecoveryHashes, &u.MFALastCounter, &mfaEnabledAt,
	)
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = parseTime(createdAt)
	if onboardedAt.Valid {
		if t, err := parseTime(onboardedAt.String); err == nil {
			u.OnboardedAt = &t
		}
	}
	if mfaEnabledAt.Valid {
		if t, err := parseTime(mfaEnabledAt.String); err == nil {
			u.MFAEnabledAt = &t
		}
	}
	return &u, nil
}

// GetUserByID fetches a user row by primary key. Used by /auth/me and
// any handler that needs to reply with the caller's email when only
// the session's user_id is known.
func (s *Store) GetUserByID(id int64) (*User, error) {
	var u User
	var createdAt string
	var onboardedAt sql.NullString
	var mfaEnabledAt sql.NullString
	err := s.db.QueryRow(
		`SELECT id, email, password_hash, COALESCE(role,'user'), created_at, onboarded_at,
		        COALESCE(mfa_type,''), COALESCE(mfa_secret_encrypted,''),
		        COALESCE(mfa_recovery_hashes,'[]'), COALESCE(mfa_last_counter,-1), mfa_enabled_at
		   FROM users WHERE id = ?`, id,
	).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Role, &createdAt, &onboardedAt,
		&u.MFAType, &u.MFASecretEncrypted, &u.MFARecoveryHashes, &u.MFALastCounter, &mfaEnabledAt,
	)
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = parseTime(createdAt)
	if onboardedAt.Valid {
		if t, err := parseTime(onboardedAt.String); err == nil {
			u.OnboardedAt = &t
		}
	}
	if mfaEnabledAt.Valid {
		if t, err := parseTime(mfaEnabledAt.String); err == nil {
			u.MFAEnabledAt = &t
		}
	}
	return &u, nil
}

// MarkUserOnboarded stamps onboarded_at = now for a user that hasn't
// finished the welcome flow yet. Idempotent: the IS NULL guard means a
// re-call won't overwrite the original timestamp.
func (s *Store) MarkUserOnboarded(userID int64) error {
	_, err := s.db.Exec(
		"UPDATE users SET onboarded_at = CURRENT_TIMESTAMP WHERE id = ? AND onboarded_at IS NULL",
		userID,
	)
	return err
}

func (s *Store) GetUserLanguage(userID int64) string {
	var language string
	err := s.db.QueryRow("SELECT language FROM user_preferences WHERE user_id = ?", userID).Scan(&language)
	if err != nil {
		return ""
	}
	return language
}

func (s *Store) SetUserLanguage(userID int64, language string) error {
	_, err := s.db.Exec(
		`INSERT INTO user_preferences (user_id, language, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(user_id) DO UPDATE SET language = excluded.language, updated_at = CURRENT_TIMESTAMP`,
		userID, language,
	)
	return err
}

func (s *Store) GetUserUILayout(userID int64) json.RawMessage {
	raw, _ := s.GetUserUILayoutWithRevision(userID)
	return raw
}

func (s *Store) GetUserUILayoutWithRevision(userID int64) (json.RawMessage, int64) {
	var raw string
	var revision int64
	if err := s.db.QueryRow("SELECT ui_layout, COALESCE(ui_layout_revision, 0) FROM user_preferences WHERE user_id = ?", userID).Scan(&raw, &revision); err != nil {
		return json.RawMessage(`{}`), 0
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || !json.Valid([]byte(raw)) {
		return json.RawMessage(`{}`), revision
	}
	return json.RawMessage(raw), revision
}

func (s *Store) SetUserUILayout(userID int64, layout json.RawMessage) error {
	raw := strings.TrimSpace(string(layout))
	if raw == "" {
		raw = "{}"
	}
	if len(raw) > 128<<10 || !json.Valid([]byte(raw)) {
		return errors.New("ui_layout must be valid JSON no larger than 128 KiB")
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return errors.New("ui_layout must be a JSON object")
	}
	_, err := s.db.Exec(
		`INSERT INTO user_preferences (user_id, ui_layout, ui_layout_revision, updated_at)
		 VALUES (?, ?, 1, CURRENT_TIMESTAMP)
		 ON CONFLICT(user_id) DO UPDATE SET
			ui_layout = excluded.ui_layout,
			ui_layout_revision = user_preferences.ui_layout_revision + 1,
			updated_at = CURRENT_TIMESTAMP`,
		userID, raw,
	)
	return err
}

var errUILayoutConflict = errors.New("ui layout changed in another session")

// PatchUserUILayoutSurface atomically replaces one project's surface while
// retaining every other project, surface, and dormant widget configuration.
// expectedRevision is optional; when supplied it prevents an older browser
// snapshot from overwriting a newer edit.
func (s *Store) PatchUserUILayoutSurface(userID int64, projectID, surface string, value json.RawMessage, expectedRevision *int64) (json.RawMessage, int64, error) {
	if len(value) > 64<<10 || !json.Valid(value) {
		return nil, 0, errors.New("surface layout must be valid JSON no larger than 64 KiB")
	}
	if _, err := s.db.Exec(
		`INSERT INTO user_preferences (user_id, ui_layout, ui_layout_revision, updated_at)
		 VALUES (?, '{}', 0, CURRENT_TIMESTAMP)
		 ON CONFLICT(user_id) DO NOTHING`, userID,
	); err != nil {
		return nil, 0, err
	}

	for attempts := 0; attempts < 3; attempts++ {
		current, revision := s.GetUserUILayoutWithRevision(userID)
		if expectedRevision != nil && *expectedRevision != revision {
			return current, revision, errUILayoutConflict
		}
		var document map[string]any
		if err := json.Unmarshal(current, &document); err != nil || document == nil {
			document = map[string]any{}
		}
		projects, _ := document["projects"].(map[string]any)
		if projects == nil {
			projects = map[string]any{}
			document["projects"] = projects
		}
		project, _ := projects[projectID].(map[string]any)
		if project == nil {
			project = map[string]any{}
			projects[projectID] = project
		}
		slots, _ := project["slots"].(map[string]any)
		if slots == nil {
			slots = map[string]any{}
			project["slots"] = slots
		}
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, revision, err
		}
		slots[surface] = decoded
		next, err := json.Marshal(document)
		if err != nil {
			return nil, revision, err
		}
		if len(next) > 128<<10 {
			return nil, revision, errors.New("ui_layout must be no larger than 128 KiB")
		}
		result, err := s.db.Exec(
			`UPDATE user_preferences
			 SET ui_layout=?, ui_layout_revision=ui_layout_revision+1, updated_at=CURRENT_TIMESTAMP
			 WHERE user_id=? AND ui_layout_revision=?`, string(next), userID, revision,
		)
		if err != nil {
			return nil, revision, err
		}
		changed, _ := result.RowsAffected()
		if changed == 1 {
			return json.RawMessage(next), revision + 1, nil
		}
		if expectedRevision != nil {
			current, currentRevision := s.GetUserUILayoutWithRevision(userID)
			return current, currentRevision, errUILayoutConflict
		}
	}
	current, revision := s.GetUserUILayoutWithRevision(userID)
	return current, revision, errUILayoutConflict
}

// UpdateUserPassword rewrites a user's bcrypt hash. The caller must
// have already verified the old password — this only enforces that
// the target row exists.
func (s *Store) UpdateUserPassword(userID int64, newHash string) error {
	res, err := s.db.Exec(
		"UPDATE users SET password_hash = ? WHERE id = ?",
		newHash, userID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %d not found", userID)
	}
	return nil
}

func (s *Store) BeginTOTPEnrollment(userID int64, encryptedSecret string) error {
	res, err := s.db.Exec(
		`UPDATE users
		    SET mfa_type='totp_pending', mfa_secret_encrypted=?,
		        mfa_recovery_hashes='[]', mfa_last_counter=-1, mfa_enabled_at=NULL
		  WHERE id=? AND COALESCE(mfa_type,'') != 'totp'`,
		encryptedSecret, userID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("MFA is already enabled")
	}
	return nil
}

func (s *Store) ConfirmTOTPEnrollment(userID, counter int64, recoveryHashes []string) error {
	raw, err := json.Marshal(recoveryHashes)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE users
		    SET mfa_type='totp', mfa_recovery_hashes=?, mfa_last_counter=?,
		        mfa_enabled_at=CURRENT_TIMESTAMP
		  WHERE id=? AND mfa_type='totp_pending' AND mfa_secret_encrypted != ''`,
		string(raw), counter, userID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("TOTP enrollment is not pending")
	}
	return nil
}

// AdvanceMFACounter atomically prevents reuse of a TOTP time step, including
// concurrent attempts arriving at different server handlers.
func (s *Store) AdvanceMFACounter(userID, counter int64) error {
	res, err := s.db.Exec(
		`UPDATE users SET mfa_last_counter=?
		  WHERE id=? AND mfa_type='totp' AND mfa_last_counter < ?`,
		counter, userID, counter,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("TOTP code was already used")
	}
	return nil
}

func (s *Store) ConsumeMFARecoveryHash(userID int64, wanted string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRow(
		"SELECT COALESCE(mfa_recovery_hashes,'[]') FROM users WHERE id=? AND mfa_type='totp'",
		userID,
	).Scan(&raw); err != nil {
		return 0, err
	}
	var hashes []string
	if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
		return 0, err
	}
	remaining := make([]string, 0, len(hashes))
	found := false
	for _, hash := range hashes {
		if !found && hash == wanted {
			found = true
			continue
		}
		remaining = append(remaining, hash)
	}
	if !found {
		return 0, fmt.Errorf("invalid recovery code")
	}
	next, err := json.Marshal(remaining)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("UPDATE users SET mfa_recovery_hashes=? WHERE id=?", string(next), userID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(remaining), nil
}

func (s *Store) ReplaceMFARecoveryHashes(userID int64, hashes []string) error {
	raw, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		"UPDATE users SET mfa_recovery_hashes=? WHERE id=? AND mfa_type='totp'",
		string(raw), userID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("MFA is not enabled")
	}
	return nil
}

func (s *Store) DisableMFA(userID int64) error {
	res, err := s.db.Exec(
		`UPDATE users
		    SET mfa_type='', mfa_secret_encrypted='', mfa_recovery_hashes='[]',
		        mfa_last_counter=-1, mfa_enabled_at=NULL
		  WHERE id=?`, userID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("user %d not found", userID)
	}
	return nil
}

func recoveryHashCount(raw string) int {
	var hashes []string
	if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
		return 0
	}
	return len(hashes)
}

// ListUsers returns every user row, ordered by id so user_id=1 (the
// admin) always comes first. Used by the /users endpoint.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT id, email, COALESCE(role,'user'), created_at FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var createdAt string
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &createdAt); err != nil {
			return nil, err
		}
		u.CreatedAt, _ = parseTime(createdAt)
		out = append(out, u)
	}
	return out, nil
}

// CountUserResources returns the tenant-scoped counts for a user. Used
// as the blast-radius preview on admin-delete (dry-run).
type UserResourceCounts struct {
	Agents        int `json:"agents"`
	APIKeys       int `json:"keys"`
	Projects      int `json:"projects"`
	Providers     int `json:"providers"`
	Connections   int `json:"connections"`
	MCPServers    int `json:"mcp_servers"`
	Subscriptions int `json:"subscriptions"`
	Channels      int `json:"channels"`
}

func (s *Store) CountUserResources(userID int64) UserResourceCounts {
	var c UserResourceCounts
	s.db.QueryRow("SELECT COUNT(*) FROM agents WHERE user_id=?", userID).Scan(&c.Agents)
	s.db.QueryRow("SELECT COUNT(*) FROM api_keys WHERE user_id=?", userID).Scan(&c.APIKeys)
	s.db.QueryRow("SELECT COUNT(*) FROM projects WHERE user_id=?", userID).Scan(&c.Projects)
	s.db.QueryRow("SELECT COUNT(*) FROM providers WHERE user_id=?", userID).Scan(&c.Providers)
	s.db.QueryRow("SELECT COUNT(*) FROM connections WHERE user_id=?", userID).Scan(&c.Connections)
	s.db.QueryRow("SELECT COUNT(*) FROM mcp_servers WHERE user_id=?", userID).Scan(&c.MCPServers)
	s.db.QueryRow("SELECT COUNT(*) FROM subscriptions WHERE user_id=?", userID).Scan(&c.Subscriptions)
	s.db.QueryRow("SELECT COUNT(*) FROM channels WHERE user_id=?", userID).Scan(&c.Channels)
	return c
}

// DeleteUser removes every row tied to this user across every tenant-
// scoped table, then the user row itself. The tables don't have ON
// DELETE CASCADE in the schema, so we do the cascade explicitly. Done
// in a single transaction so a partial failure can't leave orphaned
// rows pointing at a vanished user_id.
func (s *Store) DeleteUser(userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Order matters only for readability; none of these have FKs to
	// each other, only to users(id). Telemetry is keyed by agent_id,
	// not user_id, so it goes away transitively once instances do —
	// but we clean it up anyway for explicit hygiene.
	for _, q := range []string{
		"DELETE FROM telemetry WHERE agent_id IN (SELECT id FROM agents WHERE user_id = ?)",
		"DELETE FROM agents WHERE user_id = ?",
		"DELETE FROM api_keys WHERE user_id = ?",
		"DELETE FROM sessions WHERE user_id = ?",
		"DELETE FROM providers WHERE user_id = ?",
		"DELETE FROM connections WHERE user_id = ?",
		"DELETE FROM mcp_servers WHERE user_id = ?",
		"DELETE FROM subscriptions WHERE user_id = ?",
		"DELETE FROM channels WHERE user_id = ?",
		"DELETE FROM projects WHERE user_id = ?",
		"DELETE FROM oauth_states WHERE user_id = ?",
		"DELETE FROM users WHERE id = ?",
	} {
		if _, err := tx.Exec(q, userID); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return tx.Commit()
}

// DeleteSessionsForUser is the unconditional sibling of
// DeleteSessionsForUserExcept — used when an admin resets someone
// else's password and we want every one of that user's active sessions
// to stop working immediately.
func (s *Store) DeleteSessionsForUser(userID int64) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

// DeleteSessionsForUserExcept nukes every session row for the user
// except the one whose token is `keepToken` (which should be the
// session the password change was made from). Prevents a leaked
// cookie from surviving a password rotation.
func (s *Store) DeleteSessionsForUserExcept(userID int64, keepToken string) error {
	_, err := s.db.Exec(
		"DELETE FROM sessions WHERE user_id = ? AND token != ?",
		userID, keepToken,
	)
	return err
}

// --- API Keys ---

func HashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

type APIKeyCreateOptions struct {
	Kind               string
	ProjectID          string
	Scopes             string
	AllowedOrigins     string
	RateLimitPerMinute int
	ExpiresAt          string
	IssuerApp          string
	IssuerInstallID    int64
	SubjectType        string
	SubjectID          string
	SubjectEmail       string
	OrganizationID     string
	OrganizationSlug   string
}

func (s *Store) CreateAPIKey(userID int64, name, keyHash, keyPrefix string, options ...APIKeyCreateOptions) (*APIKey, error) {
	opt := APIKeyCreateOptions{
		Kind:               "private",
		Scopes:             "[]",
		AllowedOrigins:     "[]",
		RateLimitPerMinute: 60,
	}
	if len(options) > 0 {
		opt = options[0]
	}
	if opt.Kind == "" {
		opt.Kind = "private"
	}
	if opt.Scopes == "" {
		opt.Scopes = "[]"
	}
	if opt.AllowedOrigins == "" {
		opt.AllowedOrigins = "[]"
	}
	if opt.RateLimitPerMinute <= 0 {
		opt.RateLimitPerMinute = 60
	}
	result, err := s.db.Exec(
		`INSERT INTO api_keys
			(user_id, name, key_hash, key_prefix, kind, project_id, scopes, allowed_origins, rate_limit_per_minute, expires_at,
			 issuer_app, issuer_install_id, subject_type, subject_id, subject_email, organization_id, organization_slug)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)`,
		userID, name, keyHash, keyPrefix, opt.Kind, opt.ProjectID, opt.Scopes, opt.AllowedOrigins, opt.RateLimitPerMinute, opt.ExpiresAt,
		opt.IssuerApp, opt.IssuerInstallID, opt.SubjectType, opt.SubjectID, opt.SubjectEmail, opt.OrganizationID, opt.OrganizationSlug,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &APIKey{
		ID: id, UserID: userID, Name: name, KeyPrefix: keyPrefix,
		Kind: opt.Kind, ProjectID: opt.ProjectID, Scopes: opt.Scopes,
		AllowedOrigins: opt.AllowedOrigins, RateLimitPerMinute: opt.RateLimitPerMinute,
		ExpiresAt: opt.ExpiresAt, IssuerApp: opt.IssuerApp, IssuerInstallID: opt.IssuerInstallID,
		SubjectType: opt.SubjectType, SubjectID: opt.SubjectID, SubjectEmail: opt.SubjectEmail,
		OrganizationID: opt.OrganizationID, OrganizationSlug: opt.OrganizationSlug, CreatedAt: time.Now(),
	}, nil
}

func (s *Store) GetUserByAPIKey(keyHash string) (*User, error) {
	var u User
	err := s.db.QueryRow(`
		SELECT u.id, u.email, u.password_hash, COALESCE(u.role,'user')
		FROM users u JOIN api_keys k ON u.id = k.user_id
		WHERE k.key_hash = ?
		  AND COALESCE(k.kind, 'private') = 'private'
		  AND k.revoked_at IS NULL
		  AND (k.expires_at IS NULL OR datetime(k.expires_at) > CURRENT_TIMESTAMP)
	`, keyHash).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role)
	if err != nil {
		return nil, err
	}
	// Update last_used
	s.db.Exec("UPDATE api_keys SET last_used = CURRENT_TIMESTAMP WHERE key_hash = ?", keyHash)
	return &u, nil
}

func (s *Store) GetPublicClientAPIKey(keyHash string) (*APIKey, error) {
	var k APIKey
	var createdAt string
	err := s.db.QueryRow(`
		SELECT id, user_id, name, key_prefix, key_hash, COALESCE(kind,'private'), COALESCE(project_id,''),
		       COALESCE(scopes,'[]'), COALESCE(allowed_origins,'[]'), COALESCE(rate_limit_per_minute, 60),
		       COALESCE(expires_at,''), COALESCE(revoked_at,''), COALESCE(last_used,''), COALESCE(last_used_ip,''),
		       created_at
		  FROM api_keys
		 WHERE key_hash = ?
		   AND kind = 'public_client'
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR datetime(expires_at) > CURRENT_TIMESTAMP)
	`, keyHash).Scan(
		&k.ID, &k.UserID, &k.Name, &k.KeyPrefix, &k.KeyHash, &k.Kind, &k.ProjectID,
		&k.Scopes, &k.AllowedOrigins, &k.RateLimitPerMinute,
		&k.ExpiresAt, &k.RevokedAt, &k.LastUsed, &k.LastUsedIP,
		&createdAt,
	)
	if err != nil {
		return nil, err
	}
	k.CreatedAt, _ = parseTime(createdAt)
	return &k, nil
}

func (s *Store) GetDelegatedUserAPIKey(keyHash string) (*APIKey, error) {
	var k APIKey
	var createdAt string
	err := s.db.QueryRow(`
		SELECT id, user_id, name, key_prefix, key_hash, COALESCE(kind,'private'), COALESCE(project_id,''),
		       COALESCE(scopes,'[]'), COALESCE(allowed_origins,'[]'), COALESCE(rate_limit_per_minute, 60),
		       COALESCE(expires_at,''), COALESCE(revoked_at,''), COALESCE(last_used,''), COALESCE(last_used_ip,''),
		       COALESCE(issuer_app,''), COALESCE(issuer_install_id,0), COALESCE(subject_type,''), COALESCE(subject_id,''),
		       COALESCE(subject_email,''), COALESCE(organization_id,''), COALESCE(organization_slug,''), created_at
		  FROM api_keys
		 WHERE key_hash = ?
		   AND kind = 'delegated_user'
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR datetime(expires_at) > CURRENT_TIMESTAMP)
	`, keyHash).Scan(
		&k.ID, &k.UserID, &k.Name, &k.KeyPrefix, &k.KeyHash, &k.Kind, &k.ProjectID,
		&k.Scopes, &k.AllowedOrigins, &k.RateLimitPerMinute,
		&k.ExpiresAt, &k.RevokedAt, &k.LastUsed, &k.LastUsedIP,
		&k.IssuerApp, &k.IssuerInstallID, &k.SubjectType, &k.SubjectID,
		&k.SubjectEmail, &k.OrganizationID, &k.OrganizationSlug, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	k.CreatedAt, _ = parseTime(createdAt)
	return &k, nil
}

func (s *Store) MarkAPIKeyUsed(keyID int64, ip string) {
	_, _ = s.db.Exec("UPDATE api_keys SET last_used = CURRENT_TIMESTAMP, last_used_ip = ? WHERE id = ?", ip, keyID)
}

func (s *Store) ListAPIKeys(userID int64) ([]APIKey, error) {
	rows, err := s.db.Query(
		`SELECT id, name, key_prefix, COALESCE(kind,'private'), COALESCE(project_id,''),
		        COALESCE(scopes,'[]'), COALESCE(allowed_origins,'[]'), COALESCE(rate_limit_per_minute, 60),
		        COALESCE(expires_at,''), COALESCE(revoked_at,''), COALESCE(last_used,''), COALESCE(last_used_ip,''),
		        created_at
		   FROM api_keys
		  WHERE user_id = ?
		  ORDER BY created_at DESC, id DESC`, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var createdAt string
		rows.Scan(
			&k.ID, &k.Name, &k.KeyPrefix, &k.Kind, &k.ProjectID,
			&k.Scopes, &k.AllowedOrigins, &k.RateLimitPerMinute,
			&k.ExpiresAt, &k.RevokedAt, &k.LastUsed, &k.LastUsedIP,
			&createdAt,
		)
		k.UserID = userID
		k.CreatedAt, _ = parseTime(createdAt)
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *Store) DeleteAPIKey(userID, keyID int64) error {
	_, err := s.db.Exec("DELETE FROM api_keys WHERE id = ? AND user_id = ?", keyID, userID)
	return err
}

// --- Instances ---

func (s *Store) CreateAgent(userID int64, name, directive, mode, config, projectID string) (*Agent, error) {
	if mode == "" {
		mode = "autonomous"
	}
	result, err := s.db.Exec(
		"INSERT INTO agents (user_id, name, directive, mode, config, project_id) VALUES (?, ?, ?, ?, ?, ?)",
		userID, name, directive, mode, config, projectID,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &Agent{ID: id, UserID: userID, Name: name, Directive: directive, Mode: mode, Config: config, Status: "stopped", ProjectID: projectID, CreatedAt: time.Now()}, nil
}

// GetAgentName returns the name of an instance by ID (no user check).
// Used by the console logger to resolve instance names from telemetry events.
func (s *Store) GetAgentName(instanceID int64) (string, error) {
	var name string
	err := s.db.QueryRow("SELECT name FROM agents WHERE id = ?", instanceID).Scan(&name)
	return name, err
}

// GetAgentByID returns an instance by ID without user check (for server-internal use).
func (s *Store) GetAgentByID(instanceID int64) (*Agent, error) {
	var inst Agent
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, user_id, name, directive, COALESCE(mode,'autonomous'), config, port, pid, COALESCE(core_api_key,''), status, COALESCE(project_id,''), COALESCE(kind,'user'), COALESCE(core_version,''), COALESCE(core_build_time,''), COALESCE(core_started_at,''), created_at FROM agents WHERE id = ?",
		instanceID,
	).Scan(&inst.ID, &inst.UserID, &inst.Name, &inst.Directive, &inst.Mode, &inst.Config, &inst.Port, &inst.Pid, &inst.CoreAPIKey, &inst.Status, &inst.ProjectID, &inst.Kind, &inst.CoreVersion, &inst.CoreBuildTime, &inst.CoreStartedAt, &createdAt)
	if err != nil {
		return nil, err
	}
	inst.CreatedAt, _ = parseTime(createdAt)
	return &inst, nil
}

func (s *Store) GetAgent(userID, instanceID int64) (*Agent, error) {
	var inst Agent
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, user_id, name, directive, COALESCE(mode,'autonomous'), config, port, pid, COALESCE(core_api_key,''), status, COALESCE(project_id,''), COALESCE(kind,'user'), COALESCE(core_version,''), COALESCE(core_build_time,''), COALESCE(core_started_at,''), created_at FROM agents WHERE id = ? AND user_id = ?",
		instanceID, userID,
	).Scan(&inst.ID, &inst.UserID, &inst.Name, &inst.Directive, &inst.Mode, &inst.Config, &inst.Port, &inst.Pid, &inst.CoreAPIKey, &inst.Status, &inst.ProjectID, &inst.Kind, &inst.CoreVersion, &inst.CoreBuildTime, &inst.CoreStartedAt, &createdAt)
	if err != nil {
		return nil, err
	}
	inst.CreatedAt, _ = parseTime(createdAt)
	return &inst, nil
}

// GetOrCreatePlatformHelper returns the singleton platform-owned
// meta-agent row for a user, creating it on first call. Used by the
// dashboard helper path so apteva-server always has a real
// apteva-core process to dispatch platform work to.
//
// Idempotent: subsequent calls for the same user return the existing
// row. The directive is the canonical user-facing platform-helper prompt.
func (s *Store) GetOrCreatePlatformHelper(userID int64, directive string) (*Agent, error) {
	// Look up existing helper for this user.
	var ag Agent
	err := s.db.QueryRow(
		`SELECT id, user_id, name, directive, COALESCE(mode,'autonomous'),
		        config, port, pid, COALESCE(core_api_key,''), status, COALESCE(project_id,''),
		        COALESCE(core_version,''), COALESCE(core_build_time,''), COALESCE(core_started_at,''), created_at
		   FROM agents
		  WHERE user_id = ? AND kind = 'platform_helper'
		  ORDER BY id ASC LIMIT 1`,
		userID,
	).Scan(&ag.ID, &ag.UserID, &ag.Name, &ag.Directive, &ag.Mode,
		&ag.Config, &ag.Port, &ag.Pid, &ag.CoreAPIKey, &ag.Status, &ag.ProjectID,
		&ag.CoreVersion, &ag.CoreBuildTime, &ag.CoreStartedAt, &ag.CreatedAt)
	if err == nil {
		ag.Kind = "platform_helper"
		if ag.Name == "__platform_helper__" || strings.TrimSpace(ag.Name) == "" {
			s.db.Exec(`UPDATE agents SET name = ? WHERE id = ?`, "Apteva Helper", ag.ID)
			ag.Name = "Apteva Helper"
		}
		// Refresh the directive if the platform's version has drifted
		// (we roll new judge prompts under the same row).
		if ag.Directive != directive {
			s.db.Exec(`UPDATE agents SET directive = ? WHERE id = ?`, directive, ag.ID)
			ag.Directive = directive
		}
		return &ag, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	// Fresh insert. The helper is autonomous (no learn/cautious
	// pauses), no project scope, no extra config — a stateless
	// LLM-judge sidecar.
	res, err := s.db.Exec(
		`INSERT INTO agents (user_id, name, directive, mode, config, status, project_id, kind)
		 VALUES (?, ?, ?, 'autonomous', '{}', 'stopped', '', 'platform_helper')`,
		userID, "Apteva Helper", directive,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetAgentByID(id)
}

// ListAgentsInProject returns every agent in a project, regardless of
// which user_id created them. Used by handlers that have already
// authorised the caller via requireProjectAccess so multi-user
// members see all agents in a shared project (the original
// ListAgents filters by user_id which is wrong post-multi-user — kept
// for back-compat in code paths that still want the personal view).
func (s *Store) ListAgentsInProject(projectID string) ([]Agent, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, directive, COALESCE(mode,'autonomous'),
		        port, pid, status, COALESCE(project_id,''),
		        COALESCE(core_version,''), COALESCE(core_build_time,''), COALESCE(core_started_at,''), created_at
		   FROM agents
		  WHERE project_id = ? AND COALESCE(kind,'user') = 'user'`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var createdAt string
		rows.Scan(&a.ID, &a.UserID, &a.Name, &a.Directive, &a.Mode,
			&a.Port, &a.Pid, &a.Status, &a.ProjectID,
			&a.CoreVersion, &a.CoreBuildTime, &a.CoreStartedAt, &createdAt)
		a.CreatedAt, _ = parseTime(createdAt)
		out = append(out, a)
	}
	return out, nil
}

func (s *Store) ListAgents(userID int64, projectID string) ([]Agent, error) {
	var rows *sql.Rows
	var err error
	// Default list filters out platform-owned agents (the meta-agent
	// and friends) so they don't clutter the operator's dashboard.
	// Callers needing the platform helpers go through GetPlatformHelper.
	if projectID != "" {
		rows, err = s.db.Query(
			"SELECT id, name, directive, COALESCE(mode,'autonomous'), port, pid, status, COALESCE(project_id,''), COALESCE(core_version,''), COALESCE(core_build_time,''), COALESCE(core_started_at,''), created_at FROM agents WHERE user_id = ? AND project_id = ? AND COALESCE(kind,'user') = 'user'", userID, projectID)
	} else {
		rows, err = s.db.Query(
			"SELECT id, name, directive, COALESCE(mode,'autonomous'), port, pid, status, COALESCE(project_id,''), COALESCE(core_version,''), COALESCE(core_build_time,''), COALESCE(core_started_at,''), created_at FROM agents WHERE user_id = ? AND COALESCE(kind,'user') = 'user'", userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Agent
	for rows.Next() {
		var inst Agent
		var createdAt string
		rows.Scan(&inst.ID, &inst.Name, &inst.Directive, &inst.Mode, &inst.Port, &inst.Pid, &inst.Status, &inst.ProjectID, &inst.CoreVersion, &inst.CoreBuildTime, &inst.CoreStartedAt, &createdAt)
		inst.UserID = userID
		inst.CreatedAt, _ = parseTime(createdAt)
		instances = append(instances, inst)
	}
	return instances, nil
}

// ListTelemetryAgentIDs returns the agent ids a user may receive from
// the project-wide telemetry stream. Unlike ListAgents, this includes
// the user's platform helper: the helper is intentionally hidden from
// normal agent lists but still needs telemetry for the dashboard chat
// widget's thinking/tool-call UI.
func (s *Store) ListTelemetryAgentIDs(userID int64, projectID string) (map[int64]bool, error) {
	var rows *sql.Rows
	var err error
	if projectID != "" {
		rows, err = s.db.Query(
			`SELECT id
			   FROM agents
			  WHERE user_id = ?
			    AND (
			      (COALESCE(kind,'user') = 'user' AND project_id = ?)
			      OR COALESCE(kind,'user') = 'platform_helper'
			    )`,
			userID, projectID,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id
			   FROM agents
			  WHERE user_id = ?
			    AND COALESCE(kind,'user') IN ('user', 'platform_helper')`,
			userID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) UpdateAgent(inst *Agent) error {
	if strings.EqualFold(inst.Status, "stopped") {
		inst.Port = 0
		inst.Pid = 0
		inst.CoreAPIKey = ""
		inst.CoreStartedAt = ""
	}
	_, err := s.db.Exec(
		`UPDATE agents
		    SET name=?, directive=?, mode=?, config=?, port=?, pid=?, core_api_key=?,
		        status=?, project_id=?,
		        core_started_at=CASE WHEN LOWER(?)='stopped' THEN NULL ELSE core_started_at END
		  WHERE id=?`,
		inst.Name, inst.Directive, inst.Mode, inst.Config, inst.Port, inst.Pid, inst.CoreAPIKey,
		inst.Status, inst.ProjectID, inst.Status, inst.ID,
	)
	return err
}

func (s *Store) UpdateAgentCoreRuntime(agentID int64, version, buildTime string, startedAt time.Time) error {
	var started any
	if !startedAt.IsZero() {
		started = startedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(
		`UPDATE agents
		    SET core_version=?, core_build_time=?, core_started_at=?
		  WHERE id=?`,
		version, buildTime, started, agentID,
	)
	return err
}

// SetAgentRuntimeRunning persists the complete identity of one live core in a
// single statement. Keeping pid/port/key and version/start metadata together
// prevents readers from observing a partially activated runtime.
func (s *Store) SetAgentRuntimeRunning(inst *Agent, startedAt time.Time) error {
	if inst == nil {
		return fmt.Errorf("agent is nil")
	}
	if inst.Pid <= 0 || inst.Port <= 0 || inst.CoreAPIKey == "" {
		return fmt.Errorf("agent %d has incomplete process metadata", inst.ID)
	}
	if strings.TrimSpace(inst.CoreVersion) == "" || startedAt.IsZero() {
		return fmt.Errorf("agent %d has incomplete core runtime metadata", inst.ID)
	}
	started := startedAt.UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(
		`UPDATE agents
		    SET status='running', pid=?, port=?, core_api_key=?,
		        core_version=?, core_build_time=?, core_started_at=?
		  WHERE id=?`,
		inst.Pid, inst.Port, inst.CoreAPIKey,
		inst.CoreVersion, inst.CoreBuildTime, started, inst.ID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("agent %d runtime update affected %d rows", inst.ID, rows)
	}
	inst.Status = "running"
	inst.CoreStartedAt = started
	return nil
}

// SetAgentRuntimeStopped clears metadata that identifies a live process while
// preserving the last observed core version/build for update diagnostics.
func (s *Store) SetAgentRuntimeStopped(inst *Agent) error {
	if inst == nil {
		return fmt.Errorf("agent is nil")
	}
	res, err := s.db.Exec(
		`UPDATE agents
		    SET status='stopped', pid=0, port=0, core_api_key='', core_started_at=NULL
		  WHERE id=?`,
		inst.ID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("agent %d runtime clear affected %d rows", inst.ID, rows)
	}
	inst.Status = "stopped"
	inst.Pid = 0
	inst.Port = 0
	inst.CoreAPIKey = ""
	inst.CoreStartedAt = ""
	return nil
}

// MarkPlatformAgentsStoppedForShutdown records a clean server shutdown for
// platform-owned helpers before child cores are terminated. User agents keep
// status='running' so the next boot can resume agents the operator explicitly
// left active. Platform helpers are lazy-started by helper paths when
// actually needed, so leaving them as running would only create stale UI state.
func (s *Store) MarkPlatformAgentsStoppedForShutdown() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE agents
		    SET status='stopped', port=0, pid=0, core_api_key='', core_started_at=NULL
		  WHERE status='running'
		    AND COALESCE(kind, 'user') != 'user'`,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListAgentsByStatus scans every user's instances for ones in the given
// status. Used by the server's boot-time resume path to find everything
// that was `running` before the last shutdown and re-spawn those cores.
// The result is unsorted; callers that need ordering should sort themselves.
func (s *Store) ListAgentsByStatus(status string) ([]Agent, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, directive, COALESCE(mode,'autonomous'), config, port, pid, COALESCE(core_api_key,''), status, COALESCE(project_id,''), COALESCE(kind,'user'), COALESCE(core_version,''), COALESCE(core_build_time,''), COALESCE(core_started_at,''), created_at
		 FROM agents WHERE status = ?`,
		status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Agent
	for rows.Next() {
		var inst Agent
		var createdAt string
		rows.Scan(&inst.ID, &inst.UserID, &inst.Name, &inst.Directive, &inst.Mode, &inst.Config, &inst.Port, &inst.Pid, &inst.CoreAPIKey, &inst.Status, &inst.ProjectID, &inst.Kind, &inst.CoreVersion, &inst.CoreBuildTime, &inst.CoreStartedAt, &createdAt)
		inst.CreatedAt, _ = parseTime(createdAt)
		instances = append(instances, inst)
	}
	return instances, nil
}

func (s *Store) CreateEnvironmentRecord(rec EnvironmentRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO environments (id, project_id, name, mode, status, spec_json, created_by, error_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.ProjectID, rec.Name, rec.Mode, rec.Status, rec.SpecJSON, rec.CreatedBy, rec.ErrorMessage,
	)
	return err
}

func (s *Store) GetEnvironmentRecord(id string) (*EnvironmentRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, project_id, name, mode, status, spec_json, created_by, created_at, updated_at,
		        last_started_at, last_stopped_at, error_message
		   FROM environments WHERE id = ?`,
		id,
	)
	rec, err := scanEnvironmentRecord(row)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *Store) ListEnvironmentRecords(userID int64) ([]EnvironmentRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, name, mode, status, spec_json, created_by, created_at, updated_at,
		        last_started_at, last_stopped_at, error_message
		   FROM environments
		  WHERE created_by = ?
		  ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnvironmentRecord{}
	for rows.Next() {
		rec, err := scanEnvironmentRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) UpdateEnvironmentRecordStatus(id, status, errorMessage string) error {
	switch status {
	case "running":
		_, err := s.db.Exec(
			`UPDATE environments
			    SET status = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP, last_started_at = CURRENT_TIMESTAMP
			  WHERE id = ?`,
			status, errorMessage, id,
		)
		return err
	case "stopped":
		_, err := s.db.Exec(
			`UPDATE environments
			    SET status = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP, last_stopped_at = CURRENT_TIMESTAMP
			  WHERE id = ?`,
			status, errorMessage, id,
		)
		return err
	default:
		_, err := s.db.Exec(
			`UPDATE environments
			    SET status = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP
			  WHERE id = ?`,
			status, errorMessage, id,
		)
		return err
	}
}

func (s *Store) UpdateEnvironmentRecordSpec(id, specJSON string) error {
	_, err := s.db.Exec(
		`UPDATE environments SET spec_json = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		specJSON, id,
	)
	return err
}

func (s *Store) DeleteEnvironmentRecord(id string) error {
	_, err := s.db.Exec(`DELETE FROM environments WHERE id = ?`, id)
	return err
}

type environmentRecordScanner interface {
	Scan(dest ...any) error
}

func scanEnvironmentRecord(scanner environmentRecordScanner) (EnvironmentRecord, error) {
	var rec EnvironmentRecord
	var createdAt, updatedAt string
	var lastStarted, lastStopped sql.NullString
	err := scanner.Scan(
		&rec.ID, &rec.ProjectID, &rec.Name, &rec.Mode, &rec.Status, &rec.SpecJSON, &rec.CreatedBy,
		&createdAt, &updatedAt, &lastStarted, &lastStopped, &rec.ErrorMessage,
	)
	if err != nil {
		return rec, err
	}
	rec.CreatedAt, _ = parseTime(createdAt)
	rec.UpdatedAt, _ = parseTime(updatedAt)
	if lastStarted.Valid {
		t, _ := parseTime(lastStarted.String)
		rec.LastStartedAt = &t
	}
	if lastStopped.Valid {
		t, _ := parseTime(lastStopped.String)
		rec.LastStoppedAt = &t
	}
	return rec, nil
}

// DeleteAgent removes an instance row plus every per-instance row
// in the server's own DB. Tables here lack ON DELETE CASCADE, so a
// naive `DELETE FROM agents` left telemetry/channels/subscriptions/
// bindings behind — we found ~100 orphan instance_ids in telemetry
// alone in production. Each child delete is its own statement
// (rather than a single CTE) because the server's sqlite driver
// doesn't run multi-statement Execs reliably across versions.
//
// App-side state (channel-chat chats/messages, future helpdesk
// tickets, etc.) is NOT touched here — that's the apps registry's
// job via NotifyInstanceDetach. The caller in agents.go fires
// that hook before invoking us.
func (s *Store) DeleteAgent(userID, instanceID int64) error {
	// Verify ownership first — the deletes below are unscoped by
	// user_id, so a missing ownership check would let any caller
	// blow away another tenant's data if they knew the id.
	var owner int64
	if err := s.db.QueryRow("SELECT user_id FROM agents WHERE id = ?", instanceID).Scan(&owner); err != nil {
		if err == sql.ErrNoRows {
			return nil // already gone — idempotent
		}
		return err
	}
	if owner != userID {
		return fmt.Errorf("instance %d not owned by user %d", instanceID, userID)
	}

	stmts := []string{
		"DELETE FROM telemetry             WHERE agent_id = ?",
		"DELETE FROM channels              WHERE agent_id = ?",
		"DELETE FROM subscriptions         WHERE agent_id = ?",
		"DELETE FROM app_agent_bindings WHERE agent_id = ?",
		"DELETE FROM agents             WHERE id = ? AND user_id = ?",
	}
	for i, q := range stmts {
		var err error
		if i == len(stmts)-1 {
			_, err = s.db.Exec(q, instanceID, userID)
		} else {
			_, err = s.db.Exec(q, instanceID)
		}
		if err != nil {
			return fmt.Errorf("delete instance %d: %s: %w", instanceID, q, err)
		}
	}
	return nil
}

// --- Projects ---

func (s *Store) CreateProject(userID int64, name, description, color string) (*Project, error) {
	id := generateID()
	if color == "" {
		color = "#6366f1"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(
		"INSERT INTO projects (id, user_id, name, description, color) VALUES (?, ?, ?, ?, ?)",
		id, userID, name, description, color,
	); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		`INSERT INTO project_members(project_id,user_id,role,added_by) VALUES(?,?,'owner',?)`,
		id, userID, userID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Project{ID: id, UserID: userID, Name: name, Description: description, Color: color, CreatedAt: time.Now()}, nil
}

func (s *Store) ListProjects(userID int64) ([]Project, error) {
	rows, err := s.db.Query("SELECT id, name, description, color, created_at FROM projects WHERE user_id = ?", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		var p Project
		var createdAt string
		rows.Scan(&p.ID, &p.Name, &p.Description, &p.Color, &createdAt)
		p.UserID = userID
		p.CreatedAt, _ = parseTime(createdAt)
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *Store) GetProject(userID int64, id string) (*Project, error) {
	var p Project
	var createdAt string
	err := s.db.QueryRow("SELECT id, name, description, color, created_at FROM projects WHERE id = ? AND user_id = ?", id, userID).
		Scan(&p.ID, &p.Name, &p.Description, &p.Color, &createdAt)
	if err != nil {
		return nil, err
	}
	p.UserID = userID
	p.CreatedAt, _ = parseTime(createdAt)
	return &p, nil
}

func (s *Store) UpdateProject(userID int64, id, name, description, color string) error {
	_, err := s.db.Exec("UPDATE projects SET name=?, description=?, color=? WHERE id=? AND user_id=?",
		name, description, color, id, userID)
	return err
}

func (s *Store) DeleteProject(userID int64, id string) error {
	_, err := s.db.Exec("DELETE FROM projects WHERE id = ? AND user_id = ?", id, userID)
	return err
}

// GetProjectAny / UpdateProjectAny / DeleteProjectAny — variants that
// don't filter by user_id. Project handlers gate access via
// requireProjectAccess before calling these, so the user_id WHERE
// clause is redundant once you're past the authz check (and in fact
// wrong: a member or admin viewing a project owned by another user
// must succeed). The original *Project methods stay around for
// internal call sites that still rely on the user_id filter.
func (s *Store) GetProjectAny(id string) (*Project, error) {
	var p Project
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, user_id, name, description, color, created_at FROM projects WHERE id = ?", id,
	).Scan(&p.ID, &p.UserID, &p.Name, &p.Description, &p.Color, &createdAt)
	if err != nil {
		return nil, err
	}
	p.CreatedAt, _ = parseTime(createdAt)
	return &p, nil
}

func (s *Store) UpdateProjectAny(id, name, description, color string) error {
	_, err := s.db.Exec(
		"UPDATE projects SET name=?, description=?, color=? WHERE id=?",
		name, description, color, id,
	)
	return err
}

func (s *Store) DeleteProjectAny(id string) error {
	_, err := s.db.Exec("DELETE FROM projects WHERE id = ?", id)
	return err
}

// --- Sessions ---

// GetFirstUserID returns the ID of the first user in the database (for local auto-login).
func (s *Store) GetFirstUserID() (int64, error) {
	var userID int64
	err := s.db.QueryRow("SELECT id FROM users ORDER BY id ASC LIMIT 1").Scan(&userID)
	return userID, err
}

func (s *Store) CreateSession(token string, userID int64, expiresAt time.Time) error {
	_, err := s.db.Exec(
		"INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)",
		token, userID, expiresAt.UTC().Format("2006-01-02 15:04:05"),
	)
	return err
}

func (s *Store) CreatePendingMFASession(token string, userID int64, expiresAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (token, user_id, expires_at, auth_state, mfa_attempts)
		 VALUES (?, ?, ?, 'pending_mfa', 0)`,
		token, userID, expiresAt.UTC().Format("2006-01-02 15:04:05"),
	)
	return err
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
	return err
}

func (s *Store) GetSession(token string) (int64, error) {
	var userID int64
	var expiresAt string
	var authState string
	err := s.db.QueryRow(
		"SELECT user_id, expires_at, COALESCE(auth_state,'active') FROM sessions WHERE token = ?", token,
	).Scan(&userID, &expiresAt, &authState)
	if err != nil {
		return 0, err
	}
	exp, err := parseTime(expiresAt)
	if err != nil {
		return 0, fmt.Errorf("bad expires_at %q: %w", expiresAt, err)
	}
	if time.Now().UTC().After(exp) {
		s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
		return 0, fmt.Errorf("session expired")
	}
	if authState != "active" {
		return 0, fmt.Errorf("session is not fully authenticated")
	}
	return userID, nil
}

func (s *Store) GetPendingMFASession(token string) (userID int64, attempts int, err error) {
	var expiresAt, authState string
	err = s.db.QueryRow(
		"SELECT user_id, expires_at, COALESCE(auth_state,'active'), COALESCE(mfa_attempts,0) FROM sessions WHERE token = ?",
		token,
	).Scan(&userID, &expiresAt, &authState, &attempts)
	if err != nil {
		return 0, 0, err
	}
	exp, err := parseTime(expiresAt)
	if err != nil || time.Now().UTC().After(exp) {
		_, _ = s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
		return 0, 0, fmt.Errorf("MFA challenge expired")
	}
	if authState != "pending_mfa" {
		return 0, 0, fmt.Errorf("MFA challenge not found")
	}
	return userID, attempts, nil
}

func (s *Store) RecordPendingMFAFailure(token string, maxAttempts int) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET mfa_attempts=mfa_attempts+1
		 WHERE token=? AND auth_state='pending_mfa'`, token,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("MFA challenge not found")
	}
	var attempts int
	if err := s.db.QueryRow("SELECT mfa_attempts FROM sessions WHERE token=?", token).Scan(&attempts); err != nil {
		return err
	}
	if attempts >= maxAttempts {
		_, _ = s.db.Exec("DELETE FROM sessions WHERE token=?", token)
	}
	return nil
}

func (s *Store) ActivateMFASession(token string, expiresAt time.Time) error {
	res, err := s.db.Exec(
		`UPDATE sessions
		    SET auth_state='active', mfa_attempts=0, expires_at=?
		  WHERE token=? AND auth_state='pending_mfa'`,
		expiresAt.UTC().Format("2006-01-02 15:04:05"), token,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("MFA challenge not found")
	}
	return nil
}

// parseTime tries multiple formats that SQLite may return.
func parseTime(s string) (time.Time, error) {
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05+00:00",
		"2006-01-02 15:04:05-07:00",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q", s)
}

func (s *Store) DeleteExpiredSessions() {
	s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", time.Now().UTC().Format("2006-01-02 15:04:05"))
}

// columnExists checks whether `col` is present on `table`. Used by
// migrations that need to distinguish "first deploy of this column"
// from "subsequent boot" so that one-shot backfills don't repeat.
func columnExists(db *sql.DB, table, col string) bool {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			continue
		}
		if name == col {
			return true
		}
	}
	return false
}

// --- Channels ---

type ChannelRecord struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	AgentID   int64  `json:"instance_id"`
	ProjectID string `json:"project_id,omitempty"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func (s *Store) CreateChannel(userID, instanceID int64, chType, name, encryptedConfig string, projectID ...string) (*ChannelRecord, error) {
	pid := ""
	if len(projectID) > 0 {
		pid = projectID[0]
	}
	res, err := s.db.Exec(
		"INSERT INTO channels (user_id, agent_id, type, name, encrypted_config, project_id) VALUES (?, ?, ?, ?, ?, ?)",
		userID, instanceID, chType, name, encryptedConfig, pid,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &ChannelRecord{ID: id, UserID: userID, AgentID: instanceID, ProjectID: pid, Type: chType, Name: name, Status: "active"}, nil
}

func (s *Store) ListChannels(instanceID int64) ([]ChannelRecord, error) {
	rows, err := s.db.Query("SELECT id, user_id, agent_id, COALESCE(project_id,''), type, name, status, created_at FROM channels WHERE agent_id = ? AND status = 'active'", instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRecord
	for rows.Next() {
		var c ChannelRecord
		rows.Scan(&c.ID, &c.UserID, &c.AgentID, &c.ProjectID, &c.Type, &c.Name, &c.Status, &c.CreatedAt)
		out = append(out, c)
	}
	return out, nil
}

// ListChannelsByProject returns all channels for a project (including project-level ones with agent_id=0).
func (s *Store) ListChannelsByProject(projectID string, chType string) ([]ChannelRecord, error) {
	rows, err := s.db.Query(
		"SELECT id, user_id, agent_id, COALESCE(project_id,''), type, name, encrypted_config, status, created_at FROM channels WHERE project_id = ? AND type = ? AND status = 'active'",
		projectID, chType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRecord
	for rows.Next() {
		var c ChannelRecord
		var enc string
		rows.Scan(&c.ID, &c.UserID, &c.AgentID, &c.ProjectID, &c.Type, &c.Name, &enc, &c.Status, &c.CreatedAt)
		out = append(out, c)
	}
	return out, nil
}

func (s *Store) GetChannelConfig(id int64) (string, error) {
	var enc string
	err := s.db.QueryRow("SELECT encrypted_config FROM channels WHERE id = ?", id).Scan(&enc)
	return enc, err
}

func (s *Store) DeleteChannel(id int64) error {
	_, err := s.db.Exec("DELETE FROM channels WHERE id = ?", id)
	return err
}

// --- server_settings (key/value bag) ---

// GetSetting returns the stored value for a setting key, or "" if unset.
// Errors are intentionally swallowed to "" so callers can treat missing and
// errored the same — these settings are advisory overlays on env vars and
// shouldn't break boot if the table is somehow unreachable.
func (s *Store) GetSetting(key string) string {
	var v string
	err := s.db.QueryRow("SELECT value FROM server_settings WHERE key = ?", key).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

// SetSetting upserts a key/value. Empty value deletes the row so the
// fallback chain (env var, default) re-engages cleanly.
func (s *Store) SetSetting(key, value string) error {
	if value == "" {
		_, err := s.db.Exec("DELETE FROM server_settings WHERE key = ?", key)
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key, value,
	)
	return err
}
