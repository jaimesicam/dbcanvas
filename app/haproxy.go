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

// HAProxy node. HAProxy is a TCP/HTTP load balancer placed in front of a Patroni
// PostgreSQL cluster. It runs on a systemd OS image (built by `make images`), is
// wired to a Patroni cluster frame via a canvas association line, and is
// configured to route application traffic using Patroni's REST health checks:
// writes go to the current leader (:5000 → the member whose :8008 /primary returns
// 200) and reads are load-balanced across replicas (:5001 → /replica). A stats
// page is served on :7000. The three ports can be published to the host.

// haproxy front-end ports: writes → leader, reads → replicas, stats UI.
const (
	haproxyWritePort = 5000
	haproxyReadPort  = 5001
	haproxyStatsPort = 7000
)

var haproxyPorts = []int{haproxyWritePort, haproxyReadPort, haproxyStatsPort}

// haproxyConfig is the non-secret profile shown for a deployed HAProxy node.
type haproxyConfig struct {
	Image       string   `json:"image"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Hostname    string   `json:"hostname"`
	FQDN        string   `json:"fqdn"`
	Cluster     string   `json:"cluster"`     // associated Patroni cluster name
	Members     []string `json:"members"`     // Patroni member FQDNs in the backend
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

		// Resolve the linked Patroni cluster and wait for it to be running so its
		// members exist as backends before we write the config.
		frame, ok := patroniFrameForHAProxy(doc, n.ID)
		if !ok {
			failNode("no Patroni cluster is associated — link one to this HAProxy")
			return
		}
		setPhase("Waiting for Patroni cluster", 15)
		members, _, cerr := a.waitPatroniRunning(ctx, st.ID, frame, doc, domain, deployTimeout())
		if cerr != nil {
			failNode("%v", cerr)
			return
		}
		cfg.Cluster, cfg.Members = frame.Label, members
		logln("Patroni cluster " + frame.Label + " is running (" + strconv.Itoa(len(members)) + " members)")

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

		setPhase("Configuring HAProxy", 78)
		cfgFile := haproxyCfg(frame.Label, members)
		if err := a.docker.CopyFile(ctx, id, "/etc/haproxy", "haproxy.cfg", 0o644, []byte(cfgFile)); err != nil {
			failNode("write haproxy.cfg: %v", err)
			return
		}
		if err := a.runStep(ctx, id, haproxyStartScript, nil, logln); err != nil {
			failNode("start haproxy: %v", err)
			return
		}
		logln("haproxy started (write :5000 → leader, read :5001 → replicas, stats :7000)")

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

// ------------------------------------------------------------------ scripts

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
