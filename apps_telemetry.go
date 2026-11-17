package main

// The existing app telemetry endpoint now uses the same stream engine as
// authenticated API callers. Historical reads and safe inventory live beside it.
import (
	"net/http"
)

func (s *Server) handleCallbackTelemetry(w http.ResponseWriter, r *http.Request, installID int64) {
	s.handleTelemetryRead(w, r, installID, "/stream")
}
