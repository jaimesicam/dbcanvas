package main

// k8sqrtarget.go — operator-deployed databases as Query Runner and Benchmark targets.
//
// These two tools measure things. A Query Runner thread and a Benchmark client each
// want a socket they can open many of and keep open, which rules out the exec route
// the Database Explorer falls back on: `kubectl exec` is a process per statement, and
// timing one would measure the process rather than the database.
//
// So only the network-reachable endpoints are offered here — a LoadBalancer with a
// MetalLB address, or a NodePort — and an operator cluster whose Services are all
// ClusterIP contributes nothing rather than something that would mislead. The
// Explorer says so in its own tree, and "Expose for tools" (k8sexpose.go) is how a
// cluster gets an address without its own Services being touched.
//
// They are deliberately NOT added to listSQLTargets. That list is also the Packet
// Inspector's, and the Packet Inspector runs tcpdump inside a node's container — an
// endpoint that is a Kubernetes Service has no such container, so putting one in that
// list would offer a capture that could never start.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// k8sTargetPrefix marks a target id as a Kubernetes endpoint rather than a node in the
// design. Node ids are canvas tokens and never contain a colon, so the two cannot be
// confused — and a tool that does not understand the prefix simply fails to resolve it.
const k8sTargetPrefix = "k8s:"

// k8sTargetID is the target id for one endpoint.
func k8sTargetID(e k8sEndpoint) string { return k8sTargetPrefix + e.ID() }

// k8sSplitTarget reports whether a target names a Kubernetes endpoint.
func k8sSplitTarget(target string) (string, bool) {
	return strings.CutPrefix(target, k8sTargetPrefix)
}

// listK8sSQLTargets is every operator database a load tool can actually dial, across
// the stacks this caller may reach.
func (a *App) listK8sSQLTargets(ctx context.Context, u User) []qrTarget {
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	out := []qrTarget{}
	for _, s := range stacks {
		st, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		for _, e := range a.k8sStackEndpoints(ctx, st) {
			if !e.Reachable() {
				continue
			}
			// MongoDB is dialled by the Benchmark but not the Query Runner; both
			// filter by engine themselves, so both engines are offered here.
			if e.Engine != dexMySQL && e.Engine != dexPostgres && e.Engine != dexMongoDB {
				continue
			}
			out = append(out, qrTarget{
				StackID: st.ID, StackName: st.Name,
				NodeID: k8sTargetID(e),
				Label:  e.Label + " (" + k3dOperatorLabel(e.Operator) + ")",
				Engine: e.Engine, Type: "k8s-" + e.Kind,
				Host: e.Addr, Port: e.Port,
			})
		}
	}
	return out
}

// k8sResolveTarget re-resolves a Kubernetes target for a caller, the same way every
// other target in the app is re-resolved: by asking the stack what it has now.
//
// It refuses an endpoint with no network address even though one exists in the tree,
// because the caller is a tool that needs a socket. The refusal says what to do about
// it, since "not reachable" on its own reads like a fault rather than a Service type.
func (a *App) k8sResolveTarget(ctx context.Context, u User, stackID int64, target string) (k8sEndpoint, error) {
	id, ok := k8sSplitTarget(target)
	if !ok {
		return k8sEndpoint{}, fmt.Errorf("not a Kubernetes target")
	}
	e, err := a.k8sFindEndpoint(ctx, u, stackID, id)
	if err != nil {
		return k8sEndpoint{}, err
	}
	if !e.Reachable() {
		return k8sEndpoint{}, fmt.Errorf(
			"%s has no address outside the cluster (%s). Use Expose for tools on the cluster's frame, "+
				"or set the Service type to LoadBalancer or NodePort in the frame's settings",
			e.Service, e.SvcType)
	}
	return e, nil
}

// k8sDialTarget is everything a load tool needs to open a connection to one endpoint.
type k8sDialTarget struct {
	Engine   string
	Label    string
	Addr     string
	Port     int
	User     string
	Pass     string
	Database string
	TLS      string
}

func (e k8sEndpoint) dialTarget() k8sDialTarget {
	return k8sDialTarget{
		Engine: e.Engine, Label: e.Label, Addr: e.Addr, Port: e.Port,
		User: e.User, Pass: e.Pass, Database: e.Database, TLS: e.TLS,
	}
}

// ---------------------------------------------------------------- Data Generator

// k8sDataGenConnections are the operator databases the Data Generator can fill.
//
// PostgreSQL only, and that is a limit of how the generator reaches a database rather
// than of what it can generate: its PostgreSQL path runs psql inside the container and
// its MySQL path pipes a password through MYSQL_PWD, and `kubectl exec` does not carry
// an environment from the caller — so a MySQL cluster would need the password on a
// command line inside the pod, which is not a trade worth making silently. An operator
// MySQL or MongoDB cluster is still reachable from the Database Explorer, the Query
// Runner and the Benchmark.
func (a *App) k8sDataGenConnections(ctx context.Context, st Stack) []dgConnection {
	out := []dgConnection{}
	for _, e := range a.k8sStackEndpoints(ctx, st) {
		if e.Engine != dexPostgres || !e.Execable() {
			continue
		}
		// One connection per cluster, not per tier: the generator writes, so only the
		// endpoint that takes writes is worth offering, and a replica or a pooler
		// would fail in a way that looks like a DBCanvas fault.
		if e.Role != "primary" || strings.HasSuffix(e.Kind, "-app") {
			continue
		}
		out = append(out, dgConnection{
			StackID: st.ID, StackName: st.Name,
			NodeID: k8sTargetID(e),
			Label:  e.Label + " (" + k3dOperatorLabel(e.Operator) + ")",
			Engine: e.Engine, Type: "k8s-" + e.Kind,
		})
	}
	return out
}

// k8sDBConn builds the Data Generator's connection for an operator database. The
// client runs inside the database's own pod, reached by kubectl on the k3s server
// container — so every psql invocation the generator already makes works unchanged,
// with a prefix in front of it.
func (a *App) k8sDBConn(st Stack, target string) (dbConn, bool) {
	id, ok := k8sSplitTarget(target)
	if !ok {
		return dbConn{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, e := range a.k8sStackEndpoints(ctx, st) {
		if e.ID() != id || !e.Execable() || e.Engine != dexPostgres {
			continue
		}
		return dbConn{
			ContainerID: e.ServerID,
			Engine:      e.Engine,
			Super:       e.User,
			Password:    e.Pass,
			StackID:     st.ID,
			eng:         a.docker, // a k3d cluster is always Docker, even in a hybrid stack
			Prefix:      e.k8sExecArgv(nil),
			Env:         []string{"KUBECONFIG=" + k3dKubeconfig},
		}, true
	}
	return dbConn{}, false
}
