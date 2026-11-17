package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var installationKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
var legacyInstallUnique = regexp.MustCompile(`(?i)UNIQUE\s*\(\s*app_id\s*,\s*project_id\s*\)`)

// Rebuild only the installation table, preserving IDs, all current columns,
// indexes, triggers and incoming foreign keys. Run before starting sidecars.
func (s *Store) migrateAppInstallations() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var schema string
	if err = tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='app_installs'`).Scan(&schema); err != nil {
		return err
	}
	if legacyInstallUnique.MatchString(schema) {
		var definitions []string
		rows, e := tx.Query(`SELECT sql FROM sqlite_master WHERE tbl_name='app_installs' AND type IN ('index','trigger') AND sql IS NOT NULL`)
		if e != nil {
			return e
		}
		for rows.Next() {
			var ddl string
			if e = rows.Scan(&ddl); e != nil {
				rows.Close()
				return e
			}
			definitions = append(definitions, ddl)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		// CREATE from the live schema keeps columns introduced by earlier migrations.
		next := legacyInstallUnique.ReplaceAllString(schema, `installation_key TEXT NOT NULL DEFAULT 'default', installation_name TEXT NOT NULL DEFAULT 'Default', is_default INTEGER NOT NULL DEFAULT 0 CHECK(is_default IN (0,1)), UNIQUE(app_id, project_id, installation_key)`)
		next = strings.Replace(next, "app_installs", "app_installs_named", 1)
		if _, err = tx.Exec(next); err != nil {
			return fmt.Errorf("create named installations: %w", err)
		}
		var sequence int64
		if err = tx.QueryRow(`SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='app_installs'),0)`).Scan(&sequence); err != nil {
			return err
		}
		columns, e := tx.Query(`PRAGMA table_info(app_installs)`)
		if e != nil {
			return e
		}
		var names []string
		for columns.Next() {
			var cid, nn, pk int
			var name, typ string
			var def any
			if e = columns.Scan(&cid, &name, &typ, &nn, &def, &pk); e != nil {
				columns.Close()
				return e
			}
			names = append(names, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`)
		}
		e = columns.Err()
		columns.Close()
		if e != nil {
			return e
		}
		list := strings.Join(names, ",")
		if _, err = tx.Exec(`INSERT INTO app_installs_named (` + list + `,is_default) SELECT ` + list + `,1 FROM app_installs`); err != nil {
			return err
		}
		if _, err = tx.Exec(`DROP TABLE app_installs`); err != nil {
			return err
		}
		if _, err = tx.Exec(`ALTER TABLE app_installs_named RENAME TO app_installs`); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE sqlite_sequence SET seq=MAX(seq,?) WHERE name='app_installs'`, sequence); err != nil {
			return err
		}
		for _, ddl := range definitions {
			if _, err = tx.Exec(ddl); err != nil {
				return fmt.Errorf("restore installation schema: %w", err)
			}
		}
	}
	for _, ddl := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS app_install_default ON app_installs(app_id,project_id) WHERE is_default=1`,
		`CREATE TRIGGER IF NOT EXISTS app_install_first_default AFTER INSERT ON app_installs WHEN NOT EXISTS(SELECT 1 FROM app_installs i WHERE i.app_id=NEW.app_id AND i.project_id=NEW.project_id AND i.id!=NEW.id) BEGIN UPDATE app_installs SET is_default=1 WHERE id=NEW.id; END`,
	} {
		if _, err = tx.Exec(ddl); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type appInstallation struct {
	Key     string `json:"installation_key"`
	Name    string `json:"installation_name"`
	Default bool   `json:"is_default"`
}

func (s *Server) appInstallation(id int64) (appInstallation, error) {
	var out appInstallation
	err := s.store.db.QueryRow(`SELECT installation_key,installation_name,is_default FROM app_installs WHERE id=?`, id).Scan(&out.Key, &out.Name, &out.Default)
	return out, err
}

func (s *Server) handleAppInstallation(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method == http.MethodGet {
		v, err := s.appInstallation(id)
		if err != nil {
			http.Error(w, "installation not found", 404)
			return
		}
		writeJSON(w, v)
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "GET or PATCH only", 405)
		return
	}
	if s.isOwnedComponent(id) {
		http.Error(w, "manage owned components through their owner", 409)
		return
	}
	var body struct {
		Name    *string `json:"installation_name"`
		Default *bool   `json:"is_default"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if body.Name != nil {
		v := strings.TrimSpace(*body.Name)
		if v == "" || len(v) > 100 || strings.ContainsAny(v, "\r\n\x00") {
			http.Error(w, "installation name must contain 1 to 100 characters", 400)
			return
		}
		body.Name = &v
	}
	if body.Default != nil && !*body.Default {
		http.Error(w, "select another installation as default instead", 400)
		return
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		http.Error(w, "installation unavailable", 503)
		return
	}
	defer tx.Rollback()
	var appID int64
	var project, status string
	if err = tx.QueryRow(`SELECT app_id,project_id,status FROM app_installs WHERE id=?`, id).Scan(&appID, &project, &status); err != nil {
		http.Error(w, "installation not found", 404)
		return
	}
	if body.Default != nil {
		if status != "running" || s.installedApps.Get(id) == nil {
			http.Error(w, "default installation must be running and ready", 409)
			return
		}
		if _, err = tx.Exec(`UPDATE app_installs SET is_default=0 WHERE app_id=? AND project_id=?`, appID, project); err != nil {
			http.Error(w, "update default failed", 500)
			return
		}
		if _, err = tx.Exec(`UPDATE app_installs SET is_default=1 WHERE id=?`, id); err != nil {
			http.Error(w, "update default failed", 500)
			return
		}
	}
	if body.Name != nil {
		if _, err = tx.Exec(`UPDATE app_installs SET installation_name=? WHERE id=?`, *body.Name, id); err != nil {
			http.Error(w, "update name failed", 500)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "save installation failed", 500)
		return
	}
	s.LoadInstalledApps()
	s.RemountStaticApps()
	v, err := s.appInstallation(id)
	if err != nil {
		http.Error(w, "installation unavailable", 503)
		return
	}
	writeJSON(w, v)
}

// Additional installations get a permanent MCP namespace. Changing the routing
// default or display name never changes existing agent tools.
func installationMCPName(app, key string, id int64) string {
	if key == "" || key == "default" {
		return app
	}
	return fmt.Sprintf("%s_install_%d", app, id)
}

// Load defaults from every status. A stopped project default must not route to
// a candidate or a global install. A zero records a scope with no default.
func (s *Server) loadInstallationDefaults() (map[appInstallScope]int64, error) {
	rows, err := s.store.db.Query(`SELECT a.name,COALESCE(i.project_id,''),MAX(CASE WHEN i.is_default=1 THEN i.id ELSE 0 END) FROM app_installs i JOIN apps a ON a.id=i.app_id GROUP BY a.name,i.project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[appInstallScope]int64{}
	for rows.Next() {
		var key appInstallScope
		var id int64
		if err = rows.Scan(&key.name, &key.project, &id); err != nil {
			return nil, err
		}
		out[key] = id
	}
	return out, rows.Err()
}

func appBindingAllowsInstallation(caller *appInstallMetadata, app string, id int64) bool {
	if caller.manifestErr != nil {
		return false
	}
	for _, dep := range caller.manifest.Requires.Apps {
		if dep.Name == app && appBindingContains(caller.bindings[dep.Name], id) {
			return true
		}
	}
	for _, dep := range caller.manifest.Requires.Integrations {
		if dep.Kind == "app" && appBindingContains(caller.bindings[dep.Role], id) {
			return true
		}
	}
	return false
}
