package main

import (
	"context"
)

type storedListedApp struct {
	installID, appID                                                int64
	projID, status, statusMsg, errMsg                               string
	upgradePolicy, version, permsJSON                               string
	name, source, manifestJSON, availableManifestJSON, bindingsJSON string
	hasPendingOptions, defaultForNewAgents                          int
	installation                                                    appInstallation
	upgrading, hasComponents                                        bool
}

// Cursor ownership stays here: enrichment cannot begin before this returns.
func (s *Server) readListedApps(ctx context.Context, query string, args ...any) ([]storedListedApp, error) {
	ctx, cancel := s.store.databaseContext(ctx)
	defer cancel()
	rows, err := s.store.dbQueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []storedListedApp{}
	for rows.Next() {
		var record storedListedApp
		if err := rows.Scan(&record.installID, &record.appID, &record.projID, &record.status, &record.statusMsg, &record.errMsg,
			&record.upgradePolicy, &record.version, &record.permsJSON, &record.name, &record.source, &record.manifestJSON, &record.availableManifestJSON,
			&record.bindingsJSON, &record.hasPendingOptions, &record.defaultForNewAgents, &record.upgrading, &record.installation.Key, &record.installation.Name, &record.installation.Default, &record.hasComponents); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}
