package sshwatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SystemDir is the machine-level collector directory used when it exists and
// is writable, so every apteva-host on the machine shares one collector.
const SystemDir = "/var/lib/apteva-security"

// Config configures a Collector. Zero values select the documented defaults.
type Config struct {
	// Dir holds collector.lock, events.db, policy.json and machine-id.
	Dir string
	// Source overrides log-source discovery (journalctl, then auth.log/secure).
	Source Source
	// JournalctlPath overrides the journalctl binary used by discovery.
	JournalctlPath string
	// LogFiles overrides the syslog files tried when journald is unavailable.
	LogFiles []string

	Backfill      time.Duration // first-run window, default 7 days
	RetentionAge  time.Duration // default 365 days
	MaxEvents     int           // default 500000
	LowSpaceBytes uint64        // default 256 MiB

	// AuthorizedKeysPaths are glob patterns. nil selects
	// DefaultAuthorizedKeysPaths; an empty non-nil slice disables the watcher.
	AuthorizedKeysPaths    []string
	AuthorizedKeysInterval time.Duration // default 30s

	LockRetryInterval    time.Duration // default 10s
	HeartbeatInterval    time.Duration // default 15s
	DeliveryInterval     time.Duration // default 5s
	FlushInterval        time.Duration // default 250ms
	PolicyReloadInterval time.Duration // default 5s
	PruneInterval        time.Duration // default 1h
	SourceRetryInterval  time.Duration // default 5s

	// InitialPolicyFile seeds policy.json when the directory has none.
	InitialPolicyFile string
	// MachineIDPath defaults to /etc/machine-id; when unreadable a random ID
	// is generated once and kept in Dir/machine-id.
	MachineIDPath string
	Hostname      string
	HTTPClient    *http.Client
	// ReadOnly never takes the lock: serve events/health/policy only.
	ReadOnly bool
	Logf     func(format string, args ...any)
	Now      func() time.Time
}

func (cfg *Config) applyDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.Backfill, 7*24*time.Hour)
	def(&cfg.RetentionAge, 365*24*time.Hour)
	def(&cfg.AuthorizedKeysInterval, 30*time.Second)
	def(&cfg.LockRetryInterval, 10*time.Second)
	def(&cfg.HeartbeatInterval, 15*time.Second)
	def(&cfg.DeliveryInterval, 5*time.Second)
	def(&cfg.FlushInterval, 250*time.Millisecond)
	def(&cfg.PolicyReloadInterval, 5*time.Second)
	def(&cfg.PruneInterval, time.Hour)
	def(&cfg.SourceRetryInterval, 5*time.Second)
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = 500000
	}
	if cfg.LowSpaceBytes == 0 {
		cfg.LowSpaceBytes = 256 << 20
	}
	if cfg.AuthorizedKeysPaths == nil {
		cfg.AuthorizedKeysPaths = DefaultAuthorizedKeysPaths
	}
	if cfg.MachineIDPath == "" {
		cfg.MachineIDPath = "/etc/machine-id"
	}
	if cfg.JournalctlPath == "" {
		cfg.JournalctlPath = "journalctl"
	}
	if cfg.LogFiles == nil {
		cfg.LogFiles = []string{"/var/log/auth.log", "/var/log/secure"}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
}

// DefaultDir resolves the collector directory: APTEVA_SECURITY_DIR, else
// SystemDir when it exists and is writable, else $home/host/security.
func DefaultDir(home string) (string, error) {
	if dir := os.Getenv("APTEVA_SECURITY_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	if dirWritable(SystemDir) {
		return SystemDir, nil
	}
	if home == "" {
		return "", fmt.Errorf("no collector directory: set APTEVA_SECURITY_DIR or create %s", SystemDir)
	}
	return filepath.Join(home, "host", "security"), nil
}

// ConfigFromEnv builds a Config from the APTEVA_SECURITY_* environment.
func ConfigFromEnv(home string) (Config, error) {
	var cfg Config
	var err error
	if cfg.Dir, err = DefaultDir(home); err != nil {
		return cfg, err
	}
	cfg.InitialPolicyFile = os.Getenv("APTEVA_SECURITY_POLICY_FILE")
	if v := os.Getenv("APTEVA_SECURITY_BACKFILL"); v != "" {
		if cfg.Backfill, err = time.ParseDuration(v); err != nil {
			return cfg, fmt.Errorf("APTEVA_SECURITY_BACKFILL: %w", err)
		}
	}
	if v := os.Getenv("APTEVA_SECURITY_RETENTION_DAYS"); v != "" {
		days, err := strconv.Atoi(v)
		if err != nil || days < 1 {
			return cfg, errors.New("APTEVA_SECURITY_RETENTION_DAYS must be a positive integer")
		}
		cfg.RetentionAge = time.Duration(days) * 24 * time.Hour
	}
	if v := os.Getenv("APTEVA_SECURITY_MAX_EVENTS"); v != "" {
		if cfg.MaxEvents, err = strconv.Atoi(v); err != nil || cfg.MaxEvents < 1000 {
			return cfg, errors.New("APTEVA_SECURITY_MAX_EVENTS must be an integer >= 1000")
		}
	}
	if v, ok := os.LookupEnv("APTEVA_SECURITY_AUTHORIZED_KEYS"); ok {
		cfg.AuthorizedKeysPaths = []string{}
		if v != "none" {
			for _, p := range filepath.SplitList(v) {
				if p = strings.TrimSpace(p); p != "" {
					cfg.AuthorizedKeysPaths = append(cfg.AuthorizedKeysPaths, p)
				}
			}
		}
	}
	return cfg, nil
}

// Collector owns one collector directory. Run alternates between collecting
// (lock holder) and serving read-only (another process holds the lock).
type Collector struct {
	cfg        Config
	dbPath     string
	lockPath   string
	policyPath string
	reload     chan struct{}

	mu          sync.Mutex
	role        string
	lockHeld    bool
	store       *Store
	policy      Policy
	policyStamp string
	machineID   string
	hostname    string
	source      string
	sourceErr   string
	active      Source
	lastErr     string
}

// New prepares a collector for cfg.Dir (created 0700 if missing).
func New(cfg Config) (*Collector, error) {
	if cfg.Dir == "" {
		return nil, errors.New("collector directory is required")
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	cfg.Dir = dir
	cfg.applyDefaults()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	c := &Collector{
		cfg:        cfg,
		dbPath:     filepath.Join(dir, storeFileName),
		lockPath:   filepath.Join(dir, "collector.lock"),
		policyPath: filepath.Join(dir, policyFileName),
		reload:     make(chan struct{}, 1),
		role:       RoleStopped,
		hostname:   cfg.Hostname,
	}
	if c.hostname == "" {
		c.hostname, _ = os.Hostname()
	}
	return c, nil
}

func (c *Collector) logf(format string, args ...any) {
	if c.cfg.Logf != nil {
		c.cfg.Logf(format, args...)
		return
	}
	log.Printf("[SECURITY] "+format, args...)
}

func (c *Collector) now() time.Time { return c.cfg.Now() }

// Dir returns the collector directory.
func (c *Collector) Dir() string { return c.cfg.Dir }

// Run collects while it holds the lock and otherwise retries the lock every
// LockRetryInterval, serving reads meanwhile. It returns when ctx is done.
func (c *Collector) Run(ctx context.Context) error {
	defer c.setRole(RoleStopped, false)
	loggedLock := false
	for {
		if !c.cfg.ReadOnly {
			release, err := tryLock(c.lockPath)
			if err == nil {
				loggedLock = false
				c.setRole(RoleCollector, true)
				err = c.collect(ctx)
				c.setRole(RoleReader, false)
				release()
				if ctx.Err() != nil {
					return nil
				}
				c.noteError(err)
				c.logf("collector stopped: %v; retrying", err)
			} else if !errors.Is(err, errLocked) {
				c.noteError(err)
				if !loggedLock {
					c.logf("cannot take %s: %v", c.lockPath, err)
					loggedLock = true
				}
			}
		}
		c.setRole(RoleReader, false)
		if !sleepCtx(ctx, c.cfg.LockRetryInterval) {
			return nil
		}
	}
}

// Close releases the store handle. Call after Run returned.
func (c *Collector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		return nil
	}
	err := c.store.Close()
	c.store = nil
	return err
}

func (c *Collector) setRole(role string, held bool) {
	c.mu.Lock()
	c.role, c.lockHeld = role, held
	c.mu.Unlock()
}

func (c *Collector) noteError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.lastErr = err.Error()
	c.mu.Unlock()
}

// Role reports whether this collector currently collects.
func (c *Collector) Role() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.role
}

func (c *Collector) currentStore() *Store {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store
}

// openStore returns the store, opening it on first use. Readers never create
// the database; the holder creates it and its schema.
func (c *Collector) openStore(create bool) (*Store, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store != nil {
		if create {
			if _, err := c.store.db.Exec(schema); err != nil {
				return nil, err
			}
		}
		return c.store, nil
	}
	if !create {
		if _, err := os.Stat(c.dbPath); err != nil {
			return nil, fmt.Errorf("security store is not initialized yet: %w", err)
		}
	}
	st, err := OpenStore(c.dbPath, create)
	if err != nil {
		return nil, err
	}
	c.store = st
	return st, nil
}

func eventID(machineID, key string) string {
	sum := sha256.Sum256([]byte(machineID + "\n" + key))
	return hex.EncodeToString(sum[:])
}

func (c *Collector) loadMachineID(create bool) (string, error) {
	if b, err := os.ReadFile(c.cfg.MachineIDPath); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) == 32 && isHex(id) {
			return id, nil
		}
	}
	path := filepath.Join(c.cfg.Dir, "machine-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) == 32 && isHex(id) {
			return id, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) || !create {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	if err := writeFileAtomic(path, []byte(id+"\n"), 0600); err != nil {
		return "", err
	}
	return id, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// item is one record queued from a source to the batching loop.
type item struct {
	source string
	line   *Line
	gap    *Gap
}

type chanSink struct {
	ch      chan<- item
	source  string
	emitted *int64
	mu      *sync.Mutex
}

func (s chanSink) send(ctx context.Context, it item) error {
	select {
	case s.ch <- it:
		s.mu.Lock()
		*s.emitted++
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s chanSink) Line(ctx context.Context, l Line) error {
	return s.send(ctx, item{source: s.source, line: &l})
}

func (s chanSink) Gap(ctx context.Context, g Gap) error {
	return s.send(ctx, item{source: s.source, gap: &g})
}

func (c *Collector) sources() []Source {
	if c.cfg.Source != nil {
		return []Source{c.cfg.Source}
	}
	var out []Source
	if path, err := exec.LookPath(c.cfg.JournalctlPath); err == nil {
		out = append(out, &JournalSource{Binary: path, Backfill: c.cfg.Backfill, Now: c.cfg.Now})
	}
	for _, f := range c.cfg.LogFiles {
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
			out = append(out, &FileSource{Path: f, Backfill: c.cfg.Backfill, Now: c.cfg.Now})
		}
	}
	return out
}

func (c *Collector) setSource(src Source, errText string) {
	c.mu.Lock()
	c.active, c.source, c.sourceErr = src, "", errText
	if src != nil {
		c.source = src.Name()
	}
	c.mu.Unlock()
}

// sourceStatus returns the active source name and its error or warning.
func (c *Collector) sourceStatus() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	errText := c.sourceErr
	if j, ok := c.active.(*JournalSource); ok && errText == "" {
		errText = j.Warning()
	}
	return c.source, errText
}

func (c *Collector) sourceLoop(ctx context.Context, st *Store, items chan<- item) {
	candidates := c.sources()
	index := 0
	for ctx.Err() == nil {
		if len(candidates) == 0 {
			c.setSource(nil, "no sshd log source found (journalctl, "+strings.Join(c.cfg.LogFiles, ", ")+")")
			if !sleepCtx(ctx, 12*c.cfg.SourceRetryInterval) {
				return
			}
			candidates = c.sources()
			continue
		}
		src := candidates[index%len(candidates)]
		c.setSource(src, "")
		cursor, err := st.Cursor(ctx, src.Name())
		if err != nil {
			c.setSource(src, err.Error())
			sleepCtx(ctx, c.cfg.SourceRetryInterval)
			continue
		}
		var emitted int64
		var mu sync.Mutex
		err = src.Run(ctx, cursor, chanSink{ch: items, source: src.Name(), emitted: &emitted, mu: &mu})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("source ended")
		}
		c.setSource(src, err.Error())
		c.logf("source %s: %v", src.Name(), err)
		mu.Lock()
		produced := emitted
		mu.Unlock()
		if produced == 0 && len(candidates) > 1 {
			index++ // fall back to the next source
		}
		if !sleepCtx(ctx, c.cfg.SourceRetryInterval) {
			return
		}
	}
}

func (c *Collector) currentPolicy() Policy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.policy
}

func policyStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	// Writes replace the file by rename, so the inode changes even when the
	// size and a coarse mtime do not.
	dev, ino, _ := fileIdentity(fi)
	return fmt.Sprintf("%d:%d:%d:%d", dev, ino, fi.ModTime().UnixNano(), fi.Size())
}

// loadPolicy reads policy.json, seeding it from InitialPolicyFile (or the
// defaults) when missing. A malformed file keeps the last good policy.
func (c *Collector) loadPolicy(seed bool) (Policy, error) {
	stamp := policyStamp(c.policyPath)
	c.mu.Lock()
	if stamp != "" && stamp == c.policyStamp {
		p := c.policy
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()
	p, err := readPolicyFile(c.policyPath)
	if errors.Is(err, fs.ErrNotExist) {
		if !seed {
			p, _ = Policy{}.Normalize()
			return p, nil
		}
		p, _ = Policy{}.Normalize()
		if c.cfg.InitialPolicyFile != "" {
			if p, err = readPolicyFile(c.cfg.InitialPolicyFile); err != nil {
				return Policy{}, fmt.Errorf("APTEVA_SECURITY_POLICY_FILE: %w", err)
			}
		}
		if err = writePolicyFile(c.policyPath, p); err != nil {
			return Policy{}, err
		}
		stamp = policyStamp(c.policyPath)
	} else if err != nil {
		return c.currentPolicy(), err
	}
	c.mu.Lock()
	c.policy, c.policyStamp = p, stamp
	c.mu.Unlock()
	return p, nil
}

// Policy returns the persisted policy with the webhook secret redacted.
func (c *Collector) Policy() (Policy, error) {
	p, err := c.loadPolicy(false)
	return p.Redacted(), err
}

// SetPolicy validates, persists and applies p. An empty webhook secret keeps
// the stored one (unless the webhook URL is cleared).
func (c *Collector) SetPolicy(p Policy) (Policy, error) {
	next, err := p.Normalize()
	if err != nil {
		return Policy{}, err
	}
	if next.AlertWebhookURL != "" && next.AlertWebhookSecret == "" {
		if prev, err := readPolicyFile(c.policyPath); err == nil {
			next.AlertWebhookSecret = prev.AlertWebhookSecret
		}
	}
	if err := writePolicyFile(c.policyPath, next); err != nil {
		return Policy{}, err
	}
	select {
	case c.reload <- struct{}{}:
	default:
	}
	if _, err := c.loadPolicy(false); err != nil {
		return Policy{}, err
	}
	return next.Redacted(), nil
}

// recordPolicy applies the current policy file in the holder and records a
// policy_changed event when its digest differs from the last applied one.
func (c *Collector) recordPolicy(ctx context.Context, st *Store) error {
	p, err := c.loadPolicy(true)
	if err != nil {
		return err
	}
	digest := p.digest()
	return st.Update(ctx, func(tx *Tx) error {
		prev, err := tx.Meta(metaPolicyDigest)
		if err != nil || prev == digest {
			return err
		}
		if prev != "" {
			ev := Event{
				ID:         eventID(c.machineID, "policy:"+prev+":"+digest+":"+strconv.FormatInt(c.now().UnixNano(), 10)),
				MachineID:  c.machineID,
				Hostname:   c.hostname,
				ObservedAt: c.now().UTC(),
				Kind:       KindPolicyChanged,
				Result:     ResultChanged,
				Message:    fmt.Sprintf("policy updated: %d approved source(s), %d approved fingerprint(s), root login allowed=%t, webhook configured=%t", len(p.ApprovedSources), len(p.ApprovedFingerprints), p.RootLoginAllowed, p.AlertWebhookURL != ""),
				Source:     "policy",
			}
			if _, err := tx.InsertEvent(&ev, p); err != nil {
				return err
			}
		}
		return tx.SetMeta(metaPolicyDigest, digest)
	})
}

func (c *Collector) lifecycleEvent(kind string, at time.Time) Event {
	return Event{
		ID:         eventID(c.machineID, kind+":"+strconv.Itoa(os.Getpid())+":"+strconv.FormatInt(at.UnixNano(), 10)),
		MachineID:  c.machineID,
		Hostname:   c.hostname,
		ObservedAt: at,
		Kind:       kind,
		PID:        os.Getpid(),
		Source:     "collector",
	}
}

// collect runs while holding the lock: sources -> batches -> store.
func (c *Collector) collect(ctx context.Context) error {
	st, err := c.openStore(true)
	if err != nil {
		return err
	}
	machineID, err := c.loadMachineID(true)
	if err != nil {
		return fmt.Errorf("machine id: %w", err)
	}
	c.mu.Lock()
	c.machineID = machineID
	c.policyStamp = "" // force a fresh read in this role
	c.mu.Unlock()
	if err := c.recordPolicy(ctx, st); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	started := c.now().UTC()
	startEv := c.lifecycleEvent(KindCollectorStarted, started)
	startEv.Message = "SSH collector started in " + c.cfg.Dir
	if err := st.Update(ctx, func(tx *Tx) error {
		if _, err := tx.InsertEvent(&startEv, c.currentPolicy()); err != nil {
			return err
		}
		for k, v := range map[string]string{
			metaStartedAt: started.Format(time.RFC3339Nano), metaStoppedAt: "", metaHeartbeat: started.Format(time.RFC3339Nano),
			metaCollectorPID: strconv.Itoa(os.Getpid()), metaMachineID: machineID, metaHostname: c.hostname,
		} {
			if err := tx.SetMeta(k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := st.Prune(ctx, c.now(), c.cfg.RetentionAge, c.cfg.MaxEvents); err != nil {
		c.logf("prune: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	items := make(chan item, 1024)
	var wg sync.WaitGroup
	sourceDone := make(chan struct{})
	wg.Add(2)
	go func() { defer close(sourceDone); c.sourceLoop(runCtx, st, items) }()
	go func() { defer wg.Done(); c.keysLoop(runCtx) }()
	go func() { defer wg.Done(); c.deliveryLoop(runCtx, st) }()

	var batch []item
	flush := func(ctx context.Context) error {
		if len(batch) == 0 {
			return nil
		}
		if err := c.commit(ctx, st, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	flushT := time.NewTicker(c.cfg.FlushInterval)
	heartbeatT := time.NewTicker(c.cfg.HeartbeatInterval)
	policyT := time.NewTicker(c.cfg.PolicyReloadInterval)
	pruneT := time.NewTicker(c.cfg.PruneInterval)
	defer flushT.Stop()
	defer heartbeatT.Stop()
	defer policyT.Stop()
	defer pruneT.Stop()
	shutdown := func(loopErr error) error {
		cancel()
		// Drain what the source already produced; it stops on runCtx.
		for {
			select {
			case it := <-items:
				batch = append(batch, it)
				continue
			case <-sourceDone:
			}
			break
		}
		for len(items) > 0 {
			batch = append(batch, <-items)
		}
		wg.Wait()
		final, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := flush(final); err != nil && loopErr == nil {
			loopErr = err
		}
		stopped := c.now().UTC()
		stopEv := c.lifecycleEvent(KindCollectorStopped, stopped)
		stopEv.Message = "SSH collector stopped"
		if loopErr != nil {
			stopEv.Message += ": " + truncate(loopErr.Error(), 256)
		}
		_ = st.Update(final, func(tx *Tx) error {
			if _, err := tx.InsertEvent(&stopEv, c.currentPolicy()); err != nil {
				return err
			}
			return tx.SetMeta(metaStoppedAt, stopped.Format(time.RFC3339Nano))
		})
		return loopErr
	}
	for {
		select {
		case <-ctx.Done():
			return shutdown(nil)
		case it := <-items:
			batch = append(batch, it)
			if len(batch) >= 256 {
				if err := flush(ctx); err != nil {
					return shutdown(err)
				}
			}
		case <-flushT.C:
			if err := flush(ctx); err != nil {
				return shutdown(err)
			}
		case <-heartbeatT.C:
			source, sourceErr := c.sourceStatus()
			if err := st.SetMeta(ctx, map[string]string{metaHeartbeat: c.now().UTC().Format(time.RFC3339Nano), metaSource: source, metaSourceError: sourceErr}); err != nil {
				c.logf("heartbeat: %v", err)
			}
		case <-policyT.C:
			if err := c.recordPolicy(ctx, st); err != nil {
				c.logf("policy reload: %v", err)
			}
		case <-c.reload:
			if err := c.recordPolicy(ctx, st); err != nil {
				c.logf("policy reload: %v", err)
			}
		case <-pruneT.C:
			if _, err := st.Prune(ctx, c.now(), c.cfg.RetentionAge, c.cfg.MaxEvents); err != nil {
				c.logf("prune: %v", err)
			}
		}
	}
}

// commit stores one batch atomically: new events (deduplicated by ID), their
// outbox rows and the advanced cursors.
func (c *Collector) commit(ctx context.Context, st *Store, batch []item) error {
	policy := c.currentPolicy()
	return st.Update(ctx, func(tx *Tx) error {
		cursors := map[string]string{}
		var lastEvent time.Time
		for _, it := range batch {
			switch {
			case it.gap != nil:
				g := it.gap
				to := g.To
				if to.IsZero() {
					to = c.now().UTC()
				}
				from := g.From
				if from.IsZero() {
					from = lastEvent
				}
				if from.IsZero() {
					// Last time this source's cursor advanced bounds the gap.
					from, _ = tx.cursorUpdatedAt(it.source)
				}
				ev := Event{
					ID:         eventID(c.machineID, "gap:"+it.source+":"+g.Cursor+":"+g.Reason),
					MachineID:  c.machineID,
					Hostname:   c.hostname,
					ObservedAt: to.UTC(),
					Kind:       KindCollectorGap,
					Message:    SanitizeMessage(g.Reason),
					Source:     it.source,
					GapTo:      to.UTC(),
				}
				if !from.IsZero() {
					ev.GapFrom = from.UTC()
				}
				if _, err := tx.InsertEvent(&ev, policy); err != nil {
					return err
				}
			case it.line != nil:
				l := it.line
				if l.Message != "" && (l.Ident == "" || isSSHIdent(l.Ident)) {
					if ev, ok := ParseMessage(l.Message); ok {
						key := l.Key
						if key == "" {
							key = l.Cursor
						}
						if key == "" {
							continue // cannot be deduplicated; never store twice
						}
						ev.ID = eventID(c.machineID, key)
						ev.MachineID = c.machineID
						ev.Hostname = cleanField(l.Hostname)
						if ev.Hostname == "" {
							ev.Hostname = c.hostname
						}
						ev.ObservedAt = l.Time.UTC()
						if l.Time.IsZero() {
							ev.ObservedAt = c.now().UTC()
						}
						ev.PID = l.PID
						ev.Source = it.source
						if _, err := tx.InsertEvent(&ev, policy); err != nil {
							return err
						}
						lastEvent = ev.ObservedAt
					}
				}
				if l.Cursor != "" {
					cursors[it.source] = l.Cursor
				}
			}
		}
		for source, cursor := range cursors {
			if err := tx.SetCursor(source, cursor); err != nil {
				return err
			}
		}
		return nil
	})
}

// Events returns stored events after seq (limit is clamped to 1..2000).
func (c *Collector) Events(ctx context.Context, after int64, limit int) (EventsPage, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}
	if after < 0 {
		after = 0
	}
	st, err := c.openStore(false)
	if err != nil {
		return EventsPage{Events: []Event{}, NextAfter: after}, nil
	}
	return st.Events(ctx, after, limit)
}

// Health reports collector state; holder-written fields come from the store.
func (c *Collector) Health(ctx context.Context) Health {
	c.mu.Lock()
	h := Health{Role: c.role, LockHeld: c.lockHeld, Dir: c.cfg.Dir, MachineID: c.machineID, Hostname: c.hostname}
	localErr := c.lastErr
	c.mu.Unlock()
	if free, err := freeSpace(c.cfg.Dir); err == nil {
		h.FreeBytes = free
		h.LowSpace = free < c.cfg.LowSpaceBytes
	}
	if p, err := c.loadPolicy(false); err == nil {
		h.WebhookConfigured = p.AlertWebhookURL != ""
	}
	st, err := c.openStore(false)
	if err != nil {
		h.StoreError = err.Error()
		if localErr != "" {
			h.StoreError += "; " + localErr
		}
		h.Stale = true
		return h
	}
	meta := func(k string) string { v, _ := st.Meta(ctx, k); return v }
	h.Heartbeat = parseMetaTime(meta(metaHeartbeat))
	h.StartedAt = parseMetaTime(meta(metaStartedAt))
	h.StoppedAt = parseMetaTime(meta(metaStoppedAt))
	h.CollectorPID = parseMetaInt(meta(metaCollectorPID))
	h.Source = meta(metaSource)
	h.SourceError = meta(metaSourceError)
	h.AlertLastError = meta(metaAlertLastError)
	h.AlertLastDeliveredAt = parseMetaTime(meta(metaAlertLastDelivered))
	h.AuthorizedKeysError = meta(metaKeysError)
	if h.MachineID == "" {
		h.MachineID = meta(metaMachineID)
	}
	if v := meta(metaHostname); v != "" {
		h.Hostname = v
	}
	if h.LockHeld {
		// The holder's live view is fresher than its last heartbeat.
		h.Source, h.SourceError = c.sourceStatus()
	}
	if h.Source != "" {
		h.Cursor, _ = st.Cursor(ctx, h.Source)
	}
	if stats, err := st.stats(ctx); err == nil {
		h.EventCount, h.LastSeq, h.AlertBacklog, h.GapCount = stats.count, stats.lastSeq, stats.backlog, stats.gaps
		h.LastEventAt, h.LastGapAt = stats.lastEvent, stats.lastGap
	} else {
		h.StoreError = err.Error()
	}
	h.Stale = h.Heartbeat.IsZero() || c.now().Sub(h.Heartbeat) > 3*c.cfg.HeartbeatInterval || (!h.StoppedAt.IsZero() && !h.StoppedAt.Before(h.Heartbeat))
	return h
}
