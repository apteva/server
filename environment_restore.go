package main

// environment_restore.go — consistent snapshot capture and id-mapped restore.
//
// App data inside an environment is written under the environment's own
// project id and install ids. A snapshot records those ids; restoring it into
// a new environment hands each app an sdk.RestoreContext that maps them to the
// new ones, and the app rewrites its data before it serves anything
// (sdk.ApplyRestore). The server checks the outcome through a result file the
// app writes and through its /health body, and fails the create otherwise.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// localRestore is one app's part of a snapshot restore.
type localRestore struct {
	Context     sdk.RestoreContext
	ContextPath string
}

// prepare writes the context file and returns env plus the variable that
// makes the SDK apply it. The file stays until the environment is removed,
// so a later restart of the same process finds it and skips (the SDK records
// the restore id in the app's database).
func (r *localRestore) prepare(env map[string]string) (map[string]string, error) {
	if err := r.Context.Validate(); err != nil {
		return nil, err
	}
	_ = os.Remove(r.Context.ResultPath)
	data, err := json.Marshal(r.Context)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(r.ContextPath), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(r.ContextPath, data, 0o600); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out[sdk.RestoreContextEnv] = r.ContextPath
	return out, nil
}

func (r *localRestore) result() (*sdk.RestoreResult, error) {
	data, err := os.ReadFile(r.Context.ResultPath)
	if err != nil {
		return nil, err
	}
	var out sdk.RestoreResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("read restore result: %w", err)
	}
	return &out, nil
}

// failure returns the app's own error when it reported a failed restore.
func (r *localRestore) failure() string {
	res, err := r.result()
	if err != nil || res.ID != r.Context.ID || res.Status == sdk.RestoreStatusApplied {
		return ""
	}
	if res.Error == "" {
		return "app reported a failed restore"
	}
	return res.Error
}

// verify confirms the running app applied this restore: the result file says
// so and the live process reports the same restore id on /health. An app
// built against an SDK without restore support writes neither, and its data
// would stay under the old ids, so that fails too.
func (r *localRestore) verify(sidecarURL string) error {
	res, err := r.result()
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("app did not report a restore result; rebuild it with an app-sdk that supports snapshot restore")
	}
	if err != nil {
		return err
	}
	if res.ID != r.Context.ID || res.Status != sdk.RestoreStatusApplied {
		if res.Error != "" {
			return errors.New(res.Error)
		}
		return fmt.Errorf("restore result %q for %q, want %q for %q", res.Status, res.ID, sdk.RestoreStatusApplied, r.Context.ID)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(sidecarURL + "/health")
	if err != nil {
		return fmt.Errorf("confirm restore on /health: %w", err)
	}
	defer resp.Body.Close()
	var health struct {
		Restore   string `json:"restore"`
		RestoreID string `json:"restore_id"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health)
	if resp.StatusCode != http.StatusOK || health.Restore != sdk.RestoreStatusApplied || health.RestoreID != r.Context.ID {
		return fmt.Errorf("/health does not report restore %q (status %d)", r.Context.ID, resp.StatusCode)
	}
	return nil
}

// restorePlan maps a snapshot's data ids to a new environment's install ids.
// It returns nil when the snapshot predates id mapping or nothing is restored.
func restorePlan(source *SnapshotManifest, environmentID, newPublicURL, dir string, restoredDirs map[string]string, installs map[string]*localInstall) (map[string]*localRestore, error) {
	if !source.restoreMapped() || len(restoredDirs) == 0 {
		if source != nil && !source.restoreMapped() && len(restoredDirs) > 0 {
			log.Printf("[ENVIRONMENT] snapshot %s has no data id mapping; restoring %s by plain copy", source.ID, environmentID)
		}
		return nil, nil
	}
	ids := map[int64]int64{}
	for name, oldID := range source.DataInstallIDs {
		if inst, ok := installs[name]; ok && oldID > 0 {
			ids[oldID] = inst.InstallID
		}
	}
	urls := map[string]string{}
	if source.PublicURL != "" && newPublicURL != "" && source.PublicURL != newPublicURL {
		urls[source.PublicURL] = newPublicURL
	}
	out := map[string]*localRestore{}
	for name := range restoredDirs {
		inst, ok := installs[name]
		if !ok {
			continue
		}
		rc := sdk.RestoreContext{
			ID:             fmt.Sprintf("%s/%s/%d", source.ID, environmentID, inst.InstallID),
			OldProjectID:   source.DataProjectID,
			NewProjectID:   environmentID,
			InstallIDs:     ids,
			PublicBaseURLs: urls,
			ResultPath:     filepath.Join(dir, name+".result.json"),
		}
		if err := rc.Validate(); err != nil {
			return nil, fmt.Errorf("restore %s: %w", name, err)
		}
		out[name] = &localRestore{Context: rc, ContextPath: filepath.Join(dir, name+".context.json")}
	}
	return out, nil
}

// pauseInstall stops a running sidecar and returns a function that starts
// the same activation again: same binary, port and environment.
func (sup *LocalSupervisor) pauseInstall(installID int64) (func() error, error) {
	p := sup.currentProc(installID)
	if p == nil {
		return nil, fmt.Errorf("install %d is not running", installID)
	}
	spec := p.spec.clone()
	if spec.binPath == "" || spec.httpPort == 0 {
		return nil, fmt.Errorf("install %d has no restartable activation", installID)
	}
	if err := sup.Stop(installID); err != nil {
		return nil, err
	}
	return func() error { return sup.activate(spec, 30*time.Second) }, nil
}

// captureEnvironmentSnapshot snapshots an environment with every install
// stopped, so each app's files (SQLite WAL included) and the apps relative to
// each other are captured at one point in time. Installs restart afterwards
// in install order; a restart failure is returned after the snapshot exists.
func (s *Server) captureEnvironmentSnapshot(env *Environment, spec CaptureSpec) (*SnapshotManifest, error) {
	env.snapshotMu.Lock()
	defer env.snapshotMu.Unlock()
	names := env.InstallOrder()
	spec.DataProjectID = env.ID
	spec.DataInstallIDs = map[string]int64{}
	spec.PublicURL = s.runtimePlatformURL(env.ID)
	if spec.AppDataDirs == nil {
		spec.AppDataDirs = map[string]string{}
	}
	type paused struct {
		name   string
		resume func() error
	}
	var stopped []paused
	resumeAll := func() error {
		var errs []error
		for i := len(stopped) - 1; i >= 0; i-- {
			if err := stopped[i].resume(); err != nil {
				errs = append(errs, fmt.Errorf("restart %s: %w", stopped[i].name, err))
			}
		}
		return errors.Join(errs...)
	}
	// Stop parents before their dependencies; restart in the reverse order.
	for i := len(names) - 1; i >= 0; i-- {
		inst, ok := env.Install(names[i])
		if !ok || inst.DataDir == "" {
			continue
		}
		spec.AppDataDirs[names[i]] = inst.DataDir
		spec.DataInstallIDs[names[i]] = inst.InstallID
		if s.localApps == nil {
			continue
		}
		resume, err := s.localApps.pauseInstall(inst.InstallID)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("stop %s for snapshot: %w", names[i], err), resumeAll())
		}
		stopped = append(stopped, paused{name: names[i], resume: resume})
	}
	man, err := s.environments.Snapshots().Capture(spec)
	if rerr := resumeAll(); rerr != nil {
		if err != nil {
			return nil, errors.Join(err, rerr)
		}
		return man, fmt.Errorf("snapshot %s saved, but %w", man.ID, rerr)
	}
	return man, err
}
