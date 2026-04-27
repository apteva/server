package main

// Dependency cascade for app installs.
//
// When an app's manifest declares `requires.apps`, the install handler
// walks the dependency graph and installs every missing app first
// (topological order, deps before dependents). Already-installed apps
// are skipped. Cycles are detected and rejected.
//
// Resolution: each dep is named (manifest.name); the registry tells
// us where to fetch its apteva.yaml. The cascade fetches the
// configured registry once, builds a name → manifest_url map, and
// uses that to resolve every dep recursively.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	sdk "github.com/apteva/app-sdk"
)

// installDependencies installs everything `manifest.Requires.Apps`
// asks for, recursively. Required deps that fail abort the cascade
// with an error; optional deps that fail log and continue. Returns
// nil if every required dep is installed (or was already).
func (s *Server) installDependencies(userID int64, manifest *sdk.Manifest, projectID string) error {
	if len(manifest.Requires.Apps) == 0 {
		return nil
	}
	registryName2URL, err := s.loadRegistryNameMap()
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	visiting := map[string]bool{} // cycle detection
	visited := map[string]bool{}  // already-resolved (installed or in this run)
	return s.installDepsRecursive(userID, projectID, manifest.Requires.Apps, registryName2URL, visiting, visited)
}

func (s *Server) installDepsRecursive(
	userID int64,
	projectID string,
	deps []sdk.RequiredAppRef,
	registryName2URL map[string]string,
	visiting, visited map[string]bool,
) error {
	for _, dep := range deps {
		key := normalizeAppName(dep.Name)
		if visited[key] {
			continue
		}
		if visiting[key] {
			return fmt.Errorf("dependency cycle detected involving %q", dep.Name)
		}

		// Already installed (or built-in)? No-op.
		if s.isAppInstalled(dep.Name, projectID) {
			visited[key] = true
			continue
		}

		// Resolve the dep's manifest URL via the registry.
		manifestURL := registryName2URL[key]
		if manifestURL == "" {
			if dep.Optional {
				log.Printf("[APPS-DEP] optional dep %q not in registry — skipping", dep.Name)
				visited[key] = true
				continue
			}
			return fmt.Errorf("required dep %q not found in registry", dep.Name)
		}

		// Fetch + parse the dep's manifest.
		depManifest, err := s.fetchAndCacheManifest(manifestURL)
		if err != nil {
			if dep.Optional {
				log.Printf("[APPS-DEP] optional dep %q manifest fetch failed: %v", dep.Name, err)
				visited[key] = true
				continue
			}
			return fmt.Errorf("fetch dep %q manifest: %w", dep.Name, err)
		}

		// Recurse into the dep's own deps before installing it
		// (topo order — leaves first).
		if len(depManifest.Requires.Apps) > 0 {
			visiting[key] = true
			if err := s.installDepsRecursive(userID, projectID, depManifest.Requires.Apps, registryName2URL, visiting, visited); err != nil {
				if dep.Optional {
					log.Printf("[APPS-DEP] optional dep %q sub-deps failed: %v", dep.Name, err)
					visiting[key] = false
					visited[key] = true
					continue
				}
				return err
			}
			visiting[key] = false
		}

		// Install the dep itself.
		if err := s.installAppFromManifest(userID, depManifest, projectID); err != nil {
			if dep.Optional {
				log.Printf("[APPS-DEP] optional dep %q install failed: %v", dep.Name, err)
				visited[key] = true
				continue
			}
			return fmt.Errorf("install dep %q: %w", dep.Name, err)
		}
		log.Printf("[APPS-DEP] installed %q (required by parent)", dep.Name)
		visited[key] = true
	}
	return nil
}

// isAppInstalled reports whether an app with the given name is
// already installed (an app_installs row exists for this project or
// globally) OR is a built-in framework app.
func (s *Server) isAppInstalled(name, projectID string) bool {
	target := normalizeAppName(name)
	if s.apps != nil {
		for _, a := range s.apps.Loaded() {
			m := a.Manifest()
			if normalizeAppName(m.Slug) == target || normalizeAppName(m.Name) == target {
				return true
			}
		}
	}
	rows, err := s.store.db.Query(
		`SELECT a.name FROM apps a JOIN app_installs i ON i.app_id = a.id
		 WHERE i.project_id = '' OR i.project_id = ?`, projectID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil && normalizeAppName(n) == target {
			return true
		}
	}
	return false
}

// loadRegistryNameMap fetches the configured registry once and
// returns a name (normalized) → manifest_url map. Goes through the
// existing registry URL resolution (env override or default github
// raw) so behaviour matches handleMarketplace.
func (s *Server) loadRegistryNameMap() (map[string]string, error) {
	url := getRegistryURLFromEnv()
	if url == "" {
		url = defaultRegistryURL
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("registry %s returned %d", url, resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	var reg struct {
		Apps []RegistryEntry `json:"apps"`
	}
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range reg.Apps {
		if e.ManifestURL == "" {
			continue
		}
		out[normalizeAppName(e.Name)] = e.ManifestURL
	}
	return out, nil
}

// dependentsBlockingUninstall returns the human-facing names of every
// running install whose manifest hard-requires the install referenced
// by `installID`. Empty list (and nil err) means the uninstall is
// safe. Optional dependents don't block — they degrade silently when
// the dep goes away.
func (s *Server) dependentsBlockingUninstall(installID int64) ([]string, error) {
	// First, find the name of the app we're trying to uninstall.
	var targetName string
	err := s.store.db.QueryRow(
		`SELECT a.name FROM apps a JOIN app_installs i ON i.app_id = a.id WHERE i.id = ?`,
		installID,
	).Scan(&targetName)
	if err != nil {
		return nil, err
	}
	target := normalizeAppName(targetName)

	// Walk every other running install's manifest looking for a hard
	// requires.apps entry that names the target.
	rows, err := s.store.db.Query(
		`SELECT a.name, a.manifest_json FROM apps a JOIN app_installs i ON i.app_id = a.id
		 WHERE i.id != ? AND i.status IN ('running', 'pending')`,
		installID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blockers []string
	for rows.Next() {
		var name, manifestJSON string
		if err := rows.Scan(&name, &manifestJSON); err != nil {
			continue
		}
		var m sdk.Manifest
		if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
			continue
		}
		for _, dep := range m.Requires.Apps {
			if dep.Optional {
				continue
			}
			if normalizeAppName(dep.Name) == target {
				blockers = append(blockers, name)
				break
			}
		}
	}
	return blockers, nil
}

// writeJSONStatus is writeJSON + an explicit HTTP status. Used by
// the uninstall handler to return 409 with a structured payload
// instead of a plain text error so the dashboard can pretty-print
// the dependents list.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// installAppFromManifest creates the apps + app_installs rows for a
// dep and dispatches to the right local-install path. Mirrors the
// happy-path branches of handleInstallApp without the HTTP wrapping.
// Used only by the dependency cascade — operator-driven installs
// still go through handleInstallApp for full validation + responses.
func (s *Server) installAppFromManifest(userID int64, manifest *sdk.Manifest, projectID string) error {
	manifestJSON, _ := json.Marshal(manifest)

	var appID int64
	err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, manifest.Name).Scan(&appID)
	if err != nil {
		res, ierr := s.store.db.Exec(
			`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'registry', '', '', ?)`,
			manifest.Name, string(manifestJSON))
		if ierr != nil {
			return fmt.Errorf("create app row: %w", ierr)
		}
		appID, _ = res.LastInsertId()
	} else {
		s.store.db.Exec(`UPDATE apps SET manifest_json = ? WHERE id = ?`,
			string(manifestJSON), appID)
	}

	permsJSON, _ := json.Marshal(manifest.Requires.Permissions)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, config_encrypted, status, upgrade_policy, version, permissions_json, installed_by)
		 VALUES (?, ?, '', 'pending', 'manual', ?, ?, ?)`,
		appID, projectID, manifest.Version, string(permsJSON), userID)
	if err != nil {
		return fmt.Errorf("create install row: %w", err)
	}
	installID, _ := res.LastInsertId()

	// Static and source paths both go through installLocally —
	// installLocally's kind=static branch handles UI-only apps
	// inline; the source path is delegated for kind=source. Service
	// kind goes via the orchestrator and is rare locally; we let
	// installLocally reject it gracefully there.
	switch manifest.Runtime.Kind {
	case "source":
		return s.installFromSource(installID, manifest, projectID, nil)
	default:
		return s.installLocally(installID, manifest, projectID, nil)
	}
}
