package main

import (
	"log"
	"os"
	"strings"
)

// serverDebugLoggingEnabled controls high-volume success-path diagnostics.
// Errors, denials, dropped events, and other operational warnings continue to
// use log.Printf directly and are never suppressed by this setting.
func serverDebugLoggingEnabled() bool {
	level := strings.ToLower(strings.TrimSpace(os.Getenv("APTEVA_LOG_LEVEL")))
	if level == "debug" || level == "trace" || level == "verbose" {
		return true
	}
	debug := strings.ToLower(strings.TrimSpace(os.Getenv("APTEVA_DEBUG")))
	return debug == "1" || debug == "true" || debug == "yes" || debug == "on"
}

func debugLogf(format string, args ...any) {
	if serverDebugLoggingEnabled() {
		log.Printf(format, args...)
	}
}

func shouldLogAppBusPublish(dropped ...int) bool {
	if serverDebugLoggingEnabled() {
		return true
	}
	for _, count := range dropped {
		if count > 0 {
			return true
		}
	}
	return false
}
