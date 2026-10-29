package core

import (
	"context"
	"testing"
)

// Several dials on the same member become one request for it; a member only one
// dial touches gets only that; stopping a dial withdraws its part.
func TestConditionsMerge(t *testing.T) {
	c := newConditions()
	lat := c.start("netlat", Params{"ms": 100, "jitter": 10}, []string{"db2", "db3"}, 60)
	c.start("netloss", Params{"loss": 5}, []string{"db3"}, 300)
	c.start("diskw", Params{"wmb": 5}, []string{"db2"}, 120)

	got := map[string]Desired{}
	for _, d := range c.desired() {
		got[d.Member] = d
	}
	db2, db3 := got["db2"], got["db3"]
	if db2.Net == nil || db2.Net.LatencyMS != 100 || db2.Net.JitterMS != 10 || db2.Net.LossPct != 0 {
		t.Errorf("db2 net %+v", db2.Net)
	}
	if db2.Disk == nil || db2.Disk.WriteMBps != 5 {
		t.Errorf("db2 disk %+v", db2.Disk)
	}
	if db3.Net == nil || db3.Net.LatencyMS != 100 || db3.Net.LossPct != 5 || db3.Disk != nil {
		t.Errorf("db3 %+v %+v", db3.Net, db3.Disk)
	}
	// The earliest hold of the dials on a member is the one in force.
	if db3.Until > got["db3"].Until || db3.Until == 0 {
		t.Errorf("db3 until %d", db3.Until)
	}
	lat.Stop(context.Background())
	for _, d := range c.desired() {
		if d.Net != nil && d.Net.LatencyMS != 0 {
			t.Errorf("%s still has latency after the dial stopped", d.Member)
		}
	}
}

// Until DBCanvas has polled, every network and disk dial is unavailable and says why.
func TestConditionsNeedDBCanvas(t *testing.T) {
	c := newConditions()
	for _, d := range c.Dials() {
		if d.Available || d.Why == "" {
			t.Errorf("%s available before DBCanvas connected", d.ID)
		}
	}
	c.sync(Caps{Net: true, Disk: false, DiskWhy: "cgroup v1"}, nil)
	for _, d := range c.Dials() {
		wantAvail := d.Group == groupNet
		if d.Available != wantAvail {
			t.Errorf("%s available %v, want %v", d.ID, d.Available, wantAvail)
		}
		if d.Group == groupDisk && d.Why != "cgroup v1" {
			t.Errorf("%s why %q", d.ID, d.Why)
		}
	}
}
