package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// Runtime app settings. A runtime's app copies are new install rows with no
// settings of their own. RuntimeCreateRequest.AppConfig sets them per app
// name; they are validated against the app's config_schema, stored on the
// copy's install row before it first starts, and handed to the sidecar as
// APTEVA_APP_CONFIG, so no restart is needed.

const (
	maxRuntimeAppConfigApps     = 32
	maxRuntimeAppConfigKeys     = 128
	maxRuntimeAppConfigValueLen = 16 << 10
)

func validateRuntimeAppConfigShape(cfg map[string]map[string]string) error {
	if len(cfg) > maxRuntimeAppConfigApps {
		return fmt.Errorf("at most %d apps", maxRuntimeAppConfigApps)
	}
	for app, values := range cfg {
		if strings.TrimSpace(app) == "" {
			return fmt.Errorf("app name required")
		}
		if len(values) > maxRuntimeAppConfigKeys {
			return fmt.Errorf("%s: at most %d settings", app, maxRuntimeAppConfigKeys)
		}
		for key, value := range values {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("%s: setting name required", app)
			}
			if len(value) > maxRuntimeAppConfigValueLen {
				return fmt.Errorf("%s.%s: value longer than %d bytes", app, key, maxRuntimeAppConfigValueLen)
			}
		}
	}
	return nil
}

// validateRuntimeAppConfig checks values against the manifest's config_schema
// with the same key rule as the install settings endpoint, plus the value
// shapes the dashboard writes for boolean and select fields.
func validateRuntimeAppConfig(m *sdk.Manifest, values map[string]string) error {
	known := make(map[string]sdk.ConfigField, len(m.ConfigSchema))
	for _, f := range m.ConfigSchema {
		known[f.Name] = f
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		field, ok := known[key]
		if !ok {
			return fmt.Errorf("unknown config key: %s", key)
		}
		value := values[key]
		switch strings.ToLower(strings.TrimSpace(field.Type)) {
		case "boolean", "bool", "toggle":
			if value != "true" && value != "false" {
				return fmt.Errorf("%s must be \"true\" or \"false\"", key)
			}
		case "select":
			if len(field.Options) > 0 && value != "" && !containsString(field.Options, value) {
				return fmt.Errorf("%s must be one of %s", key, strings.Join(field.Options, ", "))
			}
		}
	}
	return nil
}

// storeRuntimeAppConfig writes the settings to a reserved copy's install row.
func (s *Server) storeRuntimeAppConfig(installID int64, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	enc, err := Encrypt(s.secret, string(raw))
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	_, err = s.store.dbExec(`UPDATE app_installs SET config_encrypted = ? WHERE id = ?`, enc, installID)
	return err
}

// runtimeAppConfigEnv is the APTEVA_APP_CONFIG value for a copy's first
// start, read back from its row like every later respawn does.
func (s *Server) runtimeAppConfigEnv(installID int64) (string, error) {
	cfg, err := decryptInstallConfig(s, installID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(cfg)
	return string(raw), err
}

// unusedRuntimeAppConfig names AppConfig entries that matched no app copy.
func unusedRuntimeAppConfig(cfg map[string]map[string]string, used map[string]bool) []string {
	var out []string
	for app := range cfg {
		if !used[app] {
			out = append(out, app)
		}
	}
	sort.Strings(out)
	return out
}
