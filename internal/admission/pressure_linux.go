//go:build linux

package admission

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func number(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
func statValue(data, name string) float64 {
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == name {
			return number(f[1])
		}
	}
	return -1
}
func read(path string) string { b, _ := os.ReadFile(path); return string(b) }
func cgroupDirs() []string {
	dirs := []string{"/sys/fs/cgroup"}
	for _, line := range strings.Split(read("/proc/self/cgroup"), "\n") {
		if strings.HasPrefix(line, "0::") {
			dir := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
			for dir != "/sys/fs/cgroup" && dir != "/" {
				dirs = append(dirs, dir)
				dir = filepath.Dir(dir)
			}
		}
	}
	return dirs
}
func cpuSetCount(s string) int {
	n := 0
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		lo, hi, rangeOK := strings.Cut(part, "-")
		a, e := strconv.Atoi(lo)
		if e != nil || a < 0 {
			return 0
		}
		if !rangeOK {
			n++
			continue
		}
		b, e := strconv.Atoi(hi)
		if e != nil || b < a {
			return 0
		}
		n += b - a + 1
	}
	return n
}

// NewHostSampler observes enclosing container quotas as well as host CPU.
// Sampling is serialized and cached by the controller, not a polling goroutine.
func NewHostSampler() func() Pressure {
	var mu sync.Mutex
	var previousTotal, previousIdle float64
	last := time.Time{}
	usage := map[string]float64{}
	throttled := map[string]float64{}
	return func() Pressure {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		p := Pressure{CPUs: float64(runtime.NumCPU()), Busy: -1}
		dirs := cgroupDirs()
		for _, dir := range dirs {
			f := strings.Fields(read(filepath.Join(dir, "cpu.max")))
			if len(f) == 2 && f[0] != "max" && number(f[1]) > 0 {
				p.CPUs = math.Min(p.CPUs, number(f[0])/number(f[1]))
			}
			if n := cpuSetCount(read(filepath.Join(dir, "cpuset.cpus.effective"))); n > 0 {
				p.CPUs = math.Min(p.CPUs, float64(n))
			}
			limit := number(strings.TrimSpace(read(filepath.Join(dir, "memory.max"))))
			used := number(strings.TrimSpace(read(filepath.Join(dir, "memory.current"))))
			if limit > 0 && limit-used < math.Min(128<<20, limit*0.1) {
				p.MemoryLow = true
			}
			stats := read(filepath.Join(dir, "cpu.stat"))
			u := statValue(stats, "usage_usec")
			t := statValue(stats, "nr_throttled")
			if old, ok := usage[dir]; ok && !last.IsZero() && u >= old {
				p.Busy = math.Max(p.Busy, (u-old)/1e6/now.Sub(last).Seconds()/math.Max(0.1, p.CPUs))
			}
			if old, ok := throttled[dir]; ok && t > old {
				p.Throttled = true
			}
			if u >= 0 {
				usage[dir] = u
			}
			if t >= 0 {
				throttled[dir] = t
			}
		}
		lines := strings.Split(read("/proc/stat"), "\n")
		if len(lines) > 0 {
			f := strings.Fields(lines[0])
			if len(f) >= 5 {
				total := float64(0)
				for i := 1; i < len(f) && i <= 8; i++ {
					total += number(f[i])
				}
				idle := number(f[4])
				if len(f) > 5 {
					idle += number(f[5])
				}
				if previousTotal > 0 && total > previousTotal {
					p.Busy = math.Max(p.Busy, 1-(idle-previousIdle)/(total-previousTotal))
				}
				previousTotal = total
				previousIdle = idle
			}
		}
		mem := read("/proc/meminfo")
		available := statValue(mem, "MemAvailable:")
		total := statValue(mem, "MemTotal:")
		if available >= 0 && total > 0 && available < math.Min(128*1024, total*0.05) {
			p.MemoryLow = true
		}
		if p.Busy > 1 {
			p.Busy = 1
		}
		last = now
		return p
	}
}

// ProcessCPU returns cumulative CPU seconds including sandbox descendants when
// a worker cgroup is supplied, otherwise just the target process. -1 is unknown.
func ProcessCPU(pid int, cgroup string) float64 {
	if cgroup != "" {
		if u := statValue(read(filepath.Join(cgroup, "cpu.stat")), "usage_usec"); u >= 0 {
			return u / 1e6
		}
	}
	// Include live descendants: an app hosting worker processes must not look
	// idle while its children burn CPU. Cgroup counters above remain preferred.
	pending := []int{pid}
	seen := map[int]bool{}
	total := float64(0)
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[current] {
			continue
		}
		seen[current] = true
		if len(seen) > 256 {
			return -1
		} // telemetry work is bounded; fail conservative
		paths, _ := filepath.Glob("/proc/" + strconv.Itoa(current) + "/task/*/schedstat")
		if len(paths) == 0 {
			return -1
		}
		for _, path := range paths {
			fields := strings.Fields(read(path))
			if len(fields) == 0 {
				return -1
			}
			total += number(fields[0]) / 1e9
			for _, child := range strings.Fields(read(filepath.Join(filepath.Dir(path), "children"))) {
				id, err := strconv.Atoi(child)
				if err == nil {
					pending = append(pending, id)
				}
			}
		}
	}
	return total
}
