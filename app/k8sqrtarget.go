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

// k8sDataGenUsable reports whether the Data Generator can fill this endpoint, and is
// the single rule both the listing and the re-resolve below go through — a target
// offered in one and refused by the other is a bug waiting to be filed.
//
// Two routes, because the generator has two. Its SQL engines run a client inside the
// container, so PostgreSQL needs the exec route and the endpoint that takes writes: a
// replica or a pooler would fail in a way that looks like a DBCanvas fault rather than
// like pointing an INSERT at a read-only endpoint. MySQL is left out because its
// client takes the password through MYSQL_PWD and `kubectl exec` carries no
// environment from the caller, so a cluster would need the password on a command line
// inside the pod — not a trade worth making silently.
//
// MongoDB does not exec at all: datagen_mongo.go dials with the driver over the stack
// network, exactly as the Query Runner and the Benchmark do. So what it needs is an
// address, not a pod, and the exec route's limits never applied to it. A config server
// is excluded because it is not where application data goes; a member is offered the
// way a replica-set node on the canvas is, and has to be the primary when the job runs
// — the server says so plainly (NotWritablePrimary) if it is not.
func k8sDataGenUsable(e k8sEndpoint) bool {
	if strings.HasSuffix(e.Kind, "-app") {
		return false
	}
	switch e.Engine {
	case dexPostgres:
		return e.Execable() && e.Role == "primary"
	case dexMongoDB:
		return e.Reachable() && e.Kind != "config"
	}
	return false
}

// k8sDataGenConnections are the operator databases the Data Generator can fill.
func (a *App) k8sDataGenConnections(ctx context.Context, st Stack) []dgConnection {
	out := []dgConnection{}
	for _, e := range a.k8sStackEndpoints(ctx, st) {
		if !k8sDataGenUsable(e) {
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

// k8sDBConn builds the Data Generator's connection for an operator database, by the
// route that endpoint's engine uses.
//
// For PostgreSQL the client runs inside the database's own pod, reached by kubectl on
// the k3s server container — so every psql invocation the generator already makes
// works unchanged, with a prefix in front of it. For MongoDB there is no client to
// run: the address travels on the connection and the driver dials it, which is the
// same thing mongoClientFor does for a node on the canvas once it has resolved one.
func (a *App) k8sDBConn(st Stack, target string) (dbConn, bool) {
	id, ok := k8sSplitTarget(target)
	if !ok {
		return dbConn{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, e := range a.k8sStackEndpoints(ctx, st) {
		if e.ID() != id || !k8sDataGenUsable(e) {
			continue
		}
		// A k3d cluster is always Docker, even in a hybrid stack.
		c := dbConn{Engine: e.Engine, Super: e.User, Password: e.Pass, StackID: st.ID, eng: a.docker}
		if e.Engine == dexMongoDB {
			c.Addr, c.Port = e.Addr, e.Port
			return c, true
		}
		c.ContainerID = e.ServerID
		c.Prefix = e.k8sExecArgv(nil)
		c.Env = []string{"KUBECONFIG=" + k3dKubeconfig}
		return c, true
	}
	return dbConn{}, false
}
