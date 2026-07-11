package main

import (
	"compress/gzip"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var hashedAssetName = regexp.MustCompile(`-[A-Za-z0-9_]{6,}\.[A-Za-z0-9]+$`)

func compressHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Protocol upgrades (most importantly WebSockets) must retain the
		// original ResponseWriter's http.Hijacker implementation. Wrapping it
		// in gzipResponseWriter makes ReverseProxy return 502 when browsers
		// advertise both gzip and Connection: Upgrade.
		if !requestAcceptsGzip(r) || r.Method == http.MethodHead || requestIsProtocolUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &gzipResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}
		next.ServeHTTP(cw, r)
		cw.finish()
	})
}

func requestIsProtocolUpgrade(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Upgrade")) == "" {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
			return true
		}
	}
	return false
}

type gzipResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	compress    bool
	gz          *gzip.Writer
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	w.ensureHeader(p)
	if w.compress {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *gzipResponseWriter) Flush() {
	w.ensureHeader(nil)
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipResponseWriter) finish() {
	if !w.wroteHeader {
		w.ensureHeader(nil)
	}
	if w.gz != nil {
		_ = w.gz.Close()
	}
}

func (w *gzipResponseWriter) ensureHeader(sample []byte) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	header := w.Header()
	if statusAllowsBody(w.status) && responseIsCompressible(header, sample) {
		header.Del("Content-Length")
		header.Set("Content-Encoding", "gzip")
		addVary(header, "Accept-Encoding")
		w.compress = true
		w.gz = gzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(w.status)
}

func requestAcceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		token := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(token, "gzip") {
			return true
		}
	}
	return false
}

func statusAllowsBody(status int) bool {
	if status >= 100 && status < 200 {
		return false
	}
	return status != http.StatusNoContent && status != http.StatusNotModified
}

func responseIsCompressible(h http.Header, sample []byte) bool {
	if h.Get("Content-Encoding") != "" {
		return false
	}
	ct := strings.ToLower(h.Get("Content-Type"))
	if ct == "" && len(sample) > 0 {
		ct = strings.ToLower(http.DetectContentType(sample))
	}
	if strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	return strings.HasPrefix(ct, "application/json") ||
		strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "xml") ||
		strings.Contains(ct, "svg")
}

func addVary(h http.Header, value string) {
	existing := h.Values("Vary")
	for _, line := range existing {
		for _, part := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(part), value) {
				return
			}
		}
	}
	h.Add("Vary", value)
}

func setStaticCacheHeaders(w http.ResponseWriter, requestPath string, index bool) {
	if w.Header().Get("Cache-Control") != "" {
		return
	}
	if index || requestPath == "" || requestPath == "/" {
		w.Header().Set("Cache-Control", "no-cache")
		return
	}
	name := filepath.Base(requestPath)
	if hashedAssetName.MatchString(name) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(365*24*60*60)+", immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}
