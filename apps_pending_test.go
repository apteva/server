package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestIsRecoverableLocalPending(t *testing.T) {
	platform := localPlatform()
	cases := []struct {
		name string
		m    sdk.Manifest
		want bool
	}{
		{
			name: "source kind",
			m: sdk.Manifest{Runtime: sdk.Runtime{
				Kind:   "source",
				Source: &sdk.SourceSpec{Repo: "github.com/apteva/apps"},
			}},
			want: true,
		},
		{
			name: "source spec on service kind",
			m: sdk.Manifest{Runtime: sdk.Runtime{
				Kind:   "service",
				Source: &sdk.SourceSpec{Repo: "github.com/apteva/apps"},
			}},
			want: true,
		},
		{
			name: "binary for current platform",
			m: sdk.Manifest{Runtime: sdk.Runtime{
				Kind:     "service",
				Binaries: map[string]string{platform: "https://example.test/app"},
			}},
			want: true,
		},
		{
			name: "static app",
			m: sdk.Manifest{Runtime: sdk.Runtime{
				Kind: "static",
			}},
			want: false,
		},
		{
			name: "manual remote-only app",
			m: sdk.Manifest{Runtime: sdk.Runtime{
				Kind:  "service",
				Image: "example/app:latest",
			}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRecoverableLocalPending(&tc.m); got != tc.want {
				t.Fatalf("isRecoverableLocalPending()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestMarkInstallRunningOnPreviousVersionClearsPending(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()

	installID := seedRunningInstall(t, s, "social", "", sdk.Manifest{
		Name:    "social",
		Version: "0.14.11",
		Runtime: sdk.Runtime{
			Kind: "source",
		},
	}, nil)
	if _, err := s.store.db.Exec(
		`UPDATE app_installs
		 SET status='pending', status_message='Queued — waiting for a build slot', pending_manifest_json='{"version":"0.14.12"}'
		 WHERE id=?`, installID,
	); err != nil {
		t.Fatalf("set pending: %v", err)
	}

	s.markInstallRunningOnPreviousVersion(installID, errors.New("build failed"))

	var status, statusMessage, errorMessage, pendingManifest, runningManifest string
	if err := s.store.db.QueryRow(
		`SELECT status, status_message, error_message, pending_manifest_json, manifest_json FROM app_installs WHERE id=?`,
		installID,
	).Scan(&status, &statusMessage, &errorMessage, &pendingManifest, &runningManifest); err != nil {
		t.Fatalf("read install: %v", err)
	}
	if status != "running" {
		t.Fatalf("status=%q, want running", status)
	}
	if !strings.Contains(statusMessage, "previous version still running") {
		t.Fatalf("status_message=%q", statusMessage)
	}
	if errorMessage != "build failed" {
		t.Fatalf("error_message=%q, want build failed", errorMessage)
	}
	if pendingManifest != "" {
		t.Fatalf("pending manifest not cleared after rollback: %s", pendingManifest)
	}
	var running sdk.Manifest
	if err := json.Unmarshal([]byte(runningManifest), &running); err != nil || running.Version != "0.14.11" {
		t.Fatalf("running manifest changed after rollback: version=%q err=%v", running.Version, err)
	}
	if got := s.installedApps.Get(installID); got == nil {
		t.Fatalf("expected install %d to be reloaded into registry", installID)
	}
}
