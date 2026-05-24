package main

import (
	"testing"
	"time"
)

func TestBuildOpenAICodexResponsesPayload_ChatMultimodal(t *testing.T) {
	payload := buildOpenAICodexResponsesPayload(map[string]any{
		"model":      "kimi-k2.6",
		"max_tokens": 123,
		"messages": []any{
			map[string]any{"role": "system", "content": "Return JSON."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Describe this."},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
			}},
		},
	})
	if payload["model"] != "gpt-5.5" {
		t.Fatalf("model=%v, want gpt-5.5", payload["model"])
	}
	if payload["instructions"] != "Return JSON." {
		t.Fatalf("instructions=%v", payload["instructions"])
	}
	if _, ok := payload["max_output_tokens"]; ok {
		t.Fatalf("max_output_tokens should be omitted for Codex")
	}
	items, ok := payload["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input=%#v", payload["input"])
	}
	item := items[0].(map[string]any)
	parts := item["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content parts=%#v", parts)
	}
	if parts[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("second part=%#v", parts[1])
	}
}

func TestNormalizeOpenAICodexChatCompletion(t *testing.T) {
	data := map[string]any{
		"id": "resp_1",
		"output": []any{map[string]any{
			"type": "message",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": `{"description":"ok"}`,
			}},
		}},
	}
	out := normalizeOpenAICodexChatCompletion(data, map[string]any{"model": "gpt-5.5"}).(map[string]any)
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != `{"description":"ok"}` {
		t.Fatalf("content=%v", msg["content"])
	}
}

func TestParseOpenAICodexSSE(t *testing.T) {
	raw := []byte("event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\" world\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"usage\":{\"total_tokens\":3}}}\n\n")
	out := parseOpenAICodexSSE(raw)
	if out["output_text"] != "hello world" {
		t.Fatalf("output_text=%v", out["output_text"])
	}
	if out["id"] != "resp_1" {
		t.Fatalf("id=%v", out["id"])
	}
}

func TestConnectionOpenAICodexNeedsRefresh(t *testing.T) {
	soon := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	if !connectionOpenAICodexNeedsRefresh(map[string]string{"token_expires_at": soon}, 10*time.Minute) {
		t.Fatal("expected refresh when token expires inside skew")
	}
	later := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	if connectionOpenAICodexNeedsRefresh(map[string]string{"token_expires_at": later}, 10*time.Minute) {
		t.Fatal("did not expect refresh for later expiry")
	}
}
