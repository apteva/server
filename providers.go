package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type ProviderType struct {
	ID                  int64    `json:"id"`
	Type                string   `json:"type"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Fields              []string `json:"fields"`
	RequiresCredentials bool     `json:"requires_credentials"`
	SortOrder           int      `json:"sort_order"`
}

type Provider struct {
	ID             int64     `json:"id"`
	UserID         int64     `json:"user_id"`
	ProviderTypeID int64     `json:"provider_type_id"`
	Type           string    `json:"type"`
	Name           string    `json:"name"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// --- Store methods ---

func (s *Store) ListProviderTypes() ([]ProviderType, error) {
	rows, err := s.db.Query("SELECT id, type, name, description, fields, requires_credentials, sort_order FROM provider_types ORDER BY sort_order")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []ProviderType
	for rows.Next() {
		var pt ProviderType
		var fieldsJSON string
		var reqCreds int
		rows.Scan(&pt.ID, &pt.Type, &pt.Name, &pt.Description, &fieldsJSON, &reqCreds, &pt.SortOrder)
		json.Unmarshal([]byte(fieldsJSON), &pt.Fields)
		pt.RequiresCredentials = reqCreds == 1
		types = append(types, pt)
	}
	return types, nil
}

func (s *Store) CreateProvider(userID, providerTypeID int64, ptype, name, encryptedData string) (*Provider, error) {
	result, err := s.db.Exec(
		"INSERT INTO providers (user_id, provider_type_id, type, name, encrypted_data) VALUES (?, ?, ?, ?, ?)",
		userID, providerTypeID, ptype, name, encryptedData,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &Provider{ID: id, UserID: userID, ProviderTypeID: providerTypeID, Type: ptype, Name: name, Status: "active", CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (s *Store) ListProviders(userID int64) ([]Provider, error) {
	rows, err := s.db.Query(
		"SELECT id, provider_type_id, type, name, COALESCE(status,'active'), created_at, updated_at FROM providers WHERE user_id = ?", userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []Provider
	for rows.Next() {
		var p Provider
		var createdAt, updatedAt string
		rows.Scan(&p.ID, &p.ProviderTypeID, &p.Type, &p.Name, &p.Status, &createdAt, &updatedAt)
		p.UserID = userID
		p.CreatedAt, _ = parseTime(createdAt)
		p.UpdatedAt, _ = parseTime(updatedAt)
		providers = append(providers, p)
	}
	return providers, nil
}

func (s *Store) GetProvider(userID, providerID int64) (*Provider, string, error) {
	var p Provider
	var encryptedData, createdAt, updatedAt string
	err := s.db.QueryRow(
		"SELECT id, type, name, encrypted_data, created_at, updated_at FROM providers WHERE id = ? AND user_id = ?",
		providerID, userID,
	).Scan(&p.ID, &p.Type, &p.Name, &encryptedData, &createdAt, &updatedAt)
	if err != nil {
		return nil, "", err
	}
	p.UserID = userID
	p.CreatedAt, _ = parseTime(createdAt)
	p.UpdatedAt, _ = parseTime(updatedAt)
	return &p, encryptedData, nil
}

func (s *Store) UpdateProvider(userID, providerID int64, ptype, name, encryptedData string) error {
	_, err := s.db.Exec(
		"UPDATE providers SET type=?, name=?, encrypted_data=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?",
		ptype, name, encryptedData, providerID, userID,
	)
	return err
}

func (s *Store) DeleteProvider(userID, providerID int64) error {
	_, err := s.db.Exec("DELETE FROM providers WHERE id = ? AND user_id = ?", providerID, userID)
	return err
}

// GetAllProviderEnvVars decrypts all providers for a user and returns env vars (UPPER_CASE keys).
func (s *Store) GetAllProviderEnvVars(userID int64, secret []byte) (map[string]string, error) {
	rows, err := s.db.Query("SELECT encrypted_data FROM providers WHERE user_id = ?", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	envVars := map[string]string{}
	for rows.Next() {
		var encData string
		rows.Scan(&encData)

		plaintext, err := Decrypt(secret, encData)
		if err != nil {
			continue
		}

		var data map[string]string
		if err := json.Unmarshal([]byte(plaintext), &data); err != nil {
			continue
		}

		for k, v := range data {
			// Only inject UPPER_CASE keys as env vars
			if isEnvVar(k) {
				envVars[k] = v
			}
		}
	}
	return envVars, nil
}

// isEnvVar returns true if the key looks like an env var (UPPER_CASE_WITH_UNDERSCORES).
func isEnvVar(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// --- HTTP Handlers ---

// GET /provider-types
func (s *Server) handleListProviderTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	types, err := s.store.ListProviderTypes()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if types == nil {
		types = []ProviderType{}
	}
	writeJSON(w, types)
}

// POST /providers
func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		ProviderTypeID int64             `json:"provider_type_id"`
		Type           string            `json:"type"`
		Name           string            `json:"name"`
		Data           map[string]string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Type == "" || body.Name == "" {
		http.Error(w, "type and name required", http.StatusBadRequest)
		return
	}

	// Allow empty data for providers that don't require credentials
	if body.Data == nil {
		body.Data = map[string]string{}
	}

	dataJSON, _ := json.Marshal(body.Data)
	encrypted, err := Encrypt(s.secret, string(dataJSON))
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	provider, err := s.store.CreateProvider(userID, body.ProviderTypeID, body.Type, body.Name, encrypted)
	if err != nil {
		http.Error(w, "failed to create provider", http.StatusInternalServerError)
		return
	}

	writeJSON(w, provider)
}

// GET /providers
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	providers, err := s.store.ListProviders(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if providers == nil {
		providers = []Provider{}
	}
	writeJSON(w, providers)
}

// GET /providers/:id — returns provider with masked data
func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/providers/")
	providerID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}

	provider, encData, err := s.store.GetProvider(userID, providerID)
	if err != nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	// Decrypt and mask secrets
	plaintext, err := Decrypt(s.secret, encData)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}

	var data map[string]string
	json.Unmarshal([]byte(plaintext), &data)

	masked := map[string]string{}
	for k, v := range data {
		if isEnvVar(k) && len(v) > 8 {
			masked[k] = v[:4] + "..." + v[len(v)-4:]
		} else {
			masked[k] = v
		}
	}

	writeJSON(w, map[string]any{
		"id":         provider.ID,
		"type":       provider.Type,
		"name":       provider.Name,
		"data":       masked,
		"created_at": provider.CreatedAt,
		"updated_at": provider.UpdatedAt,
	})
}

// PUT /providers/:id
func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/providers/")
	providerID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}

	// Verify it exists
	_, _, err = s.store.GetProvider(userID, providerID)
	if err != nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	var body struct {
		Type string            `json:"type"`
		Name string            `json:"name"`
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Type == "" || body.Name == "" {
		http.Error(w, "type and name required", http.StatusBadRequest)
		return
	}

	dataJSON, _ := json.Marshal(body.Data)
	encrypted, err := Encrypt(s.secret, string(dataJSON))
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	if err := s.store.UpdateProvider(userID, providerID, body.Type, body.Name, encrypted); err != nil {
		http.Error(w, "failed to update", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "updated"})
}

// DELETE /providers/:id
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/providers/")
	providerID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid provider ID", http.StatusBadRequest)
		return
	}
	s.store.DeleteProvider(userID, providerID)
	writeJSON(w, map[string]string{"status": "deleted"})
}
