package main

import "testing"

func TestResolveInstalledAppIcon(t *testing.T) {
	got := resolveInstalledAppIcon("computer", "/ui/icon.svg", "0.4.1", 34, "project-a")
	want := "/api/apps/computer/ui/icon.svg?install_id=34&project_id=project-a&v=0.4.1"
	if got != want {
		t.Fatalf("resolveInstalledAppIcon() = %q, want %q", got, want)
	}

	const remote = "https://cdn.example.com/computer.svg"
	if got := resolveInstalledAppIcon("computer", remote, "0.4.1", 34, "project-a"); got != remote {
		t.Fatalf("legacy remote URL changed: %q", got)
	}
}

func TestResolveMarketplaceAppIcon(t *testing.T) {
	got := resolveMarketplaceAppIcon(
		"https://raw.githubusercontent.com/apteva/apps/main/mcp/computer/apteva.yaml",
		"/ui/icon.svg",
	)
	want := "https://raw.githubusercontent.com/apteva/apps/main/mcp/computer/ui/icon.svg"
	if got != want {
		t.Fatalf("resolveMarketplaceAppIcon() = %q, want %q", got, want)
	}

	const remote = "https://cdn.example.com/computer.svg"
	if got := resolveMarketplaceAppIcon("https://example.com/apteva.yaml", remote); got != remote {
		t.Fatalf("legacy remote URL changed: %q", got)
	}
}
