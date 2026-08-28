package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultTelemetryRetention = 30 * 24 * time.Hour

func telemetryRetentionFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("TELEMETRY_RETENTION"))
	if raw == "" {
		return defaultTelemetryRetention
	}
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "disabled") {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil && strings.HasSuffix(strings.ToLower(raw), "d") {
		if days, dayErr := strconv.Atoi(strings.TrimSuffix(strings.ToLower(raw), "d")); dayErr == nil {
			d = time.Duration(days) * 24 * time.Hour
			err = nil
		}
	}
	if err != nil || d < time.Hour {
		log.Printf("[TELEMETRY] invalid TELEMETRY_RETENTION=%q; using %s", raw, defaultTelemetryRetention)
		return defaultTelemetryRetention
	}
	return d
}

func (s *Server) startTelemetryRetention() {
	retention := telemetryRetentionFromEnv()
	if retention == 0 {
		log.Printf("[TELEMETRY] automatic retention disabled")
		return
	}
	cleanup := func() {
		deliveriesDeleted, err := s.store.cleanDeliveredAgentEventDeliveries(retention)
		if err != nil {
			log.Printf("[AGENT-EVENTS] delivery retention cleanup failed: %v", err)
			return
		}
		deleted, err := s.store.CleanOldTelemetry(retention)
		if err != nil {
			log.Printf("[TELEMETRY] retention cleanup failed: %v", err)
			return
		}
		if deleted > 0 {
			log.Printf("[TELEMETRY] retention removed %d event(s) older than %s", deleted, retention)
		}
		if deliveriesDeleted > 0 {
			log.Printf("[AGENT-EVENTS] retention removed %d delivered transition(s) older than %s", deliveriesDeleted, retention)
		}
	}
	cleanup()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		cleanup()
	}
}
