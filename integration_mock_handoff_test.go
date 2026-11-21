package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// sipAppFake stands in for Telephony's side of a direct SIP hand-off: the
// SDK route (bearer check, POST, event decode) plus whatever the test makes
// it do with each event. Anything else the simulation sends it is recorded
// as unexpected.
type sipAppFake struct {
	t     *testing.T
	token string

	mu         sync.Mutex
	events     []sdk.IntegrationSimulationEvent
	unexpected []string
	// answer, when set, decides the response to an event; it may report
	// back into the simulation before answering, as Telephony does.
	answer func(event sdk.IntegrationSimulationEvent) (int, any)
}

func (f *sipAppFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != sdk.IntegrationSimulationPath {
		f.mu.Lock()
		f.unexpected = append(f.unexpected, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token || r.Method != http.MethodPost {
		f.mu.Lock()
		f.unexpected = append(f.unexpected, "unauthorized "+r.Method+" "+r.Header.Get("Authorization"))
		f.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var event sdk.IntegrationSimulationEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.events = append(f.events, event)
	answer := f.answer
	f.mu.Unlock()
	status, body := http.StatusOK, any(map[string]any{"ok": true})
	if answer != nil {
		status, body = answer(event)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *sipAppFake) setAnswer(answer func(event sdk.IntegrationSimulationEvent) (int, any)) {
	f.mu.Lock()
	f.answer = answer
	f.mu.Unlock()
}

func (f *sipAppFake) received() []sdk.IntegrationSimulationEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sdk.IntegrationSimulationEvent(nil), f.events...)
}

func (f *sipAppFake) kinds() []string {
	var out []string
	for _, event := range f.received() {
		out = append(out, event.Kind)
	}
	return out
}

// newSIPHarness is the Telnyx harness with Telephony installed as a real
// environment install (as Environments installs it) answering hand-offs.
func newSIPHarness(t *testing.T) (*telnyxSimHarness, *sipAppFake) {
	t.Helper()
	h := newTelnyxSimHarness(t)
	if _, err := h.s.store.db.Exec(`UPDATE app_installs SET source = 'environment' WHERE id = ?`, h.install); err != nil {
		t.Fatal(err)
	}
	token, err := h.s.appInstallToken(h.install)
	if err != nil {
		t.Fatal(err)
	}
	fake := &sipAppFake{t: t, token: token}
	h.setHandler(fake)
	sim := h.runtime.findSimulation("telnyx", "telephony", "carrier")
	sim.backoff = func(int) time.Duration { return time.Millisecond }
	return h, fake
}

func (h *telnyxSimHarness) trigger(name string, input map[string]any, wait bool) (*httptest.ResponseRecorder, map[string]any, []sdk.RuntimeIntegrationDelivery) {
	h.t.Helper()
	rec := runtimeAPIRequest(h.t, h.s, h.owner, http.MethodPost, "/apps/callback/runtimes/rt-sim/integrations/telnyx/triggers/"+name, sdk.RuntimeIntegrationTriggerRequest{
		RuntimeIntegrationSelector: sdk.RuntimeIntegrationSelector{App: "telephony", Role: "carrier"},
		Input:                      input, Wait: wait,
	})
	if rec.Code != http.StatusOK {
		return rec, nil, nil
	}
	var result sdk.RuntimeIntegrationTriggerResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		h.t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(result.Result, &body)
	return rec, body, result.Deliveries
}

// report posts an IntegrationSimulationReport through the real auth
// middleware, as the SDK's platform client does.
func (h *telnyxSimHarness) report(token string, header http.Header, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	raw, ok := body.([]byte)
	if !ok {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/integration-simulation/report", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Apteva-Environment-Id", h.runtime.ID)
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	h.s.authMiddleware(h.s.handleIntegrationSimulationReport)(rec, req)
	return rec
}

func (h *telnyxSimHarness) sipCall(id string) map[string]any {
	h.t.Helper()
	for _, record := range h.state().Collections["sip_calls"] {
		if record["id"] == id {
			return record
		}
	}
	h.t.Fatalf("no sip_calls record %s", id)
	return nil
}

func dataID(t *testing.T, res *ExecuteResult) string {
	t.Helper()
	body, _ := res.Data.(map[string]any)
	data, _ := body["data"].(map[string]any)
	id, _ := data["id"].(string)
	if !res.Success || id == "" {
		t.Fatalf("no data.id: status=%d %v", res.Status, res.Data)
	}
	return id
}

// configureDirectSIP makes Telephony's configureTelnyxDirectSIP calls and
// returns the FQDN connection id the number now points at.
func configureDirectSIP(t *testing.T, h *telnyxSimHarness, encryptedMedia any) string {
	t.Helper()
	application := dataID(t, h.tool("create_call_control_application", map[string]any{
		"application_name": "Apteva", "webhook_event_url": "https://runtime.apteva.invalid/rt-sim/api/apps/telephony/inbound/telnyx/route-1?secret=s3cret",
	}))
	numbers := h.tool("list_phone_numbers", map[string]any{"filter[phone_number]": "+15550100001", "page[size]": 100})
	list, _ := numbers.Data.(map[string]any)["data"].([]any)
	if len(list) != 1 {
		t.Fatalf("list_phone_numbers = %v", numbers.Data)
	}
	numberID := list[0].(map[string]any)["id"].(string)
	if res := h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": application}); !res.Success {
		t.Fatalf("assign to application: %d %v", res.Status, res.Data)
	}
	connectionInput := map[string]any{
		"connection_name": "Apteva +15550100001", "active": true, "transport_protocol": "UDP", "dtmf_type": "RFC 2833",
		"inbound":           map[string]any{"ani_number_format": "+E.164", "dnis_number_format": "+e164", "codecs": []any{"PCMU", "PCMA"}, "generate_ringback_tone": true},
		"noise_suppression": "disabled", "jitter_buffer": map[string]any{"enable_jitter_buffer": false},
	}
	if encryptedMedia != nil {
		connectionInput["encrypted_media"] = encryptedMedia
	}
	created := h.tool("create_fqdn_connection", connectionInput)
	if created.Status != http.StatusCreated {
		t.Fatalf("create_fqdn_connection = %d %v", created.Status, created.Data)
	}
	connection := dataID(t, created)
	if record := created.Data.(map[string]any)["data"].(map[string]any); record["record_type"] != "fqdn_connection" || record["transport_protocol"] != "UDP" {
		t.Fatalf("fqdn connection = %v", record)
	}
	if res := h.tool("create_fqdn", map[string]any{"connection_id": "404", "fqdn": "sip.example.test", "dns_record_type": "a", "port": 5070}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("create_fqdn on a missing connection = %d %v", res.Status, res.Data)
	}
	fqdn := h.tool("create_fqdn", map[string]any{"connection_id": connection, "fqdn": "sip.example.test", "dns_record_type": "a", "port": 5070})
	if fqdn.Status != http.StatusCreated {
		t.Fatalf("create_fqdn = %d %v", fqdn.Status, fqdn.Data)
	}
	dataID(t, fqdn)
	if res := h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": "404"}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("update_phone_number to an unknown connection = %d %v", res.Status, res.Data)
	}
	if res := h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": connection}); !res.Success {
		t.Fatalf("assign to FQDN connection: %d %v", res.Status, res.Data)
	}
	return connection
}

func TestTelnyxDirectSIPCallsAreHandedToTheOwningApp(t *testing.T) {
	h, fake := newSIPHarness(t)
	connection := configureDirectSIP(t, h, nil)

	// Telephony reports call progress while it handles each hand-off.
	fake.setAnswer(func(event sdk.IntegrationSimulationEvent) (int, any) {
		report := func(label string, fields map[string]any) {
			rec := h.report(fake.token, nil, sdk.IntegrationSimulationReport{
				Integration: event.Integration, ConnectionID: event.ConnectionID,
				Collection: event.Collection, ID: event.ID, Event: label, Fields: fields,
			})
			if rec.Code != http.StatusOK {
				t.Errorf("report %s = %d %s", label, rec.Code, rec.Body.String())
			}
		}
		switch event.Kind {
		case "sip_inbound_call":
			report("ringing", map[string]any{"state": "ringing", "sip_call_id_header": "abc@sip.example.test", "responses": []any{100, 180}})
			report("answered", map[string]any{"state": "answered", "responses": []any{100, 180, 200}, "codec": "PCMU"})
		case "sip_hangup":
			var payload map[string]any
			_ = json.Unmarshal(event.Payload, &payload)
			report("ended", map[string]any{"state": "ended", "ended_by": "caller", "cause": payload["cause"], "q850": payload["q850"],
				"rtp": map[string]any{"sent_packets": 110, "received_packets": 112, "non_silent_ms": 1800}})
		}
		return http.StatusOK, map[string]any{"accepted": event.Kind}
	})

	rec, result, deliveries := h.trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("incoming_call = %d %s", rec.Code, rec.Body.String())
	}
	sipID, _ := result["sip_call_id"].(string)
	if !strings.HasPrefix(sipID, "sip_") || result["connection_id"] != connection {
		t.Fatalf("incoming_call result = %v", result)
	}
	if len(deliveries) != 1 || deliveries[0].Event != "handoff:sip_inbound_call" || deliveries[0].State != "delivered" ||
		deliveries[0].App != "telephony" || deliveries[0].Target != "/api/apps/telephony"+sdk.IntegrationSimulationPath || deliveries[0].Lane != sipID {
		t.Fatalf("hand-off delivery = %+v", deliveries)
	}

	events := fake.received()
	if len(events) != 1 {
		t.Fatalf("hand-offs = %+v", events)
	}
	inbound := events[0]
	if inbound.Integration != "telnyx" || inbound.ConnectionID != h.connID || inbound.Kind != "sip_inbound_call" || inbound.Collection != "sip_calls" || inbound.ID != sipID {
		t.Fatalf("hand-off event = %+v", inbound)
	}
	var payload struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Target struct {
			Host      string `json:"host"`
			Port      int    `json:"port"`
			Transport string `json:"transport"`
		} `json:"target"`
		Codecs         []string        `json:"codecs"`
		DTMFType       string          `json:"dtmf_type"`
		EncryptedMedia json.RawMessage `json:"encrypted_media"`
		ConnectionID   string          `json:"connection_id"`
		CallerAudio    struct {
			Name      string `json:"name"`
			URL       string `json:"url"`
			WAVBase64 string `json:"wav_base64"`
		} `json:"caller_audio"`
	}
	if err := json.Unmarshal(inbound.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.From != "+15550100999" || payload.To != "+15550100001" || payload.Target.Host != "sip.example.test" || payload.Target.Port != 5070 ||
		payload.Target.Transport != "udp" || strings.Join(payload.Codecs, ",") != "PCMU,PCMA" || payload.DTMFType != "RFC 2833" ||
		payload.EncryptedMedia != nil || payload.ConnectionID != connection {
		t.Fatalf("sip_inbound_call payload = %s", inbound.Payload)
	}
	fixture, _ := mockFixtureByName("caller-speech-8k.wav")
	wav, err := base64.StdEncoding.DecodeString(payload.CallerAudio.WAVBase64)
	if err != nil || !bytes.Equal(wav, fixture.data) || payload.CallerAudio.Name != "caller-speech-8k.wav" ||
		!strings.HasSuffix(payload.CallerAudio.URL, "/api/integration-simulation/fixtures/caller-speech-8k.wav") {
		t.Fatalf("caller audio = %s %s (%d bytes, %v)", payload.CallerAudio.Name, payload.CallerAudio.URL, len(wav), err)
	}
	if decoded, err := mockReadWAV(wav); err != nil || decoded.channels != 1 || decoded.rate != 8000 {
		t.Fatalf("inlined caller audio is not mono 8 kHz PCM16: %+v %v", decoded.channels, err)
	}

	// The record holds the provider side and, under report, the app's.
	record := h.sipCall(sipID)
	report, _ := record["report"].(map[string]any)
	if record["to"] != "+15550100001" || record["caller_audio"] != "caller-speech-8k.wav" || report["state"] != "answered" ||
		report["codec"] != "PCMU" || report["sip_call_id_header"] != "abc@sip.example.test" || fmt.Sprint(report["responses"]) != "[100 180 200]" {
		t.Fatalf("sip call record = %v", record)
	}
	if _, inlined := record["wav_base64"]; inlined || strings.Contains(fmt.Sprint(record), payload.CallerAudio.WAVBase64[:64]) {
		t.Fatal("inlined audio leaked into simulation state")
	}

	// Later caller actions are handed off in order on the call's lane.
	if rec, _, _ := h.trigger("sip_dtmf", map[string]any{"sip_call_id": sipID, "digits": "12#"}, false); rec.Code != http.StatusOK {
		t.Fatalf("sip_dtmf = %d %s", rec.Code, rec.Body.String())
	}
	if rec, _, _ := h.trigger("sip_caller_speaks", map[string]any{"sip_call_id": sipID}, false); rec.Code != http.StatusOK {
		t.Fatalf("sip_caller_speaks = %d %s", rec.Code, rec.Body.String())
	}
	if rec, _, deliveries := h.trigger("sip_caller_hangup", map[string]any{"sip_call_id": sipID, "cause": "user_busy", "q850": 17}, true); rec.Code != http.StatusOK || deliveries[0].State != "delivered" {
		t.Fatalf("sip_caller_hangup = %d %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(fake.kinds(), ","); got != "sip_inbound_call,sip_dtmf,sip_play,sip_hangup" {
		t.Fatalf("hand-off order = %s", got)
	}
	events = fake.received()
	var dtmf, play, hangup map[string]any
	_ = json.Unmarshal(events[1].Payload, &dtmf)
	_ = json.Unmarshal(events[2].Payload, &play)
	_ = json.Unmarshal(events[3].Payload, &hangup)
	if dtmf["digits"] != "12#" || dtmf["dtmf_type"] != "RFC 2833" || hangup["cause"] != "user_busy" || hangup["q850"] != float64(17) {
		t.Fatalf("dtmf %v hangup %v", dtmf, hangup)
	}
	if audio, _ := play["caller_audio"].(map[string]any); audio["name"] != "caller-speech-8k.wav" || audio["wav_base64"] != payload.CallerAudio.WAVBase64 {
		t.Fatalf("sip_play payload = %v", play)
	}
	for _, event := range events {
		if event.Collection != "sip_calls" || event.ID != sipID {
			t.Fatalf("hand-off for another record: %+v", event)
		}
	}

	// Reports merge; the mock reads them, so an ended call refuses more.
	report = h.sipCall(sipID)["report"].(map[string]any)
	if report["state"] != "ended" || report["ended_by"] != "caller" || report["codec"] != "PCMU" || report["q850"] != float64(17) {
		t.Fatalf("merged report = %v", report)
	}
	for name, input := range map[string]map[string]any{
		"sip_caller_hangup": {"sip_call_id": sipID},
		"sip_dtmf":          {"sip_call_id": sipID, "digits": "1"},
		"sip_caller_speaks": {"sip_call_id": sipID},
	} {
		if rec, _, _ := h.trigger(name, input, false); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "is ended") {
			t.Fatalf("%s on an ended call = %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec, _, _ := h.trigger("sip_dtmf", map[string]any{"sip_call_id": "sip_nope", "digits": "1"}, false); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "unknown sip_call_id") {
		t.Fatalf("sip_dtmf on an unknown call = %d %s", rec.Code, rec.Body.String())
	}

	// No Call Control webhook went anywhere, and nothing but hand-offs did.
	fake.mu.Lock()
	unexpected := append([]string(nil), fake.unexpected...)
	fake.mu.Unlock()
	if len(unexpected) > 0 || len(h.recorded()) > 0 {
		t.Fatalf("unexpected requests %v, call control events %v", unexpected, h.recorded())
	}
	for _, d := range h.state().Deliveries {
		if !strings.HasPrefix(d.Event, "handoff:") || d.State != "delivered" {
			t.Fatalf("delivery = %+v", d)
		}
	}

	// Evidence shows each hand-off (inlined audio elided) and each report.
	var handoffs, reports []InterceptedCall
	for _, call := range h.runtime.edge.Calls() {
		switch call.Kind {
		case "integration_handoff":
			handoffs = append(handoffs, call)
		case "integration_report":
			reports = append(reports, call)
		}
	}
	if len(handoffs) != 4 || handoffs[0].Event != "handoff:sip_inbound_call" || handoffs[0].Status != http.StatusOK ||
		!strings.Contains(handoffs[0].ReqBody, `"from":"+15550100999"`) || strings.Contains(handoffs[0].ReqBody, payload.CallerAudio.WAVBase64[:64]) ||
		!strings.Contains(handoffs[0].RespBody, "sip_inbound_call") {
		t.Fatalf("hand-off evidence = %+v", handoffs)
	}
	if len(reports) != 3 || reports[0].Event != "ringing" || reports[2].Event != "ended" || !strings.Contains(reports[2].ReqBody, `"ended_by":"caller"`) {
		t.Fatalf("report evidence = %+v", reports)
	}

	// Snapshots capture reports.
	captured := false
	for _, snap := range h.runtime.SimulationSnapshots() {
		if snap.Slug == "telnyx" {
			record := snap.State.Collections["sip_calls"].Records[sipID]
			captured = record["report"].(map[string]any)["state"] == "ended"
		}
	}
	if !captured {
		t.Fatal("snapshot lost the report")
	}
}

func TestTelnyxDirectSIPSetupAndCallControlNumbers(t *testing.T) {
	h, fake := newSIPHarness(t)
	// Telephony refuses TLS with required SRTP; so does the simulation.
	if res := h.tool("create_fqdn_connection", map[string]any{"connection_name": "x", "transport_protocol": "TLS", "encrypted_media": "SRTP"}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("TLS + SRTP = %d %v", res.Status, res.Data)
	}
	if res := h.tool("create_fqdn_connection", map[string]any{"connection_name": "x", "transport_protocol": "WSS"}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("bad transport = %d %v", res.Status, res.Data)
	}
	connection := configureDirectSIP(t, h, "SRTP")

	// A connection without an FQDN cannot take calls.
	bare := dataID(t, h.tool("create_fqdn_connection", map[string]any{"connection_name": "bare", "transport_protocol": "TCP"}))
	if res := h.tool("update_phone_number", map[string]any{"id": h.state().Collections["phone_numbers"][0]["id"], "connection_id": bare}); !res.Success {
		t.Fatalf("assign to bare connection: %v", res.Data)
	}
	if rec, _, _ := h.trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999"}, false); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "has no FQDN") {
		t.Fatalf("incoming_call on a bare connection = %d %s", rec.Code, rec.Body.String())
	}

	// Silence and bad audio, on the configured connection.
	numberID := h.state().Collections["phone_numbers"][0]["id"]
	if res := h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": connection}); !res.Success {
		t.Fatal(res.Data)
	}
	if rec, _, _ := h.trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999", "caller_audio": "call-recording-mono.wav"}, false); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("incoming_call with recording audio = %d %s", rec.Code, rec.Body.String())
	}
	rec, result, _ := h.trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999", "caller_audio": "silence"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("incoming_call = %d %s", rec.Code, rec.Body.String())
	}
	events := fake.received()
	var payload map[string]any
	_ = json.Unmarshal(events[len(events)-1].Payload, &payload)
	if audio, _ := payload["caller_audio"].(map[string]any); len(audio) != 1 || audio["name"] != "silence" || payload["encrypted_media"] != "SRTP" {
		t.Fatalf("silent SRTP call payload = %v", payload)
	}
	if h.sipCall(result["sip_call_id"].(string))["encrypted_media"] != "SRTP" {
		t.Fatal("record lost encrypted_media")
	}

	// Rollback the way Telephony does it: back to the application, then
	// delete the FQDN and the connection.
	var application string
	for _, app := range h.state().Collections["call_control_applications"] {
		application = app["id"].(string)
	}
	var fqdn string
	for _, f := range h.state().Collections["fqdns"] {
		if f["connection_id"] == connection {
			fqdn = f["id"].(string)
		}
	}
	if res := h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": application}); !res.Success {
		t.Fatal(res.Data)
	}
	if res := h.tool("delete_fqdn", map[string]any{"id": fqdn}); !res.Success {
		t.Fatal(res.Data)
	}
	if res := h.tool("delete_fqdn_connection", map[string]any{"id": connection}); !res.Success {
		t.Fatal(res.Data)
	}
	if res := h.tool("delete_fqdn_connection", map[string]any{"id": connection}); res.Status != http.StatusNotFound {
		t.Fatalf("second delete = %d", res.Status)
	}

	// The number is a Call Control number again: webhooks, no hand-off.
	before := len(fake.received())
	rec, result, deliveries := h.trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999"}, false)
	if rec.Code != http.StatusOK || result["call_control_id"] == nil || result["sip_call_id"] != nil || len(deliveries) != 1 || deliveries[0].Event != "call.initiated" {
		t.Fatalf("call control incoming_call = %d %v %+v", rec.Code, result, deliveries)
	}
	if len(fake.received()) != before {
		t.Fatal("a Call Control call was handed off")
	}
}

func TestIntegrationSimulationHandoffRetriesAndReportsFailures(t *testing.T) {
	h, fake := newSIPHarness(t)
	configureDirectSIP(t, h, nil)
	var attemptsMu sync.Mutex
	attempts := map[string]int{}
	fake.setAnswer(func(event sdk.IntegrationSimulationEvent) (int, any) {
		attemptsMu.Lock()
		attempts[event.Kind]++
		n := attempts[event.Kind]
		attemptsMu.Unlock()
		switch {
		case event.Kind == "sip_inbound_call" && n == 1:
			return http.StatusServiceUnavailable, map[string]any{"error": "gateway warming up"}
		case event.Kind == "sip_dtmf":
			return http.StatusUnprocessableEntity, map[string]any{"error": "call is not answered"}
		case event.Kind == "sip_play":
			return http.StatusInternalServerError, map[string]any{"error": "rtp sender crashed"}
		}
		return http.StatusOK, nil
	})
	_, result, deliveries := h.trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999"}, true)
	sipID := result["sip_call_id"].(string)
	if deliveries[0].State != "delivered" || deliveries[0].Attempts != 2 {
		t.Fatalf("retried hand-off = %+v", deliveries[0])
	}
	if report := h.sipCall(sipID)["report"]; report != nil {
		t.Fatalf("delivered hand-off left a report: %v", report)
	}

	// 4xx is final: one attempt, and the record says why.
	_, _, deliveries = h.trigger("sip_dtmf", map[string]any{"sip_call_id": sipID, "digits": "5"}, true)
	if d := deliveries[0]; d.State != "failed" || d.Attempts != 1 || d.Status != http.StatusUnprocessableEntity || !strings.Contains(d.Error, "app responded 422: call is not answered") {
		t.Fatalf("4xx hand-off = %+v", d)
	}
	report := h.sipCall(sipID)["report"].(map[string]any)
	if report["state"] != "handoff_failed" || !strings.Contains(fmt.Sprint(report["error"]), "handoff sip_dtmf: app responded 422: call is not answered") {
		t.Fatalf("report after 4xx = %v", report)
	}

	// 5xx is retried up to delivery.max_attempts (3 for Telnyx).
	_, _, deliveries = h.trigger("sip_caller_speaks", map[string]any{"sip_call_id": sipID}, true)
	if d := deliveries[0]; d.State != "failed" || d.Attempts != 3 || d.Status != http.StatusInternalServerError || !strings.Contains(d.Error, "rtp sender crashed") {
		t.Fatalf("5xx hand-off = %+v", d)
	}
	if report := h.sipCall(sipID)["report"].(map[string]any); !strings.Contains(fmt.Sprint(report["error"]), "handoff sip_play") {
		t.Fatalf("report after 5xx = %v", report)
	}
	attemptsMu.Lock()
	if attempts["sip_play"] != 3 || attempts["sip_dtmf"] != 1 {
		t.Fatalf("attempts = %v", attempts)
	}
	attemptsMu.Unlock()

	// The app can still report afterwards, and a failed hand-off does not
	// end the call.
	if rec := h.report(fake.token, nil, sdk.IntegrationSimulationReport{Integration: "telnyx", Collection: "sip_calls", ID: sipID, Fields: map[string]any{"state": "answered", "error": nil}}); rec.Code != http.StatusOK {
		t.Fatalf("report = %d %s", rec.Code, rec.Body.String())
	}
	if _, _, deliveries := h.trigger("sip_caller_hangup", map[string]any{"sip_call_id": sipID}, true); deliveries[0].State != "delivered" {
		t.Fatalf("hangup = %+v", deliveries)
	}
}

func TestIntegrationSimulationReportAuthAndLimits(t *testing.T) {
	h, fake := newSIPHarness(t)
	configureDirectSIP(t, h, nil)
	_, result, _ := h.trigger("incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999"}, true)
	sipID := result["sip_call_id"].(string)
	valid := sdk.IntegrationSimulationReport{Integration: "telnyx", Collection: "sip_calls", ID: sipID, Event: "ringing", Fields: map[string]any{"state": "ringing"}}
	expect := func(what string, rec *httptest.ResponseRecorder, status int, contains string) {
		t.Helper()
		if rec.Code != status || !strings.Contains(rec.Body.String(), contains) {
			t.Fatalf("%s = %d %s; want %d %q", what, rec.Code, rec.Body.String(), status, contains)
		}
	}

	// Principals: only the app that owns the simulated connection, in its
	// live environment.
	expect("no credential", h.report("", nil, valid), http.StatusUnauthorized, "unauthorized")
	userKey := "sk_simulation_report_user"
	if _, err := h.s.store.CreateAPIKey(1, "test", HashAPIKey(userKey), "sk_simulat"); err != nil {
		t.Fatal(err)
	}
	expect("user API key", h.report(userKey, nil, valid), http.StatusForbidden, "come from the app")
	ownerToken, err := h.s.appInstallToken(h.owner)
	if err != nil {
		t.Fatal(err)
	}
	expect("app outside the environment", h.report(ownerToken, nil, valid), http.StatusForbidden, "not running in a live environment")
	expect("mismatched environment header", h.report(fake.token, http.Header{"X-Apteva-Environment-Id": {"rt-other"}}, valid), http.StatusForbidden, "does not match")
	if _, err := h.s.store.db.Exec(`UPDATE app_installs SET source = 'git' WHERE id = ?`, h.install); err != nil {
		t.Fatal(err)
	}
	expect("install that is not an environment install", h.report(fake.token, nil, valid), http.StatusForbidden, "not running in a live environment")
	if _, err := h.s.store.db.Exec(`UPDATE app_installs SET source = 'environment' WHERE id = ?`, h.install); err != nil {
		t.Fatal(err)
	}
	other := seedRunningInstall(t, h.s, "crm", "rt-sim", sdk.Manifest{Name: "crm"}, nil)
	if _, err := h.s.store.db.Exec(`UPDATE app_installs SET source = 'environment' WHERE id = ?`, other); err != nil {
		t.Fatal(err)
	}
	h.runtime.mu.Lock()
	h.runtime.installs["crm"] = &localInstall{InstallID: other, AppName: "crm", ProjectID: "rt-sim", SidecarURL: h.sidecar.URL}
	h.runtime.mu.Unlock()
	otherToken, err := h.s.appInstallToken(other)
	if err != nil {
		t.Fatal(err)
	}
	expect("another app in the environment", h.report(otherToken, nil, valid), http.StatusNotFound, `crm\" owns no simulated`)

	// Selection and validation.
	wrongConnection := valid
	wrongConnection.ConnectionID = h.connID + 1000
	expect("wrong connection", h.report(fake.token, nil, wrongConnection), http.StatusNotFound, "owns no simulated")
	unknownIntegration := valid
	unknownIntegration.Integration = "twilio"
	expect("unknown integration", h.report(fake.token, nil, unknownIntegration), http.StatusNotFound, "owns no simulated")
	notReportable := valid
	notReportable.Collection = "calls"
	expect("collection that is not reportable", h.report(fake.token, nil, notReportable), http.StatusUnprocessableEntity, "not reportable")
	unknownCollection := valid
	unknownCollection.Collection = "nope"
	expect("unknown collection", h.report(fake.token, nil, unknownCollection), http.StatusUnprocessableEntity, "no collection")
	missing := valid
	missing.ID = "sip_missing"
	expect("missing record", h.report(fake.token, nil, missing), http.StatusNotFound, "not found")
	empty := valid
	empty.Fields = nil
	expect("no fields", h.report(fake.token, nil, empty), http.StatusBadRequest, "fields are required")
	expect("bad JSON", h.report(fake.token, nil, []byte("{")), http.StatusBadRequest, "invalid JSON")
	expect("oversized request", h.report(fake.token, nil, []byte(`{"fields":{"x":"`+strings.Repeat("a", mockMaxReportRequest)+`"}}`)), http.StatusRequestEntityTooLarge, "over")
	req := httptest.NewRequest(http.MethodGet, "/integration-simulation/report", nil)
	req.Header.Set("Authorization", "Bearer "+fake.token)
	rec := httptest.NewRecorder()
	h.s.authMiddleware(h.s.handleIntegrationSimulationReport)(rec, req)
	expect("GET", rec, http.StatusMethodNotAllowed, "POST only")

	// Merge and the per-record cap.
	expect("first report", h.report(fake.token, nil, valid), http.StatusOK, `"state":"ringing"`)
	second := valid
	second.ConnectionID = h.connID
	second.Event = "answered"
	second.Fields = map[string]any{"state": "answered", "codec": "PCMA", "rtp": map[string]any{"sent_packets": 3}}
	expect("second report", h.report(fake.token, nil, second), http.StatusOK, `"codec":"PCMA"`)
	big := valid
	big.Fields = map[string]any{"notes": strings.Repeat("n", 20<<10)}
	expect("report under the cap", h.report(fake.token, nil, big), http.StatusOK, `"ok":true`)
	big.Fields = map[string]any{"more": strings.Repeat("m", 20<<10)}
	expect("report over the cap", h.report(fake.token, nil, big), http.StatusRequestEntityTooLarge, "over the 32768 byte limit")
	report := h.sipCall(sipID)["report"].(map[string]any)
	if report["state"] != "answered" || report["codec"] != "PCMA" || report["more"] != nil || len(fmt.Sprint(report["notes"])) != 20<<10 {
		t.Fatalf("merged report = %.200v", report)
	}
	if rtp, _ := report["rtp"].(map[string]any); rtp["sent_packets"] != float64(3) {
		t.Fatalf("rtp = %v", report["rtp"])
	}
}

func TestEnvironmentGatewaysRefuseAppControlRoutes(t *testing.T) {
	h, fake := newSIPHarness(t)
	for _, path := range []string{
		"/environment-app-public/rt-sim/api/apps/telephony" + sdk.IntegrationSimulationPath,
		fmt.Sprintf("/environment-app-public/rt-sim/api/apps/telephony/_install/%d%s", h.install, sdk.IntegrationSimulationPath),
		"/environment-app-public/rt-sim/api/apps/telephony//_apteva/runtime/integration-simulation",
		"/environment-app-public/rt-sim/api/apps/telephony/x/../_apteva/runtime/app/describe",
		"/environment-app-public/rt-sim/api/apps/telephony/_apteva",
		"/environment-app-gateway/rt-sim/telephony" + sdk.IntegrationSimulationPath,
		"/environment-app-gateway/rt-sim/agent-7/telephony" + sdk.IntegrationSimulationPath,
	} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(`{"integration":"telnyx","kind":"sip_inbound_call"}`))
		req.URL.Path = path
		req.RemoteAddr = "127.0.0.1:5555"
		rec := httptest.NewRecorder()
		if strings.HasPrefix(path, "/environment-app-gateway/") {
			h.s.handleEnvironmentAppGateway(rec, req)
		} else {
			h.s.handleEnvironmentAppPublicGateway(rec, req)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
	fake.mu.Lock()
	reached := len(fake.events) + len(fake.unexpected)
	fake.mu.Unlock()
	if reached != 0 {
		t.Fatalf("a gateway forwarded an app control route: %v %v", fake.events, fake.unexpected)
	}

	// Ordinary routes still go through, and the server's own hand-off
	// delivery still reaches the control route.
	req := httptest.NewRequest(http.MethodGet, "/environment-app-public/rt-sim/api/apps/telephony/inbound/telnyx/route-1", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.s.handleEnvironmentAppPublicGateway(rec, req)
	fake.mu.Lock()
	forwarded := len(fake.unexpected) == 1 && fake.unexpected[0] == "GET /inbound/telnyx/route-1"
	fake.mu.Unlock()
	if !forwarded {
		t.Fatalf("ordinary route = %d, sidecar saw %v", rec.Code, fake.unexpected)
	}
	status, body, err := h.s.deliverSimulationHandoff(context.Background(), h.runtime, "telephony", []byte(`{"integration":"telnyx","kind":"probe"}`))
	if err != nil || status != http.StatusOK || len(fake.received()) != 1 {
		t.Fatalf("internal hand-off = %d %s %v", status, body, err)
	}
	if _, _, err := h.s.deliverSimulationHandoff(context.Background(), h.runtime, "ghost", nil); err == nil {
		t.Fatal("hand-off to an app outside the runtime was delivered")
	}
}

// --- the DSL itself, on a small simulation ---

func handoffTestTemplate(t *testing.T, raw string) *AppTemplate {
	t.Helper()
	var mock AppMockConfig
	if err := json.Unmarshal([]byte(raw), &mock); err != nil {
		t.Fatal(err)
	}
	return &AppTemplate{Slug: "sipdemo", Tools: []AppToolDef{{Name: "noop"}}, Mock: &mock}
}

func TestMockHandoffStepCompilesAndQueues(t *testing.T) {
	for name, steps := range map[string]string{
		"collection without id":  `[{"handoff":"work","collection":"jobs"},{"respond":{}}]`,
		"unknown collection":     `[{"handoff":"work","collection":"nope","id":"x"},{"respond":{}}]`,
		"empty kind":             `[{"handoff":" "},{"respond":{}}]`,
		"payload not an object":  `[{"handoff":"work","payload":"x"},{"respond":{}}]`,
		"collection elsewhere":   `[{"create":"jobs","collection":"jobs"},{"respond":{}}]`,
		"unknown inline fixture": `[{"handoff":"work","payload":{"a":"{{fixture_base64('nope.wav')}}"}},{"respond":{}}]`,
	} {
		tmpl := handoffTestTemplate(t, `{"version":1,"collections":{"jobs":{}},"tools":{"noop":{"steps":[{"respond":{}}]}},"triggers":{"go":{"description":"d","input_schema":{"type":"object"},"steps":`+steps+`}}}`)
		if _, err := compileAppMock(tmpl); err == nil {
			t.Fatalf("%s compiled", name)
		}
	}

	tmpl := handoffTestTemplate(t, `{"version":1,
		"collections":{"jobs":{"id":{"token":"job_"},"reportable":true},"plain":{}},
		"tools":{"noop":{"steps":[{"respond":{}}]}},
		"triggers":{
			"start":{"description":"d","input_schema":{"type":"object"},"steps":[
				{"create":"jobs","record":{"name":"{{input.name}}"},"as":"job"},
				{"handoff":"{{input.kind || 'work'}}","collection":"{{'jobs'}}","id":"{{job.id}}","payload":{"name":"{{job.name}}","audio":"{{fixture_base64('caller-speech-8k.wav')}}"}},
				{"handoff":"ping","lane":"pings"},
				{"handoff":"pong"},
				{"respond":{"id":"{{job.id}}"}}]},
			"ghost":{"description":"d","input_schema":{"type":"object"},"steps":[
				{"handoff":"work","collection":"jobs","id":"job_nope"},{"respond":{}}]},
			"ghost_error":{"description":"d","input_schema":{"type":"object"},"steps":[
				{"handoff":"work","collection":"jobs","id":"job_nope","error":"gone"},{"respond":{}}]}
		},
		"errors":{"gone":{"status":404,"body":{"detail":"{{detail}}"}}}}`)
	mock, err := compileAppMock(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if !mock.collections["jobs"].reportable || mock.collections["plain"].reportable {
		t.Fatal("reportable not compiled")
	}

	var mu sync.Mutex
	var bodies [][]byte
	var apps []string
	sim := newIntegrationSimulation("env-1", "sipdemo", "worker", "", 7, tmpl, mock)
	t.Cleanup(sim.stop)
	sim.handOff = func(_ context.Context, app string, body []byte) (int, []byte, error) {
		mu.Lock()
		defer mu.Unlock()
		bodies, apps = append(bodies, body), append(apps, app)
		return http.StatusOK, []byte(`{"ok":true}`), nil
	}
	outcome := sim.execute(&mock.triggers["start"].compiledMockOperation, map[string]any{"name": "a", "kind": "build"})
	if outcome.status != http.StatusOK || len(outcome.deliveries) != 3 {
		t.Fatalf("start = %d %v %d", outcome.status, outcome.body, len(outcome.deliveries))
	}
	waitForDeliveries(context.Background(), outcome.deliveries, 5*time.Second)
	jobID := outcome.body.(map[string]any)["id"].(string)
	records := sim.deliveryRecords(outcome.deliveries)
	if records[0].Event != "handoff:build" || records[0].Lane != jobID || records[1].Lane != "pings" || records[2].Lane != "handoff:pong" ||
		records[0].App != "worker" || records[0].Target != "/api/apps/worker"+sdk.IntegrationSimulationPath {
		t.Fatalf("records = %+v", records)
	}
	// The three lanes run concurrently; find the job's hand-off.
	mu.Lock()
	var first sdk.IntegrationSimulationEvent
	for i, body := range bodies {
		var event sdk.IntegrationSimulationEvent
		_ = json.Unmarshal(body, &event)
		if event.Kind == "build" {
			first = event
			if apps[i] != "worker" {
				t.Fatalf("handed to %q", apps[i])
			}
		}
	}
	got := len(bodies)
	mu.Unlock()
	var payload map[string]any
	_ = json.Unmarshal(first.Payload, &payload)
	fixture, _ := mockFixtureByName("caller-speech-8k.wav")
	if got != 3 || first.Integration != "sipdemo" || first.ConnectionID != 7 || first.Kind != "build" ||
		first.Collection != "jobs" || first.ID != jobID || payload["name"] != "a" || payload["audio"] != base64.StdEncoding.EncodeToString(fixture.data) {
		t.Fatalf("hand-off = %+v %v", first, payload)
	}
	for _, d := range outcome.deliveries {
		if d.payload != nil {
			t.Fatal("delivered hand-off kept its payload")
		}
	}

	if outcome := sim.execute(&mock.triggers["ghost"].compiledMockOperation, nil); outcome.status != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(outcome.body), `jobs "job_nope" not found`) {
		t.Fatalf("ghost = %d %v", outcome.status, outcome.body)
	}
	if outcome := sim.execute(&mock.triggers["ghost_error"].compiledMockOperation, nil); outcome.status != http.StatusNotFound {
		t.Fatalf("ghost_error = %d %v", outcome.status, outcome.body)
	}

	// A simulation no app owns cannot hand work off; the record says so.
	orphan := newIntegrationSimulation("env-1", "sipdemo", "", "", 8, tmpl, mock)
	t.Cleanup(orphan.stop)
	outcome = orphan.execute(&mock.triggers["start"].compiledMockOperation, map[string]any{"name": "b"})
	waitForDeliveries(context.Background(), outcome.deliveries, 5*time.Second)
	if d := orphan.deliveryRecords(outcome.deliveries)[0]; d.State != "undeliverable" || !strings.Contains(d.Error, "no owning app") {
		t.Fatalf("orphan delivery = %+v", d)
	}
	job := orphan.publicState().Collections["jobs"][0]
	if report, _ := job["report"].(map[string]any); report["state"] != "handoff_failed" {
		t.Fatalf("orphan record = %v", job)
	}

	// Reports need a reportable collection and an existing record.
	if _, status, _ := sim.applyReport(sdk.IntegrationSimulationReport{Collection: "plain", ID: "x", Fields: map[string]any{"a": 1}}); status != http.StatusUnprocessableEntity {
		t.Fatalf("plain = %d", status)
	}
	merged, status, err := sim.applyReport(sdk.IntegrationSimulationReport{Collection: "jobs", ID: jobID, Fields: map[string]any{"state": "done"}})
	if err != nil || merged["state"] != "done" || status != 0 {
		t.Fatalf("report = %v %d %v", merged, status, err)
	}
	if _, err := mockFixtureBase64("nope.wav"); err == nil {
		t.Fatal("unknown fixture inlined")
	}
}
