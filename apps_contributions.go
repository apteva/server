package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// App contributions let one app discover another app's declared exports,
// UI components and native surfaces by contract or slot instead of by name.
// The catalog comes from the manifests of running installs visible in the
// caller's project; export reads resolve the endpoint from that manifest and
// call the producer with its own token. Producers still authorize every record.
//
//	GET  /apps/callback/contributions?project_id=&kind=&contract=&resource_type=&slot=&cursor=&limit=
//	POST /apps/callback/contributions/exports/read   body: sdk.AppExportReadRequest
//
// Both require platform.contributions.read. Being in the catalog is not a
// grant: the producer's export handler decides what the caller may see.

const contributionsDefaultLimit = 100

func (s *Server) handleCallbackContributions(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 0 || (len(parts) == 1 && parts[0] == ""):
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		s.handleContributionsDiscover(w, r)
	case len(parts) == 2 && parts[0] == "exports" && parts[1] == "read":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s.handleContributionsExportRead(w, r)
	default:
		http.NotFound(w, r)
	}
}

// contributionsCaller authenticates the calling install, checks the
// permission and resolves the project the request runs in.
func (s *Server) contributionsCaller(w http.ResponseWriter, r *http.Request, requestedProject string) (int64, int64, string, bool) {
	callerInstallID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return 0, 0, "", false
	}
	if !s.requireRuntimePermission(w, callerInstallID, sdk.PermContributionsRead) {
		return 0, 0, "", false
	}
	userID, projectID, ok := s.runtimeCallerProject(w, r, callerInstallID, requestedProject, ProjectViewer)
	if !ok {
		return 0, 0, "", false
	}
	return callerInstallID, userID, projectID, true
}

type contributionSelector struct {
	kind, contract, resourceType, slot string
}

func (sel contributionSelector) validate() error {
	switch sel.kind {
	case "":
		if sel.contract != "" || sel.resourceType != "" || sel.slot != "" {
			return errors.New("kind is required with contract, resource_type or slot")
		}
	case sdk.ContributionKindExport:
		if sel.resourceType != "" || sel.slot != "" {
			return errors.New("export selector takes contract only")
		}
	case sdk.ContributionKindComponent, sdk.ContributionKindSurface:
		if sel.contract != "" {
			return errors.New("contract applies to exports only")
		}
	default:
		return errors.New("kind must be export, component or surface")
	}
	return nil
}

func (sel contributionSelector) matches(item sdk.AppContribution) bool {
	if sel.kind != "" && item.Kind != sel.kind {
		return false
	}
	switch item.Kind {
	case sdk.ContributionKindExport:
		return sel.contract == "" || item.Export.Contract == sel.contract
	case sdk.ContributionKindComponent:
		return (sel.slot == "" || containsString(item.Component.Slots, sel.slot)) &&
			(sel.resourceType == "" || containsString(item.Component.ResourceTypes, sel.resourceType))
	case sdk.ContributionKindSurface:
		return sel.resourceType == "" && (sel.slot == "" || containsString(item.Surface.Slots, sel.slot))
	}
	return false
}

// contributionCatalog lists contributions from running installs visible to
// projectID. Exports from installs without a reachable sidecar are left out,
// since they could not be read.
func (s *Server) contributionCatalog(projectID string, sel contributionSelector) []sdk.AppContribution {
	items := []sdk.AppContribution{}
	for _, entry := range s.installedApps.ListForProject(projectID) {
		if isPlatformBackupApp(entry) {
			continue
		}
		manifest := entry.Manifest
		if len(manifest.Provides.Exports) == 0 && len(manifest.Provides.UIComponents) == 0 && len(manifest.Provides.UISurfaces) == 0 {
			continue
		}
		contributions, err := sdk.ManifestContributions(&manifest, entry.InstallID, entry.ProjectID)
		if err != nil {
			log.Printf("[CONTRIBUTIONS] skip install %d (%s): %v", entry.InstallID, entry.AppName, err)
			continue
		}
		for _, item := range contributions {
			if item.Kind == sdk.ContributionKindExport && entry.SidecarURL == "" {
				continue
			}
			if item.Validate() != nil || !sel.matches(item) {
				continue
			}
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.InstallID != b.InstallID {
			return a.InstallID < b.InstallID
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID < b.ID
	})
	return items
}

func parseContributionLimit(raw string) (int, error) {
	if raw == "" {
		return contributionsDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > sdk.AppExportMaxItems {
		return 0, errors.New("limit must be between 1 and 200")
	}
	return n, nil
}

func (s *Server) handleContributionsDiscover(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, _, projectID, ok := s.contributionsCaller(w, r, q.Get("project_id"))
	if !ok {
		return
	}
	sel := contributionSelector{kind: q.Get("kind"), contract: q.Get("contract"), resourceType: q.Get("resource_type"), slot: q.Get("slot")}
	if err := sel.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := parseContributionLimit(q.Get("limit"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The cursor is an offset into the sorted catalog; callers treat it as opaque.
	offset := 0
	if raw := q.Get("cursor"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < 0 {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
	}
	items := s.contributionCatalog(projectID, sel)
	page := sdk.ContributionPage{Items: []sdk.AppContribution{}}
	if offset < len(items) {
		end := min(offset+limit, len(items))
		page.Items = items[offset:end]
		if end < len(items) {
			page.NextCursor = strconv.Itoa(end)
		}
	}
	writeJSON(w, page)
}

func (s *Server) handleContributionsExportRead(w http.ResponseWriter, r *http.Request) {
	var req sdk.AppExportReadRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	callerInstallID, userID, projectID, ok := s.contributionsCaller(w, r, req.ProjectID)
	if !ok {
		return
	}
	if req.InstallID <= 0 || strings.TrimSpace(req.ExportID) == "" {
		http.Error(w, "app_install_id and export_id are required", http.StatusBadRequest)
		return
	}
	if req.Limit < 0 || req.Limit > sdk.AppExportMaxItems {
		http.Error(w, "limit must be between 0 and 200", http.StatusBadRequest)
		return
	}
	if req.Resource != nil {
		if _, err := req.Resource.InInstallation(req.InstallID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Installs outside the caller's project look the same as missing ones.
	target := s.installedApps.Get(req.InstallID)
	if target == nil || (target.ProjectID != "" && target.ProjectID != projectID) || isPlatformBackupApp(target) {
		http.Error(w, "export not found", http.StatusNotFound)
		return
	}
	var export *sdk.AppExport
	for i := range target.Manifest.Provides.Exports {
		if target.Manifest.Provides.Exports[i].ID == req.ExportID {
			export = &target.Manifest.Provides.Exports[i]
			break
		}
	}
	if export == nil {
		http.Error(w, "export not found", http.StatusNotFound)
		return
	}
	if target.SidecarURL == "" {
		http.Error(w, "export provider is not running", http.StatusServiceUnavailable)
		return
	}
	callerAppName := strings.TrimSpace(s.callerAppName(callerInstallID))
	if callerAppName == "" {
		http.Error(w, "calling app is not running", http.StatusUnauthorized)
		return
	}

	upstream, err := url.Parse(strings.TrimRight(target.SidecarURL, "/") + export.Endpoint)
	if err != nil {
		http.Error(w, "invalid export endpoint", http.StatusInternalServerError)
		return
	}
	params := url.Values{"project_id": {projectID}}
	if req.Cursor != "" {
		params.Set("cursor", req.Cursor)
	}
	if req.Limit > 0 {
		params.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Resource != nil {
		params.Set("resource_type", req.Resource.ResourceType)
		params.Set("resource_id", req.Resource.ResourceID)
	}
	upstream.RawQuery = params.Encode()
	out, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.String(), nil)
	if err != nil {
		http.Error(w, "invalid export endpoint", http.StatusInternalServerError)
		return
	}
	out.Header.Set("Accept", "application/json")
	setBoundAppCallHeaders(out, target, callerInstallID, callerAppName, userID, projectID)
	sdk.SetClientIPHeaders(out, resolvedClientIP(r), target.Token)

	client := *s.appCallbackClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(out)
	if err != nil {
		http.Error(w, "export provider unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		http.Error(w, "export access denied", http.StatusForbidden)
		return
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		http.Error(w, "export unavailable", http.StatusNotFound)
		return
	default:
		http.Error(w, "export provider returned HTTP "+strconv.Itoa(resp.StatusCode), http.StatusBadGateway)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, sdk.AppExportMaxBytes+1))
	if err != nil || len(data) > sdk.AppExportMaxBytes || !json.Valid(data) {
		http.Error(w, "invalid export response", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
