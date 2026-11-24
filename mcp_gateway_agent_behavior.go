package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Mode and proactivity are server-managed agent fields. The agent routes
// drop them from config, so the gateway takes them only as top-level fields.
var gatewayConfigBehaviorKeys = []string{"mode", "proactivity"}

// gatewayRejectConfigBehaviorKeys refuses mode and proactivity inside a
// config patch. Accepting them there would report a change that never
// happens.
func gatewayRejectConfigBehaviorKeys(config map[string]any) error {
	for _, key := range gatewayConfigBehaviorKeys {
		if _, exists := config[key]; exists {
			return fmt.Errorf("config.%s is not accepted: set it with the top-level %s field", key, key)
		}
	}
	return nil
}

// gatewayConfigObject parses a config argument already normalized to JSON
// by optionalConfigArg.
func gatewayConfigObject(config string) (map[string]any, error) {
	var patch map[string]any
	if err := json.Unmarshal([]byte(config), &patch); err != nil || patch == nil {
		return nil, fmt.Errorf("config must be a JSON object")
	}
	return patch, nil
}

// gatewayProactivityArg reads the optional proactivity argument, an integer
// from 0 to 100. Models send tool arguments as JSON numbers or as strings.
func gatewayProactivityArg(args map[string]any) (int, bool, error) {
	raw, present := args["proactivity"]
	if !present || raw == nil {
		return 0, false, nil
	}
	invalid := fmt.Errorf("proactivity must be an integer from 0 to 100")
	var value float64
	switch v := raw.(type) {
	case float64:
		value = v
	case int:
		value = float64(v)
	case int64:
		value = float64(v)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false, invalid
		}
		value = parsed
	default:
		return 0, false, invalid
	}
	if value < 0 || value > 100 || value != float64(int(value)) {
		return 0, false, invalid
	}
	return int(value), true, nil
}
