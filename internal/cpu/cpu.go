package cpu

import (
	"os"
	"runtime"

	"github.com/shirou/gopsutil/v4/process"
)

// Use existing cpuCount from the package
var cpuCount = runtime.GOMAXPROCS(0)

// ProbeLoad checks if the current process CPU usage is below a threshold relative to available cores
// Returns true if process CPU usage is NOT too high
// Returns false if process is already under heavy CPU load
func ProbeLoad(maxLoadFactor float64) bool {
	// Get the current process
	pid := os.Getpid()
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return false // Error getting process, don't scale
	}

	// Get CPU percent for this process
	// Note: this returns percent across all cores, so 100% per core * number of cores is the max
	cpuPercent, err := proc.CPUPercent()
	if err != nil {
		return false // Error getting CPU usage, don't scale
	}

	// Calculate maximum CPU percentage allowed based on the factor and available cores
	// For example, with 4 cores and maxLoadFactor of 0.7, maxAllowedPercent would be 280%
	// (representing 70% utilization of all cores)
	maxAllowedPercent := float64(cpuCount) * 100.0 * maxLoadFactor

	// Return true if CPU usage is below threshold (scaling is recommended)
	// Return false if CPU usage is already high (avoid scaling)
	return cpuPercent < maxAllowedPercent
}
