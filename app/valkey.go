package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Valkey nodes run the upstream valkey/valkey-bundle image (Debian-based; bundles
// valkey-server + the json/search/bloom/ldap modules), pulled at deploy. A standalone
// node (Type=="valkey") is the Valkey analogue of the standalone Percona Server node.
//
// Auth: a "default" user password (requirepass) is always set (shown in node props).
// Optionally the bundled valkey-ldap module is wired to the Intranet OpenLDAP so ACL
// users authenticate against it. pmm-client is installed (percona-release + pmm3-client)
// and the node registered with an associated PMM server.

const (
	valkeyImage     = "valkey/valkey-bundle:latest"
	valkeyImageRepo = "valkey/valkey-bundle"
	valkeyImageTag  = "latest"
	valkeyPort      = 6379
	valkeyConfPath  = "/etc/dbcanvas-valkey.conf"
)

// valkeyConfig is the non-secret profile shown for a deployed Valkey node.
type valkeyConfig struct {
	Image       string `json:"image"`
	Role        string `json:"role"` // "standalone" (cluster members add their own later)
	Hostname    string `json:"hostname"`
	FQDN        string `json:"fqdn"`
	ExportPort  int    `json:"exportPort"` // host-published 6379 (0 if unpublished)
	UseLDAP     bool   `json:"useLdap"`
	LDAPServers string `json:"ldapServers"` // ldap://intranet.<domain>:389 when LDAP on
	MonitoredBy string `json:"monitoredBy"`
	UseProxy    bool   `json:"useProxy"`
	Ports       []int  `json:"ports"`
}

// valkeySecrets holds the default-user password (requirepass / masterauth).
type valkeySecrets struct {
	Password string `json:"password"`
}

// valkeyConfFile renders the valkey.conf. When ldap is set it loads the valkey-ldap
// module first (so the ldap.* directives parse) and points it at the Intranet OpenLDAP;
// when cluster is set it enables clustering. The bundle entrypoint auto-loads the other
// modules and skips any module already loaded here.
func valkeyConfFile(password, domain, baseDN string, ldap, cluster bool) string {
	var b strings.Builder
	if ldap {
		// Must load the module before its ldap.* config directives are parsed.
		b.WriteString("loadmodule /usr/lib/valkey/libvalkey_ldap.so\n")
	}
	fmt.Fprintf(&b, "port %d\n", valkeyPort)
	b.WriteString("bind 0.0.0.0\n")
	b.WriteString("protected-mode no\n")
	fmt.Fprintf(&b, "requirepass %s\n", password)
	fmt.Fprintf(&b, "masterauth %s\n", password)
	b.WriteString("dir /data\n")
	b.WriteString("appendonly yes\n")
	if ldap {
		fmt.Fprintf(&b, "ldap.servers ldap://intranet.%s:389\n", domain)
		b.WriteString("ldap.auth_mode bind\n")
		b.WriteString("ldap.bind_dn_prefix uid=\n")
		fmt.Fprintf(&b, "ldap.bind_dn_suffix ,ou=People,%s\n", baseDN)
		fmt.Fprintf(&b, "ldap.search_base ou=People,%s\n", baseDN)
		b.WriteString("ldap.search_attribute uid\n")
	}
	if cluster {
		b.WriteString("cluster-enabled yes\n")
		b.WriteString("cluster-config-file nodes.conf\n")
		b.WriteString("cluster-node-timeout 5000\n")
	}
	return b.String()
}

// provisionValkeyStandalone records the deployment then brings up a single Valkey node.
func (a *App) provisionValkeyStandalone(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	baseDN := domainToDN(domain)
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	pw := strings.TrimSpace(n.RootPassword)
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		var s valkeySecrets
		if json.Unmarshal(dep.Secrets, &s) == nil && s.Password != "" {
			pw = s.Password
		}
	}
	if pw == "" {
		pw = genSecret("Valkey!")
	}
	sec := valkeySecrets{Password: pw}
	secJSON, _ := json.Marshal(sec)

	monitoredBy := ""
	if n.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == n.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[m.ID], domain)
			}
		}
	}
	ldapServers := ""
	if n.UseLDAP {
		ldapServers = fmt.Sprintf("ldap://intranet.%s:389", domain)
	}
	cfg := valkeyConfig{
		Image: valkeyImage, Role: "standalone", Hostname: host, FQDN: fqdn,
		UseLDAP: n.UseLDAP, LDAPServers: ldapServers, MonitoredBy: monitoredBy,
		UseProxy: n.UseProxy, Ports: []int{valkeyPort},
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	go func() {
		ctx := context.Background()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Pulling image", 8)
		if ok, _ := a.docker.ImageExists(ctx, valkeyImage); !ok {
			pr.logln("pulling " + valkeyImage)
			if err := a.docker.ImagePull(ctx, valkeyImageRepo, valkeyImageTag); err != nil {
				pr.fail("pull image: %v", err)
				return
			}
		}

		pr.phase("Waiting for Intranet to be ready", 14)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 22)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: valkeyImage, Hostname: host,
			Cmd:     []string{"valkey-server", valkeyConfPath},
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		if n.ExportEnabled {
			spec.PublishMap = []PortMap{{ContainerPort: valkeyPort, HostPort: n.ExportHostPort}}
		}
		id, err := a.docker.ContainerCreate(ctx, spec)
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		// Stage the config into the created (not-yet-started) container before launch.
		conf := valkeyConfFile(pw, domain, baseDN, n.UseLDAP, false)
		if err := a.docker.CopyFile(ctx, id, "/etc", "dbcanvas-valkey.conf", 0o644, []byte(conf)); err != nil {
			pr.fail("write valkey.conf: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}

		if n.ExportEnabled {
			if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", valkeyPort)); e == nil {
				if p, e2 := strconv.Atoi(hp); e2 == nil {
					cfg.ExportPort = p
				}
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		// Wait until Valkey answers an authenticated PING.
		pr.phase("Waiting for Valkey", 40)
		if err := a.runStep(ctx, id, valkeyPingScript, []string{"PW=" + pw}, pr.logln); err != nil {
			pr.fail("valkey did not become ready: %v", err)
			return
		}
		if n.UseLDAP {
			pr.logln("valkey-ldap wired to ldap://intranet." + domain + ":389 (ou=People," + baseDN + ")")
		}

		// Install pmm-client (percona-release + pmm3-client) — best-effort — and register
		// the node with an associated PMM server.
		pr.phase("Installing pmm-client", 60)
		if err := a.runStep(ctx, id, valkeyInstallPMMScript, nil, pr.logln); err != nil {
			pr.logln("pmm-client install skipped: " + err.Error())
		} else if pmmFQDN, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, n.PMMNodeID); ok {
			env := []string{"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass, "NODE=" + host}
			if err := a.runStep(ctx, id, valkeyRegisterPMMScript, env, pr.logln); err != nil {
				pr.logln("PMM registration skipped: " + err.Error())
			} else {
				pr.logln("registered with PMM (" + pmmFQDN + ")")
			}
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d valkey %s: provisioned (standalone)", st.ID, n.Label)
	}()
}

// valkeyPingScript waits until Valkey answers an authenticated PING.
const valkeyPingScript = `set -e
for i in $(seq 1 30); do
  valkey-cli -a "$PW" --no-auth-warning PING 2>/dev/null | grep -q PONG && exit 0
  sleep 1
done
echo "valkey not answering authenticated PING"; exit 1`

// valkeyInstallPMMScript installs pmm-client into the (Debian-based) bundle image via
// percona-release. Best-effort.
const valkeyInstallPMMScript = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v pmm-admin >/dev/null 2>&1 && exit 0
apt-get update -qq >/dev/null 2>&1 || true
apt-get install -y -qq wget gnupg2 ca-certificates >/dev/null 2>&1 || true
wget -qO /tmp/percona-release.deb https://repo.percona.com/apt/percona-release_latest.generic_all.deb
dpkg -i /tmp/percona-release.deb >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get -y -qq -f install >/dev/null; }
percona-release setup -y pmm3-client >/dev/null 2>&1 || percona-release enable -y pmm3-client >/dev/null 2>&1 || true
apt-get update -qq >/dev/null
apt-get install -y -qq pmm-client >/dev/null
command -v pmm-admin >/dev/null 2>&1 || { echo "pmm-client not installed"; exit 1; }`

// valkeyRegisterPMMScript points pmm-admin at the PMM server (node-level metrics).
const valkeyRegisterPMMScript = `set -e
pmm-admin config --force --server-insecure-tls --server-url="https://$PMM_USER:$PMM_PASS@$PMM_FQDN:8443" "$NODE" >/dev/null 2>&1 || \
pmm-admin config --force --server-insecure-tls --server-url="https://$PMM_USER:$PMM_PASS@$PMM_FQDN:8443" >/dev/null`

// ------------------------------------------------------------ Valkey cluster

// provisionValkeyClusterFrame brings up a Valkey Cluster: every member runs
// valkey/valkey-bundle with cluster-enabled, then one member runs `valkey-cli --cluster
// create` over all members (all-master, 3–7 shards). Shared default-user password +
// optional LDAP across the cluster.
func (a *App) provisionValkeyClusterFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	baseDN := domainToDN(domain)
	hosts := stackHostnames(doc)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "valkeycluster" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	if len(members) < 3 {
		log.Printf("stack %d valkeycluster %s: need >=3 members, have %d", st.ID, frame.Label, len(members))
		return
	}

	// Shared default-user password, reused across redeploys.
	pw := strings.TrimSpace(frame.RootPassword)
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s valkeySecrets
			if json.Unmarshal(dep.Secrets, &s) == nil && s.Password != "" {
				pw = s.Password
				break
			}
		}
	}
	if pw == "" {
		pw = genSecret("Valkey!")
	}
	sec := valkeySecrets{Password: pw}
	secJSON, _ := json.Marshal(sec)

	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == frame.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[m.ID], domain)
			}
		}
	}
	ldapServers := ""
	if frame.UseLDAP {
		ldapServers = fmt.Sprintf("ldap://intranet.%s:389", domain)
	}
	for _, n := range members {
		host := hosts[n.ID]
		cfg := valkeyConfig{
			Image: valkeyImage, Role: "cluster", Hostname: host, FQDN: fqdnOf(host, domain),
			UseLDAP: frame.UseLDAP, LDAPServers: ldapServers, MonitoredBy: monitoredBy, Ports: []int{valkeyPort},
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	go func() {
		ctx := context.Background()
		progs := map[string]*pxcProg{}
		for _, n := range members {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			for _, n := range members {
				progs[n.ID].fail("%v", werr)
			}
			return
		}
		if ok, _ := a.docker.ImageExists(ctx, valkeyImage); !ok {
			if err := a.docker.ImagePull(ctx, valkeyImageRepo, valkeyImageTag); err != nil {
				for _, n := range members {
					progs[n.ID].fail("pull image: %v", err)
				}
				return
			}
		}

		// Phase 1 (parallel): create + configure + start every member.
		var wg sync.WaitGroup
		failed := false
		var mu sync.Mutex
		conf := valkeyConfFile(pw, domain, baseDN, frame.UseLDAP, true)
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.valkeyStartMember(ctx, st, n, hosts[n.ID], intranetIP, domain, conf, pw, progs[n.ID]); err != nil {
					mu.Lock()
					failed = true
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if failed {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// Phase 2: form the cluster from the first member.
		first := members[0]
		fdep, _ := a.store.GetDeployment(st.ID, first.ID)
		var nodeArgs []string
		for _, n := range members {
			nodeArgs = append(nodeArgs, fmt.Sprintf("%s:%d", fqdnOf(hosts[n.ID], domain), valkeyPort))
		}
		progs[first.ID].phase("Forming cluster", 70)
		if err := a.runStep(ctx, fdep.ContainerID, valkeyClusterCreateScript, []string{"PW=" + pw, "NODES=" + strings.Join(nodeArgs, " ")}, progs[first.ID].logln); err != nil {
			progs[first.ID].fail("form cluster: %v", err)
			return
		}

		// Phase 3: pmm-client per member (best-effort).
		pmmFQDN, pmmUser, pmmPass, havePMM := a.pmmServerFor(st, doc, frame.PMMNodeID)
		for _, n := range members {
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			pr := progs[n.ID]
			pr.phase("Installing pmm-client", 88)
			if err := a.runStep(ctx, dep.ContainerID, valkeyInstallPMMScript, nil, pr.logln); err == nil && havePMM {
				a.runStep(ctx, dep.ContainerID, valkeyRegisterPMMScript, []string{"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass, "NODE=" + hosts[n.ID]}, pr.logln)
			}
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: dep.ContainerID, State: DeployRunning, Config: a.depConfig(st.ID, n.ID), Secrets: secJSON})
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d valkeycluster %s: provisioned (%d shards)", st.ID, frame.Label, len(members))
	}()
}

// valkeyStartMember creates + configures + starts one cluster member and waits for PING.
func (a *App) valkeyStartMember(ctx context.Context, st Stack, n designNode, host, intranetIP, domain, conf, pw string, pr *pxcProg) error {
	pr.phase("Creating container", 25)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
		a.docker.ContainerRemove(ctx, cid)
	}
	spec := ContainerSpec{
		Name: name, Image: valkeyImage, Hostname: host,
		Cmd:     []string{"valkey-server", valkeyConfPath},
		Network: networkName(st.ID), Aliases: []string{host},
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
	}
	if n.ExportEnabled {
		spec.PublishMap = []PortMap{{ContainerPort: valkeyPort, HostPort: n.ExportHostPort}}
	}
	id, err := a.docker.ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.docker.CopyFile(ctx, id, "/etc", "dbcanvas-valkey.conf", 0o644, []byte(conf)); err != nil {
		return pr.fail("write valkey.conf: %v", err)
	}
	if err := a.docker.ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	if n.ExportEnabled {
		if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", valkeyPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				if dep, e3 := a.store.GetDeployment(st.ID, n.ID); e3 == nil {
					var cfg valkeyConfig
					json.Unmarshal(dep.Config, &cfg)
					cfg.ExportPort = p
					cfgJSON, _ := json.Marshal(cfg)
					a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: dep.Secrets})
				}
			}
		}
	} else {
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: a.depConfig(st.ID, n.ID), Secrets: a.depSecrets(st.ID, n.ID)})
	}
	pr.phase("Waiting for Valkey", 45)
	return a.runStep(ctx, id, valkeyPingScript, []string{"PW=" + pw}, pr.logln)
}

// valkeyClusterCreateScript forms the cluster (idempotent: skips if already ok).
const valkeyClusterCreateScript = `set -e
valkey-cli -a "$PW" --no-auth-warning CLUSTER INFO 2>/dev/null | grep -q 'cluster_state:ok' && { echo "cluster already formed"; exit 0; }
valkey-cli -a "$PW" --no-auth-warning --cluster create $NODES --cluster-replicas 0 --cluster-yes
# Slot assignment is immediate but cluster_state:ok needs a few seconds of gossip.
for i in $(seq 1 20); do
  valkey-cli -a "$PW" --no-auth-warning CLUSTER INFO 2>/dev/null | grep -q 'cluster_state:ok' && { echo "cluster_state:ok"; exit 0; }
  sleep 2
done
echo "cluster did not reach state ok:"; valkey-cli -a "$PW" --no-auth-warning CLUSTER INFO 2>/dev/null | head; exit 1`
