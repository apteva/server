package main

import "testing"

func TestAgentManagerGatewayCommandUsesOverride(t *testing.T) {
	manager := NewAgentManager(t.TempDir(), "/tmp/apteva-core")
	manager.serverCmd = "/tmp/apteva-server-under-test"
	if got := manager.gatewayCommand(); got != manager.serverCmd {
		t.Fatalf("gateway command=%q, want override %q", got, manager.serverCmd)
	}
}
