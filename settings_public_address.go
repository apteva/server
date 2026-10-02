package main

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This authenticated read surface intentionally excludes credentials, provider
// connections, proxy ranges, certificate material, DNS records and raw errors.
func (s *Server) handlePublicAddressSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	source := "unset"
	if s.store.GetSetting("public_url") != "" {
		source = "db"
	} else if s.publicURL != "" {
		source = "env"
	}
	address := s.publicBaseURL()
	parsed, _ := url.Parse(address)
	secure := parsed != nil && parsed.Scheme == "https"
	phase, verified := "unconfigured", false
	var updatedAt *time.Time
	if n := s.instanceHTTPS; n != nil {
		n.mu.RLock()
		phase = n.state.Phase
		if !n.state.UpdatedAt.IsZero() {
			value := n.state.UpdatedAt
			updatedAt = &value
		}
		verified = secure && n.state.Active != nil && strings.EqualFold(strings.TrimRight(address, "/"), "https://"+n.state.Active.Hostname) && phase == "active"
		n.mu.RUnlock()
	}
	writeJSON(w, map[string]any{
		"public_url": address, "configured": source != "unset", "source": source,
		"https": secure, "verified": verified, "phase": phase, "updated_at": updatedAt,
		"can_manage": s.isAdmin(getUserID(r)),
	})
}
