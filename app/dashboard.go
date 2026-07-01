package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dashboard — cheap summary counters (from the store) and focus-gated live OS stats (Docker
// stats, sampled lazily on request with a short cache). Admins see everything; a regular
// user sees only their own stacks' data.

// ------------------------------------------------------------- summary

type dashUsers struct {
	Total   int `json:"total"`
	Pending int `json:"pending"`
}

type dashSummary struct {
	Scope  string `json:"scope"`
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
	Users    *dashUsers     `json:"users,omitempty"`
	Activity []Notification `json:"activity"`
}

func (a *App) handleDashboardSummary(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	admin := u.Role == RoleAdmin
	stacks, _ := a.store.ListStacks(u.ID, admin)

	sum := dashSummary{Scope: "user", ByEngine: map[string]int{}, ByType: map[string]int{}}
	if admin {
		sum.Scope = "admin"
	}
	ownerOf := map[int64]int64{}
	for _, s := range stacks {
		ownerOf[s.ID] = s.OwnerID
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
			if eng := engineForType(t); eng != "" && d.State == DeployRunning {
				sum.ByEngine[eng]++
			}
		}
	}

	// In-memory data-gen jobs, scoped by owner.
	dgJobs.Lock()
	for _, j := range dgJobs.m {
		if !admin && ownerOf[j.StackID] != u.ID {
			continue
		}
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

	sum.Activity, _ = a.store.ListNotifications(u.ID, admin, 8)
	if sum.Activity == nil {
		sum.Activity = []Notification{}
	}

	if admin {
		total, _ := a.store.CountUsers()
		pending := 0
		if users, err := a.store.ListUsers(); err == nil {
			for _, us := range users {
				if us.Status == StatusPending {
					pending++
				}
			}
		}
		sum.Users = &dashUsers{Total: total, Pending: pending}
	}
	writeJSON(w, http.StatusOK, sum)
}

// ------------------------------------------------------------- live OS stats

type containerStatRow struct {
	StackID int64  `json:"stackId"`
	Name    string `json:"name"`
	State   string `json:"state"`
	ContainerStat
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

// stackIDFromName parses the stack id from a dbcanvas-<stackID>-… container name.
func stackIDFromName(name string) int64 {
	rest := strings.TrimPrefix(name, "dbcanvas-")
	if i := strings.IndexByte(rest, '-'); i > 0 {
		if id, err := strconv.ParseInt(rest[:i], 10, 64); err == nil {
			return id
		}
	}
	return 0
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
	containers, err := a.docker.ListManaged(ctx)
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
			if st, err := a.docker.ContainerStats(ctx, id); err == nil {
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
		a.notifyStack(row.StackID, "resource.alert", "warning", "High resource usage",
			row.Name+": "+msg, "")
	}
}

func (a *App) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	admin := u.Role == RoleAdmin
	// Ownership map for filtering.
	owner := map[int64]int64{}
	if stacks, err := a.store.ListStacks(u.ID, admin); err == nil {
		for _, s := range stacks {
			owner[s.ID] = s.OwnerID
		}
	}
	rows := a.sampleStats(r.Context())

	var mine []containerStatRow
	for _, row := range rows {
		ownerID, tracked := owner[row.StackID]
		if !tracked { // skip orphaned containers whose stack no longer exists in the DB
			continue
		}
		if admin || ownerID == u.ID {
			mine = append(mine, row)
		}
	}
	// Aggregate + full per-node list (the client derives rates + ranks the tables).
	var totCPU, totMemUsed, totMemLimit float64
	nodes := []containerStatRow{}
	for _, row := range mine {
		if row.State != "running" {
			continue
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
