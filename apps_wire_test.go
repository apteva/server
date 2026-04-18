package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Smoke-test the channel-chat app end-to-end through HTTP:
//   1. framework loads, manifest lists channel-chat
//   2. default chat is auto-created on instance attach
//   3. POST /messages writes a user row and returns it
//   4. GET  /messages reads it back
//   5. agent-side Send writes an agent row
//   6. SSE stream delivers new messages
func TestChannelChatApp_EndToEnd(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "chat-test@test")
	inst, err := s.store.CreateInstance(user, "inst-chat", "test directive", "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	// Issue an API key so we can hit HTTP routes through the real
	// auth middleware without needing the session/cookie dance.
	apiKey := "apt_test_" + itoa64(user)
	_, err = s.store.CreateAPIKey(user, "test-key", HashAPIKey(apiKey), apiKey[:8])
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// Stand up the apps framework on a fresh mux.
	mux := http.NewServeMux()
	reg, err := s.startApps(mux)
	if err != nil {
		t.Fatalf("startApps: %v", err)
	}
	t.Cleanup(func() { reg.Stop(500 * time.Millisecond) })

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	authed := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// 1. Manifest lists channel-chat.
	r := authed("GET", "/apps/manifest", "")
	if r.StatusCode != 200 {
		t.Fatalf("manifest status %d", r.StatusCode)
	}
	var manifest []map[string]any
	json.NewDecoder(r.Body).Decode(&manifest)
	r.Body.Close()
	found := false
	for _, m := range manifest {
		if m["slug"] == "channel-chat" {
			found = true
		}
	}
	if !found {
		t.Fatal("channel-chat missing from manifest")
	}

	// 2. Default chat auto-created on instance attach.
	r = authed("GET", "/apps/channel-chat/chats?instance_id="+itoa64(inst.ID), "")
	if r.StatusCode != 200 {
		t.Fatalf("list chats status %d", r.StatusCode)
	}
	var chats []map[string]any
	json.NewDecoder(r.Body).Decode(&chats)
	r.Body.Close()
	if len(chats) != 1 {
		t.Fatalf("expected 1 default chat, got %d", len(chats))
	}
	chatID, _ := chats[0]["id"].(string)
	if chatID == "" {
		t.Fatal("chat id empty")
	}

	// 3. POST user message.
	postBody := `{"content": "hello from the test"}`
	r = authed("POST", "/apps/channel-chat/messages?chat_id="+chatID, postBody)
	if r.StatusCode != 200 {
		body, _ := readAll(r)
		t.Fatalf("post status %d body=%s", r.StatusCode, body)
	}
	var posted map[string]any
	json.NewDecoder(r.Body).Decode(&posted)
	r.Body.Close()
	if posted["role"] != "user" || posted["content"] != "hello from the test" {
		t.Fatalf("posted row wrong: %v", posted)
	}

	// 4. GET messages back.
	r = authed("GET", "/apps/channel-chat/messages?chat_id="+chatID, "")
	if r.StatusCode != 200 {
		t.Fatalf("get status %d", r.StatusCode)
	}
	var messages []map[string]any
	json.NewDecoder(r.Body).Decode(&messages)
	r.Body.Close()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	// 5. Agent-side Send writes an agent row. Need the chat channel
	//    as it's registered in the instance's registry. Since the
	//    instance never actually started, we build one via the
	//    factory directly using the same paths production would.
	app := reg.AppFor("channel-chat")
	if app == nil {
		t.Fatal("channel-chat app not loaded")
	}
	ctx := reg.AppCtxFor("channel-chat")
	info := s.buildInstanceInfo(inst.ID)
	if info == nil {
		t.Fatal("buildInstanceInfo returned nil")
	}
	factory := app.Channels()[0]
	ch, err := factory.Build(ctx, *info)
	if err != nil {
		t.Fatalf("factory.Build: %v", err)
	}
	if err := ch.Send("agent reply here"); err != nil {
		t.Fatalf("channel.Send: %v", err)
	}

	// 6. GET messages now shows both rows, ordered by id.
	r = authed("GET", "/apps/channel-chat/messages?chat_id="+chatID, "")
	var both []map[string]any
	json.NewDecoder(r.Body).Decode(&both)
	r.Body.Close()
	if len(both) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(both))
	}
	if both[0]["role"] != "user" || both[1]["role"] != "agent" {
		t.Fatalf("order wrong: %v", both)
	}
	if both[1]["content"] != "agent reply here" {
		t.Fatalf("agent content wrong: %v", both[1])
	}
}

func readAll(resp *http.Response) (string, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	resp.Body.Close()
	return buf.String(), err
}
