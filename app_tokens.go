package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
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

// POST /api/apps/installs/:id/runtime-token returns the opaque credential a
// manually launched sidecar must use for outbound PlatformAPI callbacks. The
// route is owner-gated in main.go and uses POST plus no-store because the
// response contains a secret. This also gives external `apteva test --server`
// runs a supported way to launch the checkout under test without falling back
// to the retired predictable dev-<install-id> credentials.
func (s *Server) handleIssueInstallRuntimeToken(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "runtime-token" {
		http.NotFound(w, r)
		return
	}
	installID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || installID <= 0 {
		http.Error(w, "invalid install id", http.StatusBadRequest)
		return
	}
	token, err := s.appInstallToken(installID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "install not found", http.StatusNotFound)
			return
		}
		http.Error(w, "app credential unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]string{"token": token})
}

// App credentials are capabilities for the app data plane, not user API
// credentials. Never allow one to reach ordinary management handlers. The
// environment SDK routes below have their own manifest-permission gates.
func appTokenRouteAllowed(path string) bool {
	switch {
	case path == "/environments":
		return true // handleEnvironments requires environments.read/manage.
	case strings.HasPrefix(path, "/environments/"):
		parts := strings.Split(strings.TrimPrefix(path, "/environments/"), "/")
		if parts[0] == "" || parts[0] == "migrate-to-app" {
			return false
		}
		// Only SDK endpoints that enforce requireEnvironmentPermission or
		// requireEnvironmentAgentPermission. Do not admit lifecycle/migration
		// or arbitrary proxy routes merely because they share this prefix.
		if len(parts) == 1 {
			return true
		}
		if len(parts) == 2 {
			switch parts[1] {
			case "seed", "snapshot", "agents", "agent":
				return true
			}
		}
		return len(parts) == 3 && parts[1] == "agents" && parts[2] != ""
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

// Predictable tokens from old development builds are no longer credentials.
func legacyAppTokenInstallID(r *http.Request, token string) int64 { return 0 }
