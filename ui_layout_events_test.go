package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func uiLayoutTestUser(t *testing.T, store *Store) int64 {
	t.Helper()
	user, err := store.CreateUser("layout-events@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func recordUILayoutChanges(store *Store) *[]uiLayoutChange {
	var got []uiLayoutChange
	store.uiLayoutChanged = func(change uiLayoutChange) { got = append(got, change) }
	return &got
}

func TestUILayoutChangeReportedForEverySavePath(t *testing.T) {
	store := newTestStore(t)
	got := recordUILayoutChanges(store)
	user := uiLayoutTestUser(t, store)

	page := workspacePage{ID: "abc123", Title: "Sales", Scope: "project", Kind: "custom"}
	if _, _, err := store.mutateUIPage(user, "proj", page, http.MethodPost); err != nil {
		t.Fatal(err)
	}
	page.Title = "Sales board"
	if _, _, err := store.mutateUIPage(user, "proj", page, http.MethodPatch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PatchUserUILayoutSurface(user, "proj", "page.abc123", json.RawMessage(`[{"id":"w1"}]`), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PatchUserUILayoutSurface(user, "proj", "dashboard.home", json.RawMessage(`[{"id":"w2"}]`), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PatchUserUILayoutGlobalSurface(user, "page.glob", json.RawMessage(`[{"id":"w3"}]`), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.mutateUIPage(user, "proj", page, http.MethodDelete); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserUILayout(user, json.RawMessage(`{"projects":{}}`)); err != nil {
		t.Fatal(err)
	}

	want := []uiLayoutChange{
		{UserID: user, ProjectID: "proj", Scope: "project", PageID: "abc123", Change: "created", Revision: 1},
		{UserID: user, ProjectID: "proj", Scope: "project", PageID: "abc123", Change: "updated", Revision: 2},
		{UserID: user, ProjectID: "proj", Scope: "project", PageID: "abc123", Surface: "page.abc123", Change: "updated", Revision: 3},
		{UserID: user, ProjectID: "proj", Scope: "project", Surface: "dashboard.home", Change: "updated", Revision: 4},
		{UserID: user, Scope: "global", PageID: "glob", Surface: "page.glob", Change: "updated", Revision: 5},
		{UserID: user, ProjectID: "proj", Scope: "project", PageID: "abc123", Change: "deleted", Revision: 6},
		{UserID: user, Scope: "layout", Change: "updated", Revision: 7},
	}
	if len(*got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(*got), len(want), *got)
	}
	for i, w := range want {
		if (*got)[i] != w {
			t.Errorf("change %d = %+v, want %+v", i, (*got)[i], w)
		}
	}
}

func TestUILayoutChangeNotReportedWhenNothingChanged(t *testing.T) {
	store := newTestStore(t)
	user := uiLayoutTestUser(t, store)
	page := workspacePage{ID: "abc123", Title: "Sales", Scope: "global", Kind: "custom"}
	if _, _, err := store.mutateUIPage(user, "", page, http.MethodPost); err != nil {
		t.Fatal(err)
	}
	value := json.RawMessage(`[{"id":"w1","settings":{"b":1,"a":2}}]`)
	if _, _, err := store.PatchUserUILayoutSurface(user, "proj", "page.abc123", value, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PatchUserUILayoutGlobalSurface(user, "page.abc123", value, nil); err != nil {
		t.Fatal(err)
	}
	// The first edit fills in fields a new page leaves out; later ones repeat it.
	if _, _, err := store.mutateUIPage(user, "", page, http.MethodPatch); err != nil {
		t.Fatal(err)
	}
	layout, _ := store.GetUserUILayoutWithRevision(user)

	got := recordUILayoutChanges(store)
	// The same writes again, with keys reordered: nothing changed.
	reordered := json.RawMessage(`[{"settings":{"a":2,"b":1},"id":"w1"}]`)
	if _, _, err := store.mutateUIPage(user, "", page, http.MethodPatch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PatchUserUILayoutSurface(user, "proj", "page.abc123", reordered, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PatchUserUILayoutGlobalSurface(user, "page.abc123", reordered, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserUILayout(user, layout); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Fatalf("repeat writes reported changes: %+v", *got)
	}

	// A failed write reports nothing either.
	stale := int64(1)
	if _, _, err := store.PatchUserUILayoutSurface(user, "proj", "page.abc123", json.RawMessage(`[]`), &stale); err == nil {
		t.Fatal("stale revision was accepted")
	}
	if len(*got) != 0 {
		t.Fatalf("conflicting write reported a change: %+v", *got)
	}
}

func TestPageUpdatedEventOnAppBus(t *testing.T) {
	store := newTestStore(t)
	s := &Server{store: store, appBus: NewAppEventBus()}
	store.uiLayoutChanged = s.publishUILayoutChange
	user := uiLayoutTestUser(t, store)

	project, _, cancelProject := s.appBus.Subscribe(uiLayoutEventApp, "proj", 0)
	defer cancelProject()
	other, _, cancelOther := s.appBus.Subscribe(uiLayoutEventApp, "other", 0)
	defer cancelOther()
	every, _, cancelEvery := s.appBus.Subscribe(uiLayoutEventApp, "", 0)
	defer cancelEvery()

	if _, _, err := store.PatchUserUILayoutSurface(user, "proj", "page.abc123", json.RawMessage(`[{"id":"w1","settings":{"secret":"s3"}}]`), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PatchUserUILayoutGlobalSurface(user, "page.glob", json.RawMessage(`[]`), nil); err != nil {
		t.Fatal(err)
	}

	if len(project) != 1 || len(other) != 0 || len(every) != 2 {
		t.Fatalf("delivered project=%d other=%d every=%d, want 1/0/2", len(project), len(other), len(every))
	}
	ev := <-project
	if ev.Topic != "page.updated" || ev.App != "pages" || ev.ProjectID != "proj" {
		t.Fatalf("event = %+v", ev)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"user_id": float64(user), "project_id": "proj", "scope": "project", "page_id": "abc123", "surface": "page.abc123", "change": "updated", "revision": float64(1)}
	if len(data) != len(want) {
		t.Fatalf("data = %v, want only identifiers %v", data, want)
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("data[%q] = %v, want %v", k, data[k], v)
		}
	}
}
