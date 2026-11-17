//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeProcessHelper(t *testing.T) {
	if os.Getenv("APTEVA_PROCESS_TEST_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func processTestChild(t testing.TB) (*exec.Cmd, string, <-chan error) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestRuntimeProcessHelper$")
	cmd.Args[0] = "/untrusted-argv-is-not-the-executable"
	cmd.Env = append(os.Environ(), "APTEVA_PROCESS_TEST_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	return cmd, bin, done
}

func TestRuntimeProcessIdentityKernelExecutableAndBirth(t *testing.T) {
	cmd, bin, _ := processTestChild(t)
	identity, err := readRuntimeProcessIdentity(cmd.Process.Pid)
	if err != nil || !identity.matchesBinary(bin) || !runtimeProcessMatches(cmd.Process.Pid, bin) {
		t.Fatalf("identity unavailable: %+v %v", identity, err)
	}
	for _, bad := range []string{"", "relative", "/bin/sh"} {
		if runtimeProcessMatches(cmd.Process.Pid, bad) {
			t.Fatalf("accepted unrelated executable %q", bad)
		}
	}
	link := filepath.Join(t.TempDir(), "app")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	if !runtimeProcessMatches(cmd.Process.Pid, link) {
		t.Fatal("canonical executable symlink rejected")
	}
	other := identity
	other.birth += "-different"
	if identity.sameProcess(other) {
		t.Fatal("PID reuse was accepted")
	}
	p := &localProc{cmd: cmd, identity: &other}
	if err := signalLocalProcHandle(p, syscall.SIGTERM); err == nil || !processAlive(cmd.Process.Pid) {
		t.Fatal("mismatched process lifetime was signalled")
	}
	if runtimeProcessMatches(-1, bin) || runtimeProcessMatches(1<<30, bin) {
		t.Fatal("absent process matched")
	}
	if runtimeProcessGone(syscall.EPERM) || runtimeProcessGone(syscall.EIO) {
		t.Fatal("unknown identity error treated as confirmed exit")
	}
}

func TestRuntimeProcessExitNotification(t *testing.T) {
	cmd, _, _ := processTestChild(t)
	watch, err := watchRuntimeProcessExit(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("native exit subscription: %v", err)
	}
	defer watch.close()
	if exited, err := watch.wait(20 * time.Millisecond); exited || err != nil {
		t.Fatalf("live process reported exited: %v %v", exited, err)
	}
	done := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			exited, err := watch.wait(time.Until(deadline))
			if exited || err != nil {
				done <- err
				return
			}
		}
		done <- fmt.Errorf("exit event was not delivered")
	}()
	start := time.Now()
	_ = cmd.Process.Kill()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("exit was not notified promptly")
	}
	t.Logf("native process exit notification: %s", time.Since(start))
}

func BenchmarkRuntimeProcessIdentity(b *testing.B) {
	pid := os.Getpid()
	bin, _ := os.Executable()
	for _, method := range []string{"kernel", "legacy_ps"} {
		b.Run(method, func(b *testing.B) {
			var selfBefore, childBefore, selfAfter, childAfter syscall.Rusage
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &selfBefore)
			_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &childBefore)
			for i := 0; i < b.N; i++ {
				if method == "kernel" {
					if !runtimeProcessMatches(pid, bin) {
						b.Fatal("identity check failed")
					}
				} else if _, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output(); err != nil {
					b.Fatal(err)
				}
			}
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &selfAfter)
			_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &childAfter)
			cpu := func(r syscall.Rusage) float64 {
				return float64(r.Utime.Sec+r.Stime.Sec)*1e9 + float64(r.Utime.Usec+r.Stime.Usec)*1e3
			}
			child := cpu(childAfter) - cpu(childBefore)
			b.ReportMetric(child/float64(b.N), "child-cpu-ns/op")
			b.ReportMetric((cpu(selfAfter)-cpu(selfBefore)+child)/float64(b.N), "total-cpu-ns/op")
		})
	}
}
