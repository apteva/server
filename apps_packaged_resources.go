package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// Packaged app resources.
//
// Source packages and downloaded artifacts keep the app's source tree beside
// the exact executable that was installed:
//
//	<cache>/<app>/<version>/packages/<digest>/bin
//	<cache>/<app>/<version>/artifacts/<digest>/bin
//	                                  <digest>/src/<runtime.source.entry>/ui
//	                                  <digest>/src/<runtime.source.entry>/<db.migrations>
//
// Every start path (first install, boot resume, respawn, managed runtime)
// derives APTEVA_UI_DIR and APTEVA_MIGRATIONS_DIR from that executable. Two
// packages can share one version, so the directory is never reconstructed
// from the app name and version, and never picked by scanning the cache.

// appResourceDirs is where an app's UI bundle and migrations live.
type appResourceDirs struct {
	Root       string // source root joined with runtime.source.entry
	UI         string
	Migrations string // empty when the manifest declares none
}

// appEntryDir joins a source root with the manifest's runtime.source.entry.
// An empty entry, or ".", is the source root itself.
func appEntryDir(m *sdk.Manifest, srcRoot string) string {
	if m.Runtime.Source != nil {
		if e := strings.TrimSpace(m.Runtime.Source.Entry); e != "" && e != "." {
			return filepath.Join(srcRoot, filepath.FromSlash(e))
		}
	}
	return srcRoot
}

// appResourceDirsAt resolves resources inside an app's entry directory. An
// absolute db.migrations path is kept as declared.
func appResourceDirsAt(m *sdk.Manifest, entryDir string) appResourceDirs {
	dirs := appResourceDirs{Root: entryDir, UI: filepath.Join(entryDir, "ui")}
	if m.DB != nil && m.DB.Migrations != "" {
		dirs.Migrations = m.DB.Migrations
		if !filepath.IsAbs(dirs.Migrations) {
			dirs.Migrations = filepath.Join(entryDir, filepath.FromSlash(dirs.Migrations))
		}
	}
	return dirs
}

// applyEnv replaces any resource paths already in env, including obsolete
// ones restored from a persisted environment.
func (d appResourceDirs) applyEnv(env map[string]string) {
	env["APTEVA_UI_DIR"] = d.UI
	if d.Migrations != "" {
		env["APTEVA_MIGRATIONS_DIR"] = d.Migrations
	} else {
		delete(env, "APTEVA_MIGRATIONS_DIR")
	}
}

// packagedBinary identifies an executable inside a hashed package.
type packagedBinary struct {
	Layout  string // "packages" or "artifacts"
	Digest  string
	Version string // the installed release's version directory
}

func (p packagedBinary) shortDigest() string {
	if len(p.Digest) > 12 {
		return p.Digest[:12]
	}
	return p.Digest
}

// packagedBinaryPath reports whether bin is <...>/<version>/{packages,artifacts}/<sha256>/bin.
// Legacy flat caches, binary releases and local-development builds are not.
func packagedBinaryPath(bin string) (packagedBinary, bool) {
	if bin == "" || filepath.Base(bin) != "bin" {
		return packagedBinary{}, false
	}
	pkgDir := filepath.Dir(bin)
	layoutDir := filepath.Dir(pkgDir)
	p := packagedBinary{Layout: filepath.Base(layoutDir), Digest: filepath.Base(pkgDir), Version: filepath.Base(filepath.Dir(layoutDir))}
	if (p.Layout != "packages" && p.Layout != "artifacts") || !validArtifactDigest(p.Digest) {
		return packagedBinary{}, false
	}
	return p, true
}

// packagedResourceDirs resolves resources for the exact packaged executable.
// ok is false when bin is not a packaged executable.
func packagedResourceDirs(m *sdk.Manifest, bin string) (dirs appResourceDirs, pkg packagedBinary, ok bool, err error) {
	pkg, ok = packagedBinaryPath(bin)
	if !ok {
		return appResourceDirs{}, pkg, false, nil
	}
	if m.Runtime.Source != nil {
		if e := strings.TrimSpace(m.Runtime.Source.Entry); e != "" && e != "." && !safeArtifactRelative(filepath.Clean(filepath.FromSlash(e))) {
			return appResourceDirs{}, pkg, true, fmt.Errorf("runtime.source.entry %q leaves the package", e)
		}
	}
	return appResourceDirsAt(m, appEntryDir(m, filepath.Join(filepath.Dir(bin), "src"))), pkg, true, nil
}

// manifestServesUI reports whether the manifest advertises files the
// sidecar serves from its UI directory.
func manifestServesUI(m *sdk.Manifest) bool {
	isUI := func(entry string) bool { return strings.HasPrefix(strings.TrimSpace(entry), "/ui/") }
	for _, p := range m.Provides.UIPanels {
		if isUI(p.Entry) {
			return true
		}
	}
	for _, c := range m.Provides.UIComponents {
		if isUI(c.Entry) {
			return true
		}
	}
	for _, s := range m.Provides.UISurfaces {
		if isUI(s.Entry) {
			return true
		}
	}
	return false
}

// verifyPackagedResources checks the directories the manifest depends on.
// A process can pass its health check while its UI routes 404, so a missing
// directory is reported here with enough detail to act on.
func verifyPackagedResources(installID int64, m *sdk.Manifest, bin string, pkg packagedBinary, dirs appResourceDirs) error {
	missing := func(what, path string) error {
		return fmt.Errorf("app %s install %d version %s: %s directory missing at %s; the cached package %s/%s is incomplete. Delete %s and reinstall %s %s to rebuild the same release",
			m.Name, installID, pkg.Version, what, path, pkg.Layout, pkg.shortDigest(), filepath.Dir(bin), m.Name, pkg.Version)
	}
	isDir := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.IsDir()
	}
	if manifestServesUI(m) && !isDir(dirs.UI) {
		return missing("UI", dirs.UI)
	}
	if dirs.Migrations != "" && !filepath.IsAbs(m.DB.Migrations) && !isDir(dirs.Migrations) {
		return missing("migrations", dirs.Migrations)
	}
	return nil
}

// applyPackagedResourceEnv points env at the resources of the exact packaged
// executable bin and verifies the ones the manifest requires. It leaves env
// unchanged for executables outside a hashed package.
func applyPackagedResourceEnv(installID int64, m *sdk.Manifest, bin string, env map[string]string) error {
	dirs, pkg, ok, err := packagedResourceDirs(m, bin)
	if !ok {
		return nil
	}
	if err != nil {
		return fmt.Errorf("app %s install %d version %s: %w", m.Name, installID, pkg.Version, err)
	}
	if err := verifyPackagedResources(installID, m, bin, pkg, dirs); err != nil {
		return err
	}
	dirs.applyEnv(env)
	return nil
}

// installResourceEnv sets resource directories for restarting an installed
// app from binPath. Packaged executables use their own package. Legacy flat
// source caches keep <cache>/<app>/<installed version>/src. Binary releases
// and local-development builds keep whatever env already holds.
func (sup *LocalSupervisor) installResourceEnv(installID int64, m *sdk.Manifest, binPath, installedVersion string, env map[string]string) error {
	if _, ok := packagedBinaryPath(binPath); ok {
		return applyPackagedResourceEnv(installID, m, binPath, env)
	}
	if m.Runtime.Source != nil && m.Runtime.Source.Repo != "" {
		version := installedVersion
		if version == "" {
			version = m.Version
		}
		appResourceDirsAt(m, appEntryDir(m, filepath.Join(sup.cacheDir, m.Name, version, "src"))).applyEnv(env)
	}
	return nil
}
