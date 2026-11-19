package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func servedVersion(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestRestartMigrationDeploysSchemaChangingRelease(t *testing.T) {
	h, c, url, candidate := fixture(t)
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	resp, err := http.Post(url, "text/plain", strings.NewReader("before-update"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var validated struct {
		Validated       bool          `json:"validated"`
		RestartRequired bool          `json:"restart_required"`
		Check           *CheckSummary `json:"check"`
	}
	candidate.AllowRestart = true
	if err := Call(context.Background(), c, "POST", "/validate", candidate, &validated); err != nil {
		t.Fatal(err)
	}
	if !validated.Validated || !validated.RestartRequired || validated.Check == nil || validated.Check.Classification != "incompatible" {
		t.Fatalf("validate with allow_restart: %v", validated)
	}
	if data, _ := os.ReadFile(h.cfg.DBPath); string(data) != "initial\nbefore-update\n" {
		t.Fatalf("validation touched live data: %q", data)
	}

	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "succeeded" || state.Version != "2.0.0" {
		t.Fatalf("restart migration: %+v", state.Job)
	}
	if got := servedVersion(t, url); got != "2.0.0" {
		t.Fatalf("serving %q after restart migration", got)
	}
	if _, err := os.Stat(candidate.Binary + ".fresh-boot"); err != nil {
		t.Fatal("new server did not boot fresh, so it could not migrate")
	}
	data, _ := os.ReadFile(h.cfg.DBPath)
	if string(data) != "initial\nbefore-update\nmigrated by 2.0.0\n" {
		t.Fatalf("database after restart migration: %q", data)
	}
	saved, err := ReadState(h.cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Binary != candidate.Binary || saved.Version != "2.0.0" || saved.CleanStop || saved.ActiveSlot == nil || saved.PendingSlot != nil {
		t.Fatalf("committed state: binary=%s version=%s clean=%v active=%v pending=%v", saved.Binary, saved.Version, saved.CleanStop, saved.ActiveSlot != nil, saved.PendingSlot != nil)
	}
	if len(saved.Capabilities) != 0 {
		t.Fatal("capabilities were persisted to state.json")
	}
	// New requests reach the migrated server and its writes land.
	resp, err = http.Post(url, "text/plain", strings.NewReader("after-update"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if data, _ := os.ReadFile(h.cfg.DBPath); !strings.HasSuffix(string(data), "after-update\n") {
		t.Fatalf("write after restart migration: %q", data)
	}
}

func TestRestartMigrationNeedsPermission(t *testing.T) {
	_, c, url, candidate := fixture(t)
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	err := Call(context.Background(), c, "POST", "/validate", candidate, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 409") || !strings.Contains(err.Error(), "restart migration is required") {
		t.Fatalf("validate without allow_restart: %v", err)
	}
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "failed" || !strings.Contains(state.Job.Message, "restart migration is required") || state.Version != "1.0.0" {
		t.Fatalf("deploy without allow_restart: %+v", state.Job)
	}
	if got := servedVersion(t, url); got != "1.0.0" {
		t.Fatalf("serving %q", got)
	}
}

func TestRestartMigrationRestartsPreviousWhenNewServerNeverStarts(t *testing.T) {
	h, c, url, candidate := fixture(t)
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	_ = os.WriteFile(candidate.Binary+".fail-standby", nil, 0600)
	candidate.AllowRestart = true
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "rolled_back" || state.Version != "1.0.0" || !strings.Contains(state.Job.Message, "before it migrated") {
		t.Fatalf("failed restart migration: %+v", state.Job)
	}
	if got := servedVersion(t, url); got != "1.0.0" {
		t.Fatalf("serving %q after rollback", got)
	}
	saved, err := ReadState(h.cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Binary == candidate.Binary || saved.Version != "1.0.0" || saved.CleanStop || saved.ActiveSlot == nil {
		t.Fatalf("committed state after rollback: version=%s clean=%v", saved.Version, saved.CleanStop)
	}
	if data, _ := os.ReadFile(h.cfg.DBPath); string(data) != "initial\n" {
		t.Fatalf("database after rollback: %q", data)
	}
}

// Once the new server was activated it may have migrated the database, so the
// previous binary must not be booted against it. The host retries the new one.
func TestRestartMigrationNeverBootsPreviousOnMigratedDatabase(t *testing.T) {
	h, c, url, candidate := fixture(t)
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	_ = os.WriteFile(candidate.Binary+".fail-activation-once", nil, 0600)
	candidate.AllowRestart = true
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "recovery_required" || !strings.Contains(state.Job.Message, "previous version was not started") {
		t.Fatalf("failed restart migration: %+v", state.Job)
	}
	saved, err := ReadState(h.cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Binary != candidate.Binary || saved.Version != "2.0.0" || !saved.CleanStop {
		t.Fatalf("a later start must boot the new version: binary=%s version=%s clean=%v", saved.Binary, saved.Version, saved.CleanStop)
	}
	if data, _ := os.ReadFile(h.cfg.DBPath); !strings.HasPrefix(string(data), "initial\nmigrated by 2.0.0\n") {
		t.Fatalf("database: %q", data)
	}
	// The failure was transient: the host's controller recovery boots the new
	// version again, never the previous one, and closes the deployment.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var s State
		if err := Call(context.Background(), c, "GET", "/status", nil, &s); err != nil {
			t.Fatal(err)
		}
		if s.Job.Phase == "succeeded" && s.Version == "2.0.0" && !s.CleanStop {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("host did not recover onto the new version: %+v", s.Job)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := servedVersion(t, url); got != "2.0.0" {
		t.Fatalf("serving %q after recovery", got)
	}
}

func TestStatusReportsRestartMigration(t *testing.T) {
	_, c, _, _ := fixture(t)
	var s State
	if err := Call(context.Background(), c, "GET", "/status", nil, &s); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(s.Capabilities) != fmt.Sprint([]string{CapabilityRestartMigration}) {
		t.Fatalf("capabilities: %v", s.Capabilities)
	}
}

func TestHostRefusesServerWithoutControlToken(t *testing.T) {
	h, c, url, candidate := fixture(t)
	legacy := filepath.Join(h.cfg.Home, "versions", "0.100.13", "apteva-server")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(candidate.Binary)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), candidate.Binary, legacy, 1))
	if err := os.WriteFile(legacy, script, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(legacy+".legacy-protocol", nil, 0600)
	if err := Call(context.Background(), c, "POST", "/deploy", Candidate{Binary: legacy, Version: "0.100.13"}, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "failed" || !strings.Contains(state.Job.Message, "predates the host control token") {
		t.Fatalf("legacy server deploy: %+v", state.Job)
	}
	if got := servedVersion(t, url); got != "1.0.0" {
		t.Fatalf("serving %q", got)
	}
}

func TestSendsControlToken(t *testing.T) {
	for _, tc := range []struct {
		version      string
		capabilities []string
		want         bool
	}{
		{"0.100.13", nil, false},
		{"0.100.14", nil, true},
		{"v0.100.14", nil, true},
		{"0.100.14-rc.1", nil, true},
		{"0.101.0", nil, true},
		{"1.0.0", nil, true},
		{"0.99.200", nil, false},
		{"dev", nil, false},
		{"dev", []string{CapabilityControlToken}, true},
		{"0.100.10", []string{"other", CapabilityControlToken}, true},
	} {
		if got := sendsControlToken(tc.version, tc.capabilities); got != tc.want {
			t.Errorf("sendsControlToken(%q, %v) = %v, want %v", tc.version, tc.capabilities, got, tc.want)
		}
	}
}

// A host stopped mid-migration has already committed the new binary with no
// slot to adopt, so its next start boots the new server fresh and the
// migration continues; it never boots the previous binary on a half-migrated
// database.
func TestHostStoppedMidMigrationResumesWithNewServer(t *testing.T) {
	h, c, url, candidate, stopped := fixtureWithStopped(t)
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	h.cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("host did not stop")
	}
	c.CloseIdleConnections()
	state, err := ReadState(h.cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	// What restartMigration journals between stopping the old server and the
	// new one becoming ready.
	state.Binary, state.Version, state.CleanStop = candidate.Binary, candidate.Version, true
	state.ActiveSlot, state.PendingSlot = nil, nil
	state.Job = &Deployment{ID: "mid-migration", Phase: "migrating", Version: candidate.Version, Previous: "1.0.0", UpdatedAt: time.Now().UTC()}
	if err := WriteState(StatePath(h.cfg.Home), state); err != nil {
		t.Fatal(err)
	}
	replacement, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- replacement.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if job := replacement.Snapshot().Job; job != nil && Terminal(job.Phase) {
			if job.Phase != "succeeded" {
				t.Fatalf("recovered job: %+v", job)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not recover")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := servedVersion(t, url); got != "2.0.0" {
		t.Fatalf("serving %q after recovery", got)
	}
	if _, err := os.Stat(candidate.Binary + ".fresh-boot"); err != nil {
		t.Fatal("new server was not booted fresh, so it could not finish migrating")
	}
}

// restartedHost stops the fixture host cleanly, lets prepare adjust the files
// it leaves behind, then starts a new host on the same installation.
func restartedHost(t *testing.T, prepare func(home string, h *Host)) (*Host, string) {
	t.Helper()
	h, c, url, _, stopped := fixtureWithStopped(t)
	h.cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("host did not stop")
	}
	c.CloseIdleConnections()
	prepare(h.cfg.Home, h)
	replacement, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- replacement.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s := replacement.Snapshot(); s.ActiveSlot != nil && (s.Job == nil || Terminal(s.Job.Phase)) {
			return replacement, url
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeNextServer(t *testing.T, home, version string) string {
	t.Helper()
	binary := filepath.Join(home, "versions", version, "apteva-server")
	raw, _ := json.Marshal(Candidate{Binary: binary, Version: version})
	if err := os.WriteFile(NextServerPath(home), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return binary
}

// `apteva update --restart` through an older host restarts the service onto
// the new release; the new host boots the requested server fresh.
func TestHostStartBootsRequestedServerFresh(t *testing.T) {
	var binary string
	h, url := restartedHost(t, func(home string, _ *Host) {
		binary = writeNextServer(t, home, "2.0.0")
		_ = os.WriteFile(binary+".incompatible", nil, 0600)
	})
	if got := servedVersion(t, url); got != "2.0.0" {
		t.Fatalf("serving %q", got)
	}
	if _, err := os.Stat(binary + ".fresh-boot"); err != nil {
		t.Fatal("requested server was not booted fresh")
	}
	if data, _ := os.ReadFile(h.cfg.DBPath); string(data) != "initial\nmigrated by 2.0.0\n" {
		t.Fatalf("database: %q", data)
	}
	if _, err := os.Stat(NextServerPath(h.cfg.Home)); !os.IsNotExist(err) {
		t.Fatal("request was not removed once the server served")
	}
	saved, err := ReadState(h.cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Binary != binary || saved.Version != "2.0.0" || saved.Job == nil || saved.Job.Phase != "succeeded" || saved.Job.Previous != "1.0.0" {
		t.Fatalf("committed state: binary=%s version=%s job=%+v", saved.Binary, saved.Version, saved.Job)
	}
}

func TestHostStartIgnoresUnsafeServerRequests(t *testing.T) {
	for name, prepare := range map[string]func(home string, h *Host){
		"unclean stop": func(home string, _ *Host) {
			writeNextServer(t, home, "2.0.0")
			state, err := ReadState(home)
			if err != nil {
				t.Fatal(err)
			}
			state.CleanStop = false
			if err := WriteState(StatePath(home), state); err != nil {
				t.Fatal(err)
			}
		},
		"outside the version directory": func(home string, _ *Host) {
			outside := filepath.Join(home, "elsewhere", "apteva-server")
			_ = os.MkdirAll(filepath.Dir(outside), 0700)
			_ = os.WriteFile(outside, []byte("#!/bin/sh\nexit 1\n"), 0700)
			raw, _ := json.Marshal(Candidate{Binary: outside, Version: "2.0.0"})
			_ = os.WriteFile(NextServerPath(home), raw, 0600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, url := restartedHost(t, prepare)
			if got := servedVersion(t, url); got != "1.0.0" {
				t.Fatalf("serving %q", got)
			}
			if _, err := os.Stat(NextServerPath(h.cfg.Home)); !os.IsNotExist(err) {
				t.Fatal("unsafe request was kept for a later start")
			}
		})
	}
}

// A release whose database changes the check found compatible may fall back
// to the previous server even after it began migrating. No backup is kept.
func TestRestartMigrationRestartsPreviousWhenChangesAreCompatible(t *testing.T) {
	h, c, url, candidate := fixture(t)
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	_ = os.WriteFile(candidate.Binary+".compatible", nil, 0600)
	_ = os.WriteFile(candidate.Binary+".fail-activation-once", nil, 0600)
	candidate.AllowRestart = true
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "rolled_back" || state.Version != "1.0.0" || !strings.Contains(state.Job.Message, "compatible with the previous version") {
		t.Fatalf("failed compatible migration: %+v", state.Job)
	}
	if state.Job.Check == nil || state.Job.Check.Classification != "compatible" || state.Job.Check.Backup != "" {
		t.Fatalf("check summary: %+v", state.Job.Check)
	}
	if got := servedVersion(t, url); got != "1.0.0" {
		t.Fatalf("serving %q after rollback", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.cfg.Home, "update-backups")); len(entries) != 0 {
		t.Fatalf("a compatible release left %d backup(s)", len(entries))
	}
	assertNoPreflightLeftovers(t, h.cfg.Home)
}

// An incompatible release is backed up once and the backup is named when the
// migration needs recovery. A failed update prunes nothing.
func TestRestartMigrationBacksUpIncompatibleRelease(t *testing.T) {
	h, c, _, candidate := fixture(t)
	stale := filepath.Join(h.cfg.Home, "update-backups", "host-old")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	_ = os.WriteFile(candidate.Binary+".fail-activation-once", nil, 0600)
	candidate.AllowRestart = true
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "recovery_required" || !strings.Contains(state.Job.Message, "A backup taken before the update is at ") {
		t.Fatalf("failed incompatible migration: %+v", state.Job)
	}
	if state.Job.Check == nil || state.Job.Check.Backup == "" {
		t.Fatalf("check summary: %+v", state.Job.Check)
	}
	if data, err := os.ReadFile(state.Job.Check.Backup); err != nil || string(data) != "backup" {
		t.Fatalf("backup: %q %v", data, err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("a failed update removed an earlier backup: %v", err)
	}
	assertNoPreflightLeftovers(t, h.cfg.Home)
}

// Older backups are pruned only once an update with its own backup succeeds.
func TestSucceededIncompatibleUpdatePrunesOlderBackups(t *testing.T) {
	h, c, url, candidate := fixture(t)
	stale := filepath.Join(h.cfg.Home, "update-backups", "host-old")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	candidate.AllowRestart = true
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "succeeded" || state.Job.Check == nil || state.Job.Check.Backup == "" {
		t.Fatalf("incompatible update: %+v %+v", state.Job, state.Job.Check)
	}
	if got := servedVersion(t, url); got != "2.0.0" {
		t.Fatalf("serving %q", got)
	}
	if data, err := os.ReadFile(state.Job.Check.Backup); err != nil || string(data) != "backup" {
		t.Fatalf("backup: %q %v", data, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("older backup kept after a successful update with its own backup")
	}
	assertNoPreflightLeftovers(t, h.cfg.Home)
}

// A deployment refused because it needs a restart migration it was not
// allowed keeps the earlier backup and leaves no new one behind.
func TestRefusedRestartMigrationKeepsEarlierBackup(t *testing.T) {
	h, c, url, candidate := fixture(t)
	stale := filepath.Join(h.cfg.Home, "update-backups", "host-old")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(stale, "apteva.db"), []byte("earlier"), 0600)
	_ = os.WriteFile(candidate.Binary+".incompatible", nil, 0600)
	if err := Call(context.Background(), c, "POST", "/deploy", candidate, nil); err != nil {
		t.Fatal(err)
	}
	state := waitJob(t, c)
	if state.Job.Phase != "failed" || !strings.Contains(state.Job.Message, "restart migration is required") {
		t.Fatalf("refused update: %+v", state.Job)
	}
	if got := servedVersion(t, url); got != "1.0.0" {
		t.Fatalf("serving %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(h.cfg.Home, "update-backups"))
	if len(entries) != 1 || entries[0].Name() != "host-old" {
		t.Fatalf("backups after a refused update: %v", entries)
	}
	if data, err := os.ReadFile(filepath.Join(stale, "apteva.db")); err != nil || string(data) != "earlier" {
		t.Fatalf("earlier backup: %q %v", data, err)
	}
	assertNoPreflightLeftovers(t, h.cfg.Home)
}

func assertNoPreflightLeftovers(t *testing.T, home string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "host", "preflight"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("check work directory left behind: %s", entry.Name())
		}
	}
}
