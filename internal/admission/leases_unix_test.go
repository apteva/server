//go:build linux || darwin

package admission

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestSharedLeaseAcrossRuntimeReplacements(t *testing.T) {
	dir := t.TempDir()
	old, next := controlled(4), controlled(4)
	old.SetLeaseDirectory(dir)
	next.SetLeaseDirectory(dir)
	p, e := old.Acquire(context.Background(), request("install:7"))
	if e != nil {
		t.Fatal(e)
	}
	r := request("install:7")
	r.Wait = 20 * time.Millisecond
	if _, e := next.Acquire(context.Background(), r); code(e) != "adaptive_queue_timeout" {
		t.Fatal("replacement bypassed live runtime", e)
	}
	p.Finish(cpuResult())
	q, e := next.Acquire(context.Background(), r)
	if e != nil {
		t.Fatal("replacement did not recover slot", e)
	}
	q.Finish(cpuResult())
}
func TestLeaseCrashHelper(t *testing.T) {
	if os.Getenv("ADMISSION_LEASE_CHILD") == "" {
		return
	}
	c := controlled(1)
	c.SetLeaseDirectory(os.Getenv("ADMISSION_LEASE_CHILD"))
	_, e := c.Acquire(context.Background(), request("shared"))
	if e != nil {
		os.Exit(2)
	}
	fmt.Println("ready")
	for {
		time.Sleep(time.Hour)
	}
}
func TestSharedLeaseReleasedOnProcessCrash(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseCrashHelper$")
	cmd.Env = append(os.Environ(), "ADMISSION_LEASE_CHILD="+dir)
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatal(line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child not ready")
	}
	c := controlled(1)
	c.SetLeaseDirectory(dir)
	r := request("shared")
	r.Wait = 15 * time.Millisecond
	if _, e := c.Acquire(context.Background(), r); code(e) != "adaptive_queue_timeout" {
		t.Fatal("live child lease bypassed", e)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	p, e := c.Acquire(context.Background(), r)
	if e != nil {
		t.Fatal("crashed child leaked lease", e)
	}
	p.Finish(cpuResult())
}
