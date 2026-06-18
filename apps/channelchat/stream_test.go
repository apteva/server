package channelchat

import (
	"strings"
	"testing"
	"time"
)

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
