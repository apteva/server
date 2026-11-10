package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Only registered tool/route pairs may use the clone's operator surface.
// A runtime caller never supplies a URL or selects an arbitrary HTTP route.
var runtimeOperatorPaths = map[string]map[string]string{
	"database": {
		"db_database_create":   "/operations/database_create",
		"db_collection_create": "/operations/collection_create",
		"db_insert":            "/operations/insert",
		"db_access_grant":      "/operations/access_grant",
		"db_get":               "/operations/get",
	},
}

func callRuntimeOperatorOperation(ctx context.Context, install *localInstall, token, path string, input map[string]any) (json.RawMessage, error) {
	if input == nil {
		input = map[string]any{}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, install.SidecarURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Apteva-Project-ID", install.ProjectID)
	client := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAppCallResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAppCallResponseBytes {
		return nil, fmt.Errorf("runtime app response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("runtime operation HTTP %d: %.400s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("runtime operation did not return JSON")
	}
	// The SDK decodes the JSON text inside the MCP result. Wrapping the raw
	// response avoids rounding integers through a float64 intermediary.
	return json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(raw)}},
	})
}

// Runtime fixture inputs can refer to the install ID assigned during cloning
// with {"$runtime_install_id":"app-name"}. Resolve only from this runtime.
func resolveRuntimeInstallRefs(input map[string]any, runtime *Environment) error {
	var resolve func(any) (any, error)
	resolve = func(value any) (any, error) {
		switch typed := value.(type) {
		case map[string]any:
			if raw, present := typed["$runtime_install_id"]; present {
				name, ok := raw.(string)
				if !ok || name == "" || len(typed) != 1 {
					return nil, fmt.Errorf("runtime install reference must contain one app name")
				}
				install, ok := runtime.Install(name)
				if !ok || install.ProjectID != runtime.ID || install.InstallID <= 0 {
					return nil, fmt.Errorf("runtime install reference %q has no clone", name)
				}
				return install.InstallID, nil
			}
			for key, item := range typed {
				resolved, err := resolve(item)
				if err != nil {
					return nil, err
				}
				typed[key] = resolved
			}
		case []any:
			for index, item := range typed {
				resolved, err := resolve(item)
				if err != nil {
					return nil, err
				}
				typed[index] = resolved
			}
		}
		return value, nil
	}
	for key, item := range input {
		resolved, err := resolve(item)
		if err != nil {
			return err
		}
		input[key] = resolved
	}
	return nil
}
