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

func TestBuildOpenAICodexImagePayload(t *testing.T) {
	payload := buildOpenAICodexImagePayload(map[string]any{
		"prompt":             "draw a red door",
		"model":              "gpt-image-2",
		"size":               "1024x1536",
		"quality":            "high",
		"output_format":      "webp",
		"background":         "transparent",
		"output_compression": 80,
	})
	if payload["model"] != "gpt-5.5" {
		t.Fatalf("model=%v, want gpt-5.5", payload["model"])
	}
	if payload["instructions"] == "" {
		t.Fatalf("instructions missing: %#v", payload)
	}
	items, ok := payload["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input=%#v, want one Responses input item", payload["input"])
	}
	item := items[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("input item=%#v", item)
	}
	parts := item["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["type"] != "input_text" || parts[0].(map[string]any)["text"] != "draw a red door" {
		t.Fatalf("content=%#v", parts)
	}
	if payload["stream"] != true {
		t.Fatalf("stream=%v, want true", payload["stream"])
	}
	tools := payload["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "image_generation" || tool["action"] != "generate" {
		t.Fatalf("tool=%#v", tool)
	}
	if tool["size"] != "1024x1536" || tool["quality"] != "high" || tool["output_format"] != "webp" {
		t.Fatalf("tool options=%#v", tool)
	}
}

func TestNormalizeOpenAICodexImageGeneration(t *testing.T) {
	data := map[string]any{
		"id":    "resp_img",
		"model": "gpt-5.5",
		"output": []any{map[string]any{
			"id":             "ig_1",
			"type":           "image_generation_call",
			"status":         "completed",
			"revised_prompt": "A red door on a stone wall.",
			"result":         "iVBORw0KGgo=",
		}},
	}
	out := normalizeOpenAICodexImageGeneration(data, map[string]any{"model": "gpt-image-2"}).(map[string]any)
	if out["id"] != "resp_img" || out["model"] != "gpt-5.5" {
		t.Fatalf("out meta=%#v", out)
	}
	images := out["data"].([]any)
	if len(images) != 1 {
		t.Fatalf("images=%#v", images)
	}
	img := images[0].(map[string]any)
	if img["b64_json"] != "iVBORw0KGgo=" || img["revised_prompt"] != "A red door on a stone wall." {
		t.Fatalf("image=%#v", img)
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
