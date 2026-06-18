package main

import (
	"encoding/json"
	"net/http"

	sdk "github.com/apteva/app-sdk"
)

func (s *Server) handleCallbackDNS(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "dns sub-path required", http.StatusBadRequest)
		return
	}
	switch parts[0] {
	case "grants":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !installHasPermission(s, installID, sdk.Permission(dnsPermRead)) &&
			!installHasPermission(s, installID, sdk.Permission(dnsPermWrite)) {
			http.Error(w, "missing permission: "+dnsPermRead, http.StatusForbidden)
			return
		}
		grants, err := s.ListPlatformDNSGrants()
		if err != nil {
			http.Error(w, "list DNS grants: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"grants": grants})
	case "records":
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
			return
		}
		if !installHasPermission(s, installID, sdk.Permission(dnsPermWrite)) {
			http.Error(w, "missing permission: "+dnsPermWrite, http.StatusForbidden)
			return
		}
		var req sdk.DNSRecordRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodDelete {
			res, err := s.DeletePlatformDNSRecord(req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, res)
			return
		}
		res, err := s.UpsertPlatformDNSRecord(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, res)
	default:
		http.Error(w, "unknown dns callback", http.StatusNotFound)
	}
}
