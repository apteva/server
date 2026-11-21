package main

// POST /api/integration-simulation/report: the app that owns a simulated
// connection reports what it did with a hand-off (or any provider-side work
// the platform does not see) into the simulation's state. Fields are merged
// into the named record's "report" object, which tests read through the
// runtime integration state, snapshots capture, and mock expressions can
// read as record.report.*.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

const (
	// mockMaxReportBytes caps one record's merged report, as JSON.
	mockMaxReportBytes = 32 << 10
	// mockMaxReportRequest bounds the report request body.
	mockMaxReportRequest = 64 << 10
)

func (s *Server) handleIntegrationSimulationReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeMockReportError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	principal, ok := r.Context().Value(appCallbackPrincipalKey{}).(appCallbackPrincipal)
	if !ok || principal.installID <= 0 {
		writeMockReportError(w, http.StatusForbidden, "integration simulation reports come from the app that owns the simulated connection")
		return
	}
	runtime, appName, status, message := s.reportingInstallEnvironment(r, principal.installID)
	if runtime == nil {
		writeMockReportError(w, status, message)
		return
	}
	var report sdk.IntegrationSimulationReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, mockMaxReportRequest)).Decode(&report); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeMockReportError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("report is over %d bytes", mockMaxReportRequest))
			return
		}
		writeMockReportError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	report.Integration = strings.TrimSpace(report.Integration)
	report.Collection = strings.TrimSpace(report.Collection)
	report.ID = strings.TrimSpace(report.ID)
	if report.Integration == "" || report.Collection == "" || report.ID == "" || len(report.Fields) == 0 {
		writeMockReportError(w, http.StatusBadRequest, "integration, collection, id and fields are required")
		return
	}
	sim, status, message := runtime.reportingSimulation(report.Integration, appName, report.ConnectionID)
	if sim == nil {
		writeMockReportError(w, status, message)
		return
	}
	merged, status, err := sim.applyReport(report)
	if err != nil {
		writeMockReportError(w, status, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "collection": report.Collection, "id": report.ID, "report": merged})
}

func writeMockReportError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// reportingInstallEnvironment derives the caller's environment from its
// install: an environment install row, whose environment is live and runs
// that very install. The X-Apteva-Environment-Id header the SDK sends must
// agree but never decides.
func (s *Server) reportingInstallEnvironment(r *http.Request, installID int64) (*Environment, string, int, string) {
	notInEnvironment := "the calling app is not running in a live environment"
	var projectID, source string
	if err := s.store.dbQueryRowContext(r.Context(),
		`SELECT COALESCE(project_id, ''), COALESCE(source, '') FROM app_installs WHERE id = ?`, installID,
	).Scan(&projectID, &source); err != nil || source != "environment" || projectID == "" {
		return nil, "", http.StatusForbidden, notInEnvironment
	}
	if s.environments == nil {
		return nil, "", http.StatusForbidden, notInEnvironment
	}
	runtime, ok := s.environments.Get(projectID)
	if !ok {
		return nil, "", http.StatusForbidden, notInEnvironment
	}
	appName := ""
	for _, name := range runtime.InstallNames() {
		if inst, ok := runtime.Install(name); ok && inst.InstallID == installID {
			appName = name
			break
		}
	}
	if appName == "" {
		return nil, "", http.StatusForbidden, notInEnvironment
	}
	if claimed := strings.TrimSpace(r.Header.Get("X-Apteva-Environment-Id")); claimed != "" && claimed != runtime.ID {
		return nil, "", http.StatusForbidden, "X-Apteva-Environment-Id does not match the calling app's environment"
	}
	return runtime, appName, 0, ""
}

// reportingSimulation selects the simulation a report is for: the one on
// connectionID, or else the only simulation of slug app owns. Only the app
// that owns a simulated connection reports into it.
func (w *Environment) reportingSimulation(slug, app string, connectionID int64) (*integrationSimulation, int, string) {
	var found []*integrationSimulation
	for _, sim := range w.simulationList() {
		if sim.slug != slug || sim.app != app {
			continue
		}
		if connectionID > 0 && sim.connectionID != connectionID {
			continue
		}
		found = append(found, sim)
	}
	switch len(found) {
	case 0:
		if connectionID > 0 {
			return nil, http.StatusNotFound, fmt.Sprintf("app %q owns no simulated %q connection %d in this environment", app, slug, connectionID)
		}
		return nil, http.StatusNotFound, fmt.Sprintf("app %q owns no simulated %q connection in this environment", app, slug)
	case 1:
		return found[0], 0, ""
	}
	return nil, http.StatusConflict, fmt.Sprintf("app %q owns several simulated %q connections; set connection_id", app, slug)
}

// applyReport shallow-merges report.Fields into the record's "report"
// object. The collection must be declared reportable and the record must
// exist; the merged report is capped at mockMaxReportBytes.
func (sim *integrationSimulation) applyReport(report sdk.IntegrationSimulationReport) (map[string]any, int, error) {
	def := sim.mock.collections[report.Collection]
	if def == nil {
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("%s simulation has no collection %q", sim.slug, report.Collection)
	}
	if !def.reportable {
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("collection %q is not reportable", report.Collection)
	}
	fields, ok := cloneMockValue(map[string]any(report.Fields)).(map[string]any)
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("fields must be an object")
	}
	sim.mu.Lock()
	records := sim.state.Collections[report.Collection]
	if records == nil || records.Records[report.ID] == nil {
		sim.mu.Unlock()
		return nil, http.StatusNotFound, fmt.Errorf("%s %q not found", report.Collection, report.ID)
	}
	record := records.Records[report.ID]
	merged, _ := cloneMockValue(record["report"]).(map[string]any)
	if merged == nil {
		merged = map[string]any{}
	}
	for key, value := range fields {
		merged[key] = value
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		sim.mu.Unlock()
		return nil, http.StatusBadRequest, err
	}
	if len(raw) > mockMaxReportBytes {
		sim.mu.Unlock()
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("report of %s %q would be %d bytes, over the %d byte limit", report.Collection, report.ID, len(raw), mockMaxReportBytes)
	}
	record["report"] = merged
	sim.mu.Unlock()

	evidence, _ := json.Marshal(map[string]any{"collection": report.Collection, "id": report.ID, "fields": mockEvidenceValue(fields)})
	sim.recordEvidence(InterceptedCall{
		Host: mockRuntimeHost, Path: sdk.IntegrationSimulationReportPath, Method: http.MethodPost, Mocked: true,
		ReqBody: truncateEvidence(evidence), Status: http.StatusOK,
		Kind: "integration_report", Integration: sim.slug, Event: report.Event,
	})
	return cloneMockValue(merged).(map[string]any), 0, nil
}
