// Package psi parses Linux Kernel Pressure Stall Information (PSI / cgroup v2)
// from /sys/fs/cgroup/.../cpu.pressure and memory.pressure to detect sub-second
// CPU micro-throttling and memory reclaim stalls ahead of Prometheus scrape lag.
package psi

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MetricLine represents either the "some" or "full" stall line in a Linux PSI file.
type MetricLine struct {
	Avg10  float64 `json:"avg10"`
	Avg60  float64 `json:"avg60"`
	Avg300 float64 `json:"avg300"`
	Total  uint64  `json:"total_microseconds"`
}

// PressureSnapshot holds both CPU and Memory PSI metrics for a cgroup or node.
type PressureSnapshot struct {
	CPUSome            MetricLine `json:"cpu_some"`
	CPUFull            MetricLine `json:"cpu_full"`
	MemSome            MetricLine `json:"mem_some"`
	MemFull            MetricLine `json:"mem_full"`
	CPUThrottlingSpike bool       `json:"cpu_throttling_spike"`
	MemReclaimDistress bool       `json:"mem_reclaim_distress"`
}

// ParsePressureContent parses the raw contents of a Linux cgroup v2 *.pressure file.
func ParsePressureContent(raw string) (some MetricLine, full MetricLine, err error) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) == 0 || strings.TrimSpace(raw) == "" {
		return some, full, fmt.Errorf("empty PSI content")
	}
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		var m MetricLine
		for _, tok := range fields[1:] {
			kv := strings.SplitN(tok, "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "avg10":
				m.Avg10, _ = strconv.ParseFloat(kv[1], 64)
			case "avg60":
				m.Avg60, _ = strconv.ParseFloat(kv[1], 64)
			case "avg300":
				m.Avg300, _ = strconv.ParseFloat(kv[1], 64)
			case "total":
				m.Total, _ = strconv.ParseUint(kv[1], 10, 64)
			}
		}
		switch fields[0] {
		case "some":
			some = m
		case "full":
			full = m
		}
	}
	return some, full, nil
}

// ReadCgroupPressure reads cpu.pressure and memory.pressure from a cgroup v2 directory.
func ReadCgroupPressure(cgroupDir string) (PressureSnapshot, error) {
	var snap PressureSnapshot
	cpuRaw, err := os.ReadFile(filepath.Join(cgroupDir, "cpu.pressure"))
	if err != nil {
		return snap, err
	}
	memRaw, err := os.ReadFile(filepath.Join(cgroupDir, "memory.pressure"))
	if err != nil {
		return snap, err
	}
	snap.CPUSome, snap.CPUFull, err = ParsePressureContent(string(cpuRaw))
	if err != nil {
		return snap, err
	}
	snap.MemSome, snap.MemFull, err = ParsePressureContent(string(memRaw))
	if err != nil {
		return snap, err
	}
	// Detect sub-second micro-throttling or memory thrashing
	snap.CPUThrottlingSpike = snap.CPUSome.Avg10 >= 15.0 || (snap.CPUSome.Avg10-snap.CPUSome.Avg60) >= 10.0
	snap.MemReclaimDistress = snap.MemFull.Avg10 >= 5.0 || snap.MemSome.Avg10 >= 20.0
	return snap, nil
}
