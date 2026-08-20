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

// Valkey nodes run on the same systemd base images as every other Percona
// product (dbcanvas-systemd:<os>-<osVersion>-<arch>), with Valkey itself
// installed via percona-release (the "valkey-91" repo) and the
// percona-valkey* packages (Percona Valkey + the bloom/json/search modules,
// plus valkey-ldap on Oracle Linux), not a pulled Docker image. A standalone
// node (Type=="valkey") is the Valkey analogue of the standalone Percona
// Server node.
//
// Auth: a "default" user password (requirepass) is always set (shown in node
// props). Optionally the bundled valkey-ldap module is wired to the Intranet
// OpenLDAP so ACL users authenticate against it — Oracle Linux only for now,
// since percona-valkey-ldap isn't published for Ubuntu yet. pmm-client is
// installed (percona-release + pmm3-client) and the node registered with an
// associated PMM server, but only when monitoring is enabled (a PMM node is
// selected).

const (
	valkeyPort = 6379
	// valkeyInstance names the systemd service instance used on every Valkey
	// node/member on RHEL (templated "valkey@<instance>.service" — see
	// valkeyServiceName; Debian's package installs a single fixed unit
	// instead, with no instance concept).
	valkeyInstance = "dbcanvas"
	// valkeyDataDir is the data directory used on every OS: a subdirectory of
	// /var/lib/valkey, which both packages already create owned by the
	// "valkey" user, and which sits inside the Debian unit's systemd sandbox
	// allow-list (ReadWritePaths=-/var/lib/valkey covers this subtree) —
	// unlike an arbitrary path such as /data, which that sandbox blocks.
	valkeyDataDir = "/var/lib/valkey/data"
)

// valkeyNodeOS defaults OS/OSVersion/Arch to Oracle Linux 9 amd64 — every lab
// design template predates these fields (Valkey used to run a pulled image,
// not a systemd base image), so an empty value means "not set", not "invalid".
func valkeyNodeOS(os, osVersion, arch string) (string, string, string) {
	if os == "" {
		os = "oraclelinux"
	}
	if osVersion == "" {
		osVersion = "9"
	}
	// Empty means "whatever this installation targets" — see archOr; hardcoding amd64
	// here would pin a Valkey node to an image that an arm64 install never built.
	arch = archOr(arch)
	return os, osVersion, arch
}

// valkeyServiceName returns the systemd unit to manage. The RHEL package
// installs a templated valkey@.service (ExecStart reads
// /etc/valkey/<instance>.conf); Debian's percona-valkey-server package
// installs one fixed valkey-server.service instead — no instance concept.
func valkeyServiceName(os string) string {
	if isDebianOS(os) {
		return "valkey-server"
	}
	return "valkey@" + valkeyInstance
}

// valkeyConfFileName is the config file valkeyServiceName's unit actually
// reads, in /etc/valkey (same directory on both OS families).
func valkeyConfFileName(os string) string {
	if isDebianOS(os) {
		return "valkey.conf"
	}
	return valkeyInstance + ".conf"
}

// valkeyLdapModulePath is where percona-valkey-ldap installs its module.
// RHEL only for now (see UseLDAP's field comment) — recorded per-OS anyway
// since Debian's other Valkey modules already use a different lib dir
// (/usr/lib/valkey/modules, not /usr/lib64) and ldap would likely follow suit
// once Percona publishes it there.
func valkeyLdapModulePath(os string) string {
	if isDebianOS(os) {
		return "/usr/lib/valkey/modules/libvalkey_ldap.so"
	}
	return "/usr/lib64/valkey/modules/libvalkey_ldap.so"
}

// valkeyPackages lists the percona-release packages to install. RHEL's
// percona-valkey already bundles valkey-cli/-server/-sentinel together, and
// percona-valkey-ldap is available there; Debian splits the server
// (percona-valkey-server) from its CLI tools (percona-valkey-tools) and
// doesn't publish percona-valkey-ldap yet, so it's left out entirely rather
// than risk apt refusing the whole install over one missing dependency.
func valkeyPackages(os string) string {
	if isDebianOS(os) {
		// Debian splits the CLI tools (valkey-cli, valkey-benchmark, ...) into their
		// own package; percona-valkey-server doesn't carry them.
		return "percona-valkey-server percona-valkey-bloom percona-valkey-json percona-valkey-search percona-valkey-tools"
	}
	// RHEL's percona-valkey already bundles valkey-cli/-server/-sentinel together —
	// there is no separate "tools" package to install.
	return "percona-valkey percona-valkey-bloom percona-valkey-json percona-valkey-search percona-valkey-ldap"
}

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

// valkeyConfFile renders valkey.conf. When ldap is set it loads the
// valkey-ldap module first (so the ldap.* directives parse) and points it at
// the Intranet OpenLDAP; when cluster is set it enables clustering.
// "supervised systemd" is required for the package's Type=notify unit to
// ever report itself active — without it the service sits in "activating"
// forever even though Valkey is actually up and serving.
func valkeyConfFile(password, domain, baseDN string, ldap, cluster bool, ldapModulePath string) string {
	var b strings.Builder
	if ldap {
		// Must load the module before its ldap.* config directives are parsed.
		b.WriteString("loadmodule " + ldapModulePath + "\n")
	}
	fmt.Fprintf(&b, "port %d\n", valkeyPort)
	b.WriteString("bind 0.0.0.0\n")
	b.WriteString("protected-mode no\n")
	b.WriteString("supervised systemd\n")
	fmt.Fprintf(&b, "requirepass %s\n", password)
	fmt.Fprintf(&b, "masterauth %s\n", password)
	fmt.Fprintf(&b, "dir %s\n", valkeyDataDir)
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

	os, osVersion, arch := valkeyNodeOS(n.OS, n.OSVersion, n.Arch)
	debian := isDebianOS(os)
	image := pxcImage(os, osVersion, arch)

	// Default-user password: node override, else .env (re-read on every deploy).
	pw := n.RootPassword
	if pw == "" {
		pw = envOr("VALKEY_PASSWORD", "valkey_password")
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
	useLdap := n.UseLDAP && !debian // percona-valkey-ldap isn't published for Ubuntu yet
	ldapServers := ""
	if useLdap {
		ldapServers = fmt.Sprintf("ldap://intranet.%s:389", domain)
	}
	cfg := valkeyConfig{
		Image: image, Role: "standalone", Hostname: host, FQDN: fqdn,
		UseLDAP: useLdap, LDAPServers: ldapServers, MonitoredBy: monitoredBy,
		UseProxy: n.UseProxy, Ports: []int{valkeyPort},
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Waiting for Intranet to be ready", 10)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 18)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		applyVMSize(&spec, n.limits())
		if n.ExportEnabled {
			spec.PublishMap = []PortMap{{ContainerPort: valkeyPort, HostPort: n.ExportHostPort}}
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		if n.ExportEnabled {
			if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", valkeyPort)); e == nil {
				if p, e2 := strconv.Atoi(hp); e2 == nil {
					cfg.ExportPort = p
				}
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		pr.phase("Waiting for systemd", 25)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, os, pr.logln)
		a.ensureDNFIPv4(ctx, id, os, pr.logln)

		if n.UseProxy {
			pr.phase("Configuring package proxy", 30)
			proxyScript := pkgProxyRHEL
			if debian {
				proxyScript = pkgProxyDebian
			}
			if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.fail("configure package proxy: %v", err)
				return
			}
			pr.logln("package egress via Intranet proxy")
		}

		pr.phase("Installing Valkey", 40)
		instScript := valkeyInstallRHEL
		if debian {
			instScript = valkeyInstallDebian
		}
		if err := a.runStep(ctx, id, instScript, []string{"PKGS=" + valkeyPackages(os), "VER=" + n.ValkeyVersion}, pr.logln); err != nil {
			pr.fail("install Valkey: %v", err)
			return
		}
		pr.logln("Valkey installed")
		a.ensureRsyslog(ctx, id, os, pr.logln)

		pr.phase("Preparing data directory", 55)
		if err := a.runStep(ctx, id, valkeyPrepDataDirScript, nil, pr.logln); err != nil {
			pr.fail("prepare data directory: %v", err)
			return
		}

		pr.phase("Writing valkey.conf", 60)
		conf := valkeyConfFile(pw, domain, baseDN, useLdap, false, valkeyLdapModulePath(os))
		if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/valkey", valkeyConfFileName(os), 0o644, []byte(conf)); err != nil {
			pr.fail("write valkey.conf: %v", err)
			return
		}

		pr.phase("Starting Valkey", 70)
		if err := a.runStep(ctx, id, valkeyStartScript, []string{"SERVICE=" + valkeyServiceName(os)}, pr.logln); err != nil {
			pr.fail("start Valkey: %v", err)
			return
		}

		// Confirm an authenticated PING as well — systemd reporting "active" only
		// means the process is up, not that it has finished loading its dataset.
		pr.phase("Waiting for Valkey", 75)
		if err := a.runStep(ctx, id, valkeyPingScript, []string{"PW=" + pw}, pr.logln); err != nil {
			pr.fail("valkey did not become ready: %v", err)
			return
		}
		if useLdap {
			pr.logln("valkey-ldap wired to ldap://intranet." + domain + ":389 (ou=People," + baseDN + ")")
		} else if n.UseLDAP {
			pr.logln("LDAP requested but skipped: percona-valkey-ldap isn't published for this OS yet")
		}

		// Install pmm-client only when this node is monitored by a PMM server.
		a.valkeySetupPMM(ctx, st, doc, id, os, host, "", pw, n.PMMNodeID, pr)

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d valkey %s: provisioned (standalone)", st.ID, n.Label)
	}()
}

// valkeyInstallRHEL/Debian install Valkey via percona-release (the "valkey-91"
// repo) + the package list from valkeyPackages. Mirrors every other product's
// install script in this codebase (pxcInstallRHEL, patroniInstallRHEL, ...):
// pin_install (from install_pin.go) pins each package to $VER when a matching
// build exists, else falls back to unpinned — so mixing the versioned server
// package with unversioned module packages in the same $PKGS list is fine.
const valkeyInstallRHEL = pinInstallRHEL + `set -e
percona-release enable valkey-91 >/dev/null 2>&1 || percona-release setup -y valkey-91 >/dev/null 2>&1
pin_install $PKGS`

const valkeyInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release enable valkey-91 >/dev/null 2>&1 || percona-release setup -y valkey-91 >/dev/null 2>&1
apt-get update -qq >/dev/null
pin_install $PKGS`

// valkeyPrepDataDirScript creates the shared data directory (see valkeyDataDir)
// with ownership matching the "valkey" system user/group the package creates
// at install time — must run after install, before the service ever starts.
const valkeyPrepDataDirScript = `set -e
mkdir -p ` + valkeyDataDir + `
chown valkey:valkey ` + valkeyDataDir + `
chmod 750 ` + valkeyDataDir

// valkeyStartScript (re)starts and enables the Valkey service, then confirms
// systemd actually reports it active — mirrors pgStartScript's shape exactly.
const valkeyStartScript = `set -e
systemctl enable "$SERVICE" >/dev/null 2>&1 || true
systemctl reset-failed "$SERVICE" 2>/dev/null || true
systemctl restart "$SERVICE"
sleep 2
systemctl is-active --quiet "$SERVICE" || { echo "valkey failed to start:"; journalctl -u "$SERVICE" --no-pager 2>/dev/null | tail -20; exit 1; }`

// valkeyPingScript waits until Valkey answers an authenticated PING.
const valkeyPingScript = `set -e
for i in $(seq 1 30); do
  valkey-cli -a "$PW" --no-auth-warning PING 2>/dev/null | grep -q PONG && exit 0
  sleep 1
done
echo "valkey not answering authenticated PING"; exit 1`

// valkeyPMMRHEL/Debian point an already-installed pmm-client at the PMM server,
// create the read-only "pmm" ACL user in Valkey (per the Percona docs), and
// register the instance with `pmm-admin add valkey`. Mirrors
// patroniPMMRHEL/Debian's config→enable-agent→register shape. The install is
// re-run here too (idempotent) so this step is self-healing for nodes
// provisioned before pmm-client install became unconditional-when-monitored.
const valkeyPMMRHEL = `set -e
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; dnf -y -q install pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove valkey "$SVC" >/dev/null 2>&1 || true
valkey-cli -a "$DEFAULT_PW" --no-auth-warning ACL SETUSER pmm on ">$PMM_USER_PW" "~*" +@read +info "+config|get" +slowlog +latency >/dev/null
pmm-admin add valkey "$SVC" 127.0.0.1:6379 --username=pmm --password="$PMM_USER_PW" $CLUSTER_ARG >/dev/null 2>&1 || \
pmm-admin add valkey "$SVC" 127.0.0.1:6379 --username=pmm --password="$PMM_USER_PW" $CLUSTER_ARG --skip-connection-check >/dev/null`

const valkeyPMMDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; apt-get update -qq >/dev/null; apt-get install -y -qq pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove valkey "$SVC" >/dev/null 2>&1 || true
valkey-cli -a "$DEFAULT_PW" --no-auth-warning ACL SETUSER pmm on ">$PMM_USER_PW" "~*" +@read +info "+config|get" +slowlog +latency >/dev/null
pmm-admin add valkey "$SVC" 127.0.0.1:6379 --username=pmm --password="$PMM_USER_PW" $CLUSTER_ARG >/dev/null 2>&1 || \
pmm-admin add valkey "$SVC" 127.0.0.1:6379 --username=pmm --password="$PMM_USER_PW" $CLUSTER_ARG --skip-connection-check >/dev/null`

// valkeySetupPMM installs pmm-client and adds the Valkey instance to
// monitoring with a dedicated read-only "pmm" user, only when this node/
// cluster has monitoring enabled (a PMM node selected) — unmonitored nodes
// skip it entirely. Shared by the standalone node and every cluster member.
// cluster is the cluster label ("" for a standalone); defaultPW is the Valkey
// default-user (requirepass) password. Best-effort — the node stays up even
// if monitoring can't be wired.
func (a *App) valkeySetupPMM(ctx context.Context, st Stack, doc designDoc, containerID, os, host, cluster, defaultPW, pmmNodeID string, pr *pxcProg) {
	if pmmNodeID == "" {
		return
	}
	pmmFQDN, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, pmmNodeID)
	if !ok {
		return
	}
	pr.phase("Installing pmm-client", 90)
	pmmScript := pxcInstallPMMClientRHEL
	if isDebianOS(os) {
		pmmScript = pxcInstallPMMClientDebian
	}
	if err := a.runStep(ctx, containerID, pmmScript, nil, pr.logln); err != nil {
		pr.logln("pmm-client install skipped: " + err.Error())
		return
	}
	pr.phase("Joining PMM", 94)
	clusterArg := ""
	if cluster != "" {
		clusterArg = "--cluster=" + cluster
	}
	registerScript := valkeyPMMRHEL
	if isDebianOS(os) {
		registerScript = valkeyPMMDebian
	}
	env := []string{
		"PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass),
		"DEFAULT_PW=" + defaultPW,
		"PMM_USER_PW=" + envOr("PMM_PASSWORD", "pmm_password"),
		"SVC=" + host, "CLUSTER_ARG=" + clusterArg,
	}
	if err := a.runStep(ctx, containerID, registerScript, env, pr.logln); err != nil {
		pr.logln("PMM registration skipped: " + err.Error())
	} else {
		pr.logln("added to PMM monitoring (valkey, user pmm) on " + pmmFQDN)
	}
}

// ------------------------------------------------------------ Valkey cluster

// provisionValkeyClusterFrame brings up a Valkey Cluster: every member installs
// Valkey via percona-release with cluster-enabled, then one member runs
// `valkey-cli --cluster create` over all members (all-master, 3–7 shards).
// Shared default-user password + optional LDAP across the cluster.
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

	os, osVersion, arch := valkeyNodeOS(frame.OS, frame.OSVersion, frame.Arch)
	debian := isDebianOS(os)
	image := pxcImage(os, osVersion, arch)

	// Shared default-user password: frame override, else .env (re-read on every deploy).
	pw := frame.RootPassword
	if pw == "" {
		pw = envOr("VALKEY_PASSWORD", "valkey_password")
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
	useLdap := frame.UseLDAP && !debian // percona-valkey-ldap isn't published for Ubuntu yet
	ldapServers := ""
	if useLdap {
		ldapServers = fmt.Sprintf("ldap://intranet.%s:389", domain)
	}
	for _, n := range members {
		host := hosts[n.ID]
		cfg := valkeyConfig{
			Image: image, Role: "cluster", Hostname: host, FQDN: fqdnOf(host, domain),
			UseLDAP: useLdap, LDAPServers: ldapServers, MonitoredBy: monitoredBy,
			UseProxy: frame.UseProxy, Ports: []int{valkeyPort},
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range members {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			for _, n := range members {
				progs[n.ID].fail("%v", werr)
			}
			return
		}

		// Phase 1 (parallel): create + install + configure + start every member.
		var wg sync.WaitGroup
		failed := false
		var mu sync.Mutex
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.valkeyStartMember(ctx, st, n, hosts[n.ID], intranetIP, domain, baseDN, image, os, debian, pw, useLdap, frame.ValkeyVersion, progs[n.ID]); err != nil {
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
		progs[first.ID].phase("Forming cluster", 80)
		if err := a.runStep(ctx, fdep.ContainerID, valkeyClusterCreateScript, []string{"PW=" + pw, "NODES=" + strings.Join(nodeArgs, " ")}, progs[first.ID].logln); err != nil {
			progs[first.ID].fail("form cluster: %v", err)
			return
		}

		// Phase 3: pmm-client per member.
		for _, n := range members {
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			pr := progs[n.ID]
			a.valkeySetupPMM(ctx, st, doc, dep.ContainerID, os, hosts[n.ID], frame.Label, pw, frame.PMMNodeID, pr)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: dep.ContainerID, State: DeployRunning, Config: a.depConfig(st.ID, n.ID), Secrets: secJSON})
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d valkeycluster %s: provisioned (%d shards)", st.ID, frame.Label, len(members))
	}()
}

// valkeyStartMember creates + installs + configures + starts one cluster
// member and waits for PING.
func (a *App) valkeyStartMember(ctx context.Context, st Stack, n designNode, host, intranetIP, domain, baseDN, image, os string, debian bool, pw string, useLdap bool, valkeyVersion string, pr *pxcProg) error {
	pr.phase("Creating container", 15)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
		a.engCtx(ctx).ContainerRemove(ctx, cid)
	}
	spec := ContainerSpec{
		Name: name, Image: image, Hostname: host, Privileged: true,
		Network: networkName(st.ID), Aliases: []string{host},
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
	}
	applyVMSize(&spec, n.limits())
	if n.ExportEnabled {
		spec.PublishMap = []PortMap{{ContainerPort: valkeyPort, HostPort: n.ExportHostPort}}
	}
	id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
	if n.ExportEnabled {
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", valkeyPort)); e == nil {
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

	pr.phase("Waiting for systemd", 25)
	if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return pr.fail("systemd did not start: %v", err)
	}
	a.trustIntranetCA(ctx, st, id, os, pr.logln)
	a.ensureDNFIPv4(ctx, id, os, pr.logln)

	if n.UseProxy {
		pr.phase("Configuring package proxy", 30)
		proxyScript := pkgProxyRHEL
		if debian {
			proxyScript = pkgProxyDebian
		}
		if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
			return pr.fail("configure package proxy: %v", err)
		}
	}

	pr.phase("Installing Valkey", 45)
	instScript := valkeyInstallRHEL
	if debian {
		instScript = valkeyInstallDebian
	}
	if err := a.runStep(ctx, id, instScript, []string{"PKGS=" + valkeyPackages(os), "VER=" + valkeyVersion}, pr.logln); err != nil {
		return pr.fail("install Valkey: %v", err)
	}
	a.ensureRsyslog(ctx, id, os, pr.logln)

	pr.phase("Preparing data directory", 55)
	if err := a.runStep(ctx, id, valkeyPrepDataDirScript, nil, pr.logln); err != nil {
		return pr.fail("prepare data directory: %v", err)
	}

	pr.phase("Writing valkey.conf", 60)
	conf := valkeyConfFile(pw, domain, baseDN, useLdap, true, valkeyLdapModulePath(os))
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/valkey", valkeyConfFileName(os), 0o644, []byte(conf)); err != nil {
		return pr.fail("write valkey.conf: %v", err)
	}

	pr.phase("Starting Valkey", 68)
	if err := a.runStep(ctx, id, valkeyStartScript, []string{"SERVICE=" + valkeyServiceName(os)}, pr.logln); err != nil {
		return pr.fail("start Valkey: %v", err)
	}

	pr.phase("Waiting for Valkey", 72)
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
