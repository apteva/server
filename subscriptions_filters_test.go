package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubscriptionPayloadMatchesFlatFieldsAndArrayMembership(t *testing.T) {
	sub := &Subscription{MatchJSON: `{"list_ids":2,"activity_event":"lead_captured","active":true}`}

	for _, tc := range []struct {
		name    string
		payload string
		want    bool
	}{
		{"matches scalar fields and array member", `{"list_ids":[1,2,5],"activity_event":"lead_captured","active":true}`, true},
		{"rejects absent array member", `{"list_ids":[1,5],"activity_event":"lead_captured","active":true}`, false},
		{"rejects scalar mismatch", `{"list_ids":[2],"activity_event":"other","active":true}`, false},
		{"does not coerce string array members to numbers", `{"list_ids":["2"],"activity_event":"lead_captured","active":true}`, false},
		{"rejects missing field", `{"list_ids":[2],"activity_event":"lead_captured"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := subscriptionPayloadMatches(sub, json.RawMessage(tc.payload)); got != tc.want {
				t.Fatalf("subscriptionPayloadMatches() = %v, want %v", got, tc.want)
			}
		})
	}

	if !subscriptionPayloadMatches(&Subscription{}, json.RawMessage(`{"anything":true}`)) {
		t.Fatal("subscription without filters must preserve match-all behavior")
	}
}

func TestAppEventSubscriptionFiltersPersistAndAreReturned(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)

	sub, err := s.store.CreateAppEventSubscriptionWithFilters(
		userID, 42, "Makecademy leads", "crm:contact.activity.added", "",
		"main", "project-a", []string{"contact.activity.added"},
		map[string]any{"list_ids": 2, "activity_event": "lead_captured"}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Filters["list_ids"] != 2 {
		t.Fatalf("created filters = %#v", sub.Filters)
	}

	got, err := s.store.GetSubscription(userID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Filters["list_ids"] != float64(2) || got.Filters["activity_event"] != "lead_captured" {
		t.Fatalf("persisted filters = %#v", got.Filters)
	}

	listed, err := s.store.ListSubscriptions(userID, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Filters["list_ids"] != float64(2) {
		t.Fatalf("listed subscriptions = %#v", listed)
	}
}

func TestCreateAppEventSubscriptionAcceptsFiltersAPI(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	body, _ := json.Marshal(map[string]any{
		"name":       "Makecademy leads",
		"slug":       "crm:*",
		"agent_id":   42,
		"project_id": "project-a",
		"source":     "app_event",
		"events":     []string{"contact.activity.added"},
		"filters":    map[string]any{"list_ids": 2},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleCreateSubscription(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sub Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Filters["list_ids"] != float64(2) {
		t.Fatalf("response filters = %#v", sub.Filters)
	}
}

func TestCreateAppEventSubscriptionRejectsStructuredFilterValues(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	body, _ := json.Marshal(map[string]any{
		"name":     "Invalid filters",
		"slug":     "crm:*",
		"agent_id": 42,
		"source":   "app_event",
		"filters":  map[string]any{"list_ids": []int{2, 3}},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleCreateSubscription(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAppEventSubscriptionTestReportsFilteredWithoutRunningAgent(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	sub, err := s.store.CreateAppEventSubscriptionWithFilters(
		userID, 999, "List two", "crm:contact.activity.added", "", "main",
		"project-a", []string{"contact.activity.added"}, map[string]any{"list_ids": 2}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"event":"contact.activity.added","payload":{"list_ids":[3]}}`)
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+sub.ID+"/test", body)
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleTestSubscription(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Status  string `json:"status"`
		Matched bool   `json:"matched"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "filtered" || response.Matched {
		t.Fatalf("response = %#v", response)
	}
}
