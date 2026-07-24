package main

// In-process edge cache for the HostRouter's reverse proxy.
//
// Scope is deliberately narrow and conservative — this is a CDN-style
// cache for PUBLIC assets, not an HTTP-spec-complete cache:
//
//   - only safe GETs with no Authorization/Cookie are eligible
//   - only responses the origin explicitly marked cacheable are stored:
//     Cache-Control must contain "public" with a positive max-age (or
//     "immutable"), and carry no Set-Cookie / Vary / private / no-store.
//     This is exactly what the storage app emits for public files
//     (Cache-Control: public, max-age=31536000, immutable).
//   - bounded by per-object and total byte budgets; oldest-out eviction
//   - honors ETag via 304 on If-None-Match
//
// When in doubt, it doesn't cache. Anything signed/private/authed flows
// straight through to the origin untouched.

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type EdgeCache struct {
	mu       sync.Mutex
	entries  map[string]*cacheEntry
	order    []string // insertion order, for oldest-out eviction
	curBytes int64
	maxBytes int64
	maxItem  int64
	ttlCap   time.Duration
}

type cacheEntry struct {
	status  int
	header  http.Header
	body    []byte
	etag    string
	expires time.Time
	size    int64
}

func NewEdgeCache() *EdgeCache {
	return &EdgeCache{
		entries:  map[string]*cacheEntry{},
		maxBytes: 64 << 20, // 64 MiB total
		maxItem:  8 << 20,  // 8 MiB per object — bigger assets stream uncached
		ttlCap:   24 * time.Hour,
	}
}

func edgeKey(host, reqURI string) string {
	return strings.ToLower(host) + "\x00" + reqURI
}

// serve writes a fresh cached response for this request and returns
// true, or returns false to let the caller proxy to the origin.
func (c *EdgeCache) serve(w http.ResponseWriter, r *http.Request, host string) bool {
	if !cacheableRequest(r) {
		return false
	}
	key := edgeKey(host, r.URL.RequestURI())
	c.mu.Lock()
	e, ok := c.entries[key]
	if ok && time.Now().After(e.expires) {
		c.removeLocked(key)
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return false
	}

	// Conditional request: 304 when the client already holds this ETag.
	if e.etag != "" && etagMatches(r.Header.Get("If-None-Match"), e.etag) {
		copyHeader(w.Header(), e.header)
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	copyHeader(w.Header(), e.header)
	w.Header().Set("X-Cache", "HIT")
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
	return true
}

// wrap returns a ResponseWriter that tees the origin response to the
// client and buffers it for caching if it turns out eligible. Call
// finalize() after proxy.ServeHTTP returns.
func (c *EdgeCache) wrap(w http.ResponseWriter, r *http.Request, host string) *cacheWriter {
	return &cacheWriter{ResponseWriter: w, cache: c, req: r, host: host}
}

func (c *EdgeCache) store(key string, e *cacheEntry) {
	if e.size > c.maxItem {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.curBytes -= old.size
	} else {
		c.order = append(c.order, key)
	}
	c.entries[key] = e
	c.curBytes += e.size
	for c.curBytes > c.maxBytes && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if oldest == key {
			continue // never evict the entry we just stored
		}
		if oe, ok := c.entries[oldest]; ok {
			c.curBytes -= oe.size
			delete(c.entries, oldest)
		}
	}
}

func (c *EdgeCache) removeLocked(key string) {
	if e, ok := c.entries[key]; ok {
		c.curBytes -= e.size
		delete(c.entries, key)
	}
}

// size/bytes accessors for status surfaces + tests.
func (c *EdgeCache) stats() (items int, bytesUsed int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.curBytes
}

// ─── response tee ──────────────────────────────────────────────────

type cacheWriter struct {
	http.ResponseWriter
	cache     *EdgeCache
	req       *http.Request
	host      string
	status    int
	wroteHdr  bool
	cacheable bool
	tooBig    bool
	buf       bytes.Buffer
}

func (cw *cacheWriter) WriteHeader(code int) {
	if cw.wroteHdr {
		return
	}
	cw.wroteHdr = true
	cw.status = code
	cw.cacheable = code == http.StatusOK &&
		cacheableRequest(cw.req) &&
		cacheableResponse(cw.ResponseWriter.Header())
	cw.ResponseWriter.Header().Set("X-Cache", "MISS")
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *cacheWriter) Write(b []byte) (int, error) {
	if !cw.wroteHdr {
		cw.WriteHeader(http.StatusOK)
	}
	if cw.cacheable && !cw.tooBig {
		if int64(cw.buf.Len()+len(b)) > cw.cache.maxItem {
			cw.tooBig = true
			cw.buf.Reset()
		} else {
			cw.buf.Write(b)
		}
	}
	return cw.ResponseWriter.Write(b)
}

// Flush passes through so streaming responses (SSE etc.) aren't broken
// by the tee. Streaming bodies are usually not cacheable anyway.
func (cw *cacheWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *cacheWriter) finalize() {
	if !cw.cacheable || cw.tooBig || cw.buf.Len() == 0 {
		return
	}
	hdr := cw.ResponseWriter.Header()
	ttl := ttlFromCacheControl(hdr.Get("Cache-Control"), cw.cache.ttlCap)
	if ttl <= 0 {
		return
	}
	cw.cache.store(edgeKey(cw.host, cw.req.URL.RequestURI()), &cacheEntry{
		status:  cw.status,
		header:  cloneHeader(hdr),
		body:    append([]byte(nil), cw.buf.Bytes()...),
		etag:    hdr.Get("ETag"),
		expires: time.Now().Add(ttl),
		size:    int64(cw.buf.Len()),
	})
}

// ─── policy helpers ────────────────────────────────────────────────

// cacheableRequest gates which requests may be served from / stored in
// the cache: safe GETs only, never anything carrying caller identity.
func cacheableRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if requestIsProtocolUpgrade(r) {
		return false
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		return false
	}
	return true
}

// cacheableResponse requires an explicit public + max-age (or immutable)
// signal and rejects anything that varies or carries Set-Cookie.
func cacheableResponse(h http.Header) bool {
	if h.Get("Set-Cookie") != "" || h.Get("Vary") != "" {
		return false
	}
	cc := strings.ToLower(h.Get("Cache-Control"))
	if cc == "" || strings.Contains(cc, "no-store") || strings.Contains(cc, "no-cache") || strings.Contains(cc, "private") {
		return false
	}
	if !strings.Contains(cc, "public") {
		return false
	}
	return strings.Contains(cc, "immutable") || maxAgeSeconds(cc) > 0
}

// ttlFromCacheControl returns the cache lifetime, capped. Requires a
// positive max-age; "immutable" without max-age falls back to the cap.
func ttlFromCacheControl(cc string, cap time.Duration) time.Duration {
	cc = strings.ToLower(cc)
	secs := maxAgeSeconds(cc)
	if secs <= 0 {
		if strings.Contains(cc, "immutable") {
			return cap
		}
		return 0
	}
	ttl := time.Duration(secs) * time.Second
	if ttl > cap {
		return cap
	}
	return ttl
}

func maxAgeSeconds(cc string) int {
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "max-age=") {
			if n, err := strconv.Atoi(strings.TrimSpace(part[len("max-age="):])); err == nil {
				return n
			}
		}
	}
	return 0
}

// etagMatches reports whether an If-None-Match header value matches the
// stored ETag. Handles "*", comma lists, and weak (W/) prefixes.
func etagMatches(ifNoneMatch, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}
	norm := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	want := norm(etag)
	for _, tok := range strings.Split(ifNoneMatch, ",") {
		if norm(tok) == want {
			return true
		}
	}
	return false
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	copyHeader(dst, src)
	return dst
}
