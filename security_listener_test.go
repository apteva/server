package main

import (
	"os"
	"testing"
)

func TestManagementListenerAddressReportsTheBoundAddress(t *testing.T) {
	t.Setenv("APTEVA_BIND", "0.0.0.0")
	s := &Server{port: "5280", managementListenAddr: "127.0.0.1:5280"}
	if got := s.managementListenerAddress(); got != "127.0.0.1:5280" {
		t.Fatalf("bound address should win over env guesses, got %q", got)
	}
}

func TestManagementListenerAddressFallsBackLikeMain(t *testing.T) {
	s := &Server{port: "5280"}

	t.Setenv("APTEVA_BIND", "")
	t.Setenv("APTEVA_HOST_BACKEND_ADDR", "")
	t.Setenv("APTEVA_HOME", "")
	if got := s.managementListenerAddress(); got != "127.0.0.1:5280" {
		t.Fatalf("default bind should be loopback, got %q", got)
	}
	if !addressIsLoopback(s.managementListenerAddress()) {
		t.Fatal("default bind reported as public")
	}

	t.Setenv("APTEVA_HOME", t.TempDir())
	want := "127.0.0.1:5280"
	if os.Geteuid() == 0 {
		want = "0.0.0.0:5280"
	}
	if got := s.managementListenerAddress(); got != want {
		t.Fatalf("APTEVA_HOME service default: got %q want %q", got, want)
	}

	t.Setenv("APTEVA_BIND", "10.0.0.5")
	if got := s.managementListenerAddress(); got != "10.0.0.5:5280" {
		t.Fatalf("APTEVA_BIND: got %q", got)
	}

	t.Setenv("APTEVA_HOST_BACKEND_ADDR", "127.0.0.1:15280")
	if got := s.managementListenerAddress(); got != "127.0.0.1:15280" {
		t.Fatalf("host backend address should win: got %q", got)
	}
}
