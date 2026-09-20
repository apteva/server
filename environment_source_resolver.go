package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
	sdk "github.com/apteva/app-sdk"
)

type environmentSourceCandidate struct {
	dir     string
	version string
	origin  string
}

func validateEnvironmentAppVersion(version, constraint string) error {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" {
		return nil
	}
	want, err := semver.NewConstraint(constraint)
	if err != nil {
		return fmt.Errorf("invalid version constraint %q: %w", constraint, err)
	}
	got, err := semver.NewVersion(strings.TrimSpace(version))
	if err != nil {
		return fmt.Errorf("version %q is not valid semver (required %s)", version, constraint)
	}
	if !want.Check(got) {
		return fmt.Errorf("version %s does not satisfy %s", version, constraint)
	}
	return nil
}

func environmentSourceDirForManifest(m *sdk.Manifest, binPath string) string {
	if m == nil || strings.TrimSpace(binPath) == "" {
		return ""
	}
	candidates := []string{artifactAppRoot(m, binPath)}
	// Older cached packages predate runtime.source.entry. Preserve their two
	// canonical layouts while the manifest-aware path above handles all new
	// packages and verified artifacts.
	versionDir := filepath.Dir(binPath)
	candidates = append(candidates,
		filepath.Join(versionDir, "src", "mcp", m.Name),
		filepath.Join(versionDir, "src"),
	)
	seen := map[string]bool{}
	for _, dir := range candidates {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if info, err := os.Stat(filepath.Join(dir, "apteva.yaml")); err == nil && !info.IsDir() {
			if abs, absErr := filepath.Abs(dir); absErr == nil {
				return abs
			}
			return dir
		}
	}
	return ""
}

func environmentManifestFromDir(dir string) (*sdk.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "apteva.yaml"))
	if err != nil {
		return nil, err
	}
	m, err := sdk.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.Name) == "" {
		return nil, fmt.Errorf("manifest has no name")
	}
	return m, nil
}

// resolveEnvironmentAppSource resolves a package without creating an install
// in the source project. Downloading or building a registry package may fill
// the shared immutable cache; only Environment.Create creates temporary install
// rows, all scoped to the environment id.
func (s *Server) resolveEnvironmentAppSource(projectID, name, constraint string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("app name required")
	}
	if strings.TrimSpace(constraint) != "" {
		if _, err := semver.NewConstraint(constraint); err != nil {
			return "", fmt.Errorf("invalid version constraint %q: %w", constraint, err)
		}
	}
	attempts := make([]string, 0, 6)

	// Same-project first, then global. Never inspect another project's package
	// as an environment dependency source.
	if s != nil && s.store != nil {
		query := `SELECT i.id, COALESCE(i.project_id,''), COALESCE(i.version,''),
				 COALESCE(i.local_bin_path,''), COALESCE(NULLIF(i.manifest_json,''),a.manifest_json)
			 FROM app_installs i JOIN apps a ON a.id=i.app_id
			 WHERE a.name=? AND i.status='running' AND (i.project_id=? OR i.project_id='')
			 ORDER BY CASE WHEN i.project_id=? THEN 0 ELSE 1 END, i.id DESC`
		rows, err := s.store.db.Query(query, name, projectID, projectID)
		if err == nil {
			for rows.Next() {
				var installID int64
				var scope, version, binPath, manifestJSON string
				if rows.Scan(&installID, &scope, &version, &binPath, &manifestJSON) != nil {
					continue
				}
				var manifest sdk.Manifest
				if json.Unmarshal([]byte(manifestJSON), &manifest) != nil {
					attempts = append(attempts, fmt.Sprintf("install %d (invalid manifest)", installID))
					continue
				}
				if manifest.Version != "" {
					version = manifest.Version
				}
				if err := validateEnvironmentAppVersion(version, constraint); err != nil {
					attempts = append(attempts, fmt.Sprintf("install %d version %s (incompatible)", installID, version))
					continue
				}
				if dir := environmentSourceDirForManifest(&manifest, binPath); dir != "" {
					rows.Close()
					return dir, nil
				}
				attempts = append(attempts, fmt.Sprintf("install %d version %s (cached source missing)", installID, version))
			}
			rows.Close()
		} else {
			attempts = append(attempts, "project/global installs (lookup failed: "+err.Error()+")")
		}
	}

	if candidate := s.resolveEnvironmentCachedSource(name, constraint); candidate.dir != "" {
		return candidate.dir, nil
	} else if candidate.origin != "" {
		attempts = append(attempts, candidate.origin)
	} else {
		attempts = append(attempts, "local app cache (no compatible package)")
	}

	var registryDir string
	var registryErr error
	if s != nil && s.environmentRegistrySource != nil {
		registryDir, registryErr = s.environmentRegistrySource(name, constraint)
	} else if s != nil {
		registryDir, registryErr = s.materializeEnvironmentRegistrySource(name, constraint)
	} else {
		registryErr = fmt.Errorf("server unavailable")
	}
	if registryErr == nil && registryDir != "" {
		return registryDir, nil
	}
	if registryErr != nil {
		attempts = append(attempts, "registry ("+registryErr.Error()+")")
	} else {
		attempts = append(attempts, "registry (no compatible package)")
	}

	if dir, err := defaultSourceResolver(name); err == nil {
		manifest, manifestErr := environmentManifestFromDir(dir)
		if manifestErr == nil && normalizeAppName(manifest.Name) == normalizeAppName(name) {
			if versionErr := validateEnvironmentAppVersion(manifest.Version, constraint); versionErr == nil {
				return dir, nil
			} else {
				attempts = append(attempts, "development checkout ("+versionErr.Error()+")")
			}
		} else if manifestErr != nil {
			attempts = append(attempts, "development checkout ("+manifestErr.Error()+")")
		}
	} else {
		attempts = append(attempts, "development checkout (not found)")
	}

	required := constraint
	if required == "" {
		required = "any version"
	}
	return "", fmt.Errorf("no compatible package for app %q (%s); attempted: %s", name, required, strings.Join(attempts, "; "))
}

func (s *Server) resolveEnvironmentCachedSource(name, constraint string) environmentSourceCandidate {
	if s == nil || s.localApps == nil || strings.TrimSpace(s.localApps.cacheDir) == "" {
		return environmentSourceCandidate{origin: "local app cache (unavailable)"}
	}
	root := filepath.Join(s.localApps.cacheDir, name)
	versionDirs, err := os.ReadDir(root)
	if err != nil {
		return environmentSourceCandidate{origin: "local app cache (not found)"}
	}
	candidates := make([]environmentSourceCandidate, 0)
	seen := map[string]bool{}
	for _, versionDir := range versionDirs {
		if !versionDir.IsDir() || !versionDirRE.MatchString(versionDir.Name()) {
			continue
		}
		base := filepath.Join(root, versionDir.Name())
		patterns := []string{
			filepath.Join(base, "src", "apteva.yaml"),
			filepath.Join(base, "src", "mcp", name, "apteva.yaml"),
			filepath.Join(base, "artifacts", "*", "src", "apteva.yaml"),
			filepath.Join(base, "artifacts", "*", "src", "mcp", name, "apteva.yaml"),
		}
		candidateCountBeforeVersion := len(candidates)
		for _, pattern := range patterns {
			matches, _ := filepath.Glob(pattern)
			for _, manifestPath := range matches {
				dir := filepath.Dir(manifestPath)
				if seen[dir] {
					continue
				}
				seen[dir] = true
				manifest, parseErr := environmentManifestFromDir(dir)
				if parseErr != nil || normalizeAppName(manifest.Name) != normalizeAppName(name) {
					continue
				}
				if validateEnvironmentAppVersion(manifest.Version, constraint) != nil {
					continue
				}
				candidates = append(candidates, environmentSourceCandidate{dir: dir, version: manifest.Version, origin: "local app cache"})
			}
		}
		// Packages may use any runtime.source.entry, not only the built-in
		// monorepo's mcp/<name> convention. Keep the common paths above fast,
		// then inspect bounded package source trees for a matching manifest.
		if len(candidates) == candidateCountBeforeVersion {
			roots := []string{filepath.Join(base, "src")}
			artifactRoots, _ := filepath.Glob(filepath.Join(base, "artifacts", "*", "src"))
			roots = append(roots, artifactRoots...)
			for _, sourceRoot := range roots {
				_ = filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return nil
					}
					rel, relErr := filepath.Rel(sourceRoot, path)
					if relErr != nil {
						return nil
					}
					depth := strings.Count(filepath.ToSlash(rel), "/")
					if entry.IsDir() {
						if rel != "." && (depth >= 8 || entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "vendor") {
							return filepath.SkipDir
						}
						return nil
					}
					if entry.Name() != "apteva.yaml" || seen[filepath.Dir(path)] {
						return nil
					}
					dir := filepath.Dir(path)
					manifest, parseErr := environmentManifestFromDir(dir)
					if parseErr != nil || normalizeAppName(manifest.Name) != normalizeAppName(name) || validateEnvironmentAppVersion(manifest.Version, constraint) != nil {
						return nil
					}
					seen[dir] = true
					candidates = append(candidates, environmentSourceCandidate{dir: dir, version: manifest.Version, origin: "local app cache"})
					return nil
				})
			}
		}
	}
	if len(candidates) == 0 {
		return environmentSourceCandidate{origin: "local app cache (no compatible package)"}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, leftErr := semver.NewVersion(candidates[i].version)
		right, rightErr := semver.NewVersion(candidates[j].version)
		if leftErr == nil && rightErr == nil {
			return left.GreaterThan(right)
		}
		return candidates[i].version > candidates[j].version
	})
	return candidates[0]
}

func (s *Server) materializeEnvironmentRegistrySource(name, constraint string) (string, error) {
	if s.localApps == nil {
		return "", fmt.Errorf("local app supervisor unavailable")
	}
	registry, err := s.fetchAndCacheRegistry()
	if err != nil {
		return "", err
	}
	manifestURL := ""
	for _, entry := range registry.Apps {
		if normalizeAppName(entry.Name) == normalizeAppName(name) {
			manifestURL = entry.ManifestURL
			break
		}
	}
	if manifestURL == "" {
		return "", fmt.Errorf("app not present in registry")
	}
	manifest, err := s.fetchAndCacheManifest(manifestURL)
	if err != nil {
		return "", fmt.Errorf("fetch manifest: %w", err)
	}
	if normalizeAppName(manifest.Name) != normalizeAppName(name) {
		return "", fmt.Errorf("registry manifest name %q does not match %q", manifest.Name, name)
	}
	if err := validateEnvironmentAppVersion(manifest.Version, constraint); err != nil {
		return "", err
	}
	binPath, err := s.localApps.BuildFromSourceBinary(manifest, nil)
	if err != nil {
		return "", fmt.Errorf("materialize package: %w", err)
	}
	dir := environmentSourceDirForManifest(manifest, binPath)
	if dir == "" {
		return "", fmt.Errorf("materialized package has no source resources")
	}
	return dir, nil
}
