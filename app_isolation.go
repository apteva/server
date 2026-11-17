package main

// Per-app OS-user isolation for locally supervised sidecars.
//
// Opt-in with APTEVA_APP_ISOLATION=uid. Each install then runs as its own
// uid/gid (APTEVA_APP_UID_BASE + install id, default base 200000) instead of
// the server's user, so one app cannot read another app's database, files
// or process environment (/proc/<pid>/environ needs the same uid), nor the
// platform's database and secret files.
//
// Layout once enabled:
//   <apps>/                        0711 server   (traverse only)
//   <apps>/<name>/                 0711 server
//   <apps>/<name>/<ver>/           0711 server   binary 0755, stderr.log 0600
//   <apps>/<name>/data/            0711 server
//   <apps>/<name>/data/<install>/  0700 app uid  cwd, HOME, APTEVA_DATA_DIR, DB
//   <apps>/.runtime-pids/          0711 server   <deployment>.pid 0600 app uid
//
// The server additionally switches to umask 077 and strips group/other
// access from the top level of its data directory, because app users can
// now traverse into the apps tree that lives inside it.
//
// It needs Linux and a root server (or root apteva-host): setuid and chown
// are not available otherwise. When it is requested but cannot be applied,
// spawns fail closed with an explanation instead of silently running apps
// as the server user. Apps that genuinely need the server's privileges
// (for example the Docker socket) can be listed in
// APTEVA_APP_ISOLATION_EXEMPT=name,name.

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const defaultAppUIDBase = 200000

// Indirections so tests can exercise the layout without root.
var (
	appIsolationPlatformOK = func() bool { return runtime.GOOS == "linux" && os.Geteuid() == 0 }
	appIsolationLchown     = os.Lchown
	appIsolationAncestors  = checkAppIsolationAncestors
)

type appIsolation struct {
	Enabled bool
	UID     uint32
	GID     uint32
}

func appIsolationRequested() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("APTEVA_APP_ISOLATION")), "uid")
}

// appIsolationMode reports "off", "uid" or "unavailable" (requested but the
// host cannot apply it — apps will refuse to start).
func appIsolationMode() string {
	if !appIsolationRequested() {
		return "off"
	}
	if !appIsolationPlatformOK() {
		return "unavailable"
	}
	return "uid"
}

func appIsolationExempt() []string {
	var out []string
	for _, name := range strings.Split(os.Getenv("APTEVA_APP_ISOLATION_EXEMPT"), ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func appIsolationIsExempt(appName string) bool {
	appName = strings.ToLower(strings.TrimSpace(appName))
	for _, name := range appIsolationExempt() {
		if name == appName {
			return true
		}
	}
	return false
}

func appUIDBase() (uint32, error) {
	raw := strings.TrimSpace(os.Getenv("APTEVA_APP_UID_BASE"))
	if raw == "" {
		return defaultAppUIDBase, nil
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || n < 65536 {
		return 0, fmt.Errorf("APTEVA_APP_UID_BASE must be a number >= 65536 (got %q) so app users never collide with system or login users", raw)
	}
	return uint32(n), nil
}

// appIsolationFor decides the identity an install runs as. A zero value with
// nil error means "run as the server user".
func appIsolationFor(appName string, installID int64) (appIsolation, error) {
	if !appIsolationRequested() || appIsolationIsExempt(appName) {
		return appIsolation{}, nil
	}
	if !appIsolationPlatformOK() {
		return appIsolation{}, fmt.Errorf("APTEVA_APP_ISOLATION=uid needs a Linux host with the server running as root; refusing to start %s as the server user (unset it, or list the app in APTEVA_APP_ISOLATION_EXEMPT)", appName)
	}
	if installID <= 0 {
		return appIsolation{}, fmt.Errorf("app isolation: %s has no install id to derive a user from", appName)
	}
	base, err := appUIDBase()
	if err != nil {
		return appIsolation{}, err
	}
	uid := uint64(base) + uint64(installID)
	if uid >= 1<<31-1 {
		return appIsolation{}, fmt.Errorf("app isolation: uid for install %d overflows (base %d)", installID, base)
	}
	return appIsolation{Enabled: true, UID: uint32(uid), GID: uint32(uid)}, nil
}

// apply sets the child's credentials for a direct (non-hosted) spawn. It
// keeps whatever SysProcAttr the caller already configured.
func (iso appIsolation) apply(cmd *exec.Cmd) {
	if !iso.Enabled {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: iso.UID, Gid: iso.GID, Groups: []uint32{}}
}

// prepareAppIsolationLayout makes the on-disk layout described above for one
// install. appsRoot is the supervisor's apps directory; bin the binary;
// persistentRoot the install's data dir; logPath the server-owned log file;
// pidFile the optional managed-runtime pid file the launcher shell writes.
func prepareAppIsolationLayout(iso appIsolation, appsRoot, bin, persistentRoot, logPath, pidFile string) error {
	if !iso.Enabled {
		return nil
	}
	if err := appIsolationAncestors(appsRoot); err != nil {
		return err
	}
	traverseOnly := append([]string{appsRoot}, dirsBelow(appsRoot, filepath.Dir(bin))...)
	traverseOnly = append(traverseOnly, dirsBelow(appsRoot, filepath.Dir(persistentRoot))...)
	for _, d := range traverseOnly {
		if err := os.Chmod(d, 0o711); err != nil {
			return fmt.Errorf("app isolation: %w", err)
		}
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("app isolation: %w", err)
	}
	if logPath != "" {
		if err := os.Chmod(logPath, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("app isolation: %w", err)
		}
	}
	if err := os.MkdirAll(persistentRoot, 0o700); err != nil {
		return fmt.Errorf("app isolation: %w", err)
	}
	if err := chownTreeIfNeeded(persistentRoot, iso.UID, iso.GID); err != nil {
		return err
	}
	if err := os.Chmod(persistentRoot, 0o700); err != nil {
		return fmt.Errorf("app isolation: %w", err)
	}
	if pidFile != "" {
		if err := os.MkdirAll(filepath.Dir(pidFile), 0o711); err != nil {
			return fmt.Errorf("app isolation: %w", err)
		}
		if err := os.Chmod(filepath.Dir(pidFile), 0o711); err != nil {
			return fmt.Errorf("app isolation: %w", err)
		}
		f, err := os.OpenFile(pidFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("app isolation: %w", err)
		}
		_ = f.Close()
		if err := appIsolationLchown(pidFile, int(iso.UID), int(iso.GID)); err != nil {
			return fmt.Errorf("app isolation: chown pid file: %w", err)
		}
	}
	return nil
}

// dirsBelow lists leaf and each of its ancestors strictly below root; empty
// when leaf is not inside root.
func dirsBelow(root, leaf string) []string {
	var out []string
	prefix := filepath.Clean(root) + string(filepath.Separator)
	for d := filepath.Clean(leaf); strings.HasPrefix(d, prefix); d = filepath.Dir(d) {
		out = append(out, d)
	}
	return out
}

// chownTreeIfNeeded hands the install's data to its user. Data written by an
// earlier, unisolated run is owned by the server, so the first isolated
// start migrates the whole tree; later starts only fix the root.
func chownTreeIfNeeded(root string, uid, gid uint32) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("app isolation: %w", err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid == uid && st.Gid == gid {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := appIsolationLchown(path, int(uid), int(gid)); err != nil {
			return fmt.Errorf("app isolation: chown %s: %w", path, err)
		}
		return nil
	})
}

// checkAppIsolationAncestors refuses layouts the app users cannot reach.
// Directories above the apps root belong to the operator (for example a
// private /root), so they are never changed here.
func checkAppIsolationAncestors(appsRoot string) error {
	for d := filepath.Dir(appsRoot); ; d = filepath.Dir(d) {
		info, err := os.Stat(d)
		if err != nil {
			return fmt.Errorf("app isolation: %w", err)
		}
		if info.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("app isolation: %s (mode %04o) is not traversable by app users; grant search permission (chmod o+x %s) or move the apps directory with APTEVA_HOME", d, info.Mode().Perm(), d)
		}
		if parent := filepath.Dir(d); parent == d {
			return nil
		}
	}
}

// tightenServerDataPermissions runs once at startup when isolation is on.
// App users can traverse to the apps tree, so every other top-level entry of
// the server's data directory (the platform database, .secret, backups,
// security log, …) loses group/other access. Anything nested below a 0700
// directory is unreachable already, so the top level is sufficient.
func tightenServerDataPermissions(dataDir, appsRoot string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	appsAbs, _ := filepath.Abs(appsRoot)
	var changed []string
	for _, e := range entries {
		path := filepath.Join(dataDir, e.Name())
		abs, _ := filepath.Abs(path)
		if abs == appsAbs || strings.HasPrefix(appsAbs, abs+string(filepath.Separator)) {
			continue // the apps tree (or a directory that contains it) is laid out per install
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		perm := info.Mode().Perm()
		if perm&0o077 == 0 {
			continue
		}
		if err := os.Chmod(path, perm&^0o077); err != nil {
			return changed, err
		}
		changed = append(changed, e.Name())
	}
	return changed, nil
}
