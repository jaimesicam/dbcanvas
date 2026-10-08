package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// drills.go — failure drills: break a cluster on purpose, the way it breaks in production, and
// follow a checklist of what should happen and what the DBA must do, each step ticked off by what
// the watcher (livewatch.go) actually saw. The point of a drill is that it is not simulated: the
// primary's container is really killed, replication really stops, and the alerts, the failover
// and the recovery are the real ones.
//
//   - kill-primary: the primary's container is stopped behind DBCanvas's back (its deployment
//     still says running, so nothing suppresses the alerts). Offered where the cluster fails over
//     and takes the old primary back by itself (Patroni, MongoDB, Group Replication) or has no
//     primary at all (Galera: any member).
//   - stop-replication: replication is stopped on a MySQL/MariaDB replica, which then drifts
//     behind; the fix is to start it again or rebuild the replica.
//
// One drill per stack at a time, kept in memory: a drill is something you watch.

type drillStep struct {
	Text string `json:"text"`
	Hint string `json:"hint,omitempty"`
	Done bool   `json:"done"`
	At   int64  `json:"at,omitempty"`
}

type drill struct {
	Kind      string      `json:"kind"`
	NodeID    string      `json:"nodeId"`
	Label     string      `json:"label"`
	Cluster   string      `json:"cluster"`
	FrameID   string      `json:"frameId"`
	TopoKind  string      `json:"topoKind"`
	StartedAt int64       `json:"startedAt"`
	EndedAt   int64       `json:"endedAt,omitempty"`
	Steps     []drillStep `json:"steps"`
}

var drills = struct {
	mu sync.Mutex
	m  map[int64]*drill
}{m: map[int64]*drill{}}

// drillKinds is which drills a cluster kind offers.
func drillKinds(topo string) []map[string]string {
	var out []map[string]string
	switch topo {
	case "patroni", "mongo", "gr":
		// Not repmgr: its old primary comes back as a second primary and needs `repmgr node
		// rejoin`, which is a drill of its own.
		out = append(out, map[string]string{"kind": "kill-primary", "title": "Kill the primary",
			"about": "The primary's container is stopped behind DBCanvas's back, as a crash would take it. The cluster should elect a new primary by itself; then you bring the old one back as a replica."})
	case "galera":
		out = append(out, map[string]string{"kind": "kill-primary", "title": "Kill a member",
			"about": "This member's container is stopped behind DBCanvas's back. The others should stay Synced and keep taking writes; then you bring it back and it rejoins."})
	}
	if topo == "mysql" || topo == "mariadb" {
		out = append(out, map[string]string{"kind": "stop-replication", "title": "Stop replication on this replica",
			"about": "Replication is stopped on this replica, which then falls behind. The alert should open; you get it replicating again (start it, or rebuild the replica) and the alert closes."})
	}
	return out
}

func (a *App) handleDrillInfo(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	resp := map[string]any{}
	if nid := r.PathValue("nid"); nid != "" {
		if t, err := a.groupTopology(st, nid, rebuildKinds, rebuildRefuse, false); err == nil {
			resp["offered"] = drillKinds(t.Kind)
		} else {
			resp["reason"] = err.Error()
		}
	}
	drills.mu.Lock()
	d := drills.m[st.ID]
	drills.mu.Unlock()
	if d != nil {
		a.evalDrill(r.Context(), st, d)
		resp["drill"] = d
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDrillStart is POST /api/stacks/{id}/nodes/{nid}/drill {"kind": …}; DELETE ends it.
func (a *App) handleDrillStart(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind string `json:"kind"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body)
	nid := r.PathValue("nid")
	drills.mu.Lock()
	if d := drills.m[st.ID]; d != nil && d.EndedAt == 0 {
		drills.mu.Unlock()
		writeErr(w, http.StatusConflict, "a drill is already running on this stack — finish it first")
		return
	}
	drills.mu.Unlock()
	t, err := a.groupTopology(st, nid, rebuildKinds, rebuildRefuse, false)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	offered := false
	for _, k := range drillKinds(t.Kind) {
		offered = offered || k["kind"] == body.Kind
	}
	if !offered {
		writeErr(w, http.StatusBadRequest, "that drill is not offered for this cluster")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	states, pi := a.probeTopo(ctx, st, t)
	target, _ := t.member(nid)
	d := &drill{Kind: body.Kind, Cluster: t.Frame.Label, FrameID: t.Frame.ID, TopoKind: t.Kind, StartedAt: time.Now().Unix()}
	switch body.Kind {
	case "kill-primary":
		if t.Kind != "galera" {
			if pi < 0 {
				writeErr(w, http.StatusConflict, "no member reports itself primary — the cluster is not healthy enough to drill")
				return
			}
			target, _ = t.member(states[pi].NodeID)
		}
		d.NodeID, d.Label = target.Node.ID, target.Node.Label
		d.Steps = []drillStep{
			{Text: "DBCanvas raises an alert that " + d.Label + " is down"},
		}
		if t.Kind == "galera" {
			d.Steps = append(d.Steps, drillStep{Text: "The other members stay Synced and keep taking writes"})
		} else {
			d.Steps = append(d.Steps, drillStep{Text: "Another member is elected primary, by the cluster itself",
				Hint: "Watch the canvas with Live view on: the primary chip moves. Proxies (HAProxy, PgBouncer, MySQL Router) follow it."})
		}
		d.Steps = append(d.Steps,
			drillStep{Text: "You bring " + d.Label + " back", Hint: "Right-click it → Start. It must rejoin as a replica, not as a second primary."},
			drillStep{Text: d.Label + " rejoins and catches up"},
			drillStep{Text: "Every alert of the drill is resolved"},
		)
		c := withEngine(ctx, a.depEngine(st, target.Node.ID))
		if err := a.engCtx(c).ContainerStop(c, target.Dep.ContainerID); err != nil {
			writeErr(w, http.StatusBadGateway, "kill "+target.Node.Label+": "+err.Error())
			return
		}
	case "stop-replication":
		if target.Node.ID == "" || (pi >= 0 && states[pi].NodeID == target.Node.ID) {
			writeErr(w, http.StatusConflict, "pick a replica for this drill")
			return
		}
		d.NodeID, d.Label = target.Node.ID, target.Node.Label
		d.Steps = []drillStep{
			{Text: "DBCanvas raises a replication alert on " + d.Label},
			{Text: "You get " + d.Label + " replicating again", Hint: "START REPLICA in its root console, or right-click it → Rebuild from primary."},
			{Text: d.Label + " catches up"},
			{Text: "Every alert of the drill is resolved"},
		}
		dlt := sqlDialect{MariaDB: t.Kind == "mariadb", Client: map[bool]string{true: "mariadb", false: "mysql"}[t.Kind == "mariadb"]}
		if v, err := a.asyncSQL(ctx, st, target, dlt, "SELECT @@version AS v"); err == nil {
			dlt = dialectFor(firstRow(v)["v"], dlt.MariaDB)
		}
		if _, err := a.asyncSQL(ctx, st, target, dlt, dlt.stop()); err != nil {
			writeErr(w, http.StatusBadGateway, "stop replication on "+target.Node.Label+": "+err.Error())
			return
		}
	}
	drills.mu.Lock()
	drills.m[st.ID] = d
	drills.mu.Unlock()
	a.recordStackEvent(st.ID, d.NodeID, "drill", "warning", fmt.Sprintf("Drill started: %s (%s)", map[string]string{"kill-primary": "kill " + d.Label, "stop-replication": "stop replication on " + d.Label}[d.Kind], d.Cluster), "", u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"drill": d})
}

func (a *App) handleDrillEnd(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	drills.mu.Lock()
	d := drills.m[st.ID]
	delete(drills.m, st.ID)
	drills.mu.Unlock()
	if d != nil {
		done := 0
		for _, s := range d.Steps {
			if s.Done {
				done++
			}
		}
		a.recordStackEvent(st.ID, d.NodeID, "drill", "info", fmt.Sprintf("Drill finished: %d of %d steps done", done, len(d.Steps)), "", u.Username)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ended"})
}

// evalDrill ticks the steps off from the timeline and the cluster as it is now. A step once done
// stays done.
func (a *App) evalDrill(ctx context.Context, st Stack, d *drill) {
	events, _ := a.store.LiveEvents(st.ID, d.StartedAt, 500)
	alerts, _ := a.store.LiveAlerts(st.ID)
	inFrame := map[string]bool{}
	for _, n := range buildDoc(st).Nodes {
		if n.FrameID == d.FrameID {
			inFrame[n.ID] = true
		}
	}
	firstEvent := func(match func(liveEvent) bool) int64 {
		var at int64
		for _, e := range events { // newest first
			if match(e) {
				at = e.At
			}
		}
		return at
	}
	t, _ := a.groupTopology(st, d.NodeID, rebuildKinds, rebuildRefuse, false)
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	states, _ := a.probeTopo(pctx, st, t)
	var me swState
	for _, s := range states {
		if s.NodeID == d.NodeID {
			me = s
		}
	}
	healthy := memberHealthy(d.TopoKind, &liveRole{Role: me.Role, State: me.State, LagSec: me.LagSec, Problems: me.Problems, Down: me.Down, Err: me.Err})
	openInFrame := 0
	for _, al := range alerts {
		if inFrame[al.NodeID] {
			openInFrame++
		}
	}
	tick := func(i int, at int64) {
		if i < len(d.Steps) && !d.Steps[i].Done && at > 0 {
			d.Steps[i].Done, d.Steps[i].At = true, at
		}
	}
	now := time.Now().Unix()
	switch d.Kind {
	case "kill-primary":
		tick(0, firstEvent(func(e liveEvent) bool { return e.Kind == "alert" && e.NodeID == d.NodeID }))
		if d.TopoKind == "galera" {
			others := 0
			for _, s := range states {
				if s.NodeID != d.NodeID && s.State == "Synced" {
					others++
				}
			}
			if others > 0 && d.Steps[0].Done {
				tick(1, now)
			}
		} else {
			at := firstEvent(func(e liveEvent) bool {
				return (e.Kind == "failover" || e.Kind == "role" && strings.Contains(e.Title, "is now primary")) && inFrame[e.NodeID] && e.NodeID != d.NodeID
			})
			if at == 0 {
				for _, s := range states {
					if s.NodeID != d.NodeID && s.Role == "primary" {
						at = now
					}
				}
			}
			tick(1, at)
		}
		tick(2, firstEvent(func(e liveEvent) bool {
			return e.Kind == "action" && e.NodeID == d.NodeID && strings.HasSuffix(e.Title, " started")
		}))
		if d.Steps[2].Done && healthy && (d.TopoKind == "galera" || me.Role != "primary" || d.Steps[1].Done) {
			tick(3, now)
		}
		if d.Steps[3].Done && openInFrame == 0 {
			tick(4, now)
		}
	case "stop-replication":
		tick(0, firstEvent(func(e liveEvent) bool { return e.Kind == "alert" && e.NodeID == d.NodeID }))
		if d.Steps[0].Done && healthy {
			tick(1, now)
			tick(2, now)
		}
		if d.Steps[2].Done && openInFrame == 0 {
			tick(3, now)
		}
	}
	all := true
	for _, s := range d.Steps {
		all = all && s.Done
	}
	if all && d.EndedAt == 0 {
		d.EndedAt = now
	}
}
