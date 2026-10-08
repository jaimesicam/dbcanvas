package main

import (
	"net/http"
	"sort"
)

// deploypreview.go — what pressing Deploy on a stack that is already deployed would do, before it
// does it. A redeploy is not a no-op that adds what is new: it removes the containers and volumes
// of every node deleted from the canvas, provisions a node that was never built or whose
// provisioning failed from scratch, and provisions a cluster as a
// unit, so a cluster with a new member (or one whose provisioning failed) has every member's
// container recreated, running ones too, with their data. A stopped node is left as it is. The preview follows handleDeployStack's own rules and says
// all of that per node, so nobody finds out from the deploy log.

type previewItem struct {
	NodeID  string `json:"nodeId"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	State   string `json:"state,omitempty"` // its deployment's state now, "" when never deployed
	Cluster string `json:"cluster,omitempty"`
	Why     string `json:"why"`
}

type deployPreview struct {
	Remove    []previewItem `json:"remove"`    // deleted from the canvas: container and volumes removed
	Create    []previewItem `json:"create"`    // never deployed: created
	Recreate  []previewItem `json:"recreate"`  // deployed but failed (error, pending): built again from scratch
	Rebuild   []previewItem `json:"rebuild"`   // running, but its cluster is provisioned again: recreated, data lost
	Unchanged int           `json:"unchanged"` // running or stopped, and left alone
}

// previewFrameTypes are the frames handleDeployStack provisions as a unit, members of their own type.
var previewFrameTypes = map[string]bool{
	"pxc": true, "proxysql": true, "mysql": true, "innodb": true, "mariadbrepl": true, "mariadbgalera": true,
	"mysqlcerepl": true, "mysqlceinnodb": true, "psmdb": true, "psmrs": true, "patroni": true, "repmgr": true,
	"spock": true, "valkeycluster": true, "k3d": true,
}

func previewDeploy(doc designDoc, deps []Deployment) deployPreview {
	p := deployPreview{Remove: []previewItem{}, Create: []previewItem{}, Recreate: []previewItem{}, Rebuild: []previewItem{}}
	existing := map[string]Deployment{}
	for _, d := range deps {
		existing[d.NodeID] = d
	}
	inDesign := map[string]bool{}
	for _, n := range doc.Nodes {
		inDesign[n.ID] = true
	}
	for _, d := range deps {
		if !inDesign[d.NodeID] {
			p.Remove = append(p.Remove, previewItem{NodeID: d.NodeID, Label: d.NodeID, State: d.State,
				Why: "deleted from the canvas — its container and volumes are removed"})
		}
	}
	running := func(id string) bool { d, ok := existing[id]; return ok && deployBuilt(d) } // running or stopped: left alone
	notRunning := func(n designNode, cluster string) previewItem {
		it := previewItem{NodeID: n.ID, Label: n.Label, Type: n.Type, Cluster: cluster}
		if d, ok := existing[n.ID]; ok {
			it.State = d.State
			it.Why = "deployed but " + d.State + " — provisioned again from scratch: a new container and an empty data directory"
			return it
		}
		it.Why = "new — created"
		return it
	}
	add := func(it previewItem) {
		if it.State == "" {
			p.Create = append(p.Create, it)
		} else {
			p.Recreate = append(p.Recreate, it)
		}
	}
	for _, n := range doc.Nodes {
		if n.FrameID != "" {
			continue
		}
		if running(n.ID) {
			p.Unchanged++
			continue
		}
		add(notRunning(n, ""))
	}
	for _, f := range doc.Frames {
		var members []designNode
		up := 0
		for _, n := range doc.Nodes {
			if n.FrameID == f.ID && n.Type == f.Type {
				members = append(members, n)
				if running(n.ID) {
					up++
				}
			}
		}
		if !previewFrameTypes[f.Type] || len(members) == 0 {
			continue
		}
		if up == len(members) {
			p.Unchanged += up
			continue
		}
		for _, n := range members {
			if running(n.ID) {
				p.Rebuild = append(p.Rebuild, previewItem{NodeID: n.ID, Label: n.Label, Type: n.Type, State: DeployRunning, Cluster: f.Label,
					Why: "built, but " + f.Label + " is provisioned as a whole because another member is new or failed — its container is recreated and its data is lost"})
				continue
			}
			add(notRunning(n, f.Label))
		}
	}
	for _, l := range [][]previewItem{p.Remove, p.Create, p.Recreate, p.Rebuild} {
		sort.Slice(l, func(i, j int) bool { return l[i].Cluster+l[i].Label < l[j].Cluster+l[j].Label })
	}
	return p
}

// handleDeployPreview is GET /api/stacks/{id}/deploy/preview.
func (a *App) handleDeployPreview(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read deployments")
		return
	}
	writeJSON(w, http.StatusOK, previewDeploy(buildDoc(st), deps))
}
