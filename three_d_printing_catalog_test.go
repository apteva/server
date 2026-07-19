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

func embeddedIntegrationForTest(t *testing.T, slug string) AppTemplate {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
	if err != nil {
		t.Fatalf("read %s catalog: %v", slug, err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatalf("decode %s catalog: %v", slug, err)
	}
	return app
}

func integrationToolForTest(t *testing.T, app AppTemplate, name string) AppToolDef {
	t.Helper()
	for _, tool := range app.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("%s catalog missing %s", app.Slug, name)
	return AppToolDef{}
}

func readMultipartParts(t *testing.T, r *http.Request) map[string][]string {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("Content-Type=%q params=%v err=%v", mediaType, params, err)
	}
	parts := map[string][]string{}
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
	}
	return parts
}

func TestShapewaysUploadUsesDocumentedJSONContract(t *testing.T) {
	var body map[string]any
	var contentType, authorization string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		authorization = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode Shapeways body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"success","modelId":123}`))
	}))
	defer ts.Close()

	app := embeddedIntegrationForTest(t, "shapeways")
	app.BaseURL = ts.URL
	tool := integrationToolForTest(t, app, "upload_model")
	res, err := executeIntegrationTool(&app, &tool, map[string]string{"token": "access-token"}, map[string]any{
		"file":                     base64.StdEncoding.EncodeToString([]byte("solid cube\nendsolid cube\n")),
		"fileName":                 "cube.stl",
		"uploadScale":              0.001,
		"hasRightsToModel":         true,
		"acceptTermsAndConditions": true,
	}, "")
	if err != nil || res == nil || !res.Success {
		t.Fatalf("execute result=%+v err=%v", res, err)
	}
	if !strings.HasPrefix(contentType, "application/json") || authorization != "Bearer access-token" {
		t.Fatalf("headers Content-Type=%q Authorization=%q", contentType, authorization)
	}
	if body["fileName"] != "cube.stl" || body["hasRightsToModel"] != true || body["acceptTermsAndConditions"] != true {
		t.Fatalf("Shapeways upload body=%#v", body)
	}
}

func TestSculpteoUploadUsesMultipartWithBodyCredentials(t *testing.T) {
	var parts map[string][]string
	var requestedWith string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedWith = r.Header.Get("X-Requested-With")
		parts = readMultipartParts(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uuid":"design-1"}`))
	}))
	defer ts.Close()

	app := embeddedIntegrationForTest(t, "sculpteo")
	app.BaseURL = ts.URL
	tool := integrationToolForTest(t, app, "upload_design")
	res, err := executeIntegrationTool(&app, &tool, map[string]string{
		"login": "designer@example.com", "password": "secret",
	}, map[string]any{
		"language": "en", "file": base64.StdEncoding.EncodeToString([]byte("solid test\nendsolid test\n")),
		"filename": "test.stl", "name": "Test part", "unit": "mm",
	}, "")
	if err != nil || res == nil || !res.Success {
		t.Fatalf("execute result=%+v err=%v", res, err)
	}
	if requestedWith != "XMLHttpRequest" {
		t.Fatalf("X-Requested-With=%q", requestedWith)
	}
	if got := parts["file"]; len(got) != 1 || got[0] != "solid test\nendsolid test\n" {
		t.Fatalf("file parts=%v", got)
	}
	if parts["login"][0] != "designer@example.com" || parts["password"][0] != "secret" || parts["name"][0] != "Test part" {
		t.Fatalf("multipart fields=%v", parts)
	}
}

func TestTreatstockUploadRepeatsURLsAndUsesPrivateKeyQuery(t *testing.T) {
	var parts map[string][]string
	var privateKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateKey = r.URL.Query().Get("private-key")
		parts = readMultipartParts(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":223672}`))
	}))
	defer ts.Close()

	app := embeddedIntegrationForTest(t, "treatstock")
	app.BaseURL = ts.URL
	tool := integrationToolForTest(t, app, "create_printable_pack")
	res, err := executeIntegrationTool(&app, &tool, map[string]string{"private_key": "private-token"}, map[string]any{
		"files-urls[]":      []any{"https://files.example/one.stl", "https://files.example/two.3mf"},
		"location[country]": "ES",
	}, "")
	if err != nil || res == nil || !res.Success {
		t.Fatalf("execute result=%+v err=%v", res, err)
	}
	if privateKey != "private-token" {
		t.Fatalf("private-key=%q", privateKey)
	}
	wantURLs := []string{"https://files.example/one.stl", "https://files.example/two.3mf"}
	if got := parts["files-urls[]"]; len(got) != 2 || got[0] != wantURLs[0] || got[1] != wantURLs[1] {
		t.Fatalf("files-urls[]=%v", got)
	}
	if got := parts["location[country]"]; len(got) != 1 || got[0] != "ES" {
		t.Fatalf("location[country]=%v", got)
	}
}
