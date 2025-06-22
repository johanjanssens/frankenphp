package cpu

import (
	"github.com/shirou/gopsutil/v4/load"
	"runtime"
)

var cpuCount = runtime.GOMAXPROCS(0)

// ProbeLoad checks if the system load average is below a threshold relative to available CPUs
// Returns true if load is NOT too high
// Returns false if system is already under heavy load
func ProbeLoad(maxLoadFactor float64) bool {

	// Get current load average immediately
	loadStat, err := load.Avg()
	if err != nil {
		return false // Error getting load, don't scale
	}

	// Check if 1-minute load average is below threshold
	// maxLoadFactor is relative to available CPU count (e.g., 0.7 means 70% of max capacity)
	maxLoad := float64(cpuCount) * maxLoadFactor

	// Return true if load is below threshold (scaling is recommended)
	// Return false if load is already high (avoid scaling)
	return loadStat.Load1 < maxLoad
}
