package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apteva/server/apps/framework"
)

// Smoke-test the channel-chat app end-to-end through HTTP:
//  1. framework loads, manifest lists channel-chat
//  2. default chat is auto-created on instance attach
//  3. POST /messages writes a user row and returns it
//  4. GET  /messages reads it back
//  5. agent-side Send writes an agent row
//  6. SSE stream delivers new messages
func TestChannelChatApp_EndToEnd(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "chat-test@test")
	inst, err := s.store.CreateAgent(user, "inst-chat", "test directive", "autonomous", "{}", "")
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

	// 3. POST user message with a tiny persisted image attachment.
	tinyPNG := "data:image/png;base64,iVBORw0KGgo="
	postBody := `{"content": "hello from the test", "attachments": [{"type":"image","data_url":"` + tinyPNG + `","name":"tiny.png"}]}`
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
	postedAttachments, _ := posted["attachments"].([]any)
	if len(postedAttachments) != 1 {
		t.Fatalf("posted attachments = %v", posted["attachments"])
	}
	if att, _ := postedAttachments[0].(map[string]any); att["data_url"] != tinyPNG || att["mime_type"] != "image/png" {
		t.Fatalf("posted attachment wrong: %v", att)
	}

	// 4. GET messages back. handlers.go writes a system "agent
	//    unreachable" row when ForwardEvent fails — which it does
	//    in this test because no real instance is running. So the
	//    list may contain a system message alongside the user one.
	//    We assert on the user row specifically rather than total
	//    count.
	r = authed("GET", "/apps/channel-chat/messages?chat_id="+chatID, "")
	if r.StatusCode != 200 {
		t.Fatalf("get status %d", r.StatusCode)
	}
	var messages []map[string]any
	json.NewDecoder(r.Body).Decode(&messages)
	r.Body.Close()
	var userRows int
	for _, msg := range messages {
		if msg["role"] == "user" {
			userRows++
		}
	}
	if userRows != 1 {
		t.Fatalf("expected 1 user message, got %d (rows=%v)", userRows, messages)
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
	approvalCh, ok := ch.(framework.ApprovalRequester)
	if !ok {
		t.Fatal("channel-chat should implement ApprovalRequester")
	}
	approval, err := approvalCh.RequestApproval(framework.ApprovalRequest{
		Title: "Deploy change",
		Body:  "Approve deploying the test change.",
	})
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if approval.MessageID == 0 || approval.ChatID != chatID || approval.Status != "pending" {
		t.Fatalf("approval result wrong: %#v", approval)
	}
	reportCh, ok := ch.(framework.ReportSender)
	if !ok {
		t.Fatal("channel-chat should implement ReportSender")
	}
	report, err := reportCh.SendReport(framework.ReportRequest{
		Title:   "Daily progress",
		Summary: "Completed the import and found no blockers.",
		Period:  "today",
		Sections: []framework.ReportSection{
			{Title: "Completed", Body: "Import finished."},
		},
		Tags: []string{"daily", "milestone"},
	})
	if err != nil {
		t.Fatalf("SendReport: %v", err)
	}
	if report.MessageID == 0 || report.ChatID != chatID || report.Status != "sent" {
		t.Fatalf("report result wrong: %#v", report)
	}

	// 6. GET messages now shows the user + agent rows, but not
	//    inbox-only report rows. A system
	//    "agent unreachable" row from step 4 may also be present;
	//    we filter to non-system to assert ordering.
	r = authed("GET", "/apps/channel-chat/messages?chat_id="+chatID, "")
	var allRows []map[string]any
	json.NewDecoder(r.Body).Decode(&allRows)
	r.Body.Close()
	var both []map[string]any
	for _, m := range allRows {
		if m["role"] != "system" {
			both = append(both, m)
		}
	}
	if len(both) < 2 {
		t.Fatalf("expected at least 2 user/agent messages, got %d (rows=%v)", len(both), allRows)
	}
	if both[0]["role"] != "user" || both[1]["role"] != "agent" {
		t.Fatalf("order wrong: %v", both)
	}
	if both[1]["content"] != "agent reply here" {
		t.Fatalf("agent content wrong: %v", both[1])
	}
	for _, row := range allRows {
		if row["content"] == "Report: Daily progress" {
			t.Fatalf("report should not appear in normal chat messages: %#v", allRows)
		}
	}

	r = authed("GET", "/apps/channel-chat/approval-messages?project_id="+inst.ProjectID+"&status=pending", "")
	if r.StatusCode != 200 {
		body, _ := readAll(r)
		t.Fatalf("approval list status %d body=%s", r.StatusCode, body)
	}
	var approvals []map[string]any
	json.NewDecoder(r.Body).Decode(&approvals)
	r.Body.Close()
	if len(approvals) != 1 || approvals[0]["status"] != "pending" || approvals[0]["title"] != "Deploy change" {
		t.Fatalf("approval list wrong: %#v", approvals)
	}

	r = authed("GET", "/apps/channel-chat/report-messages?project_id="+inst.ProjectID, "")
	if r.StatusCode != 200 {
		body, _ := readAll(r)
		t.Fatalf("report list status %d body=%s", r.StatusCode, body)
	}
	var reports []map[string]any
	json.NewDecoder(r.Body).Decode(&reports)
	r.Body.Close()
	if len(reports) != 1 || reports[0]["title"] != "Daily progress" || reports[0]["summary"] != "Completed the import and found no blockers." {
		t.Fatalf("report list wrong: %#v", reports)
	}

	actionBody := `{"message_id":` + itoa64(approval.MessageID) + `,"action_id":"approve"}`
	r = authed("POST", "/apps/channel-chat/message-action", actionBody)
	if r.StatusCode != 200 {
		body, _ := readAll(r)
		t.Fatalf("approval action status %d body=%s", r.StatusCode, body)
	}
	var actionResp map[string]any
	json.NewDecoder(r.Body).Decode(&actionResp)
	r.Body.Close()
	if actionResp["status"] != "approved" {
		t.Fatalf("approval action status wrong: %#v", actionResp)
	}

	r = authed("GET", "/apps/channel-chat/approval-messages?project_id="+inst.ProjectID+"&status=pending", "")
	var pending []map[string]any
	json.NewDecoder(r.Body).Decode(&pending)
	r.Body.Close()
	if len(pending) != 0 {
		t.Fatalf("expected no pending approvals after action, got %#v", pending)
	}
}

func readAll(resp *http.Response) (string, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	resp.Body.Close()
	return buf.String(), err
}

// Covers the complete real-time chain that the dashboard consumes: the
// Streamer publishes a named `stream` event and channel.Send publishes the
// final default SSE message through the same long-lived HTTP response.
func TestChannelChatSSEDeliversStreamingFrameAndFinalMessage(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "chat-sse@test")
	inst, err := s.store.CreateAgent(user, "inst-chat-sse", "test directive", "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	apiKey := "apt_sse_" + itoa64(user)
	if _, err := s.store.CreateAPIKey(user, "sse-test-key", HashAPIKey(apiKey), apiKey[:8]); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	mux := http.NewServeMux()
	reg, err := s.startApps(mux)
	if err != nil {
		t.Fatalf("startApps: %v", err)
	}
	t.Cleanup(func() { reg.Stop(500 * time.Millisecond) })
	app := reg.AppFor("channel-chat")
	ctx := reg.AppCtxFor("channel-chat")
	info := s.buildInstanceInfo(inst.ID)
	if app == nil || ctx == nil || info == nil {
		t.Fatal("channel-chat app context unavailable")
	}
	channel, err := app.Channels()[0].Build(ctx, *info)
	if err != nil {
		t.Fatalf("build channel: %v", err)
	}
	if s.liveTelemetryHook == nil {
		t.Fatal("channel-chat live telemetry hook unavailable")
	}
	mux.HandleFunc("/telemetry/live", s.handleLiveTelemetry)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	chatID := "default-" + itoa64(inst.ID)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/apps/channel-chat/stream?chat_id="+chatID+"&since=0", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("SSE status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("SSE content type=%q", got)
	}

	type record struct{ event, data string }
	records := make(chan record, 8)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		current := record{event: "message"}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				current.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			case line == "" && current.data != "":
				records <- current
				current = record{event: "message"}
			}
		}
	}()

	chunkData, _ := json.Marshal(map[string]any{
		"tool":  "channels_send",
		"id":    "call-sse",
		"chunk": `{"kind":"message","channel":"current","text":"Hello in pro`,
	})
	liveBody, _ := json.Marshal([]TelemetryEvent{{
		ID:       "live-sse-1",
		AgentID:  inst.ID,
		ThreadID: "main",
		Type:     "llm.tool_chunk",
		Time:     time.Now(),
		Data:     chunkData,
	}})
	liveReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/telemetry/live", bytes.NewReader(liveBody))
	liveReq.Header.Set("Content-Type", "application/json")
	liveReq.Header.Set("X-Agent-Secret", s.instanceSecret)
	liveResp, err := http.DefaultClient.Do(liveReq)
	if err != nil {
		t.Fatalf("post live telemetry: %v", err)
	}
	liveResp.Body.Close()
	if liveResp.StatusCode != http.StatusOK {
		t.Fatalf("live telemetry status=%d", liveResp.StatusCode)
	}
	if err := channel.Send("Hello in production"); err != nil {
		t.Fatalf("channel send: %v", err)
	}

	var gotStream, gotMessage bool
	deadline := time.After(2 * time.Second)
	for !gotStream || !gotMessage {
		select {
		case rec := <-records:
			if rec.event == "stream" && strings.Contains(rec.data, `"text":"Hello in pro"`) {
				gotStream = true
			}
			if rec.event == "message" && strings.Contains(rec.data, `"content":"Hello in production"`) {
				gotMessage = true
			}
		case <-deadline:
			t.Fatalf("timed out: stream=%v message=%v", gotStream, gotMessage)
		}
	}
}
