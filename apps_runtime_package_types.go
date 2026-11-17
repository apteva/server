package main

import (
	"fmt"
	"path"
	"strings"
)

const runtimePackageProtocol = "apteva.runtime-package/files-v1"
const runtimePackageLimit int64 = 512 << 20
const runtimePackageFileLimit int64 = 256 << 20
const runtimePackageCacheLimit int64 = 2 << 30

// Optional delivery extension to the existing backend protocol. A legacy
// implementation can be explicitly attested by an administrator at registration.
type runtimePackageDeliveryCaps struct {
	Protocol         string   `json:"protocol"`
	Formats          []string `json:"formats"`
	VerifiedArchives bool     `json:"verified_archives,omitempty"`
}

// Release-owned apteva.runtime-package.json supplies these build inputs. An
// administrator may supply the same declaration in placement.package. Neither
// declaration contains installation configuration or execution credentials.
type runtimePackageSpec struct {
	Platform        string                  `json:"platform,omitempty"`
	SourceRevision  string                  `json:"source_revision,omitempty"`
	AppCapabilities *runtimeAppCaps         `json:"app_capabilities,omitempty"`
	Files           []string                `json:"files,omitempty"`
	Build           []runtimePackageCommand `json:"build,omitempty"`
	CGO             bool                    `json:"cgo,omitempty"`
	ABI             string                  `json:"abi,omitempty"`
	SystemLibraries []string                `json:"system_libraries,omitempty"`
}
type runtimePackageCommand struct {
	Dir  string   `json:"dir,omitempty"`
	Args []string `json:"args"`
}
type runtimePackageFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type runtimePackageMetadata struct {
	ID              string               `json:"id"`
	AppName         string               `json:"app_name"`
	AppVersion      string               `json:"app_version"`
	Platform        string               `json:"platform"`
	Executable      string               `json:"executable"`
	Files           []runtimePackageFile `json:"files"`
	AppCapabilities runtimeAppCaps       `json:"app_capabilities"`
}
type runtimePackagePreparation struct {
	Platform       string             `json:"platform"`
	SourceRevision string             `json:"source_revision,omitempty"`
	ArchiveSHA256  string             `json:"archive_sha256,omitempty"`
	BuildKey       string             `json:"build_key"`
	GoVersion      string             `json:"go_version,omitempty"`
	BunVersion     string             `json:"bun_version,omitempty"`
	Spec           runtimePackageSpec `json:"spec"`
}
type runtimePreparedPackage struct {
	ABI              string                 `json:"abi,omitempty"`
	SystemLibraries  []string               `json:"system_libraries,omitempty"`
	Metadata         runtimePackageMetadata `json:"metadata"`
	MetadataSHA256   string                 `json:"metadata_sha256"`
	ExecutableSHA256 string                 `json:"executable_sha256"`
	ArchiveSHA256    string                 `json:"archive_sha256,omitempty"`
	SourceRevision   string                 `json:"source_revision,omitempty"`
	BuildKey         string                 `json:"build_key"`
}
type runtimePackageReference struct {
	ABI              string   `json:"abi,omitempty"`
	SystemLibraries  []string `json:"system_libraries,omitempty"`
	ID               string   `json:"id"`
	Platform         string   `json:"platform"`
	SHA256           string   `json:"sha256"`
	ExecutableSHA256 string   `json:"executable_sha256"`
	ArchiveSHA256    string   `json:"archive_sha256,omitempty"`
}

func validateRuntimePackageDelivery(c *runtimePackageDeliveryCaps, b runtimeBackendCaps) error {
	if c.Protocol != runtimePackageProtocol || len(c.Formats) > 8 || !runtimeContains(c.Formats, "binary") || !runtimeContains(b.Formats, "binary") {
		return fmt.Errorf("unsupported package delivery protocol or format")
	}
	for _, format := range c.Formats {
		if !runtimeContains(b.Formats, format) {
			return fmt.Errorf("package format is not advertised by backend")
		}
	}
	return nil
}
func runtimePackagePlatform(p string) (string, string, error) {
	parts := strings.Split(p, "-")
	if len(parts) != 2 || parts[0] != "linux" || (parts[1] != "amd64" && parts[1] != "arm64") {
		return "", "", fmt.Errorf("binary package platform must be linux-amd64 or linux-arm64")
	}
	return parts[0], parts[1], nil
}
func runtimePackagePath(v string) bool {
	if v == "" || len(v) > 240 || path.IsAbs(v) || path.Clean(v) != v || strings.Contains(v, "\\") {
		return false
	}
	for _, component := range strings.Split(v, "/") {
		if !runtimeID(component) || strings.HasPrefix(component, ".") || component == ".." {
			return false
		}
	}
	return true
}
func runtimeSystemLibraryName(lib string) bool {
	if lib == "" || len(lib) > 240 || strings.HasPrefix(lib, ".") {
		return false
	}
	for _, c := range lib {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c == '+') {
			return false
		}
	}
	return true
}
func validateRuntimePackageSpec(s runtimePackageSpec) error {
	if _, _, err := runtimePackagePlatform(s.Platform); err != nil {
		return err
	}
	if s.CGO {
		return fmt.Errorf("cross-CGO builds need a target-native builder or a verified prebuilt package; setting GOOS is insufficient")
	}
	if s.ABI != "" && s.ABI != "gnu" && s.ABI != "musl" {
		return fmt.Errorf("package ABI must be gnu or musl")
	}
	if len(s.SystemLibraries) > 32 {
		return fmt.Errorf("too many system libraries")
	}
	for _, lib := range s.SystemLibraries {
		if !runtimeSystemLibraryName(lib) {
			return fmt.Errorf("invalid system library name")
		}
	}
	if len(s.Files) > 128 || len(s.Build) > 16 {
		return fmt.Errorf("package recipe exceeds resource limits")
	}
	if s.SourceRevision != "" && (!isLikelySHA(s.SourceRevision) || len(s.SourceRevision) != 40) {
		return fmt.Errorf("source_revision must be an exact full Git commit")
	}
	for _, f := range s.Files {
		if !runtimePackagePath(f) {
			return fmt.Errorf("invalid runtime file path")
		}
	}
	for _, c := range s.Build {
		if c.Dir != "" && c.Dir != "." && !runtimePackagePath(c.Dir) || len(c.Args) == 0 || len(c.Args) > 32 {
			return fmt.Errorf("invalid package build step")
		}
		// Shell parsing is deliberately absent. Use released script files when
		// a recipe needs more elaborate preparation.
		if c.Args[0] != "bun" && c.Args[0] != "go" {
			return fmt.Errorf("package build steps must use bun or go")
		}
		for _, arg := range c.Args {
			if len(arg) > 1024 || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("invalid package build argument")
			}
		}
	}
	if s.AppCapabilities != nil && s.AppCapabilities.Routing != "" && s.AppCapabilities.Routing != "key" && s.AppCapabilities.Routing != "stateless" {
		return fmt.Errorf("invalid package routing capability")
	}
	return nil
}
