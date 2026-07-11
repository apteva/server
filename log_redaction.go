package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

func shortSecretRef(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}

func sortedAnyMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
