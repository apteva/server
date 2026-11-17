package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

const maxIntegrationToolTimeout = 10 * time.Minute

type integrationToolTimeoutKey struct{}

// integrationExecutionInput keeps platform execution options separate from
// provider arguments. The reserved _apteva object is never sent upstream.
func integrationExecutionInput(input map[string]any, tool *AppToolDef, fallback time.Duration) (map[string]any, time.Duration, bool, error) {
	timeout := fallback
	if tool != nil && tool.TimeoutMS > 0 {
		timeout = time.Duration(tool.TimeoutMS) * time.Millisecond
	}
	maximum := maxIntegrationToolTimeout
	if tool != nil && tool.MaxTimeoutMS > 0 && time.Duration(tool.MaxTimeoutMS)*time.Millisecond < maximum {
		maximum = time.Duration(tool.MaxTimeoutMS) * time.Millisecond
	}
	if timeout > maximum {
		timeout = maximum
	}
	raw, present := input["_apteva"]
	if !present {
		return input, timeout, false, nil
	}
	options, ok := raw.(map[string]any)
	if !ok || len(options) != 1 {
		return nil, 0, false, fmt.Errorf("_apteva must contain only timeout_ms")
	}
	millis, ok := integrationTimeoutNumber(options["timeout_ms"])
	if !ok || millis <= 0 || millis > int64(maximum/time.Millisecond) {
		return nil, 0, false, fmt.Errorf("_apteva.timeout_ms must be an integer between 1 and %d", maximum/time.Millisecond)
	}
	clean := make(map[string]any, len(input)-1)
	for key, value := range input {
		if key != "_apteva" {
			clean[key] = value
		}
	}
	return clean, time.Duration(millis) * time.Millisecond, true, nil
}

func integrationTimeoutNumber(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		v, err := n.Int64()
		return v, err == nil
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n > math.MaxInt64 || n < math.MinInt64 {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

func integrationTimeoutFromContext(ctx context.Context, tool *AppToolDef, fallback time.Duration) time.Duration {
	if timeout, ok := ctx.Value(integrationToolTimeoutKey{}).(time.Duration); ok && timeout > 0 {
		return timeout
	}
	_, timeout, _, _ := integrationExecutionInput(nil, tool, fallback)
	return timeout
}

func integrationMCPInputSchema(tool *AppToolDef) map[string]any {
	schema := map[string]any{"type": "object"}
	for key, value := range tool.InputSchema {
		schema[key] = value
	}
	properties := map[string]any{}
	if existing, ok := schema["properties"].(map[string]any); ok {
		for key, value := range existing {
			properties[key] = value
		}
	}
	maximum := int64(maxIntegrationToolTimeout / time.Millisecond)
	if tool.MaxTimeoutMS > 0 && int64(tool.MaxTimeoutMS) < maximum {
		maximum = int64(tool.MaxTimeoutMS)
	}
	properties["_apteva"] = map[string]any{
		"type":        "object",
		"description": "Optional Apteva execution settings; never sent to the provider.",
		"properties": map[string]any{"timeout_ms": map[string]any{
			"type": "integer", "minimum": 1, "maximum": maximum,
			"description": "Maximum time for this connector call in milliseconds.",
		}},
		"required":             []string{"timeout_ms"},
		"additionalProperties": false,
	}
	schema["properties"] = properties
	if tool.TimeoutMS > 0 {
		schema["x-apteva-timeout-ms"] = tool.TimeoutMS
	}
	return schema
}
