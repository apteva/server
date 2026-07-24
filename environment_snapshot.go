package main

// environment_snapshot.go — snapshot/restore of Environment state.
//
// A snapshot is a reusable environment fixture. It freezes:
//   - an agent's full state: config.json + history/ + memory.jsonl
//   - each in-environment sidecar's SQLite data dir (real rows the apps wrote)
//   - the environment's cassette (recorded external responses)
//
// Snapshots are plain filesystem artifacts under <dataDir>/environments/snapshots/
// — no DB schema change. The directory layout IS the format:
//
//   <root>/<snapshotID>/
//     manifest.json          — SnapshotManifest
//     agent/                 — copy of the agent instance dir (optional)
//     apps/<name>/           — copy of that sidecar's data dir
//     cassette.json          — the edge cassette (optional)
//
// Restore materialises those back into a fresh agent instance dir and fresh
// per-app data dirs, which a new Environment's SpawnSandboxedApp consumes via
// SandboxApp.DataDir — so the forked environment starts byte-identical, the
// property that makes runs independent and repeatable.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// SnapshotManifest is the metadata written at the root of every snapshot.
type SnapshotManifest struct {
	ID               string                        `json:"id"`
	ProjectID        string                        `json:"project_id"`
	OwnerInstallID   int64                         `json:"owner_install_id,omitempty"`
	Description      string                        `json:"description,omitempty"`
	Apps             []string                      `json:"apps"` // sidecar names captured
	SourceInstallIDs map[string]int64              `json:"source_install_ids,omitempty"`
	ManagedMCPs      []sdk.RuntimeManagedMCP       `json:"managed_mcps,omitempty"`
	HasAgent         bool                          `json:"has_agent"`    // agent/ dir present
	HasCassette      bool                          `json:"has_cassette"` // cassette.json present
	Subscriptions    []EnvironmentSubscriptionSpec `json:"subscriptions,omitempty"`
	CreatedAt        time.Time                     `json:"created_at"`
}

// SnapshotStore manages snapshot artifacts on disk.
type SnapshotStore struct {
	root string
}

// NewSnapshotStore roots the store at <dataDir>/environments/snapshots.
func NewSnapshotStore(environmentsDir string) *SnapshotStore {
	root := filepath.Join(environmentsDir, "snapshots")
	_ = os.MkdirAll(root, 0755)
	return &SnapshotStore{root: root}
}

func (ss *SnapshotStore) dir(id string) string { return filepath.Join(ss.root, id) }

// CaptureSpec declares what to snapshot.
type CaptureSpec struct {
	ID             string
	ProjectID      string
	OwnerInstallID int64
	Description    string
	// AgentInstanceDir, when set, is copied into the snapshot's agent/ dir.
	AgentInstanceDir string
	// AppDataDirs maps sidecar name → its live data dir to copy.
	AppDataDirs map[string]string
	// SourceInstallIDs maps each captured app name to the project install it
	// was cloned from, allowing a snapshot-only runtime request to restore apps.
	SourceInstallIDs map[string]int64
	ManagedMCPs      []sdk.RuntimeManagedMCP
	// Cassette, when non-nil, is saved as cassette.json.
	Cassette *Cassette
	// Subscriptions are logical environment-owned event routes. Raw DB row ids
	// are intentionally not captured.
	Subscriptions []EnvironmentSubscriptionSpec
}

// Capture writes a new snapshot. Fails if the id already exists.
func (ss *SnapshotStore) Capture(spec CaptureSpec) (*SnapshotManifest, error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("snapshot: ID required")
	}
	dir := ss.dir(spec.ID)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("snapshot %q already exists", spec.ID)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	man := &SnapshotManifest{
		ID:               spec.ID,
		ProjectID:        spec.ProjectID,
		OwnerInstallID:   spec.OwnerInstallID,
		Description:      spec.Description,
		SourceInstallIDs: cloneInt64Map(spec.SourceInstallIDs),
		ManagedMCPs:      append([]sdk.RuntimeManagedMCP(nil), spec.ManagedMCPs...),
		Subscriptions:    append([]EnvironmentSubscriptionSpec(nil), spec.Subscriptions...),
		CreatedAt:        time.Now(),
	}

	if spec.AgentInstanceDir != "" {
		if err := copyTree(spec.AgentInstanceDir, filepath.Join(dir, "agent")); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("capture agent: %w", err)
		}
		man.HasAgent = true
	}

	for name, src := range spec.AppDataDirs {
		if err := copyTree(src, filepath.Join(dir, "apps", name)); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("capture app %q: %w", name, err)
		}
		man.Apps = append(man.Apps, name)
	}

	if spec.Cassette != nil {
		if err := spec.Cassette.Save(filepath.Join(dir, "cassette.json")); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("capture cassette: %w", err)
		}
		man.HasCassette = true
	}

	if err := writeJSONFile(filepath.Join(dir, "manifest.json"), man); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return man, nil
}

// Get loads a snapshot's manifest.
func (ss *SnapshotStore) Get(id string) (*SnapshotManifest, error) {
	data, err := os.ReadFile(filepath.Join(ss.dir(id), "manifest.json"))
	if err != nil {
		return nil, err
	}
	var man SnapshotManifest
	if err := json.Unmarshal(data, &man); err != nil {
		return nil, err
	}
	return &man, nil
}

func (ss *SnapshotStore) AssignOwner(id, projectID string, ownerInstallID int64) error {
	man, err := ss.Get(id)
	if err != nil {
		return err
	}
	if man.ProjectID != projectID {
		return fmt.Errorf("snapshot %q belongs to another project", id)
	}
	if man.OwnerInstallID != 0 && man.OwnerInstallID != ownerInstallID {
		return fmt.Errorf("snapshot %q is already owned by another app", id)
	}
	man.OwnerInstallID = ownerInstallID
	return writeJSONFile(filepath.Join(ss.dir(id), "manifest.json"), man)
}

// List returns every snapshot manifest, newest first not guaranteed.
func (ss *SnapshotStore) List() ([]*SnapshotManifest, error) {
	ents, err := os.ReadDir(ss.root)
	if err != nil {
		return nil, err
	}
	var out []*SnapshotManifest
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if man, err := ss.Get(e.Name()); err == nil {
			out = append(out, man)
		}
	}
	return out, nil
}

// Delete removes a snapshot's artifacts.
func (ss *SnapshotStore) Delete(id string) error { return os.RemoveAll(ss.dir(id)) }

// Cassette loads the snapshot's cassette, or nil if it has none.
func (ss *SnapshotStore) Cassette(id string) (*Cassette, error) {
	p := filepath.Join(ss.dir(id), "cassette.json")
	if _, err := os.Stat(p); err != nil {
		return nil, nil
	}
	return LoadCassette(p)
}

// Restore materialises a snapshot for a fresh Environment run. It copies each
// app's captured data dir into a new temp dir and returns name→dir, ready
// to feed into SandboxApp.DataDir. If agentInto is non-empty the agent
// state is copied there too.
func (ss *SnapshotStore) Restore(id, agentInto string) (appDataDirs map[string]string, err error) {
	man, err := ss.Get(id)
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	src := ss.dir(id)

	if man.HasAgent && agentInto != "" {
		if err := copyTree(filepath.Join(src, "agent"), agentInto); err != nil {
			return nil, fmt.Errorf("restore agent: %w", err)
		}
	}

	appDataDirs = make(map[string]string, len(man.Apps))
	for _, name := range man.Apps {
		dst, err := os.MkdirTemp("", "apteva-environment-restore-"+name+"-")
		if err != nil {
			return nil, err
		}
		if err := copyTree(filepath.Join(src, "apps", name), dst); err != nil {
			return nil, fmt.Errorf("restore app %q: %w", name, err)
		}
		appDataDirs[name] = dst
	}
	return appDataDirs, nil
}

// ─── small fs helpers ──────────────────────────────────────────────────

// copyTree recursively copies src into dst (dst created if missing). Used
// for instance dirs and sidecar data dirs — both modest in size. Reuses
// copyFile from backups.go for individual files.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		return copyFile(src, dst)
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
