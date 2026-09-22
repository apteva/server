package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	gatewayDefaultPageSize = 20
	gatewayMaxPageSize     = 100
	gatewayDescriptionSize = 220
	gatewayListDescription = 140
	gatewayDirectiveChunk  = 8000
)

func gatewayCompactText(value string, limit int) string {
	value = strings.TrimSpace(strings.Join(strings.Fields(value), " "))
	if limit <= 0 || len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return strings.TrimSpace(value[:end]) + "…"
}

func gatewayObject(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

func gatewayArray(value any) []any {
	if values, ok := value.([]any); ok {
		return values
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var values []any
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func gatewayPageArgs(args map[string]any, total int) (offset, limit int, err error) {
	offset, limit = 0, gatewayDefaultPageSize
	if value, ok := args["offset"]; ok {
		parsed, parseErr := parseIntArg(value)
		if parseErr != nil || parsed < 0 {
			return 0, 0, fmt.Errorf("offset must be a non-negative integer")
		}
		offset = int(parsed)
	}
	if value, ok := args["limit"]; ok {
		parsed, parseErr := parseIntArg(value)
		if parseErr != nil || parsed <= 0 {
			return 0, 0, fmt.Errorf("limit must be a positive integer")
		}
		limit = int(parsed)
		if limit > gatewayMaxPageSize {
			limit = gatewayMaxPageSize
		}
	}
	if offset > total {
		offset = total
	}
	return offset, limit, nil
}

func gatewayPage(items []any, args map[string]any, key string) (map[string]any, error) {
	offset, limit, err := gatewayPageArgs(args, len(items))
	if err != nil {
		return nil, err
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	page := items[offset:end]
	if page == nil {
		page = []any{}
	}
	result := map[string]any{
		key: page, "total": len(items), "offset": offset, "limit": limit,
		"has_more": end < len(items),
	}
	if end < len(items) {
		result["next_offset"] = end
	}
	return result, nil
}

func gatewayQueryMatch(query string, values ...any) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(fmt.Sprint(value)), query) {
			return true
		}
	}
	return false
}

func gatewayIntFromAny(value any) int {
	parsed, err := parseIntArg(value)
	if err != nil {
		return 0
	}
	return int(parsed)
}

func compactGatewayApp(value any, detail bool) map[string]any {
	row := gatewayObject(value)
	if row == nil {
		return nil
	}
	out := map[string]any{}
	listKeys := []string{"install_id", "name", "display_name", "version", "project_id", "status", "source"}
	for _, key := range listKeys {
		if value, ok := row[key]; ok {
			out[key] = value
		}
	}
	if detail {
		for _, key := range []string{
			"app_id", "available_version", "serving", "upgrade_in_progress",
			"deprecated", "replacement", "has_pending_options",
		} {
			if value, ok := row[key]; ok {
				out[key] = value
			}
		}
	}
	out["scope"] = "project"
	if strings.TrimSpace(fmt.Sprint(row["project_id"])) == "" {
		out["scope"] = "global"
	}
	if description, _ := row["description"].(string); description != "" {
		limit := gatewayListDescription
		if detail {
			limit = gatewayDescriptionSize
		}
		out["description"] = gatewayCompactText(description, limit)
	}
	for _, key := range []string{"status_message", "error_message", "deprecation"} {
		if message, _ := row[key].(string); strings.TrimSpace(message) != "" {
			out[key] = gatewayCompactText(message, gatewayDescriptionSize)
		}
	}
	if detail {
		for _, key := range []string{
			"icon", "icon_style", "upgrade_policy", "default_for_new_agents", "permissions",
			"surfaces", "bindings",
		} {
			if value, ok := row[key]; ok {
				out[key] = value
			}
		}
	}
	return out
}

func compactGatewayMarketplaceApp(value any) map[string]any {
	row := gatewayObject(value)
	if row == nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{
		"name", "display_name", "version", "author", "manifest_url", "repo", "official",
		"installed", "builtin", "category", "deprecated", "replacement",
	} {
		if value, ok := row[key]; ok {
			out[key] = value
		}
	}
	if description, _ := row["description"].(string); description != "" {
		out["description"] = gatewayCompactText(description, gatewayDescriptionSize)
	}
	if message, _ := row["deprecation"].(string); message != "" {
		out["deprecation"] = gatewayCompactText(message, gatewayDescriptionSize)
	}
	if surfaces := gatewayObject(row["surfaces"]); surfaces != nil {
		out["capabilities"] = map[string]any{
			"kind": surfaces["kind"], "mcp_tools": surfaces["mcp_tool_count"],
			"skills": surfaces["skill_count"], "ui_panels": surfaces["ui_panel_count"],
			"has_ui": surfaces["ui_app"],
		}
	}
	return out
}

func compactGatewayIntegration(value any, detail bool) map[string]any {
	row := gatewayObject(value)
	if row == nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"slug", "name", "logo", "categories", "auth_types", "tool_count", "has_webhooks", "kind"} {
		if value, ok := row[key]; ok {
			out[key] = value
		}
	}
	if description, _ := row["description"].(string); description != "" {
		out["description"] = gatewayCompactText(description, gatewayDescriptionSize)
	}
	if !detail {
		return out
	}
	if auth := gatewayObject(row["auth"]); auth != nil {
		out["auth"] = map[string]any{
			"types": auth["types"], "credential_fields": auth["credential_fields"],
		}
	}
	tools := []any{}
	for _, value := range gatewayArray(row["tools"]) {
		tool := gatewayObject(value)
		if tool == nil {
			continue
		}
		tools = append(tools, map[string]any{
			"name":        tool["name"],
			"description": gatewayCompactText(fmt.Sprint(tool["description"]), gatewayDescriptionSize),
		})
	}
	out["tools"] = tools
	out["tool_count"] = len(tools)
	if webhooks := gatewayObject(row["webhooks"]); webhooks != nil {
		out["webhook_events"] = webhooks["events"]
	}
	return out
}

func compactGatewayAgent(value any, includeDirective bool, args map[string]any) map[string]any {
	row := gatewayObject(value)
	if row == nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{
		"id", "name", "status", "project_id", "kind", "mode", "proactivity", "created_at",
		"core_version", "target_core_version", "core_update_available", "created", "idempotent_replay", "warning",
	} {
		if value, ok := row[key]; ok {
			out[key] = value
		}
	}
	if includeDirective {
		directive, _ := row["directive"].(string)
		offset := 0
		limit := gatewayDirectiveChunk
		if value, ok := args["directive_offset"]; ok {
			if parsed, err := parseIntArg(value); err == nil && parsed >= 0 {
				offset = int(parsed)
			}
		}
		if value, ok := args["directive_limit"]; ok {
			if parsed, err := parseIntArg(value); err == nil && parsed > 0 && parsed <= gatewayDirectiveChunk {
				limit = int(parsed)
			}
		}
		if offset > len(directive) {
			offset = len(directive)
		}
		for offset < len(directive) && directive[offset]&0xc0 == 0x80 {
			offset++
		}
		end := offset + limit
		if end > len(directive) {
			end = len(directive)
		}
		for end > offset && !utf8.ValidString(directive[offset:end]) {
			end--
		}
		out["directive"] = directive[offset:end]
		out["directive_bytes"] = len(directive)
		out["directive_offset"] = offset
		out["directive_has_more"] = end < len(directive)
		if end < len(directive) {
			out["directive_next_offset"] = end
		}
		if rawConfig, _ := row["config"].(string); rawConfig != "" {
			var config map[string]any
			if json.Unmarshal([]byte(rawConfig), &config) == nil {
				out["settings"] = readOnlyAgentConfig(config)
			}
		}
	}
	return out
}

func compactGatewaySetup(value any, name string) any {
	root := gatewayObject(value)
	if root == nil {
		return value
	}
	compactAgentSpec := func(raw any) map[string]any {
		row := gatewayObject(raw)
		out := map[string]any{}
		for _, key := range []string{"key", "name", "mode", "unconscious", "apps", "app_install_ids"} {
			if value, ok := row[key]; ok {
				out[key] = value
			}
		}
		if directive, _ := row["directive"].(string); directive != "" {
			out["directive_summary"] = gatewayCompactText(directive, gatewayDescriptionSize)
		}
		return out
	}
	compactAgents := func(raw any) []any {
		items := gatewayArray(raw)
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, compactAgentSpec(item))
		}
		return out
	}

	if name == "setup_presets_list" {
		presets := gatewayArray(root["presets"])
		out := make([]any, 0, len(presets))
		for _, presetRaw := range presets {
			preset := gatewayObject(presetRaw)
			summary := map[string]any{}
			for _, key := range []string{"id", "name", "description", "category", "interface_level", "highlights", "dashboard"} {
				if value, ok := preset[key]; ok {
					summary[key] = value
				}
			}
			summary["agents"] = compactAgents(preset["agents"])
			out = append(out, summary)
		}
		return map[string]any{"schema_version": root["schema_version"], "presets": out, "total": len(out)}
	}

	if preset := gatewayObject(root["preset"]); preset != nil {
		preset["agents"] = compactAgents(preset["agents"])
		root["preset"] = preset
	}
	if _, ok := root["agents"]; ok {
		root["agents"] = compactAgents(root["agents"])
	}
	if name == "setup_apply" {
		for _, key := range []string{"created_agents", "existing_agents"} {
			items := gatewayArray(root[key])
			compact := make([]any, 0, len(items))
			for _, item := range items {
				compact = append(compact, compactGatewayAgent(item, false, nil))
			}
			root[key] = compact
		}
	}
	return root
}

func gatewayMarshalToolResult(value any) ([]byte, error) {
	// MCP results are machine-consumed. Compact JSON avoids spending model
	// context on indentation while preserving the exact structured payload.
	return json.Marshal(value)
}
