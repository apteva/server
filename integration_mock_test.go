package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type fixedMockFunctions struct{ seq int }

func (f *fixedMockFunctions) now() string { return "2026-01-02T03:04:05.000000Z" }
func (f *fixedMockFunctions) nextID(kind, prefix string) string {
	f.seq++
	return fmt.Sprintf("%s%s-%d", prefix, kind, f.seq)
}

func TestMockExpressionsAndTemplates(t *testing.T) {
	sc := &mockScope{fn: &fixedMockFunctions{}, vars: map[string]any{
		"input": map[string]any{"filter[phone_number]": "+1555", "digits": "", "size": float64(100), "flag": true},
		"call":  map[string]any{"state": "answered", "gather": map[string]any{"gather_id": "g1"}, "legs": []any{"a", "b"}},
	}}
	cases := map[string]any{
		"input['filter[phone_number]']":                    "+1555",
		"call.state == 'answered'":                         true,
		"call.state != 'parked' && call.gather.gather_id":  "g1",
		"call.missing.deeper":                              nil,
		"input.digits || 'timeout'":                        "timeout",
		"input.status || (input.digits && 'valid') || 'x'": "x",
		"!call.playback":                                   true,
		"len(call.legs)":                                   float64(2),
		"call.legs[1]":                                     "b",
		"input.size == 100 && input.size == '100'":         true,
		"lower('CALL_REJECTED')":                           "call_rejected",
		"token('v3:')":                                     "v3:token-1",
		"input.flag == true && null == input.nope":         true,
	}
	for src, want := range cases {
		expr, err := parseMockExpr(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		got, err := expr.eval(sc)
		if err != nil || !mockEqual(got, want) && !(got == nil && want == nil) {
			t.Fatalf("%s = %#v (%v), want %#v", src, got, err, want)
		}
	}
	for _, bad := range []string{"call.", "a ==", "nope()", "len()", "'open", "a & b"} {
		if _, err := parseMockExpr(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}

	templates := mockTemplates{}
	base, _ := compileMockTemplate(map[string]any{"id": "{{call.state}}", "kept": "base"})
	templates["base"] = base.(*mockMapTemplate)
	tmpl, err := compileMockTemplate(map[string]any{
		"$use":    "base",
		"whole":   "{{call.gather}}",
		"text":    "state={{call.state}} missing={{call.nope}}",
		"omitted": "{{call.nope}}",
		"null":    nil,
		"list":    []any{"{{call.legs[0]}}", 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tmpl.render(sc, &templates)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(map[string]any)
	if got["id"] != "answered" || got["kept"] != "base" || got["text"] != "state=answered missing=" {
		t.Fatalf("rendered %#v", got)
	}
	if _, present := got["omitted"]; present {
		t.Fatalf("null expression was not omitted: %#v", got)
	}
	if value, present := got["null"]; !present || value != nil {
		t.Fatalf("literal null was dropped: %#v", got)
	}
	whole := got["whole"].(map[string]any)
	whole["gather_id"] = "changed"
	if sc.vars["call"].(map[string]any)["gather"].(map[string]any)["gather_id"] != "g1" {
		t.Fatal("rendered value aliases scope state")
	}
}

func loadEmbeddedCatalog(t *testing.T) *AppCatalog {
	t.Helper()
	catalog := NewAppCatalog()
	if _, err := catalog.LoadFromFS(integrationsCatalogEmbeddedFS); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestEmbeddedCatalogMocksCompile(t *testing.T) {
	catalog := loadEmbeddedCatalog(t)
	found := 0
	for _, slug := range []string{"telnyx"} {
		if catalog.Get(slug) == nil || catalog.Get(slug).Mock == nil {
			t.Fatalf("%s has no mock section", slug)
		}
	}
	for _, summary := range catalog.List() {
		app := catalog.Get(summary.Slug)
		if app == nil || app.Mock == nil {
			continue
		}
		found++
		if _, err := compileAppMock(app); err != nil {
			t.Fatalf("%s: %v", app.Slug, err)
		}
	}
	if found == 0 {
		t.Fatal("no catalog mock sections")
	}
}

// telnyxVerify is the check Telephony applies to Telnyx webhooks.
func telnyxVerify(publicKey, timestamp, signature string, body []byte) error {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(seconds, 0)) > 5*time.Minute || time.Until(time.Unix(seconds, 0)) > 5*time.Minute {
		return errors.New("stale timestamp")
	}
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("bad key")
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || !ed25519.Verify(key, append([]byte(timestamp+"|"), body...), sig) {
		return errors.New("bad signature")
	}
	return nil
}

type telnyxSimHarness struct {
	t         *testing.T
	s         *Server
	runtime   *Environment
	owner     int64
	install   int64
	connID    int64
	app       *AppTemplate
	creds     map[string]string
	sidecar   *httptest.Server
	appID     string
	mu        sync.Mutex
	events    []string
	failFirst map[string]bool
}

func (h *telnyxSimHarness) tool(name string, input map[string]any) *ExecuteResult {
	h.t.Helper()
	var tool *AppToolDef
	for i := range h.app.Tools {
		if h.app.Tools[i].Name == name {
			tool = &h.app.Tools[i]
		}
	}
	if tool == nil {
		h.t.Fatalf("no tool %s", name)
	}
	// No environment id: a simulated connection must still be simulated.
	res, err := h.s.executeConnectionToolWithRefreshContext(context.Background(), h.connID, h.app, tool, h.creds, input, "")
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	return res
}

func (h *telnyxSimHarness) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func (h *telnyxSimHarness) state() sdk.RuntimeIntegrationState {
	h.t.Helper()
	rec := runtimeAPIRequest(h.t, h.s, h.owner, http.MethodGet, "/apps/callback/runtimes/rt-sim/integrations/telnyx/state?app=telephony&role=carrier", nil)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("state status=%d body=%s", rec.Code, rec.Body.String())
	}
	var state sdk.RuntimeIntegrationState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h *telnyxSimHarness) waitFor(what string, ok func(sdk.RuntimeIntegrationState) bool) sdk.RuntimeIntegrationState {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		state := h.state()
		if ok(state) {
			return state
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; events=%v state=%+v", what, h.recorded(), state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newTelnyxSimHarness(t *testing.T) *telnyxSimHarness {
	t.Helper()
	s := newRuntimeAPITestServer(t)
	s.catalog = loadEmbeddedCatalog(t)
	h := &telnyxSimHarness{t: t, s: s, app: s.catalog.Get("telnyx"), failFirst: map[string]bool{"call.initiated": true}}
	h.owner = seedRuntimeAPIInstall(t, s, "environments", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	manifest := sdk.Manifest{Name: "telephony", Requires: sdk.Requires{Integrations: []sdk.IntegrationDep{{Role: "carrier", CompatibleSlugs: []string{"telnyx"}}}}}
	h.install = seedRunningInstall(t, s, "telephony", "rt-sim", manifest, nil)

	h.sidecar = httptest.NewServer(http.HandlerFunc(h.serveTelephony))
	t.Cleanup(h.sidecar.Close)
	h.runtime = &Environment{
		ID: "rt-sim", ProjectID: "rt-sim", ownerInstallID: h.owner, edge: &EnvironmentEdge{},
		installs: map[string]*localInstall{"telephony": {InstallID: h.install, AppName: "telephony", ProjectID: "rt-sim", SidecarURL: h.sidecar.URL}},
		agents:   map[int64]*EnvironmentAgent{}, agentAliases: map[string]int64{}, createdAt: time.Now(),
	}
	s.environments.mu.Lock()
	s.environments.environments[h.runtime.ID] = h.runtime
	s.environments.mu.Unlock()
	t.Cleanup(h.runtime.stopSimulations)

	if err := s.bindEnvironmentIntegrationMocks(1, h.runtime, []RuntimeIntegrationBinding{{
		App: "telephony", Role: "carrier", Slug: "telnyx", Simulate: true,
		Credentials: map[string]string{"token": "test-token"},
		Seed: map[string][]map[string]any{
			"phone_numbers":           {{"phone_number": "+15550100001"}},
			"outbound_voice_profiles": {{"name": "Default"}},
		},
	}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	connID, ok := asInt64(readBindings(t, s, h.install)["carrier"])
	if !ok {
		t.Fatal("carrier not bound")
	}
	h.connID = connID
	var encrypted string
	if err := s.store.db.QueryRow(`SELECT encrypted_credentials FROM connections WHERE id=?`, connID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	plain, err := Decrypt(s.secret, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(plain), &h.creds); err != nil {
		t.Fatal(err)
	}
	return h
}

// serveTelephony behaves like Telephony's Telnyx inbound handler: it checks
// the route secret, the signature and the application id, then drives an
// IVR from inside the handler (as Telephony does) and returns 204.
func (h *telnyxSimHarness) serveTelephony(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/inbound/telnyx/route-1" || r.URL.Query().Get("secret") != "s3cret" {
		http.Error(w, "route not found", http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	if err := telnyxVerify(h.creds["public_key"], r.Header.Get("Telnyx-Timestamp"), r.Header.Get("Telnyx-Signature-Ed25519"), body); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var event struct {
		Data struct {
			EventType  string         `json:"event_type"`
			OccurredAt string         `json:"occurred_at"`
			Payload    map[string]any `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	payload := event.Data.Payload
	if payload["connection_id"] != h.appID || payload["call_control_id"] == "" || event.Data.OccurredAt == "" {
		http.Error(w, "webhook connection does not match route", http.StatusForbidden)
		return
	}
	h.mu.Lock()
	if h.failFirst[event.Data.EventType] {
		delete(h.failFirst, event.Data.EventType)
		h.mu.Unlock()
		http.Error(w, "retry", http.StatusServiceUnavailable)
		return
	}
	h.events = append(h.events, event.Data.EventType)
	h.mu.Unlock()
	callID := payload["call_control_id"]
	var res *ExecuteResult
	switch event.Data.EventType {
	case "call.initiated":
		if payload["direction"] != "incoming" || payload["to"] != "+15550100001" {
			http.Error(w, "bad call", http.StatusForbidden)
			return
		}
		res = h.tool("answer_call", map[string]any{"call_control_id": callID, "command_id": "answer-1"})
	case "call.answered":
		res = h.tool("gather_using_speak", map[string]any{"call_control_id": callID, "payload": "Press 1", "voice": "female", "gather_id": "row:menu", "valid_digits": "12", "command_id": "gather-1"})
	case "call.gather.ended":
		if payload["digits"] != "1" || payload["gather_id"] != "row:menu" || payload["status"] != "valid" {
			http.Error(w, "bad gather", http.StatusBadRequest)
			return
		}
		res = h.tool("speak_text", map[string]any{"call_control_id": callID, "payload": "Goodbye", "voice": "female", "client_state": "terminal"})
	case "call.speak.ended":
		if payload["client_state"] == "terminal" && payload["status"] == "completed" {
			res = h.tool("hangup_call", map[string]any{"call_control_id": callID})
		}
	}
	if res != nil && !res.Success {
		http.Error(w, fmt.Sprintf("carrier action failed: %d %v", res.Status, res.Data), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func TestTelnyxSimulationDrivesTelephonyInboundIVR(t *testing.T) {
	h := newTelnyxSimHarness(t)
	if key, err := base64.StdEncoding.DecodeString(h.creds["public_key"]); err != nil || len(key) != ed25519.PublicKeySize {
		t.Fatalf("simulated connection has no usable public key: %q", h.creds["public_key"])
	}

	// Route setup, as Telephony's configureTelnyxRoute performs it.
	numbers := h.tool("list_phone_numbers", map[string]any{"filter[phone_number]": "+15550100001", "page[size]": float64(100)})
	data := numbers.Data.(map[string]any)["data"].([]any)
	if !numbers.Success || len(data) != 1 {
		t.Fatalf("list_phone_numbers = %+v", numbers)
	}
	numberID := data[0].(map[string]any)["id"].(string)
	if profiles := h.tool("list_outbound_voice_profiles", map[string]any{}); len(profiles.Data.(map[string]any)["data"].([]any)) != 1 {
		t.Fatalf("profiles = %+v", profiles)
	}
	webhook := fmt.Sprintf("https://runtime.apteva.invalid/rt-sim/api/apps/telephony/_install/%d/inbound/telnyx/route-1?secret=s3cret&project_id=rt-sim", h.install)
	created := h.tool("create_call_control_application", map[string]any{"application_name": "Apteva route-1", "webhook_event_url": webhook, "active": true, "webhook_api_version": "2", "webhook_timeout_secs": float64(5)})
	if created.Status != http.StatusCreated {
		t.Fatalf("create application = %+v", created)
	}
	h.appID = created.Data.(map[string]any)["data"].(map[string]any)["id"].(string)
	if assigned := h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": h.appID}); !assigned.Success || assigned.Data.(map[string]any)["data"].(map[string]any)["connection_id"] != h.appID {
		t.Fatalf("update_phone_number = %+v", assigned)
	}

	// The caller dials in. call.initiated fails once with 503 and is retried.
	h.s.environments.mu.Lock()
	h.runtime.findSimulation("telnyx", "telephony", "carrier").backoff = func(int) time.Duration { return time.Millisecond }
	h.s.environments.mu.Unlock()
	rec := runtimeAPIRequest(t, h.s, h.owner, http.MethodPost, "/apps/callback/runtimes/rt-sim/integrations/telnyx/triggers/incoming_call", sdk.RuntimeIntegrationTriggerRequest{
		RuntimeIntegrationSelector: sdk.RuntimeIntegrationSelector{App: "telephony", Role: "carrier"},
		Input:                      map[string]any{"to": "+15550100001", "from": "+15550100999"},
		Wait:                       true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger status=%d body=%s", rec.Code, rec.Body.String())
	}
	var triggered sdk.RuntimeIntegrationTriggerResult
	if err := json.Unmarshal(rec.Body.Bytes(), &triggered); err != nil {
		t.Fatal(err)
	}
	var started struct {
		CallControlID string `json:"call_control_id"`
	}
	_ = json.Unmarshal(triggered.Result, &started)
	if started.CallControlID == "" || len(triggered.Deliveries) != 1 {
		t.Fatalf("trigger result = %s deliveries=%+v", triggered.Result, triggered.Deliveries)
	}
	first := triggered.Deliveries[0]
	if first.Event != "call.initiated" || first.State != "delivered" || first.Attempts != 2 || first.Status != http.StatusNoContent || first.App != "telephony" || first.Target != "/api/apps/telephony/inbound/telnyx/route-1" {
		t.Fatalf("call.initiated delivery = %+v", first)
	}

	h.waitFor("an active gather", func(state sdk.RuntimeIntegrationState) bool {
		calls := state.Collections["calls"]
		return len(calls) == 1 && calls[0]["gather"] != nil
	})
	rec = runtimeAPIRequest(t, h.s, h.owner, http.MethodPost, "/apps/callback/runtimes/rt-sim/integrations/telnyx/triggers/gather_input", sdk.RuntimeIntegrationTriggerRequest{
		RuntimeIntegrationSelector: sdk.RuntimeIntegrationSelector{App: "telephony", Role: "carrier"},
		Input:                      map[string]any{"call_control_id": started.CallControlID, "digits": "1"},
		Wait:                       true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("gather trigger status=%d body=%s", rec.Code, rec.Body.String())
	}
	state := h.waitFor("the call to end", func(state sdk.RuntimeIntegrationState) bool {
		calls := state.Collections["calls"]
		return len(calls) == 1 && calls[0]["state"] == "hangup" && len(h.recorded()) == 6
	})
	want := []string{"call.initiated", "call.answered", "call.gather.ended", "call.speak.started", "call.speak.ended", "call.hangup"}
	if got := h.recorded(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	call := state.Collections["calls"][0]
	if call["hangup_source"] != "callee" || call["hangup_cause"] != "normal_clearing" || call["connection_id"] != h.appID {
		t.Fatalf("call = %#v", call)
	}
	for _, d := range state.Deliveries {
		if d.State != "delivered" {
			t.Fatalf("delivery not delivered: %+v", d)
		}
	}

	// Provider command semantics: a repeated command_id is ignored, a new
	// command on an ended call fails like Telnyx, unknown ids are rejected.
	if again := h.tool("answer_call", map[string]any{"call_control_id": started.CallControlID, "command_id": "answer-1"}); !again.Success {
		t.Fatalf("repeated command_id = %+v", again)
	}
	ended := h.tool("answer_call", map[string]any{"call_control_id": started.CallControlID, "command_id": "answer-2"})
	if ended.Status != http.StatusUnprocessableEntity || !strings.Contains(mockString(ended.Data), `"90018"`) {
		t.Fatalf("answer on ended call = %+v", ended)
	}
	unknown := h.tool("hangup_call", map[string]any{"call_control_id": "v3:nope"})
	if unknown.Status != http.StatusUnprocessableEntity || !strings.Contains(mockString(unknown.Data), `"90015"`) {
		t.Fatalf("unknown call = %+v", unknown)
	}
	if len(h.recorded()) != 6 {
		t.Fatalf("deduplicated or failed commands emitted events: %v", h.recorded())
	}

	// Strict mode: a tool the simulation does not define fails.
	strict := h.tool("dial_call", map[string]any{"connection_id": h.appID, "to": "+15550100002", "from": "+15550100001"})
	if strict.Success || strict.Status != http.StatusNotImplemented || strict.Data.(map[string]any)["error"] != sdk.RuntimeErrMockUnsupportedTool {
		t.Fatalf("unsupported tool = %+v", strict)
	}

	// Evidence: simulated tool calls and webhook deliveries are in the edge
	// log, and no route secret leaks into it.
	var sawAnswer, sawWebhook bool
	for _, entry := range h.runtime.Edge().Calls() {
		if entry.Kind == "integration_simulation" && entry.Tool == "answer_call" && entry.Path == "/calls/"+started.CallControlID+"/actions/answer" && entry.Host == "api.telnyx.com" {
			sawAnswer = true
		}
		if entry.Kind == "integration_webhook" && entry.Event == "call.hangup" && entry.Path == "/api/apps/telephony/inbound/telnyx/route-1" && entry.Status == http.StatusNoContent {
			sawWebhook = true
		}
		if strings.Contains(entry.Path+entry.ReqBody+entry.RespBody, "s3cret") && entry.Tool != "create_call_control_application" {
			t.Fatalf("route secret in evidence: %+v", entry)
		}
	}
	if !sawAnswer || !sawWebhook {
		t.Fatalf("evidence missing: answer=%v webhook=%v", sawAnswer, sawWebhook)
	}
}

func TestTelnyxSimulationRejectsBadTriggersAndOutsideWebhooks(t *testing.T) {
	h := newTelnyxSimHarness(t)
	sel := sdk.RuntimeIntegrationSelector{App: "telephony", Role: "carrier"}
	trigger := func(name string, input map[string]any) *httptest.ResponseRecorder {
		return runtimeAPIRequest(t, h.s, h.owner, http.MethodPost, "/apps/callback/runtimes/rt-sim/integrations/telnyx/triggers/"+name, sdk.RuntimeIntegrationTriggerRequest{RuntimeIntegrationSelector: sel, Input: input, Wait: true})
	}
	if rec := trigger("incoming_call", map[string]any{"to": "+15550100001"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing from: %d %s", rec.Code, rec.Body.String())
	}
	if rec := trigger("incoming_call", map[string]any{"to": "+15550100009", "from": "+1"}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not a provisioned number") {
		t.Fatalf("unprovisioned number: %d %s", rec.Code, rec.Body.String())
	}
	if rec := trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+1"}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not assigned") {
		t.Fatalf("unassigned number: %d %s", rec.Code, rec.Body.String())
	}
	if rec := trigger("no_such_trigger", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown trigger: %d", rec.Code)
	}
	if rec := runtimeAPIRequest(t, h.s, h.owner, http.MethodGet, "/apps/callback/runtimes/rt-sim/integrations/aws-ses/state", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unsimulated slug: %d", rec.Code)
	}
	other := seedRuntimeAPIInstall(t, h.s, "intruder", sdk.PermRuntimesManage)
	if rec := runtimeAPIRequest(t, h.s, other, http.MethodPost, "/apps/callback/runtimes/rt-sim/integrations/telnyx/triggers/incoming_call", sdk.RuntimeIntegrationTriggerRequest{}); rec.Code != http.StatusNotFound {
		t.Fatalf("other install reached the runtime: %d", rec.Code)
	}

	// An application whose webhook points outside the runtime gets nothing.
	numbers := h.tool("list_phone_numbers", map[string]any{})
	numberID := numbers.Data.(map[string]any)["data"].([]any)[0].(map[string]any)["id"]
	app := h.tool("create_call_control_application", map[string]any{"application_name": "outside", "webhook_event_url": "https://example.com/hook"})
	h.appID = app.Data.(map[string]any)["data"].(map[string]any)["id"].(string)
	h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": h.appID})
	rec := trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999"})
	var result sdk.RuntimeIntegrationTriggerResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("trigger: %d %s", rec.Code, rec.Body.String())
	}
	if len(result.Deliveries) != 1 || result.Deliveries[0].State != "undeliverable" || len(h.recorded()) != 0 {
		t.Fatalf("outside webhook delivered: %+v events=%v", result.Deliveries, h.recorded())
	}
	for _, entry := range h.runtime.Edge().Calls() {
		if entry.Kind == "integration_webhook" && (!entry.Blocked || entry.Host != "example.com") {
			t.Fatalf("outside delivery evidence = %+v", entry)
		}
	}
}

func TestSimulatedConnectionFixturesWinAndOthersKeepStubs(t *testing.T) {
	h := newTelnyxSimHarness(t)
	remove := RegisterEnvironmentInterceptor("rt-sim", []IntegrationFixture{{App: "telnyx", Tool: "list_phone_numbers", Data: map[string]any{"data": []any{map[string]any{"id": "fixture"}}}}})
	defer remove()
	var tool *AppToolDef
	for i := range h.app.Tools {
		if h.app.Tools[i].Name == "list_phone_numbers" {
			tool = &h.app.Tools[i]
		}
	}
	res, err := executeIntegrationTool(h.app, tool, h.creds, map[string]any{}, "rt-sim", withIntegrationConnectionID(context.Background(), h.connID))
	if err != nil || !strings.Contains(mockString(res.Data), "fixture") {
		t.Fatalf("fixture did not win: %+v %v", res, err)
	}
	var send *AppToolDef
	for i := range h.app.Tools {
		if h.app.Tools[i].Name == "send_sms" {
			send = &h.app.Tools[i]
		}
	}
	// A connection that is not simulated keeps the generic stub.
	stub, err := executeIntegrationTool(h.app, send, map[string]string{}, map[string]any{"from": "a", "to": "b"}, "rt-sim", withIntegrationConnectionID(context.Background(), h.connID+1000))
	if err != nil || stub.Data.(map[string]any)["_stub"] != true {
		t.Fatalf("unsimulated connection = %+v %v", stub, err)
	}
	simulated, err := executeIntegrationTool(h.app, send, map[string]string{}, map[string]any{"from": "a", "to": "b"}, "rt-sim", withIntegrationConnectionID(context.Background(), h.connID))
	if err != nil || simulated.Status != http.StatusNotImplemented {
		t.Fatalf("simulated connection fell back to a stub: %+v %v", simulated, err)
	}
}

func TestSimulatedBindingValidationSnapshotAndTeardown(t *testing.T) {
	h := newTelnyxSimHarness(t)
	s := h.s
	for name, tc := range map[string]struct {
		binding RuntimeIntegrationBinding
		err     string
	}{
		"seed without simulate": {RuntimeIntegrationBinding{ExposeToAgents: true, Slug: "telnyx", Seed: map[string][]map[string]any{"phone_numbers": {{"phone_number": "+1"}}}}, "seed requires simulate"},
		"supplied public key":   {RuntimeIntegrationBinding{ExposeToAgents: true, Slug: "telnyx", Simulate: true, Credentials: map[string]string{"public_key": "abc"}}, `credential "public_key" is generated`},
		"unseedable collection": {RuntimeIntegrationBinding{ExposeToAgents: true, Slug: "telnyx", Simulate: true, Seed: map[string][]map[string]any{"calls": {{"call_control_id": "x"}}}}, `collection "calls" cannot be seeded`},
		"unknown collection":    {RuntimeIntegrationBinding{ExposeToAgents: true, Slug: "telnyx", Simulate: true, Seed: map[string][]map[string]any{"faxes": {{}}}}, `unknown collection "faxes"`},
		"no simulation":         {RuntimeIntegrationBinding{ExposeToAgents: true, Slug: "aws-ses", Simulate: true}, `"aws-ses" has no simulation`},
		"duplicate binding":     {RuntimeIntegrationBinding{App: "telephony", Role: "carrier", Slug: "telnyx", Simulate: true}, "already simulated"},
	} {
		err := s.bindEnvironmentIntegrationMocks(1, h.runtime, []RuntimeIntegrationBinding{tc.binding})
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.err)
		}
	}
	// An agent-visible simulation of the same integration is a separate
	// binding and coexists with the role-bound one.
	if err := s.bindEnvironmentIntegrationMocks(1, h.runtime, []RuntimeIntegrationBinding{{ExposeToAgents: true, Slug: "telnyx", Name: "Agent telnyx", Simulate: true}}); err != nil {
		t.Fatalf("agent-visible simulation: %v", err)
	}
	if h.runtime.countSimulations("telnyx") != 2 || h.runtime.findSimulation("telnyx", "", "") != nil {
		t.Fatal("two telnyx simulations must require a selector")
	}

	numbers := h.tool("list_phone_numbers", map[string]any{})
	numberID := numbers.Data.(map[string]any)["data"].([]any)[0].(map[string]any)["id"]
	app := h.tool("create_call_control_application", map[string]any{"application_name": "snap", "webhook_event_url": "https://runtime.apteva.invalid/rt-sim/api/apps/telephony/inbound/telnyx/route-1"})
	appID := app.Data.(map[string]any)["data"].(map[string]any)["id"]
	h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": appID})
	snapshots := h.runtime.SimulationSnapshots()
	if len(snapshots) != 2 || snapshots[0].App != "telephony" || snapshots[0].Role != "carrier" || snapshots[1].App != "" {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	raw, err := json.Marshal(snapshots)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []IntegrationSimulationSnapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	// A runtime restored from the snapshot starts from that state, not from
	// the binding seed.
	restored := &Environment{
		ID: "rt-restored", ProjectID: "rt-restored", restoredSimulations: decoded,
		installs: map[string]*localInstall{"telephony": {InstallID: h.install, AppName: "telephony", ProjectID: "rt-restored"}},
	}
	defer restored.stopSimulations()
	if err := s.bindEnvironmentIntegrationMocks(1, restored, []RuntimeIntegrationBinding{{App: "telephony", Role: "carrier", Slug: "telnyx", Simulate: true, Seed: map[string][]map[string]any{"phone_numbers": {{"phone_number": "+15550100777"}}}}}); err != nil {
		t.Fatal(err)
	}
	state := restored.findSimulation("telnyx", "telephony", "carrier").publicState()
	if len(state.Collections["phone_numbers"]) != 1 || state.Collections["phone_numbers"][0]["connection_id"] != appID || len(state.Collections["call_control_applications"]) != 1 {
		t.Fatalf("restored state = %+v", state.Collections)
	}

	h.runtime.stopSimulations()
	if lookupIntegrationSimulation("rt-sim", h.connID) != nil || simulatedConnectionEnvironment(h.connID) != "" {
		t.Fatal("simulation still registered after teardown")
	}
}
