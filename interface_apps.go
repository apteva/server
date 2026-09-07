package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	sdk "github.com/apteva/app-sdk"
)

// Conversations is product infrastructure for Personal and Business, not a
// marketplace choice. This path provisions only that fixed dependency using
// the operator's configured registry and existing installer. It does not grant
// ordinary users app-management privileges or activate a platform Helper.
func (s *Server) prepareInterfaceApps(w http.ResponseWriter, userID int64, level string) bool {
	if level != "personal" && level != "business" {
		return true
	}
	if err := s.ensureInterfaceConversations(userID); err != nil {
		log.Printf("[INTERFACE] prepare Conversations user=%d: %v", userID, err)
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
			"error": "We couldn't prepare conversations for your workspace. Please try again, or contact your administrator if this continues.",
			"code":  "conversations_setup_failed",
		})
		return false
	}
	return true
}

func (s *Server) ensureInterfaceConversations(userID int64) error {
	s.interfaceAppsMu.Lock()
	defer s.interfaceAppsMu.Unlock()

	if _, err := s.store.GetUserByID(userID); err != nil {
		return err
	}
	// A restricted hosted workspace must still respect its operator's app
	// allowlist. The general marketplace-install capability is not needed for
	// this fixed product dependency.
	if s.store.GetPlatformRole(userID) != PlatformAdmin {
		policy, err := s.loadAccessPolicy()
		if err != nil {
			return err
		}
		if len(policy.Capabilities.AllowedApps) > 0 && !containsExact(policy.Capabilities.AllowedApps, defaultConversationsApp) {
			return errors.New("Conversations is not allowed by the server policy")
		}
	}

	// Only a global installation is shared by every workspace. Never reuse or
	// promote a private project installation belonging to another user.
	var installID int64
	var status string
	err := s.store.db.QueryRow(`SELECT i.id, i.status
		FROM app_installs i JOIN apps a ON a.id=i.app_id
		WHERE a.name=? AND COALESCE(i.project_id,'')=''
		ORDER BY i.id LIMIT 1`, defaultConversationsApp).Scan(&installID, &status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if status != "running" {
		if s.localApps == nil {
			return errors.New("local app runtime is unavailable")
		}
		if installID > 0 {
			// Retry a failed/stopped install in place, preserving its data and
			// attachments. The installer coalesces an already-pending build.
			manifest, err := installManifest(s, installID)
			if err != nil {
				return err
			}
			storedConfig, err := decryptInstallConfig(s, installID)
			if err != nil {
				return err
			}
			config := make(map[string]string, len(storedConfig))
			for key, value := range storedConfig {
				if text, ok := value.(string); ok {
					config[key] = text
				} else {
					raw, err := json.Marshal(value)
					if err != nil {
						return err
					}
					config[key] = string(raw)
				}
			}
			if err := s.installInterfaceConversations(installID, manifest, config); err != nil {
				return err
			}
		} else {
			registry, err := s.fetchAndCacheRegistry()
			if err != nil {
				return err
			}
			manifestURL := ""
			for _, entry := range registry.Apps {
				if entry.Name == defaultConversationsApp {
					manifestURL = entry.ManifestURL
					break
				}
			}
			manifest, err := s.fetchAndCacheManifest(manifestURL)
			if err != nil {
				return err
			}
			if manifest.Name != defaultConversationsApp || !manifestAllowsScope(manifest, sdk.ScopeGlobal) {
				return errors.New("registry entry is not a global Conversations app")
			}
			// Keep the shared infrastructure owned by the server administrator,
			// not the first member who happens to select Personal or Business.
			ownerID, err := s.firstPlatformAdmin()
			if err != nil {
				return fmt.Errorf("find platform owner: %w", err)
			}
			if _, err := s.installDependencies(ownerID, manifest, "", nil); err != nil {
				return err
			}
			installID, err = s.installAppFromManifest(ownerID, manifest, "")
			if err != nil {
				return err
			}
		}
	}

	if err := s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, installID).Scan(&status); err != nil {
		return err
	}
	if status != "running" {
		return errors.New("Conversations is still starting")
	}
	// Activation normally registers this bridge; repair a missing registration
	// after an interrupted install before reporting preparation complete.
	var registered int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM mcp_servers WHERE upstream_id=?`, appMCPUpstreamID(installID)).Scan(&registered); err != nil {
		return err
	}
	if registered == 0 {
		if err := s.registerAppMCP(installID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) installInterfaceConversations(installID int64, manifest *sdk.Manifest, config map[string]string) error {
	if manifest == nil || manifest.Name != defaultConversationsApp || !manifestAllowsScope(manifest, sdk.ScopeGlobal) {
		return errors.New("invalid Conversations installation manifest")
	}
	if manifest.Runtime.Kind == "source" {
		return s.installFromSource(installID, manifest, "", config)
	}
	return s.installLocally(installID, manifest, "", config)
}
