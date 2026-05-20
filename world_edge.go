package main

// world_edge.go — the cassette-backed HTTP edge for test Worlds.
//
// This generalises eval_sandbox.go's sandboxProxy (allow | mock | block)
// into a five-mode edge that also supports record/replay cassettes — the
// VCR/Polly pattern, applied to a whole agent world. Every process inside
// a World (app sidecars today; the agent core + meta-agent later) gets
// HTTP_PROXY pointed at one WorldEdge, so all of their outbound HTTP is
// classified here without any code changes in the apps themselves.
//
// Per request the edge decides, in order:
//   1. allow      — host matches the allowlist → forward to the real host
//                   (LLM endpoints, loopback for inter-sidecar calls).
//   2. mock        — a hand-written HTTPMock matches → serve the canned body.
//   3. cassette    — a recorded entry matches → replay it deterministically.
//   4. mode default:
//        record     → forward to real, capture the response into the cassette.
//        passthrough → forward to real, don't capture.
//        block/mock/replay-miss → 502 + record (fail loud — the property
//                     that makes replay runs deterministic).
//
// This file is additive: sandboxProxy stays as-is so the current eval path
// is untouched. A later phase migrates eval onto WorldEdge and removes the
// duplication. It reuses eval_sandbox.go's HTTPMock / InterceptedCall /
// SandboxPolicy / hostMatchesSuffix / mockMatches / defaultAllowSuffixes
// (same package), so the edge and the sandbox speak the same vocabulary.
//
// Phase-1 limitation: like sandboxProxy, this serves HTTP forward-proxy
// semantics. HTTPS hosts only get CONNECT-tunnel passthrough for the
// allowlist; mocking/recording HTTPS bodies needs a MITM CA and is Phase 2.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// EdgeMode is the default classification applied to a request that is
// neither allowlisted nor matched by a hand-written mock or cassette entry.
type EdgeMode string

const (
	EdgeBlock       EdgeMode = "block"       // 502 + record; the safe default.
	EdgePassthrough EdgeMode = "passthrough" // forward to the real host, don't capture.
	EdgeMock        EdgeMode = "mock"        // only hand-written mocks served; otherwise block.
	EdgeRecord      EdgeMode = "record"      // forward to real + capture into the cassette.
	EdgeReplay      EdgeMode = "replay"      // serve from cassette; a miss blocks (fail loud).
)

// CassetteEntry is one recorded request→response pair. BodyKey is a short
// hash of the request body so two POSTs to the same path with different
// payloads get distinct entries.
type CassetteEntry struct {
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	BodyKey string            `json:"body_key"` // sha256(req body)[:16], "" when no body
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"` // response body
}

// Cassette is a saveable set of recorded edge crossings. The index is
// rebuilt from Entries on load; only Entries is serialised.
type Cassette struct {
	Entries []CassetteEntry `json:"entries"`

	mu    sync.Mutex
	index map[string]int // signature → Entries idx
}

func newCassette() *Cassette { return &Cassette{index: map[string]int{}} }

func cassetteSig(method, host, path string, body []byte) string {
	bk := ""
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		bk = hex.EncodeToString(sum[:])[:16]
	}
	return method + " " + host + path + "#" + bk
}

func (c *Cassette) reindex() {
	c.index = make(map[string]int, len(c.Entries))
	for i, e := range c.Entries {
		c.index[e.Method+" "+e.Host+e.Path+"#"+e.BodyKey] = i
	}
}

func (c *Cassette) lookup(method, host, path string, body []byte) (CassetteEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.index == nil {
		c.reindex()
	}
	if i, ok := c.index[cassetteSig(method, host, path, body)]; ok {
		return c.Entries[i], true
	}
	return CassetteEntry{}, false
}

func (c *Cassette) put(method, host, path string, body []byte, status int, headers map[string]string, respBody []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.index == nil {
		c.index = map[string]int{}
	}
	bk := ""
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		bk = hex.EncodeToString(sum[:])[:16]
	}
	ent := CassetteEntry{Method: method, Host: host, Path: path, BodyKey: bk, Status: status, Headers: headers, Body: string(respBody)}
	sig := method + " " + host + path + "#" + bk
	if i, ok := c.index[sig]; ok {
		c.Entries[i] = ent
		return
	}
	c.index[sig] = len(c.Entries)
	c.Entries = append(c.Entries, ent)
}

// LoadCassette reads a cassette JSON file and rebuilds its lookup index.
func LoadCassette(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Cassette{}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse cassette %s: %w", path, err)
	}
	c.reindex()
	return c, nil
}

// Save writes the cassette as pretty JSON, suitable for committing
// alongside an eval so CI replay is byte-stable.
func (c *Cassette) Save(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// WorldEdge is the live intercept server. One per World.
type WorldEdge struct {
	listener net.Listener
	server   *http.Server
	policy   SandboxPolicy
	mode     EdgeMode
	cassette *Cassette

	mu    sync.Mutex
	calls []InterceptedCall
}

// startWorldEdge binds a loopback port and starts serving. The caller sets
// the returned ProxyURL() as HTTP_PROXY/HTTPS_PROXY on every in-world
// process. defaultAllowSuffixes (LLM hosts + loopback) is always merged in.
func startWorldEdge(policy SandboxPolicy, mode EdgeMode, cassette *Cassette) (*WorldEdge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("world edge listen: %w", err)
	}
	policy.AllowHostSuffixes = append(append([]string{}, defaultAllowSuffixes...), policy.AllowHostSuffixes...)
	if mode == "" {
		mode = EdgeBlock
	}
	if (mode == EdgeRecord || mode == EdgeReplay) && cassette == nil {
		cassette = newCassette()
	}
	e := &WorldEdge{listener: ln, policy: policy, mode: mode, cassette: cassette}
	mux := http.NewServeMux()
	mux.HandleFunc("/", e.handle)
	e.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go e.server.Serve(ln) //nolint:errcheck
	return e, nil
}

// ProxyURL is the value to set as HTTP_PROXY / HTTPS_PROXY on in-world processes.
func (e *WorldEdge) ProxyURL() string { return "http://" + e.listener.Addr().String() }

// Stop shuts the edge down. Idempotent.
func (e *WorldEdge) Stop() {
	if e.server != nil {
		_ = e.server.Close()
	}
}

// Cassette returns the live cassette (nil unless record/replay was selected
// or one was supplied). Callers Save() it after a record run.
func (e *WorldEdge) Cassette() *Cassette { return e.cassette }

// Calls returns a snapshot of every request the edge has classified — the
// raw material for edge assertions ("exactly 1 POST to api.twitter.com").
func (e *WorldEdge) Calls() []InterceptedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]InterceptedCall, len(e.calls))
	copy(out, e.calls)
	return out
}

func (e *WorldEdge) record(c InterceptedCall) {
	e.mu.Lock()
	e.calls = append(e.calls, c)
	e.mu.Unlock()
}

func (e *WorldEdge) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		e.handleConnect(w, r)
		return
	}
	host := r.URL.Hostname()
	if host == "" {
		host = strings.SplitN(r.Host, ":", 2)[0]
	}
	path := r.URL.Path
	method := r.Method

	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	rec := InterceptedCall{Host: host, Path: path, Method: method, ReqBody: truncate(string(body), 1000), Timestamp: time.Now()}

	// 1. Allowlist passthrough.
	if hostMatchesSuffix(host, e.policy.AllowHostSuffixes) {
		st, hdr, rb, err := e.forward(r, body)
		if err != nil {
			e.fail(w, &rec, http.StatusBadGateway, "world edge: upstream error: "+err.Error())
			return
		}
		rec.Allowed = true
		writeUpstream(w, st, hdr, rb)
		rec.Status, rec.RespBody = st, truncate(string(rb), 1000)
		e.record(rec)
		return
	}

	// 2. Hand-written mocks.
	for _, m := range e.policy.Mocks {
		if mockMatches(m, host, path, method) {
			e.serveMock(w, m, &rec)
			return
		}
	}

	// 3. Cassette replay (any mode, if a cassette is present).
	if e.cassette != nil {
		if ent, ok := e.cassette.lookup(method, host, path, body); ok {
			e.serveCassette(w, ent, &rec)
			return
		}
	}

	// 4. Mode-driven default.
	switch e.mode {
	case EdgeRecord:
		st, hdr, rb, err := e.forward(r, body)
		if err != nil {
			e.fail(w, &rec, http.StatusBadGateway, "world edge: upstream error: "+err.Error())
			return
		}
		if e.cassette != nil {
			e.cassette.put(method, host, path, body, st, flattenHeaders(hdr), rb)
		}
		rec.Recorded = true
		writeUpstream(w, st, hdr, rb)
		rec.Status, rec.RespBody = st, truncate(string(rb), 1000)
		e.record(rec)
	case EdgePassthrough:
		st, hdr, rb, err := e.forward(r, body)
		if err != nil {
			e.fail(w, &rec, http.StatusBadGateway, "world edge: upstream error: "+err.Error())
			return
		}
		rec.Allowed = true
		writeUpstream(w, st, hdr, rb)
		rec.Status, rec.RespBody = st, truncate(string(rb), 1000)
		e.record(rec)
	default: // EdgeBlock, EdgeMock, or an EdgeReplay miss → fail loud.
		e.fail(w, &rec, http.StatusBadGateway,
			fmt.Sprintf("world edge: blocked %s %s%s (mode=%s; no allow/mock/cassette match)", method, host, path, e.mode))
	}
}

func (e *WorldEdge) serveMock(w http.ResponseWriter, m HTTPMock, rec *InterceptedCall) {
	rec.Mocked = true
	status := m.Status
	if status == 0 {
		status = 200
	}
	for k, v := range m.Headers {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	b := m.Body
	if len(b) == 0 {
		b = json.RawMessage(`{}`)
	}
	_, _ = w.Write(b)
	rec.Status, rec.RespBody = status, truncate(string(b), 1000)
	e.record(*rec)
}

func (e *WorldEdge) serveCassette(w http.ResponseWriter, ent CassetteEntry, rec *InterceptedCall) {
	rec.Mocked = true // a cassette hit is a recorded mock as far as the trajectory cares
	for k, v := range ent.Headers {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	status := ent.Status
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(ent.Body))
	rec.Status, rec.RespBody = status, truncate(ent.Body, 1000)
	e.record(*rec)
}

func (e *WorldEdge) fail(w http.ResponseWriter, rec *InterceptedCall, status int, msg string) {
	rec.Blocked = true
	rec.Status = status
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := fmt.Sprintf(`{"error":%q}`, msg)
	_, _ = w.Write([]byte(body))
	rec.RespBody = body
	e.record(*rec)
}

// forward makes the real outbound call. The edge runs inside apteva-server,
// which itself has no HTTP_PROXY, so this reaches the actual host.
func (e *WorldEdge) forward(r *http.Request, body []byte) (int, http.Header, []byte, error) {
	target := *r.URL
	if target.Scheme == "" {
		target.Scheme = "http"
	}
	out, err := http.NewRequest(r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, vs := range r.Header {
		if edgeHopHeader(k) {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(out)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB cap
	return resp.StatusCode, resp.Header, rb, nil
}

// handleConnect tunnels HTTPS only for allowlisted hosts (LLM endpoints).
// Everything else is blocked — mocking/recording HTTPS bodies needs a MITM
// CA, which is Phase 2.
func (e *WorldEdge) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := strings.SplitN(r.Host, ":", 2)[0]
	rec := InterceptedCall{Host: host, Path: r.URL.Path, Method: "CONNECT", Timestamp: time.Now()}
	if hostMatchesSuffix(host, e.policy.AllowHostSuffixes) {
		dest, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, "dial: "+err.Error(), http.StatusBadGateway)
			rec.Blocked, rec.Status = true, http.StatusBadGateway
			e.record(rec)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			dest.Close()
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			dest.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		rec.Allowed, rec.Status = true, 200
		e.record(rec)
		go func() { _, _ = io.Copy(dest, client); dest.Close() }()
		go func() { _, _ = io.Copy(client, dest); client.Close() }()
		return
	}
	rec.Blocked, rec.Status = true, http.StatusBadGateway
	http.Error(w, "world edge: blocked CONNECT to "+host+" (HTTPS mocking is Phase 2 / MITM)", http.StatusBadGateway)
	e.record(rec)
}

func writeUpstream(w http.ResponseWriter, status int, hdr http.Header, body []byte) {
	for k, vs := range hdr {
		if edgeHopHeader(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if edgeHopHeader(k) {
			continue
		}
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

func edgeHopHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding", "Te", "Trailer", "Upgrade":
		return true
	}
	return false
}
