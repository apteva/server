package main

import (
	"bytes"
	"context"
	"database/sql"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func runtimePackageBuildEnvironment(work string) []string {
	// Never pass a Server/process token, configuration, cloud credential or
	// service environment into released build scripts.
	env := []string{"GOWORK=off", "GOTOOLCHAIN=auto", "CGO_ENABLED=0", "GOENV=off", "HOME=" + filepath.Join(work, "home"), "TMPDIR=" + work}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
func runtimePackageGit(ctx context.Context, dir string, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(bounded, "git", args...)
	cmd.Dir = dir
	// Git can use the operator's existing repository authentication, but output
	// is never forwarded: Git errors can echo credential-bearing remote URLs.
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("package Git %s failed", args[0])
	}
	return strings.TrimSpace(string(out)), nil
}
func (m *appRuntimeManager) clonePackageSource(dest string, source *sdk.SourceSpec, revision string) (string, error) {
	ref := source.Ref
	if revision != "" {
		ref = revision
	}
	if ref == "" {
		ref = "main"
	}
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\r\n\x00") {
		return "", fmt.Errorf("invalid source reference")
	}
	if _, err := runtimePackageGit(m.ctx, "", cloneArgs(normalizeRepoURL(source.Repo), dest, ref)...); err != nil {
		return "", err
	}
	if source.Entry != "" && source.Entry != "." {
		if !runtimePackagePath(source.Entry) {
			return "", fmt.Errorf("invalid source entry")
		}
		if _, err := runtimePackageGit(m.ctx, dest, "sparse-checkout", "init", "--cone"); err != nil {
			return "", err
		}
		if _, err := runtimePackageGit(m.ctx, dest, "sparse-checkout", "set", source.Entry); err != nil {
			return "", err
		}
	}
	if _, err := runtimePackageGit(m.ctx, dest, "checkout", "--detach", refExpr(ref)); err != nil {
		return "", err
	}
	sha, err := runtimePackageGit(m.ctx, dest, "rev-parse", "HEAD")
	if err != nil || !isLikelySHA(sha) || len(sha) != 40 {
		return "", fmt.Errorf("cannot resolve exact source commit")
	}
	return sha, nil
}
func runtimePackageRecipe(appRoot string, requested *runtimePackageSpec, manifest sdk.Manifest, revision string) (runtimePackageSpec, error) {
	var spec runtimePackageSpec
	if manifest.Runtime.Package != nil {
		raw, _ := json.Marshal(manifest.Runtime.Package)
		if err := json.Unmarshal(raw, &spec); err != nil {
			return spec, err
		}
	}
	raw, err := os.ReadFile(filepath.Join(appRoot, "apteva.runtime-package.json"))
	if err == nil {
		if manifest.Runtime.Package != nil {
			return spec, fmt.Errorf("release has conflicting package metadata sources; use runtime.package")
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if len(raw) > 1<<20 || dec.Decode(&spec) != nil {
			return spec, fmt.Errorf("invalid release package descriptor")
		}
	} else if !os.IsNotExist(err) {
		return spec, err
	}
	if requested != nil {
		if requested.Platform != "" {
			spec.Platform = requested.Platform
		}
		if requested.SourceRevision != "" {
			spec.SourceRevision = requested.SourceRevision
		}
		if requested.AppCapabilities != nil {
			spec.AppCapabilities = requested.AppCapabilities
		}
		if requested.Files != nil {
			spec.Files = requested.Files
		}
		if requested.Build != nil {
			spec.Build = requested.Build
		}
		if requested.CGO {
			spec.CGO = true
		}
		if requested.ABI != "" {
			spec.ABI = requested.ABI
		}
		if requested.SystemLibraries != nil {
			spec.SystemLibraries = requested.SystemLibraries
		}
	}
	if spec.SourceRevision != "" && spec.SourceRevision != revision {
		return spec, fmt.Errorf("release recipe source revision mismatch")
	}
	if spec.AppCapabilities == nil {
		for _, route := range manifest.Provides.HTTPRoutes {
			if strings.HasPrefix(route.Prefix, runtimeControlPrefix+"app") {
				return spec, fmt.Errorf("managed app requires exact-release app_capabilities in apteva.runtime-package.json or the administrator's package declaration")
			}
		}
		spec.AppCapabilities = &runtimeAppCaps{}
	}
	if spec.Files == nil {
		if info, err := os.Lstat(filepath.Join(appRoot, "native")); err == nil && info.IsDir() {
			spec.Files = []string{"native"}
		}
	}
	if spec.Build == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if raw, err := os.ReadFile(filepath.Join(appRoot, "package.json")); err == nil {
			if json.Unmarshal(raw, &pkg) != nil {
				return spec, fmt.Errorf("invalid release package.json")
			}
			for _, script := range []string{"build:ui", "build:frontend"} {
				if pkg.Scripts[script] != "" {
					spec.Build = []runtimePackageCommand{{Args: []string{"bun", "install", "--frozen-lockfile", "--ignore-scripts"}}, {Args: []string{"bun", "run", script}}}
					break
				}
			}
		}
	}
	return spec, validateRuntimePackageSpec(spec)
}
func runtimePackageToolVersion(ctx context.Context, program, appRoot string, env []string, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(bounded, program, args...)
	cmd.Dir, cmd.Env = appRoot, env
	out, err := cmd.Output()
	if err != nil || len(out) > 1024 {
		return "", fmt.Errorf("package build tool version unavailable")
	}
	return strings.TrimSpace(string(out)), nil
}
func runtimePackageManifestMatches(root string, want sdk.Manifest) error {
	raw, err := os.ReadFile(filepath.Join(root, "apteva.yaml"))
	if err != nil {
		return fmt.Errorf("package source manifest missing")
	}
	got, err := sdk.ParseManifest(raw)
	if err != nil {
		return fmt.Errorf("invalid package source manifest")
	}
	// The resolved commit can replace a tag reference, and release archives may
	// advertise their own download map. All other permissions/runtime fields must
	// agree with the approved installation manifest.
	got.Runtime.Artifacts, want.Runtime.Artifacts = nil, nil
	if got.Runtime.Source != nil && want.Runtime.Source != nil {
		copySource := *got.Runtime.Source
		copySource.Ref = want.Runtime.Source.Ref
		got.Runtime.Source = &copySource
	}
	// Stored manifests have crossed a JSON boundary (numbers in untyped schema
	// maps become float64). Compare their canonical wire forms, not Go's YAML
	// versus JSON implementation types.
	gotJSON, gotErr := json.Marshal(got)
	wantJSON, wantErr := json.Marshal(want)
	if gotErr != nil || wantErr != nil || !bytes.Equal(gotJSON, wantJSON) {
		return fmt.Errorf("package source manifest differs from approved release")
	}
	return nil
}
func runtimePackageCheckELF(filename, platform string) error {
	_, _, err := runtimePackageELFRequirements(filename, platform, nil)
	return err
}
func runtimePackageELFRequirements(filename, platform string, spec *runtimePackageSpec) (string, []string, error) {
	f, err := elf.Open(filename)
	if err != nil {
		return "", nil, fmt.Errorf("prepared executable is not Linux ELF")
	}
	defer f.Close()
	want := elf.EM_X86_64
	if platform == "linux-arm64" {
		want = elf.EM_AARCH64
	}
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != want || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) || (f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX) {
		return "", nil, fmt.Errorf("prepared executable architecture mismatch")
	}
	var abi string
	for _, segment := range f.Progs {
		if segment.Type == elf.PT_INTERP {
			data, err := io.ReadAll(io.LimitReader(segment.Open(), 4096))
			if err != nil {
				return "", nil, fmt.Errorf("cannot inspect executable interpreter")
			}
			interpreter := strings.TrimRight(string(data), "\x00")
			gnu := "/lib64/ld-linux-x86-64.so.2"
			musl := "/lib/ld-musl-x86_64.so.1"
			if platform == "linux-arm64" {
				gnu = "/lib/ld-linux-aarch64.so.1"
				musl = "/lib/ld-musl-aarch64.so.1"
			}
			switch interpreter {
			case gnu:
				abi = "gnu"
			case musl:
				abi = "musl"
			default:
				return "", nil, fmt.Errorf("unsupported target executable interpreter")
			}
		}
	}
	libraries, err := f.ImportedLibraries()
	if err != nil {
		return "", nil, fmt.Errorf("cannot inspect target library requirements")
	}
	sort.Strings(libraries)
	if abi != "" && (spec == nil || spec.ABI != abi) {
		return "", nil, fmt.Errorf("dynamic executable requires explicit target ABI %s and approved system libraries", abi)
	}
	for _, lib := range libraries {
		if spec == nil || (spec.SystemLibraries != nil && !runtimeContains(spec.SystemLibraries, lib)) {
			return "", nil, fmt.Errorf("dynamic executable requires declared system library %s", lib)
		}
	}
	return abi, libraries, nil
}
func copyRuntimePackageResource(sourceRoot, destRoot, rel string, files map[string]runtimePackageFile, size *int64) error {
	source, err := runtimePackageContained(sourceRoot, rel)
	if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !runtimePackagePath(relative) || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe package runtime file")
		}
		if entry.IsDir() {
			return nil
		}
		if _, exists := files[relative]; exists {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > runtimePackageFileLimit {
			return fmt.Errorf("unsupported package runtime file")
		}
		*size += info.Size()
		if *size > runtimePackageLimit || len(files) >= 1024 {
			return fmt.Errorf("package exceeds delivery budget")
		}
		target := filepath.Join(destRoot, relative)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		in, err := os.Open(filename)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()&0755)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(out, io.LimitReader(in, info.Size()+1))
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil || n != info.Size() {
			return fmt.Errorf("runtime file changed while packaging")
		}
		digest, err := artifactFileHash(target)
		if err != nil {
			return err
		}
		files[relative] = runtimePackageFile{Path: relative, SHA256: digest, Size: n}
		return nil
	})
}
func (m *appRuntimeManager) buildRuntimePackage(d *runtimeDeployment, op *runtimeOperation, delivery *runtimePackageDeliveryCaps) (runtimePreparedPackage, error) {
	var result runtimePreparedPackage
	root, err := m.packageCacheRoot()
	if err != nil {
		return result, err
	}
	if d.PackagePreparation != nil && d.PackagePreparation.BuildKey != "" {
		if result, err = m.cachedRuntimePackage(d.PackagePreparation.BuildKey); err == nil {
			return result, nil
		} else if err != sql.ErrNoRows {
			return result, err
		}
	}
	work, cleanup, err := runtimePackageWorkspace(m.ctx, root)
	if err != nil {
		return result, err
	}
	defer cleanup()
	if err = os.MkdirAll(filepath.Join(work, "home"), 0700); err != nil {
		return result, err
	}
	platform := ""
	if d.PackagePreparation != nil {
		platform = d.PackagePreparation.Platform
	} else if d.Placement.Package != nil {
		platform = d.Placement.Package.Platform
	}
	if _, _, err = runtimePackagePlatform(platform); err != nil {
		return result, err
	}
	var appRoot, binary, revision, archiveSHA string
	if artifact, ok := d.Manifest.Runtime.Artifacts[platform]; ok {
		if !delivery.VerifiedArchives {
			return result, fmt.Errorf("backend does not support verified archive-to-package handoff; original archive verification cannot be removed")
		}
		archiveSHA = strings.ToLower(artifact.SHA256)
		if d.PackagePreparation != nil && d.PackagePreparation.ArchiveSHA256 != archiveSHA {
			return result, fmt.Errorf("deployment archive identity changed")
		}
		binary, err = m.s.localApps.fetchAppArtifactForPlatform(&d.Manifest, platform, nil)
		if err != nil {
			return result, err
		}
		appRoot = artifactAppRoot(&d.Manifest, binary)
	} else {
		if len(d.Manifest.Runtime.Artifacts) > 0 {
			return result, fmt.Errorf("release has no verified artifact for requested package platform")
		}
		if d.Manifest.Runtime.Source == nil {
			return result, fmt.Errorf("binary package needs source or a checksummed platform archive")
		}
		pinned := ""
		if d.PackagePreparation != nil {
			pinned = d.PackagePreparation.SourceRevision
		}
		repo := filepath.Join(work, "source")
		revision, err = m.clonePackageSource(repo, d.Manifest.Runtime.Source, pinned)
		if err != nil {
			return result, err
		}
		appRoot = repo
		if entry := d.Manifest.Runtime.Source.Entry; entry != "" && entry != "." {
			appRoot, err = runtimePackageContained(repo, entry)
			if err != nil {
				return result, err
			}
		}
	}
	if d.PackagePreparation == nil {
		d.PackagePreparation = &runtimePackagePreparation{Platform: platform, SourceRevision: revision, ArchiveSHA256: archiveSHA}
		if err = m.checkOwner(*op); err != nil {
			return result, err
		}
		if err = m.saveDeployment(*d); err != nil {
			return result, err
		}
	}
	if err = runtimePackageManifestMatches(appRoot, d.Manifest); err != nil {
		return result, err
	}
	spec, err := runtimePackageRecipe(appRoot, d.Placement.Package, d.Manifest, revision)
	if err != nil {
		return result, err
	}
	if spec.Platform != platform {
		return result, fmt.Errorf("package target platform changed")
	}
	osName, arch, _ := runtimePackagePlatform(platform)
	env := runtimePackageBuildEnvironment(work)
	env = append(env, "GOOS="+osName, "GOARCH="+arch)
	var goVersion, bunVersion string
	if archiveSHA == "" {
		goBin, e := resolveGoBinaryWithProgress(appRoot, env, nil)
		if e != nil {
			return result, fmt.Errorf("package Go toolchain unavailable")
		}
		goVersion, err = runtimePackageToolVersion(m.ctx, goBin, appRoot, env, "version")
		if err != nil {
			return result, err
		}
		for _, step := range spec.Build {
			if step.Args[0] == "bun" && bunVersion == "" {
				bunVersion, err = runtimePackageToolVersion(m.ctx, "bun", appRoot, env, "--version")
				if err != nil {
					return result, err
				}
			}
		}
	}
	prep := runtimePackagePreparation{Platform: platform, SourceRevision: revision, ArchiveSHA256: archiveSHA, GoVersion: goVersion, BunVersion: bunVersion, Spec: spec}
	prep.BuildKey = runtimeHash([]byte(runtimeJSON(struct {
		Manifest sdk.Manifest
		Inputs   runtimePackagePreparation
		Builder  string
	}{d.Manifest, prep, "native-binary-v1:" + localPlatform()})))
	if d.PackagePreparation != nil && d.PackagePreparation.BuildKey != "" && runtimeJSON(*d.PackagePreparation) != runtimeJSON(prep) {
		return result, fmt.Errorf("pinned package build inputs changed; create a new deployment instead of rebuilding different bytes")
	}
	d.PackagePreparation = &prep
	if err = m.checkOwner(*op); err != nil {
		return result, err
	}
	if err = m.saveDeployment(*d); err != nil {
		return result, err
	}
	unlock, err := runtimePackageLock(m.ctx, filepath.Join(root, prep.BuildKey+".lock"))
	if err != nil {
		return result, err
	}
	defer unlock()
	if result, err = m.cachedRuntimePackage(prep.BuildKey); err == nil {
		return result, nil
	} else if err != sql.ErrNoRows {
		return result, err
	}
	if err = m.phase(op, "building_package"); err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp(work, ".package-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(stage)
	if archiveSHA == "" {
		for _, step := range spec.Build {
			if err = m.checkOwner(*op); err != nil {
				return result, err
			}
			dir := appRoot
			if step.Dir != "" && step.Dir != "." {
				dir, err = runtimePackageContained(appRoot, step.Dir)
				if err != nil {
					return result, err
				}
			}
			bounded, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
			cmd := exec.CommandContext(bounded, step.Args[0], step.Args[1:]...)
			cmd.Dir, cmd.Env = dir, env
			// Do not persist build output: scripts can print arbitrary source data.
			err = cmd.Run()
			cancel()
			if err != nil {
				return result, fmt.Errorf("package preparation step failed")
			}
		}
		binary = filepath.Join(work, "app")
		if err = goBuildInEnvironment(appRoot, ".", binary, filepath.Join(root, ".go-build"), env, nil, env, m.ctx); err != nil {
			return result, fmt.Errorf("target Go build failed (CGO and target library requirements must be satisfied)")
		}
	}
	abi, libraries, err := runtimePackageELFRequirements(binary, platform, &spec)
	if err != nil {
		return result, err
	}
	files := map[string]runtimePackageFile{}
	var size int64
	if err = copyRuntimePackageResource(filepath.Dir(binary), stage, filepath.Base(binary), files, &size); err != nil {
		return result, err
	}
	// Normalize the executable slot; it is independent of a backend/app name.
	executable := filepath.Base(binary)
	resources := []string{"apteva.yaml"}
	if _, err = os.Lstat(filepath.Join(appRoot, "ui")); err == nil {
		resources = append(resources, "ui")
	}
	if d.Manifest.DB != nil && d.Manifest.DB.Migrations != "" {
		resources = append(resources, strings.TrimSuffix(filepath.ToSlash(d.Manifest.DB.Migrations), "/"))
	}
	for _, skill := range d.Manifest.Provides.Skills {
		if skill.BodyFile != "" {
			resources = append(resources, skill.BodyFile)
		}
	}
	resources = append(resources, spec.Files...)
	for _, rel := range resources {
		if rel == executable {
			return result, fmt.Errorf("runtime resource conflicts with executable")
		}
		if err = copyRuntimePackageResource(appRoot, stage, rel, files, &size); err != nil {
			return result, err
		}
	}
	if abi != "" {
		// Carry actual runtime prerequisites in a sealed file as well as the
		// deployment reference. Backends must validate/provide them or startup
		// readiness will fail; Server does not silently install host libraries.
		contract := []byte(runtimeJSON(struct {
			ABI       string   `json:"abi"`
			Libraries []string `json:"system_libraries"`
		}{abi, libraries}))
		name := "apteva-runtime-libraries.json"
		if _, ok := files[name]; ok {
			return result, fmt.Errorf("reserved runtime library descriptor conflict")
		}
		if err = os.WriteFile(filepath.Join(stage, name), contract, 0600); err != nil {
			return result, err
		}
		files[name] = runtimePackageFile{Path: name, SHA256: runtimeHash(contract), Size: int64(len(contract))}
	}
	meta := runtimePackageMetadata{AppName: d.Manifest.Name, AppVersion: d.Manifest.Version, Platform: platform, Executable: executable, AppCapabilities: *spec.AppCapabilities}
	for _, f := range files {
		meta.Files = append(meta.Files, f)
	}
	sort.Slice(meta.Files, func(i, j int) bool { return meta.Files[i].Path < meta.Files[j].Path })
	digest := runtimePackageMetadataHash(meta)
	meta.ID = "pkg-" + digest
	result = runtimePreparedPackage{Metadata: meta, MetadataSHA256: digest, ExecutableSHA256: files[executable].SHA256, ArchiveSHA256: archiveSHA, SourceRevision: revision, BuildKey: prep.BuildKey, ABI: abi, SystemLibraries: libraries}
	if err = verifyRuntimePreparedPackage(stage, result); err != nil {
		return result, err
	}
	if err = m.checkOwner(*op); err != nil {
		return result, err
	}
	err = m.publishRuntimePackage(root, stage, result)
	return result, err
}
