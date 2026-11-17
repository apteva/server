package main

import (
	"os"
	"sort"
	"strings"
)

// child_env.go — the environment a server-started workload may inherit.
//
// The server process routinely carries deployment credentials: SERVER_SECRET
// (the platform-wide encryption key), host control credentials, cloud and
// provider keys for unrelated apps. Workloads used to inherit all of it and
// relied on a short denylist. They now start from an explicit allowlist:
//
//   - OS/runtime basics every process needs (PATH, locale, TLS roots, proxies);
//   - non-secret platform settings that several first-party apps read;
//   - for app sidecars only, variables prefixed with that app's own name
//     (FLEET_* for fleet, A2A_* for a2a, MEDIA_DOWNLOADER_* for
//     media-downloader), so an app keeps its operator configuration without
//     seeing a sibling's master key;
//   - operator extensions in APTEVA_CHILD_ENV_ALLOW (comma separated; "NAME",
//     "PREFIX_*", or "app:NAME" / "app:PREFIX_*" for one app).
//
// APTEVA_CHILD_ENV_MODE=inherit restores the legacy whole-environment
// behaviour for an installation that has not yet moved its settings, but the
// never-forward names below are stripped in every mode.

// childEnvNeverForward is stripped in every mode, including inherit.
var childEnvNeverForward = []string{"SERVER_SECRET", "APTEVA_CHILD_ENV_ALLOW", "APTEVA_HOST_*",
	"APTEVA_DELEGATED_DNS_*", "INSTANCE_SECRET", "AGENT_SECRET", "APTEVA_SECURITY_*"}

var childEnvBaseNames = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LANGUAGE", "TZ", "TMPDIR", "TMP", "TEMP",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "SYSTEMROOT", "PROGRAMFILES", "LOCALAPPDATA",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "DISPLAY",
	"LC_*",
}

// Non-secret platform settings shared by several first-party apps.
var childEnvAppSharedNames = []string{
	"APTEVA_PUBLIC_URL", "APTEVA_PUBLIC_HOST", "APTEVA_LAN_IP", "APTEVA_HEADLESS_MODE", "APTEVA_HEADLESS_BROWSER",
	"APTEVA_CHROME_PATH", "APTEVA_CHROME_FLAGS", "APTEVA_CHROME_NO_SANDBOX", "APTEVA_CHROME_DEFAULT_WIN",
	"APTEVA_ACME_EMAIL", "APTEVA_ACME_CACHE_DIR", "APTEVA_RUNTIME_MODE", "APTEVA_FUNCTIONS_*", "APTEVA_COMPUTER_*",
	"APTEVA_SCREENSHOT_QUALITY", "CHROME_BIN", "CHROMIUM_BIN", "FFMPEG_PATH", "FFPROBE_PATH", "NODE_BIN",
	"NPM_REGISTRY", "DOCKER_HOST", "EPUBCHECK_JAR",
}

// Toolchain settings for source builds (go/bun). Explicit names only: a
// "GO" prefix would also match GOOGLE_* credentials.
var childEnvBuildNames = []string{
	"GOPATH", "GOROOT", "GOPROXY", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GONOSUMCHECK", "GOSUMDB",
	"GOINSECURE", "GOFLAGS", "GOTOOLCHAIN", "GOCACHE", "GOMODCACHE", "GOENV", "GOWORK", "GOAMD64", "GOARM",
	"BUN_INSTALL", "NPM_CONFIG_REGISTRY",
}

func envMatches(name, pattern string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return name == pattern
}

func envMatchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if envMatches(name, p) {
			return true
		}
	}
	return false
}

// appEnvPrefix maps an app name to the prefix of its own variables.
func appEnvPrefix(appName string) string {
	name := strings.ToUpper(strings.TrimSpace(appName))
	if name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String() + "_"
}

// operatorChildEnvAllow returns the operator patterns that apply to appName
// ("" means non-app workloads, which only get unscoped patterns).
func operatorChildEnvAllow(appName string) []string {
	var out []string
	for _, raw := range strings.Split(os.Getenv("APTEVA_CHILD_ENV_ALLOW"), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if scope, pattern, ok := strings.Cut(raw, ":"); ok {
			if appName != "" && strings.EqualFold(strings.TrimSpace(scope), appName) {
				out = append(out, strings.TrimSpace(pattern))
			}
			continue
		}
		out = append(out, raw)
	}
	return out
}

type childEnvKind int

const (
	childEnvApp childEnvKind = iota
	childEnvBuild
	childEnvTool
)

// childBaseEnv filters base (normally os.Environ()) for one workload.
func childBaseEnv(base []string, kind childEnvKind, appName string) []string {
	inherit := strings.EqualFold(strings.TrimSpace(os.Getenv("APTEVA_CHILD_ENV_MODE")), "inherit")
	allow := append([]string(nil), childEnvBaseNames...)
	switch kind {
	case childEnvApp:
		allow = append(allow, childEnvAppSharedNames...)
		if prefix := appEnvPrefix(appName); prefix != "" && prefix != "APTEVA_" {
			allow = append(allow, prefix+"*")
		}
	case childEnvBuild:
		allow = append(allow, childEnvBuildNames...)
	}
	allow = append(allow, operatorChildEnvAllow(appName)...)
	out := make([]string, 0, len(base))
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" || envMatchesAny(name, childEnvNeverForward) {
			continue
		}
		if inherit || envMatchesAny(name, allow) {
			out = append(out, entry)
		}
	}
	return out
}

// withheldChildEnvNames lists inherited names a workload did not receive, so
// an operator can see which settings need an APTEVA_CHILD_ENV_ALLOW entry.
// Values are never returned.
func withheldChildEnvNames(base, passed []string) []string {
	kept := map[string]bool{}
	for _, entry := range passed {
		name, _, _ := strings.Cut(entry, "=")
		kept[name] = true
	}
	var out []string
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if name != "" && !kept[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
