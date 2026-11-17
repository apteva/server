package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// Only fixed upstreams are supported. A model/request cannot choose a URL.
var runtimeImageResponsesURLs = map[string]string{
	"openai":       "https://api.openai.com/v1/responses",
	"openai-codex": "https://chatgpt.com/backend-api/codex/responses",
}

// handleRuntimeImageResponses is an internal Core transport, not a public
// image tool. It preserves the native hosted-tool protocol, replacing every
// image payload with a durable, thread-scoped handle before Core sees it.
func (s *Server) handleRuntimeImageResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if s.instanceSecret == "" || !hmac.Equal([]byte(r.Header.Get("X-Agent-Secret")), []byte(s.instanceSecret)) {
		http.Error(w, "unauthorized", 401)
		return
	}
	agentID, err := strconv.ParseInt(r.Header.Get("X-Apteva-Caller-Agent"), 10, 64)
	if err != nil {
		writeFileProblem(w, fileProblem(403, "file_context_required", "trusted agent required"))
		return
	}
	caller := fileCaller{agentID: agentID, threadID: r.Header.Get(sdk.HeaderFileReferenceThread)}
	if _, _, _, err := s.blobScope(caller); err != nil {
		writeFileProblem(w, err)
		return
	}
	upstreamURL := runtimeImageResponsesURLs[r.Header.Get("X-Apteva-Image-Provider")]
	if upstreamURL == "" {
		http.Error(w, "unsupported image provider", 400)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) == "" {
		http.Error(w, "provider credential required", 401)
		return
	}
	var payload map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF || payload["stream"] != true {
		http.Error(w, "valid streaming Responses request required", 400)
		return
	}
	tools, _ := payload["tools"].([]any)
	hasImageTool := false
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] == "image_generation" {
			hasImageTool = true
		}
	}
	if !hasImageTool {
		http.Error(w, "image_generation tool required", 400)
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "invalid Responses request", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(raw))
	if err != nil {
		http.Error(w, "invalid upstream", 502)
		return
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if account := r.Header.Get("ChatGPT-Account-ID"); account != "" {
		req.Header.Set("ChatGPT-Account-ID", account)
	}
	client := &http.Client{Timeout: 4 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "image provider request failed", 502)
		return
	}
	defer resp.Body.Close()
	for _, name := range []string{"X-Request-ID", "OpenAI-Processing-Ms"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 64<<10))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	err = s.forwardImageResponses(ctx, w, resp.Body, caller)
	if err != nil {
		// Never echo raw provider frames, base64, or credentials in errors.
		event := map[string]any{"type": "error", "error": map[string]any{"code": "image_gateway_failed", "message": err.Error()}}
		encoded, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (s *Server) forwardImageResponses(ctx context.Context, w io.Writer, body io.Reader, caller fileCaller) error {
	scanner := bufio.NewScanner(body)
	// A 25 MiB blob is ~34 MiB of base64 in a single SSE data line.
	scanner.Buffer(make([]byte, 64<<10), 40<<20)
	stored := map[string]sdk.FileHandle{}
	var total int64
	count := 0
	completed := false
	var rewrite func(any) error
	rewrite = func(value any) error {
		switch obj := value.(type) {
		case map[string]any:
			delete(obj, "partial_image_b64")
			if obj["type"] == "image_generation_call" {
				encoded, hasResult := obj["result"].(string)
				delete(obj, "result")
				delete(obj, "file_ref") // Only this gateway can mint the handle.
				if hasResult && encoded != "" {
					id, _ := obj["id"].(string)
					if id == "" {
						return fmt.Errorf("image output has no identity")
					}
					handle, exists := stored[id]
					if !exists {
						if base64.StdEncoding.DecodedLen(len(encoded)) > int(sdk.MaxFileReferenceBytes)+2 {
							return fmt.Errorf("generated image exceeds 25 MiB")
						}
						data, err := base64.StdEncoding.DecodeString(encoded)
						if err != nil {
							return fmt.Errorf("invalid generated image encoding")
						}
						total += int64(len(data))
						count++
						if total > 100<<20 || count > 8 {
							return fmt.Errorf("generated images exceed request limit")
						}
						mime := http.DetectContentType(data)
						ext := ""
						switch mime {
						case "image/png":
							ext = "png"
						case "image/jpeg":
							ext = "jpg"
						case "image/webp":
							ext = "webp"
						default:
							return fmt.Errorf("provider returned an unsupported image format")
						}
						handle, err = s.storeServerBlob(ctx, caller, "generated-image."+ext, mime, data)
						if err != nil {
							return fmt.Errorf("could not store generated image")
						}
						stored[id] = handle
					}
					obj["file_ref"] = handle
				} else if handle, ok := stored[fmt.Sprint(obj["id"])]; ok {
					obj["file_ref"] = handle
				} else if obj["status"] == "completed" {
					return fmt.Errorf("completed image has no result")
				}
			}
			for _, child := range obj {
				if err := rewrite(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range obj {
				if err := rewrite(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("image request canceled")
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var event map[string]any
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&event) != nil {
			return fmt.Errorf("invalid provider stream event")
		}
		if event["type"] == "response.image_generation_call.partial_image" {
			continue
		}
		if err := rewrite(event); err != nil {
			return err
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("invalid provider event")
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return fmt.Errorf("image stream write failed")
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if event["type"] == "response.completed" {
			completed = true
			break
		}
	}
	if scanner.Err() != nil {
		return fmt.Errorf("image stream exceeds limit or was interrupted")
	}
	if !completed {
		return fmt.Errorf("image stream ended without response.completed")
	}
	return nil
}
