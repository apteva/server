package main

// Hand-offs: provider work a simulation gives to the app that owns the
// simulated connection, for provider traffic in a protocol the platform does
// not speak itself (SIP, for example). A `handoff` step queues one; it is
// delivered like a webhook (asynchronously, in lane order, retried) but as
// an sdk.IntegrationSimulationEvent POSTed to sdk.IntegrationSimulationPath
// on the owning install, in-process through the same runtime proxy
// webhooks use. The app reports what happened back into the record the
// hand-off named (integration_mock_report.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const (
	// mockMaxHandoffBody is the SDK's hand-off request limit.
	mockMaxHandoffBody = 1 << 20
	// Evidence keeps strings up to this size; longer ones (inlined audio)
	// are replaced by their length.
	mockMaxEvidenceString = 1024
	// mockHandoffFailedState is written to report.state when a hand-off
	// fails for good.
	mockHandoffFailedState = "handoff_failed"
)

// mockHandoffFunc posts a hand-off body to app, returning its status and
// (bounded) response body.
type mockHandoffFunc func(ctx context.Context, app string, body []byte) (int, []byte, error)

// mockHandoffTarget is what a hand-off asks the app to do, and the record
// it concerns.
type mockHandoffTarget struct {
	kind       string
	collection string
	id         string
}

// mockStepFailure is a step condition failure (rather than a spec or
// rendering error): it answers with the step's error, when it has one.
type mockStepFailure struct{ detail string }

func (e *mockStepFailure) Error() string { return e.detail }

func mockHandoffPath(app string) string {
	return "/api/apps/" + url.PathEscape(app) + sdk.IntegrationSimulationPath
}

// handoffEvent renders a handoff step into a pending event.
func (sim *integrationSimulation) handoffEvent(sc *mockScope, step *mockStep, work *mockState) (mockPendingEvent, error) {
	render := func(t mockTemplate) (string, error) {
		if t == nil {
			return "", nil
		}
		value, err := t.render(sc, &sim.mock.templates)
		if err != nil {
			return "", err
		}
		return mockString(value), nil
	}
	kind, err := render(step.handoffKind)
	if err != nil {
		return mockPendingEvent{}, err
	}
	if kind = strings.TrimSpace(kind); kind == "" {
		return mockPendingEvent{}, fmt.Errorf("handoff kind is empty")
	}
	target := &mockHandoffTarget{kind: kind}
	if step.handoffCollection != nil {
		collection, err := render(step.handoffCollection)
		if err != nil {
			return mockPendingEvent{}, err
		}
		if sim.mock.collections[collection] == nil {
			return mockPendingEvent{}, fmt.Errorf("handoff: unknown collection %q", collection)
		}
		id, err := render(step.handoffID)
		if err != nil {
			return mockPendingEvent{}, err
		}
		if records := work.Collections[collection]; id == "" || records == nil || records.Records[id] == nil {
			return mockPendingEvent{}, &mockStepFailure{detail: fmt.Sprintf("%s %q not found", collection, id)}
		}
		target.collection, target.id = collection, id
	}
	lane, err := render(step.lane)
	if err != nil {
		return mockPendingEvent{}, err
	}
	if lane == "" {
		lane = target.id
	}
	if lane == "" {
		lane = "handoff:" + kind
	}
	payload := any(map[string]any{})
	if step.payload != nil {
		if payload, err = step.payload.render(sc, &sim.mock.templates); err != nil {
			return mockPendingEvent{}, err
		}
	}
	return mockPendingEvent{event: "handoff:" + kind, lane: lane, payload: payload, handoff: target}, nil
}

// deliverHandoff posts one hand-off, retrying transport errors and 5xx
// answers; a 4xx answer is final. When it fails for good, the record it
// names gets report.state = "handoff_failed" and report.error.
func (sim *integrationSimulation) deliverHandoff(d *mockDelivery) {
	path := mockHandoffPath(sim.app)
	fail := func(state string, status int, message string) {
		message = "handoff " + d.handoff.kind + ": " + message
		if sim.ctx.Err() == nil {
			sim.markHandoffFailed(d.handoff, message)
		}
		sim.finish(d, state, status, message)
	}
	if sim.app == "" || sim.handOff == nil {
		sim.recordEvidence(InterceptedCall{Host: mockRuntimeHost, Method: http.MethodPost, Blocked: true, Kind: "integration_handoff", Integration: sim.slug, Event: d.record.Event})
		fail("undeliverable", 0, "the simulated connection has no owning app to hand work to")
		return
	}
	payload, err := json.Marshal(d.payload)
	if err != nil {
		fail("failed", 0, err.Error())
		return
	}
	event := sdk.IntegrationSimulationEvent{
		Integration: sim.slug, ConnectionID: sim.connectionID, Kind: d.handoff.kind,
		Collection: d.handoff.collection, ID: d.handoff.id, Payload: payload,
	}
	body, err := json.Marshal(event)
	if err != nil {
		fail("failed", 0, err.Error())
		return
	}
	if len(body) > mockMaxHandoffBody {
		fail("failed", 0, fmt.Sprintf("hand-off is %d bytes, over the %d byte limit", len(body), mockMaxHandoffBody))
		return
	}
	evidenceBody, _ := json.Marshal(map[string]any{
		"integration": event.Integration, "connection_id": event.ConnectionID, "kind": event.Kind,
		"collection": event.Collection, "id": event.ID, "payload": mockEvidenceValue(d.payload),
	})
	for attempt := 1; attempt <= sim.mock.maxAttempts; attempt++ {
		if sim.ctx.Err() != nil {
			sim.finish(d, "failed", 0, "runtime stopped")
			return
		}
		sim.dmu.Lock()
		d.record.State, d.record.Attempts = "delivering", attempt
		sim.dmu.Unlock()
		status, response, deliverErr := sim.handOff(sim.ctx, sim.app, body)
		sim.recordEvidence(InterceptedCall{
			Host: mockRuntimeHost, Path: path, Method: http.MethodPost, Mocked: true,
			ReqBody: truncateEvidence(evidenceBody), RespBody: truncateEvidence(response), Status: status,
			Kind: "integration_handoff", Integration: sim.slug, Event: d.record.Event,
		})
		if deliverErr == nil && status >= 200 && status < 300 {
			sim.finish(d, "delivered", status, "")
			return
		}
		message := ""
		if deliverErr != nil {
			message = deliverErr.Error()
		} else {
			message = mockAppFailure(status, response)
		}
		retryable := deliverErr != nil || status >= 500
		if !retryable || attempt == sim.mock.maxAttempts {
			fail("failed", status, message)
			return
		}
		sim.dmu.Lock()
		d.record.Status, d.record.Error = status, message
		sim.dmu.Unlock()
		timer := time.NewTimer(sim.backoff(attempt))
		select {
		case <-sim.ctx.Done():
			timer.Stop()
			sim.finish(d, "failed", status, "runtime stopped")
			return
		case <-timer.C:
		}
	}
}

// markHandoffFailed tells tests, through the record the hand-off named,
// that the app never took the work.
func (sim *integrationSimulation) markHandoffFailed(target *mockHandoffTarget, message string) {
	if target == nil || target.collection == "" {
		return
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	records := sim.state.Collections[target.collection]
	if records == nil || records.Records[target.id] == nil {
		return
	}
	record := records.Records[target.id]
	report, _ := cloneMockValue(record["report"]).(map[string]any)
	if report == nil {
		report = map[string]any{}
	}
	report["state"] = mockHandoffFailedState
	report["error"] = message
	record["report"] = report
}

// mockAppFailure describes a non-2xx app answer, preferring the SDK's
// {"error": "..."} body.
func mockAppFailure(status int, response []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	text := ""
	if json.Unmarshal(response, &body) == nil && strings.TrimSpace(body.Error) != "" {
		text = strings.TrimSpace(body.Error)
	} else {
		text = strings.TrimSpace(string(response))
	}
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	if text == "" {
		return fmt.Sprintf("app responded %d", status)
	}
	return fmt.Sprintf("app responded %d: %s", status, text)
}

// mockEvidenceValue copies a JSON-shaped value with long strings replaced
// by their length, so inlined audio does not crowd evidence out.
func mockEvidenceValue(value any) any {
	switch v := value.(type) {
	case string:
		if len(v) > mockMaxEvidenceString {
			return fmt.Sprintf("[%d bytes]", len(v))
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = mockEvidenceValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = mockEvidenceValue(item)
		}
		return out
	}
	return value
}

// deliverSimulationHandoff posts a hand-off to one of the runtime's apps
// in-process, with that install's app token, through the same runtime
// proxy simulated webhooks use. Nothing goes over the network.
func (s *Server) deliverSimulationHandoff(ctx context.Context, w *Environment, app string, body []byte) (int, []byte, error) {
	inst, ok := w.Install(app)
	if !ok {
		return 0, nil, fmt.Errorf("app %q is not installed in this runtime", app)
	}
	token, err := s.appInstallToken(inst.InstallID)
	if err != nil {
		return 0, nil, fmt.Errorf("app %q credential unavailable", app)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+mockRuntimeHost+sdk.IntegrationSimulationPath, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", mockDeliveryUserAgent)
	// The proxy sets this too; it is set here as well so the SDK route,
	// which accepts only the platform's bearer, gets it even if the app
	// declared a matching public route.
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:0"
	recorder := newMockResponseRecorder(mockMaxEvidenceBody)
	s.proxyEnvironmentInstall(recorder, req, inst, sdk.IntegrationSimulationPath)
	return recorder.statusCode(), recorder.body.Bytes(), nil
}
