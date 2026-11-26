package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
)

// automatic_tool_loading is a Core setting: which tool schemas Core loads
// before each model request when an agent has too many tools to show at once.
// Operators and Helper save it in the agent's config, but Core never reads the
// server's config string. These helpers check the value against Core's limits
// and hand it to Core as the top-level key Core reads.
const automaticToolLoadingKey = "automatic_tool_loading"

// Core's limits, from core/automatic_tool_loading.go.
const (
	automaticToolLoadingMaxTools        = 20
	automaticToolLoadingMaxSchemaTokens = 16000
	automaticToolLoadingMaxWorkflow     = 20
	automaticToolLoadingMaxNameLength   = 256
)

// validateAutomaticToolLoading accepts exactly the object Core reads. Core
// decodes a present object field by field, so an object without "enabled"
// turns loading off and one without "include_memory" turns memory off. To
// avoid switching loading off by accident, "enabled" is required.
func validateAutomaticToolLoading(value any) error {
	obj, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("automatic_tool_loading must be an object such as {\"enabled\": true, \"max_schema_tokens\": 6000}")
	}
	if _, ok := obj["enabled"]; !ok {
		return fmt.Errorf("automatic_tool_loading.enabled is required (true or false)")
	}
	for key, v := range obj {
		switch key {
		case "enabled", "include_memory":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("automatic_tool_loading.%s must be true or false", key)
			}
		case "max_tools":
			if n, ok := settingInt(v); !ok || n < 0 || n > automaticToolLoadingMaxTools {
				return fmt.Errorf("automatic_tool_loading.max_tools must be an integer from 0 (default) to %d", automaticToolLoadingMaxTools)
			}
		case "max_schema_tokens":
			if n, ok := settingInt(v); !ok || n < 0 || n > automaticToolLoadingMaxSchemaTokens {
				return fmt.Errorf("automatic_tool_loading.max_schema_tokens must be an integer from 0 (default) to %d", automaticToolLoadingMaxSchemaTokens)
			}
		case "workflow_tools":
			list, ok := v.([]any)
			if !ok {
				return fmt.Errorf("automatic_tool_loading.workflow_tools must be a list of tool names")
			}
			if len(list) > automaticToolLoadingMaxWorkflow {
				return fmt.Errorf("automatic_tool_loading.workflow_tools allows at most %d tools", automaticToolLoadingMaxWorkflow)
			}
			for _, item := range list {
				name, ok := item.(string)
				if !ok || !validCoreToolName(name) {
					return fmt.Errorf("automatic_tool_loading.workflow_tools must contain full tool names such as crm_contacts_get_context")
				}
			}
		default:
			return fmt.Errorf("automatic_tool_loading.%s is not a setting; use enabled, workflow_tools, max_tools, max_schema_tokens or include_memory", key)
		}
	}
	return nil
}

func settingInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// validCoreToolName mirrors Core's validToolName plus its length cap.
func validCoreToolName(name string) bool {
	if name == "" || len(name) > automaticToolLoadingMaxNameLength {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// validateAgentToolLoadingConfig checks automatic_tool_loading inside a saved
// or incoming agent config object, so a bad value is refused when it is saved.
func validateAgentToolLoadingConfig(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil || cfg == nil {
		return fmt.Errorf("config must be a JSON object")
	}
	if value, ok := cfg[automaticToolLoadingKey]; ok {
		return validateAutomaticToolLoading(value)
	}
	return nil
}

// automaticToolLoadingUpdate returns the value a config update sends to Core:
// the top-level key (Core's own shape) or the key inside the config patch
// (agents_update and the dashboard's advanced settings). Only the incoming
// patch counts, so an unrelated edit never re-sends a saved value over one
// Core already holds.
func automaticToolLoadingUpdate(rawBody map[string]any, configPatch string) (any, bool, error) {
	top, hasTop := rawBody[automaticToolLoadingKey]
	var patched any
	hasPatched := false
	if strings.TrimSpace(configPatch) != "" {
		var patch map[string]any
		if err := json.Unmarshal([]byte(configPatch), &patch); err != nil {
			return nil, false, fmt.Errorf("config must be a JSON object")
		}
		patched, hasPatched = patch[automaticToolLoadingKey]
	}
	if hasTop && hasPatched {
		return nil, false, fmt.Errorf("send automatic_tool_loading either at the top level or inside config, not both")
	}
	if !hasTop && !hasPatched {
		return nil, false, nil
	}
	value := top
	if hasPatched {
		value = patched
	}
	if err := validateAutomaticToolLoading(value); err != nil {
		return nil, false, err
	}
	return value, true, nil
}

// restoreAutomaticToolLoading fills Core's start config from the saved agent
// config when Core's own file has no value yet. Core's file wins when both
// exist, because a running Core can be changed through its own /config.
func restoreAutomaticToolLoading(agentID int64, coreConfig map[string]any, savedConfig string) {
	if _, ok := coreConfig[automaticToolLoadingKey]; ok || strings.TrimSpace(savedConfig) == "" {
		return
	}
	var saved map[string]any
	if json.Unmarshal([]byte(savedConfig), &saved) != nil {
		return
	}
	value, ok := saved[automaticToolLoadingKey]
	if !ok {
		return
	}
	if err := validateAutomaticToolLoading(value); err != nil {
		log.Printf("[CONFIG] agent=%d ignoring saved automatic_tool_loading: %v", agentID, err)
		return
	}
	coreConfig[automaticToolLoadingKey] = value
}
