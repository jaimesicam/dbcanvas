package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// rolling.go — restart a whole cluster one member at a time, the way a DBA rolls out a
// configuration change or picks up a patched package: replicas first, each one back and caught up
// before the next goes; then, with a switchover, the primary's role handed to a replica that has
// already been restarted, and the old primary restarted last as a replica. Writes pause only for
// the switchover's few seconds. Without the switchover the primary is restarted in place, last,
// and the cluster has no primary while it comes back (or fails over by itself, where it can).
//
// Galera members are all writers: each is restarted and waited for until Synced again.
//
// The run is a background job like a rebuild (rebuild.go), followed through GET, and holds the
// cluster's lock so no switchover or rebuild runs in the middle of it.

const rollingTimeout = 90 * time.Minute

var rollingJobs = struct {
	m map[string]*rebuildJob // "stack:frame"
}{m: map[string]*rebuildJob{}}

// restartContainer restarts one node's container and puts back what a restart takes away — the
// published ports Docker hands out anew, the node's resolver, the stack's DNS — as the node menu's
// Restart does.
func (a *App) restartContainer(ctx context.Context, st Stack, nid string) error {
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" {
		return errors.New("not deployed")
	}
	c := withEngine(ctx, a.depEngine(st, nid))
	if err := a.engCtx(c).ContainerRestart(c, dep.ContainerID); err != nil {
		return err
	}
	a.store.SetDeploymentState(st.ID, nid, DeployRunning)
	a.refreshPublishedPorts(c, st, nid, dep)
	a.restoreNodeResolver(c, st, nid, dep)
	a.reconcileStackDNS(c, st.ID)
	return nil
}

// memberHealthy says whether a member that was restarted is back in its place: answering, with
// nothing wrong with its replication, and — a replica — not far behind.
func memberHealthy(kind string, r *liveRole) bool {
	if r == nil || r.Err != "" || r.Down || len(r.Problems) > 0 {
		return false
	}
	if r.LagSec != nil && *r.LagSec > 10 {
		return false
	}
	switch kind {
	case "galera":
		return r.State == "Synced"
	case "mongo":
		return r.Role == "primary" || r.Role == "secondary" || r.Role == "arbiter"
	case "gr":
		return r.Role == "primary" || r.Role == "secondary"
	}
	return r.Role == "primary" || r.Role == "replica"
}

func (a *App) waitMemberHealthy(ctx context.Context, st Stack, t swTopo, m swMember, j *rebuildJob, within time.Duration) error {
	deadline := time.Now().Add(within)
	last := ""
	for {
		c := withEngine(ctx, a.depEngine(st, m.Node.ID))
		pc, cancel := context.WithTimeout(c, 15*time.Second)
		r := a.probeLiveRole(pc, st, m.Node.ID, m.Node.Type, len(t.Members))
		cancel()
		if memberHealthy(t.Kind, r) {
			j.add("%s is back: %s", m.Node.Label, firstNonEmpty(r.State, r.Role))
			return nil
		}
		now := "starting"
		if r != nil {
			now = firstNonEmpty(r.Err, strings.Join(r.Problems, "; "), r.State, r.Role)
			if r.LagSec != nil && *r.LagSec > 10 {
				now = fmt.Sprintf("catching up, lag %.0fs", *r.LagSec)
			}
		}
		if now != last {
			j.add("%s: %s", m.Node.Label, clipLine(now, 160))
			last = now
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s was not back within %s (%s) — the rest of the cluster was not restarted", m.Node.Label, within, clipLine(now, 160))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// handleRollingInfo is the running or last rolling restart of {nid}'s cluster.
func (a *App) handleRollingInfo(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	t, err := a.groupTopology(st, r.PathValue("nid"), rebuildKinds, rebuildRefuse, false)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"supported": false, "reason": err.Error()})
		return
	}
	rebuildJobs.mu.Lock()
	j := rollingJobs.m[fmt.Sprintf("%d:%s", st.ID, t.Frame.ID)]
	rebuildJobs.mu.Unlock()
	resp := map[string]any{"supported": true, "kind": t.Kind, "cluster": t.Frame.Label, "members": len(t.Members),
		"canSwitch": t.Kind != "galera"}
	if j != nil {
		resp["job"] = j.snapshot()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRollingRestart restarts {nid}'s cluster one member at a time. Body: {"switchover": true}
// hands the primary to a restarted replica before the old primary goes.
func (a *App) handleRollingRestart(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		Switchover bool `json:"switchover"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body) // an empty body is no switchover
	t, err := a.groupTopology(st, r.PathValue("nid"), rebuildKinds, rebuildRefuse, true)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	lockKey := fmt.Sprintf("%d:%s", st.ID, t.Frame.ID)
	switchLocks.mu.Lock()
	if switchLocks.m[lockKey] {
		switchLocks.mu.Unlock()
		writeErr(w, http.StatusConflict, "a switchover, rebuild or rolling restart is already running on this cluster")
		return
	}
	switchLocks.m[lockKey] = true
	switchLocks.mu.Unlock()

	j := &rebuildJob{Label: t.Frame.Label, Method: "rolling restart", State: "running", StartedAt: time.Now().Unix()}
	rebuildJobs.mu.Lock()
	rollingJobs.m[lockKey] = j
	rebuildJobs.mu.Unlock()
	a.recordStackEvent(st.ID, "", "action", "info", "Rolling restart of "+t.Frame.Label+" started", "", u.Username)

	go func() {
		defer func() { switchLocks.mu.Lock(); delete(switchLocks.m, lockKey); switchLocks.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), rollingTimeout)
		defer cancel()
		err := a.runRolling(ctx, st, t, body.Switchover, u, j)
		clearLiveRoles(st.ID)
		rebuildJobs.mu.Lock()
		j.EndedAt = time.Now().Unix()
		if err != nil {
			j.State, j.Error = "failed", err.Error()
		} else {
			j.State = "done"
		}
		rebuildJobs.mu.Unlock()
		took := (time.Duration(j.EndedAt-j.StartedAt) * time.Second).String()
		if err != nil {
			j.add("stopped: %v", err)
			a.recordStackEvent(st.ID, "", "action", "error", "Rolling restart of "+t.Frame.Label+" stopped", err.Error(), u.Username)
			a.notify(Notification{UserID: u.ID, Scope: "user", Type: "stack.rolling", Severity: "error", StackID: st.ID,
				Title: "Rolling restart stopped", Body: fmt.Sprintf("%s: %s — %v", st.Name, t.Frame.Label, err)})
			return
		}
		a.recordStackEvent(st.ID, "", "action", "success", "Rolling restart of "+t.Frame.Label+" finished", "took "+took, u.Username)
		a.notify(Notification{UserID: u.ID, Scope: "user", Type: "stack.rolling", Severity: "success", StackID: st.ID,
			Title: "Rolling restart finished", Body: fmt.Sprintf("%s: every member of %s was restarted in %s.", st.Name, t.Frame.Label, took)})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"job": j.snapshot()})
}

func (a *App) runRolling(ctx context.Context, st Stack, t swTopo, switchover bool, u User, j *rebuildJob) error {
	restart := func(m swMember) error {
		markNodeAction(st.ID, m.Node.ID)
		j.add("restarting %s", m.Node.Label)
		if err := a.restartContainer(ctx, st, m.Node.ID); err != nil {
			return fmt.Errorf("restart %s: %v", m.Node.Label, err)
		}
		markNodeAction(st.ID, m.Node.ID)
		err := a.waitMemberHealthy(ctx, st, t, m, j, 15*time.Minute)
		markNodeAction(st.ID, m.Node.ID)
		return err
	}
	// Everybody must be healthy before anything is restarted: a rolling restart of a cluster that
	// is already short a member is how it ends up short two.
	states, pi := a.probeTopo(ctx, st, t)
	for _, s := range states {
		m, _ := t.member(s.NodeID)
		r := &liveRole{Role: s.Role, Access: s.Access, State: s.State, LagSec: s.LagSec, Problems: s.Problems, Down: s.Down, Err: s.Err}
		if !memberHealthy(t.Kind, r) {
			why := firstNonEmpty(s.Err, strings.Join(s.Problems, "; "), s.State, s.Role, "not healthy")
			return fmt.Errorf("%s is not healthy (%s) — fix it first; nothing was restarted", m.Node.Label, clipLine(why, 160))
		}
	}
	if t.Kind == "galera" {
		for _, m := range t.Members {
			if err := restart(m); err != nil {
				return err
			}
		}
		return nil
	}
	if pi < 0 {
		return errors.New("no member reports itself primary — nothing was restarted")
	}
	primary, _ := t.member(states[pi].NodeID)
	var replicas []swMember
	for _, m := range t.Members {
		if m.Node.ID != primary.Node.ID {
			replicas = append(replicas, m)
		}
	}
	j.add("%s is primary; restarting the %d other member(s) first", primary.Node.Label, len(replicas))
	for _, m := range replicas {
		if err := restart(m); err != nil {
			return err
		}
	}
	if switchover && len(replicas) > 0 {
		cand := replicas[0]
		j.add("handing the primary to %s before restarting %s", cand.Node.Label, primary.Node.Label)
		markPlannedSwitch(st.ID)
		steps, _, err := a.switchPrimary(ctx, st, t, cand.Node.ID, u)
		for _, s := range steps {
			j.add("  %s", s)
		}
		if err != nil {
			return fmt.Errorf("switchover to %s: %v", cand.Node.Label, err)
		}
	} else {
		j.add("restarting the primary %s in place", primary.Node.Label)
	}
	return restart(primary)
}
