package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIntegrationExecutionInput(t *testing.T) {
	tool := &AppToolDef{TimeoutMS: 240000, MaxTimeoutMS: 300000}
	input := map[string]any{"prompt": "hello", "_apteva": map[string]any{"timeout_ms": float64(270000)}}
	clean, timeout, overridden, err := integrationExecutionInput(input, tool, 30*time.Second)
	if err != nil || !overridden || timeout != 270*time.Second {
		t.Fatalf("timeout=%s overridden=%t err=%v", timeout, overridden, err)
	}
	if _, present := clean["_apteva"]; present || input["_apteva"] == nil {
		t.Fatal("execution metadata reached provider or caller input was mutated")
	}
	for _, value := range []any{float64(300001), float64(0), float64(2.5), "270000", true} {
		_, _, _, err := integrationExecutionInput(map[string]any{"_apteva": map[string]any{"timeout_ms": value}}, tool, 30*time.Second)
		if err == nil {
			t.Fatalf("accepted invalid timeout %v", value)
		}
	}
	_, defaultTimeout, overridden, err := integrationExecutionInput(nil, tool, 30*time.Second)
	if err != nil || overridden || defaultTimeout != 240*time.Second {
		t.Fatalf("default timeout=%s overridden=%t err=%v", defaultTimeout, overridden, err)
	}
}

func TestIntegrationExecutionOptionsNeverReachProvider(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	app := &AppTemplate{Slug: "test", BaseURL: upstream.URL}
	tool := &AppToolDef{Name: "send", Method: http.MethodPost, Path: "/", TimeoutMS: 30000}
	result, err := executeIntegrationTool(app, tool, nil, map[string]any{
		"prompt": "hello", "_apteva": map[string]any{"timeout_ms": float64(90000)},
	}, "", context.Background())
	if err != nil || result == nil || !result.Success {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if received["prompt"] != "hello" || received["_apteva"] != nil {
		t.Fatalf("provider input=%#v", received)
	}
}

func TestIntegrationExecutionOverrideBoundsProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer upstream.Close()
	app := &AppTemplate{Slug: "test", BaseURL: upstream.URL}
	tool := &AppToolDef{Name: "slow", Method: http.MethodPost, Path: "/", TimeoutMS: 300000}
	started := time.Now()
	_, err := executeIntegrationTool(app, tool, nil, map[string]any{"_apteva": map[string]any{"timeout_ms": float64(50)}}, "")
	if err == nil || !strings.Contains(err.Error(), "deadline") || time.Since(started) > time.Second {
		t.Fatalf("err=%v elapsed=%s", err, time.Since(started))
	}
}

func TestIntegrationMCPInputSchemaIncludesExecutionOption(t *testing.T) {
	tool := &AppToolDef{TimeoutMS: 240000, MaxTimeoutMS: 300000, InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"prompt": map[string]any{"type": "string"}},
	}}
	schema := integrationMCPInputSchema(tool)
	if schema["x-apteva-timeout-ms"] != 240000 {
		t.Fatalf("timeout metadata=%v", schema["x-apteva-timeout-ms"])
	}
	props := schema["properties"].(map[string]any)
	option := props["_apteva"].(map[string]any)["properties"].(map[string]any)["timeout_ms"].(map[string]any)
	if option["maximum"] != int64(300000) || props["prompt"] == nil {
		t.Fatalf("schema=%#v", schema)
	}
	if _, changed := tool.InputSchema["properties"].(map[string]any)["_apteva"]; changed {
		t.Fatal("catalog schema was mutated")
	}
}
