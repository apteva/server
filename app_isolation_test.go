package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// fakeIsolationHost pretends the server is root on Linux and records chowns
// instead of performing them, so the layout can be checked without root.
func fakeIsolationHost(t *testing.T) map[string][2]int {
	t.Helper()
	var mu sync.Mutex
	chowned := map[string][2]int{}
	prevOK, prevChown := appIsolationPlatformOK, appIsolationLchown
	appIsolationPlatformOK = func() bool { return true }
	appIsolationLchown = func(path string, uid, gid int) error {
		mu.Lock()
		defer mu.Unlock()
		chowned[path] = [2]int{uid, gid}
		return nil
	}
	t.Cleanup(func() { appIsolationPlatformOK, appIsolationLchown = prevOK, prevChown })
	return chowned
}

func TestAppIsolationModeAndIdentity(t *testing.T) {
	t.Setenv("APTEVA_APP_ISOLATION", "")
	if appIsolationMode() != "off" {
		t.Fatal("isolation must be opt-in")
	}
	if iso, err := appIsolationFor("tasks", 7); err != nil || iso.Enabled {
		t.Fatalf("off mode must run as the server user: %+v %v", iso, err)
	}

	t.Setenv("APTEVA_APP_ISOLATION", "uid")
	prev := appIsolationPlatformOK
	appIsolationPlatformOK = func() bool { return false }
	if appIsolationMode() != "unavailable" {
		t.Fatal("requested isolation on an incapable host must report unavailable")
	}
	if _, err := appIsolationFor("tasks", 7); err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("must fail closed, got %v", err)
	}
	appIsolationPlatformOK = prev

	fakeIsolationHost(t)
	a, err := appIsolationFor("tasks", 7)
	if err != nil || !a.Enabled || a.UID != defaultAppUIDBase+7 || a.GID != a.UID {
		t.Fatalf("identity: %+v %v", a, err)
	}
	b, _ := appIsolationFor("storage", 8)
	if a.UID == b.UID {
		t.Fatal("two installs must never share a user")
	}
	if _, err := appIsolationFor("tasks", 0); err == nil {
		t.Fatal("an install without id must not get a guessable shared user")
	}
	t.Setenv("APTEVA_APP_UID_BASE", "1000")
	if _, err := appIsolationFor("tasks", 7); err == nil {
		t.Fatal("a base inside the login-user range must be rejected")
	}
	t.Setenv("APTEVA_APP_UID_BASE", "2147483000")
	if _, err := appIsolationFor("tasks", 9999); err == nil {
		t.Fatal("uid overflow must be rejected")
	}
	t.Setenv("APTEVA_APP_UID_BASE", "")
	t.Setenv("APTEVA_APP_ISOLATION_EXEMPT", " Containers , deploy")
	if iso, err := appIsolationFor("containers", 3); err != nil || iso.Enabled {
		t.Fatalf("exempt app must run as the server user: %+v %v", iso, err)
	}
}

func TestAppIsolationAppliesCredentialsWithoutDroppingProcessGroup(t *testing.T) {
	cmd := exec.Command("/bin/true")
	appIsolation{}.apply(cmd)
	if cmd.SysProcAttr != nil {
		t.Fatal("disabled isolation must not touch the command")
	}
	cmd = exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	appIsolation{Enabled: true, UID: 200007, GID: 200007}.apply(cmd)
	c := cmd.SysProcAttr.Credential
	if c == nil || c.Uid != 200007 || c.Gid != 200007 || len(c.Groups) != 0 || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("credential not applied cleanly: %+v", cmd.SysProcAttr)
	}
}

func TestAppIsolationLayoutKeepsInstallsApart(t *testing.T) {
	t.Setenv("APTEVA_APP_ISOLATION", "uid")
	chowned := fakeIsolationHost(t)
	// The OS temp dir's own ancestors are private on macOS; the ancestor
	// rule has its own test below.
	prevAncestors := appIsolationAncestors
	appIsolationAncestors = func(string) error { return nil }
	t.Cleanup(func() { appIsolationAncestors = prevAncestors })
	base := t.TempDir()
	apps := filepath.Join(base, "apps")
	verDir := filepath.Join(apps, "tasks", "1.2.0")
	data := filepath.Join(apps, "tasks", "data", "7")
	for _, d := range []string{verDir, filepath.Join(data, "old")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(verDir, "tasks")
	log := filepath.Join(verDir, "stderr.log")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700)
	_ = os.WriteFile(log, []byte("secret-ish log line\n"), 0o644)
	_ = os.WriteFile(filepath.Join(data, "app.db"), []byte("db"), 0o644)
	pid := filepath.Join(apps, ".runtime-pids", "dep-1.pid")

	iso, err := appIsolationFor("tasks", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareAppIsolationLayout(iso, apps, bin, data, log, pid); err != nil {
		t.Fatal(err)
	}
	mode := func(p string) os.FileMode {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	for path, want := range map[string]os.FileMode{
		apps: 0o711, filepath.Join(apps, "tasks"): 0o711, verDir: 0o711, filepath.Join(apps, "tasks", "data"): 0o711,
		data: 0o700, bin: 0o755, log: 0o600, filepath.Dir(pid): 0o711, pid: 0o600,
	} {
		if got := mode(path); got != want {
			t.Errorf("%s: mode %04o, want %04o", strings.TrimPrefix(path, base), got, want)
		}
	}
	uid := int(iso.UID)
	for _, p := range []string{data, filepath.Join(data, "app.db"), filepath.Join(data, "old"), pid} {
		if got, ok := chowned[p]; !ok || got != [2]int{uid, uid} {
			t.Errorf("%s not handed to the app user: %v", strings.TrimPrefix(p, base), got)
		}
	}
	for _, p := range []string{apps, verDir, bin, log} {
		if _, ok := chowned[p]; ok {
			t.Errorf("%s must stay owned by the server", strings.TrimPrefix(p, base))
		}
	}
}

func TestAppIsolationRefusesUnreachableAppsRoot(t *testing.T) {
	t.Setenv("APTEVA_APP_ISOLATION", "uid")
	fakeIsolationHost(t)
	private := filepath.Join(t.TempDir(), "home")
	apps := filepath.Join(private, "apps")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(private, 0o755) })
	iso, _ := appIsolationFor("tasks", 7)
	err := prepareAppIsolationLayout(iso, apps, filepath.Join(apps, "tasks", "bin"), filepath.Join(apps, "tasks", "data", "7"), "", "")
	if err == nil || !strings.Contains(err.Error(), "not traversable") {
		t.Fatalf("must explain the unreachable ancestor instead of changing it: %v", err)
	}
	if mode, _ := os.Stat(private); mode.Mode().Perm() != 0o700 {
		t.Fatal("operator-owned ancestor was modified")
	}
}

func TestTightenServerDataPermissionsProtectsPlatformFiles(t *testing.T) {
	dir := t.TempDir()
	apps := filepath.Join(dir, "apps")
	_ = os.MkdirAll(apps, 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "backups"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "apteva.db"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".secret"), []byte("x"), 0o600)
	_ = os.Symlink("/etc/hosts", filepath.Join(dir, "link"))
	changed, err := tightenServerDataPermissions(dir, apps)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(changed, ",") != "apteva.db,backups" {
		t.Fatalf("changed=%v", changed)
	}
	for name, want := range map[string]os.FileMode{"apteva.db": 0o600, "backups": 0o700, ".secret": 0o600, "apps": 0o755} {
		info, _ := os.Stat(filepath.Join(dir, name))
		if info.Mode().Perm() != want {
			t.Errorf("%s: %04o want %04o", name, info.Mode().Perm(), want)
		}
	}
	if info, _ := os.Stat("/etc/hosts"); info.Mode().Perm()&0o004 == 0 {
		t.Fatal("symlink target was followed and changed")
	}
}

// TestAppIsolationRealUsersLinux runs real processes as two app users on a
// real layout. It needs Linux and root (CI containers, or
// `go test -c` + docker run); elsewhere it is skipped.
func TestAppIsolationRealUsersLinux(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs Linux and root")
	}
	t.Setenv("APTEVA_APP_ISOLATION", "uid")
	base, err := os.MkdirTemp("/tmp", "iso")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	_ = os.Chmod(base, 0o755)
	dataDir := filepath.Join(base, "home")
	apps := filepath.Join(dataDir, "apps")
	_ = os.MkdirAll(apps, 0o755)
	_ = os.Chmod(dataDir, 0o755)
	platformDB := filepath.Join(dataDir, "apteva.db")
	_ = os.WriteFile(platformDB, []byte("platform"), 0o644)
	_ = os.WriteFile(filepath.Join(dataDir, ".secret"), []byte("server-secret"), 0o644)
	if _, err := tightenServerDataPermissions(dataDir, apps); err != nil {
		t.Fatal(err)
	}
	type install struct {
		iso  appIsolation
		data string
	}
	setup := func(name string, id int64) install {
		ver := filepath.Join(apps, name, "1.0.0")
		_ = os.MkdirAll(ver, 0o755)
		bin := filepath.Join(ver, name)
		_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
		data := filepath.Join(apps, name, "data", strconv.FormatInt(id, 10))
		_ = os.MkdirAll(data, 0o755)
		_ = os.WriteFile(filepath.Join(data, "app.db"), []byte(name+"-private"), 0o644)
		iso, err := appIsolationFor(name, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepareAppIsolationLayout(iso, apps, bin, data, filepath.Join(ver, "stderr.log"), filepath.Join(apps, ".runtime-pids", name+".pid")); err != nil {
			t.Fatal(err)
		}
		return install{iso, data}
	}
	a, b := setup("tasks", 7), setup("storage", 8)
	run := func(in install, script string) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Dir = in.data
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		in.iso.apply(cmd)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run(a, "id -u; id -G"); err != nil || out != "200007\n200007" {
		t.Fatalf("app runs as %q (%v), want its own uid/gid and no extra groups", out, err)
	}
	if out, err := run(a, "cat app.db && echo && echo new > own.txt && cat own.txt"); err != nil || out != "tasks-private\nnew" {
		t.Fatalf("app cannot use its own data: %q %v", out, err)
	}
	for what, path := range map[string]string{
		"another app's database": filepath.Join(b.data, "app.db"),
		"the platform database":  platformDB,
		"the server secret":      filepath.Join(dataDir, ".secret"),
	} {
		if out, err := run(a, "cat "+path); err == nil {
			t.Fatalf("app read %s: %q", what, out)
		}
	}
	if out, err := run(a, "ls "+filepath.Join(apps, "storage", "data")); err == nil {
		t.Fatalf("app listed other installs: %q", out)
	}
	if out, err := run(a, "echo 4242 > "+filepath.Join(apps, ".runtime-pids", "tasks.pid")); err != nil {
		t.Fatalf("launcher cannot write its pid file: %q %v", out, err)
	}
	if out, err := run(b, "echo x > "+filepath.Join(apps, ".runtime-pids", "tasks.pid")); err == nil {
		t.Fatalf("another app overwrote a pid file: %q", out)
	}
}
