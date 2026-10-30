package main

import "encoding/json"

// The Helper's MCP entries carry server-minted loopback URLs with an
// mcp_token capability, and remote entries carry whatever URL the
// integration stored. Core needs them; the browser does not. These helpers
// strip them from every Helper response the dashboard can reach. The
// dashboard reads Helper MCP selection from /platform/helper/capabilities
// and attachment state from each entry's name and connected flag.

// platformHelperResponseConfig drops mcp_servers from the Helper's stored
// config JSON before it is serialized to a client.
func platformHelperResponseConfig(raw string) string {
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil || config == nil {
		return "{}"
	}
	delete(config, "mcp_servers")
	out, err := json.Marshal(config)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// redactMCPServerEntries keeps the descriptive fields of MCP entries and
// drops anything that can address or authenticate to the server.
func redactMCPServerEntries(servers any) any {
	entries, ok := servers.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		clean := make(map[string]any, len(m))
		for k, v := range m {
			switch k {
			case "url", "command", "args", "env", "headers":
				continue
			}
			clean[k] = v
		}
		out = append(out, clean)
	}
	return out
}

// redactPlatformHelperConfigBody rewrites a core /config JSON response for
// the Helper so its mcp_servers entries carry no URLs or credentials.
func redactPlatformHelperConfigBody(body []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body, false
	}
	if _, ok := payload["mcp_servers"]; !ok {
		return body, false
	}
	payload["mcp_servers"] = redactMCPServerEntries(payload["mcp_servers"])
	out, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return out, true
}
