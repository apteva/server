package main

// Runs one catalog provider simulation for one fake runtime connection: the
// state it keeps, the steps of its tools and triggers, and the signed webhook
// deliveries they emit. Deliveries are asynchronous and in order per lane
// (for Telnyx, per call), and they never hold the state lock, because an app
// commonly calls the provider again from inside its webhook handler.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const (
	mockTimeLayout         = "2006-01-02T15:04:05.000000Z07:00"
	mockMaxDeliveriesKept  = 2000
	mockMaxDedupeKeys      = 10000
	mockMaxEvidenceBody    = 16 << 10
	mockRuntimeHost        = "runtime.apteva.invalid"
	mockDeliveryUserAgent  = "apteva-integration-simulation"
	mockSimulationErrorKey = "mock.simulation_error"
)

// mockState is the simulation's provider-side state. It is captured into
// runtime snapshots, so it holds JSON-shaped values only.
type mockState struct {
	Seq         int64                           `json:"seq"`
	Collections map[string]*mockCollectionState `json:"collections"`
	Dedupe      map[string]mockCachedResponse   `json:"dedupe,omitempty"`
}

type mockCollectionState struct {
	Order   []string                  `json:"order"`
	Records map[string]map[string]any `json:"records"`
}

type mockCachedResponse struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

func newMockState() *mockState {
	return &mockState{Collections: map[string]*mockCollectionState{}, Dedupe: map[string]mockCachedResponse{}}
}

func (s *mockState) clone() *mockState {
	out := &mockState{Seq: s.Seq, Collections: map[string]*mockCollectionState{}, Dedupe: s.Dedupe}
	for name, collection := range s.Collections {
		copied := &mockCollectionState{Order: append([]string(nil), collection.Order...), Records: map[string]map[string]any{}}
		for key, record := range collection.Records {
			copied.Records[key] = cloneMockValue(record).(map[string]any)
		}
		out.Collections[name] = copied
	}
	return out
}

func (s *mockState) collection(name string) *mockCollectionState {
	c := s.Collections[name]
	if c == nil {
		c = &mockCollectionState{Records: map[string]map[string]any{}}
		s.Collections[name] = c
	}
	return c
}

func (c *mockCollectionState) put(key string, record map[string]any) {
	if _, exists := c.Records[key]; !exists {
		c.Order = append(c.Order, key)
	}
	c.Records[key] = record
}

func (c *mockCollectionState) remove(key string) {
	delete(c.Records, key)
	for i, existing := range c.Order {
		if existing == key {
			c.Order = append(c.Order[:i], c.Order[i+1:]...)
			break
		}
	}
}

// mockDeliveryTarget is a webhook URL resolved to one of the runtime's apps.
type mockDeliveryTarget struct {
	App       string
	InstallID int64
	Tail      string
	RawQuery  string
}

func (t mockDeliveryTarget) path() string {
	return "/api/apps/" + t.App + t.Tail
}

type mockDeliverFunc func(ctx context.Context, target mockDeliveryTarget, header http.Header, body []byte) (int, []byte, error)

type integrationSimulation struct {
	slug         string
	app          string
	role         string
	connectionID int64
	environment  string
	mock         *compiledMock
	template     *AppTemplate
	salt         uint32
	now          func() time.Time
	privateKey   ed25519.PrivateKey
	deliver      mockDeliverFunc
	evidence     func(InterceptedCall)
	backoff      func(attempt int) time.Duration
	// fixtureBase is the public URL prefix of the fixture files the server
	// serves (fixture_url). dialMedia opens a media WebSocket to one of the
	// runtime's apps; mediaFrame is the media pacing interval.
	fixtureBase string
	dialMedia   mockMediaDialFunc
	mediaFrame  time.Duration

	mu    sync.Mutex
	state *mockState
	peers map[string]*mockMediaPeer // live media streams by record key, under mu

	dmu        sync.Mutex
	deliveries []*mockDelivery
	lanes      map[string]*mockLane
	stopped    bool
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

type mockLane struct {
	queue   []*mockDelivery
	running bool
}

type mockDelivery struct {
	record  sdk.RuntimeIntegrationDelivery
	to      string
	payload any
	done    chan struct{}
}

type mockPendingEvent struct {
	event, to, lane, eventID string
	payload                  any
	occurredAt               time.Time
}

type mockOutcome struct {
	status     int
	body       any
	deliveries []*mockDelivery
}

func newIntegrationSimulation(environmentID, slug, app, role string, connectionID int64, tmpl *AppTemplate, mock *compiledMock) *integrationSimulation {
	ctx, cancel := context.WithCancel(context.Background())
	digest := sha256.Sum256([]byte(environmentID + "\x00" + slug + "\x00" + app + "\x00" + role))
	return &integrationSimulation{
		slug:         slug,
		app:          app,
		role:         role,
		connectionID: connectionID,
		environment:  environmentID,
		mock:         mock,
		template:     tmpl,
		salt:         binary.BigEndian.Uint32(digest[:4]),
		now:          func() time.Time { return time.Now().UTC() },
		state:        newMockState(),
		peers:        map[string]*mockMediaPeer{},
		mediaFrame:   mockMediaFrameInterval,
		lanes:        map[string]*mockLane{},
		ctx:          ctx,
		cancel:       cancel,
		backoff: func(attempt int) time.Duration {
			return time.Duration(attempt*attempt) * 100 * time.Millisecond
		},
	}
}

// --- functions available to expressions ---

type mockRun struct {
	sim   *integrationSimulation
	state *mockState
}

func (r *mockRun) now() string { return r.sim.now().UTC().Format(mockTimeLayout) }

func (r *mockRun) fixtureURL(name string) string { return r.sim.fixtureURL(name) }

func (sim *integrationSimulation) fixtureURL(name string) string {
	base := strings.TrimRight(sim.fixtureBase, "/")
	if base == "" {
		base = "http://localhost/api/integration-simulation/fixtures"
	}
	return base + "/" + url.PathEscape(name)
}

func (r *mockRun) nextID(kind, prefix string) string {
	r.state.Seq++
	n := r.state.Seq
	switch kind {
	case "numeric":
		return strconv.FormatInt(1_000_000_000_000_000_000+int64(r.sim.salt%1_000_000)*1_000_000_000+n, 10)
	case "token":
		return fmt.Sprintf("%smock-%08x-%06d", prefix, r.sim.salt, n)
	}
	return fmt.Sprintf("%08x-0000-4000-8000-%012d", r.sim.salt, n)
}

type mockStaticFunctions struct {
	at  string
	sim *integrationSimulation
}

func (f mockStaticFunctions) now() string                  { return f.at }
func (f mockStaticFunctions) nextID(string, string) string { return "" }
func (f mockStaticFunctions) fixtureURL(name string) string {
	if f.sim == nil {
		return ""
	}
	return f.sim.fixtureURL(name)
}

// --- seeding and restore ---

func (sim *integrationSimulation) seed(seed map[string][]map[string]any) error {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	work := sim.state.clone()
	run := &mockRun{sim: sim, state: work}
	names := make([]string, 0, len(seed))
	for name := range seed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := sim.mock.collections[name]
		if def == nil {
			return fmt.Errorf("seed: unknown collection %q", name)
		}
		if !def.seedable {
			return fmt.Errorf("seed: collection %q cannot be seeded", name)
		}
		for i, raw := range seed[name] {
			record, _ := cloneMockValue(def.defaults).(map[string]any)
			if record == nil {
				record = map[string]any{}
			}
			for key, value := range raw {
				record[key] = cloneMockValue(value)
			}
			key, err := sim.recordKey(run, def, record)
			if err != nil {
				return fmt.Errorf("seed %s[%d]: %w", name, i, err)
			}
			if _, exists := work.collection(name).Records[key]; exists {
				return fmt.Errorf("seed %s[%d]: duplicate %s %q", name, i, def.key, key)
			}
			work.collection(name).put(key, record)
		}
	}
	sim.state = work
	return nil
}

func (sim *integrationSimulation) restore(state *mockState) error {
	if state == nil {
		return nil
	}
	for name := range state.Collections {
		if sim.mock.collections[name] == nil {
			return fmt.Errorf("snapshot state has unknown collection %q", name)
		}
	}
	restored := state.clone()
	restored.Dedupe = map[string]mockCachedResponse{}
	for key, value := range state.Dedupe {
		restored.Dedupe[key] = value
	}
	// A snapshot holds stream records, not their connections.
	if media := sim.mock.media; media != nil {
		if streams := restored.Collections[media.collection]; streams != nil {
			for _, record := range streams.Records {
				if live := mockString(record["state"]); live == mockStreamConnecting || live == mockStreamStreaming {
					record["state"] = mockStreamInterrupted
					record["error"] = "runtime restored from a snapshot"
				}
			}
		}
	}
	sim.mu.Lock()
	for key, peer := range sim.peers {
		peer.detached = true
		peer.abort()
		delete(sim.peers, key)
	}
	sim.state = restored
	sim.mu.Unlock()
	return nil
}

func (sim *integrationSimulation) snapshotState() *mockState {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	sim.flushMediaLocked()
	out := sim.state.clone()
	out.Dedupe = map[string]mockCachedResponse{}
	for key, value := range sim.state.Dedupe {
		out.Dedupe[key] = mockCachedResponse{Status: value.Status, Body: cloneMockValue(value.Body)}
	}
	return out
}

func (sim *integrationSimulation) recordKey(run *mockRun, def *compiledMockCollection, record map[string]any) (string, error) {
	if value := record[def.key]; value != nil && mockString(value) != "" {
		return mockString(value), nil
	}
	if def.idKind == "" {
		return "", fmt.Errorf("record has no %s", def.key)
	}
	key := run.nextID(def.idKind, def.idPrefix)
	record[def.key] = key
	return key, nil
}

// --- execution ---

type mockStepError struct {
	name   string
	detail string
	status int
	body   any
}

func (sim *integrationSimulation) execute(op *compiledMockOperation, input map[string]any) mockOutcome {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	work := sim.state.clone()
	run := &mockRun{sim: sim, state: work}
	if input == nil {
		input = map[string]any{}
	}
	sc := &mockScope{vars: map[string]any{"input": cloneMockValue(input)}, fn: run}
	var events []mockPendingEvent
	var streams []mockStreamOp
	dedupeKey := ""

	fail := func(step *mockStep, fallback string) mockOutcome {
		return sim.errorOutcome(sc, step, fallback)
	}

	for _, step := range op.steps {
		if step.when != nil {
			value, err := step.when.eval(sc)
			if err != nil {
				return sim.internalError(err.Error())
			}
			if !mockTruthy(value) {
				continue
			}
		}
		switch step.op {
		case "let":
			for i, key := range step.letKeys {
				value, err := step.letValues[i].render(sc, &sim.mock.templates)
				if err != nil {
					return sim.internalError(err.Error())
				}
				sc.vars[key] = value
			}
		case "require":
			value, err := step.condition.eval(sc)
			if err != nil {
				return sim.internalError(err.Error())
			}
			if !mockTruthy(value) {
				return fail(step, "requirement failed")
			}
		case "lookup", "update", "delete":
			keyValue, err := step.key.render(sc, &sim.mock.templates)
			if err != nil {
				return sim.internalError(err.Error())
			}
			key := mockString(keyValue)
			collection := work.collection(step.target)
			record, exists := collection.Records[key]
			if !exists {
				if step.errName != "" || step.op != "lookup" {
					return fail(step, fmt.Sprintf("%s %q not found", step.target, key))
				}
				sc.vars[step.as] = nil
				continue
			}
			switch step.op {
			case "update":
				if err := sim.applyFields(sc, step, record); err != nil {
					return sim.internalError(err.Error())
				}
				def := sim.mock.collections[step.target]
				if mockString(record[def.key]) != key {
					return sim.internalError(fmt.Sprintf("update cannot change %s", def.key))
				}
			case "delete":
				collection.remove(key)
			}
			if step.as != "" {
				sc.vars[step.as] = cloneMockValue(record)
			}
		case "find", "list":
			matches, err := sim.match(sc, step, work)
			if err != nil {
				return sim.internalError(err.Error())
			}
			if step.op == "list" {
				sc.vars[step.as] = matches
				continue
			}
			if len(matches) == 0 {
				if step.errName != "" {
					return fail(step, "no match")
				}
				sc.vars[step.as] = nil
				continue
			}
			sc.vars[step.as] = matches[0]
		case "create":
			def := sim.mock.collections[step.target]
			record, _ := cloneMockValue(def.defaults).(map[string]any)
			if record == nil {
				record = map[string]any{}
			}
			if err := sim.applyFields(sc, step, record); err != nil {
				return sim.internalError(err.Error())
			}
			key, err := sim.recordKey(run, def, record)
			if err != nil {
				return sim.internalError(err.Error())
			}
			collection := work.collection(step.target)
			if _, exists := collection.Records[key]; exists {
				return fail(step, fmt.Sprintf("%s %q already exists", step.target, key))
			}
			collection.put(key, record)
			if step.as != "" {
				sc.vars[step.as] = cloneMockValue(record)
			}
		case "emit":
			to, err := step.to.render(sc, &sim.mock.templates)
			if err != nil {
				return sim.internalError(err.Error())
			}
			lane := mockString(to)
			if step.lane != nil {
				value, err := step.lane.render(sc, &sim.mock.templates)
				if err != nil {
					return sim.internalError(err.Error())
				}
				if text := mockString(value); text != "" {
					lane = text
				}
			}
			payload := any(map[string]any{})
			if step.payload != nil {
				if payload, err = step.payload.render(sc, &sim.mock.templates); err != nil {
					return sim.internalError(err.Error())
				}
			}
			events = append(events, mockPendingEvent{
				event: step.target, to: mockString(to), lane: lane, payload: payload,
				eventID: run.nextID("uuid", ""), occurredAt: sim.now(),
			})
		case "dedupe":
			value, err := step.dedupe.render(sc, &sim.mock.templates)
			if err != nil {
				return sim.internalError(err.Error())
			}
			key := mockString(value)
			if key == "" {
				continue
			}
			if cached, ok := sim.state.Dedupe[key]; ok {
				return mockOutcome{status: cached.Status, body: cloneMockValue(cached.Body)}
			}
			dedupeKey = key
		case "stream":
			keyValue, err := step.key.render(sc, &sim.mock.templates)
			if err != nil {
				return sim.internalError(err.Error())
			}
			op := mockStreamOp{action: step.target, key: mockString(keyValue)}
			if _, exists := work.collection(sim.mock.media.collection).Records[op.key]; !exists {
				return fail(step, fmt.Sprintf("stream %q not found", op.key))
			}
			if step.to != nil {
				to, err := step.to.render(sc, &sim.mock.templates)
				if err != nil {
					return sim.internalError(err.Error())
				}
				op.url = mockString(to)
			}
			if step.audio != nil {
				audio, err := step.audio.render(sc, &sim.mock.templates)
				if err != nil {
					return sim.internalError(err.Error())
				}
				op.audio = mockString(audio)
				if op.audio != "" && !mockAudioFixtureKnown(op.audio) {
					return fail(step, fmt.Sprintf("audio %q is not a caller audio fixture", op.audio))
				}
			}
			streams = append(streams, op)
		case "respond":
			body, err := step.respond.render(sc, &sim.mock.templates)
			if err != nil {
				return sim.internalError(err.Error())
			}
			status := step.status
			if status == 0 {
				status = http.StatusOK
			}
			if dedupeKey != "" && len(sim.state.Dedupe) < mockMaxDedupeKeys {
				dedupe := make(map[string]mockCachedResponse, len(sim.state.Dedupe)+1)
				for key, value := range sim.state.Dedupe {
					dedupe[key] = value
				}
				dedupe[dedupeKey] = mockCachedResponse{Status: status, Body: cloneMockValue(body)}
				work.Dedupe = dedupe
			}
			sim.state = work
			deliveries := sim.enqueue(events)
			sim.applyStreamOps(streams)
			return mockOutcome{status: status, body: body, deliveries: deliveries}
		}
	}
	return sim.internalError("operation ended without respond")
}

func (sim *integrationSimulation) applyFields(sc *mockScope, step *mockStep, record map[string]any) error {
	if step.merge != nil {
		value, err := step.merge.render(sc, &sim.mock.templates)
		if err != nil {
			return err
		}
		if value != nil {
			fields, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("merge must produce an object")
			}
			for key, item := range fields {
				if !step.omit[key] {
					record[key] = item
				}
			}
		}
	}
	for _, part := range []mockTemplate{step.record, step.set} {
		if part == nil {
			continue
		}
		value, err := part.render(sc, &sim.mock.templates)
		if err != nil {
			return err
		}
		for key, item := range value.(map[string]any) {
			record[key] = item
		}
	}
	for _, key := range step.unset {
		delete(record, key)
	}
	return nil
}

func (sim *integrationSimulation) match(sc *mockScope, step *mockStep, work *mockState) ([]any, error) {
	want := map[string]any{}
	for i, key := range step.whereKeys {
		value, err := step.whereVals[i].render(sc, &sim.mock.templates)
		if err != nil {
			return nil, err
		}
		if value != nil {
			want[key] = value
		}
	}
	collection := work.collection(step.target)
	out := []any{}
	for _, key := range collection.Order {
		record := collection.Records[key]
		matched := true
		for field, value := range want {
			if !mockEqual(record[field], value) {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, cloneMockValue(record))
		}
	}
	return out, nil
}

func (sim *integrationSimulation) errorOutcome(sc *mockScope, step *mockStep, fallback string) mockOutcome {
	def, ok := sim.mock.errors[step.errName]
	if !ok {
		return sim.internalError(fallback)
	}
	detail := fallback
	if step.detail != nil {
		if value, err := step.detail.render(sc, &sim.mock.templates); err == nil {
			detail = mockString(value)
		}
	}
	scoped := &mockScope{vars: map[string]any{}, fn: sc.fn}
	for key, value := range sc.vars {
		scoped.vars[key] = value
	}
	scoped.vars["detail"] = detail
	body, err := def.body.render(scoped, &sim.mock.templates)
	if err != nil {
		return sim.internalError(err.Error())
	}
	return mockOutcome{status: def.status, body: body}
}

func (sim *integrationSimulation) internalError(detail string) mockOutcome {
	return mockOutcome{status: http.StatusInternalServerError, body: map[string]any{
		"errors": []any{map[string]any{"code": mockSimulationErrorKey, "title": "Simulation error", "detail": detail}},
	}}
}

// --- delivery ---

func (sim *integrationSimulation) enqueue(events []mockPendingEvent) []*mockDelivery {
	if len(events) == 0 {
		return nil
	}
	sim.dmu.Lock()
	defer sim.dmu.Unlock()
	out := make([]*mockDelivery, 0, len(events))
	for _, event := range events {
		d := &mockDelivery{
			record: sdk.RuntimeIntegrationDelivery{
				ID:         fmt.Sprintf("dlv_%d_%d", sim.connectionID, len(sim.deliveries)+1),
				Event:      event.event,
				EventID:    event.eventID,
				Lane:       event.lane,
				State:      "queued",
				OccurredAt: event.occurredAt,
			},
			to:      event.to,
			payload: event.payload,
			done:    make(chan struct{}),
		}
		if target, err := sim.resolveTarget(event.to); err == nil {
			d.record.App, d.record.Target = target.App, target.path()
		}
		sim.deliveries = append(sim.deliveries, d)
		if len(sim.deliveries) > mockMaxDeliveriesKept {
			sim.deliveries = sim.deliveries[len(sim.deliveries)-mockMaxDeliveriesKept:]
		}
		out = append(out, d)
		if sim.stopped {
			sim.finishLocked(d, "failed", 0, "runtime stopped")
			continue
		}
		lane := sim.lanes[event.lane]
		if lane == nil {
			lane = &mockLane{}
			sim.lanes[event.lane] = lane
		}
		lane.queue = append(lane.queue, d)
		if !lane.running {
			lane.running = true
			sim.wg.Add(1)
			go sim.drain(event.lane, lane)
		}
	}
	return out
}

func (sim *integrationSimulation) drain(name string, lane *mockLane) {
	defer sim.wg.Done()
	for {
		sim.dmu.Lock()
		if len(lane.queue) == 0 || sim.stopped {
			lane.running = false
			if sim.lanes[name] == lane {
				delete(sim.lanes, name)
			}
			sim.dmu.Unlock()
			return
		}
		d := lane.queue[0]
		lane.queue = lane.queue[1:]
		sim.dmu.Unlock()
		sim.deliverOne(d)
	}
}

func (sim *integrationSimulation) deliverOne(d *mockDelivery) {
	target, err := sim.resolveTarget(d.to)
	if err != nil {
		sim.recordEvidence(InterceptedCall{Host: hostOf(d.to), Path: pathOf(d.to), Method: http.MethodPost, Blocked: true, Kind: "integration_webhook", Integration: sim.slug, Event: d.record.Event})
		sim.finish(d, "undeliverable", 0, err.Error())
		return
	}
	for attempt := 1; attempt <= sim.mock.maxAttempts; attempt++ {
		if sim.ctx.Err() != nil {
			sim.finish(d, "failed", 0, "runtime stopped")
			return
		}
		sim.dmu.Lock()
		d.record.State, d.record.Attempts = "delivering", attempt
		sim.dmu.Unlock()
		body, header, err := sim.signedEnvelope(d, attempt)
		if err != nil {
			sim.finish(d, "failed", 0, err.Error())
			return
		}
		status, response, deliverErr := sim.deliver(sim.ctx, target, header, body)
		sim.recordEvidence(InterceptedCall{
			Host: mockRuntimeHost, Path: target.path(), Method: http.MethodPost, Mocked: true,
			ReqBody: truncateEvidence(body), RespBody: truncateEvidence(response), Status: status,
			Kind: "integration_webhook", Integration: sim.slug, Event: d.record.Event,
		})
		if deliverErr == nil && status >= 200 && status < 300 {
			sim.finish(d, "delivered", status, "")
			return
		}
		message := ""
		if deliverErr != nil {
			message = deliverErr.Error()
		} else {
			message = fmt.Sprintf("app responded %d", status)
		}
		retryable := deliverErr != nil || sim.mock.retry[status]
		if !retryable || attempt == sim.mock.maxAttempts {
			sim.finish(d, "failed", status, message)
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

func (sim *integrationSimulation) signedEnvelope(d *mockDelivery, attempt int) ([]byte, http.Header, error) {
	sc := &mockScope{vars: map[string]any{
		"event":       d.record.Event,
		"event_id":    d.record.EventID,
		"occurred_at": d.record.OccurredAt.UTC().Format(mockTimeLayout),
		"payload":     d.payload,
		"attempt":     float64(attempt),
	}, fn: mockStaticFunctions{at: sim.now().UTC().Format(mockTimeLayout), sim: sim}}
	envelope, err := sim.mock.envelope.render(sc, &sim.mock.templates)
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("User-Agent", mockDeliveryUserAgent)
	if signing := sim.mock.signing; signing != nil && sim.privateKey != nil {
		// Providers sign with wall-clock time and apps check it against
		// theirs, so the logical runtime clock is used for occurred_at only.
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		message := strings.NewReplacer("{timestamp}", timestamp, "{body}", string(body)).Replace(signing.Message)
		signature := ed25519.Sign(sim.privateKey, []byte(message))
		header.Set(signing.TimestampHeader, timestamp)
		header.Set(signing.SignatureHeader, base64.StdEncoding.EncodeToString(signature))
	}
	return body, header, nil
}

func (sim *integrationSimulation) finish(d *mockDelivery, state string, status int, message string) {
	sim.dmu.Lock()
	defer sim.dmu.Unlock()
	sim.finishLocked(d, state, status, message)
}

func (sim *integrationSimulation) finishLocked(d *mockDelivery, state string, status int, message string) {
	select {
	case <-d.done:
		return
	default:
	}
	completed := time.Now().UTC()
	d.record.State, d.record.Status, d.record.Error, d.record.CompletedAt = state, status, message, &completed
	close(d.done)
}

// resolveTarget accepts only URLs that point at this runtime's own apps:
// the runtime platform URL its installs advertise, or its loopback gateway.
// Nothing is ever sent over the network.
func (sim *integrationSimulation) resolveTarget(raw string) (mockDeliveryTarget, error) {
	outside := fmt.Errorf("webhook URL is outside this runtime; simulated events reach only the runtime's own apps")
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return mockDeliveryTarget{}, outside
	}
	path := u.EscapedPath()
	var rest string
	switch {
	case strings.EqualFold(u.Hostname(), mockRuntimeHost):
		prefix := "/" + url.PathEscape(sim.environment) + "/api/apps/"
		if !strings.HasPrefix(path, prefix) {
			return mockDeliveryTarget{}, outside
		}
		rest = strings.TrimPrefix(path, prefix)
	case isLoopbackHostname(u.Hostname()):
		prefix := "/api/environment-app-public/" + url.PathEscape(sim.environment) + "/api/apps/"
		if !strings.HasPrefix(path, prefix) {
			return mockDeliveryTarget{}, outside
		}
		rest = strings.TrimPrefix(path, prefix)
	default:
		return mockDeliveryTarget{}, outside
	}
	parts := strings.SplitN(rest, "/", 2)
	app, err := url.PathUnescape(parts[0])
	if err != nil || app == "" {
		return mockDeliveryTarget{}, outside
	}
	tail := "/"
	if len(parts) == 2 {
		tail += parts[1]
	}
	target := mockDeliveryTarget{App: app, Tail: tail, RawQuery: u.RawQuery}
	if strings.HasPrefix(tail, "/_install/") {
		installParts := strings.SplitN(strings.TrimPrefix(tail, "/_install/"), "/", 2)
		id, err := strconv.ParseInt(installParts[0], 10, 64)
		if err != nil || id <= 0 {
			return mockDeliveryTarget{}, outside
		}
		target.InstallID = id
		target.Tail = "/"
		if len(installParts) == 2 {
			target.Tail += installParts[1]
		}
	}
	if unescaped, err := url.PathUnescape(target.Tail); err == nil {
		target.Tail = unescaped
	}
	return target, nil
}

func isLoopbackHostname(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}

// pathOf drops the query string, which often carries a route secret.
func pathOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return ""
}

func truncateEvidence(body []byte) string {
	if len(body) > mockMaxEvidenceBody {
		return string(body[:mockMaxEvidenceBody])
	}
	return string(body)
}

func (sim *integrationSimulation) recordEvidence(call InterceptedCall) {
	if sim.evidence == nil {
		return
	}
	call.Timestamp = time.Now().UTC()
	call.LogicalTime = sim.now()
	sim.evidence(call)
}

func (sim *integrationSimulation) stop() {
	sim.mu.Lock()
	for _, peer := range sim.peers {
		peer.abort()
	}
	// Cancelled under mu so no stream step can start a peer afterwards.
	sim.cancel()
	sim.mu.Unlock()
	sim.dmu.Lock()
	sim.stopped = true
	sim.cancel()
	for _, lane := range sim.lanes {
		for _, d := range lane.queue {
			sim.finishLocked(d, "failed", 0, "runtime stopped")
		}
		lane.queue = nil
	}
	sim.dmu.Unlock()
	waited := make(chan struct{})
	go func() {
		sim.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
	}
}

// publicState is the read model returned by the runtime state endpoint.
func (sim *integrationSimulation) publicState() sdk.RuntimeIntegrationState {
	state := sim.snapshotState()
	out := sdk.RuntimeIntegrationState{
		Slug: sim.slug, App: sim.app, Role: sim.role, ConnectionID: sim.connectionID,
		Collections: map[string][]map[string]any{}, Deliveries: sim.deliveryRecords(nil),
	}
	for name := range sim.mock.collections {
		records := []map[string]any{}
		if collection := state.Collections[name]; collection != nil {
			for _, key := range collection.Order {
				records = append(records, collection.Records[key])
			}
		}
		out.Collections[name] = records
	}
	return out
}

func (sim *integrationSimulation) deliveryRecords(only []*mockDelivery) []sdk.RuntimeIntegrationDelivery {
	sim.dmu.Lock()
	defer sim.dmu.Unlock()
	source := sim.deliveries
	if only != nil {
		source = only
	}
	out := make([]sdk.RuntimeIntegrationDelivery, 0, len(source))
	for _, d := range source {
		record := d.record
		if record.CompletedAt != nil {
			completed := *record.CompletedAt
			record.CompletedAt = &completed
		}
		out = append(out, record)
	}
	return out
}
