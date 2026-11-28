package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

type assertSubjectFixture struct {
	s                 *Server
	target            int64
	gateway, plain    int64
	gatewayToken      string
	plainToken        string
	calls             int
	principal         *sdk.TrustedPrincipal
	principalErr      error
	rawAssert, rawSub string
}

// newAssertSubjectFixture installs a target app ("mobile") and two callers
// bound to it: "api", which may assert subjects, and "media", which may not.
func newAssertSubjectFixture(t *testing.T) *assertSubjectFixture {
	t.Helper()
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	ensureTestAdmin(t, s)
	const targetToken = "mobile-token"
	t.Setenv("APTEVA_APP_TOKEN", targetToken) // lets the test verify as the target would
	f := &assertSubjectFixture{s: s}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.principal, f.principalErr = sdk.PrincipalFromRequest(r)
		f.rawAssert = r.Header.Get(sdk.HeaderAssertSubjectType) + r.Header.Get(sdk.HeaderAssertSubjectID)
		f.rawSub = r.Header.Get("X-Apteva-Subject-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
	}))
	t.Cleanup(upstream.Close)

	targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "mobile"}
	f.target = seedInstallWithBindings(t, s, "mobile", targetManifest, nil)
	s.installedApps.Add(&InstalledApp{InstallID: f.target, AppName: "mobile", ProjectID: "proj-1", Manifest: targetManifest, SidecarURL: upstream.URL, Token: targetToken})

	caller := func(name string, permissions ...sdk.Permission) (int64, string) {
		manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: name, Requires: sdk.Requires{
			Permissions: permissions, Apps: []sdk.RequiredAppRef{{Name: "mobile"}},
		}}
		id := seedInstallWithBindings(t, s, name, manifest, map[string]any{"mobile": f.target})
		token, err := s.appInstallToken(id)
		if err != nil {
			t.Fatal(err)
		}
		return id, token
	}
	f.gateway, f.gatewayToken = caller("api", sdk.PermAppsCall, sdk.PermPrincipalsAssert)
	f.plain, f.plainToken = caller("media", sdk.PermAppsCall)
	return f
}

func (f *assertSubjectFixture) do(method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	f.s.authMiddleware(f.s.handleAppCallback)(rec, req)
	return rec
}

func assertHeaders(subjectType, subjectID string) map[string]string {
	return map[string]string{sdk.HeaderAssertSubjectType: subjectType, sdk.HeaderAssertSubjectID: subjectID}
}

const assertProxyPath = "/apps/callback/apps/mobile/proxy/api/stream/s1?project_id=proj-1"
const assertCallPath = "/apps/callback/apps/mobile/call"
const assertCallBody = `{"tool":"streams_list","input":{"_project_id":"proj-1"}}`

// The target receives a signed principal naming the subject and the app that
// asserted it, on both the streaming proxy and an ordinary tool call.
func TestAssertedSubjectReachesTargetSigned(t *testing.T) {
	f := newAssertSubjectFixture(t)
	for name, send := range map[string]func() *httptest.ResponseRecorder{
		"proxy": func() *httptest.ResponseRecorder {
			return f.do(http.MethodGet, assertProxyPath, f.gatewayToken, "", assertHeaders("api_customer", "cus_42"))
		},
		"call": func() *httptest.ResponseRecorder {
			return f.do(http.MethodPost, assertCallPath, f.gatewayToken, assertCallBody, assertHeaders("api_customer", "cus_42"))
		},
	} {
		f.principal, f.principalErr = nil, nil
		before := f.calls
		rec := send()
		if rec.Code != http.StatusOK || f.calls != before+1 {
			t.Fatalf("%s: status=%d body=%q target calls=%d", name, rec.Code, rec.Body.String(), f.calls-before)
		}
		p := f.principal
		if f.principalErr != nil || p == nil {
			t.Fatalf("%s: principal=%+v err=%v", name, p, f.principalErr)
		}
		if p.SubjectType != "api_customer" || p.SubjectID != "cus_42" || p.ProjectID != "proj-1" {
			t.Fatalf("%s: principal subject/project = %+v", name, p)
		}
		if !p.Asserted() || p.AssertedByApp != "api" || p.AssertedByInstallID != f.gateway {
			t.Fatalf("%s: principal does not name the asserting app: %+v", name, p)
		}
		if p.UserID != 0 || p.Email != "" {
			t.Fatalf("%s: asserted principal carries a platform user: %+v", name, p)
		}
		if f.rawAssert != "" {
			t.Fatalf("%s: raw assertion headers reached the target: %q", name, f.rawAssert)
		}
	}
}

// An app without the permission cannot speak for anyone, and what it sends
// is refused, not forwarded without identity.
func TestAssertedSubjectNeedsPermission(t *testing.T) {
	f := newAssertSubjectFixture(t)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"proxy": f.do(http.MethodGet, assertProxyPath, f.plainToken, "", assertHeaders("api_customer", "cus_42")),
		"call":  f.do(http.MethodPost, assertCallPath, f.plainToken, assertCallBody, assertHeaders("api_customer", "cus_42")),
	} {
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(sdk.PermPrincipalsAssert)) {
			t.Fatalf("%s: status=%d body=%q, want 403 naming the permission", name, rec.Code, rec.Body.String())
		}
	}
	if f.calls != 0 {
		t.Fatalf("target was called %d times", f.calls)
	}
	// Without an assertion the same app still calls normally, with no principal.
	if rec := f.do(http.MethodGet, assertProxyPath, f.plainToken, "", nil); rec.Code != http.StatusOK || f.principal != nil || f.principalErr != nil {
		t.Fatalf("plain call: status=%d principal=%+v err=%v", rec.Code, f.principal, f.principalErr)
	}
}

// A caller cannot forge a signed principal or subject headers of its own.
func TestCallbackProxyDropsForgedIdentityHeaders(t *testing.T) {
	f := newAssertSubjectFixture(t)
	forged := map[string]string{
		sdk.HeaderTrustedPrincipal:          "eyJ2ZXJzaW9uIjoxfQ",
		sdk.HeaderTrustedPrincipalSignature: "00",
		"X-Apteva-Subject-Type":             "api_customer",
		"X-Apteva-Subject-ID":               "cus_999",
	}
	for _, token := range []string{f.gatewayToken, f.plainToken} {
		rec := f.do(http.MethodGet, assertProxyPath, token, "", forged)
		if rec.Code != http.StatusOK || f.principal != nil || f.principalErr != nil || f.rawSub != "" {
			t.Fatalf("status=%d principal=%+v err=%v subject header=%q, want no identity at the target", rec.Code, f.principal, f.principalErr, f.rawSub)
		}
	}
}

// Subjects must look like ids, and "user" is reserved for platform users.
func TestAssertedSubjectIsValidated(t *testing.T) {
	f := newAssertSubjectFixture(t)
	for name, headers := range map[string]map[string]string{
		"platform user type": assertHeaders("user", "7"),
		"upper-case user":    assertHeaders("User", "7"),
		"type with spaces":   assertHeaders("api customer", "cus_42"),
		"missing id":         {sdk.HeaderAssertSubjectType: "api_customer"},
		"missing type":       {sdk.HeaderAssertSubjectID: "cus_42"},
		"control characters": assertHeaders("api_customer", "cus\x0142"),
		"over-long id":       assertHeaders("api_customer", strings.Repeat("x", 256)),
	} {
		rec := f.do(http.MethodGet, assertProxyPath, f.gatewayToken, "", headers)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%q, want 400", name, rec.Code, rec.Body.String())
		}
	}
	if f.calls != 0 {
		t.Fatalf("target was called %d times", f.calls)
	}
}

// A batch has no per-call identity, so an assertion on it is refused.
func TestAssertedSubjectRefusedOnBatch(t *testing.T) {
	f := newAssertSubjectFixture(t)
	rec := f.do(http.MethodPost, "/apps/callback/apps/mobile/batch", f.gatewayToken, `{"calls":[{"tool":"streams_list","input":{}}]}`, assertHeaders("api_customer", "cus_42"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "batch") || f.calls != 0 {
		t.Fatalf("status=%d body=%q target calls=%d", rec.Code, rec.Body.String(), f.calls)
	}
}
