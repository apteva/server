package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func TestExecuteIntegrationToolBodyNoneUploadProbe(t *testing.T) {
	for _, mode := range []string{"default", "form", "raw", "binary", "root", "transform", "multipart"} {
		t.Run(mode, func(t *testing.T) {
			var tool AppToolDef
			if err := json.Unmarshal([]byte(`{
				"name":"probe", "method":"PUT",
				"path":"/upload/session?upload_id=existing-session",
				"body_none":true,
				"headers":{"Content-Length":"0","X-Probe":"status"},
				"header_params":{"content_range":"Content-Range"}
			}`), &tool); err != nil {
				t.Fatal(err)
			}
			if !tool.BodyNone {
				t.Fatal("catalog body_none was not decoded")
			}
			app := &AppTemplate{Slug: "upload-probe-test"}
			app.Auth.Headers = map[string]string{"Authorization": "Bearer {{access_token}}"}
			app.Auth.BodyParams = map[string]string{"body_secret": "{{access_token}}"}
			input := map[string]any{"content_range": "bytes */2097152", "payload": "must not be sent"}
			switch mode {
			case "form":
				tool.Headers["Content-Type"] = "application/x-www-form-urlencoded"
			case "raw":
				tool.BodyInput = "payload"
			case "binary":
				tool.BodyBinaryParam = "file"
				tool.InputSchema = map[string]any{"required": []any{"file"}}
			case "root":
				tool.BodyRoot = "payload"
			case "transform":
				// This would fail if an unused body transform were evaluated.
				tool.RequestTransform = &RequestTransformDef{Type: "base64_field", Source: "missing", Target: "data"}
			case "multipart":
				tool.MultipartForm = &MultipartFormDef{FieldNames: []string{"payload"}}
			}

			type probeRequest struct {
				method, uri   string
				headers       http.Header
				body          []byte
				contentLength int64
				transfer      []string
				err           error
			}
			received := make(chan probeRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				received <- probeRequest{r.Method, r.RequestURI, r.Header.Clone(), body, r.ContentLength, r.TransferEncoding, err}
				w.Header().Set("Range", "bytes=0-1048575")
				w.Header().Set("Retry-After", "7")
				w.Header().Set("Set-Cookie", "secret=session")
				w.Header().Set("Authorization", "Bearer response-secret")
				w.Header().Set("X-API-Key", "response-secret")
				w.WriteHeader(http.StatusPermanentRedirect)
			}))
			defer upstream.Close()
			app.BaseURL = upstream.URL

			result, err := executeIntegrationTool(app, &tool, map[string]string{"access_token": "test-access-token"}, input, "")
			if err != nil {
				t.Fatal(err)
			}
			got := <-received
			if got.err != nil || len(got.body) != 0 || got.contentLength != 0 || got.headers.Get("Content-Length") != "0" || len(got.transfer) != 0 {
				t.Fatalf("probe was not exactly zero bytes: %+v", got)
			}
			if got.method != http.MethodPut || got.uri != tool.Path {
				t.Fatalf("session request changed: method=%s uri=%s", got.method, got.uri)
			}
			if got.headers.Get("Authorization") != "Bearer test-access-token" || got.headers.Get("Content-Range") != "bytes */2097152" || got.headers.Get("X-Probe") != "status" {
				t.Fatalf("probe headers changed: %v", got.headers)
			}
			if mode == "form" && got.headers.Get("Content-Type") != tool.Headers["Content-Type"] {
				t.Fatalf("explicit content type changed: %v", got.headers)
			}
			if result.Status != 308 || result.Success {
				t.Fatalf("incomplete upload should preserve status 308: %+v", result)
			}
			if result.Headers["Range"] != "bytes=0-1048575" || result.Headers["Retry-After"] != "7" {
				t.Fatalf("missing upload progress headers: %v", result.Headers)
			}
			for _, sensitive := range []string{"Set-Cookie", "Authorization", "X-API-Key"} {
				if _, ok := result.Headers[sensitive]; ok {
					t.Errorf("sensitive header forwarded: %s", sensitive)
				}
			}
		})
	}
}

func TestExecuteIntegrationToolBodyNoneDefaultBehavior(t *testing.T) {
	for _, mode := range []string{"json", "form"} {
		t.Run(mode, func(t *testing.T) {
			var tool AppToolDef
			// Existing catalogs omit body_none entirely.
			if err := json.Unmarshal([]byte(`{"name":"update","method":"PUT","path":"/items/{id}","query_params":["revision"],"header_params":{"request_id":"X-Request-Id"}}`), &tool); err != nil {
				t.Fatal(err)
			}
			app := &AppTemplate{Slug: "body-default-test"}
			app.Auth.Headers = map[string]string{"Authorization": "Bearer {{access_token}}"}
			app.Auth.BodyParams = map[string]string{"auth_field": "{{access_token}}"}
			if mode == "form" {
				app.Auth.Headers["Content-Type"] = "application/x-www-form-urlencoded"
			}
			received := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				received <- body
				if r.RequestURI != "/items/item-1?revision=3" || r.Header.Get("X-Request-Id") != "request-1" || r.Header.Get("Authorization") != "Bearer test-access-token" {
					t.Errorf("request metadata changed: %s %v", r.RequestURI, r.Header)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			app.BaseURL = upstream.URL
			result, err := executeIntegrationTool(app, &tool, map[string]string{"access_token": "test-access-token"}, map[string]any{
				"id": "item-1", "revision": "3", "request_id": "request-1", "title": "upload title",
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			if !result.Success || result.Status != http.StatusNoContent {
				t.Fatalf("result=%+v", result)
			}
			body := <-received
			want := map[string]string{"auth_field": "test-access-token", "title": "upload title"}
			got := map[string]string{}
			if mode == "form" {
				values, err := url.ParseQuery(string(body))
				if err != nil {
					t.Fatal(err)
				}
				for name := range values {
					got[name] = values.Get(name)
				}
			} else if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("existing body behavior changed: got=%v want=%v", got, want)
			}
		})
	}
}

func TestPickForwardableHeadersUploadProgress(t *testing.T) {
	for _, retryAfter := range []string{"120", "Wed, 14 Oct 2026 08:00:00 GMT"} {
		headers := http.Header{}
		headers.Set("range", "bytes=0-1048575")
		headers.Set("retry-after", retryAfter)
		headers.Set("Set-Cookie", "secret=session")
		headers.Set("X-Debug-Secret", "secret")
		want := map[string]string{"Range": "bytes=0-1048575", "Retry-After": retryAfter}
		if got := pickForwardableHeaders(headers); !reflect.DeepEqual(got, want) {
			t.Fatalf("headers=%v, want=%v", got, want)
		}
	}
}
