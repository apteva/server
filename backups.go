package main

// Platform snapshot endpoint.
//
// GET /api/platform/snapshot streams a tar.gz containing every SQLite
// database the platform owns, captured via SQLite VACUUM INTO so each
// dump is internally consistent without stopping any sidecar. Layout:
//
//   manifest.json                       — versions + per-install metadata
//   server/apteva-server.db             — the platform DB
//   apps/<install_id>-<name>/app.db     — one entry per running install
//
// Admin-only. The intent is that a self-hoster (or an installable
// "backup app") periodically pulls this endpoint and ships the bytes
// to S3 / B2 / etc. Restore lives in a follow-up — designing the
// snapshot first lets us validate the file layout before committing
// to a restore wire format.

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handlePlatformSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.isAdmin(getUserID(r)) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}

	tmpDir, err := os.MkdirTemp("", "apteva-snapshot-*")
	if err != nil {
		http.Error(w, "tempdir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	// Build the file list before writing any response bytes. Failing
	// here surfaces as a clean 500 rather than a half-written tar.
	type entry struct {
		archivePath string
		srcFile     string
	}
	entries := []entry{}

	serverDump := filepath.Join(tmpDir, "apteva-server.db")
	if err := vacuumIntoFromHandle(s.store.db, serverDump); err != nil {
		http.Error(w, "snapshot server db: "+err.Error(), http.StatusInternalServerError)
		return
	}
	entries = append(entries, entry{
		archivePath: "server/apteva-server.db",
		srcFile:     serverDump,
	})

	type installMeta struct {
		InstallID  int64  `json:"install_id"`
		Name       string `json:"name"`
		Version    string `json:"version,omitempty"`
		ProjectID  string `json:"project_id,omitempty"`
		DBIncluded bool   `json:"db_included"`
		DBPath     string `json:"db_path_in_archive,omitempty"`
		Note       string `json:"note,omitempty"`
	}
	installs := []installMeta{}

	if s.installedApps != nil {
		for _, app := range s.installedApps.List() {
			meta := installMeta{
				InstallID: app.InstallID,
				Name:      app.AppName,
				Version:   app.Manifest.Version,
				ProjectID: app.ProjectID,
			}
			// Static apps have no sidecar process and no app.db.
			if app.SidecarURL == "" {
				meta.Note = "static app — no database"
				installs = append(installs, meta)
				continue
			}
			if s.localApps == nil {
				meta.Note = "local supervisor not configured"
				installs = append(installs, meta)
				continue
			}
			srcDB := filepath.Join(s.localApps.cacheDir, app.AppName, "data",
				fmt.Sprintf("%d", app.InstallID), "app.db")
			if _, statErr := os.Stat(srcDB); statErr != nil {
				// Sidecar may be running with the schema migrated but no
				// data on disk yet, or the file moved. Record the gap
				// rather than failing the whole snapshot — partial is
				// more useful than nothing.
				meta.Note = "no app.db on disk"
				installs = append(installs, meta)
				continue
			}
			dump := filepath.Join(tmpDir, fmt.Sprintf("install-%d.db", app.InstallID))
			if vErr := vacuumIntoFromPath(srcDB, dump); vErr != nil {
				meta.Note = "vacuum failed: " + vErr.Error()
				installs = append(installs, meta)
				continue
			}
			archivePath := fmt.Sprintf("apps/%d-%s/app.db", app.InstallID, safeArchiveSegment(app.AppName))
			entries = append(entries, entry{archivePath: archivePath, srcFile: dump})
			meta.DBIncluded = true
			meta.DBPath = archivePath
			installs = append(installs, meta)
		}
	}

	manifest := map[string]any{
		"format_version": 1,
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"server_version": versionInfo(),
		"installs":       installs,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		http.Error(w, "marshal manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()
	fname := "apteva-snapshot-" + now.Format("20060102-150405") + ".tar.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	if err := writeTarBytes(tw, "manifest.json", manifestBytes, now); err != nil {
		log.Printf("[SNAPSHOT] write manifest: %v", err)
		_ = tw.Close()
		_ = gz.Close()
		return
	}
	for _, e := range entries {
		if err := writeTarFile(tw, e.archivePath, e.srcFile, now); err != nil {
			log.Printf("[SNAPSHOT] write %s: %v", e.archivePath, err)
			_ = tw.Close()
			_ = gz.Close()
			return
		}
	}
	if err := tw.Close(); err != nil {
		log.Printf("[SNAPSHOT] close tar: %v", err)
		_ = gz.Close()
		return
	}
	if err := gz.Close(); err != nil {
		log.Printf("[SNAPSHOT] close gzip: %v", err)
	}
}

// vacuumIntoFromHandle dumps the open *sql.DB into dst. SQLite's
// VACUUM INTO acquires only a SHARED read lock on the source so other
// readers and writers continue uninterrupted; the resulting file is
// internally consistent.
func vacuumIntoFromHandle(db *sql.DB, dst string) error {
	// VACUUM INTO requires the target file to not exist.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	// SQLite string literal: wrap in single quotes, escape embedded ones.
	quoted := "'" + strings.ReplaceAll(dst, "'", "''") + "'"
	_, err := db.Exec("VACUUM INTO " + quoted)
	return err
}

// vacuumIntoFromPath opens a SQLite file the server doesn't already
// hold a handle to (e.g. a sidecar's app.db) and vacuums it. We open
// in read-only mode so we can never accidentally write to a sidecar's
// live database.
func vacuumIntoFromPath(src, dst string) error {
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	return vacuumIntoFromHandle(db, dst)
}

func writeTarBytes(tw *tar.Writer, name string, content []byte, mod time.Time) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(content)),
		ModTime: mod,
	}); err != nil {
		return err
	}
	_, err := tw.Write(content)
	return err
}

func writeTarFile(tw *tar.Writer, archivePath, srcFile string, fallbackMod time.Time) error {
	fi, err := os.Stat(srcFile)
	if err != nil {
		return err
	}
	mod := fi.ModTime()
	if mod.IsZero() {
		mod = fallbackMod
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    archivePath,
		Mode:    0o644,
		Size:    fi.Size(),
		ModTime: mod,
	}); err != nil {
		return err
	}
	f, err := os.Open(srcFile)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// safeArchiveSegment scrubs path-separators and traversal sequences so
// an attacker-controlled app name can't escape the apps/ prefix.
func safeArchiveSegment(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, `\`, "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" {
		s = "unnamed"
	}
	return s
}

// ─── Restore ───────────────────────────────────────────────────────
//
// POST /api/platform/restore accepts a tar.gz produced by the snapshot
// endpoint. App DBs are restored live — the supervisor stops the
// sidecar, files are swapped, sidecars are re-spawned at the end via
// the existing ResumeLocalInstalls path.
//
// The platform DB itself can't be replaced under a running server, so
// it's *staged*: the new bytes land at <dbPath>.restored with a marker
// file, and applyPendingRestore swaps it in on the next boot.
// The handler returns 200 with a per-entry status report; the body
// makes restart_required explicit when the server DB was included.
//
// Safety:
//   - Admin-only.
//   - Requires X-Confirm-Restore: yes header to make accidental clicks
//     impossible.
//   - Tar entry names are routing hints only; we never write to a path
//     constructed from them. install_id is parsed and the destination
//     path is rebuilt from <localApps.cacheDir>.

const (
	restorePendingSuffix = ".restored"
	restoreMarkerSuffix  = ".restored.marker"
	restoreBackupPrefix  = ".prerestore-"
)

type restoreInstallReport struct {
	InstallID  int64  `json:"install_id"`
	ArchivePath string `json:"archive_path"`
	Status     string `json:"status"` // "applied" | "skipped" | "error"
	Note       string `json:"note,omitempty"`
}

type restoreReport struct {
	FormatVersion   int                    `json:"format_version_seen"`
	ServerDB        string                 `json:"server_db"`        // "staged" | "skipped"
	RestartRequired bool                   `json:"restart_required"`
	Installs        []restoreInstallReport `json:"installs"`
}

func (s *Server) handlePlatformRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.isAdmin(getUserID(r)) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if r.Header.Get("X-Confirm-Restore") != "yes" {
		http.Error(w, "missing X-Confirm-Restore: yes — restore is destructive, confirmation required", http.StatusBadRequest)
		return
	}

	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		http.Error(w, "body is not gzip: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	// Drain the tar into a temp dir so we can validate the manifest before
	// touching anything live, and so we can iterate entries in any order.
	stage, err := os.MkdirTemp("", "apteva-restore-*")
	if err != nil {
		http.Error(w, "tempdir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(stage)

	staged := map[string]string{} // archivePath → on-disk path inside stage
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "tar read: "+err.Error(), http.StatusBadRequest)
			return
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// Stage under a synthesised, sanitised path. We never use the
		// archive's own name as a filesystem destination — it's only a
		// routing hint that we re-validate below.
		dst := filepath.Join(stage, fmt.Sprintf("entry-%d.bin", len(staged)))
		f, err := os.Create(dst)
		if err != nil {
			http.Error(w, "stage write: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			http.Error(w, "stage copy: "+err.Error(), http.StatusInternalServerError)
			return
		}
		f.Close()
		staged[h.Name] = dst
	}

	manifestPath, ok := staged["manifest.json"]
	if !ok {
		http.Error(w, "snapshot missing manifest.json", http.StatusBadRequest)
		return
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		http.Error(w, "read manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var manifest struct {
		FormatVersion int `json:"format_version"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		http.Error(w, "manifest json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if manifest.FormatVersion != 1 {
		http.Error(w, fmt.Sprintf("unsupported format_version %d (this server understands 1)", manifest.FormatVersion), http.StatusBadRequest)
		return
	}

	report := restoreReport{FormatVersion: manifest.FormatVersion, ServerDB: "skipped"}

	// 1) Server DB: stage for next boot.
	if src, ok := staged["server/apteva-server.db"]; ok {
		if err := stageServerDBRestore(s.dbPath, src); err != nil {
			http.Error(w, "stage server DB: "+err.Error(), http.StatusInternalServerError)
			return
		}
		report.ServerDB = "staged"
		report.RestartRequired = true
	}

	// 2) App DBs: stop sidecar → swap file → mark for resume at the end.
	for archivePath, srcFile := range staged {
		if !strings.HasPrefix(archivePath, "apps/") || !strings.HasSuffix(archivePath, "/app.db") {
			continue
		}
		entry := restoreInstallReport{ArchivePath: archivePath}
		installID, err := parseInstallIDFromArchivePath(archivePath)
		if err != nil {
			entry.Status = "error"
			entry.Note = err.Error()
			report.Installs = append(report.Installs, entry)
			continue
		}
		entry.InstallID = installID
		appName, err := s.lookupAppNameForInstall(installID)
		if err != nil {
			entry.Status = "error"
			entry.Note = err.Error()
			report.Installs = append(report.Installs, entry)
			continue
		}
		if s.localApps == nil {
			entry.Status = "error"
			entry.Note = "local supervisor not configured"
			report.Installs = append(report.Installs, entry)
			continue
		}
		dbPath := filepath.Join(s.localApps.cacheDir, appName, "data",
			fmt.Sprintf("%d", installID), "app.db")
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			entry.Status = "error"
			entry.Note = "mkdir: " + err.Error()
			report.Installs = append(report.Installs, entry)
			continue
		}
		// Stop the sidecar so we can replace the file safely. Stop is a
		// no-op when the install isn't currently in the supervisor map.
		_ = s.localApps.Stop(installID)
		if err := atomicReplaceWithBackup(dbPath, srcFile); err != nil {
			entry.Status = "error"
			entry.Note = "swap: " + err.Error()
			report.Installs = append(report.Installs, entry)
			continue
		}
		// The WAL/SHM sidecar files belong to the OLD database — leaving
		// them in place would corrupt reads of the restored bytes.
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
		entry.Status = "applied"
		report.Installs = append(report.Installs, entry)
	}

	// 3) Re-spawn any sidecars we stopped. ResumeLocalInstalls is
	// idempotent — it skips installs whose pid is still alive — so it's
	// safe to call even when nothing was restored.
	if s.installedApps != nil && s.localApps != nil {
		s.ResumeLocalInstalls()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// stageServerDBRestore writes the new platform DB next to the live one
// as <dbPath>.restored and drops a marker file. applyPendingRestore
// (called at boot) does the actual swap.
func stageServerDBRestore(dbPath, srcFile string) error {
	if dbPath == "" {
		return errors.New("server has no dbPath set — restore endpoint unavailable")
	}
	pending := dbPath + restorePendingSuffix
	marker := dbPath + restoreMarkerSuffix
	if err := copyFile(srcFile, pending); err != nil {
		return err
	}
	// Marker is what applyPendingRestore looks for. Empty file is fine —
	// a future format can put metadata in it.
	if err := os.WriteFile(marker, []byte("apteva-restore-pending\n"), 0o644); err != nil {
		_ = os.Remove(pending)
		return err
	}
	return nil
}

// applyPendingRestore is called at server boot before NewStore. When a
// staged restore is present, it backs up the current DB and renames
// the staged file into place, then clears the marker. Stale -wal/-shm
// from the old DB are removed because they describe a database that
// no longer exists at this path.
func applyPendingRestore(dbPath string) {
	marker := dbPath + restoreMarkerSuffix
	pending := dbPath + restorePendingSuffix
	if _, err := os.Stat(marker); err != nil {
		return
	}
	if _, err := os.Stat(pending); err != nil {
		log.Printf("[BOOT] restore marker present at %s but no .restored file at %s — clearing marker", marker, pending)
		_ = os.Remove(marker)
		return
	}
	backup := dbPath + restoreBackupPrefix + time.Now().UTC().Format("20060102-150405")
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Rename(dbPath, backup); err != nil {
			log.Printf("[BOOT] restore aborted: cannot back up current DB to %s: %v", backup, err)
			return
		}
	}
	if err := os.Rename(pending, dbPath); err != nil {
		log.Printf("[BOOT] restore failed: cannot move staged DB into place: %v — rolling back", err)
		_ = os.Rename(backup, dbPath)
		return
	}
	_ = os.Remove(marker)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	log.Printf("[BOOT] applied staged platform DB restore (previous DB saved as %s)", backup)
}

// parseInstallIDFromArchivePath extracts the install id from
// "apps/<id>-<name>/app.db". Names are user-controlled, so we never
// use them as filesystem paths — only the id.
func parseInstallIDFromArchivePath(p string) (int64, error) {
	rest := strings.TrimPrefix(p, "apps/")
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 {
		return 0, fmt.Errorf("archive path %q does not match apps/<id>-<name>/app.db", p)
	}
	id, err := strconv.ParseInt(rest[:dash], 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("archive path %q has non-numeric install id", p)
	}
	return id, nil
}

// lookupAppNameForInstall reads the app row's name for the given install
// id. Returns an error when the install isn't present in this server's
// DB — restore can't recreate installs that don't exist (the operator
// must install the app first, then restore its data).
func (s *Server) lookupAppNameForInstall(installID int64) (string, error) {
	var name string
	err := s.store.db.QueryRow(
		`SELECT a.name FROM app_installs i JOIN apps a ON a.id = i.app_id WHERE i.id = ?`,
		installID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("install id %d not present on this server — install the app before restoring its data", installID)
	}
	if err != nil {
		return "", err
	}
	return name, nil
}

// atomicReplaceWithBackup renames the existing file out of the way as
// <dst><restoreBackupPrefix><ts>, then renames the source into place.
// Both operations are POSIX-atomic on the same filesystem. If a backup
// can't be made, no swap happens.
func atomicReplaceWithBackup(dst, src string) error {
	if _, err := os.Stat(dst); err == nil {
		backup := dst + restoreBackupPrefix + time.Now().UTC().Format("20060102-150405.000000000")
		if err := os.Rename(dst, backup); err != nil {
			return fmt.Errorf("backup %s: %w", dst, err)
		}
	}
	// Try a rename first (cheap, atomic when on the same fs); fall back
	// to copy so cross-fs stage dirs (e.g. /tmp on tmpfs vs the apps
	// volume) still work.
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
