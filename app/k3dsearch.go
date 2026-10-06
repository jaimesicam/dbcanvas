package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// k3dsearch.go — vector search on a K3D frame running the Percona Operator for MongoDB.
//
// From 1.23.0 the operator manages Percona Search for MongoDB (mongot) itself, as a tech
// preview: `spec.search.enabled` gives every data-bearing replica set a one-pod StatefulSet
// named <cluster>-<rs>-search with its own PVC, creates the searchCoordinator user, and
// injects the mongotHost / searchIndexManagementHostAndPort parameters into mongod (and
// mongos). What it does not do is pick a server that can use it: the cr.yaml it ships
// runs PSMDB 8.0, and mongot needs 8.3 — "you must explicitly specify Percona Server for
// MongoDB 8.3 images" is the release note's own instruction. So the frame's one checkbox
// is two edits to cr.yaml: spec.search, and spec.image.
//
// The images are pinned here rather than in versions.yaml. The operator certifies no 8.3
// image yet (its 1.23.0 certified list stops at 8.0.26-11) and ships the search image
// commented out under perconalab/, so there is no catalog of supported combinations to
// discover — these are the newest of each on Docker Hub's percona/ organisation when this
// was written, and a frame that needs another can still edit cr.yaml in /root.

const (
	// psmdbSearchMinVer is the first operator release whose CRD has spec.search.
	psmdbSearchMinVer = "1.23.0"
	// psmdbSearchServerImage is the PSMDB 8.3 server a search-enabled cluster runs.
	psmdbSearchServerImage = "percona/percona-server-mongodb:8.3.11-3"
	// psmdbSearchImage is Percona Search for MongoDB (mongot).
	psmdbSearchImage = "percona/percona-search-mongodb:1.70.4-2"
)

// psmdbHasSearch reports whether an operator version understands spec.search. An empty
// version is "unknown", not "newest", for psmdbHasVault's reason.
func psmdbHasSearch(operatorVer string) bool {
	v := strings.TrimSpace(operatorVer)
	return v != "" && compareVersions(v, psmdbSearchMinVer) >= 0
}

// psmdbSearchBlock is spec.search for a lab cluster: one mongot per replica set (the only
// size the operator accepts) and a 5 GiB volume. No resources — the transform comments
// every CPU/memory request out, and the shipped 2 CPU / 2 GiB request alone would not
// schedule beside three mongod pods on a small k3d cluster.
//
// configuration turns on mongot's Prometheus endpoint (:9946), which the operator's
// generated mongot.conf ships with `metrics: enabled: false`. spec.search.configuration is
// mongot YAML the operator merges over its defaults — the documented knob for this.
func psmdbSearchBlock() string {
	return `search:
  enabled: true
  image: ` + psmdbSearchImage + `
  size: 1
  configuration: |
    metrics:
      enabled: true
  storage:
    persistentVolumeClaim:
      resources:
        requests:
          storage: 5Gi`
}

// k3dSearchIssues validates a K3D frame's vector search option.
func k3dSearchIssues(f designFrame, opCat OperatorCatalog) []issue {
	if !f.K3DVectorSearch {
		return nil
	}
	name := f.Label
	if f.K3DOperator != "psmdb" {
		return []issue{{Level: "warning", Message: "K3D cluster " + name + " has vector search on, which only the " +
			"Percona Operator for MongoDB provides — it is ignored for " + orDefault(k3dOperatorLabel(f.K3DOperator), "a cluster with no operator")}}
	}
	ver, ok := opCat.resolveOperatorVersion("psmdb", f.K3DOperatorVer)
	switch {
	case !ok:
		return []issue{{Level: "error", Message: "K3D cluster " + name + " asks for vector search, which needs a known " +
			"operator version to check against " + psmdbSearchMinVer + " — pick one from the list, or run `make versions`"}}
	case !psmdbHasSearch(ver):
		return []issue{{Level: "error", Message: "K3D cluster " + name + " asks for vector search, which the Percona " +
			"Operator for MongoDB only has from " + psmdbSearchMinVer + " — this frame pins " + ver + ", whose CRD has no " +
			"`spec.search`. Choose " + psmdbSearchMinVer + " or newer, or turn vector search off"}}
	}
	return nil
}

// psmdbSearchStuckReady recognises a cluster that is up but that operator 1.23.x will never
// call ready. With spec.search on and the replica set exposed (LoadBalancer or ClusterIP — which
// is how anything outside Kubernetes reaches it, Support Sim included), the operator's status
// code lists the replica set's pods by their replset label, and the mongot pod
// (<cluster>-rs0-search-0) carries that label too. It then looks for a per-pod Service named
// after the mongot pod, finds none, and leaves status.host empty — and "ready" requires a host.
// Found from a goroutine dump of the operator (updateStatus → connectionEndpoint →
// GetReplsetAddrs → getExtServices), and verified on 1.23.1. A sharded cluster's endpoint is
// mongos, so it is unaffected.
//
// So: state "initializing", every replica set "ready", every mongot "ready", and no host is
// that bug and nothing else — anything genuinely still starting fails one of the first three.
func (a *App) psmdbSearchStuckReady(ctx context.Context, serverID string, cfg k3dConfig) bool {
	out, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "get", "psmdb", cfg.ClusterName, "-o", "jsonpath={.status}")
	if err != nil {
		return false
	}
	return psmdbSearchStatusStuck([]byte(out))
}

// psmdbSearchStatusStuck is the decision, on the CR's status JSON.
func psmdbSearchStatusStuck(status []byte) bool {
	var st struct {
		State    string `json:"state"`
		Host     string `json:"host"`
		Replsets map[string]struct {
			Status string `json:"status"`
		} `json:"replsets"`
		Search map[string]struct {
			Status string `json:"status"`
		} `json:"search"`
	}
	if json.Unmarshal(status, &st) != nil || st.State != "initializing" || st.Host != "" ||
		len(st.Replsets) == 0 || len(st.Search) == 0 {
		return false
	}
	for _, rs := range st.Replsets {
		if rs.Status != "ready" {
			return false
		}
	}
	for _, s := range st.Search {
		if s.Status != "ready" {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- exposing it ourselves

// psmdbSearchExposeMode decides how a search-enabled replica set is exposed, given the
// expose type the frame asked for. It exists because of the status bug above
// (psmdbSearchStuckReady): with spec.search on, the operator's own replsets.expose of type
// LoadBalancer or ClusterIP leaves the cluster "initializing" for good. NodePort takes a
// different branch of connectionEndpoint and is unaffected (verified live), and a sharded
// cluster's endpoint is mongos. So:
//
//	""         — not affected: the operator exposes as asked
//	"none"     — ClusterIP asked for: leave the replica set unexposed (per-pod ClusterIP
//	             Services reach nothing outside the cluster anyway)
//	"nodeport" — LoadBalancer asked for: the operator exposes the members as NodePort, and
//	             DBCanvas adds a LoadBalancer per member beside it (<cluster>-rs0-<i>-ext) for
//	             the dedicated address and port 27017 a LoadBalancer was chosen for
//
// The exposure stays the operator's own, documented option, and the operator reports
// "ready" as it should.
func psmdbSearchExposeMode(search, sharding bool, exposeType string) string {
	if !search || sharding {
		return ""
	}
	switch exposeType {
	case "LoadBalancer":
		return "nodeport"
	case "ClusterIP":
		return "none"
	}
	return ""
}

// psmdbExtSuffix names DBCanvas's Services beside the operator's: <pod>-ext.
const psmdbExtSuffix = "-ext"

// psmdbSearchServicesYAML is what DBCanvas adds beside the operator's NodePorts: a
// LoadBalancer per mongod pod (<cluster>-rs0-<i>-ext), selecting the same pod, so each
// member has its own address on 27017; and one for mongot's metrics and health ports,
// which the operator keeps in-cluster (its headless <cluster>-rs0-search Service), so the
// Support Sim's Index Workshop can read them from outside.
//
// The names differ from the operator's and the labels are DBCanvas's: the operator
// createOrUpdates Services by its own names (it took over same-named ones when tried) and
// removeOutdatedServices deletes only Services labelled as its own external ones — so these
// are never adopted, changed or removed by it.
func psmdbSearchServicesYAML(cluster, rs string, size int) string {
	var b strings.Builder
	svc := func(name, podName string, ports [][2]any) {
		fmt.Fprintf(&b, `---
apiVersion: v1
kind: Service
metadata:
  name: %s
  labels:
    app.kubernetes.io/managed-by: dbcanvas
    app.kubernetes.io/instance: %s
  annotations:
    dbcanvas/why: "spec.search is on: the operator exposes the members as NodePort (LoadBalancer leaves 1.23.x initializing); this adds the LoadBalancer address"
spec:
  type: LoadBalancer
  selector:
    statefulset.kubernetes.io/pod-name: %s
  ports:
`, name, cluster, podName)
		for _, p := range ports {
			fmt.Fprintf(&b, "    - name: %s\n      port: %d\n      targetPort: %d\n", p[0], p[1], p[1])
		}
	}
	for i := 0; i < size; i++ {
		pod := fmt.Sprintf("%s-%s-%d", cluster, rs, i)
		svc(pod+psmdbExtSuffix, pod, [][2]any{{"mongodb", 27017}})
	}
	svc(cluster+"-"+rs+"-search"+psmdbExtSuffix, cluster+"-"+rs+"-search-0", [][2]any{{"metrics", mongotMetricsPort}, {"health", mongotHealthPort}})
	return b.String()
}

// k3dMongotMetrics is the address of a K3D cluster's mongot metrics endpoint, as the
// Support Sim (outside Kubernetes) can reach it: the -search-ext LoadBalancer DBCanvas
// created beside the replica set's. "" when there is none — a cluster exposed some other
// way — and the sim's panel then says mongot is only reachable in-cluster.
func (a *App) k3dMongotMetrics(ctx context.Context, st Stack, doc designDoc, frameID string) string {
	cfg, serverID, ok := a.k3dServerConfig(st.ID, doc, frameID)
	if !ok || cfg.Operator != "psmdb" {
		return ""
	}
	svcs, err := a.k3dServices(ctx, serverID, cfg.Namespace)
	if err != nil {
		return ""
	}
	svc, found := findService(svcs, cfg.ClusterName+"-rs0-search"+psmdbExtSuffix)
	if !found {
		return ""
	}
	ep, _, err := a.resolveService(ctx, serverID, svc, mongotMetricsPort, "metrics")
	if err != nil {
		return ""
	}
	return ep.addr()
}
