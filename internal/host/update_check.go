package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CheckSummary is the outcome of a candidate's database check, reported in
// /validate responses and on the deployment.
type CheckSummary struct {
	// Classification: unchanged (the release changes no stored data),
	// compatible (it adds to the database and the previous server can still
	// run on it), or incompatible.
	Classification string   `json:"classification"`
	Compatible     bool     `json:"compatible"`
	Reasons        []string `json:"reasons,omitempty"`
	Backup         string   `json:"backup,omitempty"`
	BackupBytes    int64    `json:"backup_bytes,omitempty"`
	DurationMS     int64    `json:"duration_ms,omitempty"`
}

type preflightResult struct {
	restart bool
	summary *CheckSummary
}

// checkStall stops a candidate's check that stops reporting progress.
const checkStall = 2 * time.Minute

// preflight runs the candidate's database check against the live database.
// It reports restart=true when the release changes stored schema or data and
// the caller allowed a restart migration; without that permission such a
// release is refused. deploy asks the check to back up the database when the
// previous server could not run on the migrated one.
func (h *Host) preflight(ctx context.Context, c *Candidate, deploy bool, progress func(string)) (preflightResult, error) {
	var result preflightResult
	capabilities, err := h.probeCandidate(ctx, *c)
	if err != nil {
		return result, err
	}
	bounded := false
	for _, capability := range capabilities {
		bounded = bounded || capability == CapabilityUpdateCheck
	}
	root := filepath.Join(h.cfg.Home, "host", "preflight")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return result, err
	}
	sweepPreflight(root)
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	logPath := filepath.Join(root, id+".log")
	env := map[string]string{"APTEVA_HOME": h.cfg.Home, "DATA_DIR": h.cfg.DataDir, "DB_PATH": h.cfg.DBPath, "APTEVA_HOST_SOCKET": "", "APTEVA_HOST_SLOT_SOCKET": "", ControlTokenEnv: ""}
	backup := filepath.Join(h.cfg.Home, "update-backups", "host-"+id, "apteva.db")
	// The backup belongs to this deployment only once the check passes and
	// the deployment goes ahead; a failed or refused check leaves nothing
	// behind. Older backups are pruned only after the deployment succeeds
	// (pruneUpdateBackups), so a refusal never costs the previous backup.
	keepBackup := false
	defer func() {
		if !keepBackup {
			os.RemoveAll(filepath.Dir(backup))
		}
	}()
	report := filepath.Join(dir, "report.json")
	if bounded {
		// A reduced copy of the database; a backup only when one is needed.
		if err := preflightSpace(h.cfg.DBPath, dir, 0, 1<<30); err != nil {
			return result, err
		}
		env["APTEVA_UPDATE_CHECK_DIR"] = dir
		env["APTEVA_UPDATE_PREFLIGHT"] = ""
		if deploy {
			if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
				return result, err
			}
			env["APTEVA_UPDATE_BACKUP"] = backup
			env["APTEVA_UPDATE_BACKUP_WHEN"] = "incompatible"
		}
	} else {
		// Earlier servers back up and clone the whole database.
		if err := preflightSpace(h.cfg.DBPath, dir, 3, 256<<20); err != nil {
			return result, err
		}
		env["APTEVA_UPDATE_PREFLIGHT"] = filepath.Join(dir, "apteva.db")
		env["APTEVA_UPDATE_CHECK_DIR"] = ""
		report = filepath.Join(dir, "apteva.db.json")
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cmd := exec.CommandContext(runCtx, c.Binary, "--preflight")
	cmd.Env = overrides(os.Environ(), env)
	// SIGTERM lets the check remove its copy and any partial backup.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 20 * time.Second
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return result, err
	}
	defer logf.Close()
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return result, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if bounded {
		err = watchCheckProgress(dir, done, progress, func() { cancel(errors.New("the database check stopped reporting progress")) })
	} else {
		err = <-done
	}
	if err != nil {
		if cause := context.Cause(runCtx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = fmt.Errorf("%w: %v", cause, err)
		}
		os.RemoveAll(filepath.Dir(backup))
		return result, fmt.Errorf("candidate preflight failed; see %s: %w", logPath, err)
	}
	var parsed struct {
		Version        string   `json:"version"`
		SchemaHash     string   `json:"schema_hash"`
		RollbackSafe   bool     `json:"rollback_safe"`
		Compatible     *bool    `json:"compatible"`
		Classification string   `json:"classification"`
		Reasons        []string `json:"reasons"`
		DurationMS     int64    `json:"duration_ms"`
		Backup         *struct {
			Path  string `json:"path"`
			Bytes int64  `json:"bytes"`
		} `json:"backup"`
	}
	b, err := os.ReadFile(report)
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(b, &parsed); err != nil {
		return result, err
	}
	if parsed.Version != c.Version || parsed.SchemaHash == "" {
		return result, errors.New("candidate preflight did not report this release's database identity")
	}
	summary := &CheckSummary{Classification: parsed.Classification, Reasons: parsed.Reasons, DurationMS: parsed.DurationMS}
	if parsed.Compatible != nil {
		summary.Compatible = *parsed.Compatible
	} else {
		// Earlier servers only report whether anything changes.
		summary.Compatible = parsed.RollbackSafe
		summary.Classification = "incompatible"
		if parsed.RollbackSafe {
			summary.Classification = "unchanged"
		}
		if deploy && !parsed.RollbackSafe {
			// Keep their full backup for a deployment that migrates.
			if err := os.MkdirAll(filepath.Dir(backup), 0o700); err == nil && os.Rename(filepath.Join(dir, "apteva.db"), backup) == nil {
				parsed.Backup = &struct {
					Path  string `json:"path"`
					Bytes int64  `json:"bytes"`
				}{Path: backup}
			}
		}
	}
	if parsed.Backup != nil && parsed.Backup.Path != "" {
		summary.Backup, summary.BackupBytes = parsed.Backup.Path, parsed.Backup.Bytes
	}
	result.summary = summary
	c.compatible = summary.Compatible
	if !parsed.RollbackSafe {
		if !c.AllowRestart {
			summary.Backup, summary.BackupBytes = "", 0
			return result, errors.New(restartMigrationRequired)
		}
		result.restart = true
	} else {
		c.SchemaHash = parsed.SchemaHash
		c.ExistingSchema = true
	}
	if summary.Backup != "" {
		c.backup = summary.Backup
		keepBackup = true
	}
	return result, nil
}

// watchCheckProgress waits for the check, forwarding its progress messages
// and stopping it when it stops reporting.
func watchCheckProgress(dir string, done <-chan error, progress func(string), stall func()) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	started := time.Now()
	shown := ""
	stalled := false
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
		}
		var state struct {
			Message   string    `json:"message"`
			UpdatedAt time.Time `json:"updated_at"`
		}
		last := started
		if raw, err := os.ReadFile(filepath.Join(dir, "progress.json")); err == nil && json.Unmarshal(raw, &state) == nil {
			if state.UpdatedAt.After(last) {
				last = state.UpdatedAt
			}
			if progress != nil && state.Message != "" && state.Message != shown {
				progress(state.Message)
				shown = state.Message
			}
		}
		if !stalled && time.Since(last) > checkStall {
			stalled = true
			stall()
		}
	}
}

// sweepPreflight removes work directories left by checks that did not finish
// and keeps the newest check logs.
func sweepPreflight(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var logs []string
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if strings.HasSuffix(entry.Name(), ".log") {
			logs = append(logs, path)
			continue
		}
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.RemoveAll(path)
		}
	}
	sort.Strings(logs) // names are creation times in base 36 of equal length
	for len(logs) > 10 {
		os.Remove(logs[0])
		logs = logs[1:]
	}
}

// pruneSucceededBackups removes the backups of earlier updates once a
// deployment that took its own backup has succeeded.
func pruneSucceededBackups(c Candidate) {
	if c.backup == "" {
		return
	}
	keep := filepath.Dir(c.backup)
	pruneUpdateBackups(filepath.Dir(keep), keep)
}

// pruneUpdateBackups removes every backup directory under root except keep.
func pruneUpdateBackups(root, keep string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() && path != keep {
			os.RemoveAll(path)
		}
	}
}

func (h *Host) recordCheck(summary *CheckSummary) {
	if summary == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state.Job != nil {
		h.state.Job.Check = summary
		_ = h.saveLocked()
	}
}
