package main

import (
	"encoding/json"
	"sync"

	sdk "github.com/apteva/app-sdk"
)

var appCatalogMetadataMu sync.Mutex

// updateAppCatalogMetadata advances the shared marketplace/latest snapshot
// without letting an older project-pinned install move it backwards. Runtime
// behavior never reads this row when an install snapshot is present.
func (s *Server) updateAppCatalogMetadata(appID int64, manifest *sdk.Manifest, source, repo, ref string) {
	if s == nil || s.store == nil || manifest == nil || appID <= 0 {
		return
	}
	appCatalogMetadataMu.Lock()
	defer appCatalogMetadataMu.Unlock()
	var currentJSON string
	if err := s.store.db.QueryRow(`SELECT manifest_json FROM apps WHERE id = ?`, appID).Scan(&currentJSON); err != nil {
		return
	}
	var current sdk.Manifest
	_ = json.Unmarshal([]byte(currentJSON), &current)
	if current.Version != "" && manifest.Version != "" && semverLess(manifest.Version, current.Version) {
		return
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return
	}
	_, _ = s.store.db.Exec(
		`UPDATE apps
		 SET manifest_json = ?,
		     source = COALESCE(NULLIF(?, ''), source),
		     repo = COALESCE(NULLIF(?, ''), repo),
		     ref = COALESCE(NULLIF(?, ''), ref)
		 WHERE id = ?`,
		string(raw), source, repo, ref, appID,
	)
}

func (s *Server) updateAppCatalogMetadataByName(appName string, manifest *sdk.Manifest) {
	if s == nil || s.store == nil || appName == "" {
		return
	}
	var appID int64
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, appName).Scan(&appID); err != nil {
		return
	}
	s.updateAppCatalogMetadata(appID, manifest, "", "", "")
}
