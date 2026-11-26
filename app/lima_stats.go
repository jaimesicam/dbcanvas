package main

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// lima_stats.go — a VM's resource sample, read from its /proc in one command over the shared ssh
// connection, in the terms Docker reports a container's: CPU as a percentage of one CPU (so two
// busy CPUs read 200), memory in use without the page cache, and the bytes its network
// interfaces and disks have moved.

// limaStatsScript prints the four /proc files a sample is made from, each behind a marker line.
const limaStatsScript = `echo @stat; cat /proc/stat; echo @meminfo; cat /proc/meminfo; echo @net; cat /proc/net/dev; echo @disk; cat /proc/diskstats`

// limaCPU is the CPU counters of one sample.
type limaCPU struct{ busy, total uint64 }

// limaPrevCPU holds each VM's previous counters, for the CPU percentage between two samples.
var limaPrevCPU sync.Map // id → limaCPU

// limaWholeDisk matches a whole disk's name, not a partition's.
var limaWholeDisk = regexp.MustCompile(`^(?:[sv]d[a-z]+|xvd[a-z]+|nvme\d+n\d+)$`)

// parseLimaStats reads limaStatsScript's output. prev is the previous sample's CPU counters (zero
// when there is none, and then CPU reads 0).
func parseLimaStats(out string, prev limaCPU) (ContainerStat, limaCPU) {
	var st ContainerStat
	var cpu limaCPU
	ncpu := 0
	mem := map[string]int64{}
	section := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "@") {
			section = line[1:]
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch section {
		case "stat":
			if f[0] == "cpu" {
				for i, v := range f[1:] {
					n, _ := strconv.ParseUint(v, 10, 64)
					cpu.total += n
					if i != 3 && i != 4 { // idle and iowait are not busy
						cpu.busy += n
					}
				}
			} else if strings.HasPrefix(f[0], "cpu") {
				ncpu++
			}
		case "meminfo":
			if len(f) >= 2 {
				n, _ := strconv.ParseInt(f[1], 10, 64)
				mem[strings.TrimSuffix(f[0], ":")] = n * 1024
			}
		case "net":
			name, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || name == "lo" {
				continue
			}
			c := strings.Fields(rest)
			if len(c) >= 9 {
				rx, _ := strconv.ParseInt(c[0], 10, 64)
				tx, _ := strconv.ParseInt(c[8], 10, 64)
				st.NetRx += rx
				st.NetTx += tx
			}
		case "disk":
			if len(f) >= 10 && limaWholeDisk.MatchString(f[2]) {
				r, _ := strconv.ParseInt(f[5], 10, 64)
				w, _ := strconv.ParseInt(f[9], 10, 64)
				st.BlkRead += r * 512
				st.BlkWrite += w * 512
			}
		}
	}
	if prev.total > 0 && cpu.total > prev.total && cpu.busy >= prev.busy {
		st.CPUPercent = float64(cpu.busy-prev.busy) / float64(cpu.total-prev.total) * float64(max(ncpu, 1)) * 100
	}
	st.MemLimit = mem["MemTotal"]
	st.MemUsed = st.MemLimit - mem["MemAvailable"]
	if st.MemUsed < 0 {
		st.MemUsed = 0
	}
	if st.MemLimit > 0 {
		st.MemPercent = float64(st.MemUsed) / float64(st.MemLimit) * 100
	}
	return st, cpu
}

// ContainerStats samples the VM's CPU, memory, network and disk.
func (l *Lima) ContainerStats(ctx context.Context, id string) (ContainerStat, error) {
	res, err := l.Exec(ctx, id, []string{"sh", "-c", limaStatsScript}, nil)
	if err != nil {
		return ContainerStat{}, err
	}
	var prev limaCPU
	if p, ok := limaPrevCPU.Load(id); ok {
		prev = p.(limaCPU)
	}
	st, cpu := parseLimaStats(res.Stdout, prev)
	limaPrevCPU.Store(id, cpu)
	return st, nil
}
