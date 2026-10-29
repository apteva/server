package main

import (
	"net/http"
	"sync"
)

// sidecarTransport is the shared pool for platform-to-sidecar traffic: app
// callbacks, batch callbacks and the app gateway proxy. http.DefaultTransport
// keeps only two idle connections per host, so concurrent sibling calls closed
// most connections after one use and could exhaust local ports under load.
// No MaxConnsPerHost: proxied /events streams would hold the slots.
var sidecarTransport = sync.OnceValue(func() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	tr := base.Clone()
	tr.MaxIdleConns = 512
	tr.MaxIdleConnsPerHost = 64
	return tr
})
