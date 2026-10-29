package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"time"
)

// A Performance Sim linked to a Kubernetes frame drives the database the frame's
// operator runs. The stressors act on each member, so each database pod needs
// an address the sim can dial from outside the cluster. A pod the operator has
// already published (a LoadBalancer or NodePort Service selecting that one pod)
// keeps that address; any other gets a companion LoadBalancer Service of its
// own, the same kind the Expose control creates (k8sexpose.go): beside the
// operator's Services, never one of them, labelled so it can be removed again.
// The workload goes through the cluster's front door, exposed the same way when
// it has no address yet.
//
// What still cannot work is said by the sim rather than hidden: network and
// disk conditions need a container DBCanvas runs, so they are unavailable on
// pods; and on the PostgreSQL operators, pausing or delaying replay would be
// undone by the operator, so those dials say so.

// perfSimK8s is what perfSimK8sTarget resolved.
type perfSimK8s struct {
	writer   perfSimEndpoint
	uri      string // MongoDB: the workload's connection string
	user     string
	pass     string
	database string // PostgreSQL: CloudNativePG's application database
	members  []perfSimMember
	operator string
	name     string
	ns       string
	exposed  []string // the companion Services this sim created
}

// perfSimK8sPort is the database port inside the pods.
func perfSimK8sPort(engine string) int {
	switch engine {
	case dexPostgres:
		return patroniPGPort
	case dexMongoDB:
		return mongoPort
	}
	return pxcMySQLPort
}

// perfSimK8sDataPod reports whether a pod runs the database itself (rather than
// a proxy, a router, a config server or a backup agent), and its replica set
// for MongoDB.
func perfSimK8sDataPod(p k8sTargetPod, engine string) (ok bool, replset string) {
	if p.Status.Phase != "Running" {
		return false, ""
	}
	has := map[string]bool{}
	for _, c := range p.Spec.Containers {
		has[c.Name] = true
	}
	switch engine {
	case dexMySQL:
		return has["pxc"] || has["mysql"], ""
	case dexMongoDB:
		rs := p.Metadata.Labels["app.kubernetes.io/replset"]
		comp := p.Metadata.Labels["app.kubernetes.io/component"]
		return has["mongod"] && rs != "cfg" && comp != "cfg" && comp != "mongos", rs
	case dexPostgres:
		return has["database"] || (has["postgres"] && p.Metadata.Labels["cnpg.io/instanceName"] != ""), ""
	}
	return false, ""
}

// perfSimK8sSelector is a selector matching exactly this pod.
func perfSimK8sSelector(p k8sTargetPod) map[string]string {
	if p.Metadata.Labels[k8sPodNameSelector] != "" {
		return map[string]string{k8sPodNameSelector: p.Metadata.Name}
	}
	if p.Metadata.Labels["cnpg.io/instanceName"] != "" {
		return map[string]string{"cnpg.io/instanceName": p.Metadata.Name}
	}
	return nil
}

// sameSelector reports whether two selectors are the same.
func sameSelector(a, b map[string]string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// perfSimK8sTarget exposes the frame's database pods and front door and reads
// the credentials the sim uses there.
func (a *App) perfSimK8sTarget(ctx context.Context, st Stack, doc designDoc, f designFrame, engine string, prevExposed []string, timeout time.Duration, pr *pxcProg) (perfSimK8s, error) {
	ctx = withEngine(ctx, a.docker) // k3d clusters are always Docker
	deadline := time.Now().Add(timeout)
	var serverID string
	var cfg k3dConfig
	var lastState string
	for {
		var ok bool
		if serverID, cfg, ok = a.k8sFrameServer(st, doc, f); ok && cfg.Operator != "" {
			// CloudNativePG's Cluster has a phase rather than Percona's .status.state;
			// Crunchy PGO reports neither, and its pods are waited for below.
			if cfg.Operator == "cnpg" {
				ns := cfg.Namespace
				if ns == "" {
					ns = "default"
				}
				if strings.HasPrefix(a.cnpgStatus(ctx, serverID, ns, cfg.ClusterName), cnpgHealthyPhase) {
					break
				}
			} else if state, ready := a.k3dCRState(ctx, serverID, cfg); ready || cfg.Operator == "pgo" {
				break
			} else {
				// An operator can pass through "error" while a cluster initializes,
				// so the state only decides at the deadline.
				lastState = state
			}
		}
		if time.Now().After(deadline) {
			return perfSimK8s{}, fmt.Errorf("the database in Kubernetes frame %s did not become ready within %s (last state %q)", f.Label, timeout, orDefault(lastState, "unknown"))
		}
		select {
		case <-ctx.Done():
			return perfSimK8s{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = "default"
	}
	out := perfSimK8s{operator: cfg.Operator, name: f.Label, ns: ns}
	if cfg.ClusterName != "" && cfg.ClusterName != sanitizeName(f.Label) {
		out.name += " · " + cfg.ClusterName
	}
	port := perfSimK8sPort(engine)
	nodeIP := a.k8sNodeAddr(ctx, st.ID, serverID)
	// The companions that exist now. One made for a Performance Sim is this
	// sim's to keep and remove, also when an earlier sim left it behind (its
	// removal could not reach the cluster); one the Expose control made is used
	// but not recorded, so removing the sim leaves it.
	existing := map[string]bool{}
	mine := map[string]bool{}
	if svcs, err := a.k8sServices(ctx, serverID, ns); err == nil {
		for _, s := range svcs {
			existing[s.Metadata.Name] = true
			if s.Metadata.Annotations[k8sExposeForAnnotation] == "perfsim" {
				mine[s.Metadata.Name] = true
			}
		}
	}
	for _, name := range prevExposed {
		if existing[name] {
			mine[name] = true
		}
	}
	expose := func(name string, sel map[string]string) error {
		e := k8sEndpoint{StackID: st.ID, FrameID: f.ID, ServerID: serverID, Namespace: ns,
			Service: name, Label: name, Selector: sel, Port: port, TargetPort: port, Engine: engine, ExposeFor: "perfsim"}
		res, err := a.k8sExposeEndpoint(ctx, e)
		if err == nil {
			pr.logln(res.Message)
			if !existing[name+k8sExposeSuffix] {
				mine[name+k8sExposeSuffix] = true
			}
		}
		return err
	}
	addrOf := func(svcs []k8sTargetSvc, name string) (string, int) {
		for _, s := range svcs {
			if s.Metadata.Name == name {
				if addr, p, _ := k8sAddressOf(s, port, nodeIP); addr != "" {
					return addr, p
				}
			}
		}
		return "", 0
	}

	// ---- members: every database pod, by an address of its own
	pods := a.k8sPods(ctx, serverID, ns)
	svcs, err := a.k8sServices(ctx, serverID, ns)
	if err != nil {
		return out, err
	}
	type pod struct {
		name, replset string
		sel           map[string]string
	}
	var data []pod
	for _, p := range pods {
		if ok, rs := perfSimK8sDataPod(p, engine); ok {
			if sel := perfSimK8sSelector(p); sel != nil {
				data = append(data, pod{p.Metadata.Name, rs, sel})
			}
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i].name < data[j].name })
	if len(data) == 0 {
		return out, fmt.Errorf("found no running database pods in namespace %s of %s", ns, f.Label)
	}
	podSvc := map[string]string{} // pod → the Service that gives it an address
	for _, p := range data {
		for _, s := range svcs {
			if sameSelector(s.Spec.Selector, p.sel) {
				if addr, _, _ := k8sAddressOf(s, port, nodeIP); addr != "" {
					podSvc[p.name] = s.Metadata.Name
				}
			}
		}
		if podSvc[p.name] == "" {
			if err := expose(p.name, p.sel); err != nil {
				pr.logln(fmt.Sprintf("%s: %v", p.name, err))
				continue
			}
			podSvc[p.name] = p.name + k8sExposeSuffix
		}
	}
	// The companions' addresses, once MetalLB (or the NodePort fallback) has them.
	for time.Now().Before(deadline) {
		svcs, _ = a.k8sServices(ctx, serverID, ns)
		missing := 0
		for _, p := range data {
			if addr, _ := addrOf(svcs, podSvc[p.name]); addr == "" {
				missing++
			}
		}
		if missing == 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	for _, p := range data {
		addr, p2 := addrOf(svcs, podSvc[p.name])
		if addr == "" {
			pr.logln(fmt.Sprintf("%s has no address the sim can reach; it is left out", p.name))
			continue
		}
		out.members = append(out.members, perfSimMember{ID: p.name, Host: addr, Port: p2, Role: p.replset})
	}
	if len(out.members) == 0 {
		return out, fmt.Errorf("none of the database pods in %s could be given an address", f.Label)
	}

	// ---- credentials
	creds := a.k8sCredentials(ctx, serverID, ns, cfg)
	admin := creds["admin"]
	out.user, out.pass, out.database = admin.User, admin.Pass, ""
	if cfg.Operator == "cnpg" {
		// CloudNativePG publishes an application user that cannot create a
		// database; the sim works in a schema of the application's database.
		out.database = admin.Database
	}
	if engine == dexMongoDB {
		u, p, err := a.perfSimK8sMongoUser(ctx, serverID, ns, cfg, data[0].name)
		if err != nil {
			return out, err
		}
		out.user, out.pass = u, p
	}
	if out.user == "" {
		return out, fmt.Errorf("could not read the database credentials out of the operator's Secrets in %s", f.Label)
	}

	// ---- the front door
	cr := cfg.ClusterName
	var front []string
	switch cfg.Operator {
	case "pxc":
		front = []string{cr + "-haproxy", cr + "-proxysql"}
	case "ps":
		front = []string{cr + "-haproxy", cr + "-router"}
	case "psmdb":
		if cfg.Sharding {
			front = []string{cr + "-mongos"}
		}
	case "cnpg":
		front = []string{cr + "-rw"}
	}
	for _, name := range front {
		for _, s := range svcs {
			if s.Metadata.Name != name {
				continue
			}
			addr, p := addrOf(svcs, name)
			if addr == "" {
				addr, p = addrOf(svcs, name+k8sExposeSuffix)
			}
			if addr == "" && len(s.Spec.Selector) > 0 && expose(name, s.Spec.Selector) == nil {
				for i := 0; i < 15 && addr == ""; i++ {
					time.Sleep(time.Second)
					s2, _ := a.k8sServices(ctx, serverID, ns)
					addr, p = addrOf(s2, name+k8sExposeSuffix)
				}
			}
			if addr != "" && out.writer.Host == "" {
				out.writer = perfSimEndpoint{Host: addr, Port: p}
				pr.logln(fmt.Sprintf("workload through %s at %s:%d", name, addr, p))
			}
		}
	}
	if out.writer.Host == "" {
		// No front door with an address: the member that takes writes now.
		w := a.perfSimK8sWriterPod(ctx, serverID, ns, cfg, engine, out.members)
		for _, m := range out.members {
			if m.ID == w {
				out.writer = perfSimEndpoint{Host: m.Host, Port: m.Port}
			}
		}
		if out.writer.Host == "" {
			out.writer = perfSimEndpoint{Host: out.members[0].Host, Port: out.members[0].Port}
		}
		pr.logln(fmt.Sprintf("workload directly on %s (%s:%d): it does not follow a failover", orDefault(w, out.members[0].ID), out.writer.Host, out.writer.Port))
	}
	for name := range mine {
		out.exposed = append(out.exposed, name)
	}
	sort.Strings(out.exposed)
	if engine == dexMongoDB {
		creds := url.QueryEscape(out.user) + ":" + url.QueryEscape(out.pass)
		out.uri = fmt.Sprintf("mongodb://%s@%s:%d/?authSource=admin", creds, out.writer.Host, out.writer.Port)
		if !cfg.Sharding {
			// The replica set's member names are cluster DNS names, which do not
			// resolve outside Kubernetes: the driver must not rediscover them.
			out.uri += "&directConnection=true"
		}
	}
	return out, nil
}

// perfSimK8sWriterPod names the pod taking writes, where that can be read: the
// MongoDB primary, the PostgreSQL primary by the operator's role label. PXC
// takes writes on any member.
func (a *App) perfSimK8sWriterPod(ctx context.Context, serverID, ns string, cfg k3dConfig, engine string, members []perfSimMember) string {
	switch engine {
	case dexMongoDB:
		rs := a.k3dPSMDBReplsetName(ctx, serverID, cfg)
		if p, ok := a.k3dPSMDBPrimaryService(ctx, serverID, cfg, rs, len(members)); ok {
			return p
		}
	case dexPostgres:
		for _, p := range a.k8sPods(ctx, serverID, ns) {
			l := p.Metadata.Labels
			if l["postgres-operator.crunchydata.com/role"] == "master" || l["postgres-operator.crunchydata.com/role"] == "primary" ||
				l["cnpg.io/instanceRole"] == "primary" || l["role"] == "primary" {
				return p.Metadata.Name
			}
		}
	}
	return ""
}

// perfSimK8sMongoUser gives the sim an account that can do what its stressors
// do: lock a member, reconfigure a replica set, and write its own database. Of
// the operator's accounts none does all three, so one is created with the
// operator's user administrator: through mongos and on every shard's replica
// set on a sharded cluster (a direct connection to a shard member authenticates
// against that shard's own users), on the replica set otherwise.
func (a *App) perfSimK8sMongoUser(ctx context.Context, serverID, ns string, cfg k3dConfig, pod string) (string, string, error) {
	cr := cfg.ClusterName
	ua, ok1 := a.k8sSecretKey(ctx, serverID, ns, cr+"-secrets", "MONGODB_USER_ADMIN_USER")
	up, ok2 := a.k8sSecretKey(ctx, serverID, ns, cr+"-secrets", "MONGODB_USER_ADMIN_PASSWORD")
	if !ok1 || !ok2 || ua == "" || up == "" {
		return "", "", fmt.Errorf("could not read the user administrator out of Secret %s-secrets", cr)
	}
	user, pass := "perfsim", a.perfSimK8sMongoPassword(ctx, serverID, ns, cr)
	js := fmt.Sprintf(`const a=db.getSiblingDB("admin");`+
		`let roles=[{role:"root",db:"admin"}];`+
		`if(a.getRole("directShardOperations",{showBuiltinRoles:true}))roles.push({role:"directShardOperations",db:"admin"});`+
		`if(a.getUser(%q))a.updateUser(%q,{pwd:%q,roles:roles});else a.createUser({user:%q,pwd:%q,roles:roles})`,
		user, user, pass, user, pass)
	targets := a.perfSimK8sMongoURIs(ctx, serverID, ns, cfg, ua, up)
	for _, uri := range targets {
		if _, err := a.kubectl(ctx, serverID, "-n", ns, "exec", pod, "-c", "mongod", "--",
			"mongosh", "--quiet", uri, "--eval", js); err != nil {
			return "", "", fmt.Errorf("creating the sim's MongoDB user: %v", err)
		}
	}
	return user, pass, nil
}

// perfSimK8sMongoPassword is the sim's MongoDB password, kept in a Secret of the
// cluster's namespace so a redeployed sim keeps using the same one.
func (a *App) perfSimK8sMongoPassword(ctx context.Context, serverID, ns, cr string) string {
	name := cr + "-dbcanvas-perfsim"
	if pw, ok := a.k8sSecretKey(ctx, serverID, ns, name, "password"); ok && pw != "" {
		return pw
	}
	pw := randHex(16)
	a.kubectlApplyNS(ctx, serverID, ns, fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\n  labels:\n    %s: \"true\"\nstringData:\n  password: %q\n",
		name, ns, k8sExposeLabel, pw))
	return pw
}

// perfSimK8sMongoURIs are where the sim's MongoDB user lives, as in-cluster
// connection strings for mongosh in a pod: each replica set, and mongos on a
// sharded cluster.
func (a *App) perfSimK8sMongoURIs(ctx context.Context, serverID, ns string, cfg k3dConfig, user, pass string) []string {
	cr := cfg.ClusterName
	creds := url.QueryEscape(user) + ":" + url.QueryEscape(pass)
	rsets := map[string]bool{}
	for _, p := range a.k8sPods(ctx, serverID, ns) {
		if ok, rs := perfSimK8sDataPod(p, dexMongoDB); ok && rs != "" {
			rsets[rs] = true
		}
	}
	var out []string
	for rs := range rsets {
		out = append(out, fmt.Sprintf("mongodb://%s@%s-%s.%s.svc.cluster.local/admin?replicaSet=%s", creds, cr, rs, ns, rs))
	}
	if cfg.Sharding {
		out = append(out, fmt.Sprintf("mongodb://%s@%s-mongos.%s.svc.cluster.local/admin", creds, cr, ns))
	}
	sort.Strings(out)
	return out
}

// clearPerfSimK8s removes what a sim created on its Kubernetes frame: its
// companion Services, and on MongoDB its user and the Secret holding its
// password. Best-effort: when the whole stack goes, the cluster is gone first
// and there is nothing left to remove.
func (a *App) clearPerfSimK8s(ctx context.Context, st Stack, d Deployment) {
	var cfg perfSimConfig
	if json.Unmarshal(d.Config, &cfg) != nil || cfg.K8sFrame == "" {
		return
	}
	doc := buildDoc(st)
	serverID, kcfg, ok := a.k8sFrameServer(st, doc, frameByID(doc, cfg.K8sFrame))
	if !ok {
		log.Printf("perfsim %s: the Kubernetes frame %s is not running; its Services %v stay", d.NodeID, cfg.K8sFrame, cfg.K8sExposed)
		return
	}
	ctx = withEngine(ctx, a.docker)
	ns := cfg.K8sNamespace
	// Another sim on the same frame shares the pods' Services: those stay.
	shared := map[string]bool{}
	userShared := false
	if deps, err := a.store.ListDeployments(st.ID); err == nil {
		for _, o := range deps {
			var oc perfSimConfig
			if o.NodeID == d.NodeID || json.Unmarshal(o.Config, &oc) != nil || oc.K8sFrame != cfg.K8sFrame {
				continue
			}
			for _, name := range oc.K8sExposed {
				shared[name] = true
			}
			userShared = userShared || oc.K8sMongoUser
		}
	}
	for _, name := range cfg.K8sExposed {
		if shared[name] {
			continue
		}
		// k8sUnexposeEndpoint deletes only a Service carrying DBCanvas's label.
		if err := a.k8sUnexposeEndpoint(ctx, k8sEndpoint{StackID: st.ID, FrameID: cfg.K8sFrame, ServerID: serverID, Namespace: ns, Service: name}); err != nil {
			log.Printf("perfsim %s: removing Service %s: %v", d.NodeID, name, err)
		}
	}
	if cfg.K8sMongoUser && !userShared {
		cr := kcfg.ClusterName
		ua, _ := a.k8sSecretKey(ctx, serverID, ns, cr+"-secrets", "MONGODB_USER_ADMIN_USER")
		up, _ := a.k8sSecretKey(ctx, serverID, ns, cr+"-secrets", "MONGODB_USER_ADMIN_PASSWORD")
		var pod string
		for _, p := range a.k8sPods(ctx, serverID, ns) {
			if ok, _ := perfSimK8sDataPod(p, dexMongoDB); ok {
				pod = p.Metadata.Name
				break
			}
		}
		if ua != "" && up != "" && pod != "" {
			for _, uri := range a.perfSimK8sMongoURIs(ctx, serverID, ns, kcfg, ua, up) {
				a.kubectl(ctx, serverID, "-n", ns, "exec", pod, "-c", "mongod", "--",
					"mongosh", "--quiet", uri, "--eval", `if(db.getSiblingDB("admin").getUser("perfsim"))db.getSiblingDB("admin").dropUser("perfsim")`)
			}
		}
		a.kubectl(ctx, serverID, "-n", ns, "delete", "secret", cr+"-dbcanvas-perfsim", "--ignore-not-found")
	}
}
