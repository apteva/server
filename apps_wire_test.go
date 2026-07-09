package main

import (
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
