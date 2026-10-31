package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestUILayoutSurfacePatchPreservesOtherSurfacesAndRejectsStaleRevision(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("widgets@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.store.CreateProject(user.ID, "Widgets", "", "")
	if err != nil {
		t.Fatal(err)
	}

	patch := func(surface, value, revision string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"value": json.RawMessage(value)})
		req := httptest.NewRequest(http.MethodPatch, "/ui-layout/projects/"+project.ID+"/surfaces/"+surface, bytes.NewReader(body))
		req.Header.Set("X-User-ID", itoa(user.ID))
		if revision != "" {
			req.Header.Set("If-Match", `"`+revision+`"`)
		}
		rec := httptest.NewRecorder()
		s.handleUILayoutSurface(rec, req)
		return rec
	}

	first := patch("dashboard.home", `[{"id":"inbox","component":"native:inbox","size":"half"}]`, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first patch status=%d body=%s", first.Code, first.Body.String())
	}
	var firstBody struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil || firstBody.Revision != 1 {
		t.Fatalf("first response=%s err=%v", first.Body.String(), err)
	}

	second := patch("dashboard.agent_card", `[{"id":"tasks","component":"tasks:agent-tasks","size":"half"}]`, "1")
	if second.Code != http.StatusOK {
		t.Fatalf("second patch status=%d body=%s", second.Code, second.Body.String())
	}
	layout, revision := s.store.GetUserUILayoutWithRevision(user.ID)
	if revision != 2 || !bytes.Contains(layout, []byte("native:inbox")) || !bytes.Contains(layout, []byte("tasks:agent-tasks")) {
		t.Fatalf("layout=%s revision=%d", layout, revision)
	}

	stale := patch("dashboard.home", `[]`, "1")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale patch status=%d body=%s", stale.Code, stale.Body.String())
	}
	layout, revision = s.store.GetUserUILayoutWithRevision(user.ID)
	if revision != 2 || !bytes.Contains(layout, []byte("native:inbox")) {
		t.Fatalf("stale write changed layout=%s revision=%d", layout, revision)
	}
}

func TestUILayoutGlobalSurfacePatchRejectsStaleRevisionAndKeepsWidths(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("global-widgets@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	patch := func(value, revision string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"value": json.RawMessage(value)})
		req := httptest.NewRequest(http.MethodPatch, "/ui-layout/global/surfaces/dashboard.home", bytes.NewReader(body))
		req.Header.Set("X-User-ID", itoa(user.ID))
		if revision != "" {
			req.Header.Set("If-Match", `"`+revision+`"`)
		}
		rec := httptest.NewRecorder()
		s.handleUILayoutGlobalSurface(rec, req)
		return rec
	}

	first := patch(`[{"id":"a","component":"native:agents","size":"quarter"},{"id":"b","component":"native:activity","size":"three-quarter"}]`, "0")
	if first.Code != http.StatusOK || first.Header().Get("ETag") != `"1"` {
		t.Fatalf("first patch status=%d etag=%q body=%s", first.Code, first.Header().Get("ETag"), first.Body.String())
	}
	stale := patch(`[]`, "0")
	if stale.Code != http.StatusConflict || stale.Header().Get("ETag") != `"1"` {
		t.Fatalf("stale patch status=%d etag=%q body=%s", stale.Code, stale.Header().Get("ETag"), stale.Body.String())
	}
	if bad := patch(`[]`, "soon"); bad.Code != http.StatusBadRequest {
		t.Fatalf("bad If-Match status=%d", bad.Code)
	}

	layout, revision := s.store.GetUserUILayoutWithRevision(user.ID)
	if revision != 1 || !bytes.Contains(layout, []byte(`"quarter"`)) || !bytes.Contains(layout, []byte(`"three-quarter"`)) {
		t.Fatalf("layout=%s revision=%d", layout, revision)
	}
}

func TestResolvedWidgetLayoutKeepsQuarterWidths(t *testing.T) {
	document := json.RawMessage(`{"projects":{"p":{"slots":{"dashboard.home":[
		{"id":"a","component":"native:agents","size":"quarter"},
		{"id":"b","component":"native:activity","size":"three-quarter"},
		{"id":"c","component":"native:apps","size":"enormous"}]}}}}`)
	layout := resolvedDashboardHomeLayout(document, "p")
	got := []string{}
	for _, widget := range layout {
		got = append(got, widget.Size)
	}
	if len(got) != 3 || got[0] != "quarter" || got[1] != "three-quarter" || got[2] != "half" {
		t.Fatalf("sizes=%v", got)
	}
}

func TestNormalizedDashboardWidgetSizes(t *testing.T) {
	cases := []struct {
		component sdk.UIComponent
		want      string
	}{
		{sdk.UIComponent{SupportedSizes: []string{"full", "half"}}, "half,three-quarter,full"},
		{sdk.UIComponent{SupportedSizes: []string{"half", "quarter", "quarter"}}, "quarter,half"},
		{sdk.UIComponent{SupportedSizes: []string{"three-quarter"}}, "three-quarter"},
		{sdk.UIComponent{SupportedSizes: []string{"giant"}, DefaultWidth: 2}, "full"},
		{sdk.UIComponent{}, "half"},
	}
	for _, c := range cases {
		if got := strings.Join(normalizedDashboardWidgetSizes(c.component), ","); got != c.want {
			t.Fatalf("%v: got %s want %s", c.component.SupportedSizes, got, c.want)
		}
	}
}
