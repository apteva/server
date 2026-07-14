package channelchat

import (
	"strings"
	"testing"
	"time"

	"github.com/apteva/server/apps/framework"
)

func TestResolveChatThread_PlatformHelperUsesMain(t *testing.T) {
	t.Setenv("CHANNELCHAT_PER_THREAD", "1")

	h := &handlers{}
	got := h.resolveChatThread(framework.InstanceInfo{
		ID:   3,
		Kind: "platform_helper",
	}, defaultChatID(3))
	if got != "main" {
		t.Fatalf("thread = %q, want main", got)
	}
}

func TestStreamerIngest_MainThreadRoutesToDefaultChat(t *testing.T) {
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
		"main",
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
		"main",
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
		"main",
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
		"main",
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
		"main",
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
		"main",
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
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted context missing %q:\n%s", want, got)
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
		"Thoughts are not visible to the user",
		"channels_send with channel=\"current\" or channel=\"apteva\" and complete text",
		"wakes you again",
		"schedule yourself with pace",
		"send another channels_send with the outcome text",
		"User message:",
		"What can you do?",
		"Dashboard context:",
		"- page: Overview",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("chat event missing %q:\n%s", want, got)
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
