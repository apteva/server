package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/apteva/server/internal/host"
	"os"
)

func validateHostedSchema(path, expected string) error {
	for _, suffix := range []string{".recovery.json", ".restored.marker"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("a restore is pending; deployment must not consume restore markers")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	actual, err := updateDatabaseSchema(path)
	if err != nil {
		return err
	}
	if expected != "" && actual != expected {
		return fmt.Errorf("live schema changed after preflight; deployment was not activated")
	}
	return nil
}
func hostedDiskIdentity(db, data string) (host.DatabaseIdentity, error) {
	var identity host.DatabaseIdentity
	if err := validateHostedSchema(db, ""); err != nil {
		return identity, err
	}
	schema, err := updateDatabaseSchema(db)
	if err != nil {
		return identity, err
	}
	key, err := LoadExistingSecret(data)
	if err != nil {
		return identity, err
	}
	sum := sha256.Sum256(key)
	identity = host.DatabaseIdentity{Version: CLIVersion, SchemaHash: schema, SecretHash: hex.EncodeToString(sum[:])}
	return identity, nil
}
