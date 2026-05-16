package main

import (
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// integrationOfficialManifest mirrors officialManifest but flips
// DynamicIntegrationAccess instead of DynamicAppCalls.
func integrationOfficialManifest(name string, dynamic bool) sdk.Manifest {
	return sdk.Manifest{
		Name: name,
		Runtime: sdk.Runtime{
			Kind:   "source",
			Source: &sdk.SourceSpec{Repo: "github.com/apteva/apps", Ref: "main", Entry: "mcp/" + name},
		},
		Requires: sdk.Requires{DynamicIntegrationAccess: dynamic},
	}
}

func integrationThirdPartyManifest(name string, dynamic bool) sdk.Manifest {
	return sdk.Manifest{
		Name: name,
		Runtime: sdk.Runtime{
			Kind:   "source",
			Source: &sdk.SourceSpec{Repo: "github.com/evil/x", Ref: "main", Entry: "mcp/" + name},
		},
		Requires: sdk.Requires{DynamicIntegrationAccess: dynamic},
	}
}

// TestResolveDynamicIntegration_ThirdPartyWithFlag_StaysBlocked: a
// non-apteva caller can't self-grant integration access.
func TestResolveDynamicIntegration_ThirdPartyWithFlag_StaysBlocked(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "evil-wf", "proj-1", integrationThirdPartyManifest("evil-wf", true), nil)

	ok, msg := s.resolveDynamicIntegration(caller, 17, "proj-1")
	if ok {
		t.Fatal("expected reject, got allow")
	}
	if msg != "" {
		// Not eligible should return empty msg so the existing 403
		// (with its richer diagnostic) fires unchanged.
		t.Errorf("expected empty msg for ineligible caller, got %q", msg)
	}
}

// TestResolveDynamicIntegration_OfficialNoFlag_StaysBlocked: an
// official caller that hasn't opted in stays on the strict gate.
func TestResolveDynamicIntegration_OfficialNoFlag_StaysBlocked(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "workflows", "proj-1", integrationOfficialManifest("workflows", false), nil)

	ok, msg := s.resolveDynamicIntegration(caller, 17, "proj-1")
	if ok {
		t.Fatal("expected reject without flag")
	}
	if msg != "" {
		t.Errorf("expected empty msg, got %q", msg)
	}
}

// TestResolveDynamicIntegration_OfficialWithFlag_SameProject: happy
// path. Eligible + project matches → allowed.
func TestResolveDynamicIntegration_OfficialWithFlag_SameProject(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "workflows", "proj-1", integrationOfficialManifest("workflows", true), nil)

	ok, msg := s.resolveDynamicIntegration(caller, 17, "proj-1")
	if !ok {
		t.Fatalf("expected allow, got reject (%s)", msg)
	}
}

// TestResolveDynamicIntegration_CrossProject_DistinctError: eligible
// caller but the connection lives in another project — distinct 403
// message so consumers can diagnose.
func TestResolveDynamicIntegration_CrossProject_DistinctError(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "workflows", "proj-A", integrationOfficialManifest("workflows", true), nil)

	ok, msg := s.resolveDynamicIntegration(caller, 17, "proj-B")
	if ok {
		t.Fatal("cross-project allowed — isolation broken")
	}
	if !strings.Contains(msg, "another project") {
		t.Errorf("expected 'another project' diagnostic, got %q", msg)
	}
}

// TestResolveDynamicIntegration_GlobalCaller_GlobalConn: a global-
// scope caller (project_id="") matches a global-scope connection.
// Pragmatic case — global runners (cross-project workflows) and
// shared global connections are both real shapes.
func TestResolveDynamicIntegration_GlobalCaller_GlobalConn(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "workflows", "" /* global */, integrationOfficialManifest("workflows", true), nil)

	ok, _ := s.resolveDynamicIntegration(caller, 17, "")
	if !ok {
		t.Errorf("global caller + global conn should match")
	}
}
