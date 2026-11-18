package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Dashboard — cheap summary counters (from the store) and focus-gated live OS stats (Docker
// stats, sampled lazily on request with a short cache).

// dbEngineOf maps a node type to a database-engine family for the "By engine" breakdown.
// Broader than engineForType (which is scoped to the Data Generator's engines); non-DB
// node types (intranet, pmm, proxysql, …) return "".
func dbEngineOf(t string) string {
	switch t {
	case "pg", "patroni", "repmgr", "pgstream", "spock":
		return "postgres"
	case "pxc", "ps", "mysql", "innodb",
		"mariadb", "mariadbrepl", "mariadbgalera",
		"mysqlce", "mysqlcerepl", "mysqlceinnodb":
		return "mysql"
	case "psm", "psmdb", "psmrs":
		return "mongodb"
	case "valkey", "valkeycluster":
		return "valkey"
	}
	return ""
}

// ------------------------------------------------------------- summary

type dashSummary struct {
	Stacks struct {
		Total    int `json:"total"`
		Deployed int `json:"deployed"`
		Draft    int `json:"draft"`
		Expired  int `json:"expired"`
	} `json:"stacks"`
	Nodes struct {
		Total   int `json:"total"`
		Running int `json:"running"`
		Error   int `json:"error"`
		Other   int `json:"other"`
	} `json:"nodes"`
	ByEngine map[string]int `json:"byEngine"`
	ByType   map[string]int `json:"byType"`
	DataGen  struct {
		Active int `json:"active"`
		Done   int `json:"done"`
		Error  int `json:"error"`
	} `json:"dataGen"`
	Activity []Notification `json:"activity"`
}

func (a *App) handleDashboardSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stacks, _ := a.store.ListStacks()

	sum := dashSummary{ByEngine: map[string]int{}, ByType: map[string]int{}}
	for _, s := range stacks {
		sum.Stacks.Total++
		switch s.Status {
		case StackDeployed:
			sum.Stacks.Deployed++
		case StackExpired:
			sum.Stacks.Expired++
		default:
			sum.Stacks.Draft++
		}
		full, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		types := map[string]string{}
		for _, n := range buildDoc(full).Nodes {
			types[n.ID] = n.Type
		}
		deps, _ := a.store.ListDeployments(s.ID)
		for _, d := range deps {
			sum.Nodes.Total++
			switch d.State {
			case DeployRunning:
				sum.Nodes.Running++
			case DeployError:
				sum.Nodes.Error++
			default:
				sum.Nodes.Other++
			}
			t := types[d.NodeID]
			if t != "" {
				sum.ByType[t]++
			}
			if eng := dbEngineOf(t); eng != "" && d.State == DeployRunning {
				sum.ByEngine[eng]++
			}
		}
	}

	// In-memory data-gen jobs.
	dgJobs.Lock()
	for _, j := range dgJobs.m {
		switch j.Status {
		case "running":
			sum.DataGen.Active++
		case "done":
			sum.DataGen.Done++
		case "error":
			sum.DataGen.Error++
		}
	}
	dgJobs.Unlock()

	sum.Activity, _ = a.store.ListNotifications(8)
	if sum.Activity == nil {
		sum.Activity = []Notification{}
	}

	writeJSON(w, http.StatusOK, sum)
}

// ------------------------------------------------------------- live OS stats

type containerStatRow struct {
	StackID int64  `json:"stackId"`
	Name    string `json:"name"`
	State   string `json:"state"`
	// Stack and Label are what the canvas calls it — "core. dump" / "mysql01" — so a busy
	// container can be traced to its card. The container name (dbcanvas-10-mysql-mv79p7lz-2) is
	// built from the stack id and the node's internal id, neither of which is on screen.
	Stack string `json:"stack,omitempty"`
	Label string `json:"label,omitempty"`
	ContainerStat
}

// canvasName is what the canvas calls a container: its stack and its node.
type canvasName struct{ Stack, Label string }

func (row *containerStatRow) name(n canvasName) {
	row.Stack, row.Label = n.Stack, n.Label
}

// containerLabels maps container names to their stack's name and node's canvas label, for
// the given stacks: the deployments say which container is which node, the design says its label.
//
// A K3D frame's containers are not on any deployment — k3d names them (k3dNodeContainer) — so
// they are worked out from the frame: its members in the order the deploy numbered them, and
// the load balancer k3d puts in front of the servers, which is the frame's own.
func (a *App) containerLabels(stackIDs map[int64]bool) map[string]canvasName {
	out := map[string]canvasName{}
	for id := range stackIDs {
		st, err := a.store.GetStack(id)
		if err != nil {
			continue
		}
		var doc designDoc
		_ = json.Unmarshal(st.Design, &doc)
		label := map[string]string{}
		for _, n := range doc.Nodes {
			label[n.ID] = n.Label
		}
		deps, _ := a.store.ListDeployments(id)
		depOf := map[string]Deployment{}
		for _, d := range deps {
			depOf[d.NodeID] = d
			if d.ContainerName != "" {
				out[d.ContainerName] = canvasName{st.Name, label[d.NodeID]}
			}
		}
		for _, f := range doc.Frames {
			if f.Type != "k3d" {
				continue
			}
			var members []designNode
			for _, n := range doc.Nodes {
				if n.FrameID == f.ID && n.Type == "k3d" {
					members = append(members, n)
				}
			}
			if len(members) == 0 {
				continue
			}
			sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
			cluster := k3dDeployedCluster(id, f, depOf[members[0].ID])
			for i, m := range members {
				out[k3dNodeContainer(cluster, i)] = canvasName{st.Name, m.Label}
			}
			out[k3dContainerPrefix+cluster+"-serverlb"] = canvasName{st.Name, f.Label + " load balancer"}
		}
	}
	return out
}

// statDisplayName is "stack / label" when the container is known, else its own name.
func statDisplayName(row containerStatRow) string {
	if row.Label != "" {
		return row.Stack + " / " + row.Label
	}
	return row.Name
}

var dashStats = struct {
	mu    sync.Mutex
	at    time.Time
	items []containerStatRow
}{}

var statsAlerts = struct {
	mu   sync.Mutex
	last map[string]time.Time // containerID+kind -> last alert
}{last: map[string]time.Time{}}

// stackIDFromName parses the stack id from a container name, in either of the two
// schemes a stack's containers carry: dbcanvas-<stackID>-… for the ones this app names,
// and k3d-<frame>-s<stackID>-… for the ones a K3D frame leaves to k3d. Both have to be
// read here, or a k3s node arrives with stack 0 and the ownership filter below drops it
// again — the same way ListManaged used to drop it one step earlier.
func stackIDFromName(name string) int64 {
	if id, ok := k3dStackIDFromContainer(name); ok {
		return id
	}
	id, _ := stackIDFromInstanceName(name)
	return id
}

// sampleStats returns a cached (≤2s) snapshot of all managed running containers. Because it
// runs only when a client requests /api/dashboard/stats (i.e. the dashboard is focused),
// there is no background CPU cost when nobody is watching.
func (a *App) sampleStats(ctx context.Context) []containerStatRow {
	dashStats.mu.Lock()
	defer dashStats.mu.Unlock()
	if dashStats.items != nil && time.Since(dashStats.at) < 2*time.Second {
		return dashStats.items
	}
	if a.docker == nil {
		return nil
	}
	containers, err := a.engCtx(ctx).ListManaged(ctx)
	if err != nil {
		return dashStats.items
	}
	rows := make([]containerStatRow, len(containers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, c := range containers {
		rows[i] = containerStatRow{StackID: stackIDFromName(c.Name), Name: c.Name, State: c.State}
		if c.State != "running" {
			continue
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if st, err := a.engCtx(ctx).ContainerStats(ctx, id); err == nil {
				rows[i].ContainerStat = st
			}
		}(i, c.ID)
	}
	wg.Wait()
	dashStats.items = rows
	dashStats.at = time.Now()
	go a.checkThresholds(rows, containers)
	return rows
}

// checkThresholds emits resource-alert notifications (per-container, 10-min cooldown).
func (a *App) checkThresholds(rows []containerStatRow, ci []ContainerInfo) {
	idByName := map[string]string{}
	for _, c := range ci {
		idByName[c.Name] = c.ID
	}
	var names map[string]canvasName // looked up only once something is over a threshold
	for _, row := range rows {
		if row.State != "running" {
			continue
		}
		var kind, msg string
		if row.CPUPercent >= 90 {
			kind, msg = "cpu", "sustained CPU above 90%"
		} else if row.MemPercent >= 90 {
			kind, msg = "mem", "memory above 90% of its limit"
		}
		if kind == "" {
			continue
		}
		key := idByName[row.Name] + ":" + kind
		statsAlerts.mu.Lock()
		last := statsAlerts.last[key]
		if time.Since(last) < 10*time.Minute {
			statsAlerts.mu.Unlock()
			continue
		}
		statsAlerts.last[key] = time.Now()
		statsAlerts.mu.Unlock()
		if names == nil {
			ids := map[int64]bool{}
			for _, r := range rows {
				ids[r.StackID] = true
			}
			names = a.containerLabels(ids)
		}
		if n, ok := names[row.Name]; ok {
			row.name(n)
		}
		a.notifyStack(row.StackID, "resource.alert", "warning", "High resource usage",
			statDisplayName(row)+": "+msg, "")
	}
}

func (a *App) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	known := map[int64]bool{}
	if stacks, err := a.store.ListStacks(); err == nil {
		for _, s := range stacks {
			known[s.ID] = true
		}
	}
	rows := a.sampleStats(r.Context())

	var mine []containerStatRow
	for _, row := range rows {
		if known[row.StackID] { // skip orphaned containers whose stack no longer exists in the DB
			mine = append(mine, row)
		}
	}
	// Aggregate + full per-node list (the client derives rates + ranks the tables).
	var totCPU, totMemUsed, totMemLimit float64
	nodes := []containerStatRow{}
	ids := map[int64]bool{}
	for _, row := range mine {
		ids[row.StackID] = true
	}
	names := a.containerLabels(ids)
	for _, row := range mine {
		if row.State != "running" {
			continue
		}
		if n, ok := names[row.Name]; ok {
			row.name(n)
		}
		totCPU += row.CPUPercent
		totMemUsed += float64(row.MemUsed)
		totMemLimit += float64(row.MemLimit)
		nodes = append(nodes, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"containers":   len(nodes),
		"cpuPercent":   totCPU,
		"memUsed":      int64(totMemUsed),
		"memLimit":     int64(totMemLimit),
		"nodes":        nodes,
		"sampledAtSec": time.Now().Unix(),
	})
}
