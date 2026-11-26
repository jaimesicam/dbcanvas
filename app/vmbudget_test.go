package main

import (
	"strings"
	"testing"
)

// TestVMBudgetIssue: the warning comes when the stack's VMs, the VMs already running and Docker's
// VM add up to more than the machine has, and names each part; it stays quiet when they fit,
// when the stack adds no VM, and when the machine's memory is unknown.
func TestVMBudgetIssue(t *testing.T) {
	const g = int64(1) << 30
	// A 32 GiB machine whose Docker VM has 25 GiB, with seven 2 GiB VMs running.
	got := vmBudgetIssue(vmBudget{host: 32 * g, docker: 25 * g, running: 14 * g, stack: 6 * g})
	for _, want := range []string{"reserve 6 GiB", "14 GiB held by VMs already running", "25 GiB given to Docker's VM", "45 GiB in all", "this machine's 32 GiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning lacks %q:\n%s", want, got)
		}
	}
	for _, b := range []vmBudget{
		{host: 32 * g, docker: 8 * g, running: 4 * g, stack: 6 * g}, // fits
		{host: 32 * g, docker: 40 * g},                              // no VM in this stack
		{docker: 8 * g, stack: 64 * g},                              // memory unknown
	} {
		if got := vmBudgetIssue(b); got != "" {
			t.Errorf("vmBudgetIssue(%+v) = %q, want nothing", b, got)
		}
	}
	if got := vmBudgetIssue(vmBudget{host: 32 * g, docker: 27264000000, stack: 8 * g}); !strings.Contains(got, "25.4 GiB given to Docker's VM: 33.4 GiB in all") {
		t.Errorf("fractions are rounded to one place: %q", got)
	}
	if got := vmBudgetIssue(vmBudget{host: 16 * g, stack: 18 * g}); !strings.Contains(got, "reserve 18 GiB: 18 GiB in all") {
		t.Errorf("stack alone over the machine: %q", got)
	}
}
