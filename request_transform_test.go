package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecuteIntegrationTool_HeaderAndRepeatedMultipartFields(t *testing.T) {
	var modelHeader string
	parts := map[string][]string{}
	filenames := map[string][]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelHeader = r.Header.Get("model")
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("content type=%q params=%v err=%v", mediaType, params, err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			parts[part.FormName()] = append(parts[part.FormName()], string(data))
			filenames[part.FormName()] = append(filenames[part.FormName()], part.FileName())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"_id":"voice-1"}`))
	}))
	defer ts.Close()

	app := &AppTemplate{Slug: "fish-audio", BaseURL: ts.URL, Auth: AppAuthConfig{Types: []string{"bearer"}}}
	tool := &AppToolDef{
		Name: "create_voice_model", Method: http.MethodPost, Path: "/model",
		HeaderParams: map[string]string{"model": "model"},
		MultipartForm: &MultipartFormDef{
			FieldNames:   []string{"title", "texts", "tags"},
			RepeatFields: []string{"texts", "tags"},
			FileFields:   map[string]string{"voices": "voices"},
		},
	}
	res, err := executeIntegrationTool(app, tool, nil, map[string]any{
		"model": "s2.1-pro", "title": "Narrator",
		"voices_filename": "sample.wav",
		"texts":           []any{"first transcript", "second transcript"},
		"tags":            []string{"english", "narration"},
		"voices": []any{
			base64.StdEncoding.EncodeToString([]byte("audio-one")),
			base64.StdEncoding.EncodeToString([]byte("audio-two")),
		},
	}, "")
	if err != nil || res == nil || !res.Success {
		t.Fatalf("execute result=%+v err=%v", res, err)
	}
	if modelHeader != "s2.1-pro" {
		t.Fatalf("model header=%q", modelHeader)
	}
	if _, leaked := parts["model"]; leaked {
		t.Fatalf("header parameter leaked into multipart body: %+v", parts)
	}
	if got := parts["texts"]; len(got) != 2 || got[0] != "first transcript" || got[1] != "second transcript" {
		t.Fatalf("texts=%v", got)
	}
	if got := parts["tags"]; len(got) != 2 || got[0] != "english" || got[1] != "narration" {
		t.Fatalf("tags=%v", got)
	}
	if got := parts["voices"]; len(got) != 2 || got[0] != "audio-one" || got[1] != "audio-two" {
		t.Fatalf("voices=%v", got)
	}
	if got := filenames["voices"]; len(got) != 2 || got[0] != "1-sample.wav" || got[1] != "2-sample.wav" {
		t.Fatalf("voice filenames=%v", got)
	}
}

func TestExecuteIntegrationTool_RequestTransformMimeEmail(t *testing.T) {
	var capturedBody map[string]any
	var capturedContentType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &capturedBody); err != nil {
			t.Fatalf("unmarshal body: %v\n%s", err, string(raw))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sent"}`))
	}))
	defer ts.Close()

	app := &AppTemplate{
		Slug:    "test-mail",
		BaseURL: ts.URL,
		Auth: AppAuthConfig{
			Types:   []string{"bearer"},
			Headers: map[string]string{"Authorization": "Bearer {{token}}"},
		},
	}
	tool := &AppToolDef{
		Name:   "send_email",
		Method: "POST",
		Path:   "/send",
		RequestTransform: &RequestTransformDef{
			Type:     "mime_email",
			Target:   "raw",
			Encoding: "base64url",
			IncludeFields: map[string]string{
				"threadId": "threadId",
			},
		},
	}

	res, err := executeIntegrationTool(app, tool, map[string]string{"access_token": "tok"}, map[string]any{
		"to":         "a@example.com, b@example.com",
		"subject":    "Olá",
		"body":       "Plain text",
		"htmlBody":   "<p>Olá</p>",
		"inReplyTo":  "<original@example.com>",
		"references": "<root@example.com> <original@example.com>",
		"threadId":   "thread-1",
	}, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %#v", res)
	}
	if capturedContentType != "application/json" {
		t.Fatalf("content-type = %q", capturedContentType)
	}
	if capturedBody["threadId"] != "thread-1" {
		t.Fatalf("threadId not copied: %#v", capturedBody)
	}
	raw, _ := capturedBody["raw"].(string)
	mime := decodeBase64URLForTest(t, raw)
	for _, want := range []string{
		"To: a@example.com, b@example.com",
		"Subject: =?UTF-8?B?T2zDoQ==?=",
		"In-Reply-To: <original@example.com>",
		"References: <root@example.com> <original@example.com>",
		"Content-Type: multipart/alternative;",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Type: text/html; charset=UTF-8",
	} {
		if !strings.Contains(mime, want) {
			t.Fatalf("MIME missing %q:\n%s", want, mime)
		}
	}
}

func TestExecuteIntegrationTool_RequestTransformNestedDraft(t *testing.T) {
	var capturedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &capturedBody); err != nil {
			t.Fatalf("unmarshal body: %v\n%s", err, string(raw))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"draft"}`))
	}))
	defer ts.Close()

	app := &AppTemplate{Slug: "test-mail", BaseURL: ts.URL, Auth: AppAuthConfig{Types: []string{"bearer"}}}
	tool := &AppToolDef{
		Name:   "create_draft",
		Method: "POST",
		Path:   "/drafts",
		RequestTransform: &RequestTransformDef{
			Type:     "mime_email",
			Target:   "message.raw",
			Encoding: "base64url",
			IncludeFields: map[string]string{
				"threadId": "message.threadId",
			},
		},
	}

	_, err := executeIntegrationTool(app, tool, nil, map[string]any{
		"to":       "a@example.com",
		"subject":  "Draft",
		"htmlBody": "<p>HTML only</p>",
		"threadId": "thread-2",
	}, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}

	message, ok := capturedBody["message"].(map[string]any)
	if !ok {
		t.Fatalf("message body missing: %#v", capturedBody)
	}
	if message["threadId"] != "thread-2" {
		t.Fatalf("message.threadId not copied: %#v", message)
	}
	mime := decodeBase64URLForTest(t, message["raw"].(string))
	if !strings.Contains(mime, "Subject: Draft") {
		t.Fatalf("MIME subject missing:\n%s", mime)
	}
	if !strings.Contains(mime, "Content-Type: text/html; charset=UTF-8") {
		t.Fatalf("MIME html content-type missing:\n%s", mime)
	}
	if !strings.Contains(mime, base64.StdEncoding.EncodeToString([]byte("<p>HTML only</p>"))) {
		t.Fatalf("MIME html body missing:\n%s", mime)
	}
}

func TestExecuteIntegrationTool_ResponseTransformEmailMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gmailMessageFixture())
	}))
	defer ts.Close()

	app := &AppTemplate{Slug: "test-mail", BaseURL: ts.URL, Auth: AppAuthConfig{Types: []string{"bearer"}}}
	tool := &AppToolDef{
		Name:              "get_message",
		Method:            "GET",
		Path:              "/messages/{messageId}",
		ResponseTransform: &ResponseTransformDef{Type: "email_message"},
	}
	res, err := executeIntegrationTool(app, tool, nil, map[string]any{"messageId": "msg-1"}, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("data shape = %#v", res.Data)
	}
	if data["from"] != "Alice <alice@example.com>" {
		t.Fatalf("from = %#v", data["from"])
	}
	if data["subject"] != "Hello" {
		t.Fatalf("subject = %#v", data["subject"])
	}
	if data["messageId"] != "<msg-1@example.com>" {
		t.Fatalf("messageId = %#v", data["messageId"])
	}
	if data["text"] != "Plain body" {
		t.Fatalf("text = %#v", data["text"])
	}
	if data["html"] != "<p>HTML body</p>" {
		t.Fatalf("html = %#v", data["html"])
	}
	attachments, ok := data["attachments"].([]map[string]any)
	if !ok {
		t.Fatalf("attachments shape = %#v", data["attachments"])
	}
	if len(attachments) != 1 || attachments[0]["filename"] != "brief.pdf" || attachments[0]["attachmentId"] != "att-1" {
		t.Fatalf("attachments = %#v", attachments)
	}
}

func TestExecuteIntegrationTool_ResponseTransformEmailThread(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg1 := gmailMessageFixture()
		msg1["id"] = "msg-1"
		msg1["snippet"] = "First"
		msg2 := gmailMessageFixture()
		msg2["id"] = "msg-2"
		msg2["snippet"] = "Second"
		msg3 := gmailMessageFixture()
		msg3["id"] = "msg-3"
		msg3["snippet"] = "Third"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":        "thread-1",
			"historyId": "h1",
			"messages":  []any{msg1, msg2, msg3},
		})
	}))
	defer ts.Close()

	app := &AppTemplate{Slug: "test-mail", BaseURL: ts.URL, Auth: AppAuthConfig{Types: []string{"bearer"}}}
	tool := &AppToolDef{
		Name:              "get_thread",
		Method:            "GET",
		Path:              "/threads/{threadId}",
		ResponseTransform: &ResponseTransformDef{Type: "email_thread"},
	}
	res, err := executeIntegrationTool(app, tool, nil, map[string]any{"threadId": "thread-1"}, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("data shape = %#v", res.Data)
	}
	if data["messageCount"] != 3 {
		t.Fatalf("messageCount = %#v", data["messageCount"])
	}
	messageIDs, ok := data["messageIds"].([]any)
	if !ok || len(messageIDs) != 3 || messageIDs[0] != "msg-1" || messageIDs[2] != "msg-3" {
		t.Fatalf("messageIds = %#v", data["messageIds"])
	}
	messages, ok := data["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("messages = %#v", data["messages"])
	}
	first, ok := messages[0].(map[string]any)
	if !ok || first["id"] != "msg-1" || first["snippet"] != "First" || first["text"] != nil || first["html"] != nil {
		t.Fatalf("first compact message = %#v", messages[0])
	}
}

func TestBuildRequestTransformBodyJSONAPI(t *testing.T) {
	body, ok, err := buildRequestTransformBody(&RequestTransformDef{
		Type:         "json_api",
		ResourceType: "appStoreVersions",
		Attributes:   []string{"versionString", "platform"},
		Relationships: map[string]JSONAPIRelationship{
			"app":    {Source: "app_id", ResourceType: "apps"},
			"builds": {Source: "build_ids", ResourceType: "builds", Many: true},
		},
	}, map[string]any{
		"app_id":        "app-1",
		"versionString": "2.0",
		"platform":      "IOS",
		"build_ids":     []any{"build-1", "build-2"},
		"ignored":       "not-on-wire",
	})
	if err != nil || !ok {
		t.Fatalf("buildRequestTransformBody: ok=%v err=%v", ok, err)
	}
	encoded, _ := json.Marshal(body)
	want := `{"data":{"attributes":{"platform":"IOS","versionString":"2.0"},"relationships":{"app":{"data":{"id":"app-1","type":"apps"}},"builds":{"data":[{"id":"build-1","type":"builds"},{"id":"build-2","type":"builds"}]}},"type":"appStoreVersions"}}`
	if string(encoded) != want {
		t.Fatalf("json_api body = %s\nwant = %s", encoded, want)
	}
}

func decodeBase64URLForTest(t *testing.T, value string) string {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode base64url: %v", err)
	}
	return string(decoded)
}

func encodeBase64URLForTest(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func gmailMessageFixture() map[string]any {
	return map[string]any{
		"id":           "msg-1",
		"threadId":     "thread-1",
		"labelIds":     []any{"INBOX", "UNREAD"},
		"snippet":      "Plain body",
		"historyId":    "h1",
		"internalDate": "1779444000000",
		"sizeEstimate": 1234,
		"payload": map[string]any{
			"mimeType": "multipart/mixed",
			"headers": []any{
				map[string]any{"name": "From", "value": "Alice <alice@example.com>"},
				map[string]any{"name": "To", "value": "agent@example.com"},
				map[string]any{"name": "Subject", "value": "Hello"},
				map[string]any{"name": "Date", "value": "Wed, 22 May 2026 10:00:00 +0000"},
				map[string]any{"name": "Message-ID", "value": "<msg-1@example.com>"},
			},
			"parts": []any{
				map[string]any{
					"partId":   "0",
					"mimeType": "multipart/alternative",
					"parts": []any{
						map[string]any{
							"partId":   "0.1",
							"mimeType": "text/plain",
							"body": map[string]any{
								"size": 10,
								"data": encodeBase64URLForTest("Plain body"),
							},
						},
						map[string]any{
							"partId":   "0.2",
							"mimeType": "text/html",
							"body": map[string]any{
								"size": 16,
								"data": encodeBase64URLForTest("<p>HTML body</p>"),
							},
						},
					},
				},
				map[string]any{
					"partId":   "2",
					"mimeType": "application/pdf",
					"filename": "brief.pdf",
					"body": map[string]any{
						"size":         12,
						"attachmentId": "att-1",
					},
				},
			},
		},
	}
}
