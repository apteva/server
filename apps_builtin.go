package main

// Built-in app auto-registration.
//
// Apps shipped inside the apteva-server image (today: `simple`) need
// to appear in the Apps tab of the dashboard so the operator can
// install / configure / uninstall them like any third-party app, and
// to be served the moment the server boots. This file walks
// $BUILTIN_APPS_DIR at startup, parses each apteva.yaml it finds, and
// upserts a row in the `apps` table tagged source='builtin'. It does
// NOT auto-create an install row — that's still an explicit operator
// action from the dashboard. The result: built-in apps show up in
// the catalog as available, and clicking "Install" goes through the
// same handleInstallApp path everything else uses.

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	sdk "github.com/apteva/app-sdk"
)

// RegisterBuiltinApps scans BUILTIN_APPS_DIR (default
// "/opt/apteva/apps") for apteva.yaml manifests and upserts each one
// into the `apps` table. Idempotent across restarts. Built-in apps
// are flagged with source='builtin' so the Apps tab can render them
// distinctly and prevent the Update button from chasing a remote.
//
// Layout convention:
//   /opt/apteva/apps/
//     simple/
//       apteva.yaml          ← parsed
//       dist/                ← already referenced by manifest.runtime.static_dir
//
// Manifests with malformed YAML are logged and skipped — never
// block boot.
func (s *Server) RegisterBuiltinApps() {
	dir := os.Getenv("BUILTIN_APPS_DIR")
	if dir == "" {
		dir = "/opt/apteva/apps"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing dir is fine on dev machines that don't bake apps in.
		return
	}
	registered := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifestPath := filepath.Join(dir, e.Name(), "apteva.yaml")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		manifest, err := sdk.ParseManifest(raw)
		if err != nil {
			log.Printf("[APPS-BUILTIN] skip %s: bad manifest: %v", e.Name(), err)
			continue
		}
		// Built-in static apps ship their dist/ pre-baked alongside
		// apteva.yaml, so rewrite a relative static_dir to absolute
		// against the manifest's directory. This bypasses the source
		// clone the install path would otherwise run for relative
		// paths, and keeps the marketplace install flow honest about
		// "already on disk, no fetch needed".
		if manifest.Runtime.Kind == "static" && manifest.Runtime.StaticDir != "" {
			d := manifest.Runtime.StaticDir
			if !filepath.IsAbs(d) {
				abs := filepath.Join(dir, e.Name(), d)
				if _, statErr := os.Stat(abs); statErr == nil {
					manifest.Runtime.StaticDir = abs
				}
			}
		}
		manifestJSON, _ := json.Marshal(manifest)
		var appID int64
		err = s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, manifest.Name).Scan(&appID)
		if err != nil {
			_, ierr := s.store.db.Exec(
				`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'builtin', '', '', ?)`,
				manifest.Name, string(manifestJSON))
			if ierr != nil {
				log.Printf("[APPS-BUILTIN] insert %s: %v", manifest.Name, ierr)
				continue
			}
			log.Printf("[APPS-BUILTIN] registered %s v%s (kind=%s)",
				manifest.Name, manifest.Version, manifest.Runtime.Kind)
		} else {
			// Refresh manifest_json on every boot so updates ship with
			// the image. Other columns (source, ref) stay frozen at
			// the original 'builtin' values.
			s.store.db.Exec(`UPDATE apps SET manifest_json = ? WHERE id = ?`,
				string(manifestJSON), appID)
		}
		registered++
	}
	if registered > 0 {
		log.Printf("[APPS-BUILTIN] registered %d built-in app(s) from %s", registered, dir)
	}
}
