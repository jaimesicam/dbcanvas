package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// A VM reserves all of its memory while it runs, unlike a container, which takes what it uses.
// vmMemoryIssues warns at validation when the stack's VMs, with the VMs already running and, on
// macOS, the memory given to Docker's own VM, add up to more than the machine has: past that the
// host swaps hard, everything slows, and macOS may stop a VM to free memory.

// hostMemoryBytes is this machine's physical memory, or 0 when it cannot tell.
func hostMemoryBytes() int64 {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return n
	case "linux":
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

// vmBudget is what vmMemoryIssues weighs, in bytes.
type vmBudget struct {
	host    int64 // the machine's physical memory
	docker  int64 // memory given to Docker's VM (macOS); 0 where Docker shares the host's
	running int64 // VMs already running, other than this stack's
	stack   int64 // this stack's VMs that are not running yet
}

// vmBudgetIssue is the warning for b, or "" when the VMs fit. Unknown host memory says nothing.
func vmBudgetIssue(b vmBudget) string {
	if b.host <= 0 || b.stack <= 0 {
		return ""
	}
	need := b.stack + b.running + b.docker
	if need <= b.host {
		return ""
	}
	gib := func(n int64) string {
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64), ".0") + " GiB"
	}
	msg := fmt.Sprintf("this stack's VMs reserve %s", gib(b.stack))
	var with []string
	if b.running > 0 {
		with = append(with, gib(b.running)+" held by VMs already running")
	}
	if b.docker > 0 {
		with = append(with, gib(b.docker)+" given to Docker's VM")
	}
	if len(with) > 0 {
		msg += ", with " + strings.Join(with, " and ")
	}
	return msg + fmt.Sprintf(": %s in all, more than this machine's %s. Each VM holds all of its memory while it runs, so the host would swap and slow everything down. Give the VMs less Memory, stop other stacks, or lower Docker's VM memory", gib(need), gib(b.host))
}

// vmMemoryIssues weighs the stack's VMs that are not running yet against the machine's memory.
func (a *App) vmMemoryIssues(ctx context.Context, stackID int64, doc designDoc) []issue {
	if a.lima == nil || len(vmNodeIDs(doc)) == 0 {
		return nil
	}
	frames := map[string]designFrame{}
	for _, f := range doc.Frames {
		frames[f.ID] = f
	}
	all, err := a.lima.instances(ctx)
	if err != nil {
		return nil
	}
	b := vmBudget{host: hostMemoryBytes()}
	managed := a.lima.managed()
	mine := map[string]bool{} // this stack's instances, by id
	for _, n := range doc.Nodes {
		if nodeRuntime(n, frames) != runtimeVM {
			continue
		}
		id := limaInstanceName(containerName(stackID, n.ID))
		mine[id] = true
		if in, ok := all[id]; ok && in.Status == "Running" {
			continue // already counted by the machine; a redeploy does not add it again
		}
		gb := n.MemoryGB
		if gb <= 0 {
			gb = limaDefaultMemoryMB / 1024
		}
		b.stack += int64(clampInt(gb, 1, 256)) << 30
	}
	for id, in := range all {
		if _, ours := managed[id]; ours && !mine[id] && in.Status == "Running" {
			b.running += in.Memory
		}
	}
	if runtime.GOOS == "darwin" && a.docker != nil {
		_, b.docker = a.docker.HostResources(ctx)
	}
	if msg := vmBudgetIssue(b); msg != "" {
		return []issue{{Level: "warning", Message: msg}}
	}
	return nil
}
