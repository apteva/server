package main

import (
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestRealtimeAudioProxyNegotiatesRendererContractOnlyOnRequest(t *testing.T) {
	const header = "X-Apteva-Audio-Session"
	const contract = `{"version":1,"continuous_conversation":true,"scoped_cancellation":true,"receipt_evidence":true}`
	query := make(chan string, 2)
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query <- r.URL.Query().Get("audio_protocol")
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, http.Header{header: {contract}})
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if conn.WriteMessage(kind, data) != nil {
				return
			}
		}
	}))
	defer core.Close()
	u, _ := url.Parse(core.URL)
	port, _ := strconv.Atoi(u.Port())
	s := newTestServer(t)
	s.agents.mu.Lock()
	s.agents.processes[42] = &runningAgent{port: port, coreAPIKey: "fixture", reattached: true}
	s.agents.mu.Unlock()
	proxy := httptest.NewServer(http.HandlerFunc(s.handleRealtimeAudioProxy))
	defer proxy.Close()
	for _, protocol := range []string{"", "1"} {
		client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+"/?agent_id=42&thread=voice&token=fixture&audio_protocol="+protocol, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := response.Header.Get(header)
		if protocol == "1" && got != contract || protocol == "" && got != "" {
			t.Fatal("contract not preserved", protocol, got)
		}
		if actual := <-query; actual != protocol {
			t.Fatal("protocol query lost", actual)
		}
		client.Close()
	}
}
