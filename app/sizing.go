package main

import "strings"

// sizing.go — a node's per-node sizing, applied to its container.

// nodeLimits is a node's per-node sizing as the design records it. designNode and the
// provisioner plan structs that stand in for it expose one via limits(), so a call site
// passes a single value rather than growing an argument per knob.
type nodeLimits struct {
	CPUs            int    // container --cpus
	MemoryGB        int    // container --memory, in GiB
	DeviceReadMBps  int    // container --device-read-bps, in MB/s
	DeviceWriteMBps int    // container --device-write-bps, in MB/s
	DevicePath      string // block device the rate limits apply to ("" → auto-detect)
}

func (n designNode) limits() nodeLimits {
	return nodeLimits{
		CPUs: n.CPUs, MemoryGB: n.MemoryGB,
		DeviceReadMBps: n.DeviceReadMBps, DeviceWriteMBps: n.DeviceWriteMBps,
		DevicePath: n.DevicePath,
	}
}

// applyVMSize copies a node's per-node sizing from the design onto the container spec,
// clamped to sane bounds. Zero values are left unset, and ContainerCreate leaves the
// container unlimited.
func applyVMSize(spec *ContainerSpec, lim nodeLimits) {
	if lim.CPUs > 0 {
		spec.CPUs = clampInt(lim.CPUs, 1, 64)
	}
	if lim.MemoryGB > 0 {
		spec.MemoryMB = clampInt(lim.MemoryGB, 1, 256) * 1024
	}
	// Clamped to 1 MB/s..16 GB/s. The floor matters: blk-throttle honours a rate of a
	// few bytes/sec literally, which would wedge a database node rather than slow it.
	if lim.DeviceReadMBps > 0 {
		spec.DeviceReadBPS = int64(clampInt(lim.DeviceReadMBps, 1, 16384)) * (1 << 20)
	}
	if lim.DeviceWriteMBps > 0 {
		spec.DeviceWriteBPS = int64(clampInt(lim.DeviceWriteMBps, 1, 16384)) * (1 << 20)
	}
	spec.DevicePath = lim.DevicePath
}

// shellQuote single-quotes a string for safe embedding in an sh command.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
