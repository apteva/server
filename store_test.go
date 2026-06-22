package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestNewStoreConfiguresSQLiteBusyHandling(t *testing.T) {
	store := newTestStore(t)

	if got := store.db.Stats().MaxOpenConnections; got != sqliteMaxOpenConns {
		t.Fatalf("MaxOpenConnections=%d, want %d", got, sqliteMaxOpenConns)
	}

	var busyTimeout int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != sqliteBusyTimeoutMS {
		t.Fatalf("busy_timeout=%d, want %d", busyTimeout, sqliteBusyTimeoutMS)
	}

	var journalMode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Fatalf("journal_mode=%q, want wal", journalMode)
	}
}

// --- Users ---

func TestCreateUser(t *testing.T) {
	store := newTestStore(t)
	user, err := store.CreateUser("alice@test.com", "hash123")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != 1 {
		t.Errorf("expected ID 1, got %d", user.ID)
	}
	if user.Email != "alice@test.com" {
		t.Errorf("expected alice@test.com, got %s", user.Email)
	}
}

func TestCreateUser_Duplicate(t *testing.T) {
	store := newTestStore(t)
	store.CreateUser("alice@test.com", "hash123")
	_, err := store.CreateUser("alice@test.com", "hash456")
	if err == nil {
		t.Error("expected error for duplicate email")
	}
}

func TestGetUserByEmail(t *testing.T) {
	store := newTestStore(t)
	store.CreateUser("bob@test.com", "secrethash")

	user, err := store.GetUserByEmail("bob@test.com")
	if err != nil {
		t.Fatal(err)
	}
	if user.PasswordHash != "secrethash" {
		t.Errorf("expected secrethash, got %s", user.PasswordHash)
	}
}

func TestGetUserByEmail_NotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.GetUserByEmail("nobody@test.com")
	if err == nil {
		t.Error("expected error for missing user")
	}
}

// --- API Keys ---

func TestCreateAndLookupAPIKey(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")

	keyHash := HashAPIKey("sk-testapikey123")
	key, err := store.CreateAPIKey(user.ID, "my-key", keyHash, "sk-testapikey")
	if err != nil {
		t.Fatal(err)
	}
	if key.Name != "my-key" {
		t.Errorf("expected my-key, got %s", key.Name)
	}

	// Look up user by API key
	found, err := store.GetUserByAPIKey(keyHash)
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != user.ID {
		t.Errorf("expected user %d, got %d", user.ID, found.ID)
	}
}

func TestGetUserByAPIKey_Invalid(t *testing.T) {
	store := newTestStore(t)
	_, err := store.GetUserByAPIKey("nonexistent-hash")
	if err == nil {
		t.Error("expected error for invalid key")
	}
}

func TestListAPIKeys(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	store.CreateAPIKey(user.ID, "key1", HashAPIKey("k1"), "sk-k1")
	store.CreateAPIKey(user.ID, "key2", HashAPIKey("k2"), "sk-k2")

	keys, err := store.ListAPIKeys(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

func TestDeleteAPIKey(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	key, _ := store.CreateAPIKey(user.ID, "deleteme", HashAPIKey("dk"), "sk-dk")

	store.DeleteAPIKey(user.ID, key.ID)

	keys, _ := store.ListAPIKeys(user.ID)
	if len(keys) != 0 {
		t.Errorf("expected 0 keys after delete, got %d", len(keys))
	}
}

func TestDeleteAPIKey_WrongUser(t *testing.T) {
	store := newTestStore(t)
	alice, _ := store.CreateUser("alice@test.com", "hash")
	bob, _ := store.CreateUser("bob@test.com", "hash")
	key, _ := store.CreateAPIKey(alice.ID, "alice-key", HashAPIKey("ak"), "sk-ak")

	// Bob can't delete Alice's key
	store.DeleteAPIKey(bob.ID, key.ID)

	keys, _ := store.ListAPIKeys(alice.ID)
	if len(keys) != 1 {
		t.Errorf("bob should not be able to delete alice's key")
	}
}

// --- Instances ---

func TestStore_CreateInstance(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")

	inst, err := store.CreateAgent(user.ID, "my-agent", "do stuff", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != "my-agent" {
		t.Errorf("expected my-agent, got %s", inst.Name)
	}
	if inst.Status != "stopped" {
		t.Errorf("expected stopped, got %s", inst.Status)
	}
}

func TestStore_GetInstance(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	created, _ := store.CreateAgent(user.ID, "my-agent", "directive", "autonomous", `{"key":"val"}`, "")

	inst, err := store.GetAgent(user.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Directive != "directive" {
		t.Errorf("expected directive, got %s", inst.Directive)
	}
	if inst.Config != `{"key":"val"}` {
		t.Errorf("expected config, got %s", inst.Config)
	}
}

func TestStore_GetInstance_WrongUser(t *testing.T) {
	store := newTestStore(t)
	alice, _ := store.CreateUser("alice@test.com", "hash")
	bob, _ := store.CreateUser("bob@test.com", "hash")
	inst, _ := store.CreateAgent(alice.ID, "agent", "dir", "autonomous", "{}", "")

	_, err := store.GetAgent(bob.ID, inst.ID)
	if err == nil {
		t.Error("bob should not see alice's instance")
	}
}

func TestStore_ListInstances(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	store.CreateAgent(user.ID, "agent1", "dir1", "autonomous", "{}", "")
	store.CreateAgent(user.ID, "agent2", "dir2", "autonomous", "{}", "")

	instances, err := store.ListAgents(user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("expected 2, got %d", len(instances))
	}
}

func TestStore_UpdateInstance(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	inst, _ := store.CreateAgent(user.ID, "agent", "old", "autonomous", "{}", "")

	inst.Directive = "new directive"
	inst.Status = "running"
	inst.Port = 3211
	inst.Pid = 12345
	inst.CoreAPIKey = "core_test"
	store.UpdateAgent(inst)

	updated, _ := store.GetAgent(user.ID, inst.ID)
	if updated.Directive != "new directive" {
		t.Errorf("expected new directive, got %s", updated.Directive)
	}
	if updated.Status != "running" {
		t.Errorf("expected running, got %s", updated.Status)
	}
	if updated.Port != 3211 {
		t.Errorf("expected port 3211, got %d", updated.Port)
	}
	if updated.CoreAPIKey != "core_test" {
		t.Errorf("expected persisted core key, got %q", updated.CoreAPIKey)
	}
}

func TestStore_UpdateAgentClearsRuntimeMetadataWhenStopped(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	inst, _ := store.CreateAgent(user.ID, "agent", "old", "autonomous", "{}", "")

	inst.Status = "stopped"
	inst.Port = 3211
	inst.Pid = 12345
	inst.CoreAPIKey = "core_test"
	if err := store.UpdateAgent(inst); err != nil {
		t.Fatal(err)
	}

	updated, _ := store.GetAgent(user.ID, inst.ID)
	if updated.Port != 0 || updated.Pid != 0 || updated.CoreAPIKey != "" {
		t.Fatalf("stopped agent should not keep runtime metadata: port=%d pid=%d key=%q", updated.Port, updated.Pid, updated.CoreAPIKey)
	}
}

func TestStore_UpdateAgentCoreRuntime(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	inst, _ := store.CreateAgent(user.ID, "agent", "old", "autonomous", "{}", "")
	startedAt := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)

	if err := store.UpdateAgentCoreRuntime(inst.ID, "0.12.3", "2026-06-22T09:59:00Z", startedAt); err != nil {
		t.Fatal(err)
	}

	updated, _ := store.GetAgent(user.ID, inst.ID)
	if updated.CoreVersion != "0.12.3" || updated.CoreBuildTime != "2026-06-22T09:59:00Z" {
		t.Fatalf("runtime version not persisted: %+v", updated)
	}
	if updated.CoreStartedAt == "" {
		t.Fatalf("expected core_started_at to be persisted")
	}
}

func TestStore_MarkPlatformAgentsStoppedForShutdown(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	runningUser, _ := store.CreateAgent(user.ID, "running", "dir", "autonomous", "{}", "")
	runningUser.Status = "running"
	runningUser.Port = 3210
	runningUser.Pid = 123
	if err := store.UpdateAgent(runningUser); err != nil {
		t.Fatal(err)
	}
	stoppedUser, _ := store.CreateAgent(user.ID, "stopped", "dir", "autonomous", "{}", "")
	platform, _ := store.CreateAgent(user.ID, "helper", "dir", "autonomous", "{}", "")
	platform.Status = "running"
	platform.Port = 3211
	platform.Pid = 456
	if err := store.UpdateAgent(platform); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE agents SET kind='platform_helper' WHERE id=?`, platform.ID); err != nil {
		t.Fatal(err)
	}

	n, err := store.MarkPlatformAgentsStoppedForShutdown()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RowsAffected=%d, want 1", n)
	}

	gotRunning, _ := store.GetAgent(user.ID, runningUser.ID)
	if gotRunning.Status != "running" || gotRunning.Port != 3210 || gotRunning.Pid != 123 {
		t.Fatalf("running user should remain resumable: %+v", gotRunning)
	}
	gotStopped, _ := store.GetAgent(user.ID, stoppedUser.ID)
	if gotStopped.Status != "stopped" {
		t.Fatalf("already stopped user changed unexpectedly: %+v", gotStopped)
	}
	gotPlatform, _ := store.GetAgentByID(platform.ID)
	if gotPlatform.Status != "stopped" || gotPlatform.Port != 0 || gotPlatform.Pid != 0 {
		t.Fatalf("platform helper should be marked stopped for lazy restart: %+v", gotPlatform)
	}

	runningRows, err := store.ListAgentsByStatus("running")
	if err != nil {
		t.Fatal(err)
	}
	if len(runningRows) != 1 || runningRows[0].ID != runningUser.ID {
		t.Fatalf("expected only user agent to remain running, got %+v", runningRows)
	}
}

func TestStore_DeleteInstance(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	inst, _ := store.CreateAgent(user.ID, "agent", "dir", "autonomous", "{}", "")

	store.DeleteAgent(user.ID, inst.ID)

	instances, _ := store.ListAgents(user.ID, "")
	if len(instances) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(instances))
	}
}

func TestStore_DeleteInstance_WrongUser(t *testing.T) {
	store := newTestStore(t)
	alice, _ := store.CreateUser("alice@test.com", "hash")
	bob, _ := store.CreateUser("bob@test.com", "hash")
	inst, _ := store.CreateAgent(alice.ID, "agent", "dir", "autonomous", "{}", "")

	store.DeleteAgent(bob.ID, inst.ID)

	// Alice's instance should still exist
	instances, _ := store.ListAgents(alice.ID, "")
	if len(instances) != 1 {
		t.Errorf("bob should not delete alice's instance")
	}
}

// --- Projects ---

func TestStore_ProjectCRUD(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")

	// Create
	p, err := store.CreateProject(user.ID, "Business A", "First business", "#ff0000")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.Name != "Business A" || p.Color != "#ff0000" {
		t.Errorf("unexpected project: %+v", p)
	}

	// List
	projects, _ := store.ListProjects(user.ID)
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}

	// Get
	got, err := store.GetProject(user.ID, p.ID)
	if err != nil || got.Name != "Business A" {
		t.Errorf("GetProject: %v / %+v", err, got)
	}

	// Update
	store.UpdateProject(user.ID, p.ID, "Business A (updated)", "Updated desc", "#00ff00")
	got2, _ := store.GetProject(user.ID, p.ID)
	if got2.Name != "Business A (updated)" || got2.Color != "#00ff00" {
		t.Errorf("update failed: %+v", got2)
	}

	// Delete
	store.DeleteProject(user.ID, p.ID)
	projects2, _ := store.ListProjects(user.ID)
	if len(projects2) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(projects2))
	}
}

func TestStore_ProjectIsolation(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")

	// Create two projects
	projA, _ := store.CreateProject(user.ID, "Business A", "", "")
	projB, _ := store.CreateProject(user.ID, "Business B", "", "")

	// Create instances in each project
	store.CreateAgent(user.ID, "agent-a", "dir-a", "autonomous", "{}", projA.ID)
	store.CreateAgent(user.ID, "agent-b", "dir-b", "autonomous", "{}", projB.ID)

	// List with project filter
	listA, _ := store.ListAgents(user.ID, projA.ID)
	if len(listA) != 1 || listA[0].Name != "agent-a" {
		t.Errorf("project A should only see agent-a, got %v", listA)
	}

	listB, _ := store.ListAgents(user.ID, projB.ID)
	if len(listB) != 1 || listB[0].Name != "agent-b" {
		t.Errorf("project B should only see agent-b, got %v", listB)
	}

	// List all (no filter) should see both
	listAll, _ := store.ListAgents(user.ID, "")
	if len(listAll) != 2 {
		t.Errorf("expected 2 total instances, got %d", len(listAll))
	}
}

func TestStore_ConnectionProjectIsolation(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	projA, _ := store.CreateProject(user.ID, "Biz A", "", "")
	projB, _ := store.CreateProject(user.ID, "Biz B", "", "")

	store.CreateConnection(user.ID, "slack", "Slack", "Slack A", "bearer", "enc-a", projA.ID)
	store.CreateConnection(user.ID, "slack", "Slack", "Slack B", "bearer", "enc-b", projB.ID)

	listA, _ := store.ListConnections(user.ID, projA.ID)
	if len(listA) != 1 || listA[0].Name != "Slack A" {
		t.Errorf("project A should only see Slack A, got %v", listA)
	}

	listB, _ := store.ListConnections(user.ID, projB.ID)
	if len(listB) != 1 || listB[0].Name != "Slack B" {
		t.Errorf("project B should only see Slack B, got %v", listB)
	}

	listAll, _ := store.ListConnections(user.ID)
	if len(listAll) != 2 {
		t.Errorf("expected 2 total connections, got %d", len(listAll))
	}
}

// --- HashAPIKey ---

func TestHashAPIKey_Deterministic(t *testing.T) {
	h1 := HashAPIKey("sk-test123")
	h2 := HashAPIKey("sk-test123")
	if h1 != h2 {
		t.Error("same key should produce same hash")
	}
}

func TestHashAPIKey_Different(t *testing.T) {
	h1 := HashAPIKey("sk-key1")
	h2 := HashAPIKey("sk-key2")
	if h1 == h2 {
		t.Error("different keys should produce different hashes")
	}
}
