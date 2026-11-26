package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Core decodes config.json by type when it starts, so one value of the wrong
// type stops an agent from starting at all. A running agent's settings go
// through Core's /config, which checks them; a stopped agent's are written to
// config.json directly. These shapes mirror the types Core declares (core
// config.go, mcp.go, execution_control.go) for the settings a config update
// may carry, so a bad value is refused here instead of at the next start.
// Nested values Core keeps in shapes the server never writes are left as raw
// JSON and are not checked.

type coreMCPServerSettings struct {
	ToolAliases map[string]string `json:"tool_aliases"`
	Name        string            `json:"name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	Transport   string            `json:"transport"`
	URL         string            `json:"url"`
	ToolLoading *struct {
		Default string            `json:"default"`
		Tools   map[string]string `json:"tools"`
	} `json:"tool_loading"`
	NoSpawn bool `json:"no_spawn"`
}

type coreProviderSettings struct {
	Name              string            `json:"name"`
	Default           bool              `json:"default"`
	Models            map[string]string `json:"models"`
	ModelCapabilities map[string]struct {
		ContextWindow                 int    `json:"context_window"`
		MaxContextWindow              int    `json:"max_context_window"`
		EffectiveContextWindowPercent int    `json:"effective_context_window_percent"`
		DefaultReasoningLevel         string `json:"default_reasoning_level"`
		SupportedReasoningLevels      []struct {
			Effort      string `json:"effort"`
			Description string `json:"description"`
		} `json:"supported_reasoning_levels"`
		InputModalities             []string `json:"input_modalities"`
		SupportsParallelToolCalls   *bool    `json:"supports_parallel_tool_calls"`
		SupportsReasoningSummaries  *bool    `json:"supports_reasoning_summaries"`
		SupportsImageDetailOriginal *bool    `json:"supports_image_detail_original"`
		SupportsSearchTool          *bool    `json:"supports_search_tool"`
	} `json:"model_capabilities"`
	BuiltinTools      []string                   `json:"builtin_tools"`
	Builtins          map[string]json.RawMessage `json:"builtins"`
	ImageGeneration   json.RawMessage            `json:"image_generation"`
	ServiceTier       string                     `json:"service_tier"`
	MaxOutputTokens   int                        `json:"max_output_tokens"`
	RealtimeVoice     string                     `json:"realtime_voice"`
	RealtimeModels    map[string]string          `json:"realtime_models"`
	RealtimeExecution json.RawMessage            `json:"realtime_execution"`
}

type corePaceSettings struct {
	WaitForEvents bool      `json:"wait_for_events"`
	Sleep         string    `json:"sleep"`
	NextWakeAt    time.Time `json:"next_wake_at"`
}

type coreThreadSettings struct {
	RealtimeExecution   json.RawMessage   `json:"realtime_execution"`
	WorkMode            bool              `json:"work_mode"`
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	ParentID            string            `json:"parent_id"`
	Depth               int               `json:"depth"`
	System              bool              `json:"system"`
	Directive           string            `json:"directive"`
	Tools               []string          `json:"tools"`
	MCPNames            []string          `json:"mcp_names"`
	InheritCapabilities bool              `json:"inherit_capabilities"`
	Model               string            `json:"model"`
	Reasoning           string            `json:"reasoning"`
	Provider            string            `json:"provider"`
	ProviderPin         *string           `json:"provider_pin"`
	BuiltinTools        []string          `json:"builtin_tools"`
	Realtime            bool              `json:"realtime"`
	AllowNoSpawn        bool              `json:"allow_no_spawn"`
	Voice               string            `json:"voice"`
	RealtimeOutput      json.RawMessage   `json:"realtime_output"`
	TurnDetection       json.RawMessage   `json:"turn_detection"`
	Pace                *corePaceSettings `json:"pace"`
	Events              []struct {
		Work           json.RawMessage   `json:"work"`
		ExecutionIDs   []string          `json:"execution_ids"`
		ID             string            `json:"id"`
		Hash           string            `json:"hash"`
		Text           string            `json:"text"`
		Parts          []json.RawMessage `json:"parts"`
		Consumed       bool              `json:"consumed"`
		TrackLifecycle bool              `json:"track_lifecycle"`
		ExecutionID    string            `json:"execution_id"`
	} `json:"events"`
}

type coreExecutionControlSettings struct {
	Threads map[string]struct {
		Mode        string   `json:"mode"`
		Breakpoints []string `json:"breakpoints"`
	} `json:"threads"`
	Mode        string   `json:"mode"`
	Scope       string   `json:"scope"`
	Breakpoints []string `json:"breakpoints"`
	Follow      string   `json:"follow"`
}

// coreConfigSettings lists, in order, the settings a stopped agent's config
// update writes into config.json, each with the type Core decodes it into.
var coreConfigSettings = []struct {
	key    string
	target func() any
}{
	{"mcp_servers", func() any { return new([]coreMCPServerSettings) }},
	{"providers", func() any { return new([]coreProviderSettings) }},
	{"threads", func() any { return new([]coreThreadSettings) }},
	{"unconscious", func() any { return new(bool) }},
	{"execution_control", func() any { return new(coreExecutionControlSettings) }},
	{"realtime_enabled", func() any { return new(bool) }},
	{"realtime_voice", func() any { return new(string) }},
	{"realtime_voice_mcp", func() any { return new([]string) }},
	{"main_pace", func() any { return new(corePaceSettings) }},
}

// checkCoreConfigSettings refuses a config update whose Core settings Core
// could not decode, naming the setting. It leaves a null (a request to remove
// the setting) alone and normalizes realtime_voice_mcp the way Core does.
func checkCoreConfigSettings(body map[string]any) error {
	for _, setting := range coreConfigSettings {
		value, sent := body[setting.key]
		if !sent || value == nil {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("%s: %v", setting.key, err)
		}
		target := setting.target()
		if err := json.Unmarshal(encoded, target); err != nil {
			return coreSettingError(setting.key, target, err)
		}
		if setting.key == "realtime_voice_mcp" {
			body[setting.key] = compactSettingList(*target.(*[]string))
		}
	}
	return nil
}

func coreSettingError(key string, target any, err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := key
		if typeErr.Field != "" {
			field = key + "." + typeErr.Field
		} else if typeErr.Type != reflect.TypeOf(target).Elem() {
			field = key + " entries"
		}
		return fmt.Errorf("%s must be %s, not %s", field, describeSettingType(typeErr.Type), describeJSONValue(typeErr.Value))
	}
	// time.Time reports its own errors, without the field's path.
	var parseErr *time.ParseError
	if errors.As(err, &parseErr) || strings.Contains(err.Error(), "Time.UnmarshalJSON") {
		return fmt.Errorf("%s has a time that is not an RFC 3339 timestamp", key)
	}
	return fmt.Errorf("%s is not valid: %v", key, err)
}

func describeSettingType(t reflect.Type) string {
	if t == reflect.TypeOf(time.Time{}) {
		return "a timestamp"
	}
	switch t.Kind() {
	case reflect.Pointer:
		return describeSettingType(t.Elem())
	case reflect.Bool:
		return "true or false"
	case reflect.String:
		return "a string"
	case reflect.Int, reflect.Int64, reflect.Int32:
		return "a whole number"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return "a list of strings"
		}
		if t.Elem().Kind() == reflect.Struct {
			return "a list of objects"
		}
		return "a list"
	case reflect.Map, reflect.Struct:
		return "an object"
	default:
		return t.Kind().String()
	}
}

func describeJSONValue(kind string) string {
	switch kind {
	case "bool":
		return "true or false"
	case "string":
		return "a string"
	case "array":
		return "a list"
	case "object":
		return "an object"
	default:
		if strings.HasPrefix(kind, "number") {
			return "a number"
		}
		return kind
	}
}

// compactSettingList matches Core's compactStringList: trimmed, no empty
// entries, first occurrence kept.
func compactSettingList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
