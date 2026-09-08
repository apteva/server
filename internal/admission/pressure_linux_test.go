//go:build linux

package admission

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCPUSetParsing(t *testing.T) {
	for input, want := range map[string]int{"0": 1, "0-3": 4, "0-1,4,6-7": 5, "": 0, "3-1": 0, "bad": 0} {
		if got := cpuSetCount(input); got != want {
			t.Fatalf("%q: %d", input, got)
		}
	}
}
func TestLinuxPressureAndProcessCPU(t *testing.T) {
	sample := NewHostSampler()
	p := sample()
	if p.CPUs <= 0 {
		t.Fatal(p)
	}
	start := ProcessCPU(os.Getpid(), "")
	until := time.Now().Add(30 * time.Millisecond)
	for time.Now().Before(until) {
	}
	end := ProcessCPU(os.Getpid(), "")
	if start < 0 || end < start {
		t.Fatalf("CPU counters unavailable: %f %f", start, end)
	}
	t.Logf("Linux effective CPUs %.2f, CPU seconds %.4f", p.CPUs, end-start)
}
func TestLinuxSingleCPUConcurrentWork(t *testing.T) {
	if os.Getenv("ADMISSION_SINGLE_CPU_TEST") != "1" {
		t.Skip("run in the bounded one-CPU Linux container")
	}
	reader := NewHostSampler()
	if p := reader(); p.CPUs > 1.01 {
		t.Fatalf("container quota not detected: %+v", p)
	}
	c := New(reader)
	var peak, active atomic.Int32
	var wg sync.WaitGroup
	for _, key := range []string{"different-heavy-a", "different-heavy-b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			p, e := c.Acquire(context.Background(), request(key))
			if e != nil {
				t.Error(e)
				return
			}
			n := active.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			start := time.Now()
			until := start.Add(150 * time.Millisecond)
			for time.Now().Before(until) {
			}
			active.Add(-1)
			p.Finish(Result{Duration: time.Since(start), CPUSeconds: 0.15})
		}(key)
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatal("simultaneous heavy executions", peak.Load())
	}
	t.Log("Actual one-CPU Linux quota: two different CPU-heavy operations completed with peak concurrency 1")
}
