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

type Instance struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Name      string    `json:"name"`
	Directive string    `json:"directive"`
	Config    string    `json:"config"` // JSON blob
	Port      int       `json:"port"`
	Pid       int       `json:"pid"`
	Status    string    `json:"status"` // running, stopped
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

	// Enable WAL mode for better concurrency
	db.Exec("PRAGMA journal_mode=WAL")

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
		CREATE TABLE IF NOT EXISTS instances (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id),
			name TEXT NOT NULL,
			directive TEXT DEFAULT '',
			config TEXT DEFAULT '{}',
			port INTEGER DEFAULT 0,
			pid INTEGER DEFAULT 0,
			status TEXT DEFAULT 'stopped',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	return err
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

func (s *Store) GetUserByEmail(email string) (*User, error) {
	var u User
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, email, password_hash, created_at FROM users WHERE email = ?", email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &createdAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
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
		k.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *Store) DeleteAPIKey(userID, keyID int64) error {
	_, err := s.db.Exec("DELETE FROM api_keys WHERE id = ? AND user_id = ?", keyID, userID)
	return err
}

// --- Instances ---

func (s *Store) CreateInstance(userID int64, name, directive, config string) (*Instance, error) {
	result, err := s.db.Exec(
		"INSERT INTO instances (user_id, name, directive, config) VALUES (?, ?, ?, ?)",
		userID, name, directive, config,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &Instance{ID: id, UserID: userID, Name: name, Directive: directive, Config: config, Status: "stopped", CreatedAt: time.Now()}, nil
}

func (s *Store) GetInstance(userID, instanceID int64) (*Instance, error) {
	var inst Instance
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, user_id, name, directive, config, port, pid, status, created_at FROM instances WHERE id = ? AND user_id = ?",
		instanceID, userID,
	).Scan(&inst.ID, &inst.UserID, &inst.Name, &inst.Directive, &inst.Config, &inst.Port, &inst.Pid, &inst.Status, &createdAt)
	if err != nil {
		return nil, err
	}
	inst.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	return &inst, nil
}

func (s *Store) ListInstances(userID int64) ([]Instance, error) {
	rows, err := s.db.Query(
		"SELECT id, name, directive, port, pid, status, created_at FROM instances WHERE user_id = ?", userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		var createdAt string
		rows.Scan(&inst.ID, &inst.Name, &inst.Directive, &inst.Port, &inst.Pid, &inst.Status, &createdAt)
		inst.UserID = userID
		inst.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		instances = append(instances, inst)
	}
	return instances, nil
}

func (s *Store) UpdateInstance(inst *Instance) error {
	_, err := s.db.Exec(
		"UPDATE instances SET directive=?, config=?, port=?, pid=?, status=? WHERE id=?",
		inst.Directive, inst.Config, inst.Port, inst.Pid, inst.Status, inst.ID,
	)
	return err
}

func (s *Store) DeleteInstance(userID, instanceID int64) error {
	_, err := s.db.Exec("DELETE FROM instances WHERE id = ? AND user_id = ?", instanceID, userID)
	return err
}
