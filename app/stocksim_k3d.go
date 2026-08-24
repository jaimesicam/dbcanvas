package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// stocksim_k3d.go — resolving a K3D cluster frame to something a Stock Market
// Sim node can connect to, for **every** operator DBCanvas can install, not
// just CloudNativePG.
//
// A K3D frame is unlike every other linked target in three ways, which is why
// it gets its own file rather than another case in the family resolvers:
//
//   - **There is no canvas node to wait on.** The frame's members are k3s
//     hosts; the database is a Kubernetes object inside them. What there is to
//     wait for is the k3dConfig the deploy recorded on the server node.
//   - **The engine is a property of the operator**, not of the frame type. A
//     `k3d` frame is PostgreSQL under CNPG, Crunchy PGO or the Percona
//     PostgreSQL operator, MySQL under PXC or PS, and MongoDB under PSMDB.
//   - **Reachability is a deliberate choice the user made on the frame.** Every
//     operator's Services default to ClusterIP, and a ClusterIP is a `.svc`
//     name that means nothing outside Kubernetes. The k3d cluster is on the
//     stack network precisely so a MetalLB LoadBalancer address — or a NodePort
//     on a k3s node's own address — *is* routable from a sibling container. So
//     an unreachable cluster is not a failure to look something up: it is a
//     setting to change, and the error says which one.
//
// Credentials are read out of the cluster rather than assumed. DBCanvas seeds
// every operator's secret from `.env`, but the cluster is the source of truth —
// a secret edited by hand, or an operator release that renames a key, should
// change what the sim connects with rather than silently break it.

// k3dOperatorEngine maps a K3D frame's operator to the engine a Stock Market
// Sim node would speak to it. Empty for a frame with no operator selected — a
// bare Kubernetes cluster has no database in it to drive.
func k3dOperatorEngine(operator string) string {
	switch strings.TrimSpace(operator) {
	case "pxc", "ps":
		return "mysql"
	case "psmdb":
		return "mongodb"
	case "pg", "cnpg", "pgo":
		return "postgres"
	}
	return ""
}

// ---------------------------------------------------------------- Services

// kubeService is the part of a Service the endpoint resolution needs. Read as
// JSON rather than through a stack of `-o jsonpath` calls because picking an
// address needs the type, the ports and the LoadBalancer status together.
type kubeService struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Type      string `json:"type"`
		ClusterIP string `json:"clusterIP"`
		Ports     []struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			NodePort int    `json:"nodePort"`
		} `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

// k3dServices lists every Service in a namespace, in one kubectl call.
func (a *App) k3dServices(ctx context.Context, serverID, ns string) ([]kubeService, error) {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "svc", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []kubeService `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("read the Services in namespace %q: %w", ns, err)
	}
	return list.Items, nil
}

func findService(svcs []kubeService, name string) (kubeService, bool) {
	for _, s := range svcs {
		if s.Metadata.Name == name {
			return s, true
		}
	}
	return kubeService{}, false
}

// servicesWithPrefix returns every Service whose name starts with prefix, in
// name order — which for the per-pod Services an operator creates (…-rs0-0,
// …-rs0-1) is also member order.
func servicesWithPrefix(svcs []kubeService, prefix string) []kubeService {
	var out []kubeService
	for _, s := range svcs {
		if strings.HasPrefix(s.Metadata.Name, prefix) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

// servicePort picks the port to connect on: the first of preferred names that
// the Service actually declares, else the one whose number is def, else the
// first port. Named ports are checked first because the number moves — the PS
// operator's MySQL Router answers read/write on 6446, not 3306 — while the name
// (`rw`, `mysql`, `mongos`, `postgres`) is stable across operator releases.
func servicePort(s kubeService, def int, preferred ...string) (port, nodePort int, ok bool) {
	if len(s.Spec.Ports) == 0 {
		return 0, 0, false
	}
	for _, want := range preferred {
		for _, p := range s.Spec.Ports {
			if p.Name == want {
				return p.Port, p.NodePort, true
			}
		}
	}
	for _, p := range s.Spec.Ports {
		if p.Port == def {
			return p.Port, p.NodePort, true
		}
	}
	p := s.Spec.Ports[0]
	return p.Port, p.NodePort, true
}

// k3dNodeIP is a k3s node's address on the stack network — what a NodePort has
// to be dialled on. The cluster is created with `--network dbcanvas-stack-<id>`,
// so a node's InternalIP is an address a sibling container can route to.
func (a *App) k3dNodeIP(ctx context.Context, serverID string) string {
	out, err := a.kubectl(ctx, serverID, "get", "nodes", "-o",
		`jsonpath={.items[*].status.addresses[?(@.type=="InternalIP")].address}`)
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(out) {
		if f != "" {
			return f
		}
	}
	return ""
}

// k3dEndpoint is one Service resolved to an address reachable from outside
// Kubernetes, or the reason there isn't one.
type k3dEndpoint struct {
	host string
	port int
	svc  string // the Service it came from, for the error messages
	typ  string // its Service type, likewise
}

func (e k3dEndpoint) addr() string { return fmt.Sprintf("%s:%d", e.host, e.port) }

// resolveService turns one Service into a routable host:port, or reports why it
// is not routable. A LoadBalancer whose address is still pending and a
// ClusterIP are different problems — the first is worth waiting for, the second
// never resolves — so they are distinguished by `retry`.
func (a *App) resolveService(ctx context.Context, serverID string, s kubeService, def int, preferred ...string) (ep k3dEndpoint, retry bool, err error) {
	name := s.Metadata.Name
	port, nodePort, ok := servicePort(s, def, preferred...)
	if !ok {
		return k3dEndpoint{}, false, fmt.Errorf("Service %s declares no ports", name)
	}
	switch s.Spec.Type {
	case "LoadBalancer":
		for _, in := range s.Status.LoadBalancer.Ingress {
			if addr := strings.TrimSpace(in.IP); addr != "" {
				return k3dEndpoint{host: addr, port: port, svc: name, typ: s.Spec.Type}, false, nil
			}
			if addr := strings.TrimSpace(in.Hostname); addr != "" {
				return k3dEndpoint{host: addr, port: port, svc: name, typ: s.Spec.Type}, false, nil
			}
		}
		return k3dEndpoint{}, true, fmt.Errorf(
			"Service %s is a LoadBalancer with no address yet — MetalLB may have run its pool dry", name)
	case "NodePort":
		if nodePort == 0 {
			return k3dEndpoint{}, true, fmt.Errorf("Service %s has no node port assigned yet", name)
		}
		ip := a.k3dNodeIP(ctx, serverID)
		if ip == "" {
			return k3dEndpoint{}, true, fmt.Errorf("no k3s node address to reach Service %s on", name)
		}
		return k3dEndpoint{host: ip, port: nodePort, svc: name, typ: s.Spec.Type}, false, nil
	}
	return k3dEndpoint{}, false, fmt.Errorf(
		"Service %s is %s, which exists only inside Kubernetes", name, orDefault(s.Spec.Type, "ClusterIP"))
}

// k3dPickEndpoint walks a preference list of Service names and returns the
// first that resolves. Every candidate's own reason for failing is kept, so a
// cluster nothing can be reached on says what each tier was rather than one
// generic "not exposed".
//
// ok and retry are separate returns on purpose: a caller that collapses them
// gets a zero endpoint back on a cluster that is merely still starting, and
// then dials `:0`.
func (a *App) k3dPickEndpoint(ctx context.Context, serverID string, svcs []kubeService, def int, preferredPorts []string, names ...string) (ep k3dEndpoint, ok, retry bool, why []string) {
	for _, name := range names {
		s, found := findService(svcs, name)
		if !found {
			why = append(why, "Service "+name+" does not exist yet")
			retry = true
			continue
		}
		got, again, err := a.resolveService(ctx, serverID, s, def, preferredPorts...)
		if err == nil {
			return got, true, false, nil
		}
		retry = retry || again
		why = append(why, err.Error())
	}
	return k3dEndpoint{}, false, retry, why
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------- the resolver

// stockSimK3DTarget resolves a database running inside a K3D frame, whichever
// of the six operators built it.
//
// It polls, like every other waitStockSim*Target, because the frame and the sim
// node are deployed concurrently: the operator may still be reconciling, and a
// LoadBalancer address arrives a few seconds after the Service does. What it
// does *not* retry is a settled misconfiguration — a frame with no operator, or
// a database exposed only as ClusterIP — because waiting cannot fix either, and
// a user staring at a deploy log deserves the answer now rather than at the
// timeout.
func (a *App) stockSimK3DTarget(ctx context.Context, st Stack, doc designDoc, frameID string, timeout time.Duration, logln func(string)) (stockSimResolved, error) {
	frame := frameByID(doc, frameID)
	deadline := time.Now().Add(timeout)
	// What to say if the timeout wins. Until the frame's server node reports a
	// k3dConfig there is nothing to blame but the frame itself, so the reason
	// starts as that and is replaced by the resolver's own once there is one.
	lastWhy := []string{"the Kubernetes cluster's first node has not finished provisioning"}

	for {
		cfg, serverID, ok := a.k3dServerConfig(st.ID, doc, frameID)
		if ok {
			engine := k3dOperatorEngine(cfg.Operator)
			if engine == "" {
				return stockSimResolved{}, fmt.Errorf(
					"the linked Kubernetes cluster %s runs no database operator — choose one on the frame (PXC, Percona Server, PSMDB, Percona PostgreSQL, CloudNativePG or Crunchy PGO), or link this node to a database elsewhere in the stack",
					frame.Label)
			}
			res, retry, err := a.k3dResolveOperator(ctx, serverID, frame, cfg)
			switch {
			case err == nil:
				return res, nil
			case !retry:
				return stockSimResolved{}, err
			}
			// Each distinct reason once, so the deploy log reads as a cluster
			// coming up rather than as a hang — but a hundred identical lines
			// while the operator does its work is not progress either.
			if logln != nil && (len(lastWhy) != 1 || lastWhy[0] != err.Error()) {
				logln(err.Error())
			}
			lastWhy = []string{err.Error()}
		}
		if time.Now().After(deadline) {
			return stockSimResolved{}, fmt.Errorf(
				"%s in %s did not report a reachable endpoint within %s: %s",
				k3dOperatorLabel(frame.K3DOperator), frame.Label, timeout, strings.Join(lastWhy, "; "))
		}
		select {
		case <-ctx.Done():
			return stockSimResolved{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// k3dResolveOperator does one attempt for whichever operator the frame runs.
// retry is true when the answer could still change on its own — the operator is
// still creating Services, or a LoadBalancer address has not landed.
func (a *App) k3dResolveOperator(ctx context.Context, serverID string, frame designFrame, cfg k3dConfig) (res stockSimResolved, retry bool, err error) {
	if cfg.Operator == "cnpg" {
		return a.k3dCNPGResolve(ctx, serverID, frame, cfg)
	}

	// The four Percona operators record their k3dConfig as soon as cr.yaml is
	// applied, so a K3D node reports Running while the database inside it is
	// still starting — a Service with a LoadBalancer address exists minutes
	// before anything answers on it. Every other linked target blocks until the
	// database is actually up, and this is how that promise is kept here.
	//
	// Crunchy PGO is exempt: it is chart-installed, has no `.status.state`
	// under a short name that matches its operator key, and installPGOOperator
	// already waited for the cluster before recording anything.
	if cfg.Operator != "pgo" {
		if state, ready := a.k3dCRState(ctx, serverID, cfg); !ready {
			if state == "error" {
				return stockSimResolved{}, false, fmt.Errorf(
					"%s in %s reports state %q — the cluster did not come up, so there is nothing to connect to",
					k3dOperatorLabel(cfg.Operator), frame.Label, state)
			}
			return stockSimResolved{}, true, fmt.Errorf(
				"%s in %s is %s, not ready yet", k3dOperatorLabel(cfg.Operator), frame.Label, orDefault(state, "still starting"))
		}
	}

	svcs, serr := a.k3dServices(ctx, serverID, cfg.Namespace)
	if serr != nil {
		return stockSimResolved{}, true, serr
	}
	switch cfg.Operator {
	case "pxc":
		return a.k3dPXCResolve(ctx, serverID, frame, cfg, svcs)
	case "ps":
		return a.k3dPSResolve(ctx, serverID, frame, cfg, svcs)
	case "psmdb":
		return a.k3dPSMDBResolve(ctx, serverID, frame, cfg, svcs)
	case "pg", "pgo":
		return a.k3dPGResolve(ctx, serverID, frame, cfg, svcs)
	}
	return stockSimResolved{}, false, fmt.Errorf("unsupported Kubernetes operator %q", cfg.Operator)
}

// k3dCRState reads the custom resource's own `.status.state`, which is how each
// of the four Percona operators reports whether the database it manages is up.
// Their CR short names are the operator keys DBCanvas already uses (pxc, ps,
// psmdb, pg), so one call covers all four.
//
// A CR that cannot be read at all counts as "not ready yet" rather than an
// error: the CRD may not be established the instant after the bundle lands.
func (a *App) k3dCRState(ctx context.Context, serverID string, cfg k3dConfig) (state string, ready bool) {
	out, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "get", cfg.Operator, cfg.ClusterName,
		"-o", "jsonpath={.status.state}")
	if err != nil {
		return "", false
	}
	state = strings.ToLower(strings.TrimSpace(out))
	return state, state == "ready"
}

// k3dExposeAdvice is the sentence every "nothing is reachable" error ends with.
// The tiers are named the way the frame's own form names them, because that is
// where the setting the user has to change lives.
func k3dExposeAdvice(frame designFrame, tiers string, why []string) error {
	msg := fmt.Sprintf("%s in %s is only reachable inside Kubernetes",
		k3dOperatorLabel(frame.K3DOperator), frame.Label)
	if len(why) > 0 {
		msg += " (" + strings.Join(why, "; ") + ")"
	}
	return fmt.Errorf("%s — set %s to LoadBalancer or NodePort on the Kubernetes frame so this application can reach it", msg, tiers)
}

// k3dSecretOr reads one key from a Secret, falling back to what DBCanvas seeded
// the cluster with. The cluster wins: DBCanvas writes these secrets from `.env`
// at deploy, but a password rotated in Kubernetes afterwards is the real one.
func (a *App) k3dSecretOr(ctx context.Context, serverID, ns, secret, key, fallback string) string {
	if v := a.cnpgSecretValue(ctx, serverID, ns, secret, key); v != "" {
		return v
	}
	return fallback
}

// ---------------------------------------------------------------- MySQL: PXC

// k3dPXCResolve resolves a Percona XtraDB Cluster to its front door.
//
// The proxy is preferred over the database pods for the reason a proxy exists:
// HAProxy's primary Service and ProxySQL both route writes to whichever member
// is currently taking them, so a failover is invisible to the sim. The per-pod
// Services (cluster1-pxc-0, …) are the fallback for a cluster deployed with no
// proxy exposed — any PXC member accepts writes, so member 0 is as good as any.
func (a *App) k3dPXCResolve(ctx context.Context, serverID string, frame designFrame, cfg k3dConfig, svcs []kubeService) (stockSimResolved, bool, error) {
	cr := cfg.ClusterName
	names := []string{cr + "-haproxy"}
	if cfg.Proxy == "proxysql" {
		names = []string{cr + "-proxysql"}
	}
	for _, s := range servicesWithPrefix(svcs, cr+"-pxc-") {
		if !strings.HasSuffix(s.Metadata.Name, "-unready") {
			names = append(names, s.Metadata.Name)
		}
	}
	ep, ok, retry, why := a.k3dPickEndpoint(ctx, serverID, svcs, pxcMySQLPort, []string{"mysql"}, names...)
	if !ok {
		return stockSimResolved{}, retry, k3dExposeAdvice(frame, "the proxy (or the PXC pods)", why)
	}
	pass := a.k3dSecretOr(ctx, serverID, cfg.Namespace, cr+"-secrets", "root", "")
	if pass == "" {
		return stockSimResolved{}, true, fmt.Errorf(
			"could not read the root password out of Secret %q in namespace %q", cr+"-secrets", cfg.Namespace)
	}
	return k3dMySQLResolved(frame, cfg, ep, "root", pass), false, nil
}

// ---------------------------------------------------------------- MySQL: PS

// k3dPSResolve resolves a Percona Server for MySQL cluster. Its front end is
// either HAProxy or MySQL Router; Router answers read/write on 6446 rather than
// 3306, which is why the port is picked by name. `-mysql-primary` is the
// fallback: the operator's exposePrimary Service, which follows the primary.
func (a *App) k3dPSResolve(ctx context.Context, serverID string, frame designFrame, cfg k3dConfig, svcs []kubeService) (stockSimResolved, bool, error) {
	cr := cfg.ClusterName
	front := cr + "-haproxy"
	if psProxy(cfg.Proxy, psClusterType(cfg.ClusterType)) == "router" {
		front = cr + "-router"
	}
	ep, ok, retry, why := a.k3dPickEndpoint(ctx, serverID, svcs, pxcMySQLPort,
		[]string{"rw", "mysql"}, front, cr+"-mysql-primary")
	if !ok {
		return stockSimResolved{}, retry, k3dExposeAdvice(frame, "the proxy (or the MySQL primary)", why)
	}
	pass := a.k3dSecretOr(ctx, serverID, cfg.Namespace, cr+"-secrets", "root", "")
	if pass == "" {
		return stockSimResolved{}, true, fmt.Errorf(
			"could not read the root password out of Secret %q in namespace %q", cr+"-secrets", cfg.Namespace)
	}
	return k3dMySQLResolved(frame, cfg, ep, "root", pass), false, nil
}

// k3dMySQLResolved is the common tail of the two MySQL operators: the DSN, and
// the profile fields the node panel renders.
func k3dMySQLResolved(frame designFrame, cfg k3dConfig, ep k3dEndpoint, user, pass string) stockSimResolved {
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?tls=false", user, pass, ep.addr())
	return stockSimResolved{
		env:         []string{"DB_ENGINE=mysql", "MYSQL_DSN=" + dsn},
		secrets:     stockSimSecrets{User: user, Password: pass},
		engine:      "mysql",
		kind:        "k3d-" + cfg.Operator,
		displayName: k3dTargetName(frame, cfg, ep),
		host:        ep.host, port: ep.port,
	}
}

// ---------------------------------------------------------------- MongoDB

// k3dPSMDBResolve resolves a Percona Server for MongoDB cluster.
//
// A sharded cluster is the easy shape: mongos is a stateless router, so one
// address is the whole cluster, and the driver never has to know what is behind
// it.
//
// A replica set is not, and the reason is worth stating because the obvious
// approach does not work. Hand a driver the members' external addresses as
// seeds and it will connect, ask one of them for the set's configuration, and
// *replace* those seeds with what it finds — which is
// `<cluster>-rs0-0.<cluster>-rs0.<ns>.svc.cluster.local:27017`, verified live on
// a cluster whose members were each on their own MetalLB address. Exposing the
// members does not change the replica set's own configuration; only
// `clusterServiceDNSMode: External` does, and that changes the addresses
// *in-cluster* clients get too — PBM, the PMM sidecar and the operator itself —
// so it is not a switch to flip for one application's benefit.
//
// So the sim is pointed at the member that currently holds the primary role,
// with `directConnection=true` to stop the driver from rediscovering its way
// back inside the cluster. The cost is honest and bounded: an election moves
// the primary, and the sim node has to be redeployed to follow it. Every write
// endpoint DBCanvas resolves for a replica set elsewhere is a name that follows
// the primary; this is the one that cannot be.
func (a *App) k3dPSMDBResolve(ctx context.Context, serverID string, frame designFrame, cfg k3dConfig, svcs []kubeService) (stockSimResolved, bool, error) {
	cr := cfg.ClusterName
	user := a.k3dSecretOr(ctx, serverID, cfg.Namespace, cr+"-secrets", "MONGODB_DATABASE_ADMIN_USER", "databaseAdmin")
	pass := a.k3dSecretOr(ctx, serverID, cfg.Namespace, cr+"-secrets", "MONGODB_DATABASE_ADMIN_PASSWORD", "")
	if pass == "" {
		return stockSimResolved{}, true, fmt.Errorf(
			"could not read MONGODB_DATABASE_ADMIN_PASSWORD out of Secret %q in namespace %q", cr+"-secrets", cfg.Namespace)
	}
	creds := url.QueryEscape(user) + ":" + url.QueryEscape(pass)

	if cfg.Sharding {
		ep, ok, retry, why := a.k3dPickEndpoint(ctx, serverID, svcs, mongoPort, []string{"mongos"}, cr+"-mongos")
		if !ok {
			return stockSimResolved{}, retry, k3dExposeAdvice(frame, "the mongos routers", why)
		}
		uri := fmt.Sprintf("mongodb://%s@%s/?authSource=admin", creds, ep.addr())
		return stockSimResolved{
			env:         []string{"DB_ENGINE=mongodb", "MONGO_URI=" + uri},
			secrets:     stockSimSecrets{User: user, Password: pass},
			engine:      "mongodb",
			kind:        "k3d-psmdb",
			displayName: k3dTargetName(frame, cfg, ep),
			host:        ep.host, port: ep.port,
		}, false, nil
	}

	// A plain replica set: the member holding the primary role, directly.
	rs := a.k3dPSMDBReplsetName(ctx, serverID, cfg)
	members := servicesWithPrefix(svcs, cr+"-"+rs+"-")
	if len(members) == 0 {
		return stockSimResolved{}, true, fmt.Errorf(
			"the %s replica set in %s has no member Services yet", rs, frame.Label)
	}
	primary, ok := a.k3dPSMDBPrimaryService(ctx, serverID, cfg, rs, len(members))
	if !ok {
		return stockSimResolved{}, true, fmt.Errorf(
			"the %s replica set in %s has not elected a primary yet", rs, frame.Label)
	}
	svc, found := findService(svcs, primary)
	if !found {
		return stockSimResolved{}, true, fmt.Errorf(
			"the %s replica set's primary is pod %s, which has no Service of its own — expose the replica set's pods on the Kubernetes frame",
			rs, primary)
	}
	ep, retry, err := a.resolveService(ctx, serverID, svc, mongoPort, "mongodb")
	if err != nil {
		return stockSimResolved{}, retry, k3dExposeAdvice(frame, "the replica set's pods", []string{err.Error()})
	}
	uri := fmt.Sprintf("mongodb://%s@%s/?authSource=admin&directConnection=true", creds, ep.addr())
	return stockSimResolved{
		env:         []string{"DB_ENGINE=mongodb", "MONGO_URI=" + uri},
		secrets:     stockSimSecrets{User: user, Password: pass},
		engine:      "mongodb",
		kind:        "k3d-psmdb",
		displayName: k3dTargetName(frame, cfg, ep),
		host:        ep.host, port: ep.port,
	}, false, nil
}

// k3dPSMDBPrimaryService names the per-pod Service in front of whichever member
// currently holds the primary role.
//
// `db.hello()` is one of the few commands MongoDB answers before authentication,
// so this needs no credentials — and its `primary` field is the member's own
// hostname, whose first label is the pod's name, which is also the name of the
// per-pod Service the operator created for it. Any member can answer; the loop
// exists because member 0 is not guaranteed to be up.
func (a *App) k3dPSMDBPrimaryService(ctx context.Context, serverID string, cfg k3dConfig, rs string, members int) (string, bool) {
	for i := 0; i < members; i++ {
		pod := fmt.Sprintf("%s-%s-%d", cfg.ClusterName, rs, i)
		out, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "exec", pod, "-c", "mongod", "--",
			"mongosh", "--quiet", "--eval", "db.hello().primary")
		if err != nil {
			continue
		}
		host, _, _ := strings.Cut(strings.TrimSpace(out), ".")
		if host != "" && strings.HasPrefix(host, cfg.ClusterName+"-"+rs+"-") {
			return host, true
		}
	}
	return "", false
}

// k3dPSMDBReplsetName reads the first replica set's name off the custom
// resource. It is "rs0" in every cr.yaml Percona ships, but the CR is the one
// that decides, and the per-pod Service names are built from it.
func (a *App) k3dPSMDBReplsetName(ctx context.Context, serverID string, cfg k3dConfig) string {
	out, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "get", "psmdb", cfg.ClusterName,
		"-o", "jsonpath={.spec.replsets[0].name}")
	if name := strings.TrimSpace(out); err == nil && name != "" {
		return name
	}
	return "rs0"
}

// ---------------------------------------------------------------- PostgreSQL

// k3dPGResolve resolves the two operators that build a Crunchy-shaped
// PostgreSQL cluster — Percona's (`pg`) and Crunchy's own (`pgo`). They lay out
// the same two tiers under the same names, so one resolver covers both.
//
// **pgBouncer first, the primary Service second.** The pooler is what a cluster
// of this shape advertises as its front door, it is what DBCanvas exposes by
// default, and it is the tier that stays put across a failover — so it is the
// right target whenever it has an address. `<cr>-ha` is the fallback for a
// cluster whose pooler was left in-cluster.
//
// Reaching the app *through* the pooler took a change in the sim itself: it
// used to pin `search_path` as a libpq startup parameter, which PgBouncer
// rejects outright ("unsupported startup parameter in options: search_path").
// It now sets it per connection and as a role default instead — see the note on
// pgStore in `stocksim/internal/store/postgres.go`.
//
// **Which user depends on which tier**, because the pooler does not know every
// user the cluster has. Its userlist is generated from `spec.users`, and the
// operator deliberately leaves the superuser out of it — connecting through the
// pool as `postgres` is answered with a flat `FATAL: no such user`, verified
// live. So:
//
//   - through pgBouncer: the application user (named after the cluster, as is
//     its database). It has no CREATEDB, so the sim claims a schema inside that
//     database — the fallback that exists for exactly this shape of target.
//   - direct to the primary: the `postgres` superuser, whose secret DBCanvas
//     pre-creates with the `.env` password, so the sim gets a database of its own.
func (a *App) k3dPGResolve(ctx context.Context, serverID string, frame designFrame, cfg k3dConfig, svcs []kubeService) (stockSimResolved, bool, error) {
	cr := cfg.ClusterName
	ep, ok, retry, why := a.k3dPickEndpoint(ctx, serverID, svcs, patroniPGPort,
		[]string{"pgbouncer", "postgres"}, cr+"-pgbouncer", cr+"-ha")
	if !ok {
		return stockSimResolved{}, retry, k3dExposeAdvice(frame,
			"the pgBouncer pool (or Expose · PostgreSQL, the primary Service)", why)
	}
	user := "postgres"
	if ep.svc == cr+"-pgbouncer" {
		user = cr
	}
	secret := cr + "-pguser-" + user
	pass := a.k3dSecretOr(ctx, serverID, cfg.Namespace, secret, "password", envOr("POSTGRES_PASSWORD", ""))
	if pass == "" {
		return stockSimResolved{}, true, fmt.Errorf(
			"could not read the %s password out of Secret %q in namespace %q", user, secret, cfg.Namespace)
	}
	return k3dPostgresResolved(frame, cfg, ep, user, pass, user), false, nil
}

// k3dCNPGResolve resolves a CloudNativePG cluster. CNPG's own -rw/-ro/-r
// Services are all ClusterIP, so cnpg.go adds a LoadBalancer in front of the
// primary when the frame asks for one and records the address; that record is
// the only way in from outside Kubernetes. The application role CNPG generates
// is not a superuser, so the sim claims a schema inside CNPG's application
// database rather than creating one of its own — the fallback exists for
// exactly this shape of target.
//
// Either tier will do, and the PgBouncer pool is preferred when it has an
// address — the same order the two Percona/Crunchy PostgreSQL operators already
// resolve in, and the reason the pool can be the only thing a frame exposes.
// The pool is a Pooler of type rw, so it follows the primary across a failover
// and the credentials are the cluster's own either way.
func (a *App) k3dCNPGResolve(ctx context.Context, serverID string, frame designFrame, cfg k3dConfig) (stockSimResolved, bool, error) {
	endpoint, tier, svc := strings.TrimSpace(cfg.CNPGEndpoint), "the CloudNativePG primary", cfg.ClusterName+"-rw-lb"
	if cfg.CNPGPooler && cfg.CNPGPoolerExpose == "LoadBalancer" {
		endpoint, tier, svc = strings.TrimSpace(cfg.CNPGPoolerEndpoint), "the PgBouncer pool", cnpgPoolerName(cfg.ClusterName)
	} else if cfg.CNPGExpose != "LoadBalancer" {
		return stockSimResolved{}, false, k3dExposeAdvice(frame, "the CloudNativePG primary or its PgBouncer pool",
			[]string{"the primary is exposed as " + orDefault(endpoint, "ClusterIP")})
	}
	if endpoint == "" || endpoint == "pending" {
		return stockSimResolved{}, true, fmt.Errorf("CloudNativePG in %s has not reported an address for %s yet", frame.Label, tier)
	}
	user := orDefault(cfg.CNPGAppUser, "app")
	db := orDefault(cfg.CNPGAppDB, "app")
	pass := a.k3dSecretOr(ctx, serverID, cfg.Namespace, cfg.CNPGAppSecret, "password", "")
	if pass == "" {
		return stockSimResolved{}, true, fmt.Errorf(
			"could not read the CloudNativePG application password out of Secret %q in namespace %q",
			cfg.CNPGAppSecret, cfg.Namespace)
	}
	host, port := splitHostPortDefault(endpoint, cnpgPostgresPort)
	return k3dPostgresResolved(frame, cfg,
		k3dEndpoint{host: host, port: port, svc: svc, typ: "LoadBalancer"},
		user, pass, db), false, nil
}

// k3dPostgresResolved is the common tail of the three PostgreSQL operators.
// sslmode=prefer covers all of them: CNPG and the Percona operator accept
// plaintext, Crunchy PGO refuses it, and `prefer` negotiates TLS first either
// way.
func k3dPostgresResolved(frame designFrame, cfg k3dConfig, ep k3dEndpoint, user, pass, db string) stockSimResolved {
	dsn := (&url.URL{
		Scheme: "postgres", User: url.UserPassword(user, pass),
		Host: ep.addr(), Path: "/" + db,
		RawQuery: "sslmode=prefer&connect_timeout=10",
	}).String()
	return stockSimResolved{
		env:         []string{"DB_ENGINE=postgres", "POSTGRES_DSN=" + dsn},
		secrets:     stockSimSecrets{User: user, Password: pass},
		engine:      "postgres",
		kind:        "k3d-" + cfg.Operator,
		displayName: k3dTargetName(frame, cfg, ep),
		host:        ep.host, port: ep.port,
	}
}

// k3dTargetName is what the node panel shows as the target: the frame, then the
// database inside it, then the Service the address came from. All three matter
// — the frame is what the user drew, and the Service is the one `kubectl get
// svc` that confirms the address is still the right one.
func k3dTargetName(frame designFrame, cfg k3dConfig, ep k3dEndpoint) string {
	name := frame.Label
	if cfg.ClusterName != "" {
		name += " / " + cfg.ClusterName
	}
	if ep.svc != "" {
		name += " (" + ep.svc + ")"
	}
	return name
}

// ---------------------------------------------------------------- validation

// k3dExposedTiers lists the frame's Service types for the tiers a Stock Market
// Sim node could connect through, named the way the frame's own form names
// them. Empty for an operator with no database to drive.
//
// Read straight off the design rather than off a deployment, so the answer
// exists before anything is provisioned — which is the point: a cluster
// reachable only inside Kubernetes is a setting to change, and finding that out
// twenty minutes into a deploy is finding it out too late.
func k3dExposedTiers(f designFrame) map[string]string {
	switch f.K3DOperator {
	case "pxc":
		proxy := "HAProxy"
		exposeProxy := k3dExposeOf(f.K3DExposeHAProxy, f.K3DExpose)
		if k3dProxy(f) == "proxysql" {
			proxy, exposeProxy = "ProxySQL", k3dExposeOf(f.K3DExposeProxySQL, f.K3DExpose)
		}
		return map[string]string{proxy: exposeProxy, "the PXC pods": k3dExposeOf(f.K3DExposePXC, f.K3DExpose)}
	case "ps":
		proxy := "HAProxy"
		exposeProxy := k3dExposeOf(f.K3DExposeHAProxy, f.K3DExpose)
		if psProxy(f.K3DProxy, psClusterType(f.K3DClusterType)) == "router" {
			proxy, exposeProxy = "MySQL Router", k3dExposeOf(f.K3DExposeRouter, f.K3DExpose)
		}
		return map[string]string{proxy: exposeProxy, "the MySQL primary": k3dExposeOf(f.K3DExposeMySQL, f.K3DExpose)}
	case "psmdb":
		if f.K3DSharding {
			return map[string]string{"the mongos routers": k3dExposeOf(f.K3DExposeMongos, f.K3DExpose)}
		}
		// A replica set has no router in front of it: the driver reads the
		// member list out of the set's own configuration, and the operator only
		// puts reachable addresses there when the members themselves are
		// exposed. So the replica set tier is the only one that counts.
		return map[string]string{"the replica set's pods": k3dExposeOf(f.K3DExposeReplset, f.K3DExpose)}
	case "pg", "pgo":
		return map[string]string{
			"the pgBouncer pool":     k3dExposeOf(f.K3DExposePGBouncer, f.K3DExpose),
			"the PostgreSQL primary": k3dExposeOf(f.K3DExposePG, f.K3DExpose),
		}
	case "cnpg":
		tiers := map[string]string{"the CloudNativePG primary": k3dExposeOf(f.K3DCNPGExpose, "clusterip")}
		// Only counted when the pool is actually enabled — an absent tier must not read as
		// one more thing the designer is asking you to expose.
		if f.K3DCNPGPooler {
			tiers["the PgBouncer pool"] = k3dExposeOf(f.K3DCNPGPoolerExpose, "clusterip")
		}
		return tiers
	}
	return nil
}

// stockSimK3DExposeIssues warns when a Stock Market Sim node is linked to a
// Kubernetes frame whose database is exposed only inside the cluster.
//
// A warning rather than an error: the design says what will happen, but the
// cluster is the one that decides, and an operator release that adds a Service
// DBCanvas does not know about should not be able to block a deploy. The
// resolver gives the definitive answer at deploy time — this just gives it
// twenty minutes earlier.
func stockSimK3DExposeIssues(doc designDoc, n designNode) []issue {
	if stockSimMode(n) != "linked" {
		return nil
	}
	kind, targetID, ok := stockSimTarget(doc, n.ID)
	if !ok || kind != "k3d" {
		return nil
	}
	f := frameByID(doc, targetID)
	tiers := k3dExposedTiers(f)
	if len(tiers) == 0 {
		return nil // no operator: stockSimEngineAndIssues has already said so
	}
	var names []string
	for tier, typ := range tiers {
		if typ != "ClusterIP" {
			return nil // at least one tier is routable from the stack network
		}
		names = append(names, tier)
	}
	sort.Strings(names)
	return []issue{{"warning", fmt.Sprintf(
		"Stock Market Sim node %s is linked to Kubernetes frame %s, where %s %s exposed as ClusterIP — an address that exists only inside the cluster. Set %s to LoadBalancer or NodePort on the frame, or the sim will have nothing to connect to.",
		n.Label, f.Label, strings.Join(names, " and "), plural(len(names), "is", "are"), joinOr(names))}}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// joinOr renders a list as "a", "a or b", "a, b or c".
func joinOr(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
