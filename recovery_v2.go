package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

type recoveryRoot struct {
	Archive string `json:"archive"`
	Kind    string `json:"kind"`
	ID      int64  `json:"id"`
	Name    string `json:"name,omitempty"`
}
type recoveryFile struct {
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	SQLite bool   `json:"sqlite,omitempty"`
}
type recoveryManifest struct {
	FormatVersion  int                     `json:"format_version"`
	GeneratedAt    string                  `json:"generated_at"`
	ServerVersion  any                     `json:"server_version"`
	Roots          []recoveryRoot          `json:"roots"`
	Files          map[string]recoveryFile `json:"files"`
	KeyFingerprint string                  `json:"key_fingerprint"`
	KeySalt        string                  `json:"key_salt,omitempty"`
	WrappedKey     string                  `json:"wrapped_key,omitempty"`
	KeyPolicy      string                  `json:"key_policy"`
	SourceDataDir  string                  `json:"source_data_dir"`
	SourceCacheDir string                  `json:"source_cache_dir"`
}

var recoveryStageMu sync.Mutex

func keyFingerprint(key []byte) string { sum := sha256.Sum256(key); return hex.EncodeToString(sum[:]) }
func recoveryWrappingKey(passphrase, salt string) ([]byte, error) {
	decoded, err := hex.DecodeString(salt)
	if err != nil || len(decoded) != 16 {
		return nil, fmt.Errorf("invalid recovery salt")
	}
	return scrypt.Key([]byte(passphrase), decoded, 32768, 8, 1, 32)
}
func sqliteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var header [16]byte
	_, err = io.ReadFull(f, header[:])
	return err == nil && string(header[:]) == "SQLite format 3\x00"
}
func validateRecoveryDB(path string) error {
	if !sqliteFile(path) {
		return fmt.Errorf("invalid SQLite file")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err = db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity: %s", result)
	}
	return nil
}
func recoveryDigest(path string) (recoveryFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return recoveryFile{}, err
	}
	defer f.Close()
	h := sha256.New()
	info, err := f.Stat()
	if err != nil {
		return recoveryFile{}, err
	}
	size, err := io.Copy(h, f)
	return recoveryFile{Mode: uint32(0600 | info.Mode().Perm()&0111), SHA256: hex.EncodeToString(h.Sum(nil)), Size: size, SQLite: sqliteFile(path)}, err
}

// Materialize all bytes before writing response headers. SQLite databases are
// independently consistent online snapshots; file trees are captured while live.
// No runtime stream or event processing is paused by a backup.
func (s *Server) writePlatformSnapshot(w http.ResponseWriter, r *http.Request) {
	tmp, err := os.MkdirTemp("", "apteva-recovery-*")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.RemoveAll(tmp)
	files := map[string]string{}
	manifest := recoveryManifest{FormatVersion: 2, GeneratedAt: time.Now().UTC().Format(time.RFC3339), ServerVersion: versionInfo(), Files: map[string]recoveryFile{}, KeyFingerprint: keyFingerprint(s.secret), KeyPolicy: "original_key_required", SourceDataDir: s.dataDir}
	if s.localApps != nil {
		manifest.SourceCacheDir = s.localApps.cacheDir
	}
	if pass := r.Header.Get("X-Backup-Passphrase"); pass != "" {
		if len(pass) < 12 {
			http.Error(w, "backup passphrase must contain at least 12 characters", 400)
			return
		}
		salt := make([]byte, 16)
		if _, err = rand.Read(salt); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		manifest.KeySalt = hex.EncodeToString(salt)
		key, e := recoveryWrappingKey(pass, manifest.KeySalt)
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		manifest.WrappedKey, err = Encrypt(key, hex.EncodeToString(s.secret))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		manifest.KeyPolicy = "passphrase_wrapped"
	}
	fail := func(err error) { http.Error(w, "snapshot: "+err.Error(), 500) }
	dbPath := filepath.Join(tmp, "server.db")
	if err = vacuumIntoFromHandle(s.store.db, dbPath); err != nil {
		fail(err)
		return
	}
	files["server/apteva-server.db"] = dbPath
	inventory, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		fail(err)
		return
	}
	defer inventory.Close()
	type sourceRoot struct {
		recoveryRoot
		Path string
	}
	roots := []sourceRoot{}
	rows, err := inventory.Query("SELECT i.id,a.name FROM app_installs i JOIN apps a ON a.id=i.app_id ORDER BY i.id")
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var id int64
		var name string
		if err = rows.Scan(&id, &name); err != nil {
			rows.Close()
			fail(err)
			return
		}
		if filepath.Base(name) != name || name == "." || name == ".." {
			rows.Close()
			fail(fmt.Errorf("invalid app name"))
			return
		}
		if s.localApps == nil {
			continue
		}
		roots = append(roots, sourceRoot{recoveryRoot{fmt.Sprintf("apps/%d-%s", id, safeArchiveSegment(name)), "app", id, name}, filepath.Join(s.localApps.cacheDir, name, "data", strconv.FormatInt(id, 10))})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(err)
		return
	}
	rows, err = inventory.Query("SELECT id FROM agents ORDER BY id")
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			fail(err)
			return
		}
		if s.agents != nil {
			roots = append(roots, sourceRoot{recoveryRoot{fmt.Sprintf("agents/%d", id), "agent", id, ""}, filepath.Join(s.agents.dataDir, fmt.Sprintf("instance_%d", id))})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(err)
		return
	}
	rows, err = inventory.Query("SELECT id FROM mcp_servers WHERE source=? ORDER BY id", managedMCPSource)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			fail(err)
			return
		}
		roots = append(roots, sourceRoot{recoveryRoot{fmt.Sprintf("mcp-servers/%d/source", id), "mcp", id, ""}, s.managedMCPSourceDir(id)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(err)
		return
	}
	for _, root := range roots {
		manifest.Roots = append(manifest.Roots, root.recoveryRoot)
		if _, e := os.Lstat(root.Path); os.IsNotExist(e) {
			continue
		}
		err = filepath.WalkDir(root.Path, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink requires explicit backup policy: %s", path)
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("unsupported persistent file: %s", path)
			}
			if strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") {
				return nil
			}
			if err := r.Context().Err(); err != nil {
				return err
			}
			rel, e := filepath.Rel(root.Path, path)
			if e != nil {
				return e
			}
			dst := filepath.Join(tmp, fmt.Sprintf("file-%d", len(files)))
			if sqliteFile(path) {
				e = vacuumIntoFromPath(path, dst)
			} else {
				e = copyFile(path, dst)
			}
			if e != nil {
				return e
			}
			info, e := d.Info()
			if e != nil {
				return e
			}
			if e = os.Chmod(dst, 0600|info.Mode().Perm()&0111); e != nil {
				return e
			}
			files[root.Archive+"/"+filepath.ToSlash(rel)] = dst
			return nil
		})
		if err != nil {
			fail(err)
			return
		}
	}
	if len(files)+1 > maxRestoreArchiveEntryCount {
		fail(fmt.Errorf("snapshot exceeds recovery file-count limit (%d)", maxRestoreArchiveEntryCount))
		return
	}
	var expanded int64
	for name, path := range files {
		meta, e := recoveryDigest(path)
		if e != nil {
			fail(e)
			return
		}
		if meta.Size > maxRestoreEntryBytes || expanded > maxRestoreExpandedBytes-meta.Size {
			fail(fmt.Errorf("snapshot exceeds recovery size limit"))
			return
		}
		expanded += meta.Size
		manifest.Files[name] = meta
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fail(err)
		return
	}
	if int64(len(raw)) > maxRestoreManifestBytes || expanded > maxRestoreExpandedBytes-int64(len(raw)) {
		fail(fmt.Errorf("snapshot manifest exceeds recovery limit"))
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="apteva-snapshot-`+time.Now().UTC().Format("20060102-150405")+`.tar.gz"`)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	defer gz.Close()
	defer tw.Close()
	if err = writeTarBytes(tw, "manifest.json", raw, time.Now()); err != nil {
		return
	}
	for name, path := range files {
		if err = writeTarFile(tw, name, path, time.Now()); err != nil {
			return
		}
	}
}

type recoverySwap struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Backup      string `json:"backup"`
}
type recoveryPlan struct {
	Swaps    []recoverySwap `json:"swaps"`
	Database string         `json:"database"`
}

func durableJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".recovery-plan-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncRecoveryDir(filepath.Dir(path))
}
func syncRecoveryDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Server) stageRecoveryV2(staged map[string]string, raw []byte, passphrase string) error {
	recoveryStageMu.Lock()
	defer recoveryStageMu.Unlock()
	if s.dbPath == "" {
		return fmt.Errorf("server database path unavailable")
	}
	marker := s.dbPath + ".recovery.json"
	if _, err := os.Stat(marker); err == nil {
		return fmt.Errorf("a recovery is already staged; restart before another restore")
	}
	var m recoveryManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if len(m.Files)+1 != len(staged) {
		return fmt.Errorf("recovery inventory does not match archive")
	}
	for name, meta := range m.Files {
		path, ok := staged[name]
		if !ok {
			return fmt.Errorf("missing recovery file %s", name)
		}
		actual, err := recoveryDigest(path)
		if err != nil {
			return err
		}
		actual.Mode = meta.Mode
		if meta.Mode != uint32(0600|os.FileMode(meta.Mode)&0111) {
			return fmt.Errorf("invalid recovery permissions")
		}
		if actual != meta {
			return fmt.Errorf("recovery digest mismatch: %s", name)
		}
		if meta.SQLite {
			if err = validateRecoveryDB(path); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	dbSource, ok := staged["server/apteva-server.db"]
	if !ok {
		return fmt.Errorf("missing server database")
	}
	if err := validateRecoveryDB(dbSource); err != nil {
		return err
	}
	key := s.secret
	if keyFingerprint(key) != m.KeyFingerprint {
		if passphrase == "" || m.WrappedKey == "" {
			return fmt.Errorf("original encryption key or backup passphrase required")
		}
		wrap, err := recoveryWrappingKey(passphrase, m.KeySalt)
		if err != nil {
			return err
		}
		plain, err := Decrypt(wrap, m.WrappedKey)
		if err != nil {
			return fmt.Errorf("invalid backup passphrase")
		}
		key, err = hex.DecodeString(plain)
		if err != nil || len(key) != 32 || keyFingerprint(key) != m.KeyFingerprint {
			return fmt.Errorf("invalid recovery key")
		}
		if env := os.Getenv("SERVER_SECRET"); env != "" && env != hex.EncodeToString(key) {
			return fmt.Errorf("SERVER_SECRET must match the recovery key before activation")
		}
	}
	db, err := sql.Open("sqlite", "file:"+dbSource)
	if err != nil {
		return err
	}
	defer db.Close()
	plan := recoveryPlan{Database: s.dbPath}
	committed := false
	defer func() {
		if !committed {
			for _, swap := range plan.Swaps {
				os.RemoveAll(swap.Source)
			}
		}
	}()
	add := func(dst string, dir bool) (string, error) {
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return "", err
		}
		var tmp string
		var err error
		if dir {
			tmp, err = os.MkdirTemp(filepath.Dir(dst), ".recovery-data-*")
		} else {
			var f *os.File
			f, err = os.CreateTemp(filepath.Dir(dst), ".recovery-data-*")
			if err == nil {
				tmp = f.Name()
				err = f.Close()
			}
		}
		if err != nil {
			return "", err
		}
		plan.Swaps = append(plan.Swaps, recoverySwap{tmp, dst, dst + ".prerecovery-" + filepath.Base(tmp)})
		return tmp, nil
	}
	used := map[string]bool{"server/apteva-server.db": true}
	destinations := map[string]bool{}
	for _, root := range m.Roots {
		if root.ID <= 0 {
			return fmt.Errorf("invalid recovery identity")
		}
		var dst string
		var count int
		switch root.Kind {
		case "app":
			var name string
			if err = db.QueryRow("SELECT a.name FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?", root.ID).Scan(&name); err != nil {
				return err
			}
			if name != root.Name || filepath.Base(name) != name || name == "." || name == ".." || s.localApps == nil {
				return fmt.Errorf("invalid app recovery destination")
			}
			dst = filepath.Join(s.localApps.cacheDir, name, "data", strconv.FormatInt(root.ID, 10))
		case "agent":
			if s.agents == nil {
				return fmt.Errorf("agent data directory unavailable")
			}
			if err = db.QueryRow("SELECT COUNT(*) FROM agents WHERE id=?", root.ID).Scan(&count); err != nil || count != 1 {
				return fmt.Errorf("unknown recovery agent")
			}
			dst = filepath.Join(s.agents.dataDir, fmt.Sprintf("instance_%d", root.ID))
		case "mcp":
			if err = db.QueryRow("SELECT COUNT(*) FROM mcp_servers WHERE id=? AND source=?", root.ID, managedMCPSource).Scan(&count); err != nil || count != 1 {
				return fmt.Errorf("unknown recovery MCP")
			}
			dst = s.managedMCPSourceDir(root.ID)
		default:
			return fmt.Errorf("unknown recovery root")
		}
		if destinations[dst] {
			return fmt.Errorf("duplicate recovery destination")
		}
		destinations[dst] = true
		tmp, err := add(dst, true)
		if err != nil {
			return err
		}
		for name, path := range staged {
			if !strings.HasPrefix(name, root.Archive+"/") {
				continue
			}
			if used[name] {
				return fmt.Errorf("overlapping recovery inventory")
			}
			rel := strings.TrimPrefix(name, root.Archive+"/")
			if rel == "" || filepath.IsAbs(rel) || filepath.ToSlash(filepath.Clean(rel)) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
				return fmt.Errorf("invalid recovery path")
			}
			dest := filepath.Join(tmp, filepath.FromSlash(rel))
			if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
				return err
			}
			if err = copyFile(path, dest); err != nil {
				return err
			}
			if err = os.Chmod(dest, os.FileMode(m.Files[name].Mode)); err != nil {
				return err
			}
			used[name] = true
		}
		if err = syncRecoveryDir(tmp); err != nil {
			return err
		}
	}
	if len(used) != len(m.Files) {
		return fmt.Errorf("unassigned recovery files")
	}
	// Process identities cannot be inherited on a fresh machine. The existing
	// running/stopped intent remains available to the normal resume path.
	if _, err = db.Exec("UPDATE agents SET pid=0,port=0,core_api_key='',core_started_at=NULL"); err != nil {
		return err
	}
	// App binaries are rebuildable; paths into the previous host cannot be used.
	if m.SourceCacheDir != "" && s.localApps != nil {
		if _, err = db.Exec("UPDATE app_installs SET local_bin_path=REPLACE(local_bin_path,?,?) WHERE local_bin_path LIKE ?", m.SourceCacheDir, s.localApps.cacheDir, m.SourceCacheDir+"/%"); err != nil {
			return err
		}
	}
	if err = db.Close(); err != nil {
		return err
	}
	if keyFingerprint(s.secret) != m.KeyFingerprint {
		tmp, err := add(filepath.Join(s.dataDir, ".secret"), false)
		if err != nil {
			return err
		}
		if err = os.WriteFile(tmp, []byte(hex.EncodeToString(key)), 0600); err != nil {
			return err
		}
		f, err := os.OpenFile(tmp, os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	tmp, err := add(s.dbPath, false)
	if err != nil {
		return err
	}
	if err = copyFile(dbSource, tmp); err != nil {
		return err
	}
	if err = durableJSON(marker, plan); err != nil {
		return err
	}
	committed = true
	return nil
}

// Apply before opening any runtime/database. Each replacement is on the same
// filesystem. An interrupted activation resumes from the durable plan; startup
// fails closed until every swap is installed. Previous trees remain as backups.
func applyPendingRecovery(dbPath string) error {
	marker := dbPath + ".recovery.json"
	raw, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var plan recoveryPlan
	if err = json.Unmarshal(raw, &plan); err != nil {
		return err
	}
	if plan.Database != dbPath || len(plan.Swaps) == 0 {
		return fmt.Errorf("invalid recovery plan")
	}
	for _, swap := range plan.Swaps {
		if _, err = os.Stat(swap.Source); os.IsNotExist(err) {
			if _, err = os.Stat(swap.Destination); err != nil {
				return fmt.Errorf("missing activated recovery destination: %w", err)
			}
			continue
		} else if err != nil {
			return err
		}
		if _, err = os.Lstat(swap.Backup); os.IsNotExist(err) {
			if _, err = os.Lstat(swap.Destination); err == nil {
				if err = os.Rename(swap.Destination, swap.Backup); err != nil {
					return err
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if err = os.Rename(swap.Source, swap.Destination); err != nil {
			return err
		}
		if err = syncRecoveryDir(filepath.Dir(swap.Destination)); err != nil {
			return err
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err = os.Remove(marker); err != nil {
		return err
	}
	return syncRecoveryDir(filepath.Dir(marker))
}
