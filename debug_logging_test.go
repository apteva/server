package main

import "testing"

func TestServerDebugLoggingEnabled(t *testing.T) {
	t.Setenv("APTEVA_LOG_LEVEL", "")
	t.Setenv("APTEVA_DEBUG", "")
	if serverDebugLoggingEnabled() {
		t.Fatal("debug logging enabled by default")
	}

	t.Setenv("APTEVA_LOG_LEVEL", "debug")
	if !serverDebugLoggingEnabled() {
		t.Fatal("APTEVA_LOG_LEVEL=debug did not enable debug logging")
	}

	t.Setenv("APTEVA_LOG_LEVEL", "")
	t.Setenv("APTEVA_DEBUG", "true")
	if !serverDebugLoggingEnabled() {
		t.Fatal("APTEVA_DEBUG=true did not enable debug logging")
	}
}

func TestAppBusPublishLoggingKeepsDropsVisible(t *testing.T) {
	t.Setenv("APTEVA_LOG_LEVEL", "")
	t.Setenv("APTEVA_DEBUG", "")
	if shouldLogAppBusPublish(0, 0, 0) {
		t.Fatal("successful publish should be quiet outside debug mode")
	}
	if !shouldLogAppBusPublish(0, 1, 0) {
		t.Fatal("dropped publish should always be logged")
	}

	t.Setenv("APTEVA_LOG_LEVEL", "debug")
	if !shouldLogAppBusPublish(0, 0, 0) {
		t.Fatal("debug mode should log successful publishes")
	}
}
