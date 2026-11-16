package sshwatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource replays a fixed list of records whose cursors are "c<i>". It
// resumes after the stored cursor but also re-sends `resend` already-seen
// records, like a source that cannot position exactly. An unknown cursor is
// reported as a gap.
type fakeSource struct {
	mu     sync.Mutex
	lines  []Line
	resend int
	runs   int
}

func newFakeSource(resend int, prefix string, messages ...string) *fakeSource {
	f := &fakeSource{resend: resend}
	base := time.Now().UTC().Add(-time.Minute)
	for i, m := range messages {
		f.lines = append(f.lines, Line{Cursor: prefix + strconv.Itoa(i), Time: base.Add(time.Duration(i) * time.Second), Hostname: "web1", Ident: "sshd", PID: 100 + i, Message: m})
	}
	return f
}

func (f *fakeSource) Name() string { return "fake" }

func (f *fakeSource) Runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func (f *fakeSource) Run(ctx context.Context, cursor string, sink Sink) error {
	f.mu.Lock()
	f.runs++
	lines := append([]Line(nil), f.lines...)
	f.mu.Unlock()
	start := 0
	if cursor != "" {
		idx := -1
		for i, l := range lines {
			if l.Cursor == cursor {
				idx = i
			}
		}
		if idx < 0 {
			if err := sink.Gap(ctx, Gap{Reason: "fake cursor not found (rotated)", Cursor: cursor}); err != nil {
				return err
			}
		} else {
			start = max(idx+1-f.resend, 0)
		}
	}
	for _, l := range lines[start:] {
		if err := sink.Line(ctx, l); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

type webhook struct {
	*httptest.Server
	secret string
	mu     sync.Mutex
	byKey  map[string]int
	bodies []AlertDelivery
	bad    int
}

func newWebhook(t *testing.T, secret string) *webhook {
	w := &webhook{secret: secret, byKey: map[string]int{}}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		if !VerifySignature(w.secret, body, r.Header.Get(HeaderSignature)) {
			w.bad++
			http.Error(rw, "bad signature", 401)
			return
		}
		var d AlertDelivery
		if err := json.Unmarshal(body, &d); err != nil || d.IdempotencyKey != r.Header.Get(HeaderIdempotencyKey) {
			w.bad++
			http.Error(rw, "bad body", 400)
			return
		}
		w.byKey[d.IdempotencyKey]++
		w.bodies = append(w.bodies, d)
		rw.WriteHeader(204)
	}))
	t.Cleanup(w.Close)
	return w
}

func (w *webhook) snapshot() (map[string]int, []AlertDelivery, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	keys := map[string]int{}
	for k, v := range w.byKey {
		keys[k] = v
	}
	return keys, append([]AlertDelivery(nil), w.bodies...), w.bad
}

type running struct {
	c      *Collector
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
		if r.err != nil {
			t.Fatal(r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("collector did not stop")
	}
	r.c.Close()
}

func testConfig(dir string, src Source) Config {
	return Config{
		Dir:                  dir,
		Source:               src,
		AuthorizedKeysPaths:  []string{},
		LockRetryInterval:    30 * time.Millisecond,
		HeartbeatInterval:    50 * time.Millisecond,
		DeliveryInterval:     20 * time.Millisecond,
		FlushInterval:        10 * time.Millisecond,
		PolicyReloadInterval: 30 * time.Millisecond,
		SourceRetryInterval:  30 * time.Millisecond,
		MachineIDPath:        filepath.Join(dir, "no-such-machine-id"),
		Hostname:             "test-host",
		Logf:                 func(string, ...any) {},
	}
}

func start(t *testing.T, cfg Config) *running {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{c: c, cancel: cancel, done: make(chan struct{})}
	go func() { r.err = c.Run(ctx); close(r.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
		}
		c.Close()
	})
	return r
}

func allEvents(t *testing.T, c *Collector) []Event {
	t.Helper()
	var out []Event
	after := int64(0)
	for {
		page, err := c.Events(context.Background(), after, 2000)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Events...)
		if len(page.Events) == 0 {
			return out
		}
		after = page.NextAfter
	}
}

func countKind(events []Event, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func setPolicy(t *testing.T, dir string, p Policy) {
	t.Helper()
	c, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
}

const knownLogin = "Accepted publickey for deploy from 198.51.100.23 port 51234 ssh2: ED25519 " + sampleFP

// Acceptance 1 + 2: a known key from an unapproved IP alerts via the webhook
// with no apteva-server present; a restart that replays already-seen records
// neither duplicates events nor re-delivers alerts.
func TestKnownKeyFromUnapprovedSourceAlertsOnceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	hook := newWebhook(t, "hook-secret")
	setPolicy(t, dir, Policy{ApprovedSources: []string{"10.0.0.0/8"}, ApprovedFingerprints: []string{sampleFP}, AlertWebhookURL: hook.URL, AlertWebhookSecret: "hook-secret"})

	msgs := []string{
		"Accepted publickey for deploy from 10.1.2.3 port 40000 ssh2: ED25519 " + sampleFP, // approved: no alert
		knownLogin,
		"pam_unix(sshd:session): session opened for user deploy(uid=1000) by (uid=0)",
	}
	r := start(t, testConfig(dir, newFakeSource(0, "c", msgs...)))
	eventually(t, "alert delivery", func() bool {
		keys, _, _ := hook.snapshot()
		return len(keys) == 1 && r.c.Health(context.Background()).AlertBacklog == 0
	})
	keys, bodies, bad := hook.snapshot()
	if bad != 0 || bodies[0].Alert.Rule != RuleUnapprovedSource || bodies[0].Event.SourceIP != "198.51.100.23" || bodies[0].Event.Fingerprint != sampleFP {
		t.Fatalf("delivery %+v bad=%d", bodies, bad)
	}
	if want := bodies[0].Event.ID + ":" + RuleUnapprovedSource; keys[want] != 1 {
		t.Fatalf("idempotency key %v want %s", keys, want)
	}
	h := r.c.Health(context.Background())
	if h.Role != RoleCollector || !h.LockHeld || h.Source != "fake" || h.Cursor != "c2" || h.MachineID == "" || !h.WebhookConfigured {
		t.Fatalf("health %+v", h)
	}
	before := allEvents(t, r.c)
	if countKind(before, KindSSHAuth) != 2 || countKind(before, KindSSHSession) != 1 {
		t.Fatalf("events %+v", before)
	}
	r.stop(t)

	// Restart: replays the last 2 already-stored records plus one new one.
	src := newFakeSource(2, "c", append(msgs, "Failed password for root from 203.0.113.5 port 22 ssh2")...)
	r = start(t, testConfig(dir, src))
	eventually(t, "new record", func() bool { return countKind(allEvents(t, r.c), KindSSHAuth) == 3 })
	time.Sleep(150 * time.Millisecond) // several delivery ticks
	after := allEvents(t, r.c)
	seen := map[string]bool{}
	for _, e := range after {
		if seen[e.ID] {
			t.Fatalf("duplicate event %s", e.ID)
		}
		seen[e.ID] = true
	}
	if countKind(after, KindSSHAuth) != 3 || countKind(after, KindSSHSession) != 1 || countKind(after, KindCollectorStarted) != 2 || countKind(after, KindCollectorStopped) != 1 {
		t.Fatalf("events after restart %+v", after)
	}
	keys, _, _ = hook.snapshot()
	if len(keys) != 1 {
		t.Fatalf("deliveries after restart %v", keys)
	}
	for k, n := range keys {
		if n != 1 {
			t.Fatalf("alert %s delivered %d times", k, n)
		}
	}
	for i := 1; i < len(after); i++ {
		if after[i].Seq <= after[i-1].Seq {
			t.Fatal("seq not monotonic")
		}
	}
}

// Acceptance 3: an invalidated cursor produces a collector_gap event.
func TestInvalidatedCursorRecordsGap(t *testing.T) {
	dir := t.TempDir()
	r := start(t, testConfig(dir, newFakeSource(0, "c", "Failed password for root from 203.0.113.5 port 22 ssh2")))
	eventually(t, "first record", func() bool { return countKind(allEvents(t, r.c), KindSSHAuth) == 1 })
	r.stop(t)

	// The journal was vacuumed/rotated: none of the new source's cursors match.
	r = start(t, testConfig(dir, newFakeSource(0, "d", "Failed password for root from 203.0.113.6 port 22 ssh2")))
	eventually(t, "gap", func() bool { return countKind(allEvents(t, r.c), KindCollectorGap) == 1 })
	var gap Event
	for _, e := range allEvents(t, r.c) {
		if e.Kind == KindCollectorGap {
			gap = e
		}
	}
	if gap.GapTo.IsZero() || gap.GapFrom.IsZero() || len(gap.Alerts) != 1 || gap.Alerts[0].Rule != RuleCollectorGap {
		t.Fatalf("gap event %+v", gap)
	}
	eventually(t, "health gap", func() bool {
		h := r.c.Health(context.Background())
		return h.GapCount == 1 && !h.LastGapAt.IsZero() && h.Cursor == "d0"
	})
	// No webhook configured: alerts stay queued for the server.
	if h := r.c.Health(context.Background()); h.AlertBacklog < 1 || h.WebhookConfigured {
		t.Fatalf("health %+v", h)
	}
}

// Acceptance 4: two collectors on one directory; one collects, the other
// serves reads and takes over when the first stops.
func TestSingleCollectorPerDirectoryWithTakeover(t *testing.T) {
	dir := t.TempDir()
	srcA := newFakeSource(0, "c", "Failed password for root from 203.0.113.5 port 22 ssh2")
	srcB := newFakeSource(0, "c", "Failed password for root from 203.0.113.5 port 22 ssh2", "Failed password for root from 203.0.113.6 port 22 ssh2")
	a := start(t, testConfig(dir, srcA))
	eventually(t, "a collecting", func() bool { return a.c.Role() == RoleCollector })
	b := start(t, testConfig(dir, srcB))
	eventually(t, "b reading", func() bool { return b.c.Role() == RoleReader })
	eventually(t, "a stored", func() bool { return countKind(allEvents(t, a.c), KindSSHAuth) == 1 })
	time.Sleep(100 * time.Millisecond)
	if srcB.Runs() != 0 || b.c.Role() != RoleReader {
		t.Fatalf("second collector collected: runs=%d role=%s", srcB.Runs(), b.c.Role())
	}
	// The reader serves the holder's events and health from the shared store.
	if got := countKind(allEvents(t, b.c), KindSSHAuth); got != 1 {
		t.Fatalf("reader events %d", got)
	}
	if h := b.c.Health(context.Background()); h.LockHeld || h.Role != RoleReader || h.Stale || h.Source != "fake" || h.EventCount == 0 {
		t.Fatalf("reader health %+v", h)
	}
	a.stop(t)
	eventually(t, "b takeover", func() bool { return b.c.Role() == RoleCollector && srcB.Runs() > 0 })
	eventually(t, "b collected", func() bool { return countKind(allEvents(t, b.c), KindSSHAuth) == 2 })
	if got := countKind(allEvents(t, b.c), KindCollectorStarted); got != 2 {
		t.Fatalf("collector_started events %d", got)
	}
}

func TestPolicyPersistenceAndChangeEvent(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(seed, []byte(`{"approved_sources":["10.0.0.0/8"],"root_login_allowed":false}`), 0600)
	cfg := testConfig(dir, newFakeSource(0, "c"))
	cfg.InitialPolicyFile = seed
	r := start(t, cfg)
	eventually(t, "seeded policy", func() bool {
		_, err := os.Stat(filepath.Join(dir, policyFileName))
		return err == nil
	})
	p, err := r.c.Policy()
	if err != nil || len(p.ApprovedSources) != 1 {
		t.Fatalf("seeded policy %+v %v", p, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, policyFileName)); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("policy file mode: %v %v", fi, err)
	}
	if _, err := r.c.SetPolicy(Policy{ApprovedSources: []string{"bogus"}}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if _, err := r.c.SetPolicy(Policy{ApprovedSources: []string{"192.0.2.0/24"}, AlertWebhookURL: "https://alerts.example/x", AlertWebhookSecret: "first"}); err != nil {
		t.Fatal(err)
	}
	// Empty secret keeps the stored one; reads never reveal it.
	got, err := r.c.SetPolicy(Policy{ApprovedSources: []string{"192.0.2.0/24"}, AlertWebhookURL: "https://alerts.example/y"})
	if err != nil || got.AlertWebhookSecret != "" || !got.AlertWebhookSecretSet {
		t.Fatalf("put response %+v %v", got, err)
	}
	stored, _ := readPolicyFile(filepath.Join(dir, policyFileName))
	if stored.AlertWebhookSecret != "first" || stored.AlertWebhookURL != "https://alerts.example/y" {
		t.Fatalf("stored %+v", stored)
	}
	eventually(t, "policy_changed", func() bool { return countKind(allEvents(t, r.c), KindPolicyChanged) >= 1 })
}

func TestAuthorizedKeysWatcher(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	keysPath := filepath.Join(home, "authorized_keys")
	k1, fp1 := newKey(t)
	k2, fp2 := newKey(t)
	os.WriteFile(keysPath, []byte(k1+" ops\n"), 0600)
	setPolicy(t, dir, Policy{ApprovedFingerprints: []string{fp1}})
	cfg := testConfig(dir, newFakeSource(0, "c"))
	cfg.AuthorizedKeysPaths = []string{filepath.Join(home, "authorized_*")}
	cfg.AuthorizedKeysInterval = 20 * time.Millisecond
	r := start(t, cfg)
	keyEvents := func() []Event {
		var out []Event
		for _, e := range allEvents(t, r.c) {
			if e.Kind == KindAuthorizedKeysChange {
				out = append(out, e)
			}
		}
		return out
	}
	eventually(t, "baseline", func() bool { return len(keyEvents()) == 1 })
	if e := keyEvents()[0]; e.Result != ResultBaseline || len(e.Alerts) != 0 || e.KeyCount != 1 {
		t.Fatalf("baseline %+v", e)
	}
	os.WriteFile(keysPath, []byte(k1+" ops\n"+k2+" attacker\n"), 0600)
	eventually(t, "added", func() bool { return len(keyEvents()) == 2 })
	added := keyEvents()[1]
	if added.Result != ResultChanged || len(added.KeysAdded) != 1 || added.KeysAdded[0].Fingerprint != fp2 || len(added.Alerts) != 1 || added.Alerts[0].Severity != SeverityCritical {
		t.Fatalf("added %+v", added)
	}
	raw, _ := json.Marshal(added)
	if blob := strings.Fields(k2)[1]; strings.Contains(string(raw), blob[:24]) {
		t.Fatal("key material stored")
	}
	os.Remove(keysPath)
	eventually(t, "removed", func() bool { return len(keyEvents()) == 3 })
	if e := keyEvents()[2]; len(e.KeysRemoved) != 2 || e.KeyCount != 0 {
		t.Fatalf("removed %+v", e)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(keyEvents()); n != 3 {
		t.Fatalf("unchanged files produced events: %d", n)
	}
}
