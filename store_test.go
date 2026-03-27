package main

import (
	"path/filepath"
	"testing"
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

	inst, err := store.CreateInstance(user.ID, "my-agent", "do stuff", "{}", "")
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
	created, _ := store.CreateInstance(user.ID, "my-agent", "directive", `{"key":"val"}`, "")

	inst, err := store.GetInstance(user.ID, created.ID)
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
	inst, _ := store.CreateInstance(alice.ID, "agent", "dir", "{}", "")

	_, err := store.GetInstance(bob.ID, inst.ID)
	if err == nil {
		t.Error("bob should not see alice's instance")
	}
}

func TestStore_ListInstances(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	store.CreateInstance(user.ID, "agent1", "dir1", "{}", "")
	store.CreateInstance(user.ID, "agent2", "dir2", "{}", "")

	instances, err := store.ListInstances(user.ID, "")
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
	inst, _ := store.CreateInstance(user.ID, "agent", "old", "{}", "")

	inst.Directive = "new directive"
	inst.Status = "running"
	inst.Port = 3211
	inst.Pid = 12345
	store.UpdateInstance(inst)

	updated, _ := store.GetInstance(user.ID, inst.ID)
	if updated.Directive != "new directive" {
		t.Errorf("expected new directive, got %s", updated.Directive)
	}
	if updated.Status != "running" {
		t.Errorf("expected running, got %s", updated.Status)
	}
	if updated.Port != 3211 {
		t.Errorf("expected port 3211, got %d", updated.Port)
	}
}

func TestStore_DeleteInstance(t *testing.T) {
	store := newTestStore(t)
	user, _ := store.CreateUser("alice@test.com", "hash")
	inst, _ := store.CreateInstance(user.ID, "agent", "dir", "{}", "")

	store.DeleteInstance(user.ID, inst.ID)

	instances, _ := store.ListInstances(user.ID, "")
	if len(instances) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(instances))
	}
}

func TestStore_DeleteInstance_WrongUser(t *testing.T) {
	store := newTestStore(t)
	alice, _ := store.CreateUser("alice@test.com", "hash")
	bob, _ := store.CreateUser("bob@test.com", "hash")
	inst, _ := store.CreateInstance(alice.ID, "agent", "dir", "{}", "")

	store.DeleteInstance(bob.ID, inst.ID)

	// Alice's instance should still exist
	instances, _ := store.ListInstances(alice.ID, "")
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
	store.CreateInstance(user.ID, "agent-a", "dir-a", "{}", projA.ID)
	store.CreateInstance(user.ID, "agent-b", "dir-b", "{}", projB.ID)

	// List with project filter
	listA, _ := store.ListInstances(user.ID, projA.ID)
	if len(listA) != 1 || listA[0].Name != "agent-a" {
		t.Errorf("project A should only see agent-a, got %v", listA)
	}

	listB, _ := store.ListInstances(user.ID, projB.ID)
	if len(listB) != 1 || listB[0].Name != "agent-b" {
		t.Errorf("project B should only see agent-b, got %v", listB)
	}

	// List all (no filter) should see both
	listAll, _ := store.ListInstances(user.ID, "")
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
