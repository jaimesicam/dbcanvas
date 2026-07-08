package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// HAProxy node. HAProxy is a TCP/HTTP load balancer placed in front of ONE backend
// database cluster (mutually exclusive) — either a Patroni PostgreSQL cluster or a
// Percona XtraDB Cluster (PXC). It runs on a systemd OS image (built by `make images`)
// and is wired to the cluster frame via a canvas association line. The routing differs
// by backend:
//
//	Patroni — writes go to the current leader (:5000 → the member whose Patroni REST
//	          :8008 /primary returns 200); reads round-robin the replicas (:5001 →
//	          /replica).
//	PXC     — writes go to a single active node, the rest kept as backups (:5000), to
//	          avoid multi-master write conflicts; reads round-robin all synced nodes
//	          (:5001). Health is the PXC "clustercheck" HTTP endpoint on each node's
//	          :9200, which reports 200 only while the node is wsrep-Synced. See
//	          https://docs.percona.com/percona-xtradb-cluster/8.0/haproxy.html and
//	          https://docs.percona.com/percona-xtradb-cluster/8.0/haproxy-config.html
//
// A stats page is served on :7000. The three ports can be published to the host.

// haproxy front-end ports: writes → primary/writer, reads → replicas, stats UI.
const (
	haproxyWritePort = 5000
	haproxyReadPort  = 5001
	haproxyStatsPort = 7000
	// PXC backend: MySQL client port and the clustercheck (mysqlchk) health port.
	pxcMySQLPort        = 3306
	pxcClusterCheckPort = 9200
)

var haproxyPorts = []int{haproxyWritePort, haproxyReadPort, haproxyStatsPort}

// haproxyConfig is the non-secret profile shown for a deployed HAProxy node.
type haproxyConfig struct {
	Image       string   `json:"image"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Hostname    string   `json:"hostname"`
	FQDN        string   `json:"fqdn"`
	Backend     string   `json:"backend"`     // "patroni" | "pxc" — the backend cluster kind
	Cluster     string   `json:"cluster"`     // associated cluster name
	Members     []string `json:"members"`     // backend member FQDNs
	UseProxy    bool     `json:"useProxy"`    // route package egress via the Intranet Squid proxy
	MonitoredBy string   `json:"monitoredBy"` // PMM node FQDN, if any
	Ports       []int    `json:"ports"`
	WritePort   int      `json:"writePort"` // host port mapped to 5000 (0 = not published)
	ReadPort    int      `json:"readPort"`  // host port mapped to 5001
	StatsPort   int      `json:"statsPort"` // host port mapped to 7000
}

func haproxyAlias(label string) string {
	a := sanitizeName(strings.TrimSpace(label))
	if a == "" {
		a = "haproxy"
	}
	return a
}

// provisionHAProxy records + provisions an HAProxy node fronting the Patroni
// cluster it is linked to on the canvas.
func (a *App) provisionHAProxy(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = haproxyAlias(n.Label)
	}
	image := pxcImage(n.OS, n.OSVersion, n.Arch)

	monitoredBy := ""
	if n.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == n.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(stackHostnames(doc)[m.ID], domain)
			}
		}
	}

	cfg := haproxyConfig{
		Image: image, OS: n.OS, Arch: archOr(n.Arch),
		Hostname: host, FQDN: fqdnOf(host, domain),
		UseProxy: n.UseProxy, MonitoredBy: monitoredBy, Ports: haproxyPorts,
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON})

	go func() {
		ctx := context.Background()
		prog := &provProgress{Phase: "Starting", Log: []string{}}
		save := func() { b, _ := json.Marshal(prog); a.store.SetDeploymentProgress(st.ID, n.ID, b) }
		logln := func(s string) {
			prog.Log = append(prog.Log, s)
			if len(prog.Log) > 200 {
				prog.Log = prog.Log[len(prog.Log)-200:]
			}
			save()
		}
		setPhase := func(ph string, pct int) { prog.Phase = ph; prog.Percent = pct; save() }
		failNode := func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			log.Printf("stack %d haproxy %s: %s", st.ID, n.ID, msg)
			prog.Phase = "failed"
			prog.Message = msg
			save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployError)
		}

		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		setPhase("Waiting for Intranet to be ready", 5)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			failNode("%v", werr)
			return
		}

		// Resolve the single associated backend cluster (Patroni or PXC — mutually
		// exclusive) and wait for it to be running so its members exist before we write
		// the config. PXC members carry their container ids so clustercheck can be set up.
		frame, kind, ok := haproxyBackend(doc, n.ID)
		if !ok {
			failNode("HAProxy must be linked to exactly one Patroni or PXC cluster")
			return
		}
		cfg.Backend = kind
		var members []string
		var pxcMembers []pxcMember
		var pxcSec pxcSecrets
		switch kind {
		case "patroni":
			setPhase("Waiting for Patroni cluster", 15)
			m, _, cerr := a.waitPatroniRunning(ctx, st.ID, frame, doc, domain, deployTimeout())
			if cerr != nil {
				failNode("%v", cerr)
				return
			}
			members = m
		case "pxc":
			setPhase("Waiting for PXC cluster", 15)
			ms, sc, cerr := a.waitPXCMembers(ctx, st.ID, frame, doc, domain, deployTimeout())
			if cerr != nil {
				failNode("%v", cerr)
				return
			}
			pxcMembers, pxcSec = ms, sc
			for _, m := range ms {
				members = append(members, m.FQDN)
			}
		default:
			failNode("unsupported backend cluster type %q", kind)
			return
		}
		cfg.Cluster, cfg.Members = frame.Label, members
		logln(kind + " cluster " + frame.Label + " is running (" + strconv.Itoa(len(members)) + " members)")

		setPhase("Creating container", 25)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		if n.ExportEnabled {
			spec.PublishMap = []PortMap{
				{ContainerPort: haproxyWritePort, HostPort: n.ExportHostPort},
				{ContainerPort: haproxyReadPort, HostPort: 0},
				{ContainerPort: haproxyStatsPort, HostPort: 0},
			}
		}
		id, err := a.docker.ContainerCreate(ctx, spec)
		if err != nil {
			failNode("create container: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			failNode("start container: %v", err)
			return
		}
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		cfg.WritePort, cfg.ReadPort, cfg.StatsPort = a.readHAProxyPorts(ctx, id, n.ExportEnabled)
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON})

		setPhase("Waiting for systemd", 35)
		if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
			failNode("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, n.OS, logln)
		a.ensureDNFIPv4(ctx, id, n.OS, logln)

		debian := isDebianOS(n.OS)
		if n.UseProxy {
			proxyScript := pkgProxyRHEL
			if debian {
				proxyScript = pkgProxyDebian
			}
			if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, logln); err != nil {
				failNode("configure package proxy: %v", err)
				return
			}
			logln("package egress via Intranet proxy")
		}

		setPhase("Installing HAProxy", 50)
		instScript := haproxyInstallRHEL
		if debian {
			instScript = haproxyInstallDebian
		}
		if err := a.runStep(ctx, id, instScript, nil, logln); err != nil {
			failNode("install haproxy: %v", err)
			return
		}
		logln("haproxy installed")
		a.ensureRsyslog(ctx, id, n.OS, logln)

		// Install pmm-client only when this node is monitored by a PMM server.
		if n.PMMNodeID != "" {
			setPhase("Installing PMM client", 62)
			pmmScript := pxcInstallPMMClientRHEL
			if debian {
				pmmScript = pxcInstallPMMClientDebian
			}
			if err := a.runStep(ctx, id, pmmScript, nil, logln); err != nil {
				logln("pmm-client install skipped: " + err.Error())
			} else {
				logln("pmm-client installed")
			}
		}

		// PXC backend: set up the clustercheck (mysqlchk) HTTP health endpoint on :9200
		// of every data member, which HAProxy polls to route only to Synced nodes.
		var cfgFile string
		switch kind {
		case "pxc":
			setPhase("Configuring PXC clustercheck", 70)
			if err := a.pxcSetupClustercheck(ctx, pxcMembers, pxcSec, logln); err != nil {
				failNode("%v", err)
				return
			}
			cfgFile = haproxyPXCCfg(frame.Label, members)
		default: // patroni
			cfgFile = haproxyCfg(frame.Label, members)
		}

		setPhase("Configuring HAProxy", 78)
		if err := a.docker.CopyFile(ctx, id, "/etc/haproxy", "haproxy.cfg", 0o644, []byte(cfgFile)); err != nil {
			failNode("write haproxy.cfg: %v", err)
			return
		}
		if err := a.runStep(ctx, id, haproxyStartScript, nil, logln); err != nil {
			failNode("start haproxy: %v", err)
			return
		}
		if kind == "pxc" {
			logln("haproxy started (write :5000 → single writer, read :5001 → round-robin synced nodes, stats :7000)")
		} else {
			logln("haproxy started (write :5000 → leader, read :5001 → replicas, stats :7000)")
		}

		if n.PMMNodeID != "" {
			setPhase("Registering with PMM", 92)
			a.haproxyRegisterPMM(ctx, st, n, doc, logln) // best-effort
		}

		a.reconcileStackDNS(ctx, st.ID)
		setPhase("Running", 100)
		prog.Message = "provisioned"
		save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		log.Printf("stack %d haproxy %s: provisioned (cluster %s)", st.ID, n.ID, frame.Label)
	}()
}

// readHAProxyPorts reads the published host ports for the three front-ends (0 when
// export is off / unmapped).
func (a *App) readHAProxyPorts(ctx context.Context, id string, exported bool) (int, int, int) {
	if !exported {
		return 0, 0, 0
	}
	read := func(p int) int {
		if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", p)); e == nil {
			if v, e2 := strconv.Atoi(hp); e2 == nil {
				return v
			}
		}
		return 0
	}
	return read(haproxyWritePort), read(haproxyReadPort), read(haproxyStatsPort)
}

// haproxyRegisterPMM registers the HAProxy service with the PMM server
// (best-effort) using the node's linked PMM node.
func (a *App) haproxyRegisterPMM(ctx context.Context, st Stack, n designNode, doc designDoc, logln func(string)) {
	pmmFQDN, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, n.PMMNodeID)
	if !ok {
		logln("PMM registration skipped: PMM node not running")
		return
	}
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	script := haproxyPMMRHEL
	if isDebianOS(n.OS) {
		script = haproxyPMMDebian
	}
	env := []string{
		"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass, "PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass),
		"NODE=" + n.Label,
	}
	if _, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", script}, env); err != nil {
		logln("PMM registration skipped: " + err.Error())
	} else {
		logln("registered with PMM at " + pmmFQDN)
	}
}

// --------------------------------------------------------------- PXC backend

// pxcMember is a running PXC data member: its FQDN (backend address) and container id
// (where clustercheck is set up).
type pxcMember struct {
	FQDN        string
	ContainerID string
}

// waitPXCMembers blocks until every regular (data) member of a PXC frame is running,
// then returns each member's FQDN + container id and the shared cluster secrets.
func (a *App) waitPXCMembers(ctx context.Context, stackID int64, frame designFrame, doc designDoc, domain string, timeout time.Duration) ([]pxcMember, pxcSecrets, error) {
	hosts := stackHostnames(doc)
	var regulars []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "pxc" && n.Role != "arbitrator" {
			regulars = append(regulars, n)
		}
	}
	if len(regulars) == 0 {
		return nil, pxcSecrets{}, fmt.Errorf("PXC cluster %s has no regular (data) node", frame.Label)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allRunning := true
		var sec pxcSecrets
		var ms []pxcMember
		for _, n := range regulars {
			dep, err := a.store.GetDeployment(stackID, n.ID)
			if err != nil {
				allRunning = false
				break
			}
			if dep.State == DeployError {
				return nil, pxcSecrets{}, fmt.Errorf("PXC cluster %s failed to provision", frame.Label)
			}
			if dep.State != DeployRunning || dep.ContainerID == "" {
				allRunning = false
				break
			}
			json.Unmarshal(dep.Secrets, &sec)
			ms = append(ms, pxcMember{FQDN: fqdnOf(hosts[n.ID], domain), ContainerID: dep.ContainerID})
		}
		if allRunning {
			return ms, sec, nil
		}
		time.Sleep(3 * time.Second)
	}
	return nil, pxcSecrets{}, fmt.Errorf("PXC cluster %s did not become ready within %s", frame.Label, timeout)
}

// pxcSetupClustercheck installs the PXC "clustercheck" HTTP health endpoint on every
// data member so HAProxy can route only to Synced nodes (per the Percona docs). The
// `clustercheck`@'localhost' MySQL user (PROCESS priv) it authenticates as is created in
// every node's baseline from CLUSTERCHECK_PASSWORD — so it already exists cluster-wide
// without a post-baseline write that could become an errant cross-cluster transaction —
// so here we only lay down, on each member, a check script + a systemd socket-activated
// service on :9200 (mysqlchk) that returns HTTP 200 while wsrep_local_state is Synced (4),
// else 503.
func (a *App) pxcSetupClustercheck(ctx context.Context, members []pxcMember, sec pxcSecrets, logln func(string)) error {
	if len(members) == 0 {
		return fmt.Errorf("no PXC members to configure clustercheck on")
	}
	for _, m := range members {
		if err := a.runStep(ctx, m.ContainerID, pxcClusterCheckServiceScript, []string{"CC_PW=" + sec.ClusterCheckPassword}, logln); err != nil {
			return fmt.Errorf("configure clustercheck on %s: %w", m.FQDN, err)
		}
	}
	logln(fmt.Sprintf("clustercheck (mysqlchk) listening on :%d of %d member(s)", pxcClusterCheckPort, len(members)))
	return nil
}

// --------------------------------------------------------------- config file

// haproxyCfg renders /etc/haproxy/haproxy.cfg: a write front-end that routes to
// whichever member is the Patroni leader (httpchk GET /primary), a read front-end
// load-balancing the replicas (GET /replica), and a stats page. Backend health is
// checked against each member's Patroni REST API (:8008).
func haproxyCfg(cluster string, members []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "global\n")
	fmt.Fprintf(&b, "    maxconn 1000\n")
	fmt.Fprintf(&b, "    log 127.0.0.1 local0\n\n")
	fmt.Fprintf(&b, "defaults\n")
	fmt.Fprintf(&b, "    log global\n")
	fmt.Fprintf(&b, "    mode tcp\n")
	fmt.Fprintf(&b, "    retries 2\n")
	fmt.Fprintf(&b, "    timeout client 30m\n")
	fmt.Fprintf(&b, "    timeout connect 4s\n")
	fmt.Fprintf(&b, "    timeout server 30m\n")
	fmt.Fprintf(&b, "    timeout check 5s\n\n")

	fmt.Fprintf(&b, "listen stats\n")
	fmt.Fprintf(&b, "    mode http\n")
	fmt.Fprintf(&b, "    bind *:%d\n", haproxyStatsPort)
	// Expose HAProxy's native Prometheus metrics at /metrics on the stats port so PMM
	// (pmm-admin add haproxy --listen-port=<stats port>) can scrape them; the HTML stats
	// page stays served at every other path.
	fmt.Fprintf(&b, "    http-request use-service prometheus-exporter if { path /metrics }\n")
	fmt.Fprintf(&b, "    stats enable\n")
	fmt.Fprintf(&b, "    stats uri /\n")
	fmt.Fprintf(&b, "    stats refresh 5s\n\n")

	// shortHost derives a stable server label from each member FQDN.
	shortHost := func(fqdn string) string {
		if i := strings.Index(fqdn, "."); i > 0 {
			return fqdn[:i]
		}
		return fqdn
	}

	fmt.Fprintf(&b, "listen %s_write\n", sanitizeName(cluster))
	fmt.Fprintf(&b, "    bind *:%d\n", haproxyWritePort)
	fmt.Fprintf(&b, "    option httpchk GET /primary\n")
	fmt.Fprintf(&b, "    http-check expect status 200\n")
	fmt.Fprintf(&b, "    default-server inter 3s fall 3 rise 2 on-marked-down shutdown-sessions init-addr last,libc,none\n")
	for _, m := range members {
		fmt.Fprintf(&b, "    server %s %s:%d maxconn 1000 check port %d\n", shortHost(m), m, patroniPGPort, patroniRESTPort)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "listen %s_read\n", sanitizeName(cluster))
	fmt.Fprintf(&b, "    bind *:%d\n", haproxyReadPort)
	fmt.Fprintf(&b, "    balance roundrobin\n")
	fmt.Fprintf(&b, "    option httpchk GET /replica\n")
	fmt.Fprintf(&b, "    http-check expect status 200\n")
	fmt.Fprintf(&b, "    default-server inter 3s fall 3 rise 2 on-marked-down shutdown-sessions init-addr last,libc,none\n")
	for _, m := range members {
		fmt.Fprintf(&b, "    server %s %s:%d maxconn 1000 check port %d\n", shortHost(m), m, patroniPGPort, patroniRESTPort)
	}
	return b.String()
}

// haproxyPXCCfg renders /etc/haproxy/haproxy.cfg for a Percona XtraDB Cluster backend
// (see the Percona haproxy-config docs). A write front-end (:5000) sends traffic to a
// single active node — the rest are `backup`, promoted only if it fails — to avoid
// multi-master write conflicts; a read front-end (:5001) round-robins all nodes. Both
// health-check each node's clustercheck endpoint (:9200) via option httpchk, so only
// wsrep-Synced nodes receive traffic. DB traffic itself is proxied in TCP mode to :3306.
func haproxyPXCCfg(cluster string, members []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "global\n")
	fmt.Fprintf(&b, "    maxconn 1000\n")
	fmt.Fprintf(&b, "    log 127.0.0.1 local0\n\n")
	fmt.Fprintf(&b, "defaults\n")
	fmt.Fprintf(&b, "    log global\n")
	fmt.Fprintf(&b, "    mode tcp\n")
	fmt.Fprintf(&b, "    retries 3\n")
	fmt.Fprintf(&b, "    timeout client 30m\n")
	fmt.Fprintf(&b, "    timeout connect 4s\n")
	fmt.Fprintf(&b, "    timeout server 30m\n")
	fmt.Fprintf(&b, "    timeout check 5s\n\n")

	fmt.Fprintf(&b, "listen stats\n")
	fmt.Fprintf(&b, "    mode http\n")
	fmt.Fprintf(&b, "    bind *:%d\n", haproxyStatsPort)
	// Expose HAProxy's native Prometheus metrics at /metrics on the stats port so PMM
	// (pmm-admin add haproxy --listen-port=<stats port>) can scrape them; the HTML stats
	// page stays served at every other path.
	fmt.Fprintf(&b, "    http-request use-service prometheus-exporter if { path /metrics }\n")
	fmt.Fprintf(&b, "    stats enable\n")
	fmt.Fprintf(&b, "    stats uri /\n")
	fmt.Fprintf(&b, "    stats refresh 5s\n\n")

	shortHost := func(fqdn string) string {
		if i := strings.Index(fqdn, "."); i > 0 {
			return fqdn[:i]
		}
		return fqdn
	}

	// Writer: a single active node, the rest kept as backups (single-writer).
	fmt.Fprintf(&b, "listen %s_write\n", sanitizeName(cluster))
	fmt.Fprintf(&b, "    bind *:%d\n", haproxyWritePort)
	fmt.Fprintf(&b, "    option httpchk\n")
	fmt.Fprintf(&b, "    http-check expect status 200\n")
	fmt.Fprintf(&b, "    default-server inter 3s fall 3 rise 2 on-marked-down shutdown-sessions init-addr last,libc,none\n")
	for i, m := range members {
		backup := ""
		if i > 0 {
			backup = " backup"
		}
		fmt.Fprintf(&b, "    server %s %s:%d maxconn 1000 check port %d%s\n", shortHost(m), m, pxcMySQLPort, pxcClusterCheckPort, backup)
	}
	b.WriteString("\n")

	// Reader: round-robin across all Synced nodes.
	fmt.Fprintf(&b, "listen %s_read\n", sanitizeName(cluster))
	fmt.Fprintf(&b, "    bind *:%d\n", haproxyReadPort)
	fmt.Fprintf(&b, "    balance roundrobin\n")
	fmt.Fprintf(&b, "    option httpchk\n")
	fmt.Fprintf(&b, "    http-check expect status 200\n")
	fmt.Fprintf(&b, "    default-server inter 3s fall 3 rise 2 on-marked-down shutdown-sessions init-addr last,libc,none\n")
	for _, m := range members {
		fmt.Fprintf(&b, "    server %s %s:%d maxconn 1000 check port %d\n", shortHost(m), m, pxcMySQLPort, pxcClusterCheckPort)
	}
	return b.String()
}

// ------------------------------------------------------------------ scripts

// pxcClusterCheckServiceScript installs the mysqlchk health endpoint on a PXC member: a
// check script that reports the node's wsrep sync state as an HTTP response, wired to a
// systemd socket on :9200 (Accept=yes → one instance per connection with stdio bound to
// the socket, exactly like the classic xinetd mysqlchk from the Percona docs). HAProxy's
// httpchk polls it; the node answers 200 only while Synced.
const pxcClusterCheckServiceScript = `set -e
cat >/etc/dbcanvas-clustercheck.cnf <<CNF
[client]
user=clustercheck
password=$CC_PW
socket=/var/lib/mysql/mysql.sock
CNF
chmod 600 /etc/dbcanvas-clustercheck.cnf
cat >/usr/local/bin/dbcanvas-clustercheck <<'SCRIPT'
#!/bin/bash
# PXC clustercheck for HAProxy: HTTP 200 when this node is wsrep-Synced (4), else 503.
# Drain the incoming HTTP request first so no unread data remains when we exit —
# otherwise the kernel RSTs the socket and HAProxy's httpchk reports it as a failed
# "Connection reset by peer" check.
while IFS= read -r -t 1 line; do line=${line%$'\r'}; [ -z "$line" ] && break; done
STATE=$(mysql --defaults-extra-file=/etc/dbcanvas-clustercheck.cnf -N -e "SHOW STATUS LIKE 'wsrep_local_state'" 2>/dev/null | awk '{print $2}')
if [ "$STATE" = "4" ]; then
  BODY="Percona XtraDB Cluster Node is synced."
  printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s' "${#BODY}" "$BODY"
else
  BODY="Percona XtraDB Cluster Node is not synced."
  printf 'HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s' "${#BODY}" "$BODY"
fi
SCRIPT
chmod +x /usr/local/bin/dbcanvas-clustercheck
cat >/etc/systemd/system/mysqlchk.socket <<'UNIT'
[Unit]
Description=PXC clustercheck (mysqlchk) socket for HAProxy
[Socket]
ListenStream=9200
Accept=yes
[Install]
WantedBy=sockets.target
UNIT
cat >/etc/systemd/system/mysqlchk@.service <<'UNIT'
[Unit]
Description=PXC clustercheck (mysqlchk) per-connection responder
[Service]
ExecStart=/usr/local/bin/dbcanvas-clustercheck
StandardInput=socket
StandardOutput=socket
StandardError=journal
UNIT
systemctl daemon-reload
systemctl reset-failed mysqlchk.socket 2>/dev/null || true
systemctl enable --now mysqlchk.socket`

const haproxyInstallRHEL = `set -e
dnf -y -q install haproxy >/dev/null`

const haproxyInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq haproxy >/dev/null`

// haproxyStartScript validates the config, enables, and (re)starts haproxy. SELinux
// on EL may block binding non-standard ports; the systemd images run permissive,
// so no extra policy is needed.
const haproxyStartScript = `set -e
haproxy -c -f /etc/haproxy/haproxy.cfg >/dev/null
systemctl enable haproxy >/dev/null 2>&1 || true
systemctl reset-failed haproxy 2>/dev/null || true
systemctl restart haproxy
sleep 1
systemctl is-active --quiet haproxy || { echo "haproxy failed to start:"; journalctl -u haproxy --no-pager 2>/dev/null | tail -15; exit 1; }`

// haproxyPMM{RHEL,Debian} register the HAProxy service with the PMM server
// (best-effort). pmm-admin's haproxy integration scrapes HAProxy's own stats.
const haproxyPMMRHEL = `set -e
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; dnf -y -q install pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove haproxy "$NODE" >/dev/null 2>&1 || true
pmm-admin add haproxy --listen-port=7000 "$NODE"`

const haproxyPMMDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; apt-get update -qq >/dev/null; apt-get install -y -qq pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove haproxy "$NODE" >/dev/null 2>&1 || true
pmm-admin add haproxy --listen-port=7000 "$NODE"`
