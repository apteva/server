package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

const crmContributionsManifest = `
schema: apteva-app/v1
name: crm
version: 1.0.0
provides:
  http_routes:
    - prefix: /exports/
  permissions:
    - name: crm.read
  exports:
    - id: overview
      contract: apteva.summary/v1
      label: CRM overview
      endpoint: /exports/overview
      permissions: [crm.read]
    - id: tickets
      contract: apteva.entities/v1
      label: Tickets
      endpoint: /exports/tickets
`

type contributionsFixture struct {
	s         *Server
	crmID     int64
	worldsID  int64
	upstream  *httptest.Server
	lastQuery string
	lastAuth  string
	lastPath  string
	lastHdrs  http.Header
}

// newContributionsFixture registers a running crm producer and a consumer
// ("worlds") holding platform.contributions.read, both in proj-1.
func newContributionsFixture(t *testing.T, consumerPerms ...sdk.Permission) *contributionsFixture {
	t.Helper()
	f := &contributionsFixture{s: newTestServer(t)}
	f.s.installedApps = NewInstalledAppsRegistry()
	ensureTestAdmin(t, f.s)
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath, f.lastQuery, f.lastAuth, f.lastHdrs = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Clone()
		switch r.URL.Path {
		case "/exports/overview":
			_, _ = io.WriteString(w, `{"contract":"apteva.summary/v1","sections":[]}`)
		case "/exports/tickets":
			http.Error(w, "export access denied", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.upstream.Close)

	crm, err := sdk.ParseManifest([]byte(crmContributionsManifest))
	if err != nil {
		t.Fatal(err)
	}
	f.crmID = seedInstallWithBindings(t, f.s, "crm", *crm, nil)
	f.s.installedApps.Add(&InstalledApp{
		InstallID: f.crmID, AppName: "crm", ProjectID: "proj-1",
		Manifest: *crm, SidecarURL: f.upstream.URL, Token: "crm-token",
	})
	if consumerPerms == nil {
		consumerPerms = []sdk.Permission{sdk.PermContributionsRead}
	}
	worlds := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "worlds", Version: "1.0.0", Requires: sdk.Requires{Permissions: consumerPerms}}
	f.worldsID = seedInstallWithBindings(t, f.s, "worlds", worlds, nil)
	f.s.installedApps.Add(&InstalledApp{
		InstallID: f.worldsID, AppName: "worlds", ProjectID: "proj-1",
		Manifest: worlds, SidecarURL: "http://127.0.0.1:1", Token: "worlds-token",
	})
	return f
}

func (f *contributionsFixture) call(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(f.worldsID))
	rec := httptest.NewRecorder()
	f.s.handleAppCallback(rec, req)
	return rec
}

func TestContributions_DiscoverFiltersByContract(t *testing.T) {
	f := newContributionsFixture(t)
	rec := f.call(http.MethodGet, "/apps/callback/contributions?project_id=proj-1&kind=export&contract=apteva.summary/v1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page sdk.ContributionPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if err := page.Validate(); err != nil {
		t.Fatalf("page fails SDK validation: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].InstallID != f.crmID || page.Items[0].ID != "overview" || page.Items[0].App != "crm" {
		t.Fatalf("items=%+v", page.Items)
	}

	rec = f.call(http.MethodGet, "/apps/callback/contributions?project_id=proj-1&limit=1", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor != "1" {
		t.Fatalf("first page=%+v", page)
	}
	rec = f.call(http.MethodGet, "/apps/callback/contributions?project_id=proj-1&limit=1&cursor=1", "")
	page = sdk.ContributionPage{}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "tickets" || page.NextCursor != "" {
		t.Fatalf("second page=%+v", page)
	}
}

func TestContributions_RequiresPermissionAndProject(t *testing.T) {
	f := newContributionsFixture(t, sdk.PermAppsCall)
	if rec := f.call(http.MethodGet, "/apps/callback/contributions?project_id=proj-1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("without permission: status=%d body=%s", rec.Code, rec.Body.String())
	}
	f = newContributionsFixture(t)
	if rec := f.call(http.MethodGet, "/apps/callback/contributions?project_id=other", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-project: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := f.call(http.MethodGet, "/apps/callback/contributions?project_id=proj-1&contract=apteva.summary/v1", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("selector without kind: status=%d", rec.Code)
	}
}

func TestContributions_HidesOtherProjectsInstalls(t *testing.T) {
	f := newContributionsFixture(t)
	f.s.installedApps.Get(f.crmID).ProjectID = "proj-2"
	rec := f.call(http.MethodGet, "/apps/callback/contributions?project_id=proj-1", "")
	var page sdk.ContributionPage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Items) != 0 {
		t.Fatalf("foreign-project producer advertised: %+v", page.Items)
	}
	rec = f.call(http.MethodPost, "/apps/callback/contributions/exports/read",
		`{"project_id":"proj-1","app_install_id":`+itoa(f.crmID)+`,"export_id":"overview"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-project read: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestContributions_ReadExportResolvesEndpointAndSwapsToken(t *testing.T) {
	f := newContributionsFixture(t)
	rec := f.call(http.MethodPost, "/apps/callback/contributions/exports/read",
		`{"project_id":"proj-1","app_install_id":`+itoa(f.crmID)+`,"export_id":"overview","limit":5,"resource":{"resource_type":"crm.ticket","resource_id":"42"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var summary sdk.SummaryExport
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil || summary.Contract != sdk.SummaryExportContract {
		t.Fatalf("payload=%s err=%v", rec.Body.String(), err)
	}
	if f.lastPath != "/exports/overview" || f.lastAuth != "Bearer crm-token" {
		t.Fatalf("upstream path=%q auth=%q", f.lastPath, f.lastAuth)
	}
	for _, want := range []string{"project_id=proj-1", "limit=5", "resource_type=crm.ticket", "resource_id=42"} {
		if !strings.Contains(f.lastQuery, want) {
			t.Fatalf("upstream query %q missing %q", f.lastQuery, want)
		}
	}
	if f.lastHdrs.Get("X-Apteva-Bound-Caller-Install-ID") != itoa(f.worldsID) || f.lastHdrs.Get(sdk.HeaderBoundCallerAppName) != "worlds" {
		t.Fatalf("caller identity headers=%v", f.lastHdrs)
	}

	rec = f.call(http.MethodPost, "/apps/callback/contributions/exports/read",
		`{"project_id":"proj-1","app_install_id":`+itoa(f.crmID)+`,"export_id":"tickets"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("producer denial: status=%d", rec.Code)
	}
	rec = f.call(http.MethodPost, "/apps/callback/contributions/exports/read",
		`{"project_id":"proj-1","app_install_id":`+itoa(f.crmID)+`,"export_id":"missing"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown export: status=%d", rec.Code)
	}
}
