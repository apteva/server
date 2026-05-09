package main

// install_method.go — detect how apteva-server got onto disk so the
// dashboard's update banner can show the right upgrade copy.
//
// Mirrors apteva/update.go::detectInstallMethod conceptually, but
// from the SERVER's perspective: we don't know whether the user
// invoked `apteva` or systemd did, but we can inspect our own
// process tree, our binary path, and a few well-known env vars
// the supervisor sets, and decide.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// detectInstallMethod is the dashboard-facing classifier. Returns
// one of: foreground, systemd-user, systemd-system, launchd-user,
// launchd-system, docker, source, packaged. Stable lower-case
// strings — the dashboard switches on them.
func detectInstallMethod() string {
	// Docker: the canonical "are we in a container" test.
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}

	// Source build: our binary's parent dir has sibling `core/`,
	// `app-sdk/`, `dashboard/` — the build-local.sh layout.
	if isSourceBuild() {
		return "source"
	}

	// Packaged: dpkg/rpm/pacman claim ownership. Cheapest tells
	// before we spend an exec on package-manager queries.
	if isPackageManaged() {
		return "packaged"
	}

	// Supervisor flavor: systemd sets INVOCATION_ID + JOURNAL_STREAM
	// for any unit it spawns. launchd sets XPC_SERVICE_NAME (=our
	// label) and the per-process LaunchService env. Both are far
	// cheaper than walking /proc/1/comm and survive container init
	// edge cases.
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("JOURNAL_STREAM") != "" {
		// Systemd: distinguish user vs system by EUID and by
		// XDG_RUNTIME_DIR (set on user-mode units).
		if os.Geteuid() != 0 || os.Getenv("XDG_RUNTIME_DIR") != "" {
			return "systemd-user"
		}
		return "systemd-system"
	}
	if name := os.Getenv("XPC_SERVICE_NAME"); strings.Contains(name, "ai.apteva") {
		// launchd. LaunchAgents run as the user, LaunchDaemons as
		// root. EUID is the unambiguous signal.
		if os.Geteuid() == 0 {
			return "launchd-system"
		}
		return "launchd-user"
	}

	return "foreground"
}

// isSourceBuild walks up the binary path looking for sibling repo
// dirs that the build-local.sh layout produces. Same heuristic the
// CLI uses, but applied from the server's vantage point so the
// dashboard's banner correctly reads "git pull + rebuild" on a dev
// machine.
func isSourceBuild() bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	resolved, _ := filepath.EvalSymlinks(self)
	if resolved == "" {
		resolved = self
	}
	dir := filepath.Dir(resolved)
	parent := filepath.Dir(dir)
	for _, sib := range []string{"core", "app-sdk", "dashboard"} {
		if info, err := os.Stat(filepath.Join(parent, sib)); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// isPackageManaged shells out to dpkg/rpm/pacman to check whether
// the binary is owned by an installed package. Best-effort; any
// negative answer means "no". We don't bail aggressively because
// false positives would hide the in-app update button on a
// homebrew-tarball install just because dpkg happens to be on
// $PATH.
func isPackageManaged() bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	resolved, _ := filepath.EvalSymlinks(self)
	if resolved == "" {
		resolved = self
	}
	for _, probe := range [][]string{
		{"dpkg", "-S", resolved},
		{"rpm", "-qf", resolved},
		{"pacman", "-Qo", resolved},
	} {
		if _, err := exec.LookPath(probe[0]); err != nil {
			continue
		}
		out, err := exec.Command(probe[0], probe[1:]...).Output()
		if err == nil && len(out) > 0 {
			return true
		}
	}
	return false
}

// suppress unused-import warnings under build flags
var _ = runtime.GOOS
var _ = strconv.Itoa
