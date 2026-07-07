package main

import (
	"encoding/json"
	"fmt"

	sdk "github.com/apteva/app-sdk"
)

const appBindingModeMultiple = "multiple"

func appBindingIsMultiple(dep sdk.IntegrationDep) bool {
	return dep.Mode == appBindingModeMultiple
}

func appBindingIsMultipleForManifest(manifest *sdk.Manifest, dep sdk.IntegrationDep) bool {
	if appBindingIsMultiple(dep) {
		return true
	}
	return manifest != nil && manifest.Name == "media-studio" && dep.Role == "image_provider"
}

func appBindingIsSet(raw any) bool {
	ids, _ := appBindingIDs(raw)
	return len(ids) > 0
}

func appBindingContains(raw any, id int64) bool {
	if id <= 0 {
		return false
	}
	ids, _ := appBindingIDs(raw)
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

func appBindingIDs(raw any) ([]int64, int64) {
	if id, ok := appBindingInt64(raw); ok && id > 0 {
		return []int64{id}, id
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, 0
	}
	ids := make([]int64, 0)
	seen := map[int64]bool{}
	switch v := m["ids"].(type) {
	case []any:
		for _, item := range v {
			if id, ok := appBindingInt64(item); ok && id > 0 && !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	case []int64:
		for _, id := range v {
			if id > 0 && !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	case []float64:
		for _, item := range v {
			id := int64(item)
			if id > 0 && !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	}
	defaultID := int64(0)
	if id, ok := appBindingInt64(m["default_id"]); ok && seen[id] {
		defaultID = id
	}
	return ids, defaultID
}

func appBindingInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n <= 0 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil && i > 0
	}
	return 0, false
}

func normalizeAppBinding(dep sdk.IntegrationDep, raw any) (any, error) {
	if raw == nil {
		return nil, nil
	}
	if !appBindingIsMultipleForManifest(nil, dep) {
		if id, ok := appBindingInt64(raw); ok && id > 0 {
			return id, nil
		}
		ids, defaultID := appBindingIDs(raw)
		if len(ids) == 0 {
			return nil, nil
		}
		if defaultID != 0 {
			return defaultID, nil
		}
		return ids[0], nil
	}

	ids, defaultID := appBindingIDs(raw)
	if len(ids) == 0 {
		return map[string]any{"ids": []int64{}}, nil
	}
	if defaultID == 0 {
		defaultID = ids[0]
	}
	return map[string]any{
		"ids":        ids,
		"default_id": defaultID,
	}, nil
}

func normalizeManifestIntegrationBindings(manifest *sdk.Manifest, bindings map[string]any) error {
	if manifest == nil {
		return nil
	}
	for _, dep := range manifest.Requires.Integrations {
		raw, present := bindings[dep.Role]
		if present {
			if appBindingIsMultipleForManifest(manifest, dep) && dep.Mode == "" {
				dep.Mode = appBindingModeMultiple
			}
			normalized, err := normalizeAppBinding(dep, raw)
			if err != nil {
				return fmt.Errorf("role %q: %w", dep.Role, err)
			}
			bindings[dep.Role] = normalized
		}
		if dep.Required && !appBindingIsSet(bindings[dep.Role]) {
			return fmt.Errorf("required integration role %q is unbound", dep.Role)
		}
	}
	return nil
}
