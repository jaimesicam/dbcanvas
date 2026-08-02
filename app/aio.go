package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// aio.go — the All-in-One node: ONE container running many database feature
// instances side by side.
//
// Every other node type is one product per container, so it can own the machine:
// default ports, /var/lib/<product>, the vendor systemd unit. An All-in-One node
// inverts that. It exists so a single container can demonstrate a whole estate —
// a Percona Server, a replication pair, a Patroni cluster, a Valkey cluster —
// without spending a container (and an IP, and a DNS name) on each.
//
// Three rules make co-tenancy work; each is enforced somewhere in this package:
//
//  1. Nothing runs on a default port. aio_ports.go allocates every instance a
//     private 10-port slot; the vendor units are masked so they cannot start.
//  2. Nothing shares a directory. aio_layout.go gives each instance /opt/aio/<inst>.
//  3. One package install serves a whole family, so a family has one version —
//     and, for MySQL only, one *flavor*: percona-server-server and
//     percona-xtradb-cluster-server conflict, so PXC cannot share a container
//     with ps/psrepl/innodb. That is a validation error, not a silent surprise.
//
// Instances are controlled with `aioctl` inside the container (aio_ctl.go) and
// through the same operations in the node's manager UI (aio_mgmt.go), which
// execs that same script so the two cannot drift.

// ---------------------------------------------------------------- design model

// aioInstance is one feature instance declared on an All-in-One node. It is a
// flat union in the style of designFrame: a superset of every kind's options,
// with each provisioner reading only the fields its kind uses. That keeps the
// JSON design document self-describing and lets an instance be converted to the
// synthetic designFrame/designNode the classic provisioners already accept.
type aioInstance struct {
	ID      string `json:"id"`      // stable id, unique within the node
	Kind    string `json:"kind"`    // aioKinds: ps|psrepl|pxc|innodb|pg|patroni|…
	Name    string `json:"name"`    // ps01, pxc-cluster-01 — unique within the node
	Members int    `json:"members"` // cluster kinds only; 1 for standalone

	// Shared knobs.
	RootPassword   string `json:"rootPassword"` // "" → from .env, like every classic node
	GenerateCert   bool   `json:"generateCert"`
	CertTTLValue   int    `json:"certTtlValue"`
	CertTTLUnit    string `json:"certTtlUnit"`
	ExportEnabled  bool   `json:"exportEnabled"`  // publish this instance's client port to the host
	ExportHostPort int    `json:"exportHostPort"` // 0 → auto-assign

	// Drop-down wiring. An All-in-One node draws no association lines (its
	// NODE_TYPES entry sets ports:false) — every relationship is a picker.
	PMMNodeID       string `json:"pmmNodeId"`
	OrchestratorRef string `json:"orchestratorRef"` // "<nodeId>" | "inst:<instanceId>"
	OpenBaoNodeID   string `json:"openbaoNodeId"`
	KeycloakNodeID  string `json:"keycloakNodeId"`
	SeaweedFSNodeID string `json:"seaweedfsNodeId"`
	SeaweedFSBucket string `json:"seaweedfsBucket"`
	LdapAuth        bool   `json:"ldapAuth"`
	LdapDirNodeID   string `json:"ldapDirNodeId"`
	BackendInstance string `json:"backendInstanceId"` // proxysql/haproxy → the AiO instance it fronts
	EnableVault     bool   `json:"enableVault"`

	// MySQL family.
	GTID     bool   `json:"gtid"`
	ReplMode string `json:"replMode"` // psrepl: async|semisync · innodb: innodbcluster|groupreplication

	// PostgreSQL family (pgMajor is per-instance: PPG packages are per-major and
	// co-install, so this is the one family without a shared version).
	PGMajor       string `json:"pgMajor"`
	PGVersion     string `json:"pgVersion"`
	UsePgBackRest bool   `json:"usePgBackRest"`
	UseBarman     bool   `json:"useBarman"`

	// MongoDB family.
	PSMDBSetup string `json:"psmdbSetup"` // sharded: standard|minimum
	EnablePBM  bool   `json:"enablePBM"`
	EnableOIDC bool   `json:"enableOIDC"`

	// Valkey / ProxySQL / Orchestrator.
	UseLDAP    bool   `json:"useLdap"`
	Mode       string `json:"mode"`       // proxysql: singlewrite|loadbal
	AlertEmail string `json:"alertEmail"` // orchestrator
}

// ---------------------------------------------------------------- runtime model

// aioInstanceRuntime is one *member* as actually provisioned: the registry row,
// the manager UI's table row, and what aioctl reads. One per daemon, so a
// 3-member cluster instance yields three of these.
type aioInstanceRuntime struct {
	Inst    string   `json:"inst"`    // ps01 | pxc-cluster-01-n2
	Kind    string   `json:"kind"`    //
	Family  string   `json:"family"`  //
	Group   string   `json:"group"`   // the declaring instance's Name ("" for standalone)
	Role    string   `json:"role"`    // bootstrap|member|primary|replica|mongos|config|shard|…
	Unit    string   `json:"unit"`    // aio-<inst>
	FQDN    string   `json:"fqdn"`    // per-instance DNS alias
	DataDir string   `json:"dataDir"` //
	Conf    string   `json:"conf"`    //
	Client  string   `json:"client"`  // the CLI aioctl connect should launch
	Ports   aioPorts `json:"ports"`   //
	// ExportOn is the opt-in; Export is the host port it resolved to. They are
	// separate because "publish on an auto-assigned port" is Export==0 with
	// ExportOn==true, which a single int cannot express.
	ExportOn bool `json:"exportOn"`
	Export   int  `json:"export"`
	// PGMajor is recorded for PostgreSQL instances so tools that exec a client
	// inside the container can pick the psql that matches this server — the
	// container may hold several majors, and PATH points at only one.
	PGMajor string `json:"pgMajor,omitempty"`
	// Ready is set only once this instance has actually been built. The config is
	// written optimistically at the start of a deploy (so the UI can show the
	// plan), so "present in the config" does NOT mean "provisioned" — a deploy
	// that failed halfway would otherwise make the next one skip the instance it
	// never finished. See aioPrevInstances / aioMarkReady.
	Ready bool `json:"ready"`
}

// aioConfig is the deployment's non-secret profile: enough for the manager UI to
// render the whole node without exec'ing into the container.
type aioConfig struct {
	Image     string               `json:"image"`
	OS        string               `json:"os"`
	OSVersion string               `json:"osVersion"`
	Arch      string               `json:"arch"`
	Hostname  string               `json:"hostname"`
	FQDN      string               `json:"fqdn"`
	UseProxy  bool                 `json:"useProxy"`
	Flavor    string               `json:"flavor"` // "" | "ps" | "pxc"
	Instances []aioInstanceRuntime `json:"instances"`
}

// ---------------------------------------------------------------- planning

// aioPlan resolves a design node into the concrete set of members to provision.
// It is pure (no I/O) so validation, the provisioner and the tests all agree on
// exactly what a design means.
func aioPlan(n designNode, domain, host string) []aioInstanceRuntime {
	slots := aioAssignSlots(n.AIOInstances)
	var out []aioInstanceRuntime
	for _, in := range n.AIOInstances {
		k, ok := aioKindOf(in.Kind)
		if !ok {
			continue
		}
		total := aioMemberCount(in.Kind, in.Members)
		slot := slots[in.ID]
		for m := 0; m < total; m++ {
			inst := aioMemberInst(in.Name, in.Kind, m, total)
			ports := aioPortsFor(in.Kind, slot, m)
			l := aioLayout(inst, in.Kind, ports)
			group := ""
			if k.Cluster {
				group = aioSanitizeInst(in.Name)
			}
			rt := aioInstanceRuntime{
				Inst: inst, Kind: in.Kind, Family: k.Family, Group: group,
				Role: aioRoleFor(in, m, total), Unit: l.Unit,
				FQDN: fqdnOf(inst, domain), DataDir: l.DataDir, Conf: l.ConfPath,
				Client: aioClientFor(k.Family), Ports: ports,
			}
			if k.Family == famPG {
				rt.PGMajor = ppgMajorOf(in.PGMajor)
			}
			// Only the first member of an instance can publish to the host: the
			// export toggle is per instance, and N members cannot share one port.
			if in.ExportEnabled && m == 0 {
				rt.ExportOn = true
				rt.Export = in.ExportHostPort
			}
			out = append(out, rt)
		}
	}
	return out
}

// aioRoleFor names a member's role within its instance. Roles drive start order
// (aioctl starts a group's bootstrap/primary first) and provisioning logic.
func aioRoleFor(in aioInstance, member, total int) string {
	switch in.Kind {
	case "psrepl":
		if member == 0 {
			return "primary"
		}
		return "replica"
	case "pxc", "innodb", "valkeycluster":
		if member == 0 {
			return "bootstrap"
		}
		return "member"
	case "patroni", "repmgr", "spock", "psmrs":
		if member == 0 {
			return "primary"
		}
		return "member"
	case "psmdbsharded":
		// Topology inside one container: member 0 is the mongos router, member 1
		// the (single-member) config replica set, and every remaining member its
		// own single-member shard replica set. Deliberately flatter than the
		// classic frame's 3x3 — thirty mongods in one container is not the point
		// of this node type, and MongoDB accepts single-member replica sets for
		// both the config servers and the shards.
		if member == 0 {
			return "mongos"
		}
		if member == 1 {
			return "config"
		}
		return "shard"
	case "proxysql":
		if total > 1 && member == 0 {
			return "bootstrap"
		}
		return "member"
	}
	return "standalone"
}

// aioClientFor is the interactive client `aioctl connect` launches for a family.
func aioClientFor(family string) string {
	switch family {
	case famMySQL, famProxy:
		return "mysql"
	case famPG:
		return "psql"
	case famMongo:
		return "mongosh"
	case famValkey:
		return "valkey-cli"
	}
	return "sh"
}

// aioMySQLFlavor derives the node's single MySQL flavor from its instances, and
// reports whether the design asks for both (which cannot be installed together).
func aioMySQLFlavor(instances []aioInstance) (flavor string, conflict bool) {
	ps, pxc := false, false
	for _, in := range instances {
		switch aioMySQLFlavorOfKind(in.Kind) {
		case flavorPS:
			ps = true
		case flavorPXC:
			pxc = true
		}
	}
	switch {
	case ps && pxc:
		return flavorNone, true
	case pxc:
		return flavorPXC, false
	case ps:
		return flavorPS, false
	}
	return flavorNone, false
}

// aioFamiliesUsed is the sorted set of families a node's instances need, so the
// provisioner installs each package set exactly once.
func aioFamiliesUsed(instances []aioInstance) []string {
	seen := map[string]bool{}
	var out []string
	for _, in := range instances {
		if f := aioFamilyOf(in.Kind); f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// aioEstMemMB is the rough resident footprint of every member on the node, used
// for the designer's warning and nothing else.
func aioEstMemMB(instances []aioInstance) int {
	total := 0
	for _, in := range instances {
		if k, ok := aioKindOf(in.Kind); ok {
			total += k.EstMemMB * aioMemberCount(in.Kind, in.Members)
		}
	}
	return total
}

// aioInstancesOf returns a stack's All-in-One node's declared instances.
func aioInstancesOf(doc designDoc, nodeID string) []aioInstance {
	for _, n := range doc.Nodes {
		if n.ID == nodeID {
			return n.AIOInstances
		}
	}
	return nil
}

// ---------------------------------------------------------------- provisioning

// provisionAIO records the deployment and boots the All-in-One container: base
// OS, the union of every family's packages, the aioctl control plane, then each
// family's instances in turn.
func (a *App) provisionAIO(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	image := pxcImage(n.OS, n.OSVersion, n.Arch)
	flavor, _ := aioMySQLFlavor(n.AIOInstances)
	members := aioPlan(n, domain, host)

	cfg := aioConfig{
		Image: image, OS: n.OS, OSVersion: n.OSVersion, Arch: archOr(n.Arch),
		Hostname: host, FQDN: fqdnOf(host, domain), UseProxy: n.UseProxy,
		Flavor: flavor, Instances: members,
	}
	cfgJSON, _ := json.Marshal(cfg)
	// What the PREVIOUS deploy actually built. Read before the Upsert below
	// replaces the stored config with this pass's plan — afterwards the old
	// instance list is gone, and with it any way to tell an existing instance from
	// a newly added one.
	prevInstances := aioPrevInstances(a, st, n.ID)
	// Credentials are shared across the node's MySQL-family instances, exactly as
	// a classic multi-node cluster shares them (all from .env).
	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Waiting for Intranet to be ready", 3)
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		// Reuse an existing container when this node is already deployed, so a
		// redeploy that only ADDS an instance does not destroy the datadirs of the
		// ones already running. aioCreateContainer would remove and recreate.
		pr.phase("Creating container", 8)
		id, reused, err := a.aioEnsureContainer(ctx, st, n, members, host, image, intranetIP, domain)
		if err != nil {
			pr.fail("%v", err)
			return
		}
		// Instances already provisioned into that container must not be touched
		// again: re-running a MySQL baseline would RESET a live server's GTID
		// history, and re-running initdb/rs.initiate is at best wasted work.
		fresh := aioFreshInstances(prevInstances, members)
		if reused {
			pr.logln(fmt.Sprintf("reusing the running container; %d new instance(s) to provision", len(fresh)))
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		pr.phase("Waiting for systemd", 12)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 120*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, n.OS, pr.logln)
		a.ensureDNFIPv4(ctx, id, n.OS, pr.logln)
		a.ensureRsyslog(ctx, id, n.OS, pr.logln)

		if n.UseProxy {
			proxyScript := pkgProxyRHEL
			if isDebianOS(n.OS) {
				proxyScript = pkgProxyDebian
			}
			if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.fail("configure package proxy: %v", err)
				return
			}
			pr.logln("package egress via Intranet proxy")
		}

		// The control plane goes in before any product: if a later step fails, the
		// user can still open a terminal and inspect what exists with `aioctl list`.
		pr.phase("Installing aioctl", 16)
		if err := a.aioInstallControl(ctx, id, cfg, pr); err != nil {
			pr.fail("%v", err)
			return
		}

		// Install each family's packages once, then bring up its instances.
		fams := aioFamiliesUsed(n.AIOInstances)
		for i, fam := range fams {
			base := 20 + (i*70)/max(1, len(fams))
			span := 70 / max(1, len(fams))
			if err := a.aioProvisionFamily(ctx, st, n, doc, id, fam, cfg, fresh, sec, pr, base, span); err != nil {
				pr.fail("%v", err)
				return
			}
			// Mark this family's new instances built, so a later deploy skips them.
			// Done per family rather than at the end: if a later family fails, the
			// ones that succeeded should not be rebuilt on the retry.
			done := map[string]bool{}
			for _, m := range cfg.Instances {
				if m.Family == fam && fresh[m.Inst] {
					done[m.Inst] = true
				}
			}
			a.aioMarkReady(st, n.ID, done)
		}

		// Per-instance TLS, then monitoring — certificates first, since applying
		// one restarts the instance and PMM's agent should register the server it
		// will actually keep talking to.
		a.aioApplyTLS(ctx, st, n, doc, id, cfg, fresh, pr)
		a.aioRegisterPMM(ctx, st, n, doc, id, cfg, fresh, sec, pr)

		// Cross-family wiring, once every instance exists: point each Orchestrator
		// instance at the MySQL instances that named it. Best-effort — a discovery
		// failure must not fail a node whose databases are all up.
		a.aioOrchDiscover(ctx, id, n, cfg, pr)

		// Re-read the config: family provisioners record discovered facts (real
		// published ports, versions) onto the runtime rows.
		if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil && len(dep.Config) > 0 {
			json.Unmarshal(dep.Config, &cfg)
		}
		if err := a.aioWriteRegistry(ctx, id, cfg); err != nil {
			pr.logln("refresh aioctl registry: " + err.Error())
		}

		a.reconcileStackDNS(ctx, st.ID)
		_ = intranetID

		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		pr.phase("Running", 100)
		pr.p.Message = fmt.Sprintf("provisioned (%d instance(s))", len(cfg.Instances))
		pr.save()
		log.Printf("stack %d aio %s: provisioned %d instance(s)", st.ID, n.ID, len(cfg.Instances))
	}()
}

// aioPrevInstances is the set of instance names the last deploy provisioned.
func aioPrevInstances(a *App, st Stack, nodeID string) map[string]bool {
	out := map[string]bool{}
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil || dep.ContainerID == "" {
		return out
	}
	for _, m := range aioRuntimeInstances(dep) {
		if m.Ready {
			out[m.Inst] = true
		}
	}
	return out
}

// aioMarkReady records that the named instances are now actually provisioned, so
// a later deploy skips them. Called after each family finishes, not before.
func (a *App) aioMarkReady(st Stack, nodeID string, done map[string]bool) {
	if len(done) == 0 {
		return
	}
	a.persistConfigAIO(st, nodeID, func(cfg *aioConfig) {
		for i := range cfg.Instances {
			if done[cfg.Instances[i].Inst] {
				cfg.Instances[i].Ready = true
			}
		}
	})
}

// aioFreshInstances is the members of this plan that the previous deploy did not
// build. On a first deploy that is all of them.
func aioFreshInstances(prev map[string]bool, members []aioInstanceRuntime) map[string]bool {
	out := make(map[string]bool, len(members))
	for _, m := range members {
		if !prev[m.Inst] {
			out[m.Inst] = true
		}
	}
	return out
}

// aioEnsureContainer returns the node's container, reusing a running one when the
// node is already deployed. reused is true in that case.
//
// Reuse is what makes "add an instance to a live node" safe. The cost is that
// published host ports are fixed at create time, so an instance that newly opts
// into an export needs a full destroy/redeploy to get one — called out in the log
// rather than silently ignored.
func (a *App) aioEnsureContainer(ctx context.Context, st Stack, n designNode, members []aioInstanceRuntime, host, image, intranetIP, domain string) (string, bool, error) {
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok && cid != "" {
		if err := a.engCtx(ctx).ContainerStart(ctx, cid); err == nil {
			return cid, true, nil
		}
		// Present but unstartable — fall through and rebuild it.
		a.engCtx(ctx).ContainerRemove(ctx, cid)
	}
	id, err := a.aioCreateContainer(ctx, st, n, members, host, image, intranetIP, domain)
	return id, false, err
}

// aioCreateContainer creates and starts the node's single container. Every
// instance's name is registered as a network alias so in-container resolution
// works before the Intranet zone is rewritten, and each instance that opted into
// an export gets its client port published.
func (a *App) aioCreateContainer(ctx context.Context, st Stack, n designNode, members []aioInstanceRuntime, host, image, intranetIP, domain string) (string, error) {
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
		a.engCtx(ctx).ContainerRemove(ctx, cid)
	}
	aliases := []string{host}
	var pubs []PortMap
	for _, m := range members {
		aliases = append(aliases, m.Inst)
		if m.ExportOn {
			pubs = append(pubs, PortMap{ContainerPort: m.Ports.Client, HostPort: m.Export})
		}
	}
	spec := ContainerSpec{
		Name: name, Image: image, Hostname: host, Privileged: true,
		Network: networkName(st.ID), Aliases: aliases,
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
		PublishMap: pubs,
	}
	applyVMSize(&spec, n.CPUs, n.MemoryGB)
	id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
	return id, nil
}

// aioProvisionFamily brings up every instance of one family. Each family's
// implementation lives in its own file (aio_mysql.go, …); unimplemented families
// are reported rather than silently skipped, so a design can never look
// "provisioned" when part of it was ignored.
func (a *App) aioProvisionFamily(ctx context.Context, st Stack, n designNode, doc designDoc, id, family string, cfg aioConfig, fresh map[string]bool, sec pxcSecrets, pr *pxcProg, base, span int) error {
	switch family {
	case famMySQL:
		return a.aioProvisionMySQL(ctx, st, n, doc, id, cfg, fresh, sec, pr, base, span)
	case famPG:
		return a.aioProvisionPG(ctx, st, n, doc, id, cfg, fresh, pr, base, span)
	case famValkey:
		return a.aioProvisionValkey(ctx, st, n, doc, id, cfg, fresh, pr, base, span)
	case famOrch:
		return a.aioProvisionOrch(ctx, st, n, doc, id, cfg, fresh, sec, pr, base, span)
	case famMongo:
		return a.aioProvisionMongo(ctx, st, n, doc, id, cfg, fresh, sec, pr, base, span)
	case famProxy, famHAProxy:
		return a.aioProvisionProxy(ctx, st, n, doc, id, family, cfg, fresh, sec, pr, base, span)
	}
	return fmt.Errorf("All-in-One does not support %s instances yet", family)
}

// aioSupportedFamilies is the set aioProvisionFamily can actually build. The
// designer greys out the rest and validateStack rejects them, so the error above
// is a backstop rather than the primary guard.
var aioSupportedFamilies = map[string]bool{
	famMySQL: true, famPG: true, famValkey: true, famOrch: true, famMongo: true,
	famProxy: true, famHAProxy: true,
}

// aioUnsupportedKinds narrows aioSupportedFamilies where only PART of a family
// is built. Without it a half-built family silently mis-deploys: `innodb` shares
// famMySQL with the working kinds, so it would provision as plain Percona
// Servers with no Group Replication at all — a node that reports "running" while
// being the wrong thing. Both PostgreSQL's clustered kinds and MySQL's
// cluster kinds are gated here until their provisioners exist.
// Empty: every kind in the catalog now has a provisioner. Kept as the seam for
// gating a half-built kind — see the aioUnsupportedModes map below for the
// finer-grained equivalent that still gates InnoDB Cluster mode.
var aioUnsupportedKinds = map[string]bool{}

// aioUnsupportedModes gates a MODE within an otherwise-supported kind. InnoDB
// Cluster proper is driven by MySQL Shell (dba.createCluster + addInstance over
// per-member URIs); only raw Group Replication is built, so the other mode of the
// same kind must be refused rather than silently deploying as plain GR.
var aioUnsupportedModes = map[string]string{
	"innodb:innodbcluster": "InnoDB Cluster mode needs MySQL Shell, which All-in-One does not install yet — choose Group Replication",
}

// ---------------------------------------------------------------- registry

// aioWriteRegistry renders /etc/dbcanvas/aio/instances.tsv — the table aioctl
// reads. Rewritten whenever the runtime set changes so the CLI never shows a
// stale instance.
func aioRegistryTSV(cfg aioConfig) string {
	var b strings.Builder
	b.WriteString("# dbcanvas All-in-One instance registry — generated, do not edit\n")
	b.WriteString("# inst\tkind\tfamily\tgroup\trole\tunit\tclient_port\tall_ports\tdatadir\tconf\tclient\tfqdn\n")
	for _, m := range cfg.Instances {
		ports := m.Ports.list()
		strs := make([]string, 0, len(ports))
		for _, p := range ports {
			strs = append(strs, fmt.Sprint(p))
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			m.Inst, m.Kind, m.Family, dash(m.Group), dash(m.Role), m.Unit,
			m.Ports.Client, dash(strings.Join(strs, ",")), dash(m.DataDir),
			dash(m.Conf), dash(m.Client), dash(m.FQDN))
	}
	return b.String()
}

// dash renders an empty field as "-" so the TSV stays column-aligned and awk
// never sees an empty field.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func (a *App) aioWriteRegistry(ctx context.Context, id string, cfg aioConfig) error {
	return a.engCtx(ctx).CopyFile(ctx, id, aioEtc, aioRegistryName, 0o644, []byte(aioRegistryTSV(cfg)))
}

// aioInstallControl lays down the control plane: the aio.target every instance
// unit binds to, the aioctl script, and the initial registry.
func (a *App) aioInstallControl(ctx context.Context, id string, cfg aioConfig, pr *pxcProg) error {
	if err := a.runStep(ctx, id, aioPrepControlScript, nil, pr.logln); err != nil {
		return fmt.Errorf("prepare aio control dirs: %w", err)
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/systemd/system", aioTarget, 0o644, []byte(aioTargetUnit)); err != nil {
		return fmt.Errorf("write %s: %w", aioTarget, err)
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/usr/local/bin", "aioctl", 0o755, []byte(aioCtlScript)); err != nil {
		return fmt.Errorf("write aioctl: %w", err)
	}
	if err := a.aioWriteRegistry(ctx, id, cfg); err != nil {
		return fmt.Errorf("write instance registry: %w", err)
	}
	if err := a.runStep(ctx, id, "systemctl daemon-reload && systemctl enable "+aioTarget+" >/dev/null 2>&1 || true", nil, pr.logln); err != nil {
		return fmt.Errorf("enable %s: %w", aioTarget, err)
	}
	pr.logln("aioctl installed — run `aioctl list` in this node's terminal")
	return nil
}

// aioWriteUnit writes one instance's systemd unit and its env file, then enables
// it. execStart is the full ExecStart line; the unit is otherwise uniform across
// products so `aioctl` can treat every instance the same way.
func (a *App) aioWriteUnit(ctx context.Context, id string, l instLayout, u aioUnitSpec) error {
	if u.EnvFile != "" {
		if err := a.engCtx(ctx).CopyFile(ctx, id, aioEtc, l.Inst+".env", 0o640, []byte(u.EnvFile)); err != nil {
			return fmt.Errorf("write %s env: %w", l.Inst, err)
		}
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/systemd/system", l.Unit+".service", 0o644, []byte(aioUnitFile(l, u))); err != nil {
		return fmt.Errorf("write unit %s: %w", l.Unit, err)
	}
	return nil
}

// aioNeedsRedeploy reports whether a RUNNING All-in-One node's design now plans
// instances its last deploy did not build.
//
// Every other node type is skipped once running, which is correct for them: a
// single-product node's contents cannot change without changing the node. An
// All-in-One node's can — the instance list is edited on the node itself — so
// without this, adding a feature to a deployed node silently did nothing.
//
// Only ADDITIONS re-enter. A removed instance is left running: tearing down a
// datadir is destructive and belongs behind an explicit action, not a redeploy.
func aioNeedsRedeploy(a *App, st Stack, n designNode, dep Deployment) bool {
	// Ready, not merely present: an instance the last deploy planned but never
	// finished building must be re-entered, not treated as done.
	prev := map[string]bool{}
	for _, m := range aioRuntimeInstances(dep) {
		if m.Ready {
			prev[m.Inst] = true
		}
	}
	domain := envOr("DOMAIN", "example.net")
	for _, m := range aioPlan(n, domain, "") {
		if !prev[m.Inst] {
			return true
		}
	}
	return false
}
