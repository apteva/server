package main

import "testing"

func TestManagementBindDefaultsToLoopback(t *testing.T) {
	// Regardless of uid: an unset APTEVA_BIND never means 0.0.0.0.
	if bind, warning := managementBind(""); bind != "127.0.0.1" || warning != "" {
		t.Fatalf("default bind = %q, %q", bind, warning)
	}
	for _, loopback := range []string{"127.0.0.1", "::1", "localhost", "127.0.0.2"} {
		if bind, warning := managementBind(loopback); bind != loopback || warning != "" {
			t.Fatalf("%s: bind %q warning %q", loopback, bind, warning)
		}
	}
	for _, public := range []string{"0.0.0.0", "::", "203.0.113.4"} {
		if bind, warning := managementBind(public); bind != public || warning == "" {
			t.Fatalf("%s: bind %q warning %q", public, bind, warning)
		}
	}
}
