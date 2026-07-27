package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

const generatedCredentialBytes = 32

// materializeGeneratedConnectionCredentials adds server-owned secret fields
// declared by an integration catalog entry. Generated fields are never
// accepted from the request, even if a client submits a value for a hidden
// field. They live in the connection's normal encrypted credential blob and
// are available only through the existing bound-app credential gate.
func materializeGeneratedConnectionCredentials(app *AppTemplate, supplied map[string]string) (map[string]string, error) {
	generated := make(map[string]bool)
	if app != nil {
		for _, field := range app.Auth.CredentialFields {
			if field.Source == "generated" && field.Name != "" {
				generated[field.Name] = true
			}
		}
	}
	credentials := make(map[string]string, len(supplied)+len(generated))
	for key, value := range supplied {
		if generated[key] {
			continue
		}
		credentials[key] = value
	}
	return backfillGeneratedConnectionCredentials(app, credentials)
}

// backfillGeneratedConnectionCredentials adds only missing generated fields.
// It supports connections created before a catalog introduced a generated
// secret without rotating values that are already in use.
func backfillGeneratedConnectionCredentials(app *AppTemplate, existing map[string]string) (map[string]string, error) {
	credentials := make(map[string]string, len(existing))
	for key, value := range existing {
		credentials[key] = value
	}
	if app == nil {
		return credentials, nil
	}
	for _, field := range app.Auth.CredentialFields {
		if field.Source != "generated" || field.Name == "" || credentials[field.Name] != "" {
			continue
		}
		raw := make([]byte, generatedCredentialBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generate %s: %w", field.Name, err)
		}
		credentials[field.Name] = base64.RawURLEncoding.EncodeToString(raw)
	}
	return credentials, nil
}

func hasMissingGeneratedConnectionCredentials(app *AppTemplate, credentials map[string]string) bool {
	if app == nil {
		return false
	}
	for _, field := range app.Auth.CredentialFields {
		if field.Source == "generated" && field.Name != "" && credentials[field.Name] == "" {
			return true
		}
	}
	return false
}
