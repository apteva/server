package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexSSETerminalErrorDetails(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       map[string]any
	}{
		{"nested error", `data: {"type":"error","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later.","param":null,"retry_after_ms":1500,"request_id":"req_event"}}` + "\n\n", map[string]any{"event_type": "error", "code": "server_is_overloaded", "type": "service_unavailable_error", "param": nil, "retry_after_ms": float64(1500), "request_id": "req_event"}},
		{"flat error", "event: error\r\ndata: {\"code\":\"server_is_overloaded\",\"type\":\"service_unavailable_error\",\"message\":\"Overloaded\",\"parameter\":\"input\"}\r\n\r\n", map[string]any{"event_type": "error", "code": "server_is_overloaded", "type": "service_unavailable_error", "parameter": "input", "message": "Overloaded"}},
		{"nested failed", `data: {"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"invalid_api_key","type":"authentication_error","message":"Invalid key","param":"Authorization","headers":{"retry-after":"10","set-cookie":"secret"}}}}` + "\n\n", map[string]any{"event_type": "response.failed", "response_id": "resp_failed", "code": "invalid_api_key", "type": "authentication_error", "param": "Authorization", "retry_headers": map[string]string{"Retry-After": "10"}}},
		{"incomplete", `data: {"type":"response.incomplete","response":{"id":"resp_inc","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}` + "\n\n", map[string]any{"event_type": "response.incomplete", "response_id": "resp_inc", "incomplete_reason": "max_output_tokens"}},
		{"prior IDs", `data: {"type":"response.created","request_id":"req_early","response":{"id":"resp_early"}}` + "\n\n" + `data: {"type":"error","code":"insufficient_quota","message":"Quota exhausted"}` + "\n\n", map[string]any{"code": "insufficient_quota", "request_id": "req_early", "response_id": "resp_early"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := parseOpenAICodexSSE([]byte(tc.body))
			var detail *codexStreamError
			if out != nil || !errors.As(err, &detail) {
				t.Fatalf("missing structured failure: %v %#v", err, out)
			}
			for key, want := range tc.want {
				got, exists := detail.details[key]
				if !exists || !reflect.DeepEqual(got, want) {
					t.Errorf("%s: %#v want %#v", key, got, want)
				}
			}
			if detail.details["message"] == nil || err.Error() != detail.details["message"] {
				t.Fatal("human-readable error lost")
			}
		})
	}
}

func TestCodexStreamingHTTP200ErrorEnvelope(t *testing.T) {
	for _, toolName := range []string{"responses_create", "chat_completion", "vision_describe", "generate_image"} {
		t.Run(toolName, func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload["model"] != "gpt-6.1-sol" {
					t.Errorf("changed requested model: %#v", payload["model"])
				}
				if toolName == "responses_create" && !reflect.DeepEqual(payload["reasoning"], map[string]any{"effort": "low"}) {
					t.Error("changed low reasoning")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-Id", "req_header")
				w.Header().Set("Retry-After", "3")
				w.Header().Set("X-RateLimit-Reset-Requests", "4s")
				w.Header().Set("Set-Cookie", "secret-cookie")
				w.Header().Set("X-Api-Key", "secret-key")
				io.WriteString(w, `event: error`+"\n"+`data: {"type":"error","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later.","param":null}}`+"\n\n")
			}))
			defer provider.Close()
			previous := integrationOpenAICodexResponsesURL
			integrationOpenAICodexResponsesURL = provider.URL
			defer func() { integrationOpenAICodexResponsesURL = previous }()
			signed := "https://example.com/thumbnail.png?signature=secret-image"
			input := map[string]any{"model": "gpt-6.1-sol", "reasoning": map[string]any{"effort": "low"}, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": signed}}}}, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "secret prompt"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": signed}}}}}, "prompt": "secret prompt"}
			result, err := executeIntegrationTool(&AppTemplate{Slug: integrationOpenAICodexSlug}, &AppToolDef{Name: toolName}, map[string]string{"access_token": "secret-token"}, input, "")
			if err != nil || result == nil || result.Success || result.Status != 200 {
				t.Fatalf("HTTP-200 failure became success: %#v %v", result, err)
			}
			data := result.Data.(map[string]any)
			details := data["error_details"].(map[string]any)
			if details["code"] != "server_is_overloaded" || details["type"] != "service_unavailable_error" || details["event_type"] != "error" || details["request_id"] != "req_header" {
				t.Fatalf("lost details: %#v", data)
			}
			if details["retry_headers"].(map[string]string)["Retry-After"] != "3" || result.Headers["X-RateLimit-Reset-Requests"] != "4s" {
				t.Fatalf("lost retry hints: %#v", result)
			}
			timing := data["timing"].(map[string]any)
			duration := timing["request_duration_ms"].(float64)
			terminal := timing["terminal_event_ms"].(float64)
			if terminal < 0 || duration < terminal {
				t.Fatalf("wrong timing: %#v", timing)
			}
			raw, _ := json.Marshal(result)
			for _, secret := range []string{signed, "secret-token", "secret prompt", "secret-cookie", "secret-key"} {
				if strings.Contains(string(raw), secret) {
					t.Errorf("failure exposed sensitive response/request data")
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("adapter introduced retries: %d", calls.Load())
			}
		})
	}
}

func TestCodexSSEStrictMalformedAndIncomplete(t *testing.T) {
	completed := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	for name, body := range map[string]string{
		"null event":                "data: null\n\n",
		"array event":               "data: []\n\n",
		"no type":                   "data: {}\n\n",
		"malformed":                 "data: {bad}\n\n",
		"partial done":              "data: {\"type\":\"response.output_text.done\",\"text\":\"partial\"}\n\ndata: [DONE]\n\n",
		"completed with failure":    "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\"}}}\n\n",
		"completed with error":      "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"error\":{\"code\":\"server_error\"}}}\n\n",
		"conflicting event":         "event: response.completed\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"completed\"}}\n\n",
		"malformed after completed": completed + "data: {bad}\n\n",
		"error after completed":     completed + "data: {\"type\":\"error\",\"message\":\"Failed\"}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := parseOpenAICodexSSE([]byte(body)); err == nil || out != nil {
				t.Fatalf("accepted invalid stream: %#v %v", out, err)
			}
		})
	}
	out, err := parseOpenAICodexSSE([]byte(": keepalive\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"id\":\"resp\",\"status\":\"completed\",\"output\":[]}}\r\n\r\ndata: [DONE]\r\n\r\n"))
	if err != nil || out["id"] != "resp" {
		t.Fatalf("valid multiline stream rejected: %#v %v", out, err)
	}
}

func TestCodexTerminalTimingBeforeEOF(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"id\":\"resp\"}}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
	}))
	defer provider.Close()
	previous := integrationOpenAICodexResponsesURL
	integrationOpenAICodexResponsesURL = provider.URL
	defer func() { integrationOpenAICodexResponsesURL = previous }()
	status, data, _, err := callOpenAICodexResponses(context.Background(), "token", "", map[string]any{"model": "gpt-6.1-sol"}, time.Second)
	if status != 200 || err != nil {
		t.Fatal(status, err)
	}
	timing := data.(map[string]any)["timing"].(map[string]any)
	if timing["request_duration_ms"].(float64)-timing["terminal_event_ms"].(float64) < 25 {
		t.Fatalf("terminal timing waited for EOF: %#v", timing)
	}
}

func TestCodexHTTPErrorStillPreservesStatusAndDetails(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"code":"invalid_api_key","type":"authentication_error","message":"Invalid key","param":null}}`)
	}))
	defer provider.Close()
	previous := integrationOpenAICodexResponsesURL
	integrationOpenAICodexResponsesURL = provider.URL
	defer func() { integrationOpenAICodexResponsesURL = previous }()
	status, data, _, err := callOpenAICodexResponses(context.Background(), "token", "", map[string]any{}, time.Second)
	if status != 401 || err == nil || data.(map[string]any)["error_details"].(map[string]any)["code"] != "invalid_api_key" {
		t.Fatalf("lost HTTP failure: %d %#v %v", status, data, err)
	}
}

func TestCodexResponseOutcomesThroughAdapter(t *testing.T) {
	for _, tc := range []struct {
		name, stream            string
		success                 bool
		eventType, code, reason string
	}{
		{"nested response failed", `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"insufficient_quota","type":"quota_error","message":"Quota exhausted","param":"model"}}}` + "\n\n", false, "response.failed", "insufficient_quota", ""},
		{"incomplete response", `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}` + "\n\n", false, "response.incomplete", "", "content_filter"},
		{"partial text", `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n", false, "stream.missing_completion", "", ""},
		{"malformed JSON", "data: {bad}\n\n", false, "stream.invalid_event", "", ""},
		{"malformed delta", `data: {"type":"response.output_text.delta","delta":42}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n", false, "response.output_text.delta", "", ""},
		{"successful completion", `data: {"type":"response.output_text.delta","delta":"Complete answer"}` + "\n\n" + `data: {"type":"response.completed","response":{"id":"resp_ok","status":"completed","usage":{"total_tokens":3}}}` + "\n\n", true, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.stream)
			}))
			defer upstream.Close()
			previous := integrationOpenAICodexResponsesURL
			integrationOpenAICodexResponsesURL = upstream.URL
			defer func() { integrationOpenAICodexResponsesURL = previous }()
			result, err := executeIntegrationTool(&AppTemplate{Slug: integrationOpenAICodexSlug}, &AppToolDef{Name: "chat_completion"}, map[string]string{"access_token": "token"}, map[string]any{"model": "gpt-6.1-sol", "messages": []any{map[string]any{"role": "user", "content": "tiny text"}}}, "")
			if err != nil || result.Status != 200 || result.Success != tc.success {
				t.Fatalf("wrong outcome: %#v %v", result, err)
			}
			data := result.Data.(map[string]any)
			if data["timing"] == nil {
				t.Fatal("lost normalized timing")
			}
			if tc.success {
				if data["id"] != "resp_ok" || data["object"] != "chat.completion" || data["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "Complete answer" || data["usage"] == nil {
					t.Fatalf("success normalization changed: %#v", data)
				}
			} else {
				if data["choices"] != nil || data["output"] != nil || data["output_text"] != nil {
					t.Fatalf("failed output escaped as content: %#v", data)
				}
				details := data["error_details"].(map[string]any)
				if details["event_type"] != tc.eventType || (tc.code != "" && details["code"] != tc.code) || (tc.reason != "" && details["incomplete_reason"] != tc.reason) {
					t.Fatalf("lost failure details: %#v", details)
				}
			}
		})
	}
}

func TestCodexCompletedButTransportInterrupted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
	}))
	defer upstream.Close()
	previous := integrationOpenAICodexResponsesURL
	integrationOpenAICodexResponsesURL = upstream.URL
	defer func() { integrationOpenAICodexResponsesURL = previous }()
	status, data, _, err := callOpenAICodexResponses(context.Background(), "token", "", map[string]any{}, time.Second)
	if status != 200 || err == nil || data.(map[string]any)["error_details"].(map[string]any)["event_type"] != "stream.read_error" {
		t.Fatalf("interrupted transport accepted: %d %#v %v", status, data, err)
	}
}

func TestCodexSSESizeLimit(t *testing.T) {
	body := strings.Repeat(":padding\n", 1_250_001)
	if out, err := parseOpenAICodexSSE([]byte(body)); out != nil || err == nil || !strings.Contains(err.Error(), "exceeds 10 MB") {
		t.Fatalf("size limit lost: %#v %v", out, err)
	}
}

func TestCodexStreamSniffedWithoutContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
	}))
	defer upstream.Close()
	previous := integrationOpenAICodexResponsesURL
	integrationOpenAICodexResponsesURL = upstream.URL
	defer func() { integrationOpenAICodexResponsesURL = previous }()
	_, data, _, err := callOpenAICodexResponses(context.Background(), "token", "", map[string]any{}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	timing := data.(map[string]any)["timing"].(map[string]any)
	if timing["request_duration_ms"].(float64)-timing["terminal_event_ms"].(float64) < 25 {
		t.Fatalf("sniffed stream was buffered to EOF: %#v", timing)
	}
}
