package main

import "context"

type storedRunningInstall struct {
	id, appID                                                              int64
	projectID, serviceName, sidecarOverride, configEnc, permsJSON, version string
	appName, manifestJSON, installationKey                                 string
	localPID                                                               int
	private                                                                bool
}

func (s *Server) readRunningInstalls(ctx context.Context) ([]storedRunningInstall, error) {
	ctx, cancel := s.store.databaseContext(ctx)
	defer cancel()
	rows, err := s.store.dbQueryContext(ctx,
		`SELECT i.id, i.app_id, COALESCE(i.project_id, ''), i.service_name,
			COALESCE(i.sidecar_url_override, ''),
			COALESCE(i.config_encrypted, ''),
			CASE WHEN i.runtime_managed=0 THEN COALESCE(i.local_pid, 0) ELSE 0 END,
			i.permissions_json, i.version, a.name,
			COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json),i.installation_key,i.private
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.status = 'running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []storedRunningInstall{}
	for rows.Next() {
		var record storedRunningInstall
		if err := rows.Scan(&record.id, &record.appID, &record.projectID, &record.serviceName, &record.sidecarOverride,
			&record.configEnc, &record.localPID, &record.permsJSON, &record.version, &record.appName, &record.manifestJSON, &record.installationKey, &record.private); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}
