package main

import (
	"fmt"
	"net/http"
	"strings"
)

// Several installations of one app can publish into the same project, for
// example a production and a staging install. Events carry the publishing
// installation (AppEvent.InstallID, stamped by the server). The helpers here
// let consumers keep to the installations they chose.

// appEventSubscriptionSourceProject checks that sourceInstallID is an
// installation of the slug's app that the caller may read, and returns the
// project the subscription listens in. Once a source installation is set,
// matching compares the event's project exactly, so a global source needs an
// explicit project and a project-scoped source implies its own.
func (s *Server) appEventSubscriptionSourceProject(w http.ResponseWriter, r *http.Request, userID int64, slug, projectID string, sourceInstallID int64) (string, bool) {
	app, _, ok := splitAppEventSlug(slug)
	if !ok {
		http.Error(w, "slug must be '<app>:<topic_pattern>' for app_event subscriptions", http.StatusBadRequest)
		return "", false
	}
	meta, err := s.appMetadataContext(r.Context(), sourceInstallID)
	if err != nil || meta == nil || meta.owner == 0 {
		http.Error(w, fmt.Sprintf("source installation %d not found", sourceInstallID), http.StatusBadRequest)
		return "", false
	}
	if meta.name != app {
		http.Error(w, fmt.Sprintf("source installation %d is not an installation of %s", sourceInstallID, app), http.StatusBadRequest)
		return "", false
	}
	projectID = strings.TrimSpace(projectID)
	if meta.project != "" {
		if projectID != "" && projectID != meta.project {
			http.Error(w, fmt.Sprintf("source installation %d is in another project", sourceInstallID), http.StatusBadRequest)
			return "", false
		}
		if _, _, ok := s.requireProjectAccess(w, r, meta.project, ProjectViewer); !ok {
			return "", false
		}
		return meta.project, true
	}
	if projectID == "" {
		http.Error(w, "project_id is required when source_install_id names a global installation", http.StatusBadRequest)
		return "", false
	}
	if meta.owner != userID && s.store.GetPlatformRole(userID) != PlatformAdmin {
		http.Error(w, fmt.Sprintf("source installation %d belongs to another user", sourceInstallID), http.StatusForbidden)
		return "", false
	}
	return projectID, true
}

// subscriberAcceptsEventSource applies a subscriber's bindings to events it
// subscribed to through its manifest. Among the installations of the source
// app the subscriber is bound to, only those in the event's project (or
// global ones) count. If there are any, the event must come from one of
// them; if there are none, the subscriber has not chosen an installation for
// that project and receives the event as before. When the subscriber's
// bindings cannot be read, delivery also stays as before.
func (s *Server) subscriberAcceptsEventSource(subscriberInstallID int64, ev AppEvent) bool {
	meta, err := s.appMetadata(subscriberInstallID)
	if err != nil || meta == nil {
		return true
	}
	chosen := false
	for _, id := range boundInstallationsOfApp(meta, ev.App) {
		if id == ev.InstallID {
			return true
		}
		bound, err := s.appMetadata(id)
		if err != nil || bound == nil || bound.name != ev.App {
			continue
		}
		if bound.project == "" || bound.project == ev.ProjectID {
			chosen = true
		}
	}
	return !chosen
}

// boundInstallationsOfApp lists the installation IDs a caller's bindings
// select for app, through requires.apps or an app-kind integration role that
// accepts app.
func boundInstallationsOfApp(meta *appInstallMetadata, app string) []int64 {
	if meta == nil || meta.manifestErr != nil {
		return nil
	}
	seen := map[int64]bool{}
	out := []int64{}
	add := func(raw any) {
		ids, defaultID := appBindingIDs(raw)
		if defaultID > 0 {
			ids = append(ids, defaultID)
		}
		for _, id := range ids {
			if id > 0 && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	for _, dep := range meta.manifest.Requires.Apps {
		if dep.Name == app {
			add(meta.bindings[dep.Name])
		}
	}
	for _, dep := range meta.manifest.Requires.Integrations {
		if dep.Kind == "app" && contains(dep.CompatibleAppNames, app) {
			add(meta.bindings[dep.Role])
		}
	}
	return out
}
