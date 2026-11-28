package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func eventStreamRequest(userID int64, projectID string) *http.Request {
	req := httptest.NewRequest("GET", "/app-events/_all?project_id="+projectID, nil)
	if userID != 0 {
		req.Header.Set("X-User-ID", itoa64(userID))
	}
	return req
}

// A user session may read a project's app events only as a member of that
// project. Being logged in used to be enough.
func TestEventStreamRequiresProjectMembership(t *testing.T) {
	s, member, project := newAppProxyProjectUser(t, ProjectViewer)
	outsider, err := s.store.CreateUser("event-outsider@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.store.CreateProject(outsider.ID, "Outsider Project", "", "")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		user    int64
		project string
		allowed bool
	}{
		{"viewer of the project", member, project, true},
		{"platform admin", 1, project, true},
		{"owner of their own project", outsider.ID, other.ID, true},
		{"member of another project", outsider.ID, project, false},
		{"member asking for another project", member, other.ID, false},
		{"unknown project", member, "no-such-project", false},
		{"admin and unknown project", 1, "no-such-project", false},
		{"no user", 0, project, false},
	}
	for _, c := range cases {
		err := s.checkProjectAccess(eventStreamRequest(c.user, c.project), c.project)
		if (err == nil) != c.allowed {
			t.Errorf("%s: err=%v, want allowed=%v", c.name, err, c.allowed)
		}
	}

	// A non-member cannot tell a missing project from one they are not in.
	notMember := s.checkProjectAccess(eventStreamRequest(member, other.ID), other.ID)
	missing := s.checkProjectAccess(eventStreamRequest(member, "no-such-project"), "no-such-project")
	if notMember == nil || missing == nil || notMember.Error() != missing.Error() {
		t.Fatalf("not a member: %v; missing project: %v; want the same refusal", notMember, missing)
	}
}

// The stream itself refuses a non-member before any event is sent, on both the
// single-app and the all-apps project streams.
func TestEventStreamHandlerRefusesNonMember(t *testing.T) {
	s, _, project := newAppProxyProjectUser(t, ProjectViewer)
	s.appBus = NewAppEventBus()
	outsider, err := s.store.CreateUser("event-outsider@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	s.appBus.Publish("tickets", project, 0, "ticket.created", []byte(`{"id":1}`))
	for _, app := range []string{"tickets", allAppsLaneKey} {
		req := httptest.NewRequest("GET", "/app-events/"+app+"?project_id="+project+"&since=0", nil)
		req.Header.Set("X-User-ID", itoa64(outsider.ID))
		rec := httptest.NewRecorder()
		s.handleAppEventStream(rec, req)
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "ticket.created") {
			t.Fatalf("%s stream for a non-member: status=%d body=%q", app, rec.Code, rec.Body.String())
		}
	}
}
