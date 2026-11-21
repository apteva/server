package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// deepgramSim is a runtime whose Media app has a simulated Deepgram
// connection, the way Media and Functions reach Deepgram: through the
// catalog's listen tool.
type deepgramSim struct {
	t      *testing.T
	s      *Server
	owner  int64
	connID int64
	app    *AppTemplate
	creds  map[string]string
}

func newDeepgramSim(t *testing.T) *deepgramSim {
	t.Helper()
	s := newRuntimeAPITestServer(t)
	s.catalog = loadEmbeddedCatalog(t)
	d := &deepgramSim{t: t, s: s, app: s.catalog.Get("deepgram")}
	d.owner = seedRuntimeAPIInstall(t, s, "environments", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	manifest := sdk.Manifest{Name: "media", Requires: sdk.Requires{Integrations: []sdk.IntegrationDep{{Role: "transcripts", CompatibleSlugs: []string{"deepgram"}}}}}
	install := seedRunningInstall(t, s, "media", "rt-dg", manifest, nil)
	runtime := &Environment{
		ID: "rt-dg", ProjectID: "rt-dg", ownerInstallID: d.owner, edge: &EnvironmentEdge{},
		installs: map[string]*localInstall{"media": {InstallID: install, AppName: "media", ProjectID: "rt-dg"}},
		agents:   map[int64]*EnvironmentAgent{}, agentAliases: map[string]int64{}, createdAt: time.Now(),
	}
	s.environments.mu.Lock()
	s.environments.environments[runtime.ID] = runtime
	s.environments.mu.Unlock()
	t.Cleanup(runtime.stopSimulations)
	if err := s.bindEnvironmentIntegrationMocks(1, runtime, []RuntimeIntegrationBinding{{
		App: "media", Role: "transcripts", Slug: "deepgram", Simulate: true, Credentials: map[string]string{"api_key": "test-key"},
	}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	connID, ok := asInt64(readBindings(t, s, install)["transcripts"])
	if !ok {
		t.Fatal("transcripts not bound")
	}
	d.connID = connID
	d.creds = map[string]string{"api_key": "test-key"}
	return d
}

func (d *deepgramSim) listen(input map[string]any) *ExecuteResult {
	d.t.Helper()
	var tool *AppToolDef
	for i := range d.app.Tools {
		if d.app.Tools[i].Name == "listen" {
			tool = &d.app.Tools[i]
		}
	}
	res, err := d.s.executeConnectionToolWithRefreshContext(context.Background(), d.connID, d.app, tool, d.creds, input, "")
	if err != nil {
		d.t.Fatalf("listen: %v", err)
	}
	return res
}

func (d *deepgramSim) trigger(name string, input map[string]any) (int, string) {
	d.t.Helper()
	rec := runtimeAPIRequest(d.t, d.s, d.owner, http.MethodPost, "/apps/callback/runtimes/rt-dg/integrations/deepgram/triggers/"+name, sdk.RuntimeIntegrationTriggerRequest{
		RuntimeIntegrationSelector: sdk.RuntimeIntegrationSelector{App: "media", Role: "transcripts"}, Input: input, Wait: true,
	})
	return rec.Code, rec.Body.String()
}

func (d *deepgramSim) state() sdk.RuntimeIntegrationState {
	d.t.Helper()
	rec := runtimeAPIRequest(d.t, d.s, d.owner, http.MethodGet, "/apps/callback/runtimes/rt-dg/integrations/deepgram/state?app=media&role=transcripts", nil)
	var state sdk.RuntimeIntegrationState
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &state) != nil {
		d.t.Fatalf("state = %d %s", rec.Code, rec.Body.String())
	}
	return state
}

// mediaListen is the request Media 0.17 sends.
func mediaListen() map[string]any {
	return map[string]any{"url": "https://storage.example.test/signed/recording.wav", "model": "nova-3", "smart_format": true, "paragraphs": true, "utterances": true, "detect_language": true}
}

// deepgramTranscript decodes a listen response the way Media and the
// Flexylead transcription function read it.
type deepgramTranscript struct {
	Metadata struct {
		RequestID string  `json:"request_id"`
		Duration  float64 `json:"duration"`
	} `json:"metadata"`
	Results struct {
		Channels []struct {
			DetectedLanguage string `json:"detected_language"`
			Alternatives     []struct {
				Transcript string `json:"transcript"`
				Words      []struct {
					Word  string   `json:"word"`
					Start *float64 `json:"start"`
					End   *float64 `json:"end"`
				} `json:"words"`
				Paragraphs struct {
					Paragraphs []struct {
						Sentences []struct {
							Text  string  `json:"text"`
							Start float64 `json:"start"`
							End   float64 `json:"end"`
						} `json:"sentences"`
					} `json:"paragraphs"`
				} `json:"paragraphs"`
			} `json:"alternatives"`
		} `json:"channels"`
		Utterances []struct {
			Transcript string `json:"transcript"`
		} `json:"utterances"`
	} `json:"results"`
}

func decodeTranscript(t *testing.T, res *ExecuteResult) deepgramTranscript {
	t.Helper()
	raw, _ := json.Marshal(res.Data)
	var out deepgramTranscript
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Results.Channels) != 1 || len(out.Results.Channels[0].Alternatives) != 1 {
		t.Fatalf("listen response = %s (%v)", raw, err)
	}
	return out
}

func TestDeepgramSimulationTranscribesWithTheFixture(t *testing.T) {
	d := newDeepgramSim(t)
	res := d.listen(mediaListen())
	if !res.Success || res.Status != http.StatusOK {
		t.Fatalf("listen = %+v", res)
	}
	got := decodeTranscript(t, res)
	alt := got.Results.Channels[0].Alternatives[0]
	if alt.Transcript != "Hello, I would like an appointment. Sure, which day?" || len(alt.Words) != 9 || got.Metadata.Duration != 3.89 || got.Metadata.RequestID == "" {
		t.Fatalf("transcript = %+v", got)
	}
	// Words fall inside the recording fixtures, which last 3.89 s.
	for _, word := range alt.Words {
		if word.Start == nil || word.End == nil || *word.End < *word.Start || *word.End > 3.89 {
			t.Fatalf("word %+v", word)
		}
	}
	if got.Results.Channels[0].DetectedLanguage != "en" || len(alt.Paragraphs.Paragraphs) != 1 || len(got.Results.Utterances) != 1 || got.Results.Utterances[0].Transcript != alt.Transcript {
		t.Fatalf("paragraphs/utterances/language = %+v", got)
	}
	// The Flexylead transcription function: French, no paragraphs.
	plain := d.listen(map[string]any{"url": "https://storage.example.test/r.wav", "language": "fr", "model": "nova-3", "smart_format": true, "punctuate": true, "diarize": true})
	raw, _ := json.Marshal(plain.Data)
	if !plain.Success || strings.Contains(string(raw), "paragraphs") || strings.Contains(string(raw), "utterances") || strings.Contains(string(raw), "detected_language") {
		t.Fatalf("plain listen = %s", raw)
	}
	for name, input := range map[string]map[string]any{
		"no audio": {"model": "nova-3"},
		"callback": {"url": "https://storage.example.test/r.wav", "callback": "https://example.com/cb"},
	} {
		res := d.listen(input)
		if res.Status != http.StatusBadRequest || !strings.Contains(mockString(res.Data), `"err_code":"Bad Request"`) {
			t.Fatalf("%s: %+v", name, res)
		}
	}
}

func TestDeepgramSimulationQueuedAndDefaultOutcomes(t *testing.T) {
	d := newDeepgramSim(t)
	for _, outcome := range []string{"payment_required", "ok", "timeout", "server_error", "bad_audio"} {
		input := map[string]any{"outcome": outcome}
		if outcome == "ok" {
			input["transcript"] = "Bonjour, je voudrais un rendez-vous."
			input["language"] = "fr"
			input["duration"] = 2.5
		}
		if code, body := d.trigger("queue_response", input); code != http.StatusOK {
			t.Fatalf("queue %s = %d %s", outcome, code, body)
		}
	}
	if code, body := d.trigger("queue_response", map[string]any{"outcome": "nope"}); code != http.StatusUnprocessableEntity || !strings.Contains(body, `"status":400`) {
		t.Fatalf("unknown outcome = %d %s", code, body)
	}

	// Queued answers are used once each, oldest first.
	res := d.listen(mediaListen())
	body, _ := json.Marshal(res.Data)
	if res.Status != http.StatusPaymentRequired || !strings.Contains(string(body), `"err_code":"ASR_PAYMENT_REQUIRED"`) ||
		!strings.Contains(string(body), "Project does not have enough credits for this request and does not have an overage agreement.") {
		t.Fatalf("payment_required = %d %s", res.Status, body)
	}
	custom := decodeTranscript(t, d.listen(mediaListen()))
	if alt := custom.Results.Channels[0].Alternatives[0]; alt.Transcript != "Bonjour, je voudrais un rendez-vous." || len(alt.Words) != 0 ||
		custom.Metadata.Duration != 2.5 || custom.Results.Channels[0].DetectedLanguage != "fr" || alt.Paragraphs.Paragraphs[0].Sentences[0].End != 2.5 {
		t.Fatalf("custom transcript = %+v", custom)
	}
	for _, want := range []int{http.StatusGatewayTimeout, http.StatusInternalServerError, http.StatusBadRequest} {
		if res := d.listen(mediaListen()); res.Status != want || res.Success {
			t.Fatalf("queued failure = %+v, want %d", res, want)
		}
	}
	if res := d.listen(mediaListen()); !res.Success {
		t.Fatalf("empty queue falls back to the fixture: %+v", res)
	}

	// A default answer lasts until cleared; queued answers still come first.
	if code, body := d.trigger("set_default_response", map[string]any{"outcome": "payment_required"}); code != http.StatusOK {
		t.Fatalf("set default = %d %s", code, body)
	}
	d.trigger("queue_response", map[string]any{})
	if res := d.listen(mediaListen()); !res.Success {
		t.Fatalf("queued ok before the default = %+v", res)
	}
	for i := 0; i < 2; i++ {
		if res := d.listen(mediaListen()); res.Status != http.StatusPaymentRequired {
			t.Fatalf("default payment_required = %+v", res)
		}
	}
	if code, body := d.trigger("set_default_response", map[string]any{"clear": true}); code != http.StatusOK {
		t.Fatalf("clear default = %d %s", code, body)
	}
	if res := d.listen(mediaListen()); !res.Success {
		t.Fatalf("after clear = %+v", res)
	}

	// Every request is logged with its outcome; used answers are marked.
	state := d.state()
	var outcomes []string
	for _, request := range state.Collections["requests"] {
		outcomes = append(outcomes, mockString(request["outcome"]))
		if request["url"] != "https://storage.example.test/signed/recording.wav" || request["model"] != "nova-3" {
			t.Fatalf("request = %v", request)
		}
	}
	if got := strings.Join(outcomes, ","); got != "payment_required,ok,timeout,server_error,bad_audio,ok,ok,payment_required,payment_required,ok" {
		t.Fatalf("request outcomes = %s", got)
	}
	for _, response := range state.Collections["responses"] {
		if response["used"] != true || response["request_id"] == nil {
			t.Fatalf("unused queued answer = %v", response)
		}
	}
	if len(state.Collections["default_responses"]) != 0 {
		t.Fatalf("default not cleared: %v", state.Collections["default_responses"])
	}
}

func TestDeepgramSimulationSeededDefault(t *testing.T) {
	d := newDeepgramSim(t)
	// A binding can also seed the default, e.g. a project out of credits
	// from the start.
	sim := lookupIntegrationSimulation("rt-dg", d.connID)
	if err := sim.seed(map[string][]map[string]any{"default_responses": {{"id": "listen", "outcome": "server_error"}}}); err != nil {
		t.Fatal(err)
	}
	if res := d.listen(mediaListen()); res.Status != http.StatusInternalServerError || !strings.Contains(mockString(res.Data), "INTERNAL_SERVER_ERROR") {
		t.Fatalf("seeded default = %+v", res)
	}
}
