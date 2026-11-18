package claudepool

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPlanUsageAsksClaudeCode(t *testing.T) {
	tp := newTestPool(t, Limits{})
	ctx := context.Background()
	usage, err := tp.PlanUsage(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if usage.SubscriptionType != "max" || !usage.Available || usage.CheckedAt.IsZero() {
		t.Fatalf("usage = %+v", usage)
	}
	want := []struct {
		id, label string
		pct       float64
	}{{"five_hour", "", 45}, {"seven_day", "", 54}, {"model:fable", "Fable", 0}}
	if len(usage.Windows) != len(want) {
		t.Fatalf("windows = %+v", usage.Windows)
	}
	for i, w := range want {
		got := usage.Windows[i]
		if got.ID != w.id || got.Label != w.label || got.Utilization == nil || *got.Utilization != w.pct || got.ResetsAt == nil {
			t.Fatalf("window %d = %+v, want %+v", i, got, w)
		}
	}
	if reset := usage.Windows[0].ResetsAt; !reset.Equal(time.Date(2030, 1, 1, 10, 0, 0, 5e8, time.UTC)) {
		t.Fatalf("five_hour resets at %v", reset)
	}

	// A fresh answer is reused; the asking process saved no session.
	if _, err := tp.PlanUsage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	spawns := tp.spawns(t)
	if len(spawns) != 1 || spawns[0].Mode != "probe" || spawns[0].Session != "" || spawns[0].HasAPIKey || spawns[0].NoMDs != "1" {
		t.Fatalf("spawns = %+v", spawns)
	}
	if entries, _ := os.ReadDir(tp.projectDir()); len(entries) != 0 {
		t.Fatalf("session files left: %v", entries)
	}
	if st := tp.Status(); st.Running != 0 {
		t.Fatalf("status = %+v", st)
	}

	// When Claude Code cannot answer, the last answer comes back with the
	// error.
	t.Setenv("CLAUDEPOOL_FAKE_ASK_FAIL", "1")
	stale, err := tp.PlanUsage(ctx, 0)
	if err == nil || !stale.CheckedAt.Equal(usage.CheckedAt) || len(stale.Windows) != 3 {
		t.Fatalf("after a failure: %+v, %v", stale, err)
	}
}

func TestPlanUsageFailsWithoutAnAnswer(t *testing.T) {
	t.Setenv("CLAUDEPOOL_FAKE_ASK_FAIL", "1")
	tp := newTestPool(t, Limits{})
	usage, err := tp.PlanUsage(context.Background(), time.Minute)
	if err == nil || !usage.CheckedAt.IsZero() {
		t.Fatalf("usage = %+v, err = %v", usage, err)
	}
}

func TestModelsListsWhatClaudeCodeOffers(t *testing.T) {
	tp := newTestPool(t, Limits{})
	models, err := tp.Models(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, m := range models {
		values = append(values, m.Value)
	}
	// Disabled rows are left out.
	want := []string{"default", "opus[1m]", "claude-fable-5-1[1m]", "sonnet", "haiku"}
	if len(values) != len(want) {
		t.Fatalf("models = %v", values)
	}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("models = %v", values)
		}
	}
	if models[2].DisplayName != "Fable" || models[2].ResolvedModel != "claude-fable-5-1" {
		t.Fatalf("fable = %+v", models[2])
	}
	if _, err := tp.Models(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if spawns := tp.spawns(t); len(spawns) != 1 {
		t.Fatalf("spawns = %+v", spawns)
	}
}

func TestAskAfterCloseFails(t *testing.T) {
	tp := newTestPool(t, Limits{})
	tp.Close()
	if _, err := tp.PlanUsage(context.Background(), time.Minute); err != ErrClosed {
		t.Fatalf("err = %v", err)
	}
}

// Updating Claude Code (a new binary behind the same path) drops the
// cached model list at once.
func TestModelsRefreshWhenClaudeCodeChanges(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "claude")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	tp := newTestPoolWith(t, Limits{}, func(cfg *Config) { cfg.CLI.Binary = link })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := tp.Models(ctx, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(tp.spawns(t)); n != 1 {
		t.Fatalf("spawns before the update = %d", n)
	}

	// "Update": the symlink now points at another copy of the binary.
	updated := filepath.Join(dir, "claude-next")
	copyFile(t, exe, updated)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(updated, link); err != nil {
		t.Fatal(err)
	}
	if _, err := tp.Models(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := len(tp.spawns(t)); n != 2 {
		t.Fatalf("spawns after the update = %d", n)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
