package main

import (
	"encoding/json"
	"net/http"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

func (s *Server) handleCallbackIngress(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "ingress sub-path required", http.StatusBadRequest)
		return
	}
	switch parts[0] {
	case "expose":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !installHasPermission(s, installID, sdk.Permission(ingressPermWrite)) {
			http.Error(w, "missing permission: "+ingressPermWrite, http.StatusForbidden)
			return
		}
		var req IngressExposeRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.OwnerInstallID = installID
		if req.OwnerKind == "" {
			req.OwnerKind = "app"
		}
		route, err := s.ExposeIngressRoute(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*route = s.ingressRouteWithCertificate(*route)
		writeJSON(w, map[string]any{"route": route})
	case "unexpose":
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
			return
		}
		if !installHasPermission(s, installID, sdk.Permission(ingressPermWrite)) {
			http.Error(w, "missing permission: "+ingressPermWrite, http.StatusForbidden)
			return
		}
		host := ""
		if len(parts) > 1 {
			host = strings.Join(parts[1:], "/")
		}
		if host == "" {
			var body struct {
				Hostname string `json:"hostname"`
			}
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
			host = body.Hostname
		}
		if err := s.DeleteIngressRoute(host, installID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "routes":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !installHasPermission(s, installID, sdk.Permission(ingressPermRead)) &&
			!installHasPermission(s, installID, sdk.Permission(ingressPermWrite)) {
			http.Error(w, "missing permission: "+ingressPermRead, http.StatusForbidden)
			return
		}
		routes, err := s.ListIngressRoutes("", installID)
		if err != nil {
			http.Error(w, "list ingress routes: "+err.Error(), http.StatusInternalServerError)
			return
		}
		routes = s.ingressRoutesWithCertificates(routes)
		writeJSON(w, map[string]any{"routes": routes})
	default:
		http.Error(w, "unknown ingress callback", http.StatusNotFound)
	}
}
