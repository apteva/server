package main

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

// Node's generated schemas use this bounded JSON Schema subset. Unsupported
// assertion keywords fail closed rather than being silently ignored.
func validateDeviceToolArguments(rawSchema, args json.RawMessage) error {
	var schema map[string]any
	var value any
	if json.Unmarshal(rawSchema, &schema) != nil || json.Unmarshal(args, &value) != nil {
		return fmt.Errorf("invalid device tool arguments")
	}
	return validateDeviceSchemaValue(schema, value, "arguments", 0)
}
func validateDeviceSchemaValue(s map[string]any, v any, path string, depth int) error {
	if depth > 16 {
		return fmt.Errorf("%s exceeds schema depth", path)
	}
	for key := range s {
		switch key {
		case "type", "properties", "required", "additionalProperties", "items", "enum", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "description", "title", "default", "$schema":
		default:
			return fmt.Errorf("%s: unsupported schema constraint %s", path, key)
		}
	}
	bad := func() error { return fmt.Errorf("%s does not match its device tool schema", path) }
	if values, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range values {
			found = found || reflect.DeepEqual(x, v)
		}
		if !found {
			return bad()
		}
	}
	switch s["type"] {
	case "object":
		object, ok := v.(map[string]any)
		if !ok || object == nil {
			return bad()
		}
		properties, _ := s["properties"].(map[string]any)
		if additional, exists := s["additionalProperties"]; exists {
			if _, ok := additional.(bool); !ok {
				return fmt.Errorf("%s: unsupported additionalProperties schema", path)
			}
		}
		if required, ok := s["required"].([]any); ok {
			for _, field := range required {
				name, ok := field.(string)
				if !ok {
					return bad()
				}
				if _, ok = object[name]; !ok {
					return fmt.Errorf("%s.%s is required", path, name)
				}
			}
		}
		for name, value := range object {
			if raw, ok := properties[name]; ok {
				child, ok := raw.(map[string]any)
				if !ok {
					return bad()
				}
				if err := validateDeviceSchemaValue(child, value, path+"."+name, depth+1); err != nil {
					return err
				}
			} else if additional, ok := s["additionalProperties"].(bool); ok && !additional {
				return fmt.Errorf("%s.%s is not allowed", path, name)
			}
		}
	case "array":
		list, ok := v.([]any)
		if !ok {
			return bad()
		}
		if n, ok := s["minItems"].(float64); ok && len(list) < int(n) {
			return bad()
		}
		if n, ok := s["maxItems"].(float64); ok && len(list) > int(n) {
			return bad()
		}
		if child, ok := s["items"].(map[string]any); ok {
			for i, x := range list {
				if err := validateDeviceSchemaValue(child, x, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
					return err
				}
			}
		}
	case "string":
		text, ok := v.(string)
		if !ok {
			return bad()
		}
		if n, ok := s["minLength"].(float64); ok && len([]rune(text)) < int(n) {
			return bad()
		}
		if n, ok := s["maxLength"].(float64); ok && len([]rune(text)) > int(n) {
			return bad()
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return bad()
		}
	case "integer", "number":
		number, ok := v.(float64)
		if !ok {
			return bad()
		}
		if s["type"] == "integer" && math.Trunc(number) != number {
			return bad()
		}
		if n, ok := s["minimum"].(float64); ok && number < n {
			return bad()
		}
		if n, ok := s["maximum"].(float64); ok && number > n {
			return bad()
		}
	case "null":
		if v != nil {
			return bad()
		}
	case nil:
	default:
		return bad()
	}
	return nil
}
