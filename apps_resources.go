package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// Cached resources belong to the selected executable, never another package
// sharing its app/version. workingRoot is used only for explicit working-copy
// builds, whose resources intentionally live outside the binary cache.
func applyAppResourceEnv(id int64, m *sdk.Manifest, bin, workingRoot, entry string, env map[string]string) error {
	root := workingRoot
	if root == "" {
		root = filepath.Join(filepath.Dir(bin), "src")
		entry = ""
		if m.Runtime.Source != nil {
			entry = m.Runtime.Source.Entry
		}
	}
	if entry != "" && entry != "." {
		if !safeArtifactRelative(entry) {
			return fmt.Errorf("install %d: invalid source entry %q", id, entry)
		}
		root = filepath.Join(root, filepath.FromSlash(entry))
	}
	ui := filepath.Join(root, "ui")
	env["APTEVA_UI_DIR"] = ui
	require := func(path string, dir bool) error {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() != dir {
			return fmt.Errorf("install %d: declared resource missing or invalid at %s; restore the resources for the selected binary %s", id, path, bin)
		}
		return nil
	}
	entries := []string{m.Icon}
	for _, p := range m.Provides.UIPanels {
		entries = append(entries, p.Entry)
	}
	for _, p := range m.Provides.UIComponents {
		entries = append(entries, p.Entry)
	}
	for _, p := range m.Provides.UISurfaces {
		entries = append(entries, p.Entry)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e, "/ui/") {
			continue
		}
		rel := strings.TrimPrefix(e, "/")
		if !safeArtifactRelative(rel) {
			return fmt.Errorf("install %d: invalid UI resource path %q", id, e)
		}
		if err := require(ui, true); err != nil {
			return err
		}
		if err := require(filepath.Join(root, filepath.FromSlash(rel)), strings.HasSuffix(e, "/")); err != nil {
			return err
		}
	}
	delete(env, "APTEVA_MIGRATIONS_DIR")
	if m.DB != nil && m.DB.Migrations != "" {
		migrations := m.DB.Migrations
		if !filepath.IsAbs(migrations) {
			if !safeArtifactRelative(migrations) {
				return fmt.Errorf("install %d: invalid migrations path %q", id, migrations)
			}
			migrations = filepath.Join(root, filepath.FromSlash(migrations))
		}
		env["APTEVA_MIGRATIONS_DIR"] = migrations
		if err := require(migrations, true); err != nil {
			return err
		}
	}
	return nil
}
