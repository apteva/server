package main

import (
	"os"
	"testing"
)

func requireRealAppEnvironmentTests(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-app environment test in -short mode")
	}
	if !envTruthy(os.Getenv("APTEVA_RUN_REAL_APP_TESTS")) {
		t.Skip("set APTEVA_RUN_REAL_APP_TESTS=1 to run real-app environment tests")
	}
}
