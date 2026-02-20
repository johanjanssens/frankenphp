package memory

import (
	"github.com/shirou/gopsutil/v4/mem"
)

// TotalSysMemory returns available system memory in bytes
func TotalSysMemory() (uint64, error) {
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}

	return vmStat.Available, nil
}
