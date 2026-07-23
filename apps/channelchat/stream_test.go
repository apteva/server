package channelchat

import (
	"strings"
	"testing"
	"time"

	"github.com/apteva/server/apps/framework"
)

func TestEnsureChatThread_PlatformHelperUsesDedicatedConversation(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "1")
	db := openChannelTestDB(t, true)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (3, 99, 'Helper', 'project-a')`); err != nil {
		t.Fatal(err)
	}
	st := newStore(db)
	if _, err := st.EnsureDefaultChat(3); err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: st, instances: &conversationResolver{agents: map[int64]framework.InstanceInfo{
		3: {ID: 3, UserID: 99, Kind: "platform_helper"},
	}}}
	got, err := h.ensureChatThread(framework.InstanceInfo{ID: 3, UserID: 99, Kind: "platform_helper"}, defaultChatID(3))
	if err != nil {
		t.Fatal(err)
	}
	if got != "chat-default-3" {
		t.Fatalf("thread = %q, want chat-default-3", got)
	}
}

func TestStreamerIngest_MainThreadRoutesToDefaultChat(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "0")
	h := newHub()
	st := newStreamer(h)
	ch, _, cancel := h.subscribeStream(defaultChatID(42))
	defer cancel()

	st.Ingest(
		"llm.tool_chunk",
		42,
		"main",
		`{"tool":"channels_respond","id":"call-1","chunk":"{\"channel\":\"chat\",\"text\":\"Hel"}`,
		time.Now(),
	)

	select {
	case frame := <-ch:
		if frame.ChatID != defaultChatID(42) {
			t.Fatalf("ChatID = %q, want %q", frame.ChatID, defaultChatID(42))
		}
		if frame.ThreadID != "main" {
			t.Fatalf("ThreadID = %q, want main", frame.ThreadID)
		}
		if frame.CallID != "call-1" {
			t.Fatalf("CallID = %q, want call-1", frame.CallID)
		}
		if frame.Text != "Hel" {
			t.Fatalf("Text = %q, want Hel", frame.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream frame")
	}
}

func TestStreamerScopesSameCallIDByAgentAndThread(t *testing.T) {
	h := newHub()
	st := newStreamer(h)
	first, _, cancelFirst := h.subscribeStream(defaultChatID(42))
	defer cancelFirst()
	second, _, cancelSecond := h.subscribeStream(defaultChatID(43))
	defer cancelSecond()

	// Two separate cores may legitimately produce the same provider call ID
	// on their dedicated default-chat threads. Their partial JSON must never
	// share a buffer.
	st.Ingest("llm.tool_chunk", 42, "chat-"+defaultChatID(42), `{"tool":"channels_send","id":"call-1","chunk":"{\"text\":\"Alpha"}`, time.Now())
	st.Ingest("llm.tool_chunk", 43, "chat-"+defaultChatID(43), `{"tool":"channels_send","id":"call-1","chunk":"{\"text\":\"Beta"}`, time.Now())

	select {
	case frame := <-first:
		if frame.ChatID != defaultChatID(42) || frame.Text != "Alpha" {
			t.Fatalf("first frame=%+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first scoped frame")
	}
	select {
	case frame := <-second:
		if frame.ChatID != defaultChatID(43) || frame.Text != "Beta" {
			t.Fatalf("second frame=%+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second scoped frame")
	}
}

func TestStreamerIngest_NonChatWorkerIgnored(t *testing.T) {
	h := newHub()
	st := newStreamer(h)
	ch, _, cancel := h.subscribeStream(defaultChatID(42))
	defer cancel()

	st.Ingest(
		"llm.tool_chunk",
		42,
		"worker-1",
		`{"tool":"channels_respond","id":"call-1","chunk":"{\"channel\":\"chat\",\"text\":\"Hel"}`,
		time.Now(),
	)

	select {
	case frame := <-ch:
		t.Fatalf("unexpected stream frame: %+v", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStreamerIngest_ToolChunkPayloadAliases(t *testing.T) {
	h := newHub()
	st := newStreamer(h)
	chatID := defaultChatID(42)
	ch, _, cancel := h.subscribeStream(chatID)
	defer cancel()

	st.Ingest(
		"llm.tool_chunk",
		42,
		"chat-"+chatID,
		`{"name":"channels_respond","call_id":"call-2","delta":"{\"text\":\"Hi"}`,
		time.Now(),
	)

	select {
	case frame := <-ch:
		if frame.ChatID != chatID {
			t.Fatalf("ChatID = %q, want %q", frame.ChatID, chatID)
		}
		if frame.CallID != "call-2" {
			t.Fatalf("CallID = %q, want call-2", frame.CallID)
		}
		if frame.Text != "Hi" {
			t.Fatalf("Text = %q, want Hi", frame.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream frame")
	}
}

func TestStreamerIngest_ChannelsSendMessageStreams(t *testing.T) {
	h := newHub()
	st := newStreamer(h)
	chatID := defaultChatID(42)
	ch, _, cancel := h.subscribeStream(chatID)
	defer cancel()

	st.Ingest(
		"llm.tool_chunk",
		42,
		"chat-"+chatID,
		`{"name":"channels_send","call_id":"call-send","delta":"{\"channel\":\"current\",\"text\":\"Hi"}`,
		time.Now(),
	)

	select {
	case frame := <-ch:
		if frame.ChatID != chatID {
			t.Fatalf("ChatID = %q, want %q", frame.ChatID, chatID)
		}
		if frame.CallID != "call-send" {
			t.Fatalf("CallID = %q, want call-send", frame.CallID)
		}
		if frame.Text != "Hi" {
			t.Fatalf("Text = %q, want Hi", frame.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream frame")
	}
}

func TestStreamerIngest_ChannelsSendAptevaMessageStreams(t *testing.T) {
	h := newHub()
	st := newStreamer(h)
	chatID := defaultChatID(42)
	ch, _, cancel := h.subscribeStream(chatID)
	defer cancel()

	st.Ingest(
		"tool.call",
		42,
		"chat-"+chatID,
		`{"tool":"channels_send","tool_call_id":"call-apteva","arguments":{"channel":"apteva","text":"Saved for you."}}`,
		time.Now(),
	)

	select {
	case frame := <-ch:
		if frame.ChatID != chatID {
			t.Fatalf("ChatID = %q, want %q", frame.ChatID, chatID)
		}
		if frame.CallID != "call-apteva" {
			t.Fatalf("CallID = %q, want call-apteva", frame.CallID)
		}
		if frame.Text != "Saved for you." {
			t.Fatalf("Text = %q, want Saved for you.", frame.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream frame")
	}
}

func TestStreamerIngest_ChannelsPublishArtifactDoesNotStreamAsChat(t *testing.T) {
	h := newHub()
	st := newStreamer(h)
	chatID := defaultChatID(42)
	ch, _, cancel := h.subscribeStream(chatID)
	defer cancel()

	st.Ingest(
		"tool.call",
		42,
		"chat-"+chatID,
		`{"tool":"channels_publish","tool_call_id":"call-approval","arguments":{"kind":"approval","title":"Approve","content":"Proceed?"}}`,
		time.Now(),
	)

	select {
	case frame := <-ch:
		t.Fatalf("unexpected stream frame: %+v", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStreamerIngest_FinalArgsPayloadAliases(t *testing.T) {
	h := newHub()
	st := newStreamer(h)
	chatID := defaultChatID(42)
	ch, _, cancel := h.subscribeStream(chatID)
	defer cancel()

	st.Ingest(
		"tool.call",
		42,
		"chat-"+chatID,
		`{"tool":"channels_respond","tool_call_id":"call-3","arguments":{"channel":"chat","text":"Final text"}}`,
		time.Now(),
	)

	select {
	case frame := <-ch:
		if frame.ChatID != chatID {
			t.Fatalf("ChatID = %q, want %q", frame.ChatID, chatID)
		}
		if frame.CallID != "call-3" {
			t.Fatalf("CallID = %q, want call-3", frame.CallID)
		}
		if frame.Text != "Final text" {
			t.Fatalf("Text = %q, want Final text", frame.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream frame")
	}
}

func TestStreamerIngest_ToolResultEmitsDone(t *testing.T) {
	h := newHub()
	st := newStreamer(h)
	chatID := defaultChatID(42)
	ch, _, cancel := h.subscribeStream(chatID)
	defer cancel()

	st.Ingest(
		"tool.result",
		42,
		"chat-"+chatID,
		`{"name":"channels_respond","call_id":"call-4"}`,
		time.Now(),
	)

	select {
	case frame := <-ch:
		if frame.ChatID != chatID {
			t.Fatalf("ChatID = %q, want %q", frame.ChatID, chatID)
		}
		if frame.CallID != "call-4" {
			t.Fatalf("CallID = %q, want call-4", frame.CallID)
		}
		if !frame.Done {
			t.Fatalf("Done = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for done frame")
	}
}

func TestFormatDashboardContext(t *testing.T) {
	got := formatDashboardContext(map[string]any{
		"source":       "dashboard-floating",
		"title":        "Media app",
		"route":        "/apps/media/page",
		"project_name": "Launch",
		"project_id":   "proj_123",
		"page_kind":    "app",
		"detail":       "App UI panel with project-scoped context",
		"chips":        []any{"app", "media"},
	})
	for _, want := range []string{
		"Dashboard context:",
		"- page: Media app",
		"- route: /apps/media/page",
		"- project: Launch",
		"- project_id: proj_123",
		"- kind: app",
		"- detail: App UI panel with project-scoped context",
		"- tags: app, media",
		"Project scope rule: This conversation is authoritatively scoped to project_id proj_123.",
		"Pass this exact project_id to every project-aware read or mutation tool.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted context missing %q:\n%s", want, got)
		}
	}
}

func TestFormatDashboardContextAcceptsBuildWorkspace(t *testing.T) {
	got := formatDashboardContext(map[string]any{
		"source":       "dashboard-build",
		"title":        "Build checkout agent",
		"route":        "/build",
		"project_name": "Storefront",
		"project_id":   "project-storefront",
		"page_kind":    "build",
	})
	for _, want := range []string{
		"- page: Build checkout agent",
		"- route: /build",
		"- project: Storefront",
		"- project_id: project-storefront",
		"- kind: build",
		"Project scope rule: This conversation is authoritatively scoped to project_id project-storefront.",
		"Pass this exact project_id to every project-aware read or mutation tool.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted build context missing %q:\n%s", want, got)
		}
	}
}

func TestFormatAgentChatEventIncludesReplyContract(t *testing.T) {
	got := formatAgentChatEvent("What can you do?", map[string]any{
		"source": "dashboard-floating",
		"title":  "Overview",
		"route":  "/",
	})
	for _, want := range []string{
		"[chat]",
		"new dashboard-chat user turn",
		"one optional acknowledgement",
		"selective progress at major phase completions or achievements",
		"what was achieved and the meaningful next step",
		"explicit result-to-parent completion contract",
		"do not narrate tools",
		"Use REPORT ONLY selectively",
		"Use ACTION REQUIRED only",
		"Thoughts and plain assistant output are not visible to the user",
		"Never repeat a message after a successful channels_send receipt",
		"DASHBOARD CHAT COMPLETION REQUIREMENT",
		"optional channels_send acknowledgement; action or read tool; observe its result in a later turn",
		"is only an acknowledgement, never the final outcome",
		"a small number of meaningful progress messages",
		"exactly one final outcome is still required",
		"REPORT ONLY messages do not satisfy the visible reply requirement",
		"User message:",
		"What can you do?",
		"Dashboard context:",
		"- page: Overview",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("chat event missing %q:\n%s", want, got)
		}
	}
	helper := formatPlatformHelperChatEvent("What can you do?", map[string]any{
		"source": "dashboard-floating",
		"title":  "Overview",
		"route":  "/",
	})
	if !strings.HasPrefix(helper, got+"\n\n") {
		t.Fatalf("platform helper lost the shared ordinary chat protocol:\n%s", helper)
	}
	for _, want := range []string{
		"PLATFORM HELPER TURN REQUIREMENT",
		"persistent agents_update",
		"does not require a main handoff",
		"acknowledgement and selective-progress guidance applies here too",
		"never repeat the final response",
	} {
		if !strings.Contains(helper, want) {
			t.Fatalf("platform helper chat event missing %q:\n%s", want, helper)
		}
	}
	for _, forbidden := range []string{
		"do not send a preliminary acknowledgement",
		"perform the tool work first",
		"acknowledgements both before and after",
		"Do not send any additional placeholder or progress message",
		"TASK TYPE: platform_assistant",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("chat event retained conflicting instruction %q:\n%s", forbidden, got)
		}
	}
}

func TestValidateChatAttachmentsKeepsDurableDataURL(t *testing.T) {
	dataURL := "data:image/png;base64,iVBORw0KGgo="
	event, persisted, err := validateChatAttachments([]ChatAttachment{{
		Type:    "image",
		DataURL: dataURL,
		Name:    "tiny.png",
	}})
	if err != nil {
		t.Fatalf("validateChatAttachments: %v", err)
	}
	if len(event) != 1 || len(persisted) != 1 {
		t.Fatalf("event=%d persisted=%d, want 1/1", len(event), len(persisted))
	}
	if persisted[0].DataURL != dataURL {
		t.Fatalf("persisted data_url was stripped")
	}
	if persisted[0].MimeType != "image/png" || persisted[0].Size == 0 {
		t.Fatalf("persisted metadata = %+v", persisted[0])
	}
}

func TestBuildCoreContentPartsIncludesImageURL(t *testing.T) {
	dataURL := "data:image/png;base64,iVBORw0KGgo="
	parts := buildCoreContentParts("hello", []ChatAttachment{{
		Type:    "image",
		DataURL: dataURL,
	}})
	if len(parts) != 2 {
		t.Fatalf("parts=%d, want 2", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "hello" {
		t.Fatalf("text part = %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != dataURL {
		t.Fatalf("image part = %+v", parts[1])
	}
}
