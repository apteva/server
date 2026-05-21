package main

import (
	"encoding/base64"
	"testing"
	"time"
)

// TestWorld_SeedPlan_RealStorage (gated) proves AI-seeding's execution half:
// a seed plan that calls the REAL storage app's files_upload lands a real file
// in storage's isolated DB inside the world. (The meta-agent would produce
// this plan from a plain-English instruction; here we supply it directly.)
func TestWorld_SeedPlan_RealStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("real-app world test builds the storage sidecar")
	}
	src := findAppSource(t, "storage")
	s := newWorldTestServer(t)

	world, err := s.worlds.Create(WorldSpec{
		ID:           "seed-w",
		ProjectID:    "seed-w",
		GatewayURL:   s.localGatewayURL(),
		AppSrcDirs:   map[string]string{"storage": src},
		Mode:         EdgeBlock,
		HealthBudget: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("create world: %v", err)
	}
	defer world.Stop()

	plan := []SeedCall{{
		App:  "storage",
		Tool: "files_upload",
		Input: map[string]any{
			"name":           "seed.txt",
			"content_base64": base64.StdEncoding.EncodeToString([]byte("hello from the seeder")),
		},
	}}
	results, err := s.ExecuteSeedPlan(world, plan)
	if err != nil {
		t.Fatalf("execute seed plan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 seed result, got %d", len(results))
	}

	// The seeded file is REAL state in storage's isolated DB.
	dbPath, ok := world.AppDBPath("storage")
	if !ok {
		t.Fatal("no storage DB path")
	}
	n := countRows(t, dbPath, `SELECT COUNT(*) FROM files WHERE name='seed.txt' AND deleted_at IS NULL`)
	if n != 1 {
		t.Fatalf("expected 1 seeded file row in storage's DB, got %d", n)
	}
	t.Logf("✓ seed plan drove real storage.files_upload → %d real file row in the world's isolated DB", n)
}
