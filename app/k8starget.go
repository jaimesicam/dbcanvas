package main

// k8starget.go — the databases a Kubernetes operator deployed, as targets the rest of
// DBCanvas can use.
//
// A K3D frame is a real k3s cluster with a real operator in it, and the databases that
// operator creates are as much a part of the stack as the ones DBCanvas provisions
// itself. Until this file they were invisible to every tool: the Data Generator, the
// Query Runner, the Benchmark and the Database Explorer all walk `doc.Nodes`, and an
// operator's cluster is not a node — it is a custom resource inside a frame.
//
// -------------------------------------------------------------------- reaching them
//
// Two routes, and which one an endpoint has is a fact about how its Service was
// exposed rather than a preference:
//
//	network   a Service of type LoadBalancer has an address from the cluster's MetalLB
//	          pool, which is carved out of the *stack's own Docker subnet* — so it is
//	          dialable from the app exactly like any other node. A NodePort is reachable
//	          the same way, at any k3s node container's address on that subnet.
//	exec      a ClusterIP Service has no address outside the cluster at all. Those are
//	          reached by running the database's own client inside one of its pods,
//	          through `kubectl exec` on the k3s server container — the same exec seam
//	          the PMM connections use.
//
// ClusterIP is the operator default, so the exec route is not a fallback for unusual
// cases; it is the common one. What it cannot do is measure anything: a Query Runner
// thread or a Benchmark client needs a socket it can open many of, and `kubectl exec`
// is a process per statement. So those two tools list only the network-reachable
// endpoints, and say why when an endpoint has no address — see k8sEndpoint.Why.
//
// -------------------------------------------------------------------- credentials
//
// From the operator's own Secret, read at discovery rather than assumed. Percona's
// operators are given DBCanvas's .env passwords at deploy (k3dSecretsPasswords), but
// reading the Secret is what keeps this working for a cluster whose passwords were
// rotated, for the community operators that generate their own, and for the per-user
// Secrets the PostgreSQL operators publish.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// k8sEndpoint is one database inside an operator's cluster.
type k8sEndpoint struct {
	StackID    int64
	StackName  string
	FrameID    string
	FrameLabel string
	Cluster    string // the k3d cluster name
	ServerID   string // the k3s server container every kubectl call runs in
	Operator   string // pxc | ps | psmdb | pg | cnpg | pgo
	Namespace  string
	CRName     string

	Engine string // mysql | postgres | mongodb
	// Kind is the tier this endpoint is, which is what decides whether it is the one
	// a client should use: "primary", "replica", "haproxy", "proxysql", "router",
	// "pgbouncer", "mongos", "member".
	Kind      string
	Service   string
	Label     string
	Role      string
	Preferred bool
	Note      string

	// Addr/Port are the network route, empty when the Service is ClusterIP. SvcType
	// and Why together explain the absence to a tool that needs one.
	Addr    string
	Port    int
	SvcType string
	Why     string
	// TLS is the sslmode a PostgreSQL client needs here. The PostgreSQL operators put
	// TLS in front of everything, so "require" rather than "prefer" is the honest
	// default; the chain cannot be verified because the certificate names the
	// in-cluster Service and the app dials an address (see dbexplorer_tls.go).
	TLS string

	// Selector and TargetPort are what a companion Service would have to copy to
	// point at the same pods — see k8sexpose.go. A Service with no selector cannot be
	// mirrored, which is a fact worth carrying rather than rediscovering.
	Selector   map[string]string
	TargetPort int
	// ExposedBy names the companion Service that gave this endpoint its address, when
	// one did. It is what lets the control say "Remove" rather than "Expose", and what
	// distinguishes an address the operator published from one DBCanvas added.
	ExposedBy string

	// Pod/Container are the exec route: a pod running this database and the container
	// in it that has the client.
	Pod       string
	Container string

	User     string
	Pass     string
	Database string
	AuthDB   string
}

// Reachable reports whether this endpoint can be dialled over the network.
func (e k8sEndpoint) Reachable() bool { return e.Addr != "" && e.Port > 0 }

// Execable reports whether a client can be run inside one of its pods.
func (e k8sEndpoint) Execable() bool { return e.Pod != "" && e.ServerID != "" }

// ID is the stable handle a tool hands back to re-resolve this endpoint. Frame ids and
// Service names are canvas/Kubernetes tokens, so neither contains the separator.
func (e k8sEndpoint) ID() string { return e.FrameID + "/" + e.Service }

// ---------------------------------------------------------------- caching

// k8sEndpointTTL is how long a frame's endpoint list is reused. Discovery is three
// kubectl calls into a container, and the four tools that use it all redraw their
// target lists on a timer — without a cache, a Database Explorer tab left open would
// exec into every k3s cluster on the host every few seconds. Short enough that a
// cluster that has just finished deploying appears on the next refresh.
const k8sEndpointTTL = 20 * time.Second

type k8sEndpointCacheEntry struct {
	at   time.Time
	list []k8sEndpoint
}

var k8sEndpointCache = struct {
	sync.Mutex
	m map[string]k8sEndpointCacheEntry
}{m: map[string]k8sEndpointCacheEntry{}}

func k8sCacheKey(stackID int64, frameID string) string {
	return strconv.FormatInt(stackID, 10) + "/" + frameID
}

// k8sInvalidateFrame drops a frame's cached endpoints, so a change DBCanvas made
// itself — exposing a Service for the tools — shows up immediately rather than after
// the TTL.
func k8sInvalidateFrame(stackID int64, frameID string) {
	k8sEndpointCache.Lock()
	delete(k8sEndpointCache.m, k8sCacheKey(stackID, frameID))
	k8sEndpointCache.Unlock()
}

// ---------------------------------------------------------------- discovery

// k8sStackEndpoints is every operator-deployed database in one stack. Best-effort per
// frame: a cluster that is still deploying, has no operator, or whose server node is
// stopped contributes nothing rather than failing the whole listing.
func (a *App) k8sStackEndpoints(ctx context.Context, st Stack) []k8sEndpoint {
	doc := buildDoc(st)
	var out []k8sEndpoint
	for _, f := range doc.Frames {
		if f.Type != "k3d" {
			continue
		}
		out = append(out, a.k8sFrameEndpoints(ctx, st, doc, f)...)
	}
	return out
}

// k8sFrameEndpoints is one cluster's databases, cached.
func (a *App) k8sFrameEndpoints(ctx context.Context, st Stack, doc designDoc, f designFrame) []k8sEndpoint {
	key := k8sCacheKey(st.ID, f.ID)
	k8sEndpointCache.Lock()
	if e, ok := k8sEndpointCache.m[key]; ok && time.Since(e.at) < k8sEndpointTTL {
		k8sEndpointCache.Unlock()
		return e.list
	}
	k8sEndpointCache.Unlock()

	list := a.k8sDiscoverFrame(ctx, st, doc, f)
	k8sEndpointCache.Lock()
	k8sEndpointCache.m[key] = k8sEndpointCacheEntry{at: time.Now(), list: list}
	k8sEndpointCache.Unlock()
	return list
}

// k8sFrameServer resolves the running k3s server container for a frame, plus the
// k3dConfig any of its members carries. Every kubectl call goes through that
// container, which is where k3s keeps the admin kubeconfig.
func (a *App) k8sFrameServer(st Stack, doc designDoc, f designFrame) (serverID string, cfg k3dConfig, ok bool) {
	for _, n := range doc.Nodes {
		if n.FrameID != f.ID || n.Type != "k3d" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		var c k3dConfig
		json.Unmarshal(dep.Config, &c)
		if c.Role != "server" {
			continue
		}
		return dep.ContainerID, c, true
	}
	return "", k3dConfig{}, false
}

// k8sTargetSvc is the part of a Service this file reads.
type k8sTargetSvc struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Type      string            `json:"type"`
		ClusterIP string            `json:"clusterIP"`
		Selector  map[string]string `json:"selector"`
		Ports     []struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			NodePort int    `json:"nodePort"`
			Protocol string `json:"protocol"`
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

type k8sTargetPod struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// k8sDiscoverFrame does the real work for one cluster.
func (a *App) k8sDiscoverFrame(ctx context.Context, st Stack, doc designDoc, f designFrame) []k8sEndpoint {
	serverID, cfg, ok := a.k8sFrameServer(st, doc, f)
	if !ok || cfg.Operator == "" || cfg.ClusterName == "" {
		return nil
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = "default"
	}
	ctx = withEngine(ctx, a.docker) // k3d clusters are always Docker, even in a hybrid stack

	svcs, err := a.k8sServices(ctx, serverID, ns)
	if err != nil || len(svcs) == 0 {
		return nil
	}
	pods := a.k8sPods(ctx, serverID, ns)
	// The PostgreSQL operators publish their primary as a Service with no selector,
	// whose Endpoints Patroni rewrites on every failover. That is the endpoint people
	// most want, so it cannot be skipped for want of a label to match — its pod is
	// read out of the Endpoints object instead, which is exact and needs no knowledge
	// of any operator's labelling.
	eps := a.k8sEndpointsPods(ctx, serverID, ns)
	nodeIP := a.k8sNodeAddr(ctx, st.ID, serverID)
	creds := a.k8sCredentials(ctx, serverID, ns, cfg)

	// The companion Services DBCanvas created for the load tools, by the Service each
	// one mirrors. They are not endpoints of their own — they are an address the
	// endpoint beside them did not have.
	companion := map[string]k8sTargetSvc{}
	for _, s := range svcs {
		if strings.HasSuffix(s.Metadata.Name, k8sExposeSuffix) {
			companion[strings.TrimSuffix(s.Metadata.Name, k8sExposeSuffix)] = s
		}
	}

	var out []k8sEndpoint
	for _, s := range svcs {
		e, ok := k8sClassifyService(s, cfg)
		if !ok {
			continue
		}
		e.StackID, e.StackName = st.ID, st.Name
		e.FrameID, e.FrameLabel = f.ID, f.Label
		e.Cluster, e.ServerID = cfg.Cluster, serverID
		e.Operator, e.Namespace, e.CRName = cfg.Operator, ns, cfg.ClusterName
		e.Label = f.Label + " · " + e.Label

		// The network route, if the Service has one. The in-cluster port is kept
		// when there is not: an endpoint reached by exec is still on 5432 inside the
		// cluster, and reporting port 0 would say something untrue about it.
		addr, port, why := k8sAddressOf(s, e.Port, nodeIP)
		e.Addr, e.Why = addr, why
		if port > 0 {
			e.Port = port
		}
		if addr == "" {
			// Exposed for the tools: the address belongs to the companion Service,
			// but it is this endpoint's address — the same pods answer on it.
			if c, ok := companion[s.Metadata.Name]; ok {
				if a2, p2, _ := k8sAddressOf(c, e.Port, nodeIP); a2 != "" {
					e.Addr, e.Port, e.Why = a2, p2, ""
					e.ExposedBy = c.Metadata.Name
				}
			}
		}

		// The exec route: a running pod this Service points at.
		e.Pod, e.Container = k8sPodFor(pods, s, e.Engine)
		if e.Pod == "" {
			e.Pod, e.Container = k8sPodFromEndpoints(eps[s.Metadata.Name], pods, e.Engine)
		}

		if c, ok := creds[e.credKey()]; ok {
			e.User, e.Pass, e.Database, e.AuthDB = c.User, c.Pass, c.Database, c.AuthDB
		}
		if e.User == "" && e.Engine != "" {
			// No credentials means no usable endpoint. Reporting it anyway would put
			// a connection in the tree that can only ever fail to open.
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Preferred != out[j].Preferred {
			return out[i].Preferred
		}
		return out[i].Label < out[j].Label
	})
	return out
}

func (a *App) k8sServices(ctx context.Context, serverID, ns string) ([]k8sTargetSvc, error) {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "svc", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []k8sTargetSvc `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (a *App) k8sPods(ctx context.Context, serverID, ns string) []k8sTargetPod {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "pods", "-o", "json")
	if err != nil {
		return nil
	}
	var list struct {
		Items []k8sTargetPod `json:"items"`
	}
	json.Unmarshal([]byte(out), &list)
	return list.Items
}

// k8sEndpointsPods maps a Service name to the pods its Endpoints object points at.
// One call for the whole namespace rather than one per Service.
func (a *App) k8sEndpointsPods(ctx context.Context, serverID, ns string) map[string][]string {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "endpoints", "-o", "json")
	if err != nil {
		return nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Subsets []struct {
				Addresses []struct {
					TargetRef struct {
						Kind string `json:"kind"`
						Name string `json:"name"`
					} `json:"targetRef"`
				} `json:"addresses"`
			} `json:"subsets"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return nil
	}
	m := map[string][]string{}
	for _, it := range list.Items {
		for _, sub := range it.Subsets {
			for _, ad := range sub.Addresses {
				if ad.TargetRef.Kind == "Pod" && ad.TargetRef.Name != "" {
					m[it.Metadata.Name] = append(m[it.Metadata.Name], ad.TargetRef.Name)
				}
			}
		}
	}
	return m
}

// k8sPodFromEndpoints picks a running pod out of a Service's Endpoints.
func k8sPodFromEndpoints(names []string, pods []k8sTargetPod, engine string) (pod, container string) {
	for _, name := range names {
		for _, p := range pods {
			if p.Metadata.Name != name || p.Status.Phase != "Running" {
				continue
			}
			return p.Metadata.Name, k8sClientContainer(p, engine)
		}
	}
	return "", ""
}

// k8sNodeAddr is the k3s server container's address on the stack network, which is
// where a NodePort is reachable.
//
// It inspects and does not join. Attaching the app's own container to a stack network
// is disruptive enough that every other tool defers it off the request path (see
// joinStackForDial), and doing it here cancelled the very request that was listing
// connections — the second cluster in a stack came back "context canceled" while the
// first had already been read. The join still happens, later, when something actually
// dials (dexNodeAddr); all that is needed here is the address to record.
func (a *App) k8sNodeAddr(ctx context.Context, stackID int64, serverID string) string {
	ip, err := a.docker.ContainerIP(ctx, serverID, networkName(stackID))
	if err != nil {
		return ""
	}
	return ip
}

// k8sAddressOf resolves a Service to something dialable, or explains why there is
// nothing. The three cases are the three Service types, and the explanation matters
// as much as the address: "this cluster has nothing to connect to" is the wrong thing
// to tell somebody whose Service is simply ClusterIP.
func k8sAddressOf(s k8sTargetSvc, port int, nodeIP string) (addr string, outPort int, why string) {
	switch s.Spec.Type {
	case "LoadBalancer":
		for _, in := range s.Status.LoadBalancer.Ingress {
			if in.IP != "" {
				return in.IP, port, ""
			}
			if in.Hostname != "" {
				return in.Hostname, port, ""
			}
		}
		return "", 0, "this Service is a LoadBalancer but has no address yet — MetalLB may have run out of its pool"
	case "NodePort":
		for _, p := range s.Spec.Ports {
			if p.Port == port && p.NodePort > 0 {
				if nodeIP == "" {
					return "", 0, "this Service is a NodePort but the cluster's node address could not be resolved"
				}
				return nodeIP, p.NodePort, ""
			}
		}
		return "", 0, "this Service is a NodePort but no node port was assigned"
	}
	return "", 0, "this Service is ClusterIP, so it has no address outside the cluster"
}

// k8sPodFor picks a running pod behind a Service, and the container in it that has
// the database client. A Service with no selector (the PostgreSQL operators' headless
// primary, whose Endpoints are managed by Patroni) matches nothing, which is correct:
// its pods are reachable through the Service that does have one.
func k8sPodFor(pods []k8sTargetPod, s k8sTargetSvc, engine string) (pod, container string) {
	if len(s.Spec.Selector) == 0 {
		return "", ""
	}
	for _, p := range pods {
		if p.Status.Phase != "Running" {
			continue
		}
		match := true
		for k, v := range s.Spec.Selector {
			if p.Metadata.Labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		return p.Metadata.Name, k8sClientContainer(p, engine)
	}
	return "", ""
}

// k8sClientContainer is the container in a pod that has the database and its client.
// Operator pods are several containers — sidecars for backups, logs, certificates —
// and the client only exists in one of them.
func k8sClientContainer(p k8sTargetPod, engine string) string {
	want := map[string]bool{}
	switch engine {
	case dexPostgres:
		want = map[string]bool{"database": true, "postgres": true, "pgbouncer": true}
	case dexMySQL:
		want = map[string]bool{"pxc": true, "mysql": true, "haproxy": true, "proxysql": true, "router": true}
	case dexMongoDB:
		want = map[string]bool{"mongod": true, "mongos": true}
	}
	for _, c := range p.Spec.Containers {
		if want[c.Name] {
			return c.Name
		}
	}
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Name
	}
	return ""
}

// ---------------------------------------------------------------- classification

// k8sTier describes one Service name suffix: what tier it is, and how it should read.
type k8sTier struct {
	Kind      string
	Role      string
	Preferred bool
	Label     string
	Note      string
	// App says this tier authenticates as the cluster's *application* user rather
	// than its superuser. A connection pooler is the case: pgBouncer knows the app
	// roles the operator registered with it and nothing else, so offering it the
	// superuser would produce "no such user" and look like a DBCanvas bug.
	App bool
}

// k8sTiers maps a Service's name suffix to what it is. Suffixes rather than exact
// names because every operator prefixes with the cluster's own name, and longest
// match wins so "-ha-config" cannot be read as "-ha".
var k8sTiers = []struct {
	Suffix string
	Tier   k8sTier
}{
	// PostgreSQL — Percona's operator and Crunchy's share these names.
	{"-pgbouncer", k8sTier{Kind: "pgbouncer", Role: "proxy", Preferred: true, App: true,
		Label: "pgBouncer", Note: "the connection pooler — the endpoint an application should use"}},
	{"-ha", k8sTier{Kind: "primary", Role: "primary", Preferred: true,
		Label: "primary", Note: "whichever instance is primary right now"}},
	{"-primary", k8sTier{Kind: "primary", Role: "primary",
		Label: "primary (direct)", Note: "the primary instance"}},
	{"-replicas", k8sTier{Kind: "replica", Role: "replica",
		Label: "replicas", Note: "read-only replicas — writes will be refused"}},
	// CloudNativePG.
	{"-rw", k8sTier{Kind: "primary", Role: "primary", Preferred: true,
		Label: "read/write", Note: "routed to the primary"}},
	{"-ro", k8sTier{Kind: "replica", Role: "replica",
		Label: "read-only", Note: "routed to a replica — writes will be refused"}},
	{"-pooler-rw", k8sTier{Kind: "pgbouncer", Role: "proxy", Preferred: true, App: true,
		Label: "pooler (read/write)", Note: "the PgBouncer pooler in front of the primary"}},
	// MySQL.
	{"-haproxy-replicas", k8sTier{Kind: "haproxy", Role: "replica",
		Label: "HAProxy (read)", Note: "balanced across the replicas"}},
	{"-haproxy", k8sTier{Kind: "haproxy", Role: "primary", Preferred: true,
		Label: "HAProxy", Note: "balanced onto the member that can take writes"}},
	{"-proxysql", k8sTier{Kind: "proxysql", Role: "proxy", Preferred: true,
		Label: "ProxySQL", Note: "ProxySQL splits reads from writes itself"}},
	{"-router", k8sTier{Kind: "router", Role: "router", Preferred: true,
		Label: "MySQL Router", Note: "routed to whichever member is primary"}},
	{"-pxc", k8sTier{Kind: "member", Role: "member", Label: "PXC members"}},
	{"-mysql", k8sTier{Kind: "member", Role: "primary", Preferred: true, Label: "MySQL"}},
	// MongoDB.
	{"-mongos", k8sTier{Kind: "mongos", Role: "router", Preferred: true,
		Label: "mongos", Note: "the entry point to the sharded cluster"}},
	{"-cfg", k8sTier{Kind: "config", Role: "config", Label: "config servers"}},
	{"-rs0", k8sTier{Kind: "member", Role: "primary", Preferred: true, Label: "replica set rs0"}},
}

// k8sPortEngine maps a Service port to the engine speaking on it. The named port is
// preferred where the operator sets one; the number is the fallback.
func k8sPortEngine(name string, port int) string {
	switch strings.ToLower(name) {
	case "postgres", "postgresql", "pgbouncer":
		return dexPostgres
	case "mysql", "mysql-replicas", "haproxy-mysql", "proxysql-mysql":
		return dexMySQL
	case "mongodb", "mongos":
		return dexMongoDB
	}
	switch port {
	case 5432, 6432:
		return dexPostgres
	case 3306, 6033, 6446, 6447:
		return dexMySQL
	case 27017, 27018, 27019:
		return dexMongoDB
	}
	return ""
}

// k8sClassifyService decides whether a Service is a database endpoint worth offering,
// and what it is. A headless Service, one with no ports, and the operator's own
// bookkeeping Services are all filtered out here.
func k8sClassifyService(s k8sTargetSvc, cfg k3dConfig) (k8sEndpoint, bool) {
	name := s.Metadata.Name
	// Headless Services have no address of their own and exist to publish pod DNS.
	// The tier that matters is always published by a Service that is not headless.
	if s.Spec.ClusterIP == "None" {
		return k8sEndpoint{}, false
	}
	if len(s.Spec.Ports) == 0 {
		return k8sEndpoint{}, false
	}
	// Anything not named after this cluster belongs to something else in the
	// namespace — the operator's own Service, a webhook, the Kubernetes API.
	if cfg.ClusterName != "" && !strings.HasPrefix(name, cfg.ClusterName) {
		return k8sEndpoint{}, false
	}
	// A companion Service DBCanvas created is the same database as the Service it
	// mirrors, so listing it as well would put the same endpoint in the tree twice.
	// Its address is picked up on the endpoint it belongs to instead.
	if strings.HasSuffix(name, k8sExposeSuffix) {
		return k8sEndpoint{}, false
	}
	var tier k8sTier
	var matched string
	for _, t := range k8sTiers {
		if strings.HasSuffix(name, t.Suffix) && len(t.Suffix) > len(matched) {
			tier, matched = t.Tier, t.Suffix
		}
	}
	if matched == "" {
		return k8sEndpoint{}, false
	}
	// The port that carries the database, and the engine on it.
	var engine string
	var port int
	for _, p := range s.Spec.Ports {
		if e := k8sPortEngine(p.Name, p.Port); e != "" {
			engine, port = e, p.Port
			break
		}
	}
	if engine == "" {
		return k8sEndpoint{}, false
	}
	e := k8sEndpoint{
		Engine: engine, Kind: tier.Kind, Service: name, Label: tier.Label,
		Role: tier.Role, Preferred: tier.Preferred, Note: tier.Note, Port: port,
		SvcType: s.Spec.Type, Selector: s.Spec.Selector, TargetPort: port,
	}
	if engine == dexPostgres {
		// The PostgreSQL operators put TLS in front of every Service, and a client
		// that does not ask for it is refused outright ("SSL required").
		e.TLS = "require"
	}
	if tier.App {
		e.Kind += "-app"
	}
	return e, true
}

// credKey is which credential set this endpoint authenticates with. A pooler needs
// the application user; everything else uses the administrative one.
func (e k8sEndpoint) credKey() string {
	if strings.HasSuffix(e.Kind, "-app") {
		return "app"
	}
	return "admin"
}

// ---------------------------------------------------------------- credentials

type k8sCred struct {
	User     string
	Pass     string
	Database string
	AuthDB   string
}

// k8sCredentials reads the operator's Secrets. Two sets come back where an operator
// publishes two — an administrative account and an application one — because which is
// usable depends on the endpoint: a pooler only knows the application roles.
func (a *App) k8sCredentials(ctx context.Context, serverID, ns string, cfg k3dConfig) map[string]k8sCred {
	out := map[string]k8sCred{}
	cr := cfg.ClusterName
	switch cfg.Operator {
	case "pg", "pgo":
		// Percona's PostgreSQL operator and Crunchy's both publish one Secret per
		// role, carrying user, password and dbname — a complete descriptor.
		if c, ok := a.k8sPGUserSecret(ctx, serverID, ns, cr+"-pguser-postgres"); ok {
			out["admin"] = c
		}
		app := cfg.PGOAppSecret
		if app == "" {
			app = cr + "-pguser-" + cr
		}
		if c, ok := a.k8sPGUserSecret(ctx, serverID, ns, app); ok {
			out["app"] = c
		}
	case "cnpg":
		// CloudNativePG generates its own application credentials and records the
		// Secret's name on the frame at deploy.
		name := cfg.CNPGAppSecret
		if name == "" {
			name = cr + "-app"
		}
		if c, ok := a.k8sBasicAuthSecret(ctx, serverID, ns, name); ok {
			c.Database = cfg.CNPGAppDB
			if c.Database == "" {
				c.Database = c.User
			}
			out["admin"], out["app"] = c, c
		}
	case "pxc", "ps":
		if pw, ok := a.k8sSecretKey(ctx, serverID, ns, cr+"-secrets", "root"); ok {
			out["admin"] = k8sCred{User: "root", Pass: pw}
			out["app"] = out["admin"]
		}
	case "psmdb":
		// The operator ships several accounts; the database admin is the one that can
		// read a database, which is what every tool here wants to do first.
		for _, pair := range [][2]string{
			{"MONGODB_DATABASE_ADMIN_USER", "MONGODB_DATABASE_ADMIN_PASSWORD"},
			{"MONGODB_USER_ADMIN_USER", "MONGODB_USER_ADMIN_PASSWORD"},
			{"MONGODB_CLUSTER_ADMIN_USER", "MONGODB_CLUSTER_ADMIN_PASSWORD"},
		} {
			u, ok1 := a.k8sSecretKey(ctx, serverID, ns, cr+"-secrets", pair[0])
			p, ok2 := a.k8sSecretKey(ctx, serverID, ns, cr+"-secrets", pair[1])
			if ok1 && ok2 && u != "" && p != "" {
				out["admin"] = k8sCred{User: u, Pass: p, AuthDB: "admin"}
				out["app"] = out["admin"]
				break
			}
		}
	}
	return out
}

// k8sSecretJSON reads one Secret's data map, base64-decoded.
func (a *App) k8sSecretJSON(ctx context.Context, serverID, ns, name string) (map[string]string, bool) {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "secret", name, "-o", "json")
	if err != nil {
		return nil, false
	}
	var sec struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal([]byte(out), &sec) != nil {
		return nil, false
	}
	vals := make(map[string]string, len(sec.Data))
	for k, v := range sec.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			continue
		}
		vals[k] = string(b)
	}
	return vals, true
}

func (a *App) k8sSecretKey(ctx context.Context, serverID, ns, name, key string) (string, bool) {
	vals, ok := a.k8sSecretJSON(ctx, serverID, ns, name)
	if !ok {
		return "", false
	}
	v, ok := vals[key]
	return v, ok
}

// k8sPGUserSecret reads a PostgreSQL operator's per-role Secret.
func (a *App) k8sPGUserSecret(ctx context.Context, serverID, ns, name string) (k8sCred, bool) {
	vals, ok := a.k8sSecretJSON(ctx, serverID, ns, name)
	if !ok || vals["user"] == "" || vals["password"] == "" {
		return k8sCred{}, false
	}
	db := vals["dbname"]
	if db == "" {
		db = "postgres"
	}
	return k8sCred{User: vals["user"], Pass: vals["password"], Database: db}, true
}

// k8sBasicAuthSecret reads a kubernetes.io/basic-auth style Secret.
func (a *App) k8sBasicAuthSecret(ctx context.Context, serverID, ns, name string) (k8sCred, bool) {
	vals, ok := a.k8sSecretJSON(ctx, serverID, ns, name)
	if !ok {
		return k8sCred{}, false
	}
	user, pass := vals["username"], vals["password"]
	if user == "" {
		user = vals["user"]
	}
	if user == "" || pass == "" {
		return k8sCred{}, false
	}
	return k8sCred{User: user, Pass: pass}, true
}

// ---------------------------------------------------------------- resolution

// k8sFindEndpoint re-resolves one endpoint id for a caller who may reach the stack.
// Like every other target resolution in the app, it re-reads the stack and re-derives
// what is there rather than trusting the id: an endpoint whose Service was deleted, or
// whose cluster was destroyed, stops resolving.
func (a *App) k8sFindEndpoint(ctx context.Context, u User, stackID int64, id string) (k8sEndpoint, error) {
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return k8sEndpoint{}, fmt.Errorf("endpoint not found")
	}
	if st.OwnerID != u.ID && u.Role != RoleAdmin {
		return k8sEndpoint{}, fmt.Errorf("endpoint not found")
	}
	for _, e := range a.k8sStackEndpoints(ctx, st) {
		if e.ID() == id {
			return e, nil
		}
	}
	return k8sEndpoint{}, fmt.Errorf("endpoint not found — the cluster may have been destroyed or its Service removed")
}

// k8sExecArgv wraps a database client command so it runs inside this endpoint's pod.
// The result is what `kubectl exec` needs, run on the k3s server container — so the
// caller's exec seam does not have to know Kubernetes exists.
func (e k8sEndpoint) k8sExecArgv(argv []string) []string {
	out := []string{"kubectl", "-n", e.Namespace, "exec", e.Pod}
	if e.Container != "" {
		out = append(out, "-c", e.Container)
	}
	out = append(out, "-i", "--")
	return append(out, argv...)
}
