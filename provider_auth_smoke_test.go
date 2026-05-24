package main

import (
	"strings"
	"testing"
)

func TestParseOpenAICodexSmokeSSE_ComputerCall(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"cu_1","type":"computer_call","call_id":"call_1","actions":[{"type":"screenshot"}]}}`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	resp := parseOpenAICodexSmokeSSE(body)
	if len(resp.ComputerCalls) != 1 {
		t.Fatalf("ComputerCalls len = %d, want 1: %#v", len(resp.ComputerCalls), resp.ComputerCalls)
	}
	if resp.ComputerCalls[0].ID != "call_1" || resp.ComputerCalls[0].Action != "screenshot" {
		t.Fatalf("ComputerCalls[0] = %#v", resp.ComputerCalls[0])
	}
	if resp.InputTokens != 10 || resp.OutputTokens != 2 || resp.CachedTokens != 4 {
		t.Fatalf("usage = in:%d out:%d cached:%d", resp.InputTokens, resp.OutputTokens, resp.CachedTokens)
	}
}

func TestParseOpenAICodexResponseObject_ComputerCall(t *testing.T) {
	resp := parseOpenAICodexResponseObject(map[string]any{
		"output": []any{
			map[string]any{
				"type":    "computer_call",
				"call_id": "call_2",
				"action":  map[string]any{"type": "screenshot"},
			},
		},
	})
	if len(resp.ComputerCalls) != 1 {
		t.Fatalf("ComputerCalls len = %d, want 1: %#v", len(resp.ComputerCalls), resp.ComputerCalls)
	}
	if resp.ComputerCalls[0].ID != "call_2" || resp.ComputerCalls[0].Action != "screenshot" {
		t.Fatalf("ComputerCalls[0] = %#v", resp.ComputerCalls[0])
	}
}

func TestCodexSmokeVisionImageURL(t *testing.T) {
	url, err := codexSmokeVisionImageURL()
	if err != nil {
		t.Fatalf("codexSmokeVisionImageURL: %v", err)
	}
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("unexpected image URL prefix: %.32q", url)
	}
	if len(url) < 100 {
		t.Fatalf("image URL unexpectedly short: %d", len(url))
	}
}
