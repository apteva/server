package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// Exercises the unset-base path used by app callbacks and the app gateway proxy.
func TestSidecarTransportReusesConnectionsUnderConcurrency(t *testing.T) {
	const concurrent = 32
	var opened atomic.Int64
	release := make(chan struct{})
	var arrived sync.WaitGroup
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Done()
		<-release
		io.WriteString(w, "ok")
	}))
	up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	up.Start()
	defer up.Close()
	client := &http.Client{Transport: &automaticTransport{server: autoTestServer(), target: "app:1", operation: "burst", caller: "test"}}

	burst := func() {
		arrived.Add(concurrent)
		release = make(chan struct{})
		var done sync.WaitGroup
		for i := 0; i < concurrent; i++ {
			done.Add(1)
			go func() {
				defer done.Done()
				resp, err := client.Get(up.URL)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}()
		}
		// Hold every request open so the burst really needs N connections.
		arrived.Wait()
		close(release)
		done.Wait()
	}
	burst()
	first := opened.Load()
	burst()
	if extra := opened.Load() - first; extra != 0 {
		t.Fatalf("second burst opened %d new connections; want 0 (first burst opened %d)", extra, first)
	}
}
