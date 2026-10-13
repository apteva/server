package main

import (
	"testing"
	"time"
)

func TestManagedRuntimeExternalOriginPolicyAndRevocation(t *testing.T) {
	s, m, _, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	d := activateRuntimeFixture(t, m, id, "activate")
	origin := "http://127.0.0.1:5397"
	if s.runtimeBrowserOriginAllowed(id, origin) {
		t.Fatal("unregistered origin accepted")
	}
	if e := s.replaceAppCORSOrigins(id, runtimeBrowserOriginsKey, []string{origin}, appCORSPreflightPlatform, false); e != nil {
		t.Fatal(e)
	}
	if !s.runtimeBrowserOriginAllowed(id, origin) || s.runtimeBrowserOriginAllowed(id+1, origin) {
		t.Fatal("origin policy crossed installations")
	}
	token := runtimeSignedBrowserToken(s, runtimeBrowserClaims{InstallID: id, UserID: 1, DeploymentID: d.ID, Generation: d.Generation, Origin: origin, Operations: []string{"read"}, ExpiresAt: time.Now().Unix() + 60, Nonce: "test"})
	if _, e := s.validateRuntimeBrowser(token, id, origin, "read"); e != nil {
		t.Fatal(e)
	}
	if e := s.replaceAppCORSOrigins(id, runtimeBrowserOriginsKey, nil, appCORSPreflightPlatform, false); e != nil {
		t.Fatal(e)
	}
	if _, e := s.validateRuntimeBrowser(token, id, origin, "read"); e == nil {
		t.Fatal("removed origin retained authorization")
	}
}
