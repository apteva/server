package managedmcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	input := Definition{
		Version: DefinitionVersion,
		Tools: []Tool{{
			Name:        "lookup_customer",
			Description: "Look up one customer.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
				"required": []any{"id"},
			},
			Handler: "tools/lookup_customer.js",
			Code:    `return {id: input.id, active: true};`,
		}},
	}
	if err := Write(dir, input); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "lookup_customer" {
		t.Fatalf("unexpected tools: %#v", got.Tools)
	}
	if got.Tools[0].Code != input.Tools[0].Code {
		t.Fatalf("handler code changed: %q", got.Tools[0].Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "tools", "lookup_customer.js")); err != nil {
		t.Fatalf("handler file: %v", err)
	}
}

func TestValidateRejectsUnsafeOrInvalidDefinitions(t *testing.T) {
	base := Tool{
		Name:        "safe_tool",
		Description: "A safe tool.",
		InputSchema: map[string]any{"type": "object"},
		Handler:     "tools/safe_tool.js",
		Code:        "return {ok: true};",
	}
	tests := []struct {
		name string
		edit func(*Tool)
		want string
	}{
		{"path traversal", func(tool *Tool) { tool.Handler = "../secret.js" }, "escapes"},
		{"bad name", func(tool *Tool) { tool.Name = "bad name" }, "invalid name"},
		{"non-object input", func(tool *Tool) { tool.InputSchema = map[string]any{"type": "array"} }, "type object"},
		{"invalid javascript", func(tool *Tool) { tool.Code = "return {" }, "code"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tool := base
			test.edit(&tool)
			err := Validate(Definition{Version: DefinitionVersion, Tools: []Tool{tool}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want substring %q", err, test.want)
			}
		})
	}
}
