package managedmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dop251/goja"
)

const DefinitionVersion = 1

var toolNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,127}$`)

type Definition struct {
	Version int    `json:"version"`
	Tools   []Tool `json:"tools"`
}

type Tool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Handler      string         `json:"handler"`
	Code         string         `json:"code,omitempty"`
}

func EmptyDefinition() Definition {
	return Definition{Version: DefinitionVersion, Tools: []Tool{}}
}

func Load(sourceDir string) (Definition, error) {
	raw, err := os.ReadFile(filepath.Join(sourceDir, "server.json"))
	if err != nil {
		return Definition{}, err
	}
	var def Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		return Definition{}, fmt.Errorf("parse server.json: %w", err)
	}
	for i := range def.Tools {
		path, err := HandlerPath(sourceDir, def.Tools[i].Handler)
		if err != nil {
			return Definition{}, fmt.Errorf("tool %q: %w", def.Tools[i].Name, err)
		}
		code, err := os.ReadFile(path)
		if err != nil {
			return Definition{}, fmt.Errorf("read handler for %q: %w", def.Tools[i].Name, err)
		}
		def.Tools[i].Code = string(code)
	}
	if err := Validate(def); err != nil {
		return Definition{}, err
	}
	return def, nil
}

func Validate(def Definition) error {
	if def.Version == 0 {
		def.Version = DefinitionVersion
	}
	if def.Version != DefinitionVersion {
		return fmt.Errorf("unsupported definition version %d", def.Version)
	}
	seen := map[string]bool{}
	for i, tool := range def.Tools {
		tool.Name = strings.TrimSpace(tool.Name)
		if !toolNameRE.MatchString(tool.Name) {
			return fmt.Errorf("tool %d has invalid name %q", i+1, tool.Name)
		}
		if seen[tool.Name] {
			return fmt.Errorf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = true
		if strings.TrimSpace(tool.Description) == "" {
			return fmt.Errorf("tool %q requires a description", tool.Name)
		}
		if tool.InputSchema == nil {
			return fmt.Errorf("tool %q requires an input schema", tool.Name)
		}
		if typ, _ := tool.InputSchema["type"].(string); typ != "" && typ != "object" {
			return fmt.Errorf("tool %q input schema must have type object", tool.Name)
		}
		if typ, _ := tool.OutputSchema["type"].(string); typ != "" && typ != "object" {
			return fmt.Errorf("tool %q output schema must have type object", tool.Name)
		}
		if strings.TrimSpace(tool.Handler) == "" {
			return fmt.Errorf("tool %q requires a handler path", tool.Name)
		}
		if _, err := safeRelativePath(tool.Handler); err != nil {
			return fmt.Errorf("tool %q handler: %w", tool.Name, err)
		}
		if err := ValidateCode(tool.Code); err != nil {
			return fmt.Errorf("tool %q code: %w", tool.Name, err)
		}
	}
	return nil
}

func ValidateCode(code string) error {
	if strings.TrimSpace(code) == "" {
		return errors.New("handler code is empty")
	}
	_, err := goja.Compile("handler.js", HandlerProgram(code), false)
	if err != nil {
		return err
	}
	return nil
}

func HandlerProgram(code string) string {
	return "(function(input, apteva) {\n\"use strict\";\n" + code + "\n})"
}

func HandlerPath(sourceDir, handler string) (string, error) {
	rel, err := safeRelativePath(handler)
	if err != nil {
		return "", err
	}
	base, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(base, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	check, err := filepath.Rel(base, path)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", errors.New("handler escapes source directory")
	}
	return path, nil
}

func safeRelativePath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.HasPrefix(value, "/") {
		return "", errors.New("handler must be a relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("handler escapes source directory")
	}
	return clean, nil
}

func Normalize(def Definition) Definition {
	def.Version = DefinitionVersion
	if def.Tools == nil {
		def.Tools = []Tool{}
	} else {
		// Definition is passed by value but its Tools slice is not. Copy the
		// entries before filling handler defaults or sorting so validation and
		// persistence never mutate the caller's draft (notably clearing Code
		// when Write emits server.json).
		def.Tools = append([]Tool(nil), def.Tools...)
	}
	for i := range def.Tools {
		def.Tools[i].Name = strings.TrimSpace(def.Tools[i].Name)
		def.Tools[i].Description = strings.TrimSpace(def.Tools[i].Description)
		if def.Tools[i].Handler == "" {
			def.Tools[i].Handler = "tools/" + def.Tools[i].Name + ".js"
		}
		if def.Tools[i].InputSchema == nil {
			def.Tools[i].InputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
	}
	sort.SliceStable(def.Tools, func(i, j int) bool { return def.Tools[i].Name < def.Tools[j].Name })
	return def
}
