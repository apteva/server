//go:build !linux

package admission

import "runtime"

func NewHostSampler() func() Pressure {
	return func() Pressure { return Pressure{CPUs: float64(runtime.NumCPU()), Busy: -1} }
}
func ProcessCPU(pid int, cgroup string) float64 { return -1 }
