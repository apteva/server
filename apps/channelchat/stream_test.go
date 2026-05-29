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
