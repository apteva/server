package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

func (s *Server) appTokenCipherKey() ([]byte, error) {
	if len(s.secret) == 16 || len(s.secret) == 24 || len(s.secret) == 32 {
		return s.secret, nil
	}
	const setting = "app_token_encryption_key"
	if encoded := s.store.GetSetting(setting); encoded != "" {
		key, err := hex.DecodeString(encoded)
		if err == nil && len(key) == 32 {
			return key, nil
		}
	}
	encoded := generateToken(32)
	if err := s.store.SetSetting(setting, encoded); err != nil {
		return nil, err
	}
	return hex.DecodeString(encoded)
}

// appInstallToken returns a stable, random credential for one app install.
// Older installs are upgraded lazily so a server update does not require a
// destructive migration or reinstall.
func (s *Server) appInstallToken(installID int64) (string, error) {
	if s == nil || s.store == nil || installID <= 0 {
		return "", fmt.Errorf("invalid app install")
	}
	s.appTokenMu.Lock()
	defer s.appTokenMu.Unlock()
	key, err := s.appTokenCipherKey()
	if err != nil {
		return "", err
	}

	var encrypted, hash string
	err = s.store.db.QueryRow(
		`SELECT COALESCE(app_token_encrypted,''), COALESCE(app_token_hash,'') FROM app_installs WHERE id=?`,
		installID,
	).Scan(&encrypted, &hash)
	if err != nil {
		return "", err
	}
	if encrypted != "" && hash != "" {
		if token, err := Decrypt(key, encrypted); err == nil && token != "" && HashAPIKey(token) == hash {
			return token, nil
		}
	}

	token := "app_" + generateToken(32)
	encrypted, err = Encrypt(key, token)
	if err != nil {
		return "", err
	}
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET app_token_hash=?, app_token_encrypted=? WHERE id=?`,
		HashAPIKey(token), encrypted, installID,
	); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Server) appInstallForToken(token string) (installID, installedBy int64, status string, err error) {
	if token == "" {
		return 0, 0, "", sql.ErrNoRows
	}
	err = s.store.db.QueryRow(
		`SELECT id, COALESCE(installed_by,0), status FROM app_installs WHERE app_token_hash=?`,
		HashAPIKey(token),
	).Scan(&installID, &installedBy, &status)
	return
}

// App credentials are capabilities for the app data plane, not user API
// credentials. Never allow one to reach ordinary management handlers.
func appTokenRouteAllowed(path string) bool {
	switch {
	case path == "/app-events/internal/emit":
		return true
	case strings.HasPrefix(path, "/app-events/"):
		// App-event stream handlers perform the project/global scope check
		// using X-Apteva-App-Install-ID. Authentication must let the install
		// token reach that handler, but only for a single app lane.
		lane := strings.Trim(strings.TrimPrefix(path, "/app-events/"), "/")
		return lane != "" && !strings.Contains(lane, "/")
	case strings.HasPrefix(path, "/apps/callback/"):
		return true
	case strings.HasPrefix(path, "/apps/"):
		first := strings.TrimPrefix(path, "/apps/")
		if i := strings.IndexByte(first, '/'); i >= 0 {
			first = first[:i]
		}
		return !isAppManagementRoute(first)
	default:
		return false
	}
}

func legacyAppTokenInstallID(r *http.Request, token string) int64 {
	if !requestFromLoopback(r) || !appTokenRouteAllowed(r.URL.Path) {
		return 0
	}
	return installIDFromDevAPIKey(token)
}
