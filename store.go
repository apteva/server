package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type User struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

type APIKey struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Name      string    `json:"name"`
	KeyPrefix string    `json:"key_prefix"` // first 8 chars for display
	KeyHash   string    `json:"-"`
	LastUsed  time.Time `json:"last_used,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Project struct {
	ID          string    `json:"id"`
	UserID      int64     `json:"user_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Color       string    `json:"color"`
	CreatedAt   time.Time `json:"created_at"`
}

type Instance struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Name      string    `json:"name"`
	Directive string    `json:"directive"`
	Mode      string    `json:"mode"` // "autonomous" | "cautious" | "learn"
	Config    string    `json:"config"` // JSON blob
	Port      int       `json:"port"`
	Pid       int       `json:"pid"`
	Status    string    `json:"status"` // running, stopped
	ProjectID string    `json:"project_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	db *sql.DB
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// WAL mode allows concurrent reads + writes (server + mcp-gateway subprocess)
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA busy_timeout=5000")

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			name TEXT NOT NULL,
			key_prefix TEXT NOT NULL,
			key_hash TEXT UNIQUE NOT NULL,
			last_used DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id),
			expires_at DATETIME NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS provider_types (
			id INTEGER PRIMARY KEY,
			type TEXT NOT NULL,
			name TEXT UNIQUE NOT NULL,
			description TEXT DEFAULT '',
			fields TEXT DEFAULT '[]',
			requires_credentials INTEGER DEFAULT 1,
			sort_order INTEGER DEFAULT 0
		);

		INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
			(1, 'llm', 'Fireworks', 'LLM inference via Fireworks AI', '["FIREWORKS_API_KEY"]', 1, 10),
			(2, 'llm', 'OpenAI', 'LLM inference and embeddings', '["OPENAI_API_KEY","OPENAI_BASE_URL"]', 1, 11),
			(3, 'llm', 'Anthropic', 'LLM inference via Anthropic', '["ANTHROPIC_API_KEY"]', 1, 12),
			(4, 'llm', 'Ollama', 'Local LLM inference', '["OLLAMA_HOST"]', 1, 13),
			(5, 'integrations', 'Apteva Local', '200+ app integrations (GitHub, Slack, Stripe, etc.)', '[]', 0, 15),
			(6, 'embeddings', 'Voyage', 'Text embeddings', '["VOYAGE_API_KEY"]', 1, 20),
			(7, 'tts', 'ElevenLabs', 'Text-to-speech', '["ELEVENLABS_API_KEY"]', 1, 30),
			(8, 'browserbase', 'Browserbase', 'Cloud browser automation via Browserbase', '["BROWSERBASE_API_KEY","BROWSERBASE_PROJECT_ID"]', 1, 40),
			(9, 'integrations', 'Composio', '250+ app integrations via Composio (MCP-native)', '["COMPOSIO_API_KEY"]', 1, 16),
			(10, 'llm', 'NVIDIA', 'LLM inference via NVIDIA NIM (integrate.api.nvidia.com)', '["NVIDIA_API_KEY"]', 1, 14),
			(11, 'steel', 'Steel', 'Cloud browser automation via Steel.dev', '["STEEL_API_KEY"]', 1, 41),
			(12, 'browser-engine', 'Browser Engine', 'Cloud browser automation via Browser Engine (self-hosted)', '["BROWSER_API_KEY","BROWSER_API_URL"]', 1, 42);

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
			instance_id INTEGER NOT NULL DEFAULT 0,
			connection_id INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL,
			slug TEXT NOT NULL DEFAULT '',
			description TEXT DEFAULT '',
			webhook_path TEXT UNIQUE NOT NULL,
			encrypted_hmac_secret TEXT DEFAULT '',
			enabled INTEGER DEFAULT 1,
			thread_id TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_sub_webhook ON subscriptions(webhook_path);

		CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			instance_id INTEGER NOT NULL DEFAULT 0,
			type TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			encrypted_config TEXT DEFAULT '',
			status TEXT DEFAULT 'active',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS telemetry (
			id TEXT PRIMARY KEY,
			instance_id INTEGER NOT NULL,
			thread_id TEXT NOT NULL DEFAULT 'main',
			type TEXT NOT NULL,
			time DATETIME NOT NULL,
			data TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_telem_instance_time ON telemetry(instance_id, time);
		CREATE INDEX IF NOT EXISTS idx_telem_type ON telemetry(type, time);

		CREATE TABLE IF NOT EXISTS instances (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			name TEXT NOT NULL,
			directive TEXT DEFAULT '',
			mode TEXT DEFAULT 'autonomous',
			config TEXT DEFAULT '{}',
			port INTEGER DEFAULT 0,
			pid INTEGER DEFAULT 0,
			status TEXT DEFAULT 'stopped',
			project_id TEXT DEFAULT '',
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
	`)
	if err != nil {
		return err
	}

	// Migrations for existing DBs — silently ignored if columns already exist
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN thread_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE connections ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE mcp_servers ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE instances ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE providers ADD COLUMN project_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE channels ADD COLUMN project_id TEXT DEFAULT ''")
	// is_default removed — default is per-instance, stored in instances.config
	s.db.Exec("ALTER TABLE instances ADD COLUMN mode TEXT DEFAULT 'autonomous'")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN external_webhook_id TEXT DEFAULT ''")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN events TEXT DEFAULT ''")

	// Provider webhook_token: per-provider-per-project opaque token used
	// as the path component of /webhooks/<token>. The unified ingress
	// handler matches this column for provider-backed trigger deliveries
	// (Composio today; any other trigger backend tomorrow). Indexed so
	// the ingress lookup is O(1) without decrypting blobs.
	s.db.Exec("ALTER TABLE providers ADD COLUMN webhook_token TEXT DEFAULT ''")
	s.db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_providers_webhook_token ON providers(webhook_token) WHERE webhook_token != ''")

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
	// upstream_id: used for source=remote rows (Composio) so we can track a
	// versioned rename when the tool filter changes. Our mcp_servers.id
	// stays stable for clients; upstream_id is rotated when we re-create
	// the hosted server with a different action list.
	s.db.Exec("ALTER TABLE mcp_servers ADD COLUMN upstream_id TEXT NOT NULL DEFAULT ''")

	// Pending-OAuth state table for local catalog OAuth2 flows (composio OAuth is
	// delegated and does not use this table).
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

	// Seed new provider types on existing DBs (idempotent). The initial
	// CREATE-TABLE seed above only fires on fresh schemas; this block
	// catches upgrades so new provider types show up in the dashboard's
	// "add provider" picker after a binary upgrade without requiring a
	// DB reset.
	s.db.Exec(`INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
		(9, 'integrations', 'Composio', '250+ app integrations via Composio (MCP-native)', '["COMPOSIO_API_KEY"]', 1, 16)`)
	s.db.Exec(`INSERT OR IGNORE INTO provider_types (id, type, name, description, fields, requires_credentials, sort_order) VALUES
		(10, 'llm', 'NVIDIA', 'LLM inference via NVIDIA NIM (integrate.api.nvidia.com)', '["NVIDIA_API_KEY"]', 1, 14)`)
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

	// Fix historical row 8: it was seeded with type='browser' but its
	// fields / name describe Browserbase. getBrowserConfig treats
	// type='browser' as local-Chromium/CDP, so credentials saved under the
	// old row were silently ignored at spawn time. Flip the type to
	// 'browserbase' on existing installs. Idempotent — re-running is a
	// no-op once the row has the correct type.
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
			permissions_json     TEXT NOT NULL DEFAULT '[]',
			installed_at         DATETIME DEFAULT CURRENT_TIMESTAMP,
			installed_by         INTEGER DEFAULT 0,
			UNIQUE(app_id, project_id)
		)
	`)
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
	// the storage app after image-studio). Dashboard surfaces a
	// "configure" banner on the install detail page.
	s.db.Exec(`ALTER TABLE app_installs ADD COLUMN has_pending_options INTEGER NOT NULL DEFAULT 0`)
	// created_via on connections: 'integration' (default — top-level
	// install via the Integrations page, auto-creates an mcp_servers
	// row) vs 'app_install' (created inside an app's dependency flow,
	// no auto-MCP).
	s.db.Exec(`ALTER TABLE connections ADD COLUMN created_via TEXT NOT NULL DEFAULT 'integration'`)
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS app_instance_bindings (
			install_id   INTEGER NOT NULL REFERENCES app_installs(id),
			instance_id  INTEGER NOT NULL REFERENCES instances(id),
			enabled      INTEGER NOT NULL DEFAULT 1,
			PRIMARY KEY (install_id, instance_id)
		)
	`)

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

	return nil
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
	err := s.db.QueryRow(
		"SELECT id, email, password_hash, created_at FROM users WHERE email = ?", email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &createdAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = parseTime(createdAt)
	return &u, nil
}

// GetUserByID fetches a user row by primary key. Used by /auth/me and
// any handler that needs to reply with the caller's email when only
// the session's user_id is known.
func (s *Store) GetUserByID(id int64) (*User, error) {
	var u User
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, email, password_hash, created_at FROM users WHERE id = ?", id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &createdAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = parseTime(createdAt)
	return &u, nil
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

// ListUsers returns every user row, ordered by id so user_id=1 (the
// admin) always comes first. Used by the /users endpoint.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT id, email, created_at FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var createdAt string
		if err := rows.Scan(&u.ID, &u.Email, &createdAt); err != nil {
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
	s.db.QueryRow("SELECT COUNT(*) FROM instances WHERE user_id=?", userID).Scan(&c.Agents)
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
	// each other, only to users(id). Telemetry is keyed by instance_id,
	// not user_id, so it goes away transitively once instances do —
	// but we clean it up anyway for explicit hygiene.
	for _, q := range []string{
		"DELETE FROM telemetry WHERE instance_id IN (SELECT id FROM instances WHERE user_id = ?)",
		"DELETE FROM instances WHERE user_id = ?",
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

func (s *Store) CreateAPIKey(userID int64, name, keyHash, keyPrefix string) (*APIKey, error) {
	result, err := s.db.Exec(
		"INSERT INTO api_keys (user_id, name, key_hash, key_prefix) VALUES (?, ?, ?, ?)",
		userID, name, keyHash, keyPrefix,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &APIKey{ID: id, UserID: userID, Name: name, KeyPrefix: keyPrefix, CreatedAt: time.Now()}, nil
}

func (s *Store) GetUserByAPIKey(keyHash string) (*User, error) {
	var u User
	err := s.db.QueryRow(`
		SELECT u.id, u.email, u.password_hash
		FROM users u JOIN api_keys k ON u.id = k.user_id
		WHERE k.key_hash = ?
	`, keyHash).Scan(&u.ID, &u.Email, &u.PasswordHash)
	if err != nil {
		return nil, err
	}
	// Update last_used
	s.db.Exec("UPDATE api_keys SET last_used = CURRENT_TIMESTAMP WHERE key_hash = ?", keyHash)
	return &u, nil
}

func (s *Store) ListAPIKeys(userID int64) ([]APIKey, error) {
	rows, err := s.db.Query(
		"SELECT id, name, key_prefix, created_at FROM api_keys WHERE user_id = ?", userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var createdAt string
		rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &createdAt)
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

func (s *Store) CreateInstance(userID int64, name, directive, mode, config, projectID string) (*Instance, error) {
	if mode == "" {
		mode = "autonomous"
	}
	result, err := s.db.Exec(
		"INSERT INTO instances (user_id, name, directive, mode, config, project_id) VALUES (?, ?, ?, ?, ?, ?)",
		userID, name, directive, mode, config, projectID,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &Instance{ID: id, UserID: userID, Name: name, Directive: directive, Mode: mode, Config: config, Status: "stopped", ProjectID: projectID, CreatedAt: time.Now()}, nil
}

// GetInstanceName returns the name of an instance by ID (no user check).
// Used by the console logger to resolve instance names from telemetry events.
func (s *Store) GetInstanceName(instanceID int64) (string, error) {
	var name string
	err := s.db.QueryRow("SELECT name FROM instances WHERE id = ?", instanceID).Scan(&name)
	return name, err
}

// GetInstanceByID returns an instance by ID without user check (for server-internal use).
func (s *Store) GetInstanceByID(instanceID int64) (*Instance, error) {
	var inst Instance
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, user_id, name, directive, COALESCE(mode,'autonomous'), config, port, pid, status, COALESCE(project_id,''), created_at FROM instances WHERE id = ?",
		instanceID,
	).Scan(&inst.ID, &inst.UserID, &inst.Name, &inst.Directive, &inst.Mode, &inst.Config, &inst.Port, &inst.Pid, &inst.Status, &inst.ProjectID, &createdAt)
	if err != nil {
		return nil, err
	}
	inst.CreatedAt, _ = parseTime(createdAt)
	return &inst, nil
}

func (s *Store) GetInstance(userID, instanceID int64) (*Instance, error) {
	var inst Instance
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, user_id, name, directive, COALESCE(mode,'autonomous'), config, port, pid, status, COALESCE(project_id,''), created_at FROM instances WHERE id = ? AND user_id = ?",
		instanceID, userID,
	).Scan(&inst.ID, &inst.UserID, &inst.Name, &inst.Directive, &inst.Mode, &inst.Config, &inst.Port, &inst.Pid, &inst.Status, &inst.ProjectID, &createdAt)
	if err != nil {
		return nil, err
	}
	inst.CreatedAt, _ = parseTime(createdAt)
	return &inst, nil
}

func (s *Store) ListInstances(userID int64, projectID string) ([]Instance, error) {
	var rows *sql.Rows
	var err error
	if projectID != "" {
		rows, err = s.db.Query(
			"SELECT id, name, directive, COALESCE(mode,'autonomous'), port, pid, status, COALESCE(project_id,''), created_at FROM instances WHERE user_id = ? AND project_id = ?", userID, projectID)
	} else {
		rows, err = s.db.Query(
			"SELECT id, name, directive, COALESCE(mode,'autonomous'), port, pid, status, COALESCE(project_id,''), created_at FROM instances WHERE user_id = ?", userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		var createdAt string
		rows.Scan(&inst.ID, &inst.Name, &inst.Directive, &inst.Mode, &inst.Port, &inst.Pid, &inst.Status, &inst.ProjectID, &createdAt)
		inst.UserID = userID
		inst.CreatedAt, _ = parseTime(createdAt)
		instances = append(instances, inst)
	}
	return instances, nil
}

func (s *Store) UpdateInstance(inst *Instance) error {
	_, err := s.db.Exec(
		"UPDATE instances SET name=?, directive=?, mode=?, config=?, port=?, pid=?, status=?, project_id=? WHERE id=?",
		inst.Name, inst.Directive, inst.Mode, inst.Config, inst.Port, inst.Pid, inst.Status, inst.ProjectID, inst.ID,
	)
	return err
}

// ListInstancesByStatus scans every user's instances for ones in the given
// status. Used by the server's boot-time resume path to find everything
// that was `running` before the last shutdown and re-spawn those cores.
// The result is unsorted; callers that need ordering should sort themselves.
func (s *Store) ListInstancesByStatus(status string) ([]Instance, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, directive, COALESCE(mode,'autonomous'), config, port, pid, status, COALESCE(project_id,''), created_at
		 FROM instances WHERE status = ?`,
		status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		var createdAt string
		rows.Scan(&inst.ID, &inst.UserID, &inst.Name, &inst.Directive, &inst.Mode, &inst.Config, &inst.Port, &inst.Pid, &inst.Status, &inst.ProjectID, &createdAt)
		inst.CreatedAt, _ = parseTime(createdAt)
		instances = append(instances, inst)
	}
	return instances, nil
}

// DeleteInstance removes an instance row plus every per-instance row
// in the server's own DB. Tables here lack ON DELETE CASCADE, so a
// naive `DELETE FROM instances` left telemetry/channels/subscriptions/
// bindings behind — we found ~100 orphan instance_ids in telemetry
// alone in production. Each child delete is its own statement
// (rather than a single CTE) because the server's sqlite driver
// doesn't run multi-statement Execs reliably across versions.
//
// App-side state (channel-chat chats/messages, future helpdesk
// tickets, etc.) is NOT touched here — that's the apps registry's
// job via NotifyInstanceDetach. The caller in instances.go fires
// that hook before invoking us.
func (s *Store) DeleteInstance(userID, instanceID int64) error {
	// Verify ownership first — the deletes below are unscoped by
	// user_id, so a missing ownership check would let any caller
	// blow away another tenant's data if they knew the id.
	var owner int64
	if err := s.db.QueryRow("SELECT user_id FROM instances WHERE id = ?", instanceID).Scan(&owner); err != nil {
		if err == sql.ErrNoRows {
			return nil // already gone — idempotent
		}
		return err
	}
	if owner != userID {
		return fmt.Errorf("instance %d not owned by user %d", instanceID, userID)
	}

	stmts := []string{
		"DELETE FROM telemetry             WHERE instance_id = ?",
		"DELETE FROM channels              WHERE instance_id = ?",
		"DELETE FROM subscriptions         WHERE instance_id = ?",
		"DELETE FROM app_instance_bindings WHERE instance_id = ?",
		"DELETE FROM instances             WHERE id = ? AND user_id = ?",
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
	_, err := s.db.Exec(
		"INSERT INTO projects (id, user_id, name, description, color) VALUES (?, ?, ?, ?, ?)",
		id, userID, name, description, color,
	)
	if err != nil {
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

func (s *Store) GetSession(token string) (int64, error) {
	var userID int64
	var expiresAt string
	err := s.db.QueryRow(
		"SELECT user_id, expires_at FROM sessions WHERE token = ?", token,
	).Scan(&userID, &expiresAt)
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
	return userID, nil
}

// parseTime tries multiple formats that SQLite may return.
func parseTime(s string) (time.Time, error) {
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05+00:00",
		"2006-01-02 15:04:05-07:00",
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

// --- Channels ---

type ChannelRecord struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	InstanceID int64  `json:"instance_id"`
	ProjectID  string `json:"project_id,omitempty"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

func (s *Store) CreateChannel(userID, instanceID int64, chType, name, encryptedConfig string, projectID ...string) (*ChannelRecord, error) {
	pid := ""
	if len(projectID) > 0 {
		pid = projectID[0]
	}
	res, err := s.db.Exec(
		"INSERT INTO channels (user_id, instance_id, type, name, encrypted_config, project_id) VALUES (?, ?, ?, ?, ?, ?)",
		userID, instanceID, chType, name, encryptedConfig, pid,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &ChannelRecord{ID: id, UserID: userID, InstanceID: instanceID, ProjectID: pid, Type: chType, Name: name, Status: "active"}, nil
}

func (s *Store) ListChannels(instanceID int64) ([]ChannelRecord, error) {
	rows, err := s.db.Query("SELECT id, user_id, instance_id, COALESCE(project_id,''), type, name, status, created_at FROM channels WHERE instance_id = ? AND status = 'active'", instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRecord
	for rows.Next() {
		var c ChannelRecord
		rows.Scan(&c.ID, &c.UserID, &c.InstanceID, &c.ProjectID, &c.Type, &c.Name, &c.Status, &c.CreatedAt)
		out = append(out, c)
	}
	return out, nil
}

// ListChannelsByProject returns all channels for a project (including project-level ones with instance_id=0).
func (s *Store) ListChannelsByProject(projectID string, chType string) ([]ChannelRecord, error) {
	rows, err := s.db.Query(
		"SELECT id, user_id, instance_id, COALESCE(project_id,''), type, name, encrypted_config, status, created_at FROM channels WHERE project_id = ? AND type = ? AND status = 'active'",
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
		rows.Scan(&c.ID, &c.UserID, &c.InstanceID, &c.ProjectID, &c.Type, &c.Name, &enc, &c.Status, &c.CreatedAt)
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
