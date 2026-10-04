package main

import (
	"bytes"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// RUN_NATIVE_IMAGE_GENERATION_LIVE=1 go test -v -count=1 -run '^TestNativeImageGenerationLive$' -timeout 6m .
// Default: Codex + GPT Image 2.5 Flare, using an existing local credential.
// Select API billing with APTEVA_IMAGE_GENERATION_LIVE_PROVIDER=openai.
// Tests never start/reconfigure an existing agent or write to its storage.
func TestNativeImageGenerationLive(t *testing.T) {
	if testing.Short() || os.Getenv("RUN_NATIVE_IMAGE_GENERATION_LIVE") != "1" {
		t.Skip("set RUN_NATIVE_IMAGE_GENERATION_LIVE=1 without -short")
	}
	s := newTestServer(t)
	caller := newBlobTestCaller(t, s)
	caller.threadID = "image-live"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/runtime-image-responses", s.handleRuntimeImageResponses)
	var consumed atomic.Bool
	mux.HandleFunc("/consume-file", func(w http.ResponseWriter, r *http.Request) {
		if !hmac.Equal([]byte(r.Header.Get("X-Agent-Secret")), []byte(s.instanceSecret)) {
			http.Error(w, "unauthorized", 401)
			return
		}
		var args map[string]any
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&args) != nil {
			http.Error(w, "bad arguments", 400)
			return
		}
		resolved, release, err := s.resolveFileArguments(r.Context(), args, map[string]any{"properties": map[string]any{"ref": sdk.FileArgumentSchema("Generated image")}}, caller)
		if err != nil {
			http.Error(w, "file resolution failed", 500)
			return
		}
		defer release()
		envelope, ok := resolved["ref"].(map[string]any)
		encoded, _ := envelope["base64"].(string)
		data, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if !ok || decodeErr != nil {
			http.Error(w, "invalid file", 500)
			return
		}
		config, format, imageErr := image.DecodeConfig(bytes.NewReader(data))
		if imageErr != nil || format != "png" || config.Width <= 0 || config.Height <= 0 {
			http.Error(w, "invalid image", 500)
			return
		}
		consumed.Store(true)
		_, _ = w.Write([]byte("FILE_ACCEPTED"))
	})
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	cmd := exec.Command("go", "test", "-v", "-count=1", "-run", "^TestNativeImageGenerationLive$", "-timeout", "5m", ".")
	cmd.Dir = "../core"
	cmd.Env = append(os.Environ(), "RUN_NATIVE_IMAGE_GENERATION_LIVE=1", "APTEVA_IMAGE_GENERATION_GATEWAY_URL="+gateway.URL+"/api/runtime-image-responses", "APTEVA_IMAGE_GENERATION_LIVE_CONSUMER_URL="+gateway.URL+"/consume-file", "AGENT_ID="+strconv.FormatInt(caller.agentID, 10), "AGENT_SECRET="+s.instanceSecret)
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("Core live image test failed: %v", err)
	}
	if !consumed.Load() {
		t.Fatal("real LLM never consumed the generated file through the gateway")
	}
	var count int
	if err := s.store.db.QueryRow("SELECT COUNT(*) FROM server_blob_grants WHERE agent_id=? AND thread_id=?", caller.agentID, caller.threadID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("image grant count=%d err=%v", count, err)
	}
	// Optional review artifact. Export from Server storage, never through Core.
	if path := os.Getenv("APTEVA_IMAGE_GENERATION_LIVE_OUTPUT"); path != "" {
		if !filepath.IsAbs(path) {
			t.Fatal("APTEVA_IMAGE_GENERATION_LIVE_OUTPUT must be an absolute path")
		}
		var data []byte
		if err := s.store.db.QueryRow(`SELECT b.data FROM server_blobs b JOIN server_blob_grants g ON g.file_id=b.id WHERE g.agent_id=? AND g.thread_id=?`, caller.agentID, caller.threadID).Scan(&data); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("exported generated image: %s", path)
	}
}
