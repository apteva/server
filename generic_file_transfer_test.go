package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fileTransferDefinition(t *testing.T, slug, name string) (*AppTemplate, *AppToolDef) {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	for i := range app.Tools {
		if app.Tools[i].Name == name {
			return &app, &app.Tools[i]
		}
	}
	t.Fatalf("missing %s.%s", slug, name)
	return nil, nil
}

func TestGenericFileTransferGmailToDrive(t *testing.T) {
	content := []byte{0, 0xfb, 0xff, 0x80, '\r', '\n'}
	requests := make(chan string, 2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if r.URL.RawQuery != "" {
				t.Error("local file MIME metadata was sent to Gmail")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": base64.RawURLEncoding.EncodeToString(content), "size": len(content)})
		case http.MethodPatch:
			if r.URL.Query().Get("uploadType") != "media" || r.URL.Query().Get("supportsAllDrives") != "true" || r.URL.Query().Get("fields") != "id,size" {
				t.Error("missing Drive upload query options")
			}
			if r.Header.Get("Content-Type") != "application/pdf" {
				t.Error("file MIME type was not preserved")
			}
			body, _ := io.ReadAll(r.Body)
			if !bytes.Equal(content, body) {
				t.Error("file bytes changed during upload")
			}
			_, _ = w.Write([]byte(`{"id":"file"}`))
		}
	}))
	defer ts.Close()
	gmail, download := fileTransferDefinition(t, "gmail", "download_attachment")
	gmail.BaseURL = ts.URL
	result, err := executeIntegrationTool(gmail, download, nil, map[string]any{"messageId": "message", "attachmentId": "attachment", "mimeType": "application/pdf"}, "")
	if err != nil || !result.Success {
		t.Fatalf("download failed: %v", err)
	}
	envelope := result.Data.(map[string]any)
	if envelope["_binary"] != true || envelope["base64"] != base64.StdEncoding.EncodeToString(content) {
		t.Fatal("attachment did not become a binary envelope")
	}
	drive, upload := fileTransferDefinition(t, "google-drive", "upload_file_content")
	drive.BaseURL = ts.URL
	result, err = executeIntegrationTool(drive, upload, nil, map[string]any{"fileId": "file", "file": envelope, "fields": "id,size", "supportsAllDrives": true}, "")
	if err != nil || !result.Success {
		t.Fatalf("upload failed: %v", err)
	}
	if <-requests != "/users/me/messages/message/attachments/attachment" || <-requests != "/upload/drive/v3/files/file" {
		t.Fatal("incorrect download/upload routes")
	}
}

func TestGenericFileDownloadsPreserveBytesAndErrors(t *testing.T) {
	for _, slug := range []string{"google-cloud-storage", "google-drive"} {
		for _, mime := range []string{"application/json", "text/csv", "application/pdf"} {
			t.Run(slug+"/"+mime, func(t *testing.T) {
				content := []byte("  {\"value\": 1}\r\n\r\n")
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("alt") != "media" || len(r.URL.Query()["alt"]) != 1 {
						t.Error("media mode must be sent exactly once")
					}
					w.Header().Set("Content-Type", mime)
					_, _ = w.Write(content)
				}))
				defer ts.Close()
				name := "download_file"
				input := map[string]any{"fileId": "file"}
				if slug == "google-cloud-storage" {
					name = "download_object"
					input = map[string]any{"bucket": "source", "object": "folder/file", "alt": "media"}
				}
				app, tool := fileTransferDefinition(t, slug, name)
				app.BaseURL = ts.URL
				result, err := executeIntegrationTool(app, tool, nil, input, "")
				if err != nil || !result.Success {
					t.Fatalf("download failed: %v", err)
				}
				envelope, ok := result.Data.(map[string]any)
				if !ok || envelope["_binary"] != true || envelope["base64"] != base64.StdEncoding.EncodeToString(content) || envelope["mimeType"] != mime {
					t.Fatal("download did not preserve the file bytes and MIME type")
				}
			})
		}
	}
	t.Run("provider errors", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"Access denied"}}`))
		}))
		defer ts.Close()
		for _, def := range [][2]string{{"gmail", "download_attachment"}, {"google-cloud-storage", "download_object"}} {
			app, tool := fileTransferDefinition(t, def[0], def[1])
			app.BaseURL = ts.URL
			result, err := executeIntegrationTool(app, tool, nil, map[string]any{"messageId": "m", "attachmentId": "a", "bucket": "b", "object": "o"}, "")
			if err != nil || result.Success || result.Status != http.StatusForbidden {
				t.Fatalf("provider error was not preserved: %v", err)
			}
			m := result.Data.(map[string]any)
			if m["error"].(map[string]any)["message"] != "Access denied" {
				t.Fatal("provider error response changed")
			}
		}
	})
}

func TestGenericDriveMetadataAndParentRequests(t *testing.T) {
	for _, name := range []string{"create_folder", "create_file_metadata", "move_file", "update_file", "rename_file"} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("fields") != "id" || r.URL.Query().Get("supportsAllDrives") != "true" {
					t.Error("projection and shared-drive options must be query parameters")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
					t.Error(err)
				}
				if _, exists := body["fields"]; exists {
					t.Error("query options leaked into metadata")
				}
				if name == "create_folder" && body["mimeType"] != "application/vnd.google-apps.folder" {
					t.Error("folder MIME type was not injected")
				}
				if name == "create_file_metadata" && body["name"] != "report.pdf" {
					t.Error("missing file name")
				}
				if name == "move_file" || name == "update_file" {
					if r.URL.Query().Get("addParents") != "new" || r.URL.Query().Get("removeParents") != "old" || body["addParents"] != nil || body["removeParents"] != nil {
						t.Error("parent changes must be query parameters")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"file"}`))
			}))
			defer ts.Close()
			app, tool := fileTransferDefinition(t, "google-drive", name)
			app.BaseURL = ts.URL
			input := map[string]any{"fields": "id", "supportsAllDrives": true}
			switch name {
			case "create_folder":
				input["name"] = "folder"
			case "create_file_metadata":
				input["name"] = "report.pdf"
				input["parents"] = []string{"folder"}
				input["appProperties"] = map[string]any{"sourceId": "source"}
			default:
				input["fileId"] = "file"
				if name == "rename_file" {
					input["name"] = "renamed.pdf"
				} else {
					input["addParents"] = "new"
					input["removeParents"] = "old"
				}
			}
			result, err := executeIntegrationTool(app, tool, nil, input, "")
			if err != nil || !result.Success {
				t.Fatalf("request failed: %v", err)
			}
		})
	}
}

func TestGenericBase64BinaryTransform(t *testing.T) {
	for _, encoding := range []string{"base64", "base64url"} {
		codec := base64.StdEncoding
		if encoding == "base64url" {
			codec = base64.URLEncoding
		}
		for _, content := range [][]byte{{}, {0, 0xfb, 0xff}, {0, 0xff}} {
			for _, pad := range []bool{true, false} {
				encoder := codec
				if !pad {
					encoder = encoder.WithPadding(base64.NoPadding)
				}
				data, _, err := buildResponseTransformData(&ResponseTransformDef{Type: "base64_to_binary", Source: "payload.bytes", Encoding: encoding}, map[string]any{"payload": map[string]any{"bytes": encoder.EncodeToString(content)}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				envelope := data.(map[string]any)
				if envelope["base64"] != base64.StdEncoding.EncodeToString(content) || envelope["mimeType"] != "application/octet-stream" {
					t.Fatal("decoded binary content differs")
				}
			}
		}
	}
	for _, encoded := range []string{"A", "???", "AA=", "AB==", "AA==\n", strings.Repeat("A", ((maxBase64BinaryBytes+2)/3)*4+1)} {
		_, _, err := buildResponseTransformData(&ResponseTransformDef{Type: "base64_to_binary", Source: "data"}, map[string]any{"data": encoded}, nil)
		if err == nil {
			t.Fatal("accepted corrupt or oversized binary data")
		}
	}
}

func TestGenericAttachmentLargerThanJSONResponseCap(t *testing.T) {
	// Eight MiB of file bytes becomes over ten MB of JSON: it must not hit
	// the general metadata response cap before the bounded file decoder runs.
	content := bytes.Repeat([]byte{0xff}, 8*1024*1024)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": base64.RawURLEncoding.EncodeToString(content)})
	}))
	defer ts.Close()
	app, tool := fileTransferDefinition(t, "gmail", "download_attachment")
	app.BaseURL = ts.URL
	result, err := executeIntegrationTool(app, tool, nil, map[string]any{"messageId": "m", "attachmentId": "a"}, "")
	if err != nil || !result.Success {
		t.Fatalf("large attachment failed: %v", err)
	}
	if result.Data.(map[string]any)["size"] != len(content) {
		t.Fatal("attachment was truncated")
	}
}

func TestGenericDriveUploadRequiresValidBytes(t *testing.T) {
	requests := make(chan bool, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- true
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Error("zero-byte file upload changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file"}`))
	}))
	defer ts.Close()
	app, tool := fileTransferDefinition(t, "google-drive", "upload_file_content")
	app.BaseURL = ts.URL
	for _, file := range []any{nil, "plain text", map[string]any{"_binary": true}, map[string]any{"_binary": true, "base64": "???"}} {
		if _, err := executeIntegrationTool(app, tool, nil, map[string]any{"fileId": "file", "file": file}, ""); err == nil {
			t.Fatal("accepted missing or invalid binary file")
		}
	}
	if len(requests) != 0 {
		t.Fatal("invalid uploads reached the provider")
	}
	result, err := executeIntegrationTool(app, tool, nil, map[string]any{"fileId": "file", "file": map[string]any{"_binary": true, "base64": ""}}, "")
	if err != nil || !result.Success {
		t.Fatalf("valid zero-byte upload failed: %v", err)
	}
}
