package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/gorilla/websocket"
)

// telnyxApp stands in for Telephony behind a simulated Telnyx connection: it
// verifies and records every signed webhook, and serves the media WebSocket
// the simulation connects to. The media path picks the app's behavior:
// /talk/ answers with audio, marks and a clear; /close/ hangs up the socket
// after the start frame; /reject/ refuses the upgrade; anything else listens.
type telnyxApp struct {
	h      *telnyxSimHarness
	mu     sync.Mutex
	appID  string
	events []telnyxAppEvent
	logs   map[string]*mediaLog
}

type telnyxAppEvent struct {
	path, query, event string
	payload            map[string]any
}

const telnyxAppMediaURL = "wss://runtime.apteva.invalid/rt-sim/api/apps/telephony/media/telnyx/"

func newTelnyxApp(t *testing.T) *telnyxApp {
	t.Helper()
	h := newTelnyxSimHarness(t)
	a := &telnyxApp{h: h, logs: map[string]*mediaLog{}}
	h.setHandler(a)
	sim := h.runtime.findSimulation("telnyx", "telephony", "carrier")
	sim.mu.Lock()
	sim.mediaFrame = time.Millisecond
	sim.mu.Unlock()
	webhook := fmt.Sprintf("https://runtime.apteva.invalid/rt-sim/api/apps/telephony/_install/%d/inbound/telnyx/route-1", h.install)
	created := h.tool("create_call_control_application", map[string]any{"application_name": "Apteva route-1", "webhook_event_url": webhook, "active": true})
	if created.Status != http.StatusCreated {
		t.Fatalf("create application = %+v", created)
	}
	appID := created.Data.(map[string]any)["data"].(map[string]any)["id"].(string)
	a.mu.Lock()
	a.appID = appID
	a.mu.Unlock()
	h.appID = appID
	numbers := h.tool("list_phone_numbers", map[string]any{})
	numberID := numbers.Data.(map[string]any)["data"].([]any)[0].(map[string]any)["id"]
	if res := h.tool("update_phone_number", map[string]any{"id": numberID, "connection_id": appID}); !res.Success {
		t.Fatalf("assign number = %+v", res)
	}
	return a
}

func (a *telnyxApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/media/telnyx/") {
		a.serveMedia(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	if err := telnyxVerify(a.h.creds["public_key"], r.Header.Get("Telnyx-Timestamp"), r.Header.Get("Telnyx-Signature-Ed25519"), body); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var event struct {
		Data struct {
			EventType string         `json:"event_type"`
			Payload   map[string]any `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if event.Data.Payload["connection_id"] != a.appID {
		http.Error(w, "webhook connection does not match route", http.StatusForbidden)
		return
	}
	a.events = append(a.events, telnyxAppEvent{path: r.URL.Path, query: r.URL.RawQuery, event: event.Data.EventType, payload: event.Data.Payload})
	w.WriteHeader(http.StatusNoContent)
}

func (a *telnyxApp) serveMedia(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/reject/") {
		http.Error(w, "call is over", http.StatusGone)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	log := newMediaLog(r)
	a.mu.Lock()
	a.logs[r.URL.Path] = log
	a.mu.Unlock()
	var onStart func()
	switch {
	case strings.Contains(r.URL.Path, "/talk/"):
		onStart = func() { talkBack(conn) }
	case strings.Contains(r.URL.Path, "/close/"):
		onStart = func() {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second))
		}
	}
	log.read(conn, onStart)
}

// talkBack is what Telephony sends into a stream: 25 frames of agent audio,
// a mark, 5 more frames, then a barge-in clear and a second mark.
func talkBack(conn *websocket.Conn) {
	frames := toneFrames(30, 16000)
	for i, frame := range frames {
		if i == 25 {
			_ = conn.WriteJSON(map[string]any{"event": "mark", "mark": map[string]any{"name": "m1"}})
		}
		_ = conn.WriteJSON(map[string]any{"event": "media", "media": map[string]any{"payload": base64.StdEncoding.EncodeToString(frame)}})
		time.Sleep(time.Millisecond)
	}
	_ = conn.WriteJSON(map[string]any{"event": "clear"})
	_ = conn.WriteJSON(map[string]any{"event": "mark", "mark": map[string]any{"name": "m2"}})
}

// toneFrames is n 20 ms frames of a 440 Hz tone as little-endian L16.
func toneFrames(n, rate int) [][]byte {
	per := rate / 50
	out := make([][]byte, n)
	for i := range out {
		samples := make([]int16, per)
		for j := range samples {
			at := float64(i*per+j) / float64(rate)
			samples[j] = int16(8000 * math.Sin(2*math.Pi*440*at))
		}
		out[i] = mockEncodeAudio(samples, "L16")
	}
	return out
}

func (a *telnyxApp) mediaLog(t *testing.T, path string) *mediaLog {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a.mu.Lock()
		log := a.logs[path]
		a.mu.Unlock()
		if log != nil {
			return log
		}
		if time.Now().After(deadline) {
			t.Fatalf("no media connection on %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (a *telnyxApp) eventsFor(callID string) []telnyxAppEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []telnyxAppEvent
	for _, event := range a.events {
		if event.payload["call_control_id"] == callID {
			out = append(out, event)
		}
	}
	return out
}

func (a *telnyxApp) eventNames(callID string) []string {
	var out []string
	for _, event := range a.eventsFor(callID) {
		out = append(out, event.event)
	}
	return out
}

// waitEvents waits until the call has received at least the wanted events
// and checks they arrived first, in that order.
func (a *telnyxApp) waitEvents(t *testing.T, callID string, want ...string) []telnyxAppEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := a.eventsFor(callID)
		if len(got) >= len(want) {
			for i, name := range want {
				if got[i].event != name {
					t.Fatalf("events for %s = %v, want %v first", callID, a.eventNames(callID), want)
				}
			}
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: events for %s = %v, want %v", callID, a.eventNames(callID), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (a *telnyxApp) event(t *testing.T, callID, name string) map[string]any {
	t.Helper()
	for _, event := range a.eventsFor(callID) {
		if event.event == name {
			return event.payload
		}
	}
	t.Fatalf("no %s for %s; events = %v", name, callID, a.eventNames(callID))
	return nil
}

func (a *telnyxApp) trigger(t *testing.T, name string, input map[string]any) (int, map[string]any, string) {
	t.Helper()
	rec := runtimeAPIRequest(t, a.h.s, a.h.owner, http.MethodPost, "/apps/callback/runtimes/rt-sim/integrations/telnyx/triggers/"+name, sdk.RuntimeIntegrationTriggerRequest{
		RuntimeIntegrationSelector: sdk.RuntimeIntegrationSelector{App: "telephony", Role: "carrier"},
		Input:                      input,
		Wait:                       true,
	})
	if rec.Code != http.StatusOK {
		return rec.Code, nil, rec.Body.String()
	}
	var result sdk.RuntimeIntegrationTriggerResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(result.Result, &out)
	return rec.Code, out, rec.Body.String()
}

func (a *telnyxApp) mustTrigger(t *testing.T, name string, input map[string]any) map[string]any {
	t.Helper()
	code, out, body := a.trigger(t, name, input)
	if code != http.StatusOK {
		t.Fatalf("%s = %d %s", name, code, body)
	}
	return out
}

func (a *telnyxApp) mustTool(t *testing.T, name string, input map[string]any) map[string]any {
	t.Helper()
	res := a.h.tool(name, input)
	if !res.Success {
		t.Fatalf("%s = %d %v", name, res.Status, res.Data)
	}
	out, _ := res.Data.(map[string]any)
	return out
}

// inboundCall rings the provisioned number and, with answer input, answers.
func (a *telnyxApp) inboundCall(t *testing.T, answer map[string]any) string {
	t.Helper()
	result := a.mustTrigger(t, "incoming_call", map[string]any{"to": "+15550100001", "from": "+15550100999"})
	callID, _ := result["call_control_id"].(string)
	if callID == "" {
		t.Fatalf("incoming_call = %v", result)
	}
	if answer != nil {
		input := map[string]any{"call_control_id": callID}
		for key, value := range answer {
			input[key] = value
		}
		a.mustTool(t, "answer_call", input)
		a.waitEvents(t, callID, "call.initiated", "call.answered")
	}
	return callID
}

func (a *telnyxApp) dial(t *testing.T, id string, extra map[string]any) string {
	t.Helper()
	input := map[string]any{
		"connection_id": a.h.appID, "to": "+15550100200", "from": "+15550100001", "command_id": "dial-" + id, "timeout_secs": float64(30),
		"webhook_url": fmt.Sprintf("https://runtime.apteva.invalid/rt-sim/api/apps/telephony/_install/%d/status/telnyx/%s?token=t-%s", a.h.install, id, id),
	}
	for key, value := range extra {
		input[key] = value
	}
	data := a.mustTool(t, "dial_call", input)["data"].(map[string]any)
	callID, _ := data["call_control_id"].(string)
	if callID == "" || data["call_leg_id"] == "" || data["call_session_id"] == "" || data["is_alive"] != true {
		t.Fatalf("dial_call = %v", data)
	}
	a.waitEvents(t, callID, "call.initiated")
	return callID
}

// --- state helpers ---

func simRecords(state sdk.RuntimeIntegrationState, collection, field string, value any) []map[string]any {
	var out []map[string]any
	for _, record := range state.Collections[collection] {
		if mockEqual(record[field], value) {
			out = append(out, record)
		}
	}
	return out
}

func simRecord(state sdk.RuntimeIntegrationState, collection, field string, value any) map[string]any {
	records := simRecords(state, collection, field, value)
	if len(records) != 1 {
		return nil
	}
	return records[0]
}

func num(value any) float64 {
	n, _ := mockNumber(value)
	return n
}

func sub(record map[string]any, key string) map[string]any {
	out, _ := record[key].(map[string]any)
	return out
}

func (a *telnyxApp) waitStream(t *testing.T, what, streamID string, ok func(stream map[string]any) bool) map[string]any {
	t.Helper()
	state := a.h.waitFor(what, func(state sdk.RuntimeIntegrationState) bool {
		stream := simRecord(state, "streams", "stream_id", streamID)
		return stream != nil && ok(stream)
	})
	return simRecord(state, "streams", "stream_id", streamID)
}

func (a *telnyxApp) callStream(t *testing.T, callID string) string {
	t.Helper()
	call := simRecord(a.h.state(), "calls", "call_control_id", callID)
	id, _ := call["stream_id"].(string)
	if id == "" {
		t.Fatalf("call %s has no stream: %v", callID, call)
	}
	return id
}

// --- the media peer's view, from the app side ---

type mediaLog struct {
	mu       sync.Mutex
	path     string
	auth     string
	events   []string
	seqs     []int
	start    map[string]any
	stop     map[string]any
	codec    string
	media    int
	loud     int
	sizes    map[int]int
	tracks   map[string]int
	chunks   []int
	marks    []string
	finished chan struct{}
}

func newMediaLog(r *http.Request) *mediaLog {
	return &mediaLog{path: r.URL.Path, auth: r.Header.Get("Authorization"), sizes: map[int]int{}, tracks: map[string]int{}, finished: make(chan struct{})}
}

func (l *mediaLog) read(conn *websocket.Conn, onStart func()) {
	defer close(l.finished)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var frame map[string]any
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		event, _ := frame["event"].(string)
		l.mu.Lock()
		l.events = append(l.events, event)
		if seq, ok := frame["sequence_number"].(string); ok {
			n, _ := strconv.Atoi(seq)
			l.seqs = append(l.seqs, n)
		}
		switch event {
		case "start":
			l.start = frame
			l.codec, _ = sub(sub(frame, "start"), "media_format")["encoding"].(string)
		case "media":
			media := sub(frame, "media")
			raw, _ := base64.StdEncoding.DecodeString(mockString(media["payload"]))
			l.sizes[len(raw)]++
			l.tracks[mockString(media["track"])]++
			chunk, _ := strconv.Atoi(mockString(media["chunk"]))
			l.chunks = append(l.chunks, chunk)
			samples, _ := mockDecodeAudio(raw, l.codec)
			sum := 0.0
			for _, s := range samples {
				sum += float64(s) * float64(s)
			}
			if len(samples) > 0 && math.Sqrt(sum/float64(len(samples))) >= mockMediaSilenceRMS {
				l.loud++
			}
			l.media++
		case "mark":
			l.marks = append(l.marks, mockString(sub(frame, "mark")["name"]))
		case "stop":
			l.stop = frame
		}
		l.mu.Unlock()
		if event == "start" && onStart != nil {
			go onStart()
		}
	}
}

func (l *mediaLog) wait(t *testing.T) {
	t.Helper()
	select {
	case <-l.finished:
	case <-time.After(10 * time.Second):
		t.Fatalf("media connection %s did not close", l.path)
	}
}

func (l *mediaLog) snapshot() mediaLog {
	l.mu.Lock()
	defer l.mu.Unlock()
	return mediaLog{path: l.path, auth: l.auth, events: append([]string(nil), l.events...), seqs: append([]int(nil), l.seqs...),
		start: l.start, stop: l.stop, codec: l.codec, media: l.media, loud: l.loud, sizes: l.sizes, tracks: l.tracks,
		chunks: append([]int(nil), l.chunks...), marks: append([]string(nil), l.marks...)}
}

// --- tests ---

func TestTelnyxSimulationMediaStreamTwoWayAudio(t *testing.T) {
	a := newTelnyxApp(t)
	// Answer the way Telephony does: bidirectional L16 at 16 kHz, recording
	// from answer.
	callID := a.inboundCall(t, map[string]any{
		"command_id": "answer-1", "stream_url": telnyxAppMediaURL + "talk/sekrit", "stream_track": "inbound_track",
		"stream_codec": "L16", "stream_bidirectional_mode": "rtp", "stream_bidirectional_codec": "L16",
		"stream_bidirectional_sampling_rate": float64(16000), "stream_bidirectional_target_legs": "self", "send_silence_when_idle": true,
		"record": "record-from-answer", "record_channels": "single", "record_format": "wav", "record_track": "both",
	})
	streamID := a.callStream(t, callID)
	stream := a.waitStream(t, "two-way audio", streamID, func(stream map[string]any) bool {
		received, sent := sub(stream, "received"), sub(stream, "sent")
		return stream["state"] == "streaming" && num(received["frames"]) == 30 && num(received["marks_acked"]) == 2 && num(sent["frames"]) >= 110
	})
	for field, want := range map[string]any{"codec": "L16", "sample_rate": float64(16000), "bidirectional_codec": "L16", "bidirectional_sample_rate": float64(16000), "track": "inbound_track", "call_control_id": callID, "caller_audio": "caller-speech-8k.wav"} {
		if !mockEqual(stream[field], want) {
			t.Fatalf("stream %s = %v, want %v (%v)", field, stream[field], want, stream)
		}
	}
	received := sub(stream, "received")
	for field, want := range map[string]float64{"frames": 30, "non_silent_frames": 30, "ms": 600, "non_silent_ms": 600, "bytes": 30 * 640, "marks": 2, "marks_acked": 2, "clears": 1, "decode_errors": 0, "unknown_frames": 0} {
		if num(received[field]) != want {
			t.Fatalf("received.%s = %v, want %v (%v)", field, received[field], want, received)
		}
	}
	if peak := num(received["peak"]); peak < 7900 || peak > 8000 {
		t.Fatalf("received.peak = %v", peak)
	}
	if rms := num(received["rms"]); rms < 5600 || rms > 5700 {
		t.Fatalf("received.rms = %v", rms)
	}

	// The caller speaks again: silence, then the fixture; a recording is
	// not caller audio.
	a.mustTrigger(t, "caller_speaks", map[string]any{"call_control_id": callID, "audio": "silence"})
	a.mustTrigger(t, "caller_speaks", map[string]any{"call_control_id": callID})
	if code, _, body := a.trigger(t, "caller_speaks", map[string]any{"call_control_id": callID, "audio": "call-recording-mono.wav"}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "not a caller audio fixture") {
		t.Fatalf("recording as caller audio = %d %s", code, body)
	}
	a.waitStream(t, "the second play", streamID, func(stream map[string]any) bool {
		sent := sub(stream, "sent")
		return num(sent["plays"]) == 3 && num(sent["frames"]) >= 220 && num(sent["pending_ms"]) == 0
	})

	a.mustTool(t, "hangup_call", map[string]any{"call_control_id": callID, "command_id": "hangup-1"})
	a.waitEvents(t, callID, "call.initiated", "call.answered", "call.streaming.started", "call.hangup", "call.recording.saved", "call.streaming.stopped")
	log := a.mediaLog(t, "/media/telnyx/talk/sekrit")
	log.wait(t)
	state := a.h.state()
	stream = simRecord(state, "streams", "stream_id", streamID)
	if stream["state"] != "stopped" || stream["closed_by"] != "provider" || num(stream["close_code"]) != 1000 || stream["connected_at"] == nil || stream["ended_at"] == nil {
		t.Fatalf("stopped stream = %v", stream)
	}
	sent := sub(stream, "sent")
	if num(sent["plays"]) != 3 || num(sent["frames"]) != 220 || sent["fixture"] != "caller-speech-8k.wav" || num(sent["ms"]) != 4400 {
		t.Fatalf("sent = %v", sent)
	}

	// What the app saw on the socket.
	got := log.snapshot()
	if len(got.events) < 3 || got.events[0] != "connected" || got.events[1] != "start" || got.events[len(got.events)-1] != "stop" {
		t.Fatalf("socket events = %v ... %v", got.events[:min(len(got.events), 3)], got.events[len(got.events)-1])
	}
	start := sub(got.start, "start")
	format := sub(start, "media_format")
	if format["encoding"] != "L16" || num(format["sample_rate"]) != 16000 || num(format["channels"]) != 1 || start["call_control_id"] != callID || got.start["stream_id"] != streamID {
		t.Fatalf("start frame = %v", got.start)
	}
	if got.stop == nil || sub(got.stop, "stop")["call_control_id"] != callID || got.stop["stream_id"] != streamID {
		t.Fatalf("stop frame = %v", got.stop)
	}
	for i, seq := range got.seqs {
		if seq != i+1 {
			t.Fatalf("sequence numbers = %v", got.seqs)
		}
	}
	for i, chunk := range got.chunks {
		if chunk != i+1 {
			t.Fatalf("chunks = %v", got.chunks)
		}
	}
	if len(got.sizes) != 1 || got.sizes[640] != got.media || got.tracks["inbound"] != got.media {
		t.Fatalf("media payload sizes %v tracks %v", got.sizes, got.tracks)
	}
	if got.media != int(num(sent["frames"])+num(sent["silence_frames"])) || num(sent["bytes"]) != float64(640*got.media) {
		t.Fatalf("app read %d media frames; simulation sent %v", got.media, sent)
	}
	if got.loud < 140 || got.media-got.loud < 1 {
		t.Fatalf("caller audio: %d loud of %d frames", got.loud, got.media)
	}
	if strings.Join(got.marks, ",") != "m1,m2" {
		t.Fatalf("mark echoes = %v", got.marks)
	}
	if !strings.HasPrefix(got.auth, "Bearer ") {
		t.Fatalf("media upgrade had no app credential: %q", got.auth)
	}

	// Webhooks Telephony maps, and the recording it downloads.
	started := a.event(t, callID, "call.streaming.started")
	if started["stream_id"] != streamID || started["stream_type"] != "websocket" || started["stream_url"] != nil {
		t.Fatalf("streaming.started = %v", started)
	}
	recording := simRecord(state, "recordings", "call_control_id", callID)
	saved := a.event(t, callID, "call.recording.saved")
	wantURL := strings.TrimRight(a.h.s.publicBaseURL(), "/") + "/api/integration-simulation/fixtures/call-recording-mono.wav"
	if recording == nil || saved["recording_id"] != recording["id"] || saved["format"] != "wav" || saved["channels"] != "single" ||
		sub(saved, "recording_urls")["wav"] != wantURL || saved["recording_started_at"] == nil || saved["recording_ended_at"] == nil {
		t.Fatalf("recording.saved = %v (recording %v)", saved, recording)
	}
	got2 := a.mustTool(t, "get_recording", map[string]any{"recording_id": recording["id"]})
	if sub(sub(got2, "data"), "download_urls")["wav"] != wantURL {
		t.Fatalf("get_recording = %v", got2)
	}

	// The per-call secret in the stream URL stays out of state and evidence
	// (only the answer_call request, which carried it, holds it).
	raw, _ := json.Marshal(state)
	if strings.Contains(string(raw), "sekrit") {
		t.Fatal("stream URL secret in simulation state")
	}
	sawConnect := false
	for _, entry := range a.h.runtime.Edge().Calls() {
		if entry.Kind == "integration_media" && entry.Event == "stream.connected" && entry.Status == http.StatusSwitchingProtocols && strings.HasPrefix(entry.Path, "/api/apps/telephony/media/telnyx/") {
			sawConnect = true
		}
		if strings.Contains(entry.Path+entry.RespBody, "sekrit") || (strings.Contains(entry.ReqBody, "sekrit") && entry.Tool != "answer_call") {
			t.Fatalf("stream URL secret in evidence: %+v", entry)
		}
	}
	if !sawConnect {
		t.Fatal("no media evidence")
	}
}

func TestTelnyxSimulationMediaStreamFailuresAndStop(t *testing.T) {
	a := newTelnyxApp(t)
	callID := a.inboundCall(t, map[string]any{})
	startStream := func(url string, extra map[string]any) string {
		t.Helper()
		input := map[string]any{"call_control_id": callID, "stream_url": url}
		for key, value := range extra {
			input[key] = value
		}
		a.mustTool(t, "start_streaming", input)
		return a.callStream(t, callID)
	}
	failed := func(streamID, reason string) {
		t.Helper()
		stream := a.waitStream(t, "a failed stream", streamID, func(stream map[string]any) bool { return stream["state"] == "failed" })
		if !strings.Contains(mockString(stream["error"]), reason) {
			t.Fatalf("stream error = %v, want %q", stream["error"], reason)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			for _, event := range a.eventsFor(callID) {
				if event.event == "call.streaming.failed" && event.payload["stream_id"] == streamID {
					if !strings.Contains(mockString(event.payload["failure_reason"]), reason) {
						t.Fatalf("streaming.failed = %v", event.payload)
					}
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no call.streaming.failed for %s; events %v", streamID, a.eventNames(callID))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	failed(startStream("wss://media.example.com/stream/sekrit", nil), "outside this runtime")
	failed(startStream(telnyxAppMediaURL+"reject/1", nil), "410")
	failed(startStream(telnyxAppMediaURL+"listen/g722", map[string]any{"stream_codec": "G722"}), "G722 is not simulated")
	blocked := false
	for _, entry := range a.h.runtime.Edge().Calls() {
		if entry.Kind == "integration_media" && entry.Blocked && entry.Host == "media.example.com" && entry.Path == "" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("outside stream URL not recorded as blocked")
	}

	// The app closes the socket itself.
	closedID := startStream(telnyxAppMediaURL+"close/1", nil)
	closed := a.waitStream(t, "an app-closed stream", closedID, func(stream map[string]any) bool { return stream["state"] == "closed" })
	if closed["closed_by"] != "app" || num(closed["close_code"]) != 1000 || closed["error"] != nil {
		t.Fatalf("app-closed stream = %v", closed)
	}

	// A default stream is PCMU at 8 kHz; stop_streaming ends it.
	listenID := startStream(telnyxAppMediaURL+"listen/1", nil)
	a.waitStream(t, "PCMU audio", listenID, func(stream map[string]any) bool {
		return stream["state"] == "streaming" && num(sub(stream, "sent")["frames"]) >= 50
	})
	if res := a.h.tool("stop_streaming", map[string]any{"call_control_id": callID, "stream_id": closedID}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("stop of an old stream = %+v", res)
	}
	a.mustTool(t, "stop_streaming", map[string]any{"call_control_id": callID, "stream_id": listenID})
	stopped := a.waitStream(t, "a stopped stream", listenID, func(stream map[string]any) bool { return stream["state"] == "stopped" })
	if stopped["closed_by"] != "provider" || stopped["codec"] != "PCMU" || num(stopped["sample_rate"]) != 8000 {
		t.Fatalf("stopped stream = %v", stopped)
	}
	log := a.mediaLog(t, "/media/telnyx/listen/1")
	log.wait(t)
	got := log.snapshot()
	format := sub(sub(got.start, "start"), "media_format")
	if format["encoding"] != "PCMU" || num(format["sample_rate"]) != 8000 || len(got.sizes) != 1 || got.sizes[160] != got.media || got.stop == nil {
		t.Fatalf("PCMU socket: format %v sizes %v stop %v", format, got.sizes, got.stop)
	}
	if code, _, body := a.trigger(t, "caller_speaks", map[string]any{"call_control_id": callID}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "media stream is stopped") {
		t.Fatalf("caller_speaks on a stopped stream = %d %s", code, body)
	}
	a.waitEvents(t, callID, "call.initiated", "call.answered")
	var lifecycle []string
	for _, event := range a.eventsFor(callID)[2:] {
		lifecycle = append(lifecycle, event.event+":"+mockString(event.payload["stream_id"]))
	}
	want := []string{
		"call.streaming.failed:" + a.streamAt(t, 0), "call.streaming.failed:" + a.streamAt(t, 1), "call.streaming.failed:" + a.streamAt(t, 2),
		"call.streaming.started:" + closedID, "call.streaming.stopped:" + closedID,
		"call.streaming.started:" + listenID, "call.streaming.stopped:" + listenID,
	}
	deadline := time.Now().Add(5 * time.Second)
	for strings.Join(lifecycle, ",") != strings.Join(want, ",") {
		if time.Now().After(deadline) {
			t.Fatalf("stream events = %v, want %v", lifecycle, want)
		}
		time.Sleep(5 * time.Millisecond)
		lifecycle = nil
		for _, event := range a.eventsFor(callID)[2:] {
			lifecycle = append(lifecycle, event.event+":"+mockString(event.payload["stream_id"]))
		}
	}
}

func (a *telnyxApp) streamAt(t *testing.T, i int) string {
	t.Helper()
	streams := a.h.state().Collections["streams"]
	if i >= len(streams) {
		t.Fatalf("no stream %d", i)
	}
	return mockString(streams[i]["stream_id"])
}

func TestTelnyxSimulationOutboundStreamWaitsForAnswer(t *testing.T) {
	a := newTelnyxApp(t)
	callID := a.dial(t, "media", map[string]any{
		"stream_url": telnyxAppMediaURL + "listen/out", "stream_track": "inbound_track", "stream_codec": "L16",
		"stream_bidirectional_mode": "rtp", "stream_bidirectional_codec": "L16", "stream_bidirectional_sampling_rate": float64(16000),
		"stream_establish_before_call_originate": true,
	})
	streamID := a.callStream(t, callID)
	a.waitStream(t, "a connected stream", streamID, func(stream map[string]any) bool { return stream["state"] == "streaming" })
	time.Sleep(60 * time.Millisecond)
	stream := simRecord(a.h.state(), "streams", "stream_id", streamID)
	if sent := sub(stream, "sent"); num(sent["frames"]) != 0 || num(sent["silence_frames"]) != 0 || num(sent["plays"]) != 0 {
		t.Fatalf("media before answer: %v", sent)
	}
	a.mustTrigger(t, "callee_answer", map[string]any{"call_control_id": callID, "audio": "silence"})
	a.waitStream(t, "silence after answer", streamID, func(stream map[string]any) bool {
		sent := sub(stream, "sent")
		return num(sent["silence_frames"]) > 0 && num(sent["frames"]) == 0 && num(sent["plays"]) == 1
	})
	a.mustTrigger(t, "caller_speaks", map[string]any{"call_control_id": callID})
	a.waitStream(t, "callee speech", streamID, func(stream map[string]any) bool { return num(sub(stream, "sent")["frames"]) >= 110 })
	a.mustTrigger(t, "callee_hangup", map[string]any{"call_control_id": callID})
	a.waitEvents(t, callID, "call.initiated", "call.streaming.started", "call.answered", "call.hangup", "call.streaming.stopped")
	a.mediaLog(t, "/media/telnyx/listen/out").wait(t)
	got := a.mediaLog(t, "/media/telnyx/listen/out").snapshot()
	if got.events[0] != "connected" || got.events[1] != "start" || got.loud < 70 || got.stop == nil {
		t.Fatalf("outbound socket: %d events, %d loud, stop %v", len(got.events), got.loud, got.stop)
	}
	for _, event := range a.eventsFor(callID) {
		if event.path != "/status/telnyx/media" || event.query != "token=t-media" {
			t.Fatalf("outbound event went to %s?%s", event.path, event.query)
		}
	}
}

func TestTelnyxSimulationStreamsEndWithRestoreAndTeardown(t *testing.T) {
	a := newTelnyxApp(t)
	callID := a.inboundCall(t, map[string]any{"stream_url": telnyxAppMediaURL + "listen/before", "stream_codec": "L16"})
	first := a.callStream(t, callID)
	a.waitStream(t, "streaming", first, func(stream map[string]any) bool { return stream["state"] == "streaming" })
	sim := a.h.runtime.findSimulation("telnyx", "telephony", "carrier")

	// A runtime restored from a snapshot holds stream records, not sockets.
	if err := sim.restore(sim.snapshotState()); err != nil {
		t.Fatal(err)
	}
	a.mediaLog(t, "/media/telnyx/listen/before").wait(t)
	stream := simRecord(a.h.state(), "streams", "stream_id", first)
	if stream["state"] != "interrupted" || !strings.Contains(mockString(stream["error"]), "snapshot") {
		t.Fatalf("restored stream = %v", stream)
	}
	if got := a.mediaLog(t, "/media/telnyx/listen/before").snapshot(); got.stop != nil {
		t.Fatal("an interrupted stream sent a stop frame")
	}

	second := ""
	a.mustTool(t, "start_streaming", map[string]any{"call_control_id": callID, "stream_url": telnyxAppMediaURL + "listen/after"})
	second = a.callStream(t, callID)
	a.waitStream(t, "a new stream", second, func(stream map[string]any) bool { return stream["state"] == "streaming" })
	a.h.runtime.stopSimulations()
	a.mediaLog(t, "/media/telnyx/listen/after").wait(t)
	sim.mu.Lock()
	peers := len(sim.peers)
	sim.mu.Unlock()
	if peers != 0 {
		t.Fatalf("%d media peers after teardown", peers)
	}
	time.Sleep(20 * time.Millisecond)
	for _, name := range a.eventNames(callID) {
		if name == "call.streaming.stopped" {
			t.Fatalf("teardown or restore delivered a stream event: %v", a.eventNames(callID))
		}
	}
}

func TestTelnyxSimulationOutboundCallOutcomes(t *testing.T) {
	a := newTelnyxApp(t)
	hangup := func(callID string) map[string]any {
		t.Helper()
		events := a.waitEvents(t, callID, "call.initiated")
		for _, event := range events {
			if event.event == "call.hangup" {
				return event.payload
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, event := range a.eventsFor(callID) {
				if event.event == "call.hangup" {
					return event.payload
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("no hangup for %s: %v", callID, a.eventNames(callID))
		return nil
	}
	expectHangup := func(callID, cause, source, sip string) {
		t.Helper()
		payload := hangup(callID)
		if payload["hangup_cause"] != cause || payload["hangup_source"] != source || payload["sip_hangup_cause"] != sip {
			t.Fatalf("hangup of %s = %v, want %s/%s/%s", callID, payload, cause, source, sip)
		}
	}

	answered := a.dial(t, "1", nil)
	initiated := a.event(t, answered, "call.initiated")
	if initiated["direction"] != "outgoing" || initiated["state"] != "bridging" || initiated["to"] != "+15550100200" || initiated["connection_id"] != a.h.appID {
		t.Fatalf("call.initiated = %v", initiated)
	}
	again := a.mustTool(t, "dial_call", map[string]any{"connection_id": a.h.appID, "to": "+15550100200", "from": "+15550100001", "command_id": "dial-1"})
	if sub(again, "data")["call_control_id"] != answered || len(a.h.state().Collections["calls"]) != 1 {
		t.Fatalf("repeated dial command = %v", again)
	}
	if status := a.mustTool(t, "get_call_status", map[string]any{"call_control_id": answered}); sub(status, "data")["is_alive"] != true {
		t.Fatalf("ringing call status = %v", status)
	}
	if code, _, _ := a.trigger(t, "callee_hangup", map[string]any{"call_control_id": answered}); code != http.StatusUnprocessableEntity {
		t.Fatalf("callee_hangup before answer = %d", code)
	}
	a.mustTrigger(t, "callee_answer", map[string]any{"call_control_id": answered})
	a.waitEvents(t, answered, "call.initiated", "call.answered")
	a.mustTool(t, "hangup_call", map[string]any{"call_control_id": answered})
	expectHangup(answered, "normal_clearing", "caller", "unspecified")
	if status := a.mustTool(t, "get_call_status", map[string]any{"call_control_id": answered}); sub(status, "data")["is_alive"] != false {
		t.Fatalf("ended call status = %v", status)
	}
	if code, _, body := a.trigger(t, "callee_answer", map[string]any{"call_control_id": answered}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "90018") {
		t.Fatalf("answer after hangup = %d %s", code, body)
	}

	noAnswer := a.dial(t, "2", nil)
	a.mustTrigger(t, "callee_no_answer", map[string]any{"call_control_id": noAnswer})
	expectHangup(noAnswer, "timeout", "callee", "480")

	busy := a.dial(t, "3", nil)
	a.mustTrigger(t, "callee_fail", map[string]any{"call_control_id": busy})
	expectHangup(busy, "user_busy", "callee", "486")

	rejected := a.dial(t, "4", nil)
	a.mustTrigger(t, "callee_fail", map[string]any{"call_control_id": rejected, "cause": "call_rejected"})
	expectHangup(rejected, "call_rejected", "callee", "unspecified")

	cancelled := a.dial(t, "5", nil)
	a.mustTool(t, "hangup_call", map[string]any{"call_control_id": cancelled})
	expectHangup(cancelled, "originator_cancel", "caller", "unspecified")

	remote := a.dial(t, "6", nil)
	a.mustTrigger(t, "callee_answer", map[string]any{"call_control_id": remote})
	a.mustTrigger(t, "callee_hangup", map[string]any{"call_control_id": remote})
	expectHangup(remote, "normal_clearing", "callee", "unspecified")
	if got := a.eventNames(remote); strings.Join(got, ",") != "call.initiated,call.answered,call.hangup" {
		t.Fatalf("remote hangup events = %v", got)
	}

	machine := a.dial(t, "7", map[string]any{"answering_machine_detection": "detect"})
	a.mustTrigger(t, "callee_answer", map[string]any{"call_control_id": machine, "answered_by": "machine"})
	a.waitEvents(t, machine, "call.initiated", "call.answered", "call.machine.detection.ended")
	if result := a.event(t, machine, "call.machine.detection.ended")["result"]; result != "machine" {
		t.Fatalf("detection result = %v", result)
	}
	premium := a.dial(t, "8", map[string]any{"answering_machine_detection": "premium"})
	a.mustTrigger(t, "callee_answer", map[string]any{"call_control_id": premium})
	a.waitEvents(t, premium, "call.initiated", "call.answered", "call.machine.premium.detection.ended")
	if result := a.event(t, premium, "call.machine.premium.detection.ended")["result"]; result != "human_residence" {
		t.Fatalf("premium detection result = %v", result)
	}

	for name, tc := range map[string]struct {
		input  map[string]any
		detail string
	}{
		"no to":         {map[string]any{"connection_id": a.h.appID, "from": "+15550100001"}, "to is required"},
		"unknown app":   {map[string]any{"connection_id": "999", "to": "+1", "from": "+2"}, "not a Call Control application"},
		"no connection": {map[string]any{"to": "+1", "from": "+2"}, "connection_id is required"},
	} {
		res := a.h.tool("dial_call", tc.input)
		if res.Status != http.StatusUnprocessableEntity || !strings.Contains(mockString(res.Data), tc.detail) {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	inbound := a.inboundCall(t, nil)
	if code, _, body := a.trigger(t, "callee_answer", map[string]any{"call_control_id": inbound}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "not an outbound call") {
		t.Fatalf("callee_answer on an inbound call = %d %s", code, body)
	}
}

func TestTelnyxSimulationRecordingCommands(t *testing.T) {
	a := newTelnyxApp(t)
	callID := a.dial(t, "rec", map[string]any{"record": "record-from-answer", "record_channels": "dual", "record_format": "mp3", "record_track": "both"})
	if res := a.h.tool("start_call_recording", map[string]any{"call_control_id": callID}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("record before answer = %+v", res)
	}
	answered := a.mustTrigger(t, "callee_answer", map[string]any{"call_control_id": callID})
	recordingID, _ := answered["recording_id"].(string)
	recording := simRecord(a.h.state(), "recordings", "id", recordingID)
	if recording == nil || recording["status"] != "recording" || recording["channels"] != "dual" || recording["requested_format"] != "mp3" || recording["format"] != "wav" {
		t.Fatalf("recording from answer = %v (answer %v)", recording, answered)
	}
	ok := map[string]any{"data": map[string]any{"result": "ok"}}
	if got := a.mustTool(t, "pause_call_recording", map[string]any{"call_control_id": callID, "command_id": "p1"}); !mockEqual(got, ok) {
		t.Fatalf("pause = %v", got)
	}
	if status := simRecord(a.h.state(), "recordings", "id", recordingID)["status"]; status != "paused" {
		t.Fatalf("paused recording status = %v", status)
	}
	if res := a.h.tool("resume_call_recording", map[string]any{"call_control_id": callID, "recording_id": "other"}); res.Status != http.StatusNotFound {
		t.Fatalf("resume of another recording = %+v", res)
	}
	a.mustTool(t, "resume_call_recording", map[string]any{"call_control_id": callID, "recording_id": recordingID})
	if status := simRecord(a.h.state(), "recordings", "id", recordingID)["status"]; status != "recording" {
		t.Fatalf("resumed recording status = %v", status)
	}
	if res := a.h.tool("start_call_recording", map[string]any{"call_control_id": callID}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("second recording = %+v", res)
	}
	if list := a.mustTool(t, "list_recordings", map[string]any{"filter[call_control_id]": callID}); len(list["data"].([]any)) != 0 {
		t.Fatalf("a recording in progress is listed: %v", list)
	}
	if res := a.h.tool("get_recording", map[string]any{"recording_id": recordingID}); res.Status != http.StatusNotFound {
		t.Fatalf("get of a recording in progress = %+v", res)
	}

	a.mustTool(t, "stop_call_recording", map[string]any{"call_control_id": callID, "command_id": "s1"})
	a.waitEvents(t, callID, "call.initiated", "call.answered", "call.recording.saved")
	base := strings.TrimRight(a.h.s.publicBaseURL(), "/") + "/api/integration-simulation/fixtures/"
	saved := a.event(t, callID, "call.recording.saved")
	if saved["recording_id"] != recordingID || saved["channels"] != "dual" || sub(saved, "recording_urls")["wav"] != base+"call-recording-stereo.wav" {
		t.Fatalf("recording.saved = %v", saved)
	}
	if res := a.h.tool("stop_call_recording", map[string]any{"call_control_id": callID}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("stop without a recording = %+v", res)
	}
	got := a.mustTool(t, "get_recording", map[string]any{"recording_id": recordingID})
	if data := sub(got, "data"); data["status"] != "completed" || sub(data, "download_urls")["wav"] != base+"call-recording-stereo.wav" || data["recording_ended_at"] == nil {
		t.Fatalf("get_recording = %v", got)
	}

	// A second, single-channel recording is saved when the callee hangs up.
	a.mustTool(t, "start_call_recording", map[string]any{"call_control_id": callID, "channels": "single", "format": "wav"})
	a.mustTrigger(t, "callee_hangup", map[string]any{"call_control_id": callID})
	a.waitEvents(t, callID, "call.initiated", "call.answered", "call.recording.saved", "call.hangup", "call.recording.saved")
	last := a.eventsFor(callID)[4].payload
	if last["recording_id"] == recordingID || last["channels"] != "single" || sub(last, "recording_urls")["wav"] != base+"call-recording-mono.wav" {
		t.Fatalf("second recording.saved = %v", last)
	}
	list := a.mustTool(t, "list_recordings", map[string]any{"filter[call_control_id]": callID})
	if len(list["data"].([]any)) != 2 || num(sub(list, "meta")["total_results"]) != 2 {
		t.Fatalf("list_recordings = %v", list)
	}
	a.mustTool(t, "delete_recording", map[string]any{"recording_id": recordingID})
	if res := a.h.tool("get_recording", map[string]any{"recording_id": recordingID}); res.Status != http.StatusNotFound {
		t.Fatalf("get after delete = %+v", res)
	}
	if list := a.mustTool(t, "list_recordings", map[string]any{"filter[call_control_id]": callID}); len(list["data"].([]any)) != 1 {
		t.Fatalf("list after delete = %v", list)
	}

	// Inbound: record-from-answer on answer_call, saved when the caller
	// hangs up.
	inbound := a.inboundCall(t, map[string]any{"record": "record-from-answer"})
	a.mustTrigger(t, "caller_hangup", map[string]any{"call_control_id": inbound})
	a.waitEvents(t, inbound, "call.initiated", "call.answered", "call.hangup", "call.recording.saved")
	if saved := a.event(t, inbound, "call.recording.saved"); saved["channels"] != "single" || sub(saved, "recording_urls")["wav"] != base+"call-recording-mono.wav" {
		t.Fatalf("inbound recording.saved = %v", saved)
	}
}

func TestTelnyxSimulationBridgesAnsweredLegs(t *testing.T) {
	a := newTelnyxApp(t)
	inbound := a.inboundCall(t, map[string]any{})
	outbound := a.dial(t, "bridge", nil)
	if res := a.h.tool("bridge_call", map[string]any{"source_call_control_id": inbound, "call_control_id": outbound}); res.Status != http.StatusUnprocessableEntity || !strings.Contains(mockString(res.Data), "not answered") {
		t.Fatalf("bridge to a ringing leg = %+v", res)
	}
	a.mustTrigger(t, "callee_answer", map[string]any{"call_control_id": outbound})
	if res := a.h.tool("bridge_call", map[string]any{"source_call_control_id": inbound, "call_control_id": inbound}); res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("bridge to itself = %+v", res)
	}
	if res := a.h.tool("bridge_call", map[string]any{"source_call_control_id": inbound, "call_control_id": "v3:nope"}); res.Status != http.StatusUnprocessableEntity || !strings.Contains(mockString(res.Data), "90015") {
		t.Fatalf("bridge to an unknown leg = %+v", res)
	}
	a.mustTool(t, "bridge_call", map[string]any{"source_call_control_id": inbound, "call_control_id": outbound, "command_id": "b1"})
	a.waitEvents(t, inbound, "call.initiated", "call.answered", "call.bridged")
	a.waitEvents(t, outbound, "call.initiated", "call.answered", "call.bridged")
	state := a.h.state()
	if simRecord(state, "calls", "call_control_id", inbound)["bridged_to"] != outbound || simRecord(state, "calls", "call_control_id", outbound)["bridged_to"] != inbound {
		t.Fatalf("bridged calls = %v", state.Collections["calls"])
	}
	if bridged := a.event(t, outbound, "call.bridged"); bridged["direction"] != "outgoing" || bridged["state"] != "answered" {
		t.Fatalf("outbound call.bridged = %v", bridged)
	}
}

func TestSimulatedCatalogsMatchIntegrationsSource(t *testing.T) {
	for _, slug := range []string{"telnyx", "deepgram"} {
		source, err := os.ReadFile("../integrations/src/apps/" + slug + ".json")
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
		if err != nil || !bytes.Equal(source, embedded) {
			t.Fatalf("embedded %s catalog differs from the integrations source: %v", slug, err)
		}
	}
}
