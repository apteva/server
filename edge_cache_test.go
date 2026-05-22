package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// put simulates an origin response flowing through the tee writer.
func putAsset(c *EdgeCache, host, path, body string, header http.Header) {
	r := httptest.NewRequest("GET", "http://"+host+path, nil)
	cw := c.wrap(httptest.NewRecorder(), r, host)
	for k, vs := range header {
		for _, v := range vs {
			cw.Header().Add(k, v)
		}
	}
	cw.WriteHeader(http.StatusOK)
	_, _ = cw.Write([]byte(body))
	cw.finalize()
}

func TestEdgeCache_StoreServeAnd304(t *testing.T) {
	c := NewEdgeCache()
	putAsset(c, "files.acme.com", "/a.png", "PNGDATA",
		hdr("Cache-Control", "public, max-age=3600", "ETag", `"abc"`, "Content-Type", "image/png"))

	if items, _ := c.stats(); items != 1 {
		t.Fatalf("cached items = %d, want 1", items)
	}

	// Plain GET → HIT with the stored body + headers.
	rec := httptest.NewRecorder()
	if !c.serve(rec, httptest.NewRequest("GET", "http://files.acme.com/a.png", nil), "files.acme.com") {
		t.Fatal("expected cache HIT")
	}
	if rec.Body.String() != "PNGDATA" {
		t.Errorf("body=%q, want PNGDATA", rec.Body.String())
	}
	if rec.Header().Get("X-Cache") != "HIT" || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("headers not replayed: %v", rec.Header())
	}

	// Conditional GET with matching ETag → 304, no body.
	req := httptest.NewRequest("GET", "http://files.acme.com/a.png", nil)
	req.Header.Set("If-None-Match", `"abc"`)
	rec2 := httptest.NewRecorder()
	if !c.serve(rec2, req, "files.acme.com") {
		t.Fatal("expected HIT for conditional request")
	}
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Errorf("conditional → code=%d bodylen=%d, want 304 / 0", rec2.Code, rec2.Body.Len())
	}
}

func TestEdgeCache_DoesNotStoreUncacheable(t *testing.T) {
	c := NewEdgeCache()
	cases := []http.Header{
		hdr("Cache-Control", "private, max-age=60"),
		hdr("Cache-Control", "no-store"),
		hdr("Cache-Control", "public, max-age=0"),
		hdr("Cache-Control", "public, max-age=60", "Set-Cookie", "s=1"),
		hdr("Cache-Control", "public, max-age=60", "Vary", "Accept"),
		hdr(), // no Cache-Control at all
	}
	for i, h := range cases {
		putAsset(c, "x", "/p", "data", h)
		if items, _ := c.stats(); items != 0 {
			t.Errorf("case %d (%v) was cached but shouldn't be", i, h)
		}
	}
}

func TestEdgeCache_SkipsAuthedRequests(t *testing.T) {
	c := NewEdgeCache()
	putAsset(c, "x", "/a", "d", hdr("Cache-Control", "public, max-age=60"))

	authed := httptest.NewRequest("GET", "http://x/a", nil)
	authed.Header.Set("Authorization", "Bearer t")
	if c.serve(httptest.NewRecorder(), authed, "x") {
		t.Error("authed request must not be served from cache")
	}
	cookied := httptest.NewRequest("GET", "http://x/a", nil)
	cookied.Header.Set("Cookie", "sid=1")
	if c.serve(httptest.NewRecorder(), cookied, "x") {
		t.Error("cookie request must not be served from cache")
	}
}

func TestEdgeCache_Expiry(t *testing.T) {
	c := NewEdgeCache()
	putAsset(c, "x", "/a", "d", hdr("Cache-Control", "public, max-age=1"))
	// Force expiry by rewinding the stored entry.
	c.mu.Lock()
	for _, e := range c.entries {
		e.expires = time.Now().Add(-time.Second)
	}
	c.mu.Unlock()
	if c.serve(httptest.NewRecorder(), httptest.NewRequest("GET", "http://x/a", nil), "x") {
		t.Error("expired entry should not be served")
	}
	if items, _ := c.stats(); items != 0 {
		t.Error("expired entry should be purged on lookup")
	}
}

func TestEdgeCache_EvictsOldestOverBudget(t *testing.T) {
	c := NewEdgeCache()
	c.maxBytes, c.maxItem = 10, 10
	h := hdr("Cache-Control", "public, max-age=60")
	putAsset(c, "x", "/a", "aaaa", h) // 4 bytes
	putAsset(c, "x", "/b", "bbbb", h) // 8
	putAsset(c, "x", "/c", "cccc", h) // 12 → over 10, evict oldest (/a)

	if c.serve(httptest.NewRecorder(), httptest.NewRequest("GET", "http://x/a", nil), "x") {
		t.Error("/a should have been evicted")
	}
	if !c.serve(httptest.NewRecorder(), httptest.NewRequest("GET", "http://x/c", nil), "x") {
		t.Error("/c (newest) should be present")
	}
}

func TestEdgeCache_TTLCapAndMaxAge(t *testing.T) {
	if got := ttlFromCacheControl("public, max-age=31536000", 24*time.Hour); got != 24*time.Hour {
		t.Errorf("ttl=%v, want capped at 24h", got)
	}
	if got := ttlFromCacheControl("public, max-age=60", time.Hour); got != 60*time.Second {
		t.Errorf("ttl=%v, want 60s", got)
	}
	if got := ttlFromCacheControl("public, immutable", time.Hour); got != time.Hour {
		t.Errorf("immutable w/o max-age → ttl=%v, want cap", got)
	}
	if got := ttlFromCacheControl("private", time.Hour); got != 0 {
		t.Errorf("private → ttl=%v, want 0", got)
	}
}
