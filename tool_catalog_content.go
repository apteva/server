package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

const toolCatalogFormat = "apteva.tool-catalog/v1"

// Revision, transport addresses and activation generations are deliberately
// outside this envelope. Tool identities, schemas, descriptions, policy fields
// and aliases remain part of its content identity. Map keys and tools are sorted.
func canonicalToolCatalog(data json.RawMessage) (string, json.RawMessage, int, int, error) {
	var input struct {
		Tools   []json.RawMessage `json:"tools"`
		Aliases map[string]string `json:"aliases"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return "", nil, 0, 0, err
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(data, &present); err != nil {
		return "", nil, 0, 0, err
	}
	if _, ok := present["tools"]; !ok {
		return "", nil, 0, 0, errors.New("catalog has no tools")
	}
	tools := make([]json.RawMessage, 0, len(input.Tools))
	for _, raw := range input.Tools {
		var object map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&object); err != nil || object == nil {
			return "", nil, 0, 0, errors.New("invalid catalog tool")
		}
		canonical, err := json.Marshal(object)
		if err != nil {
			return "", nil, 0, 0, err
		}
		tools = append(tools, canonical)
	}
	sort.Slice(tools, func(i, j int) bool { return bytes.Compare(tools[i], tools[j]) < 0 })
	if input.Aliases == nil {
		input.Aliases = map[string]string{}
	}
	body, err := json.Marshal(map[string]any{"format": toolCatalogFormat, "tools": tools, "aliases": input.Aliases})
	if err != nil {
		return "", nil, 0, 0, err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), body, len(tools), len(input.Aliases), nil
}
