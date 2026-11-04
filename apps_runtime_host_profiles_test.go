package main

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func hostProfileManifest(profiles ...string) sdk.Manifest {
	var m sdk.Manifest
	m.Runtime.Package = &sdk.RuntimePackage{HostProfiles: profiles}
	return m
}

func TestRuntimeHostSetupStatus(t *testing.T) {
	remote := runtimePlacement{Type: "backend"}
	approved := remote
	approved.HostSetup = &runtimeHostSetup{ApprovedProfiles: []string{"chrome-headless"}}
	advertised := runtimeBackendCaps{HostProfiles: []string{"chrome-headless"}}
	states := func(state string) *sdk.RuntimeTargetInfo {
		return &sdk.RuntimeTargetInfo{HostProfiles: map[string]string{"chrome-headless": state}}
	}
	for _, tc := range []struct {
		name       string
		manifest   sdk.Manifest
		p          runtimePlacement
		caps       runtimeBackendCaps
		info       *sdk.RuntimeTargetInfo
		install    []string
		unverified []string
		err        string
	}{
		{name: "nothing required", manifest: sdk.Manifest{}, p: remote, info: states("installable")},
		{name: "local placement", manifest: hostProfileManifest("chrome-headless"), p: runtimePlacement{Type: "local"}},
		{name: "present", manifest: hostProfileManifest("chrome-headless"), p: remote, caps: advertised, info: states("present")},
		{name: "needs approval", manifest: hostProfileManifest("chrome-headless"), p: remote, caps: advertised, info: states("installable"), err: "approve it"},
		{name: "approved", manifest: hostProfileManifest("chrome-headless"), p: approved, caps: advertised, info: states("installable"), install: []string{"chrome-headless"}},
		{name: "unsupported target", manifest: hostProfileManifest("chrome-headless"), p: approved, caps: advertised, info: states("unsupported"), err: "cannot provide"},
		{name: "backend lacks profile", manifest: hostProfileManifest("gpu-drivers"), p: remote, caps: advertised, info: states("present"), err: "cannot provide"},
		{name: "older backend", manifest: hostProfileManifest("chrome-headless"), p: remote, info: &sdk.RuntimeTargetInfo{}, unverified: []string{"chrome-headless"}},
		{name: "not inspected", manifest: hostProfileManifest("chrome-headless"), p: remote, unverified: []string{"chrome-headless"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			install, unverified, err := runtimeHostSetupStatus(tc.manifest, tc.p, tc.caps, tc.info)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if !reflect.DeepEqual(install, tc.install) || !reflect.DeepEqual(unverified, tc.unverified) {
				t.Fatalf("install=%v unverified=%v", install, unverified)
			}
		})
	}
}

func TestRuntimePlacementValidatesHostSetup(t *testing.T) {
	local := runtimePlacement{Type: "local", HostSetup: &runtimeHostSetup{ApprovedProfiles: []string{"chrome-headless"}}}
	if local.validate() == nil {
		t.Fatal("local placement accepted host setup")
	}
	remote := runtimePlacement{Type: "backend", Backend: runtimeBackendRef{1, "native"}, Target: runtimeResourceRef{1, "machine", "a"}, GatewayURL: "https://gateway.example.test"}
	remote.HostSetup = &runtimeHostSetup{ApprovedProfiles: []string{"Chrome Headless"}}
	if remote.validate() == nil {
		t.Fatal("invalid profile name accepted")
	}
	remote.HostSetup.ApprovedProfiles = []string{"chrome-headless"}
	if err := remote.validate(); err != nil {
		t.Fatal(err)
	}
	if validateRuntimeHostProfileStates(map[string]string{"chrome-headless": "maybe"}) == nil {
		t.Fatal("invalid host profile state accepted")
	}
}

func TestRuntimeRequestSendsOnlyRequiredApprovedHostProfiles(t *testing.T) {
	_, m, _, id, p := runtimeTestSetup(t)
	p.HostSetup = &runtimeHostSetup{ApprovedProfiles: []string{"chrome-headless", "gpu-drivers"}}
	d := runtimeDeployment{ID: "d1", InstallID: id, Placement: p, Manifest: hostProfileManifest("chrome-headless")}
	req, err := m.request(d, runtimeOperation{ID: "op1"}, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.ApprovedHostProfiles, []string{"chrome-headless"}) {
		t.Fatalf("approved host profiles = %v", req.ApprovedHostProfiles)
	}
	d.Manifest = sdk.Manifest{}
	if req, _ = m.request(d, runtimeOperation{ID: "op2"}, "prepare"); req.ApprovedHostProfiles != nil {
		t.Fatalf("sent approvals the release does not need: %v", req.ApprovedHostProfiles)
	}
}

func TestRuntimePreflightRequiresHostSetupApproval(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	reg, _ := m.registration(p.Backend)
	reg.Caps.TargetInspection = true
	reg.Caps.HostProfiles = []string{"chrome-headless"}
	if _, err := s.store.db.Exec(`UPDATE app_runtime_backends SET record_json=? WHERE install_id=? AND backend_id=?`, runtimeJSON(reg), reg.InstallID, reg.ID); err != nil {
		t.Fatal(err)
	}
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/inspect-target") {
			original.ServeHTTP(w, r)
			return
		}
		writeJSON(w, sdk.RuntimeTargetInfo{HostKey: "host:test", Platform: "linux-amd64", Formats: []string{"source"}, HostProfiles: map[string]string{"chrome-headless": sdk.HostProfileInstallable}})
	})
	manifest, err := installManifest(s, id)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runtime.Package = &sdk.RuntimePackage{HostProfiles: []string{"chrome-headless"}}
	_, checks := m.preflight(context.Background(), 0, "", *manifest, p)
	requireRuntimeCheck(t, checks, "host_setup_required", "blocked")
	p.HostSetup = &runtimeHostSetup{ApprovedProfiles: []string{"chrome-headless"}}
	_, checks = m.preflight(context.Background(), 0, "", *manifest, p)
	if err := firstRuntimeBlock(checks); err != nil {
		t.Fatal(err)
	}
	requireRuntimeCheck(t, checks, "host_setup_required", "passed")
	assertNoRuntimeMutation(t, s, id)
}
