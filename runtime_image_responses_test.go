package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func testImageStream(t *testing.T, imageData []byte) string {
	t.Helper()
	item := map[string]any{"type": "image_generation_call", "id": "ig_test", "status": "completed", "result": base64.StdEncoding.EncodeToString(imageData)}
	var stream strings.Builder
	for _, event := range []any{
		map[string]any{"type": "response.image_generation_call.partial_image", "partial_image_b64": "PREVIEW_BYTES"},
		map[string]any{"type": "response.output_item.done", "item": item},
		map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}},
	} {
		b, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString("data: " + string(b) + "\n\n")
	}
	return stream.String()
}

func TestRuntimeImageGatewayStoresBytesAndForwardsOnlyHandles(t *testing.T) {
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	// Exercise an SSE event larger than Core's 1 MiB stream cap. Only Server
	// should ever see this payload, including its repeated terminal copy.
	raw := append(pngData.Bytes(), bytes.Repeat([]byte{0}, 1200<<10)...)
	var output bytes.Buffer
	if err := s.forwardImageResponses(context.Background(), &output, strings.NewReader(testImageStream(t, raw)), caller); err != nil {
		t.Fatal(err)
	}
	if output.Len() > 3000 || strings.Contains(output.String(), "PREVIEW_BYTES") || strings.Contains(output.String(), `"result"`) || strings.Contains(output.String(), "base64") {
		t.Fatal("image bytes escaped the gateway")
	}
	var count int
	if err := s.store.db.QueryRow("SELECT COUNT(*) FROM server_blobs").Scan(&count); err != nil || count != 1 {
		t.Fatalf("stored blobs=%d, err=%v", count, err)
	}
	var id string
	var data []byte
	if err := s.store.db.QueryRow("SELECT id,data FROM server_blobs").Scan(&id, &data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, raw) || !strings.Contains(output.String(), "blobref://"+id) {
		t.Fatal("gateway changed bytes or lost handle")
	}
	file, err := s.loadFileReference("blobref://" + id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeFileRead(file, caller); err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeFileRead(file, fileCaller{agentID: caller.agentID, threadID: "other"}); err == nil {
		t.Fatal("image escaped thread scope")
	}
}

func TestRuntimeImageGatewayFailsClosed(t *testing.T) {
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	for name, stream := range map[string]string{
		"truncated":        "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
		"invalid_encoding": "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"id\":\"ig_1\",\"result\":\"!INVALID!\"}}\n\n",
		"missing_result":   "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"status\":\"completed\",\"id\":\"ig_1\"}}\n\n",
		"malformed":        "data: {bad json}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := s.forwardImageResponses(context.Background(), &output, strings.NewReader(stream), caller); err == nil {
				t.Fatal("invalid stream accepted")
			}
			if strings.Contains(output.String(), "INVALID") {
				t.Fatal("invalid image payload leaked")
			}
		})
	}
	for _, secret := range []string{"", "wrong"} {
		r := httptest.NewRequest(http.MethodPost, "/runtime-image-responses", strings.NewReader(`{"stream":true,"tools":[{"type":"image_generation"}]}`))
		r.Header.Set("X-Agent-Secret", secret)
		w := httptest.NewRecorder()
		s.handleRuntimeImageResponses(w, r)
		if w.Code != 401 {
			t.Fatalf("invalid secret status=%d", w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/runtime-image-responses", strings.NewReader(`{"stream":true,"tools":[{"type":"image_generation"}]}`))
	r.Header.Set("X-Agent-Secret", s.instanceSecret)
	r.Header.Set("X-Apteva-Caller-Agent", strconv.FormatInt(caller.agentID, 10))
	w := httptest.NewRecorder()
	s.handleRuntimeImageResponses(w, r)
	if w.Code != 403 {
		t.Fatalf("missing thread status=%d", w.Code)
	}
}

func TestNativeImageGenerationFlagSurvivesAgentConfigHydration(t *testing.T) {
	pool := []ProviderInfo{{Type: "openai-codex", ModelLarge: "gpt-6.1-sol", ModelMedium: "gpt-6.1-sol", ModelSmall: "gpt-6.1-sol"}}
	providers := buildAgentCoreProviderConfigs(pool, `{"providers":[{"name":"openai-codex","image_generation":{"enabled":true,"model":"gpt-image-2.5-flare"}}]}`)
	if len(providers) != 1 || providers[0]["image_generation"] == nil {
		t.Fatalf("image flag lost: %v", providers)
	}
	options := providers[0]["image_generation"].(map[string]any)
	if options["enabled"] != true || options["model"] != "gpt-image-2.5-flare" {
		t.Fatalf("wrong options=%v", options)
	}
	providers = buildAgentCoreProviderConfigs(pool, `{}`)
	if providers[0]["image_generation"] != nil {
		t.Fatal("image generation enabled without an operator flag")
	}
}
