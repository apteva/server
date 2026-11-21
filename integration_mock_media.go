package main

// The media side of a catalog provider simulation. A `stream` step makes the
// simulation act as the provider's media peer: it opens the WebSocket the app
// handed to the provider (always one of the runtime's own apps), speaks the
// provider's media streaming protocol, plays a caller audio fixture into it
// and measures the audio the app sends back. The stream's record in
// media.collection carries its state and audio counters, so a test can
// assert two-way audio from the runtime's simulation state.
//
// Telnyx Media Streaming is the one protocol implemented: JSON text frames
// connected, start, media (base64 PCMU/PCMA at 8 kHz or little-endian L16),
// stop, and mark echoes; the app sends media, mark and clear frames back.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/gorilla/websocket"
)

const (
	mockMediaProtocolTelnyx = "telnyx"
	// mockMediaFrameInterval paces outgoing frames; each frame always holds
	// mockMediaFrameAudio of audio.
	mockMediaFrameInterval = 20 * time.Millisecond
	mockMediaFrameAudio    = 20 * time.Millisecond
	mockMediaDialTimeout   = 10 * time.Second
	mockMediaWriteTimeout  = 5 * time.Second
	mockMediaMaxFrameBytes = 1 << 20
	// A received frame whose RMS is below this (about -50 dBFS) is silence.
	mockMediaSilenceRMS = 100

	mockStreamConnecting  = "connecting"
	mockStreamStreaming   = "streaming"
	mockStreamStopped     = "stopped"
	mockStreamClosed      = "closed"
	mockStreamFailed      = "failed"
	mockStreamInterrupted = "interrupted"

	// mockCallerSilence as caller audio sends silence frames only.
	mockCallerSilence = "silence"
)

type mockMediaDialFunc func(ctx context.Context, target mockDeliveryTarget) (*websocket.Conn, error)

// mockStreamOp is a stream step, applied once its operation succeeds.
type mockStreamOp struct {
	action string // start, play, stop
	key    string
	url    string
	audio  string
}

// mockStreamConfig is a stream record's negotiated media, normalized.
type mockStreamConfig struct {
	codec         string // what the provider sends: PCMU, PCMA or L16
	rate          int
	biCodec       string // what the app sends back
	biRate        int
	track         string
	audio         string
	autoplay      bool
	callControlID string
	clientState   string
}

var mockStreamRates = map[int]bool{8000: true, 16000: true, 22050: true, 24000: true, 48000: true}

func mockNormalizeCodec(raw any, fallback string) (string, error) {
	codec := strings.ToUpper(strings.TrimSpace(mockString(raw)))
	if codec == "" || codec == "DEFAULT" {
		codec = fallback
	}
	switch codec {
	case "PCMU", "PCMA", "L16":
		return codec, nil
	}
	return "", fmt.Errorf("stream codec %s is not simulated; PCMU, PCMA and L16 are", codec)
}

func mockNormalizeRate(codec string, raw any) (int, error) {
	n, _ := mockNumber(raw)
	if text, ok := raw.(string); ok && text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return 0, fmt.Errorf("sample rate %q is not a number", text)
		}
		n = float64(parsed)
	}
	rate := int(n)
	if codec != "L16" {
		if rate != 0 && rate != 8000 {
			return 0, fmt.Errorf("%s streams are 8000 Hz, not %d", codec, rate)
		}
		return 8000, nil
	}
	if rate == 0 {
		return 16000, nil
	}
	if !mockStreamRates[rate] {
		return 0, fmt.Errorf("L16 sample rate %d is not supported", rate)
	}
	return rate, nil
}

func mockStreamConfigFrom(record map[string]any, defaultAudio string) (mockStreamConfig, error) {
	cfg := mockStreamConfig{
		track:         mockString(record["track"]),
		audio:         mockString(record["caller_audio"]),
		autoplay:      record["autoplay"] != false,
		callControlID: mockString(record["call_control_id"]),
		clientState:   mockString(record["client_state"]),
	}
	var err error
	if cfg.codec, err = mockNormalizeCodec(record["codec"], "PCMU"); err != nil {
		return cfg, err
	}
	if cfg.rate, err = mockNormalizeRate(cfg.codec, record["sample_rate"]); err != nil {
		return cfg, err
	}
	if cfg.biCodec, err = mockNormalizeCodec(record["bidirectional_codec"], cfg.codec); err != nil {
		return cfg, err
	}
	biRate := record["bidirectional_sample_rate"]
	if biRate == nil && cfg.biCodec == cfg.codec {
		biRate = float64(cfg.rate)
	}
	if cfg.biRate, err = mockNormalizeRate(cfg.biCodec, biRate); err != nil {
		return cfg, err
	}
	if cfg.track == "" {
		cfg.track = "inbound_track"
	}
	if cfg.audio == "" {
		cfg.audio = defaultAudio
	}
	if !mockAudioFixtureKnown(cfg.audio) {
		return cfg, fmt.Errorf("caller audio %q is not a fixture", cfg.audio)
	}
	return cfg, nil
}

// --- simulation side ---

// applyStreamOps runs the stream steps of a committed operation. sim.mu is
// held; peers do their network work on their own goroutines.
func (sim *integrationSimulation) applyStreamOps(ops []mockStreamOp) {
	for _, op := range ops {
		switch op.action {
		case "start":
			if old := sim.peers[op.key]; old != nil {
				old.detached = true
				old.requestStop()
			}
			sim.startStreamLocked(op)
		case "play":
			if peer := sim.peers[op.key]; peer != nil {
				peer.play(op.audio)
			}
		case "stop":
			if peer := sim.peers[op.key]; peer != nil {
				peer.requestStop()
				continue
			}
			if record := sim.streamRecordLocked(op.key); record != nil {
				if state := mockString(record["state"]); state == mockStreamConnecting || state == mockStreamStreaming {
					record["state"] = mockStreamStopped
					record["ended_at"] = sim.now().UTC().Format(mockTimeLayout)
				}
			}
		}
	}
}

func (sim *integrationSimulation) streamRecordLocked(key string) map[string]any {
	if sim.mock.media == nil {
		return nil
	}
	collection := sim.state.Collections[sim.mock.media.collection]
	if collection == nil {
		return nil
	}
	return collection.Records[key]
}

func (sim *integrationSimulation) startStreamLocked(op mockStreamOp) {
	record := sim.streamRecordLocked(op.key)
	if record == nil {
		return
	}
	cfg, err := mockStreamConfigFrom(record, sim.mock.media.audio)
	record["state"] = mockStreamConnecting
	record["stream_id"] = op.key
	if err == nil {
		record["caller_audio"] = cfg.audio
		record["codec"], record["sample_rate"] = cfg.codec, float64(cfg.rate)
		record["bidirectional_codec"], record["bidirectional_sample_rate"] = cfg.biCodec, float64(cfg.biRate)
		record["track"] = cfg.track
	}
	peer := &mockMediaPeer{sim: sim, key: op.key, cfg: cfg, stopReq: make(chan struct{}), playoutEnd: time.Now()}
	peer.writeStats(record)
	var target mockDeliveryTarget
	if err == nil {
		target, err = sim.resolveTarget(op.url)
		if err != nil {
			sim.recordEvidence(InterceptedCall{Host: hostOf(op.url), Method: http.MethodGet, Blocked: true, Kind: "integration_media", Integration: sim.slug, Event: "stream.failed"})
			err = errors.New("stream URL is outside this runtime; simulated media reaches only the runtime's own apps")
		}
	}
	if err == nil && sim.dialMedia == nil {
		err = errors.New("this runtime cannot open simulated media streams")
	}
	if err == nil && sim.ctx.Err() != nil {
		err = errors.New("runtime stopped")
	}
	if err != nil {
		record["state"] = mockStreamFailed
		record["error"] = err.Error()
		record["ended_at"] = sim.now().UTC().Format(mockTimeLayout)
		if event := sim.streamEventLocked(sim.mock.media.failed, record); event != nil {
			sim.enqueue([]mockPendingEvent{*event})
		}
		return
	}
	peer.target = target
	peer.ctx, peer.cancel = context.WithCancel(sim.ctx)
	if cfg.autoplay {
		peer.queueAudioLocked(cfg.audio)
	}
	sim.peers[op.key] = peer
	sim.wg.Add(1)
	go peer.run()
}

// streamEventLocked renders a media lifecycle event for a stream record.
func (sim *integrationSimulation) streamEventLocked(event string, record map[string]any) *mockPendingEvent {
	media := sim.mock.media
	if media == nil || event == "" || media.to == nil || record == nil {
		return nil
	}
	run := &mockRun{sim: sim, state: sim.state}
	sc := &mockScope{vars: map[string]any{"stream": cloneMockValue(record)}, fn: run}
	to, err := media.to.render(sc, &sim.mock.templates)
	if err != nil {
		return nil
	}
	lane := mockString(to)
	if media.lane != nil {
		if value, err := media.lane.render(sc, &sim.mock.templates); err == nil && mockString(value) != "" {
			lane = mockString(value)
		}
	}
	payload, err := media.payload.render(sc, &sim.mock.templates)
	if err != nil {
		return nil
	}
	return &mockPendingEvent{event: event, to: mockString(to), lane: lane, payload: payload, eventID: run.nextID("uuid", ""), occurredAt: sim.now()}
}

// flushMediaLocked copies live audio counters into the stream records.
func (sim *integrationSimulation) flushMediaLocked() {
	for key, peer := range sim.peers {
		if record := sim.streamRecordLocked(key); record != nil {
			peer.writeStats(record)
		}
	}
}

// streamTransition updates a stream's record and emits its event, if any.
func (sim *integrationSimulation) streamTransition(peer *mockMediaPeer, event string, apply func(record map[string]any)) {
	sim.mu.Lock()
	var record map[string]any
	if !peer.detached {
		record = sim.streamRecordLocked(peer.key)
	}
	var pending *mockPendingEvent
	if record != nil {
		apply(record)
		peer.writeStats(record)
		if !peer.aborted.Load() {
			pending = sim.streamEventLocked(event, record)
		}
	}
	sim.mu.Unlock()
	if pending != nil {
		sim.enqueue([]mockPendingEvent{*pending})
	}
}

// --- the peer ---

type mockMediaPeer struct {
	sim    *integrationSimulation
	key    string
	target mockDeliveryTarget
	cfg    mockStreamConfig
	ctx    context.Context
	cancel context.CancelFunc

	stopOnce sync.Once
	stopReq  chan struct{}
	aborted  atomic.Bool
	// detached peers no longer own their record (replaced or restored
	// over); guarded by sim.mu.
	detached bool

	writeMu sync.Mutex
	conn    *websocket.Conn
	seq     int64
	chunk   int64

	mu         sync.Mutex
	queue      []int16
	started    bool
	playoutEnd time.Time
	marks      []*mockPendingMark
	stats      mockMediaStats
}

type mockPendingMark struct {
	name  string
	timer *time.Timer
	done  bool
}

type mockMediaStats struct {
	plays, sentFrames, sentSamples, sentBytes, silenceFrames              int64
	recvFrames, recvSamples, recvBytes, nonSilentFrames, nonSilentSamples int64
	peak                                                                  int64
	sumSquares                                                            float64
	marks, marksAcked, clears, decodeErrors, unknownFrames                int64
}

func (p *mockMediaPeer) requestStop() {
	p.stopOnce.Do(func() { close(p.stopReq) })
}

// abort ends the stream without events (runtime teardown).
func (p *mockMediaPeer) abort() {
	p.aborted.Store(true)
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *mockMediaPeer) stopRequested() bool {
	select {
	case <-p.stopReq:
		return true
	default:
		return false
	}
}

func (p *mockMediaPeer) play(audio string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if audio == "" {
		audio = p.cfg.audio
	}
	p.queueAudioLocked(audio)
}

// queueAudioLocked starts media and queues one play of a caller fixture.
func (p *mockMediaPeer) queueAudioLocked(audio string) {
	p.started = true
	p.stats.plays++
	if audio == mockCallerSilence {
		return
	}
	samples, err := mockCallerSamples(audio, p.cfg.rate)
	if err != nil {
		return
	}
	p.queue = append(p.queue, samples...)
}

// writeStats copies the peer's counters into its record. Callers hold
// sim.mu; the peer's own lock is taken here.
func (p *mockMediaPeer) writeStats(record map[string]any) {
	p.mu.Lock()
	s := p.stats
	pending := int64(len(p.queue))
	p.mu.Unlock()
	ms := func(samples int64, rate int) float64 {
		if rate <= 0 {
			return 0
		}
		return float64(samples * 1000 / int64(rate))
	}
	rms := 0.0
	if s.recvSamples > 0 {
		rms = math.Round(math.Sqrt(s.sumSquares/float64(s.recvSamples))*10) / 10
	}
	record["sent"] = map[string]any{
		"fixture":        p.cfg.audio,
		"plays":          float64(s.plays),
		"frames":         float64(s.sentFrames),
		"ms":             ms(s.sentSamples, p.cfg.rate),
		"bytes":          float64(s.sentBytes),
		"silence_frames": float64(s.silenceFrames),
		"pending_ms":     ms(pending, p.cfg.rate),
	}
	record["received"] = map[string]any{
		"frames":            float64(s.recvFrames),
		"ms":                ms(s.recvSamples, p.cfg.biRate),
		"bytes":             float64(s.recvBytes),
		"non_silent_frames": float64(s.nonSilentFrames),
		"non_silent_ms":     ms(s.nonSilentSamples, p.cfg.biRate),
		"peak":              float64(s.peak),
		"rms":               rms,
		"marks":             float64(s.marks),
		"marks_acked":       float64(s.marksAcked),
		"clears":            float64(s.clears),
		"decode_errors":     float64(s.decodeErrors),
		"unknown_frames":    float64(s.unknownFrames),
	}
}

func (p *mockMediaPeer) evidencePath() string {
	// The tail of a media URL often carries a per-call secret.
	tail := strings.Trim(p.target.Tail, "/")
	parts := strings.Split(tail, "/")
	if len(parts) > 2 {
		parts = append(parts[:2], "...")
	}
	return "/api/apps/" + p.target.App + "/" + strings.Join(parts, "/")
}

func (p *mockMediaPeer) run() {
	defer p.sim.wg.Done()
	defer p.cancel()
	dialCtx, cancelDial := context.WithTimeout(p.ctx, mockMediaDialTimeout)
	conn, err := p.sim.dialMedia(dialCtx, p.target)
	cancelDial()
	if err != nil {
		p.sim.recordEvidence(InterceptedCall{Host: mockRuntimeHost, Path: p.evidencePath(), Method: http.MethodGet, Mocked: true, Kind: "integration_media", Integration: p.sim.slug, Event: "stream.failed", RespBody: truncateEvidence([]byte(err.Error()))})
		if p.stopRequested() || p.ctx.Err() != nil {
			p.finish(mockStreamStopped, "", 0, "", "")
			return
		}
		p.finish(mockStreamFailed, err.Error(), 0, "", p.sim.mock.media.failed)
		return
	}
	conn.SetReadLimit(mockMediaMaxFrameBytes)
	p.conn = conn
	p.sim.recordEvidence(InterceptedCall{Host: mockRuntimeHost, Path: p.evidencePath(), Method: http.MethodGet, Mocked: true, Status: http.StatusSwitchingProtocols, Kind: "integration_media", Integration: p.sim.slug, Event: "stream.connected"})
	p.sim.streamTransition(p, p.sim.mock.media.started, func(record map[string]any) {
		record["state"] = mockStreamStreaming
		record["connected_at"] = p.sim.now().UTC().Format(mockTimeLayout)
	})

	readerDone := make(chan error, 1)
	if err := p.writeJSON(map[string]any{"event": "connected", "version": "1.0.0"}); err != nil {
		p.closed(conn, err)
		return
	}
	if err := p.writeJSON(p.startFrame()); err != nil {
		p.closed(conn, err)
		return
	}
	go p.read(conn, readerDone)

	interval := p.sim.mediaFrame
	if interval <= 0 {
		interval = mockMediaFrameInterval
	}
	next := time.Now()
	for {
		next = next.Add(interval)
		if behind := time.Since(next); behind > 10*interval {
			next = time.Now()
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-p.ctx.Done():
			timer.Stop()
			_ = conn.Close()
			<-readerDone
			p.finish(mockStreamStopped, "", 0, "", "")
			return
		case <-p.stopReq:
			timer.Stop()
			_ = p.writeJSON(p.stopFrame())
			p.writeMu.Lock()
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stream stopped"), time.Now().Add(time.Second))
			p.writeMu.Unlock()
			select {
			case <-readerDone:
			case <-time.After(2 * time.Second):
			}
			_ = conn.Close()
			p.finish(mockStreamStopped, "", websocket.CloseNormalClosure, "provider", p.sim.mock.media.stopped)
			return
		case err := <-readerDone:
			timer.Stop()
			readerDone <- err
			p.closed(conn, err)
			return
		case <-timer.C:
			if err := p.sendFrame(); err != nil {
				_ = conn.Close()
				err = <-readerDone
				readerDone <- err
				p.closed(conn, err)
				return
			}
		}
	}
}

// closed ends a stream the app closed (or broke).
func (p *mockMediaPeer) closed(conn *websocket.Conn, err error) {
	_ = conn.Close()
	code := 0
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		code = closeErr.Code
	}
	message := ""
	if code == 0 || code != websocket.CloseNormalClosure && code != websocket.CloseGoingAway {
		if err != nil {
			message = err.Error()
		}
	}
	if p.ctx.Err() != nil && p.aborted.Load() {
		p.finish(mockStreamStopped, "", code, "", "")
		return
	}
	p.finish(mockStreamClosed, message, code, "app", p.sim.mock.media.stopped)
}

func (p *mockMediaPeer) finish(state, message string, code int, closedBy, event string) {
	p.mu.Lock()
	for _, mark := range p.marks {
		mark.timer.Stop()
		mark.done = true
	}
	p.marks = nil
	p.mu.Unlock()
	p.sim.mu.Lock()
	if p.sim.peers[p.key] == p {
		delete(p.sim.peers, p.key)
	}
	p.sim.mu.Unlock()
	p.sim.streamTransition(p, event, func(record map[string]any) {
		record["state"] = state
		record["ended_at"] = p.sim.now().UTC().Format(mockTimeLayout)
		if message != "" {
			record["error"] = message
		}
		if code != 0 {
			record["close_code"] = float64(code)
		}
		if closedBy != "" {
			record["closed_by"] = closedBy
		}
	})
}

func (p *mockMediaPeer) writeJSON(frame map[string]any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.conn == nil {
		return errors.New("stream is not connected")
	}
	if frame["event"] != "connected" {
		p.seq++
		frame["sequence_number"] = strconv.FormatInt(p.seq, 10)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(mockMediaWriteTimeout))
	return p.conn.WriteMessage(websocket.TextMessage, raw)
}

func (p *mockMediaPeer) startFrame() map[string]any {
	start := map[string]any{
		"user_id":         mockMediaUserID,
		"call_control_id": p.cfg.callControlID,
		"media_format":    map[string]any{"encoding": p.cfg.codec, "sample_rate": p.cfg.rate, "channels": 1},
	}
	if p.cfg.clientState != "" {
		start["client_state"] = p.cfg.clientState
	}
	return map[string]any{"event": "start", "start": start, "stream_id": p.key}
}

func (p *mockMediaPeer) stopFrame() map[string]any {
	return map[string]any{"event": "stop", "stop": map[string]any{"user_id": mockMediaUserID, "call_control_id": p.cfg.callControlID}, "stream_id": p.key}
}

const mockMediaUserID = "00000000-0000-4000-8000-00000000a7e7"

// sendFrame sends one frame of caller audio, or silence once the queue is
// empty. Nothing is sent before media starts (an outbound call that has not
// been answered yet).
func (p *mockMediaPeer) sendFrame() error {
	frameSamples := int(int64(p.cfg.rate) * int64(mockMediaFrameAudio) / int64(time.Second))
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return nil
	}
	take := min(frameSamples, len(p.queue))
	samples := make([]int16, frameSamples)
	copy(samples, p.queue[:take])
	p.queue = p.queue[take:]
	p.mu.Unlock()

	payload := mockEncodeAudio(samples, p.cfg.codec)
	p.writeMu.Lock()
	p.chunk++
	chunk := p.chunk
	p.writeMu.Unlock()
	err := p.writeJSON(map[string]any{
		"event": "media",
		"media": map[string]any{
			"track":     "inbound",
			"chunk":     strconv.FormatInt(chunk, 10),
			"timestamp": strconv.FormatInt(chunk*int64(mockMediaFrameAudio/time.Millisecond), 10),
			"payload":   base64.StdEncoding.EncodeToString(payload),
		},
		"stream_id": p.key,
	})
	if err != nil {
		return err
	}
	p.mu.Lock()
	if take > 0 {
		p.stats.sentFrames++
		p.stats.sentSamples += int64(take)
	} else {
		p.stats.silenceFrames++
	}
	p.stats.sentBytes += int64(len(payload))
	p.mu.Unlock()
	return nil
}

func (p *mockMediaPeer) read(conn *websocket.Conn, done chan<- error) {
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			done <- err
			return
		}
		if kind != websocket.TextMessage {
			p.count(func(s *mockMediaStats) { s.unknownFrames++ })
			continue
		}
		var frame struct {
			Event string `json:"event"`
			Media *struct {
				Payload string `json:"payload"`
			} `json:"media"`
			Mark *struct {
				Name string `json:"name"`
			} `json:"mark"`
		}
		if json.Unmarshal(data, &frame) != nil {
			p.count(func(s *mockMediaStats) { s.unknownFrames++ })
			continue
		}
		switch frame.Event {
		case "media":
			if frame.Media == nil {
				p.count(func(s *mockMediaStats) { s.decodeErrors++ })
				continue
			}
			p.receiveMedia(frame.Media.Payload)
		case "mark":
			if frame.Mark == nil || frame.Mark.Name == "" {
				p.count(func(s *mockMediaStats) { s.unknownFrames++ })
				continue
			}
			p.scheduleMark(frame.Mark.Name)
		case "clear":
			p.clear()
		default:
			p.count(func(s *mockMediaStats) { s.unknownFrames++ })
		}
	}
}

func (p *mockMediaPeer) count(apply func(*mockMediaStats)) {
	p.mu.Lock()
	apply(&p.stats)
	p.mu.Unlock()
}

func (p *mockMediaPeer) receiveMedia(payload string) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(raw) == 0 {
		p.count(func(s *mockMediaStats) { s.decodeErrors++ })
		return
	}
	samples, err := mockDecodeAudio(raw, p.cfg.biCodec)
	if err != nil || len(samples) == 0 {
		p.count(func(s *mockMediaStats) { s.decodeErrors++ })
		return
	}
	var sum float64
	var peak int64
	for _, sample := range samples {
		v := int64(sample)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
		sum += float64(v * v)
	}
	loud := math.Sqrt(sum/float64(len(samples))) >= mockMediaSilenceRMS
	duration := time.Duration(len(samples)) * time.Second / time.Duration(p.cfg.biRate)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats.recvFrames++
	p.stats.recvSamples += int64(len(samples))
	p.stats.recvBytes += int64(len(raw))
	p.stats.sumSquares += sum
	if peak > p.stats.peak {
		p.stats.peak = peak
	}
	if loud {
		p.stats.nonSilentFrames++
		p.stats.nonSilentSamples += int64(len(samples))
	}
	now := time.Now()
	if p.playoutEnd.Before(now) {
		p.playoutEnd = now
	}
	p.playoutEnd = p.playoutEnd.Add(duration)
}

// scheduleMark echoes a mark once the audio queued before it has played,
// as the provider does.
func (p *mockMediaPeer) scheduleMark(name string) {
	p.mu.Lock()
	p.stats.marks++
	wait := time.Until(p.playoutEnd)
	if wait <= 0 {
		p.mu.Unlock()
		p.ackMark(name)
		return
	}
	mark := &mockPendingMark{name: name}
	mark.timer = time.AfterFunc(wait, func() {
		p.mu.Lock()
		if mark.done {
			p.mu.Unlock()
			return
		}
		mark.done = true
		p.removeMarkLocked(mark)
		p.mu.Unlock()
		p.ackMark(name)
	})
	p.marks = append(p.marks, mark)
	p.mu.Unlock()
}

func (p *mockMediaPeer) removeMarkLocked(mark *mockPendingMark) {
	for i, pending := range p.marks {
		if pending == mark {
			p.marks = append(p.marks[:i], p.marks[i+1:]...)
			return
		}
	}
}

// clear drops the app's queued audio; marks waiting on it are echoed now.
func (p *mockMediaPeer) clear() {
	p.mu.Lock()
	p.stats.clears++
	p.playoutEnd = time.Now()
	pending := p.marks
	p.marks = nil
	var names []string
	for _, mark := range pending {
		if mark.timer.Stop() && !mark.done {
			mark.done = true
			names = append(names, mark.name)
		}
	}
	p.mu.Unlock()
	for _, name := range names {
		p.ackMark(name)
	}
}

func (p *mockMediaPeer) ackMark(name string) {
	if p.writeJSON(map[string]any{"event": "mark", "mark": map[string]any{"name": name}, "stream_id": p.key}) == nil {
		p.count(func(s *mockMediaStats) { s.marksAcked++ })
	}
}

// --- codecs ---

func mockEncodeAudio(samples []int16, codec string) []byte {
	switch codec {
	case "PCMU":
		out := make([]byte, len(samples))
		for i, s := range samples {
			out[i] = mockLinearToULaw(s)
		}
		return out
	case "PCMA":
		out := make([]byte, len(samples))
		for i, s := range samples {
			out[i] = mockLinearToALaw(s)
		}
		return out
	}
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		out[2*i] = byte(uint16(s))
		out[2*i+1] = byte(uint16(s) >> 8)
	}
	return out
}

func mockDecodeAudio(raw []byte, codec string) ([]int16, error) {
	switch codec {
	case "PCMU":
		out := make([]int16, len(raw))
		for i, b := range raw {
			out[i] = mockULawToLinear(b)
		}
		return out, nil
	case "PCMA":
		out := make([]int16, len(raw))
		for i, b := range raw {
			out[i] = mockALawToLinear(b)
		}
		return out, nil
	}
	if len(raw)%2 != 0 {
		return nil, errors.New("L16 payload has an odd length")
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(uint16(raw[2*i]) | uint16(raw[2*i+1])<<8)
	}
	return out, nil
}

// G.711 mu-law and A-law, as in the ITU reference.
func mockLinearToULaw(sample int16) byte {
	const bias, clip = 0x84, 32635
	s := int(sample)
	sign := 0
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > clip {
		s = clip
	}
	s += bias
	exponent := 7
	for mask := 0x4000; s&mask == 0 && exponent > 0; mask >>= 1 {
		exponent--
	}
	mantissa := (s >> (exponent + 3)) & 0x0F
	return ^byte(sign | exponent<<4 | mantissa)
}

func mockULawToLinear(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exponent := int(u>>4) & 0x07
	mantissa := int(u & 0x0F)
	s := ((mantissa << 3) + 0x84) << exponent
	s -= 0x84
	if sign != 0 {
		return int16(-s)
	}
	return int16(s)
}

func mockLinearToALaw(sample int16) byte {
	s := int(sample)
	sign := 0x80
	if s < 0 {
		s = -s - 1
		sign = 0
	}
	if s > 32767 {
		s = 32767
	}
	var out int
	if s < 256 {
		out = s >> 4
	} else {
		exponent := 7
		for mask := 0x4000; s&mask == 0 && exponent > 1; mask >>= 1 {
			exponent--
		}
		out = exponent<<4 | (s>>(exponent+3))&0x0F
	}
	return byte((out | sign) ^ 0x55)
}

func mockALawToLinear(a byte) int16 {
	a ^= 0x55
	exponent := int(a>>4) & 0x07
	mantissa := int(a & 0x0F)
	var s int
	if exponent == 0 {
		s = mantissa<<4 + 8
	} else {
		s = (mantissa<<4 + 0x108) << (exponent - 1)
	}
	if a&0x80 == 0 {
		s = -s
	}
	return int16(s)
}

// --- dialing the runtime's app ---

// dialSimulatedMedia opens a media WebSocket straight to an app of this
// runtime, with the same credentials the runtime's app proxy adds.
func (s *Server) dialSimulatedMedia(ctx context.Context, w *Environment, target mockDeliveryTarget) (*websocket.Conn, error) {
	inst, ok := w.Install(target.App)
	if !ok {
		return nil, fmt.Errorf("app %q is not installed in this runtime", target.App)
	}
	if target.InstallID != 0 && target.InstallID != inst.InstallID {
		return nil, fmt.Errorf("install %d is not app %q in this runtime", target.InstallID, target.App)
	}
	base, err := url.Parse(inst.SidecarURL)
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("app %q has no reachable sidecar", target.App)
	}
	u := *base
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path, u.RawPath, u.RawQuery = target.Tail, "", target.RawQuery
	token, err := s.appInstallToken(inst.InstallID)
	if err != nil {
		return nil, fmt.Errorf("app credential unavailable: %w", err)
	}
	var entry *InstalledApp
	if s.installedApps != nil {
		entry = s.installedApps.Get(inst.InstallID)
	}
	req := &http.Request{Method: http.MethodGet, URL: &u, Header: http.Header{}}
	if appProxyRouteIsNoAuth(entry, target.Tail, http.MethodGet) {
		req.Header.Set("X-Apteva-App-Token", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	sdk.SetClientIPHeaders(req, "127.0.0.1", token)
	dialer := websocket.Dialer{HandshakeTimeout: mockMediaDialTimeout}
	conn, resp, err := dialer.DialContext(ctx, u.String(), req.Header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("app rejected the media WebSocket: %s", resp.Status)
		}
		return nil, err
	}
	return conn, nil
}
