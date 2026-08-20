package main

// provider_migration.go — moves legacy `providers` rows onto
// `connections`, one row at a time, at boot.
//
// The two tables held the same kind of thing in different shapes: a
// provider stored env var names as literal keys inside its encrypted
// blob (ANTHROPIC_API_KEY), while a connection stores the catalog's
// semantic field names (api_key) and lets runtime.env do the mapping.
// Migrating is therefore a translation, not a copy, and the catalog's
// runtime block is the dictionary.
//
// Three rules keep this safe to run unattended on every boot:
//
//   - Idempotent. A provider already migrated (a connection carries its
//     legacy_provider_id) is skipped.
//   - Non-destructive. The provider row is left in place. Dual-read
//     keeps serving it, and the migrated connection wins only because it
//     carries legacy_provider_id. Deleting rows is a separate, later,
//     deliberate step.
//   - Refuses to guess. When a same-app connection already exists with a
//     DIFFERENT credential, we do not merge, overwrite, or pick — we log
//     the conflict and leave both. Silently choosing one would change
//     which key an operator's agents authenticate with.

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

type providerMigrationResult struct {
	Migrated  int
	Skipped   int
	Conflicts int
}

// migrateProvidersToConnections translates every LLM provider row that
// has a catalog counterpart into a connection.
func (s *Server) migrateProvidersToConnections() providerMigrationResult {
	var result providerMigrationResult
	if s.catalog == nil || s.store == nil {
		return result
	}

	// Build providerKey -> catalog app once. Only entries declaring a
	// runtime block can receive a provider's credentials.
	appByProviderKey := map[string]*AppTemplate{}
	for _, summary := range s.catalog.List() {
		app := s.catalog.Get(summary.Slug)
		if app == nil || app.Runtime == nil {
			continue
		}
		appByProviderKey[app.Runtime.ProviderKey] = app
	}
	if len(appByProviderKey) == 0 {
		return result
	}

	rows, err := s.store.db.Query(
		`SELECT id, user_id, type, name, COALESCE(project_id,''), encrypted_data FROM providers ORDER BY id`)
	if err != nil {
		log.Printf("[PROVIDER-MIGRATION] cannot read providers: %v", err)
		return result
	}
	type providerRow struct {
		id, userID         int64
		typ, name, project string
		encrypted          string
	}
	var pending []providerRow
	for rows.Next() {
		var p providerRow
		if err := rows.Scan(&p.id, &p.userID, &p.typ, &p.name, &p.project, &p.encrypted); err != nil {
			continue
		}
		pending = append(pending, p)
	}
	rows.Close()

	for _, p := range pending {
		providerKey := strings.ToLower(p.typ)
		if providerKey == "llm" {
			providerKey = providerKeyFromName(p.name)
		}
		app := appByProviderKey[providerKey]
		if app == nil || !isLLMKey(providerKey) {
			// Browser/embedding providers have no runtime counterpart yet;
			// they keep working through the providers table untouched.
			result.Skipped++
			continue
		}

		already, err := s.store.connectionForLegacyProvider(p.userID, p.id)
		if err == nil && already != 0 {
			result.Skipped++
			continue
		}

		credentials, err := translateProviderBlob(s.secret, p.encrypted, app)
		if err != nil {
			log.Printf("[PROVIDER-MIGRATION] provider=%d (%s) not translatable: %v", p.id, p.name, err)
			result.Skipped++
			continue
		}
		if len(credentials) == 0 {
			result.Skipped++
			continue
		}

		conflict, err := s.store.conflictingConnection(s.secret, p.userID, app.Slug, p.project, credentials)
		if err == nil && conflict != 0 {
			// An existing connection for the same app holds a different
			// credential. Merging would silently repoint the operator's
			// agents, so both rows stay and a human decides.
			log.Printf("[PROVIDER-MIGRATION] provider=%d (%s) conflicts with connection=%d — "+
				"both hold %s credentials but they differ; leaving both in place",
				p.id, p.name, conflict, app.Slug)
			result.Conflicts++
			continue
		}

		// Model pins and capabilities live beside the credentials in the
		// old blob. They move to runtime_config, not credentials.
		runtimeConfig, cfgErr := translateProviderRuntimeConfig(s.secret, p.encrypted)
		if cfgErr != nil {
			runtimeConfig = map[string]any{}
		}
		if err := s.createMigratedConnection(p.userID, p.id, p.project, p.name, app, credentials, runtimeConfig); err != nil {
			log.Printf("[PROVIDER-MIGRATION] provider=%d (%s) could not be migrated: %v", p.id, p.name, err)
			result.Skipped++
			continue
		}
		result.Migrated++
		log.Printf("[PROVIDER-MIGRATION] provider=%d (%s) → connection for %s", p.id, p.name, app.Slug)
	}

	if result.Migrated > 0 || result.Conflicts > 0 {
		log.Printf("[PROVIDER-MIGRATION] migrated=%d skipped=%d conflicts=%d",
			result.Migrated, result.Skipped, result.Conflicts)
	}
	return result
}

// translateProviderBlob turns a provider's env-var-keyed blob into the
// catalog's credential field names, by reading runtime.env backwards:
// ANTHROPIC_API_KEY: "{{credentials.api_key}}" means whatever the blob
// stored under ANTHROPIC_API_KEY belongs in the field `api_key`.
//
// Only credentials.* templates are invertible. A var sourced from
// config.* or connection.* is derived rather than stored, so there is
// nothing in the blob to carry across.
func translateProviderBlob(secret []byte, encrypted string, app *AppTemplate) (map[string]string, error) {
	plaintext, err := Decrypt(secret, encrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(plaintext), &blob); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	credentials := map[string]string{}
	for envName, tmpl := range app.Runtime.Env {
		field, ok := credentialFieldForTemplate(tmpl)
		if !ok {
			continue
		}
		value, _ := blob[envName].(string)
		if strings.TrimSpace(value) == "" {
			continue
		}
		credentials[field] = value
	}
	return credentials, nil
}

// runtimeConfigKeys are the non-secret settings a provider blob kept
// alongside its credentials. They have to survive the move or a migrated
// provider silently loses its pinned models and the agent falls back to
// whatever apteva-core defaults to — a behaviour change nobody asked for
// and nothing surfaces.
var runtimeConfigKeys = []string{
	"model_large", "model_medium", "model_small",
	"model_capabilities", "builtin_tools",
}

// translateProviderRuntimeConfig lifts the settings half of a provider
// blob into runtime_config. Key names are unchanged — GetProviderPool
// reads model_large et al. identically from either store — so this is a
// move between columns, not a rename.
func translateProviderRuntimeConfig(secret []byte, encrypted string) (map[string]any, error) {
	plaintext, err := Decrypt(secret, encrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(plaintext), &blob); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	config := map[string]any{}
	for _, key := range runtimeConfigKeys {
		value, present := blob[key]
		if !present || value == nil {
			continue
		}
		if str, ok := value.(string); ok && strings.TrimSpace(str) == "" {
			continue
		}
		config[key] = value
	}
	return config, nil
}

// credentialFieldForTemplate extracts `api_key` from
// "{{credentials.api_key}}". Returns false for anything that isn't a
// single, whole-string credentials reference — a composite like
// "Bearer {{credentials.token}}" has no unambiguous inverse.
func credentialFieldForTemplate(tmpl string) (string, bool) {
	trimmed := strings.TrimSpace(tmpl)
	if !strings.HasPrefix(trimmed, "{{") || !strings.HasSuffix(trimmed, "}}") {
		return "", false
	}
	ref := strings.TrimSpace(trimmed[2 : len(trimmed)-2])
	if strings.Count(ref, "{{") > 0 {
		return "", false
	}
	const prefix = "credentials."
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	field := strings.TrimPrefix(ref, prefix)
	if field == "" || strings.Contains(field, ".") {
		// Nested paths describe OAuth token state the device-auth flow
		// mints; there is no flat blob field to migrate.
		return "", false
	}
	return field, true
}

// connectionForLegacyProvider returns the id of the connection already
// migrated from this provider row, or 0.
func (s *Store) connectionForLegacyProvider(userID, providerID int64) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM connections WHERE user_id = ? AND legacy_provider_id = ? LIMIT 1`,
		userID, providerID,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// conflictingConnection finds an existing connection for the same app
// and scope whose credentials differ from what we are about to migrate.
// An identical one is not a conflict — it's the same key already moved
// across by hand, and the migration can safely treat the provider row as
// redundant.
func (s *Store) conflictingConnection(
	secret []byte, userID int64, appSlug, projectID string, incoming map[string]string,
) (int64, error) {
	rows, err := s.db.Query(
		`SELECT id, encrypted_credentials FROM connections
		 WHERE user_id = ? AND app_slug = ? AND COALESCE(project_id,'') = ?`,
		userID, appSlug, projectID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var encrypted string
		if err := rows.Scan(&id, &encrypted); err != nil {
			continue
		}
		plaintext, err := Decrypt(secret, encrypted)
		if err != nil {
			// Unreadable is treated as conflicting: we cannot prove it is
			// the same credential, so we must not assume it away.
			return id, nil
		}
		existing := map[string]string{}
		if err := json.Unmarshal([]byte(plaintext), &existing); err != nil {
			return id, nil
		}
		if !sameCredentials(existing, incoming) {
			return id, nil
		}
	}
	return 0, rows.Err()
}

// sameCredentials compares only the fields the migration would write —
// an existing connection may legitimately carry extra keys (refresh
// tokens, expiry) that a provider blob never had.
func sameCredentials(existing, incoming map[string]string) bool {
	for field, value := range incoming {
		if strings.TrimSpace(existing[field]) != strings.TrimSpace(value) {
			return false
		}
	}
	return true
}

// createMigratedConnection writes the translated credential as a
// connection stamped with the provider row it came from.
//
// auto_mcp is off: these credentials exist to back the agent runtime.
// Exposing the provider's REST tools to every agent in the project is a
// separate decision, and turning it on during a migration would hand
// agents capabilities they did not have yesterday.
func (s *Server) createMigratedConnection(
	userID, providerID int64, projectID, providerName string,
	app *AppTemplate, credentials map[string]string, runtimeConfig map[string]any,
) error {
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	encrypted, err := Encrypt(s.secret, string(encoded))
	if err != nil {
		return err
	}
	authType := "api_key"
	if len(app.Auth.Types) > 0 {
		authType = app.Auth.Types[0]
	}
	autoMCP := false
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: userID, AppSlug: app.Slug, AppName: app.Name,
		Name:           providerName,
		AuthType:       authType,
		EncryptedCreds: encrypted,
		ProjectID:      projectID,
		Status:         "active",
		CreatedVia:     "integration",
		AutoMCP:        &autoMCP,
	})
	if err != nil {
		return err
	}
	if _, err := s.store.db.Exec(
		`UPDATE connections SET legacy_provider_id = ? WHERE id = ?`, providerID, conn.ID); err != nil {
		return err
	}
	if len(runtimeConfig) > 0 {
		encodedConfig, err := json.Marshal(runtimeConfig)
		if err != nil {
			return err
		}
		return s.store.UpdateConnectionRuntimeConfig(conn.ID, string(encodedConfig))
	}
	return nil
}
