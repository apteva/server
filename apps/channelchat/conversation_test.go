package channelchat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apteva/server/apps/framework"
)

type conversationResolver struct {
	agents      map[int64]framework.InstanceInfo
	forwarded   atomic.Int64
	forwardErr  error
	forwardFn   func(call int64) error
	forwards    chan conversationShutdownCall
	spawned     atomic.Int64
	spawnErr    error
	updated     atomic.Int64
	updateErr   error
	toolMu      sync.Mutex
	threadTools []string
	spawnTools  []string
	updateTools []string
	events      chan string
	shutdowns   chan conversationShutdownCall
	kills       chan conversationShutdownCall
	threadIDs   map[int64][]string
	listErr     error
}

type conversationShutdownCall struct {
	AgentID  int64
	ThreadID string
	Message  string
}

func TestChatThreadProfileSupportsWorkProgressChildrenAndSelectiveReporting(t *testing.T) {
	for _, required := range []string{
		"[USER CHAT ROLE]",
		"perform interactive work with your attached tools",
		"keep the user informed at major phases or achievements",
		"what was achieved and what meaningful step comes next",
		"Do not narrate every tool call",
		"exactly one complete final result",
		"observed non-channel tool result is still unfinished",
		"Pace, done, and idle are prohibited",
		"you may create temporary child jobs",
		"exact result it owes you",
		"report that result to its parent before sleeping",
		"Children report to you",
		"REPORT ONLY — no action or reply required",
		"ACTION REQUIRED — reply to this conversation",
		"Do not forward every child event",
		"Never call evolve",
	} {
		if !strings.Contains(chatThreadDirectiveSuffix, required) {
			t.Fatalf("chat thread directive missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"Any acknowledgement is the only progress message",
		"Do not send any additional placeholder or progress message",
	} {
		if strings.Contains(chatThreadDirectiveSuffix, forbidden) {
			t.Fatalf("chat thread directive retained obsolete progress ban %q", forbidden)
		}
	}
	profile := chatThreadProfileFor(framework.InstanceInfo{Kind: "user"})
	if strings.Join(profile.Tools, ",") != "send,spawn,pace" {
		t.Fatalf("ordinary chat tools=%v, want send, spawn, pace", profile.Tools)
	}
}

func TestPlatformHelperConversationUsesControlPlaneToolsDirectly(t *testing.T) {
	inst := framework.InstanceInfo{Kind: "platform_helper"}
	directive := chatThreadDirectiveFor(inst)
	for _, required := range []string{
		"[PLATFORM HELPER CONVERSATION POLICY]",
		"shared acknowledgement and selective-progress guidance",
		"agents_update directly",
		"MUST NOT be handed to main",
		"Do not call core send(id=\"main\")",
		"exactly one complete final channels_send outcome",
		"Temporary children may assist",
		"does not replace that final outcome",
		"never send a second paraphrase",
	} {
		if !strings.Contains(directive, required) {
			t.Fatalf("platform Helper directive missing %q", required)
		}
	}
	if got := chatThreadDirectiveFor(framework.InstanceInfo{Kind: "user"}); got != chatThreadDirectiveSuffix {
		t.Fatal("ordinary agent unexpectedly received the platform Helper policy")
	}

	event := formatPlatformHelperChatEvent("Remove the target schedule.", map[string]any{"project_id": "project-a"})
	for _, required := range []string{
		"PLATFORM HELPER TURN REQUIREMENT",
		"persistent agents_update",
		"does not require a main handoff",
		"acknowledgement and selective-progress guidance applies here too",
		"never repeat the final response",
	} {
		if !strings.Contains(event, required) {
			t.Fatalf("platform Helper event missing %q", required)
		}
	}
}

func TestConversationCollectionCreatesAndListsRoom(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	resolver := &conversationResolver{agents: map[int64]framework.InstanceInfo{
		285: {ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"},
		286: {ID: 286, UserID: 99, Name: "Researcher", ProjectID: "project-a"},
	}}
	for _, inst := range resolver.agents {
		if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
			t.Fatal(err)
		}
	}
	h := &handlers{store: newStore(db), instances: resolver}
	body := []byte(`{"project_id":"project-a","title":"Launch room","agent_ids":[285,286],"lead_agent_id":285}`)
	createRequest := httptest.NewRequest(http.MethodPost, "/conversations", bytes.NewReader(body))
	createResponse := httptest.NewRecorder()
	h.conversations(createResponse, createRequest, nil)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created Chat
	if err := json.NewDecoder(createResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Kind != "room" || len(created.AgentIDs) != 2 {
		t.Fatalf("created=%+v", created)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/conversations?project_id=project-a", nil)
	listResponse := httptest.NewRecorder()
	h.conversations(listResponse, listRequest, nil)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var listed []Chat
	if err := json.NewDecoder(listResponse.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed=%+v", listed)
	}
}

func TestConversationCollectionsNeverExposeInternalDefaultChat(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	inst := framework.InstanceInfo{ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"}
	resolver := &conversationResolver{agents: map[int64]framework.InstanceInfo{inst.ID: inst}}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
		t.Fatal(err)
	}
	st := newStore(db)
	primary, err := st.EnsureDefaultChat(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: st, instances: resolver}

	list := func(path string) []Chat {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		h.conversations(response, request, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("list %q status=%d body=%s", path, response.Code, response.Body.String())
		}
		var chats []Chat
		if err := json.NewDecoder(response.Body).Decode(&chats); err != nil {
			t.Fatal(err)
		}
		return chats
	}

	if chats := list("/conversations?project_id=project-a"); len(chats) != 0 {
		t.Fatalf("untouched primary chat should be hidden, got %+v", chats)
	}
	// Agent detail uses the per-agent collection. It must not turn the internal
	// main-thread inbox record into an undeletable user conversation.
	agentListResponse := httptest.NewRecorder()
	h.listChats(agentListResponse, httptest.NewRequest(http.MethodGet, "/chats?agent_id=285", nil), nil)
	if agentListResponse.Code != http.StatusOK {
		t.Fatalf("agent chat list status=%d body=%s", agentListResponse.Code, agentListResponse.Body.String())
	}
	var agentChats []Chat
	if err := json.NewDecoder(agentListResponse.Body).Decode(&agentChats); err != nil {
		t.Fatal(err)
	}
	if len(agentChats) != 0 {
		t.Fatalf("agent detail exposed internal default chat: %+v", agentChats)
	}
	if chats := list("/conversations?project_id=project-a&include_chat_id=default-285"); len(chats) != 0 {
		t.Fatalf("legacy deep link exposed internal default chat: %+v", chats)
	}

	// Startup/current-status activity belongs on monitoring surfaces and must
	// not make an otherwise untouched main chat look user-created.
	if _, err := db.Exec(`
		INSERT INTO channel_chat_messages (chat_id, role, content, agent_id, components_json)
		VALUES (?, 'agent', 'Status: Waiting', ?, '[{"app":"channel-chat","name":"status-card","props":{}}]')`, primary.ID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if chats := list("/conversations?project_id=project-a"); len(chats) != 0 {
		t.Fatalf("status-only primary chat should be hidden, got %+v", chats)
	}

	userMessage, err := db.Exec(`
		INSERT INTO channel_chat_messages (chat_id, role, content, user_id)
		VALUES (?, 'user', 'Hello', ?)`, primary.ID, inst.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if chats := list("/conversations?project_id=project-a"); len(chats) != 0 {
		t.Fatalf("internal default chat with legacy user history should stay hidden: %+v", chats)
	}
	userMessageID, err := userMessage.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM channel_chat_messages WHERE id=?`, userMessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO channel_chat_messages (chat_id, role, content, agent_id, components_json)
		VALUES (?, 'agent', 'A meaningful update', ?, '[]')`, primary.ID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if chats := list("/conversations?project_id=project-a"); len(chats) != 0 {
		t.Fatalf("main-only agent history must not create a user conversation: %+v", chats)
	}
	if chats := list("/conversations?project_id=project-a&include_chat_id=default-285"); len(chats) != 0 {
		t.Fatalf("deep link exposed main-only routing row: %+v", chats)
	}

	created, err := st.CreateConversation(inst.UserID, inst.ProjectID, "Planner", []int64{inst.ID}, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if chats := list("/conversations?project_id=project-a"); len(chats) != 1 || chats[0].ID != created.ID {
		t.Fatalf("explicit conversation missing from project list: %+v", chats)
	}
	agentListResponse = httptest.NewRecorder()
	h.listChats(agentListResponse, httptest.NewRequest(http.MethodGet, "/chats?agent_id=285", nil), nil)
	if agentListResponse.Code != http.StatusOK {
		t.Fatalf("agent chat list status=%d body=%s", agentListResponse.Code, agentListResponse.Body.String())
	}
	agentChats = nil
	if err := json.NewDecoder(agentListResponse.Body).Decode(&agentChats); err != nil {
		t.Fatal(err)
	}
	if len(agentChats) != 1 || agentChats[0].ID != created.ID {
		t.Fatalf("agent detail did not expose explicit conversation: %+v", agentChats)
	}
}

func TestLegacyCreateChatCreatesDeletableConversation(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	inst := framework.InstanceInfo{ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
		t.Fatal(err)
	}
	h := &handlers{
		store:     newStore(db),
		instances: &conversationResolver{agents: map[int64]framework.InstanceInfo{inst.ID: inst}},
	}
	createResponse := httptest.NewRecorder()
	h.createChat(createResponse, httptest.NewRequest(http.MethodPost, "/chats", bytes.NewBufferString(`{"agent_id":285}`)), nil)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created Chat
	if err := json.NewDecoder(createResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.ID, "conv-") {
		t.Fatalf("created internal chat instead of conversation: %+v", created)
	}
	deleteResponse := httptest.NewRecorder()
	h.conversation(deleteResponse, httptest.NewRequest(http.MethodDelete, "/conversation?id="+created.ID, nil), nil)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	internalResponse := httptest.NewRecorder()
	h.conversation(internalResponse, httptest.NewRequest(http.MethodDelete, "/conversation?id=default-285", nil), nil)
	if internalResponse.Code != http.StatusNotFound {
		t.Fatalf("internal default addressable status=%d body=%s", internalResponse.Code, internalResponse.Body.String())
	}
}

func TestConversationCollectionAllowsGlobalPlatformHelperInProjectConversation(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	resolver := &conversationResolver{agents: map[int64]framework.InstanceInfo{
		900: {ID: 900, UserID: 99, Name: "Apteva Helper", ProjectID: "", Kind: "platform_helper"},
	}}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (900, 99, 'Apteva Helper', '')`); err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: newStore(db), instances: resolver}
	body := []byte(`{"project_id":"project-a","title":"Build checkout flow","agent_ids":[900],"lead_agent_id":900}`)
	request := httptest.NewRequest(http.MethodPost, "/conversations", bytes.NewReader(body))
	response := httptest.NewRecorder()

	h.conversations(response, request, nil)

	if response.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created Chat
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ProjectID != "project-a" || created.AgentID != 900 || created.Kind != "direct" {
		t.Fatalf("created=%+v", created)
	}
}

func TestDeleteConversationNotifiesEveryThreadThenForceCleansIt(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	resolver := &conversationResolver{
		agents: map[int64]framework.InstanceInfo{
			285: {ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"},
			286: {ID: 286, UserID: 99, Name: "Researcher", ProjectID: "project-a"},
		},
		shutdowns: make(chan conversationShutdownCall, 2),
		kills:     make(chan conversationShutdownCall, 2),
	}
	for _, inst := range resolver.agents {
		if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
			t.Fatal(err)
		}
	}
	st := newStore(db)
	chat, err := st.CreateConversation(99, "project-a", "Launch room", []int64{285, 286}, 285)
	if err != nil {
		t.Fatal(err)
	}
	threadID, err := st.EnsureChatThread(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	chat, err = st.GetChat(chat.ID)
	if err != nil {
		t.Fatal(err)
	}

	h := &handlers{
		store: st, instances: resolver,
		presenceStates: map[string]*chatPresenceState{
			chat.ID: {connected: true, timer: time.NewTimer(time.Hour)},
		},
		shutdownGrace: 10 * time.Millisecond,
	}
	req := httptest.NewRequest(http.MethodDelete, "/conversation?id="+chat.ID, nil)
	res := httptest.NewRecorder()
	h.conversation(res, req, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", res.Code, res.Body.String())
	}
	if _, err := st.GetChat(chat.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted conversation lookup err=%v, want ErrNotFound", err)
	}
	if _, exists := h.presenceStates[chat.ID]; exists {
		t.Fatal("deleted conversation retained presence state")
	}

	notified := map[int64]bool{}
	for range 2 {
		call := waitConversationShutdownCall(t, resolver.shutdowns)
		if call.ThreadID != threadID {
			t.Fatalf("shutdown thread=%q want=%q", call.ThreadID, threadID)
		}
		for _, required := range []string{"permanently deleted", "Do not call channels_send", "Call done exactly once", "give main a concise final handoff"} {
			if !strings.Contains(call.Message, required) {
				t.Fatalf("shutdown message missing %q: %s", required, call.Message)
			}
		}
		notified[call.AgentID] = true
	}
	if !notified[285] || !notified[286] {
		t.Fatalf("notified agents=%v", notified)
	}

	killed := map[int64]bool{}
	for range 2 {
		call := waitConversationShutdownCall(t, resolver.kills)
		if call.ThreadID != threadID {
			t.Fatalf("kill thread=%q want=%q", call.ThreadID, threadID)
		}
		killed[call.AgentID] = true
	}
	if !killed[285] || !killed[286] {
		t.Fatalf("force-cleaned agents=%v", killed)
	}
}

func TestCleanupOrphanConversationThreadsKeepsOnlyDatabaseBackedRooms(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	resolver := &conversationResolver{
		agents: map[int64]framework.InstanceInfo{
			285: {ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"},
			286: {ID: 286, UserID: 99, Name: "Researcher", ProjectID: "project-a"},
		},
		threadIDs: map[int64][]string{},
		kills:     make(chan conversationShutdownCall, 4),
	}
	for _, inst := range resolver.agents {
		if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
			t.Fatal(err)
		}
	}
	st := newStore(db)
	chat, err := st.CreateConversation(99, "project-a", "Keep me", []int64{285, 286}, 285)
	if err != nil {
		t.Fatal(err)
	}
	validThreadID, err := st.EnsureChatThread(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolver.threadIDs[285] = []string{"main", validThreadID, "chat-conv-deleted-a", "worker-keep"}
	resolver.threadIDs[286] = []string{validThreadID, "chat-default-286", "chat-conv-deleted-b"}

	h := &handlers{store: st, instances: resolver}
	removed, err := h.cleanupOrphanConversationThreads()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want two deleted-conversation threads", removed)
	}
	killed := map[int64]string{}
	for range 2 {
		call := waitConversationShutdownCall(t, resolver.kills)
		killed[call.AgentID] = call.ThreadID
	}
	if killed[285] != "chat-conv-deleted-a" || killed[286] != "chat-conv-deleted-b" {
		t.Fatalf("killed=%v", killed)
	}
	select {
	case extra := <-resolver.kills:
		t.Fatalf("reconciliation removed valid or non-conversation thread: %+v", extra)
	default:
	}
}

func TestOnMountAutomaticallyReconcilesDeletedConversationThread(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	resolver := &conversationResolver{
		agents: map[int64]framework.InstanceInfo{
			285: {ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"},
		},
		threadIDs: map[int64][]string{285: {"chat-conv-deleted-before-restart"}},
		kills:     make(chan conversationShutdownCall, 1),
	}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Planner', 'project-a')`); err != nil {
		t.Fatal(err)
	}
	app := New(resolver).(*App)
	if err := app.OnMount(&framework.AppCtx{DB: db}); err != nil {
		t.Fatal(err)
	}
	call := waitConversationShutdownCall(t, resolver.kills)
	if call.AgentID != 285 || call.ThreadID != "chat-conv-deleted-before-restart" {
		t.Fatalf("mount cleanup=%+v", call)
	}
}

func TestCleanupUnusedConversationThreadsRemovesPresenceOnlyRuntime(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	inst := framework.InstanceInfo{ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"}
	resolver := &conversationResolver{
		agents: map[int64]framework.InstanceInfo{inst.ID: inst},
		kills:  make(chan conversationShutdownCall, 2),
	}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
		t.Fatal(err)
	}
	st := newStore(db)
	unused, err := st.EnsureDefaultChat(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	unusedThread, err := st.EnsureChatThread(unused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO channel_chat_messages (chat_id, role, content, agent_id, thread_id) VALUES (?, 'agent', 'Main-only report', ?, 'main')`, unused.ID, inst.ID); err != nil {
		t.Fatal(err)
	}
	used, err := st.CreateConversation(inst.UserID, inst.ProjectID, "Used", []int64{inst.ID}, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	usedThread, err := st.EnsureChatThread(used.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO channel_chat_messages (chat_id, role, content, user_id, thread_id) VALUES (?, 'user', 'Hello', ?, ?)`, used.ID, inst.UserID, usedThread); err != nil {
		t.Fatal(err)
	}

	h := &handlers{store: st, instances: resolver}
	removed, err := h.cleanupUnusedConversationThreads()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want one presence-only runtime", removed)
	}
	call := waitConversationShutdownCall(t, resolver.kills)
	if call.AgentID != inst.ID || call.ThreadID != unusedThread {
		t.Fatalf("unused cleanup=%+v", call)
	}
	select {
	case extra := <-resolver.kills:
		t.Fatalf("cleanup killed used conversation: %+v", extra)
	default:
	}
	unused, err = st.GetChat(unused.ID)
	if err != nil {
		t.Fatal(err)
	}
	used, err = st.GetChat(used.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unused.ThreadID != "" {
		t.Fatalf("unused thread id=%q, want cleared", unused.ThreadID)
	}
	if used.ThreadID != usedThread {
		t.Fatalf("used thread id=%q, want %q", used.ThreadID, usedThread)
	}
}

func TestOnMountAutomaticallyRemovesPresenceOnlyRuntime(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	inst := framework.InstanceInfo{ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"}
	resolver := &conversationResolver{
		agents: map[int64]framework.InstanceInfo{inst.ID: inst},
		kills:  make(chan conversationShutdownCall, 1),
	}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
		t.Fatal(err)
	}
	st := newStore(db)
	chat, err := st.EnsureDefaultChat(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	threadID, err := st.EnsureChatThread(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	app := New(resolver).(*App)
	if err := app.OnMount(&framework.AppCtx{DB: db}); err != nil {
		t.Fatal(err)
	}
	call := waitConversationShutdownCall(t, resolver.kills)
	if call.ThreadID != threadID {
		t.Fatalf("mount removed thread=%q, want %q", call.ThreadID, threadID)
	}
	chat, err = st.GetChat(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if chat.ThreadID != "" {
		t.Fatalf("mount left unused thread id=%q", chat.ThreadID)
	}
}

func TestInternalDefaultConversationIsNotAddressableOrKilled(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	inst := framework.InstanceInfo{ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
		t.Fatal(err)
	}
	resolver := &conversationResolver{
		agents:    map[int64]framework.InstanceInfo{inst.ID: inst},
		shutdowns: make(chan conversationShutdownCall, 1),
		kills:     make(chan conversationShutdownCall, 1),
	}
	st := newStore(db)
	chat, err := st.EnsureDefaultChat(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: st, instances: resolver, shutdownGrace: time.Millisecond}
	res := httptest.NewRecorder()
	h.conversation(res, httptest.NewRequest(http.MethodDelete, "/conversation?id="+chat.ID, nil), nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("delete internal default status=%d body=%s", res.Code, res.Body.String())
	}
	time.Sleep(5 * time.Millisecond)
	select {
	case call := <-resolver.shutdowns:
		t.Fatalf("internal default notified thread: %+v", call)
	default:
	}
	select {
	case call := <-resolver.kills:
		t.Fatalf("internal default killed thread: %+v", call)
	default:
	}
}

func waitConversationShutdownCall(t *testing.T, calls <-chan conversationShutdownCall) conversationShutdownCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for conversation shutdown call")
		return conversationShutdownCall{}
	}
}

func (r *conversationResolver) OwnedInstance(userID, agentID int64) (framework.InstanceInfo, error) {
	inst, ok := r.agents[agentID]
	if !ok || inst.UserID != userID {
		return framework.InstanceInfo{}, ErrNotFound
	}
	return inst, nil
}
func (r *conversationResolver) LookupUserID(*http.Request) int64 { return 99 }
func (r *conversationResolver) ForwardEvent(inst framework.InstanceInfo, message any, threadID string) error {
	call := r.forwarded.Add(1)
	text, _ := message.(string)
	if r.forwards != nil {
		r.forwards <- conversationShutdownCall{AgentID: inst.ID, ThreadID: threadID, Message: text}
	}
	if r.events != nil {
		if text != "" {
			r.events <- text
		}
	}
	if r.shutdowns != nil && strings.Contains(text, "[chat.session_closing]") {
		r.shutdowns <- conversationShutdownCall{AgentID: inst.ID, ThreadID: threadID, Message: text}
	}
	if r.forwardFn != nil {
		return r.forwardFn(call)
	}
	return r.forwardErr
}

func TestChatPresenceIgnoresRefreshGapAndDisconnectsAfterTabClose(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "0")
	resolver := &conversationResolver{
		agents: map[int64]framework.InstanceInfo{285: {ID: 285, UserID: 99, Name: "Agent"}},
		events: make(chan string, 4),
	}
	h := &handlers{instances: resolver, presenceGrace: 20 * time.Millisecond}
	inst := resolver.agents[285]
	chatID := defaultChatID(inst.ID)

	h.chatStreamOpened(chatID, inst)
	if got := waitPresenceEvent(t, resolver.events); got != "[chat] user connected to chat" {
		t.Fatalf("first presence event=%q", got)
	}

	h.chatStreamClosed(chatID)
	time.Sleep(5 * time.Millisecond)
	h.chatStreamOpened(chatID, inst)
	time.Sleep(30 * time.Millisecond)
	select {
	case got := <-resolver.events:
		t.Fatalf("refresh emitted presence event %q", got)
	default:
	}

	h.chatStreamClosed(chatID)
	if got := waitPresenceEvent(t, resolver.events); got != "[chat] user disconnected from chat" {
		t.Fatalf("final presence event=%q", got)
	}
}

func TestPassivePresenceDoesNotCreateConversationThread(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "1")
	db := openChannelTestDB(t, true)
	defer db.Close()
	inst := framework.InstanceInfo{ID: 285, UserID: 99, Name: "Agent", ProjectID: "project-a"}
	resolver := &conversationResolver{agents: map[int64]framework.InstanceInfo{inst.ID: inst}}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
		t.Fatal(err)
	}
	st := newStore(db)
	chat, err := st.EnsureDefaultChat(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: st, instances: resolver}

	body := bytes.NewBufferString(`{"chat_id":"default-285","action":"connected"}`)
	res := httptest.NewRecorder()
	h.presence(res, httptest.NewRequest(http.MethodPost, "/presence", body), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("presence status=%d body=%s", res.Code, res.Body.String())
	}
	var presenceResult map[string]string
	if err := json.NewDecoder(res.Body).Decode(&presenceResult); err != nil {
		t.Fatal(err)
	}
	if presenceResult["thread_id"] != "" {
		t.Fatalf("passive presence thread=%q, want none", presenceResult["thread_id"])
	}
	if err := h.forwardPresence(inst, chat.ID, "connected"); err != nil {
		t.Fatal(err)
	}
	if resolver.spawned.Load() != 0 || resolver.forwarded.Load() != 0 {
		t.Fatalf("passive presence spawned=%d forwarded=%d", resolver.spawned.Load(), resolver.forwarded.Load())
	}
	reloaded, err := st.GetChat(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ThreadID != "" {
		t.Fatalf("passive presence persisted thread=%q", reloaded.ThreadID)
	}

	threadID, err := h.forwardConversationEvent(inst, chat.ID, "[chat]\nUser message:\nHello")
	if err != nil {
		t.Fatal(err)
	}
	if threadID != "chat-default-285" || resolver.spawned.Load() != 1 || resolver.forwarded.Load() != 1 {
		t.Fatalf("first user message thread=%q spawned=%d forwarded=%d", threadID, resolver.spawned.Load(), resolver.forwarded.Load())
	}
}

func waitPresenceEvent(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for presence event")
		return ""
	}
}
func (r *conversationResolver) SpawnThread(_ framework.InstanceInfo, _ string, _ string, tools []string, _ []string) error {
	r.spawned.Add(1)
	r.toolMu.Lock()
	r.spawnTools = append([]string(nil), tools...)
	if len(r.threadTools) == 0 {
		r.threadTools = append([]string(nil), tools...)
	}
	r.toolMu.Unlock()
	return r.spawnErr
}
func (r *conversationResolver) ListMCPNames(framework.InstanceInfo) ([]string, error) {
	return []string{"channels"}, nil
}
func (r *conversationResolver) ThreadTools(framework.InstanceInfo, string) ([]string, error) {
	r.toolMu.Lock()
	defer r.toolMu.Unlock()
	return append([]string(nil), r.threadTools...), nil
}
func (r *conversationResolver) UpdateThread(_ framework.InstanceInfo, _ string, _ string, tools []string) error {
	r.updated.Add(1)
	r.toolMu.Lock()
	r.updateTools = append([]string(nil), tools...)
	if len(tools) > 0 {
		r.threadTools = append([]string(nil), tools...)
	}
	r.toolMu.Unlock()
	return r.updateErr
}
func (r *conversationResolver) KillThread(inst framework.InstanceInfo, threadID string) error {
	if r.kills != nil {
		r.kills <- conversationShutdownCall{AgentID: inst.ID, ThreadID: threadID}
	}
	return nil
}

func (r *conversationResolver) ListThreadIDs(inst framework.InstanceInfo) ([]string, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return append([]string(nil), r.threadIDs[inst.ID]...), nil
}
func (r *conversationResolver) MainDirective(framework.InstanceInfo) (string, error) {
	return "# Role\nHelp", nil
}
func (r *conversationResolver) InstanceIDsForUser(int64) ([]int64, error) {
	return []int64{285, 286}, nil
}

type missingThreadTestError struct{}

func (missingThreadTestError) Error() string       { return "thread not found" }
func (missingThreadTestError) ThreadMissing() bool { return true }

func newConversationHarness(t *testing.T, agentID int64, resolver *conversationResolver) (*handlers, Chat, framework.InstanceInfo) {
	t.Helper()
	db := openChannelTestDB(t, true)
	t.Cleanup(func() { _ = db.Close() })
	inst := framework.InstanceInfo{ID: agentID, UserID: 99, Name: "Agent", ProjectID: "project-a"}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
		t.Fatal(err)
	}
	st := newStore(db)
	chat, err := st.CreateConversation(inst.UserID, inst.ProjectID, "Explicit conversation", []int64{agentID}, agentID)
	if err != nil {
		t.Fatal(err)
	}
	resolver.agents = map[int64]framework.InstanceInfo{agentID: inst}
	spawnedChatThreads.Delete(fmt.Sprintf("%d/%s", agentID, chat.ID))
	return &handlers{store: st, instances: resolver}, *chat, inst
}

func TestExplicitConversationAlwaysUsesDedicatedEnsuredThread(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "1")
	resolver := &conversationResolver{forwards: make(chan conversationShutdownCall, 2)}
	h, chat, inst := newConversationHarness(t, 901, resolver)

	if err := h.deliverConversationMessage(inst, chat, "hello", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	first := <-resolver.forwards
	wantThreadID := "chat-" + chat.ID
	if first.ThreadID != wantThreadID {
		t.Fatalf("first delivery thread=%q, want %q", first.ThreadID, wantThreadID)
	}
	if err := h.deliverConversationMessage(inst, chat, "again", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	second := <-resolver.forwards
	if second.ThreadID != first.ThreadID {
		t.Fatalf("second delivery thread=%q, want %q", second.ThreadID, first.ThreadID)
	}
	if got := resolver.spawned.Load(); got != 2 {
		t.Fatalf("idempotent ensure calls=%d, want one before every delivery", got)
	}
	if got := resolver.updated.Load(); got != 1 {
		t.Fatalf("directive updates=%d, want only initial cache population", got)
	}
	resolver.toolMu.Lock()
	spawnTools := append([]string(nil), resolver.spawnTools...)
	resolver.toolMu.Unlock()
	if strings.Join(spawnTools, ",") != "send,spawn,pace" {
		t.Fatalf("spawn tools=%v, want chat leader profile", spawnTools)
	}
}

func TestExistingConversationAddsSpawnWithoutDroppingScopedMCPTools(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "1")
	resolver := &conversationResolver{
		forwards:    make(chan conversationShutdownCall, 1),
		threadTools: []string{"send", "pace", "channels_send", "schedule_lookup"},
	}
	h, chat, inst := newConversationHarness(t, 904, resolver)

	if err := h.deliverConversationMessage(inst, chat, "continue", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	<-resolver.forwards
	resolver.toolMu.Lock()
	updated := append([]string(nil), resolver.updateTools...)
	resolver.toolMu.Unlock()
	for _, required := range []string{"send", "pace", "channels_send", "schedule_lookup", "spawn"} {
		if !slices.Contains(updated, required) {
			t.Fatalf("updated tools=%v, missing %q", updated, required)
		}
	}
}

func TestConversationSpawnFailureNeverFallsBackToMain(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "1")
	resolver := &conversationResolver{spawnErr: errors.New("core unavailable")}
	h, chat, inst := newConversationHarness(t, 902, resolver)

	err := h.deliverConversationMessage(inst, chat, "do not misroute", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "ensure core conversation thread") {
		t.Fatalf("delivery error=%v", err)
	}
	if got := resolver.forwarded.Load(); got != 0 {
		t.Fatalf("forward calls=%d, want zero and no fallback to main", got)
	}
}

func TestConversationMissingThreadIsEnsuredAndRetriedOnce(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "1")
	resolver := &conversationResolver{forwards: make(chan conversationShutdownCall, 2)}
	resolver.forwardFn = func(call int64) error {
		if call == 1 {
			return missingThreadTestError{}
		}
		return nil
	}
	h, chat, inst := newConversationHarness(t, 903, resolver)

	if err := h.deliverConversationMessage(inst, chat, "recover me", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	first := <-resolver.forwards
	second := <-resolver.forwards
	if first.ThreadID != "chat-"+chat.ID || second.ThreadID != first.ThreadID {
		t.Fatalf("retry threads first=%q second=%q", first.ThreadID, second.ThreadID)
	}
	if got := resolver.forwarded.Load(); got != 2 {
		t.Fatalf("forward calls=%d, want exactly two", got)
	}
	if got := resolver.spawned.Load(); got != 2 {
		t.Fatalf("ensure calls=%d, want initial plus one recovery", got)
	}
}

func TestConversationSupportsMultipleAgentsAndScopedReplies(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	for _, row := range []struct {
		id   int64
		name string
	}{{285, "Planner"}, {286, "Researcher"}} {
		if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, 99, ?, 'project-a')`, row.id, row.name); err != nil {
			t.Fatal(err)
		}
	}
	st := newStore(db)
	chat, err := st.CreateConversation(99, "project-a", "Launch room", []int64{285, 286}, 285)
	if err != nil {
		t.Fatal(err)
	}
	if chat.Kind != "room" || chat.AgentID != 285 || len(chat.AgentIDs) != 2 {
		t.Fatalf("chat=%+v", chat)
	}

	threadID, err := st.EnsureChatThread(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := newHub()
	base := &chatChannel{chatID: defaultChatID(286), agentID: 286, userID: 99, store: st, hub: h}
	scoped, ok := base.ForConversationContext(threadID).(*chatChannel)
	if !ok {
		t.Fatal("scoped channel has wrong type")
	}
	if scoped.chatID != chat.ID {
		t.Fatalf("scoped chat=%q want %q", scoped.chatID, chat.ID)
	}
	if forged := base.ForConversationContext("chat-conv-does-not-exist"); forged != nil {
		t.Fatalf("forged conversation context resolved to %#v", forged)
	}
	if err := scoped.Send("Research complete"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListRecentMessages(chat.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AgentID == nil || *rows[0].AgentID != 286 {
		t.Fatalf("messages=%+v", rows)
	}
	report := framework.ChatComponent{App: "channel-chat", Name: "report-card", Props: map[string]any{
		"title": "Research report", "summary": "The secondary agent completed the review.", "period": "today",
	}}
	if _, err := st.AppendAgentArtifact(chat.ID, "Research report", 286, threadID, []framework.ChatComponent{report}); err != nil {
		t.Fatal(err)
	}
	reports, err := st.ListReportMessages([]int64{285, 286}, "project-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].AgentID != 286 || reports[0].AgentName != "Researcher" {
		t.Fatalf("reports=%+v", reports)
	}

	for _, agentID := range []int64{285, 286} {
		chats, err := st.ListChatsForAgent(agentID)
		if err != nil {
			t.Fatal(err)
		}
		if len(chats) != 1 || chats[0].ID != chat.ID {
			t.Fatalf("agent %d chats=%+v", agentID, chats)
		}
	}
}

func TestAgentDetachDeletesSoleAgentConversationAndMessages(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Planner', 'project-a')`); err != nil {
		t.Fatal(err)
	}
	st := newStore(db)
	chat, err := st.EnsureDefaultChat(285)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec(`
		INSERT INTO channel_chat_messages (chat_id, role, content, agent_id)
		VALUES (?, 'agent', 'Direct reply', 285)`, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO channel_chat_deliveries (message_id, agent_id)
		VALUES (?, 285)`, messageID); err != nil {
		t.Fatal(err)
	}

	app := &App{store: st}
	if err := app.OnInstanceDetach(nil, framework.InstanceInfo{ID: 285}); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"channel_chat_chats",
		"channel_chat_participants",
		"channel_chat_messages",
		"channel_chat_deliveries",
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows=%d, want 0", table, count)
		}
	}
}

func TestAgentDetachPreservesSharedConversationAndPromotesLead(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	for _, row := range []struct {
		id   int64
		name string
	}{{285, "Planner"}, {286, "Researcher"}} {
		if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, 99, ?, 'project-a')`, row.id, row.name); err != nil {
			t.Fatal(err)
		}
	}
	st := newStore(db)
	chat, err := st.CreateConversation(99, "project-a", "Launch room", []int64{285, 286}, 285)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO channel_chat_messages (chat_id, role, content, agent_id)
		VALUES (?, 'agent', 'Lead reply', 285), (?, 'agent', 'Research reply', 286)`, chat.ID, chat.ID); err != nil {
		t.Fatal(err)
	}

	app := &App{store: st}
	if err := app.OnInstanceDetach(nil, framework.InstanceInfo{ID: 285}); err != nil {
		t.Fatal(err)
	}

	remaining, err := st.GetChat(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining.AgentID != 286 || remaining.Kind != "direct" {
		t.Fatalf("remaining chat=%+v", remaining)
	}
	if len(remaining.AgentIDs) != 1 || remaining.AgentIDs[0] != 286 {
		t.Fatalf("remaining participants=%v", remaining.AgentIDs)
	}
	var lead int
	if err := db.QueryRow(`
		SELECT is_lead FROM channel_chat_participants
		WHERE chat_id = ? AND agent_id = 286`, chat.ID).Scan(&lead); err != nil {
		t.Fatal(err)
	}
	if lead != 1 {
		t.Fatalf("replacement is_lead=%d, want 1", lead)
	}
	var messageCount, anonymousCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_chat_messages WHERE chat_id = ?`, chat.ID).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_chat_messages WHERE chat_id = ? AND agent_id IS NULL`, chat.ID).Scan(&anonymousCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 2 || anonymousCount != 1 {
		t.Fatalf("messages=%d anonymous=%d, want 2/1", messageCount, anonymousCount)
	}
}

func TestCleanupOrphanedAgentDataRepairsExistingRows(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (286, 99, 'Researcher', 'project-a')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO channel_chat_chats (id, agent_id, title, project_id, owner_user_id, kind)
		VALUES ('orphan-direct', 285, 'Old direct chat', 'project-a', 99, 'direct'),
		       ('orphan-room', 285, 'Old room', 'project-a', 99, 'room');
		INSERT INTO channel_chat_participants (chat_id, agent_id, is_lead)
		VALUES ('orphan-room', 286, 0);
		INSERT INTO channel_chat_messages (chat_id, role, content, agent_id)
		VALUES ('orphan-direct', 'agent', 'Delete me', 285),
		       ('orphan-room', 'agent', 'Keep shared history', 285);`); err != nil {
		t.Fatal(err)
	}

	st := newStore(db)
	if err := st.CleanupOrphanedAgentData(); err != nil {
		t.Fatal(err)
	}

	var directCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_chat_chats WHERE id='orphan-direct'`).Scan(&directCount); err != nil {
		t.Fatal(err)
	}
	if directCount != 0 {
		t.Fatalf("orphan direct chats=%d, want 0", directCount)
	}
	var leadID int64
	var kind string
	if err := db.QueryRow(`SELECT agent_id, kind FROM channel_chat_chats WHERE id='orphan-room'`).Scan(&leadID, &kind); err != nil {
		t.Fatal(err)
	}
	if leadID != 286 || kind != "direct" {
		t.Fatalf("repaired room lead=%d kind=%q", leadID, kind)
	}
	var anonymousCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM channel_chat_messages
		WHERE chat_id='orphan-room' AND agent_id IS NULL`).Scan(&anonymousCount); err != nil {
		t.Fatal(err)
	}
	if anonymousCount != 1 {
		t.Fatalf("anonymous shared messages=%d, want 1", anonymousCount)
	}
}

func TestCleanupLegacyMainConversationDataPreservesArtifactsAndScopesChat(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Planner', 'project-a')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO channel_chat_chats
			(id, agent_id, title, project_id, owner_user_id, kind, thread_id, last_seen_id)
		VALUES ('legacy-main', 285, 'Old main chat', 'project-a', 99, 'direct', 'main', 999);
		INSERT INTO channel_chat_participants (chat_id, agent_id, is_lead)
		VALUES ('legacy-main', 285, 1);
		INSERT INTO channel_chat_messages
			(chat_id, role, content, agent_id, thread_id, components_json)
		VALUES
			('legacy-main', 'agent', 'Legacy ordinary reply', 285, 'main', '[]'),
			('legacy-main', 'agent', 'Report: Keep me', 285, 'main', '[{"app":"channel-chat","name":"report-card","props":{}}]'),
			('legacy-main', 'agent', 'Conversation reply', 285, 'chat-legacy-main', '[]'),
			('legacy-main', 'user', 'Original request', NULL, 'main', '[]');
		INSERT INTO channel_chat_deliveries (message_id, agent_id)
		SELECT id, 285 FROM channel_chat_messages WHERE chat_id='legacy-main';`); err != nil {
		t.Fatal(err)
	}

	st := newStore(db)
	if err := st.CleanupLegacyMainConversationData(); err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupLegacyMainConversationData(); err != nil {
		t.Fatalf("cleanup is not idempotent: %v", err)
	}

	var threadID string
	var lastSeenID int64
	if err := db.QueryRow(`SELECT thread_id, last_seen_id FROM channel_chat_chats WHERE id='legacy-main'`).Scan(&threadID, &lastSeenID); err != nil {
		t.Fatal(err)
	}
	if threadID != "chat-legacy-main" {
		t.Fatalf("thread_id=%q, want chat-legacy-main", threadID)
	}
	var ordinary, artifact, conversationReply, userMessage, deliveries int
	for query, dst := range map[string]*int{
		`SELECT COUNT(*) FROM channel_chat_messages WHERE content='Legacy ordinary reply'`: &ordinary,
		`SELECT COUNT(*) FROM channel_chat_messages WHERE content='Report: Keep me'`:       &artifact,
		`SELECT COUNT(*) FROM channel_chat_messages WHERE content='Conversation reply'`:    &conversationReply,
		`SELECT COUNT(*) FROM channel_chat_messages WHERE content='Original request'`:      &userMessage,
		`SELECT COUNT(*) FROM channel_chat_deliveries`:                                     &deliveries,
	} {
		if err := db.QueryRow(query).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if ordinary != 0 || artifact != 1 || conversationReply != 1 || userMessage != 1 {
		t.Fatalf("ordinary=%d artifact=%d conversation=%d user=%d", ordinary, artifact, conversationReply, userMessage)
	}
	if deliveries != 3 {
		t.Fatalf("deliveries=%d, want 3 after deleting only the legacy ordinary row", deliveries)
	}
	var maxMessageID int64
	if err := db.QueryRow(`SELECT MAX(id) FROM channel_chat_messages WHERE chat_id='legacy-main'`).Scan(&maxMessageID); err != nil {
		t.Fatal(err)
	}
	if lastSeenID != maxMessageID {
		t.Fatalf("last_seen_id=%d, want clamped max=%d", lastSeenID, maxMessageID)
	}
}

func TestConversationTargetSelectionUsesLeadMentionsAndAll(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	resolver := &conversationResolver{agents: map[int64]framework.InstanceInfo{
		285: {ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"},
		286: {ID: 286, UserID: 99, Name: "Researcher", ProjectID: "project-a"},
	}}
	for _, inst := range resolver.agents {
		if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
			t.Fatal(err)
		}
	}
	st := newStore(db)
	chat, err := st.CreateConversation(99, "project-a", "Room", []int64{285, 286}, 285)
	if err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: st, instances: resolver}

	assertTargets := func(text string, explicit []int64, want ...int64) {
		t.Helper()
		got, err := h.resolveConversationTargets(chat, text, explicit)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]int64, 0, len(got))
		for _, inst := range got {
			ids = append(ids, inst.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if len(ids) != len(want) {
			t.Fatalf("targets=%v want=%v", ids, want)
		}
		for i := range ids {
			if ids[i] != want[i] {
				t.Fatalf("targets=%v want=%v", ids, want)
			}
		}
	}
	assertTargets("Please plan this", nil, 285)
	assertTargets("@Researcher investigate this", nil, 286)
	assertTargets("@all review this", nil, 285, 286)
	assertTargets("Please review", []int64{286}, 286)
	if _, err := h.resolveConversationTargets(chat, "", []int64{999}); err == nil {
		t.Fatal("expected non-participant rejection")
	}
}

func TestConversationRetriesSavedFailedDelivery(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "0")
	db := openChannelTestDB(t, true)
	defer db.Close()
	inst := framework.InstanceInfo{ID: 285, UserID: 99, Name: "Planner", ProjectID: "project-a"}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (?, ?, ?, ?)`, inst.ID, inst.UserID, inst.Name, inst.ProjectID); err != nil {
		t.Fatal(err)
	}
	resolver := &conversationResolver{agents: map[int64]framework.InstanceInfo{inst.ID: inst}}
	st := newStore(db)
	room, err := st.CreateConversation(99, "project-a", "Durable work", []int64{inst.ID}, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	message, inserted, err := st.AppendUserMessageWithDeliveries(room.ID, "Continue the import", 99, nil, map[string]any{"context": map[string]any{"route": "/chat"}}, "retry-test", []int64{inst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("first client message was not inserted")
	}
	duplicate, inserted, err := st.AppendUserMessageWithDeliveries(room.ID, "Continue the import", 99, nil, nil, "retry-test", []int64{inst.ID})
	if err != nil || inserted || duplicate.ID != message.ID {
		t.Fatalf("duplicate=%+v inserted=%v err=%v", duplicate, inserted, err)
	}
	if err := st.MarkDelivery(message.ID, inst.ID, false, ErrNotFound); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE channel_chat_deliveries SET updated_at=datetime('now', '-31 seconds') WHERE message_id=?`, message.ID); err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: st, instances: resolver}
	if err := h.retryPendingDeliveries(); err != nil {
		t.Fatal(err)
	}
	if resolver.forwarded.Load() != 1 {
		t.Fatalf("forwarded=%d want=1", resolver.forwarded.Load())
	}
	var status string
	var attempts int
	if err := db.QueryRow(`SELECT status, attempts FROM channel_chat_deliveries WHERE message_id=? AND agent_id=?`, message.ID, inst.ID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || attempts != 2 {
		t.Fatalf("status=%s attempts=%d", status, attempts)
	}
}
