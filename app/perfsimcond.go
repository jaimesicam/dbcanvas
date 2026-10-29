package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Network and disk conditions for Performance Sim.
//
// The sim's Network and Disk stressors cannot be applied over a database
// connection: latency and loss live in a member's network namespace, disk
// limits in its cgroup. DBCanvas applies them, with the same code that applies a
// node's canvas settings (netem.go, blkio.go). The sim only says what it wants;
// this loop polls every running Performance Sim (POST /api/conditions/sync on
// its own port, over the stack network), applies the difference, and reports
// what the kernel now has. Polling rather than having the sim call DBCanvas
// keeps the direction every other DBCanvas-to-node path already uses, and means
// the sim never needs credentials for DBCanvas.
//
// DBCanvas, not the sim, is what makes the conditions safe:
//   - they are applied only to the members DBCanvas resolved for that sim at
//     deploy (perfSimConfig.MemberNodes), whatever the sim asks;
//   - each one's maximum hold is enforced here as well as in the sim;
//   - if the sim stops answering for perfSimQuiet, or is no longer running, or
//     is removed, everything it asked for is cleared;
//   - clearing restores the node's own canvas settings (its netem spec and
//     device limits), not nothing, so a node designed with 80 ms latency keeps it;
//   - what is applied is recorded on the sim's deployment, so an app restart
//     still knows what to clear.

const (
	perfSimSyncEvery = 2 * time.Second
	perfSimQuiet     = 20 * time.Second
)

// perfSimNet and perfSimDisk are one member's requested conditions, as the sim
// sends them (perfsim/internal/core.NetSpec and DiskSpec).
type perfSimNet struct {
	LatencyMS int     `json:"latencyMs,omitempty"`
	JitterMS  int     `json:"jitterMs,omitempty"`
	LossPct   float64 `json:"lossPct,omitempty"`
	RateMbit  int     `json:"rateMbit,omitempty"`
}

type perfSimDisk struct {
	ReadMBps  int `json:"readMBps,omitempty"`
	WriteMBps int `json:"writeMBps,omitempty"`
	WriteIOPS int `json:"writeIops,omitempty"`
}

type perfSimDesired struct {
	Member string       `json:"member"`
	Net    *perfSimNet  `json:"net,omitempty"`
	Disk   *perfSimDisk `json:"disk,omitempty"`
	Until  int64        `json:"until"`
}

// perfSimApplied is what is on one member for a sim: the request in force and
// the kernel's own description of it.
type perfSimApplied struct {
	Net     *perfSimNet  `json:"net,omitempty"`
	NetOut  string       `json:"netOut,omitempty"`
	Disk    *perfSimDisk `json:"disk,omitempty"`
	DiskOut string       `json:"diskOut,omitempty"`
	Err     string       `json:"err,omitempty"`
}

type perfSimCaps struct {
	Net      bool   `json:"net"`
	Disk     bool   `json:"disk"`
	NetWhy   string `json:"netWhy,omitempty"`
	DiskWhy  string `json:"diskWhy,omitempty"`
	DiskWarn string `json:"diskWarn,omitempty"`
}

var perfSimSeen = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

func (a *App) startPerfSimConditions() {
	go func() {
		for {
			a.perfSimPass(context.Background())
			time.Sleep(perfSimSyncEvery)
		}
	}()
}

// perfSimPass syncs every Performance Sim node of every stack once.
func (a *App) perfSimPass(ctx context.Context) {
	stacks, err := a.store.ListStacks(0, true)
	if err != nil {
		return
	}
	for _, sum := range stacks {
		// The listing carries no design; only a deployed stack can have a sim running.
		if sum.Status != StackDeployed {
			continue
		}
		st, err := a.store.GetStack(sum.ID)
		if err != nil {
			continue
		}
		doc := buildDoc(st)
		for _, n := range doc.Nodes {
			if n.Type != "perfsim" {
				continue
			}
			dep, err := a.store.GetDeployment(st.ID, n.ID)
			if err != nil {
				continue
			}
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			a.syncPerfSim(c, st, doc, dep)
			cancel()
		}
	}
}

func (a *App) syncPerfSim(ctx context.Context, st Stack, doc designDoc, dep Deployment) {
	var cfg perfSimConfig
	if json.Unmarshal(dep.Config, &cfg) != nil || len(cfg.MemberNodes) == 0 {
		return
	}
	key := fmt.Sprintf("%d/%s", st.ID, dep.NodeID)
	desired := map[string]perfSimDesired{}
	if dep.State == DeployRunning && dep.ContainerID != "" {
		got, err := a.perfSimCall(ctx, st, dep.ContainerID, a.perfSimCaps(ctx, st, doc, cfg), cfg.Applied)
		perfSimSeen.Lock()
		last, seen := perfSimSeen.m[key]
		if err == nil {
			perfSimSeen.m[key] = time.Now()
		}
		perfSimSeen.Unlock()
		if err != nil {
			// A missed poll is not yet a reason to clear: the sim may be busy.
			if !seen || time.Since(last) < perfSimQuiet || len(cfg.Applied) == 0 {
				return
			}
			log.Printf("stack %d: Performance Sim %s has not answered for %s; clearing its network and disk conditions", st.ID, dep.NodeID, perfSimQuiet)
		} else {
			for _, d := range got {
				desired[d.Member] = d
			}
		}
	}
	if a.applyPerfSimConditions(ctx, st, doc, &cfg, desired) {
		dep.Config = mustJSON(cfg)
		a.store.UpsertDeployment(dep)
	}
}

// applyPerfSimConditions brings every member to what is desired, and reports
// whether anything changed.
func (a *App) applyPerfSimConditions(ctx context.Context, st Stack, doc designDoc, cfg *perfSimConfig, desired map[string]perfSimDesired) bool {
	if cfg.Applied == nil {
		cfg.Applied = map[string]perfSimApplied{}
	}
	members := map[string]bool{}
	for m := range cfg.Applied {
		members[m] = true
	}
	for m := range desired {
		members[m] = true
	}
	names := make([]string, 0, len(members))
	for m := range members {
		names = append(names, m)
	}
	sort.Strings(names)
	nodes := map[string]designNode{}
	for _, n := range doc.Nodes {
		nodes[n.ID] = n
	}

	// Members are brought up to date in parallel, each step with its own time
	// limit: a member can stop answering (a write cap under emulation slows
	// starting any program in it to a crawl), and it must not keep the others,
	// or its own release, waiting. Disk goes first for the same reason: lifting
	// a write cap is what makes such a member answer again, and the network step
	// runs a program inside the member.
	var mu sync.Mutex
	var wg sync.WaitGroup
	changed := false
	now := time.Now().Unix()
	step := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 20*time.Second)
	}
	for _, m := range names {
		mu.Lock()
		have := cfg.Applied[m]
		mu.Unlock()
		want := desired[m]
		if want.Until > 0 && now > want.Until {
			want = perfSimDesired{} // past its maximum hold
		}
		node, known := nodes[cfg.MemberNodes[m]]
		if !known {
			// Not one of this sim's members: refuse, and forget anything recorded.
			if have.Net != nil || have.Disk != nil || have.Err != "" {
				mu.Lock()
				delete(cfg.Applied, m)
				changed = true
				mu.Unlock()
			}
			continue
		}
		mdep, err := a.store.GetDeployment(st.ID, node.ID)
		if err != nil || mdep.ContainerID == "" {
			continue
		}
		if sameNet(have.Net, want.Net) && sameDisk(have.Disk, want.Disk) {
			continue
		}
		wg.Add(1)
		go func(m string, have perfSimApplied, want perfSimDesired, node designNode, cid string) {
			defer wg.Done()
			have.Err = ""
			if !sameDisk(have.Disk, want.Disk) {
				read, write := int64(node.DeviceReadMBps)<<20, int64(node.DeviceWriteMBps)<<20
				var iops int64
				if d := want.Disk; d != nil {
					if d.ReadMBps > 0 {
						read = int64(d.ReadMBps) << 20
					}
					if d.WriteMBps > 0 {
						write = int64(d.WriteMBps) << 20
					}
					iops = int64(d.WriteIOPS)
				}
				c, cancel := step()
				out, err := a.docker.ApplyDiskLimitsIOPS(c, intranetImage(stackIntranetArch(doc)), []string{cid}, node.DevicePath, read, write, iops)
				cancel()
				if err != nil {
					have.Err = "disk: " + err.Error()
					log.Printf("stack %d: %s disk limits not changed: %v", st.ID, node.Label, err)
				} else {
					have.Disk, have.DiskOut = want.Disk, ""
					if want.Disk != nil {
						// The helper prints "<container id> <io.max line>".
						if _, line, ok := strings.Cut(out, " "); ok {
							have.DiskOut = "io.max " + line
						}
					}
					log.Printf("stack %d: %s disk now %s", st.ID, node.Label, strings.TrimSpace(out))
				}
			}
			if !sameNet(have.Net, want.Net) {
				spec := nodeNetemSpec(node)
				if want.Net != nil {
					spec = clampNetem(want.Net.LatencyMS, want.Net.JitterMS, want.Net.LossPct, want.Net.RateMbit, false, netemPortsFor(node.Type))
				}
				c, cancel := step()
				_, err := applyNetem(c, a.nodeEngine(st, node.Type), cid, spec)
				cancel()
				if err != nil {
					have.Err = strings.TrimSpace(have.Err + " network: " + err.Error())
					log.Printf("stack %d: %s network not changed: %v", st.ID, node.Label, err)
				} else {
					have.Net, have.NetOut = want.Net, ""
					if want.Net != nil {
						have.NetOut = describeNetem(spec)
					}
					log.Printf("stack %d: %s network now %s", st.ID, node.Label, describeNetem(spec))
				}
			}
			mu.Lock()
			if have.Net == nil && have.Disk == nil && have.Err == "" {
				delete(cfg.Applied, m)
			} else {
				cfg.Applied[m] = have
			}
			changed = true
			mu.Unlock()
		}(m, have, want, node, mdep.ContainerID)
	}
	wg.Wait()
	return changed
}

// clearPerfSimConditions undoes everything a sim's deployment applied. Called
// when the sim node is removed, before its record goes.
func (a *App) clearPerfSimConditions(ctx context.Context, st Stack, d Deployment) {
	var cfg perfSimConfig
	if json.Unmarshal(d.Config, &cfg) != nil || len(cfg.Applied) == 0 {
		return
	}
	a.applyPerfSimConditions(ctx, st, buildDoc(st), &cfg, nil)
}

// perfSimCaps says what DBCanvas can apply to this sim's members.
func (a *App) perfSimCaps(ctx context.Context, st Stack, doc designDoc, cfg perfSimConfig) perfSimCaps {
	c := perfSimCaps{Net: true, Disk: true}
	nodes := map[string]designNode{}
	for _, n := range doc.Nodes {
		nodes[n.ID] = n
	}
	for _, id := range cfg.MemberNodes {
		n := nodes[id]
		if !netemSupported(n.Type) {
			c.Net, c.NetWhy = false, fmt.Sprintf("%s nodes cannot have their network shaped.", n.Type)
		}
		if _, docker := a.nodeEngine(st, n.Type).(*Docker); !docker {
			c.Disk, c.DiskWhy = false, "These members run as Vagrant VMs; disk limits apply to Docker containers only."
		}
		if _, emulated := a.emulatedAmd64(ctx, n.Arch); emulated && c.DiskWarn == "" {
			c.DiskWarn = "These members run under x86-64 emulation. There, a low write cap also slows the translator that starts every program in the member, so the member can stop answering until the cap is released. DBCanvas releases it on time regardless."
		}
	}
	if c.Disk {
		if v := a.docker.CgroupVersion(ctx); v != "2" {
			c.Disk = false
			c.DiskWhy = "The Docker host uses cgroup v1; changing a running container's disk limits needs cgroup v2."
			if v == "" {
				c.DiskWhy = "DBCanvas could not tell which cgroup version the Docker host uses; disk limits need cgroup v2."
			}
		}
	}
	return c
}

// perfSimCall is one poll of the sim, over the stack network.
func (a *App) perfSimCall(ctx context.Context, st Stack, containerID string, caps perfSimCaps, applied map[string]perfSimApplied) ([]perfSimDesired, error) {
	eng := a.dialEngine(st.ID, containerID)
	netName := networkName(st.ID)
	if err := a.joinStackForDial(ctx, eng, netName); err != nil {
		return nil, err
	}
	ip, err := eng.ContainerIP(ctx, containerID, netName)
	if err != nil || ip == "" {
		return nil, fmt.Errorf("the sim has no address on the stack network")
	}
	report := map[string]map[string]string{}
	for m, ap := range applied {
		report[m] = map[string]string{"net": ap.NetOut, "disk": ap.DiskOut, "err": ap.Err}
	}
	body, _ := json.Marshal(map[string]any{"caps": caps, "applied": report})
	url := "http://" + net.JoinHostPort(ip, strconv.Itoa(perfSimPort)) + "/api/conditions/sync"
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sync answered %d", resp.StatusCode)
	}
	var out struct {
		Conditions []perfSimDesired `json:"conditions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Conditions, nil
}

func stackIntranetArch(doc designDoc) string {
	for _, n := range doc.Nodes {
		if n.Type == "intranet" {
			return n.Arch
		}
	}
	return ""
}

func sameNet(a, b *perfSimNet) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameDisk(a, b *perfSimDisk) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
