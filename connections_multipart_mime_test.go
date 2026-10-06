package main

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

func TestMultipartFileDeclaredMIME(t *testing.T) {
	for _, tc := range []struct {
		name        string
		file        any
		contentType string
		bytes       []byte
	}{
		{"base64-data-url", "data:image/png;base64,AP+ADQo=", "image/png", []byte{0, 255, 128, 13, 10}},
		{"binary-envelope", map[string]any{"base64": "AP+ADQo=", "mimeType": "image/png"}, "image/png", []byte{0, 255, 128, 13, 10}},
		{"raw-base64", "AP+ADQo=", "application/octet-stream", []byte{0, 255, 128, 13, 10}},
		{"percent-data-url", "data:text/plain,hello%20+%20user%0D%0A", "text/plain", []byte("hello + user\r\n")},
		{"invalid-mime", map[string]any{"base64": "AP+ADQo=", "mimeType": "image/png\r\nX-Injected: true"}, "application/octet-stream", []byte{0, 255, 128, 13, 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &AppToolDef{MultipartForm: &MultipartFormDef{FileFields: map[string]string{"file": "file"}}}
			body, contentType, err := buildMultipartRequestBody(tool, map[string]any{"file": tc.file, "filename": "test.png"}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, params, err := mime.ParseMediaType(contentType)
			if err != nil {
				t.Fatal(err)
			}
			reader := multipart.NewReader(body, params["boundary"])
			part, err := reader.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(part)
			if !bytes.Equal(raw, tc.bytes) || part.Header.Get("Content-Type") != tc.contentType || part.FormName() != "file" || part.FileName() != "test.png" {
				t.Fatalf("unexpected file: %v %#v", raw, part.Header)
			}
			if _, err := reader.NextPart(); err != io.EOF {
				t.Fatalf("unexpected extra multipart field: %v", err)
			}
		})
	}
}
