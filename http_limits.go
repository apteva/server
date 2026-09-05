package main

import (
	"net/http"
	"strings"
)

const (
	defaultAPIRequestLimit = int64(16 << 20)
	appDataRequestLimit    = int64(1 << 30)
)

func limitAPIRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			limit := defaultAPIRequestLimit
			path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api"), "/")
			if strings.HasPrefix(path, "apps/") {
				first := strings.TrimPrefix(path, "apps/")
				if i := strings.IndexByte(first, '/'); i >= 0 {
					first = first[:i]
				}
				if first == "callback" || !isAppManagementRoute(first) {
					limit = appDataRequestLimit
				}
			}
			if path == "platform/restore" || strings.HasSuffix(path, "/platform/restore") {
				limit = 64 << 30
			} else if strings.HasPrefix(path, "backups/") {
				limit = appDataRequestLimit
			}
			if r.ContentLength > limit {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}
