package main

import (
	"encoding/json"
	"fmt"
	"sync"

	sdk "github.com/apteva/app-sdk"
)

// The generation is changed by SQLite triggers in the same transaction as
// authorization metadata. This also covers lifecycle code, direct SQL, and
// writers outside this process; handler-only invalidation would miss those.
// No TTL grace period is allowed for revoked access. Endpoints and tokens are
// deliberately excluded. Cached entries are immutable and bounded.
type appMetadataCache struct {
	mu         sync.Mutex
	generation int64
	entries    map[int64]*appInstallMetadata
}

type appInstallMetadata struct {
	name, status, project string
	owner                 int64
	manifest              sdk.Manifest
	manifestErr           error
	permissions           map[sdk.Permission]bool
	bindings              map[string]any
}

func (s *Store) migrateAppMetadataGeneration() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS app_metadata_generation (id INTEGER PRIMARY KEY CHECK(id=1), generation INTEGER NOT NULL);
		INSERT OR IGNORE INTO app_metadata_generation VALUES(1,1)`); err != nil {
		return err
	}
	for _, table := range []string{"apps", "app_installs", "users", "projects", "project_members"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			statement := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS app_metadata_%s_%s AFTER %s ON %s BEGIN UPDATE app_metadata_generation SET generation=generation+1 WHERE id=1; END`, table, event, event, table)
			if _, err := s.db.Exec(statement); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) appMetadata(installID int64) (*appInstallMetadata, error) {
	c := &s.store.appMetadata
	c.mu.Lock()
	defer c.mu.Unlock()
	var generation int64
	if err := s.store.db.QueryRow(`SELECT generation FROM app_metadata_generation WHERE id=1`).Scan(&generation); err != nil {
		return nil, err
	}
	if c.generation != generation || c.entries == nil {
		c.generation = generation
		c.entries = make(map[int64]*appInstallMetadata)
	}
	if entry := c.entries[installID]; entry != nil {
		return entry, nil
	}
	entry := &appInstallMetadata{permissions: map[sdk.Permission]bool{}, bindings: map[string]any{}}
	var manifest, permissions, bindings string
	err := s.store.db.QueryRow(`SELECT a.name, i.status, COALESCE(i.project_id,''), COALESCE(i.installed_by,0),
		COALESCE(NULLIF(i.manifest_json,''),a.manifest_json), COALESCE(i.permissions_json,'[]'), COALESCE(i.integration_bindings,'{}')
		FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, installID).
		Scan(&entry.name, &entry.status, &entry.project, &entry.owner, &manifest, &permissions, &bindings)
	if err != nil {
		return nil, err
	}
	entry.manifestErr = json.Unmarshal([]byte(manifest), &entry.manifest)
	var perms []sdk.Permission
	if json.Unmarshal([]byte(permissions), &perms) == nil {
		for _, permission := range perms {
			entry.permissions[permission] = true
		}
	}
	if json.Unmarshal([]byte(bindings), &entry.bindings) != nil {
		entry.bindings = map[string]any{}
	}
	if len(c.entries) >= 1024 {
		c.entries = make(map[int64]*appInstallMetadata)
	}
	c.entries[installID] = entry
	return entry, nil
}

// Binding editors receive private maps, including nested multi-bindings.
func cloneAppBinding(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneAppBinding(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneAppBinding(item)
		}
		return out
	default:
		return value
	}
}
