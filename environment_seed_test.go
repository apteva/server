package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareSeedInput_LocalFixtureFile(t *testing.T) {
	root := t.TempDir()
	fixtures := filepath.Join(root, "fixtures")
	if err := os.MkdirAll(fixtures, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "sample.mp4"), []byte("video bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	input, err := prepareSeedInput(&Environment{ID: "environment-1", ProjectID: "source-project"}, SeedCall{
		App:  "storage",
		Tool: "files_upload",
		File: "./fixtures/sample.mp4",
		Input: map[string]any{
			"content_type": "video/mp4",
		},
	}, root)
	if err != nil {
		t.Fatalf("prepare seed input: %v", err)
	}
	if input["name"] != "sample.mp4" {
		t.Fatalf("name = %v, want sample.mp4", input["name"])
	}
	if input["_project_id"] != "environment-1" {
		t.Fatalf("_project_id = %v, want environment-1", input["_project_id"])
	}
	if input["content_base64"] != base64.StdEncoding.EncodeToString([]byte("video bytes")) {
		t.Fatalf("content_base64 not injected from fixture")
	}
}

func TestPrepareSeedInput_InputFileConvenience(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	input, err := prepareSeedInput(nil, SeedCall{
		Input: map[string]any{
			"file": "sample.txt",
			"name": "custom.txt",
		},
	}, root)
	if err != nil {
		t.Fatalf("prepare seed input: %v", err)
	}
	if _, ok := input["file"]; ok {
		t.Fatalf("input.file should be removed before calling the app: %#v", input)
	}
	if input["name"] != "custom.txt" {
		t.Fatalf("name = %v, want custom.txt", input["name"])
	}
	if input["content_base64"] != base64.StdEncoding.EncodeToString([]byte("hello")) {
		t.Fatalf("content_base64 not injected from input.file")
	}
}

func TestPrepareSeedInput_SeedResultRefs(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"{\"id\":42,\"contact\":{\"id\":7}}"}]}`)
	input, err := prepareSeedInput(nil, SeedCall{
		Input: map[string]any{
			"calendar_id": map[string]any{"$ref": "0.id"},
			"nested": map[string]any{
				"contact_id": map[string]any{"$ref": "0.contact.id"},
			},
		},
	}, "", raw)
	if err != nil {
		t.Fatalf("prepare seed input: %v", err)
	}
	if input["calendar_id"] != float64(42) {
		t.Fatalf("calendar_id = %#v, want 42", input["calendar_id"])
	}
	nested := input["nested"].(map[string]any)
	if nested["contact_id"] != float64(7) {
		t.Fatalf("nested.contact_id = %#v, want 7", nested["contact_id"])
	}
}

func TestResolveSeedFixturePathRequiresBaseAndRejectsEscape(t *testing.T) {
	if _, err := resolveSeedFixturePath("", "sample.mp4"); err == nil {
		t.Fatalf("expected missing seed_base_dir to fail")
	}
	root := t.TempDir()
	if _, err := resolveSeedFixturePath(root, "../sample.mp4"); err == nil {
		t.Fatalf("expected escaping seed_base_dir to fail")
	}
}

// TestEnvironment_SeedPlan_RealStorage (gated) proves AI-seeding's execution half:
// a seed plan that calls the REAL storage app's files_upload lands a real file
// in storage's isolated DB inside the environment. (The meta-agent would produce
// this plan from a plain-English instruction; here we supply it directly.)
func TestEnvironment_SeedPlan_RealStorage(t *testing.T) {
	requireRealAppEnvironmentTests(t)
	src := findAppSource(t, "storage")
	s := newEnvironmentTestServer(t)

	environment, err := s.environments.Create(EnvironmentSpec{
		ID:           "seed-w",
		ProjectID:    "seed-w",
		GatewayURL:   s.localGatewayURL(),
		AppSrcDirs:   map[string]string{"storage": src},
		Mode:         EdgeBlock,
		HealthBudget: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	defer environment.Stop()

	plan := []SeedCall{{
		App:  "storage",
		Tool: "files_upload",
		Input: map[string]any{
			"name":           "seed.txt",
			"content_base64": base64.StdEncoding.EncodeToString([]byte("hello from the seeder")),
		},
	}}
	results, err := s.ExecuteSeedPlan(environment, plan)
	if err != nil {
		t.Fatalf("execute seed plan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 seed result, got %d", len(results))
	}

	// The seeded file is REAL state in storage's isolated DB.
	dbPath, ok := environment.AppDBPath("storage")
	if !ok {
		t.Fatal("no storage DB path")
	}
	n := countRows(t, dbPath, `SELECT COUNT(*) FROM files WHERE name='seed.txt' AND deleted_at IS NULL`)
	if n != 1 {
		t.Fatalf("expected 1 seeded file row in storage's DB, got %d", n)
	}
	t.Logf("✓ seed plan drove real storage.files_upload → %d real file row in the environment's isolated DB", n)
}
