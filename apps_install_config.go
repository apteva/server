package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// handleGetInstallConfig returns the operator-set settings for one
// install plus the schema (for the dashboard's auto-generated form).
//
//	GET /api/apps/installs/:id/config →
//	  { config: {…}, schema: [...config_schema entries...] }
//
// Empty `config` for fresh installs that haven't had settings touched
// since install. The schema is whatever the manifest declares —
// dashboard renders one form field per entry.
func (s *Server) handleGetInstallConfig(w http.ResponseWriter, r *http.Request) {
	installID, err := parseInstallIDFromConfigPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	manifest, err := installManifest(s, installID)
	if err != nil || manifest == nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	cfg, err := decryptInstallConfig(s, installID)
	if err != nil {
		http.Error(w, "decrypt: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"config": cfg,
		"schema": manifest.ConfigSchema,
	})
}

// handleSetInstallConfig accepts a partial-or-full settings update,
// merges it with the existing settings (so the dashboard can PUT one
// field at a time without round-tripping the whole config), and
// re-encrypts.
//
//	PUT /api/apps/installs/:id/config
//	Body: { config: { default_visibility: "private", … } }
//
// Validates that every key appears in the manifest's config_schema —
// rejects unknown keys with 400 so silent typos can't accumulate
// unread fields. Type coercion is intentionally minimal; the
// dashboard form is the authority on shapes (the manifest schema
// declares them).
func (s *Server) handleSetInstallConfig(w http.ResponseWriter, r *http.Request) {
	installID, err := parseInstallIDFromConfigPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	manifest, err := installManifest(s, installID)
	if err != nil || manifest == nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	var body struct {
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Config == nil {
		body.Config = map[string]any{}
	}
	known := make(map[string]sdk.ConfigField, len(manifest.ConfigSchema))
	for _, f := range manifest.ConfigSchema {
		known[f.Name] = f
	}
	for k := range body.Config {
		if _, ok := known[k]; !ok {
			http.Error(w, "unknown config key: "+k, http.StatusBadRequest)
			return
		}
	}
	current, err := decryptInstallConfig(s, installID)
	if err != nil {
		http.Error(w, "decrypt: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// json.Marshal sorts map keys, so the before/after strings are
	// directly comparable to detect whether anything actually changed.
	beforeRaw, _ := json.Marshal(current)
	for k, v := range body.Config {
		current[k] = v
	}
	raw, _ := json.Marshal(current)
	enc, err := Encrypt(s.secret, string(raw))
	if err != nil {
		http.Error(w, "encrypt: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET config_encrypted = ? WHERE id = ?`,
		enc, installID,
	); err != nil {
		http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Config reaches the sidecar only as the APTEVA_APP_CONFIG env var,
	// read once at process boot — a running sidecar can't see this
	// change until it's bounced. Respawn it so the new config actually
	// takes effect. Guarded: only when the value genuinely changed AND
	// the supervisor is currently running this install. A stopped,
	// remote, or not-yet-started install just keeps the persisted
	// config for its next boot. Best-effort — the config is already
	// saved, so a failed respawn is a warning, not a 500.
	restarted := false
	if string(beforeRaw) != string(raw) &&
		s.localApps != nil && s.localApps.PID(installID) > 0 {
		if err := s.RespawnLocalInstall(installID); err != nil {
			log.Printf("[APPS-CONFIG] install %d: config saved but respawn failed: %v", installID, err)
		} else {
			restarted = true
		}
	}
	writeJSON(w, map[string]any{"config": current, "restarted": restarted})
}

// parseInstallIDFromConfigPath extracts the install id from
// /apps/installs/<id>/config — common to both GET and PUT.
func parseInstallIDFromConfigPath(urlPath string) (int64, error) {
	rest := strings.TrimPrefix(urlPath, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "config" {
		return 0, errInstallNotFound
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, errInstallBadID
	}
	return id, nil
}

// decryptInstallConfig pulls the install's config_encrypted blob, runs
// it through Decrypt, parses the JSON, and returns a map. Returns an
// empty map (not nil) for installs whose config is blank, so callers
// can always range over the result without nil-checks.
func decryptInstallConfig(s *Server, installID int64) (map[string]any, error) {
	var enc string
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(config_encrypted, '') FROM app_installs WHERE id = ?`,
		installID,
	).Scan(&enc); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if enc == "" {
		return out, nil
	}
	plaintext, err := Decrypt(s.secret, enc)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(plaintext), &out)
	return out, nil
}

// Sentinel errors so the handler can surface a clean message without
// the request inspector having to special-case path-parse failures.
var (
	errInstallNotFound = httpError("install not found")
	errInstallBadID    = httpError("invalid id")
)

type httpError string

func (e httpError) Error() string { return string(e) }
