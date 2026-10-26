package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"sync"
)

// Serialize retry receipts across the install handler, which also resolves
// dependencies. The receipt and installation are committed together; a crash
// before queueing resumes the same installation and exact stored release.
var runtimeConsoleInstallMu sync.Mutex

func runtimeConsoleInstallOperationKey(key string) string {
	if key == "" {
		key = generateToken(12)
	}
	return "console-install-" + key
}
func (s *Server) insertRuntimeConsoleInstall(user int64, key, fingerprint, query string, args ...any) (sql.Result, error) {
	return s.insertManifestInstall(user, key, fingerprint, "", nil, query, args...)
}
func (s *Server) insertManifestInstall(user int64, key, fingerprint, version string, components appComponentDeclarations, query string, args ...any) (sql.Result, error) {
	tx, err := s.store.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(query, args...)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err = saveAppComponentDeclarations(tx, id, version, components); err != nil {
		return nil, err
	}
	if key != "" {
		if _, err = tx.Exec(`INSERT INTO app_runtime_install_receipts VALUES(?,?,?,?)`, user, key, fingerprint, id); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}
func (s *Server) replayRuntimeConsoleInstall(w http.ResponseWriter, r *http.Request, user int64, key, fingerprint string, p runtimePlacement) bool {
	var previous string
	var id int64
	err := s.store.db.QueryRow(`SELECT fingerprint,install_id FROM app_runtime_install_receipts WHERE user_id=? AND request_key=?`, user, key).Scan(&previous, &id)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		http.Error(w, "install receipt unavailable", 503)
		return true
	}
	if previous != fingerprint {
		http.Error(w, "idempotency key reused with a different install request", 409)
		return true
	}
	var appID int64
	if s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, id).Scan(&appID) != nil {
		http.Error(w, "the original installation no longer exists", 409)
		return true
	}
	m := s.appRuntimeManager()
	var opID string
	err = s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=? AND idempotency_key=?`, id, runtimeConsoleInstallOperationKey(key)).Scan(&opID)
	var op runtimeOperation
	if err == nil {
		op, err = m.operation(opID)
	} else if err == sql.ErrNoRows {
		manifest, e := installManifest(s, id)
		if e != nil {
			err = e
		} else {
			op, err = m.begin(id, "deploy", runtimeConsoleInstallOperationKey(key), p, manifest)
		}
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("installation %d exists; deployment could not resume", id), 409)
		return true
	}
	writeJSONStatus(w, 202, map[string]any{"install_id": id, "app_id": appID, "operation": op})
	return true
}
