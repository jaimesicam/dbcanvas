package main

import (
	"math"
	"testing"
)

func TestParseLimaStats(t *testing.T) {
	sample := func(user, idle string) string {
		return `@stat
cpu  ` + user + ` 0 100 ` + idle + ` 0 0 0 0 0 0
cpu0 1 0 0 1 0 0 0 0 0 0
cpu1 1 0 0 1 0 0 0 0 0 0
intr 1
@meminfo
MemTotal:        2000000 kB
MemFree:          200000 kB
MemAvailable:     500000 kB
@net
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  999 1 0 0 0 0 0 0  999 1 0 0 0 0 0 0
  eth0:  1000 10 0 0 0 0 0 0  2000 20 0 0 0 0 0 0
  dbc0:  300 3 0 0 0 0 0 0  400 4 0 0 0 0 0 0
@disk
 252       0 vda 100 0 8 0 50 0 16 0 0 0 0
 252       1 vda1 100 0 8 0 50 0 16 0 0 0 0
`
	}
	first, cpu := parseLimaStats(sample("1000", "8900"), limaCPU{})
	if first.CPUPercent != 0 {
		t.Errorf("first sample CPU %v, want 0", first.CPUPercent)
	}
	if first.MemLimit != 2000000*1024 || first.MemUsed != 1500000*1024 || math.Abs(first.MemPercent-75) > 0.01 {
		t.Errorf("memory %+v", first)
	}
	if first.NetRx != 1300 || first.NetTx != 2400 {
		t.Errorf("network rx %d tx %d: want lo left out", first.NetRx, first.NetTx)
	}
	if first.BlkRead != 8*512 || first.BlkWrite != 16*512 {
		t.Errorf("disk read %d write %d: want the partition left out", first.BlkRead, first.BlkWrite)
	}
	// 1000 more busy out of 2000 more in all, on two CPUs: 100%.
	second, _ := parseLimaStats(sample("2000", "9900"), cpu)
	if math.Abs(second.CPUPercent-100) > 0.01 {
		t.Errorf("CPU %v, want 100", second.CPUPercent)
	}
}
