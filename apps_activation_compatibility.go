package main

import (
	"fmt"
	"strings"

	semver "github.com/Masterminds/semver/v3"
	sdk "github.com/apteva/app-sdk"
)

// Check at admission and again before activation so restored JSON manifests
// cannot silently lose their runtime contract. Development builds implement
// the current contract but have no release version to compare.
func validateAppRuntimeCompatibility(m *sdk.Manifest, serverVersion string) error {
	if m == nil {
		return fmt.Errorf("activation manifest required")
	}
	if err := sdk.ValidateActivationStrategy(m.Runtime); err != nil {
		return err
	}
	if m.MinAptevaVersion == "" {
		return nil
	}
	minimum, err := semver.NewVersion(strings.TrimSpace(m.MinAptevaVersion))
	if err != nil {
		return fmt.Errorf("invalid min_apteva_version %q: %w", m.MinAptevaVersion, err)
	}
	if serverVersion == "dev" {
		return nil
	}
	current, err := semver.NewVersion(strings.TrimSpace(serverVersion))
	if err != nil {
		return fmt.Errorf("cannot verify min_apteva_version: server version %q is invalid", serverVersion)
	}
	if current.LessThan(minimum) {
		return fmt.Errorf("app %s requires Apteva %s or newer (running %s)", m.Name, minimum, current)
	}
	return nil
}
