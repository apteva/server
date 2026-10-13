package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestManagedRuntimeBackendRetriesOnlyAdvertisedTransientErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		retry    bool
		expected int
	}{{"busy", 409, true, 2}, {"conflict", 409, false, 1}, {"auth", 403, true, 1}} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t)
			m := s.appRuntimeManager()
			defer m.Close()
			var count atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Idempotency-Key") != "same-operation:stop" {
					t.Error("retry changed operation identity")
				}
				if count.Add(1) == 1 {
					if test.retry {
						w.Header().Set("Retry-After", "1")
					}
					w.WriteHeader(test.status)
					return
				}
				writeJSON(w, runtimeExecution{State: "stopped", StoppedConfirmed: true})
			}))
			defer backend.Close()
			b := remoteRuntimeBackend{manager: m, registration: runtimeBackendRegistration{ID: "future-compute", Endpoint: runtimeControlPrefix + "backends/future-compute"}, base: backend.URL, token: "test"}
			out, err := b.Stop(runtimeBackendRequest{IdempotencyKey: "same-operation"})
			if int(count.Load()) != test.expected {
				t.Fatalf("requests=%d", count.Load())
			}
			if test.expected == 2 && (err != nil || !out.StoppedConfirmed) {
				t.Fatal("advertised retry did not recover")
			}
			if test.expected == 1 && err == nil {
				t.Fatal("permanent error was accepted")
			}
		})
	}
}
