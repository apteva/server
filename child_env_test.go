package main

import (
	"slices"
	"strings"
	"testing"
)

func envNames(env []string) []string {
	var out []string
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		out = append(out, name)
	}
	return out
}

var serverEnvFixture = []string{
	"PATH=/usr/bin", "HOME=/root", "LC_ALL=C.UTF-8", "SSL_CERT_FILE=/etc/ssl/cert.pem",
	"SERVER_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	"APTEVA_HOST_PROXY_SECRET=hostsecret", "APTEVA_HOST_CONTROL_TOKEN=hosttoken",
	"APTEVA_DELEGATED_DNS_TOKEN=dns", "INSTANCE_SECRET=inst",
	"FLEET_MASTER_KEY=fleet-key", "A2A_MASTER_KEY=a2a-key", "MEDIA_DOWNLOADER_SECRET=md",
	"OPENAI_API_KEY=sk-openai", "GOOGLE_API_KEY=google", "AWS_SECRET_ACCESS_KEY=aws",
	"GOPROXY=https://proxy.golang.org", "GOFLAGS=-mod=mod",
	"APTEVA_PUBLIC_URL=https://example.test", "CHROME_BIN=/usr/bin/chromium",
	"BROWSERBASE_API_KEY=bb",
}

func TestChildEnvNeverForwardsPlatformSecrets(t *testing.T) {
	for _, mode := range []string{"", "inherit"} {
		t.Setenv("APTEVA_CHILD_ENV_MODE", mode)
		t.Setenv("APTEVA_CHILD_ENV_ALLOW", "SERVER_SECRET,APTEVA_HOST_*,fleet:SERVER_SECRET")
		for _, kind := range []childEnvKind{childEnvApp, childEnvBuild, childEnvTool} {
			got := envNames(childBaseEnv(serverEnvFixture, kind, "fleet"))
			for _, banned := range []string{"SERVER_SECRET", "APTEVA_HOST_PROXY_SECRET", "APTEVA_HOST_CONTROL_TOKEN", "APTEVA_DELEGATED_DNS_TOKEN", "INSTANCE_SECRET"} {
				if slices.Contains(got, banned) {
					t.Fatalf("mode=%q kind=%d forwarded %s: %v", mode, kind, banned, got)
				}
			}
		}
	}
}

func TestChildEnvScopesAppVariablesToTheirOwnApp(t *testing.T) {
	t.Setenv("APTEVA_CHILD_ENV_MODE", "")
	t.Setenv("APTEVA_CHILD_ENV_ALLOW", "")
	fleet := envNames(childBaseEnv(serverEnvFixture, childEnvApp, "fleet"))
	if !slices.Contains(fleet, "FLEET_MASTER_KEY") {
		t.Fatalf("fleet lost its own key: %v", fleet)
	}
	for _, sibling := range []string{"A2A_MASTER_KEY", "MEDIA_DOWNLOADER_SECRET", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "BROWSERBASE_API_KEY"} {
		if slices.Contains(fleet, sibling) {
			t.Fatalf("fleet received %s: %v", sibling, fleet)
		}
	}
	for _, shared := range []string{"PATH", "HOME", "LC_ALL", "SSL_CERT_FILE", "APTEVA_PUBLIC_URL", "CHROME_BIN"} {
		if !slices.Contains(fleet, shared) {
			t.Fatalf("fleet missing shared %s: %v", shared, fleet)
		}
	}
	md := envNames(childBaseEnv(serverEnvFixture, childEnvApp, "media-downloader"))
	if !slices.Contains(md, "MEDIA_DOWNLOADER_SECRET") || slices.Contains(md, "FLEET_MASTER_KEY") {
		t.Fatalf("media-downloader scoping wrong: %v", md)
	}
}

func TestChildEnvOperatorAllowlist(t *testing.T) {
	t.Setenv("APTEVA_CHILD_ENV_MODE", "")
	t.Setenv("APTEVA_CHILD_ENV_ALLOW", " browser:BROWSERBASE_* , OPENAI_API_KEY")
	browser := envNames(childBaseEnv(serverEnvFixture, childEnvApp, "browser"))
	if !slices.Contains(browser, "BROWSERBASE_API_KEY") || !slices.Contains(browser, "OPENAI_API_KEY") {
		t.Fatalf("operator allowlist not applied: %v", browser)
	}
	other := envNames(childBaseEnv(serverEnvFixture, childEnvApp, "storage"))
	if slices.Contains(other, "BROWSERBASE_API_KEY") {
		t.Fatalf("app-scoped allow leaked to another app: %v", other)
	}
	if tool := envNames(childBaseEnv(serverEnvFixture, childEnvTool, "")); slices.Contains(tool, "BROWSERBASE_API_KEY") {
		t.Fatalf("app-scoped allow leaked to a tool: %v", tool)
	}
}

func TestChildEnvBuildGetsToolchainSettingsOnly(t *testing.T) {
	t.Setenv("APTEVA_CHILD_ENV_MODE", "")
	t.Setenv("APTEVA_CHILD_ENV_ALLOW", "")
	build := envNames(childBaseEnv(serverEnvFixture, childEnvBuild, ""))
	if !slices.Contains(build, "GOPROXY") || !slices.Contains(build, "GOFLAGS") {
		t.Fatalf("build lost toolchain settings: %v", build)
	}
	for _, banned := range []string{"GOOGLE_API_KEY", "FLEET_MASTER_KEY", "OPENAI_API_KEY", "APTEVA_PUBLIC_URL"} {
		if slices.Contains(build, banned) {
			t.Fatalf("build received %s: %v", banned, build)
		}
	}
}

func TestChildEnvInheritModeKeepsLegacyNames(t *testing.T) {
	t.Setenv("APTEVA_CHILD_ENV_MODE", "inherit")
	got := envNames(childBaseEnv(serverEnvFixture, childEnvApp, "fleet"))
	if !slices.Contains(got, "OPENAI_API_KEY") || !slices.Contains(got, "A2A_MASTER_KEY") {
		t.Fatalf("inherit mode should keep legacy names: %v", got)
	}
}

func TestWithheldChildEnvNamesListsNamesOnly(t *testing.T) {
	t.Setenv("APTEVA_CHILD_ENV_MODE", "")
	t.Setenv("APTEVA_CHILD_ENV_ALLOW", "")
	passed := childBaseEnv(serverEnvFixture, childEnvApp, "fleet")
	withheld := withheldChildEnvNames(serverEnvFixture, passed)
	joined := strings.Join(withheld, ",")
	if !strings.Contains(joined, "SERVER_SECRET") || !strings.Contains(joined, "A2A_MASTER_KEY") {
		t.Fatalf("withheld list incomplete: %v", withheld)
	}
	if strings.Contains(joined, "a2a-key") || strings.Contains(joined, "=") {
		t.Fatalf("withheld list contains values: %v", withheld)
	}
}
