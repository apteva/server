package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apteva/server/apps/channelchat"
	"github.com/apteva/server/apps/framework"
)

func TestServerResolverUpdateThreadPreservesScopedMCPToolsWhenToolsOmitted(t *testing.T) {
	var got map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/threads/chat-conv-1" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode update body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	}))
	defer core.Close()

	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	resolver := &serverResolver{}
	if err := resolver.UpdateThread(framework.InstanceInfo{Port: port}, "chat-conv-1", "conversation suffix", nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["tools"]; exists {
		t.Fatalf("directive-only update replaced scoped MCP tool allowlist: %v", got)
	}
	if got["directive_suffix"] != "conversation suffix" {
		t.Fatalf("update body=%v", got)
	}
	if _, exists := got["conversation"]; exists {
		t.Fatalf("update retained obsolete conversation flag: %v", got)
	}
}

func TestServerResolverSpawnThreadSubmitsInitialEventsAtomically(t *testing.T) {
	var got map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/threads/chat-conv-1" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode spawn body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "created",
			"events": map[string]any{"accepted": []string{"chat-message:42:agent:7"}, "duplicates": []string{}},
		})
	}))
	defer core.Close()

	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	eventID := "chat-message:42:agent:7"
	receipt, err := (&serverResolver{}).SpawnThread(
		framework.InstanceInfo{Port: port},
		"chat-conv-1",
		"user-facing conversation",
		[]string{"send", "spawn", "pace"},
		[]string{"channels", "crm"},
		[]channelchat.ThreadEvent{{ID: eventID, Message: "Hi"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "created" || len(receipt.Accepted) != 1 || receipt.Accepted[0] != eventID {
		t.Fatalf("receipt=%+v", receipt)
	}
	if _, exists := got["conversation"]; exists {
		t.Fatalf("spawn retained obsolete conversation flag: %v", got)
	}
	events, ok := got["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("events=%v", got["events"])
	}
	first, _ := events[0].(map[string]any)
	if first["id"] != eventID || first["message"] != "Hi" {
		t.Fatalf("event=%v", first)
	}
}

func TestServerResolverSpawnThreadRejectsMissingEventReceipt(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "created"})
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	_, err = (&serverResolver{}).SpawnThread(
		framework.InstanceInfo{Port: port}, "chat-conv-1", "suffix", nil, nil,
		[]channelchat.ThreadEvent{{ID: "chat-message:1:agent:7", Message: "Hi"}},
	)
	if err == nil || !strings.Contains(err.Error(), "did not acknowledge event") {
		t.Fatalf("error=%v, want missing receipt", err)
	}
}

func TestServerResolverUpdateThreadSendsMergedTools(t *testing.T) {
	var got map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode update body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	tools := []string{"channels_send", "send", "spawn", "pace"}
	if err := (&serverResolver{}).UpdateThread(framework.InstanceInfo{Port: port}, "chat-conv-1", "suffix", tools); err != nil {
		t.Fatal(err)
	}
	raw, ok := got["tools"].([]any)
	if !ok || len(raw) != len(tools) {
		t.Fatalf("update tools=%v, want %v", got["tools"], tools)
	}
}

func TestServerResolverSpawnOpaqueThreadPreservesAppOwnedContract(t *testing.T) {
	var got map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/threads/app-run-42" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer core-secret" {
			t.Fatalf("authorization=%q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode spawn body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "created"})
	}))
	defer core.Close()

	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	status, err := (&serverResolver{}).SpawnOpaqueThread(framework.InstanceInfo{
		ID: 7, Port: port, CoreAPIKey: "core-secret",
	}, "app-run-42", "app-owned instructions", []string{"send", "pace"}, []string{"work-ledger"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if status != "created" {
		t.Fatalf("status=%q", status)
	}
	if got["directive_suffix"] != "app-owned instructions" || got["ephemeral"] != true {
		t.Fatalf("spawn body=%v", got)
	}
	for _, forbidden := range []string{"role", "kind", "conversation", "worker", "main"} {
		if _, exists := got[forbidden]; exists {
			t.Fatalf("platform classified opaque thread with %q: %v", forbidden, got)
		}
	}
}

func TestServerResolverThreadToolsReadsEffectiveAllowlist(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "main"},
			{"id": "chat-conv-1", "tools": []string{"channels_send", "send", "pace"}},
		})
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := (&serverResolver{}).ThreadTools(framework.InstanceInfo{Port: port}, "chat-conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tools, ",") != "channels_send,send,pace" {
		t.Fatalf("thread tools=%v", tools)
	}
}

func TestServerResolverListsLiveCoreThreads(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/threads" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "main"},
			{"id": "chat-conv-live"},
		})
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := (&serverResolver{}).ListThreadIDs(framework.InstanceInfo{Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "main,chat-conv-live" {
		t.Fatalf("live thread ids=%v", ids)
	}
}

func TestServerResolverReconcilesDetachedCoreBeforeManagerReattach(t *testing.T) {
	var deleted string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer persisted-core-key" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/threads":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "main"}, {"id": "chat-conv-orphan"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/threads/chat-conv-orphan":
			deleted = "chat-conv-orphan"
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "killed"})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	registerAndLogin(t, s)
	agent, err := s.store.CreateAgent(1, "detached-core", "directive", "autonomous", `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	agent.Status = "running"
	agent.Port = port
	agent.CoreAPIKey = "persisted-core-key"
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatal(err)
	}
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["threads"] = []any{map[string]any{"id": "chat-conv-orphan"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	resolver := &serverResolver{srv: s}
	bootInfo := framework.InstanceInfo{ID: agent.ID, UserID: agent.UserID} // manager has no port yet
	ids, err := resolver.ListThreadIDs(bootInfo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "chat-conv-orphan" {
		t.Fatalf("detached live thread ids=%v", ids)
	}
	if err := resolver.KillThread(bootInfo, "chat-conv-orphan"); err != nil {
		t.Fatal(err)
	}
	if deleted != "chat-conv-orphan" {
		t.Fatalf("detached Core delete=%q", deleted)
	}
}

func TestServerResolverChatAgentIDsIncludePlatformHelper(t *testing.T) {
	store := newTestStore(t)
	user, err := store.CreateUser("helper-chat-summary@test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	regular, err := store.CreateAgent(user.ID, "Regular", "directive", "autonomous", "{}", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := store.GetOrCreatePlatformHelper(user.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}

	resolver := &serverResolver{srv: &Server{store: store}}
	ids, err := resolver.InstanceIDsForUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[int64]bool, len(ids))
	for _, id := range ids {
		got[id] = true
	}
	if !got[regular.ID] || !got[helper.ID] || len(got) != 2 {
		t.Fatalf("chat agent ids=%v, want regular=%d and helper=%d", ids, regular.ID, helper.ID)
	}
}

// Smoke-test the channel-chat app end-to-end through HTTP:
//  1. framework loads, manifest lists channel-chat
//  2. no internal default row is exposed; an explicit conv-* chat is created
//  3. POST /messages writes a user row and returns it
//  4. GET  /messages reads it back
//  5. a thread-scoped agent Send writes back to that same conversation
//  6. main-channel approvals/reports remain available through the Inbox
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
	// Pre-seed the synthetic inventory row written by older servers. Startup
	// must remove it now that channel-chat is marked as internal.
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json)
		 VALUES ('channel-chat', 'builtin', '', '', '{}')`,
	); err != nil {
		t.Fatalf("seed legacy channel-chat app: %v", err)
	}
	if _, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, source)
		 SELECT id, '', 'running', 'builtin' FROM apps WHERE name='channel-chat'`,
	); err != nil {
		t.Fatalf("seed legacy channel-chat install: %v", err)
	}
	mux := http.NewServeMux()
	reg, err := s.startApps(mux)
	if err != nil {
		t.Fatalf("startApps: %v", err)
	}
	t.Cleanup(func() { reg.Stop(500 * time.Millisecond) })
	var channelChatInstalls int
	if err := s.store.db.QueryRow(
		`SELECT COUNT(*) FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE a.name='channel-chat'`,
	).Scan(&channelChatInstalls); err != nil {
		t.Fatalf("count channel-chat installs: %v", err)
	}
	if channelChatInstalls != 0 {
		t.Fatalf("channel-chat inventory rows=%d, want 0", channelChatInstalls)
	}

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

	// 2. Internal default-* storage is not a user chat. Creating a chat is an
	// explicit action and returns a normal deletable conv-* conversation.
	r = authed("GET", "/apps/channel-chat/chats?instance_id="+itoa64(inst.ID), "")
	if r.StatusCode != 200 {
		t.Fatalf("list chats status %d", r.StatusCode)
	}
	var chats []map[string]any
	json.NewDecoder(r.Body).Decode(&chats)
	r.Body.Close()
	if len(chats) != 0 {
		t.Fatalf("expected no implicit conversations, got %d: %#v", len(chats), chats)
	}
	r = authed("POST", "/apps/channel-chat/chats", `{"agent_id":`+itoa64(inst.ID)+`}`)
	if r.StatusCode != http.StatusOK {
		body, _ := readAll(r)
		t.Fatalf("create chat status=%d body=%s", r.StatusCode, body)
	}
	var createdChat map[string]any
	json.NewDecoder(r.Body).Decode(&createdChat)
	r.Body.Close()
	chatID, _ := createdChat["id"].(string)
	if !strings.HasPrefix(chatID, "conv-") {
		t.Fatalf("explicit chat id=%q, want conv-*", chatID)
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
	threadID := "chat-" + chatID
	deadline := time.Now().Add(time.Second)
	for {
		var persistedThreadID string
		_ = s.store.db.QueryRow(`SELECT thread_id FROM channel_chat_chats WHERE id=?`, chatID).Scan(&persistedThreadID)
		if persistedThreadID == threadID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation thread was not persisted: got %q want %q", persistedThreadID, threadID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	scoper, ok := ch.(framework.ConversationScopedChannel)
	if !ok {
		t.Fatal("channel-chat should implement ConversationScopedChannel")
	}
	conversationChannel := scoper.ForConversationContext(threadID)
	if conversationChannel == nil {
		t.Fatalf("conversation channel did not resolve thread %q", threadID)
	}
	if err := conversationChannel.Send("agent reply here"); err != nil {
		t.Fatalf("channel.Send: %v", err)
	}

	// 6. Main remains the durable autonomous/control-plane context. Its
	// structured artifacts use the internal inbox sink, not the conversation.
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
	internalChatID := "default-" + itoa64(inst.ID)
	if approval.MessageID == 0 || approval.ChatID != internalChatID || approval.Status != "pending" {
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
	if report.MessageID == 0 || report.ChatID != internalChatID || report.Status != "sent" {
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
	createReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/apps/channel-chat/chats", strings.NewReader(`{"agent_id":`+itoa64(inst.ID)+`}`))
	createReq.Header.Set("Authorization", "Bearer "+apiKey)
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if createResp.StatusCode != http.StatusOK {
		body, _ := readAll(createResp)
		t.Fatalf("create conversation status=%d body=%s", createResp.StatusCode, body)
	}
	var createdChat map[string]any
	if err := json.NewDecoder(createResp.Body).Decode(&createdChat); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	createResp.Body.Close()
	chatID, _ := createdChat["id"].(string)
	if !strings.HasPrefix(chatID, "conv-") {
		t.Fatalf("created chat id=%q, want conv-*", chatID)
	}
	threadID := "chat-" + chatID
	if _, err := s.store.db.Exec(`UPDATE channel_chat_chats SET thread_id=? WHERE id=?`, threadID, chatID); err != nil {
		t.Fatalf("persist conversation thread: %v", err)
	}
	scoper, ok := channel.(framework.ConversationScopedChannel)
	if !ok {
		t.Fatal("channel-chat should implement ConversationScopedChannel")
	}
	conversationChannel := scoper.ForConversationContext(threadID)
	if conversationChannel == nil {
		t.Fatalf("conversation channel did not resolve %q", threadID)
	}
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
		ThreadID: threadID,
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
	if err := conversationChannel.Send("Hello in production"); err != nil {
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
