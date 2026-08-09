package main

// Agent Plugins 1.0.0 compatibility for the native Apteva Apps system.
//
// The portable package is additive: plugin.json, skills/, and mcp.json remain
// understandable to other clients, while com.apteva.manifest points Apteva to
// the existing apteva.yaml that owns runtime, permissions, UI, data, and app
// lifecycle. Existing apps without plugin.json follow the exact legacy path.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/app-sdk/agentplugin"
)

const maxAgentPluginDocument = 256 * 1024

// resolveAgentPluginManifestDocument turns a portable plugin.json URL into
// the native manifest selected by extensions.com.apteva.manifest. Non-plugin
// documents are returned byte-for-byte so all existing apteva.yaml callers
// retain their behavior.
func (s *Server) resolveAgentPluginManifestDocument(sourceURL string, document []byte) ([]byte, error) {
	if !looksLikeAgentPluginManifest(document) {
		return document, nil
	}
	plugin, issues, err := agentplugin.ParseManifest(document)
	if err != nil {
		return nil, fmt.Errorf("invalid Agent Plugin: %w", err)
	}
	for _, issue := range issues {
		log.Printf("[AGENT-PLUGIN] %s %s: %s", issue.Component, issue.Name, issue.Message)
	}
	ext, present, err := plugin.Apteva()
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("Agent Plugin %q has no %s.manifest extension; portable-only package installation is not supported by the Apps endpoint", plugin.Name, agentplugin.AptevaNamespace)
	}
	if strings.TrimSpace(sourceURL) == "" {
		return nil, errors.New("inline plugin.json cannot resolve com.apteva.manifest; provide a plugin URL")
	}
	manifestURL, err := resolvePluginSiblingURL(sourceURL, ext.Manifest)
	if err != nil {
		return nil, fmt.Errorf("resolve %s manifest: %w", agentplugin.AptevaNamespace, err)
	}
	manifestDocument, err := fetchAgentPluginDocument(manifestURL)
	if err != nil {
		return nil, err
	}
	manifest, err := sdk.ParseManifest(manifestDocument)
	if err != nil {
		return nil, fmt.Errorf("invalid Apteva manifest selected by plugin: %w", err)
	}
	if manifest.Name != plugin.Name {
		return nil, fmt.Errorf("plugin name %q does not match Apteva app name %q", plugin.Name, manifest.Name)
	}
	if plugin.Version != "" && plugin.Version != manifest.Version {
		return nil, fmt.Errorf("plugin version %q does not match Apteva app version %q", plugin.Version, manifest.Version)
	}
	return manifestDocument, nil
}

func looksLikeAgentPluginManifest(document []byte) bool {
	var envelope struct {
		Schema string `json:"$schema"`
	}
	return json.Unmarshal(document, &envelope) == nil && envelope.Schema == agentplugin.ManifestSchema
}

func fetchAgentPluginDocument(rawURL string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/x-yaml, text/yaml, text/plain, */*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: http %d", rawURL, resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, maxAgentPluginDocument+1)
	document, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if len(document) > maxAgentPluginDocument {
		return nil, fmt.Errorf("fetch %s: document exceeds %d bytes", rawURL, maxAgentPluginDocument)
	}
	return document, nil
}

func resolvePluginSiblingURL(pluginURL, relative string) (string, error) {
	base, err := url.Parse(pluginURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", errors.New("plugin URL must be absolute")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", errors.New("plugin URL must use HTTP or HTTPS")
	}
	if strings.Contains(relative, "\\") {
		return "", errors.New("extension path must use portable forward slashes")
	}
	reference, err := url.Parse(relative)
	if err != nil || reference.IsAbs() || reference.Host != "" || reference.RawQuery != "" || reference.Fragment != "" {
		return "", errors.New("extension path must be a relative path without query or fragment")
	}
	baseDir := *base
	if index := strings.LastIndex(baseDir.Path, "/"); index >= 0 {
		baseDir.Path = baseDir.Path[:index+1]
	} else {
		baseDir.Path = "/"
	}
	baseDir.RawPath = ""
	resolved := baseDir.ResolveReference(reference)
	if !strings.EqualFold(resolved.Scheme, base.Scheme) || !strings.EqualFold(resolved.Host, base.Host) {
		return "", errors.New("extension path changed origin")
	}
	// Compare decoded, cleaned URL paths. Escaped dot segments such as
	// %2e%2e must not bypass the same-root containment check.
	basePrefix := path.Clean(baseDir.Path)
	if !strings.HasSuffix(basePrefix, "/") {
		basePrefix += "/"
	}
	resolvedPath := path.Clean(resolved.Path)
	if resolvedPath != strings.TrimSuffix(basePrefix, "/") && !strings.HasPrefix(resolvedPath, basePrefix) {
		return "", errors.New("extension path escapes plugin root")
	}
	return resolved.String(), nil
}

// syncAgentPluginSkillsForInstall discovers portable Agent Skills from a
// checked-out source app and merges them with the native manifest skill list.
// Native declarations win on duplicate names so existing metadata/commands do
// not change. Invalid portable siblings are logged and isolated by the loader.
func (s *Server) syncAgentPluginSkillsForInstall(installID int64, manifest *sdk.Manifest, projectID, root string) error {
	if manifest == nil {
		return errors.New("manifest required")
	}
	merged := append([]sdk.Skill(nil), manifest.Provides.Skills...)
	seen := make(map[string]bool, len(merged))
	for _, skill := range merged {
		seen[skill.Name] = true
	}

	// Native skills always remain usable, even if an optional portable
	// package is malformed. This is important during upgrades: compatibility
	// metadata can never take an otherwise healthy Apteva app down or leave
	// it with stale native skills.
	var compatibilityErr error
	if _, err := os.Lstat(filepath.Join(root, "plugin.json")); err == nil {
		pkg, loadErr := agentplugin.LoadDir(root)
		if loadErr != nil {
			compatibilityErr = loadErr
		} else if pkg.Manifest.Name != manifest.Name {
			compatibilityErr = fmt.Errorf("plugin name %q does not match app name %q", pkg.Manifest.Name, manifest.Name)
		} else if pkg.Manifest.Version != "" && pkg.Manifest.Version != manifest.Version {
			compatibilityErr = fmt.Errorf("plugin version %q does not match app version %q", pkg.Manifest.Version, manifest.Version)
		} else if _, present, extensionErr := pkg.Manifest.Apteva(); extensionErr != nil {
			compatibilityErr = extensionErr
		} else if !present {
			compatibilityErr = fmt.Errorf("plugin has no %s.manifest extension", agentplugin.AptevaNamespace)
		} else {
			for _, issue := range pkg.Issues {
				log.Printf("[AGENT-PLUGIN] app=%s install=%d component=%s name=%s: %s",
					manifest.Name, installID, issue.Component, issue.Name, issue.Message)
			}
			for _, portable := range pkg.Skills {
				if seen[portable.Name] {
					continue
				}
				metadata := make(map[string]any, len(portable.Metadata)+3)
				for key, value := range portable.Metadata {
					metadata[key] = value
				}
				if portable.License != "" {
					metadata["license"] = portable.License
				}
				if portable.Compatibility != "" {
					metadata["compatibility"] = portable.Compatibility
				}
				if portable.AllowedTools != "" {
					metadata["allowed-tools"] = portable.AllowedTools
				}
				merged = append(merged, sdk.Skill{
					Name: portable.Name, Description: portable.Description,
					Body: portable.Body, Metadata: metadata,
				})
				seen[portable.Name] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		compatibilityErr = err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(rootAbs); resolveErr == nil {
		rootAbs = resolved
	}
	localFetcher := func(name string) (string, error) {
		candidate := filepath.Join(rootAbs, filepath.FromSlash(name))
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(rootAbs, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("skill body_file escapes app root")
		}
		data, err := os.ReadFile(resolved)
		return string(data), err
	}
	if len(merged) == 0 {
		if err := s.deleteAppSkillsForInstall(installID, "app skill removed"); err != nil {
			return err
		}
	} else if err := s.registerAppSkills(installID, manifest.Name, projectID, merged, localFetcher); err != nil {
		return err
	}
	return compatibilityErr
}
