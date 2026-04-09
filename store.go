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
	Mode      string    `json:"mode"` // "autonomous" or "supervised"
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
			(8, 'browser', 'Browserbase', 'Browser automation', '["BROWSERBASE_API_KEY","BROWSERBASE_PROJECT_ID"]', 1, 40);

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
	// is_default removed — default is per-instance, stored in instances.config
	s.db.Exec("ALTER TABLE instances ADD COLUMN mode TEXT DEFAULT 'autonomous'")
	s.db.Exec("ALTER TABLE subscriptions ADD COLUMN external_webhook_id TEXT DEFAULT ''")

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
		"UPDATE instances SET directive=?, mode=?, config=?, port=?, pid=?, status=?, project_id=? WHERE id=?",
		inst.Directive, inst.Mode, inst.Config, inst.Port, inst.Pid, inst.Status, inst.ProjectID, inst.ID,
	)
	return err
}

func (s *Store) DeleteInstance(userID, instanceID int64) error {
	_, err := s.db.Exec("DELETE FROM instances WHERE id = ? AND user_id = ?", instanceID, userID)
	return err
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
	Type       string `json:"type"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

func (s *Store) CreateChannel(userID, instanceID int64, chType, name, encryptedConfig string) (*ChannelRecord, error) {
	res, err := s.db.Exec(
		"INSERT INTO channels (user_id, instance_id, type, name, encrypted_config) VALUES (?, ?, ?, ?, ?)",
		userID, instanceID, chType, name, encryptedConfig,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &ChannelRecord{ID: id, UserID: userID, InstanceID: instanceID, Type: chType, Name: name, Status: "active"}, nil
}

func (s *Store) ListChannels(instanceID int64) ([]ChannelRecord, error) {
	rows, err := s.db.Query("SELECT id, user_id, instance_id, type, name, status, created_at FROM channels WHERE instance_id = ? AND status = 'active'", instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRecord
	for rows.Next() {
		var c ChannelRecord
		rows.Scan(&c.ID, &c.UserID, &c.InstanceID, &c.Type, &c.Name, &c.Status, &c.CreatedAt)
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
