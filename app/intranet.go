package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// design parsing (the canvas document stored in stacks.design_json)
type designNode struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Label     string `json:"label"`
	OS        string `json:"os"`
	OSVersion string `json:"osVersion"` // OS release (e.g. "9", "24.04") — used by ProxySQL
	Arch      string `json:"arch"`
	// PMM node fields (ignored by other node types).
	Version       string `json:"version"`       // PMM minor version tag ("" → catalog default)
	AdminPassword string `json:"adminPassword"` // PMM admin password ("" → auto-generated)
	GenerateCert  bool   `json:"generateCert"`  // sign nginx certs from the Intranet CA on deploy
	// PXC node fields — a PXC node belongs to a PXC frame (FrameID) and is either
	// a data member ("regular") or a voting-only "arbitrator" (garbd).
	FrameID        string `json:"frameId"`
	Role           string `json:"role"`           // "regular" | "arbitrator"
	ExportEnabled  bool   `json:"exportEnabled"`  // publish the DB port to the host
	ExportHostPort int    `json:"exportHostPort"` // desired host port (0 = random/unused)
	// ProxySQL node fields (ignored by other node types). os/osVersion/arch are the
	// shared image fields above; these add the ProxySQL series + behaviour.
	ProxySQLMajor   string `json:"proxysqlMajor"`   // "2" | "3"
	ProxySQLVersion string `json:"proxysqlVersion"` // minor (e.g. 2.7.1-1); "" → latest
	Mode            string `json:"mode"`            // "singlewrite" (default) | "loadbal"
	PMMNodeID       string `json:"pmmNodeId"`       // PMM node monitoring this ProxySQL (optional)
	UseProxy        bool   `json:"useProxy"`        // route package egress via the Intranet Squid proxy
	// Standalone Percona Server node fields (Type=="ps"; ignored by other types).
	PSMajor      string `json:"psMajor"`      // Percona Server "8.0" | "8.4"
	PSVersion    string `json:"psVersion"`    // minor; "" → latest
	GTID         bool   `json:"gtid"`         // enable GTID
	RootPassword string `json:"rootPassword"` // "" → auto-generated
	CertTTLValue int    `json:"certTtlValue"`
	CertTTLUnit  string `json:"certTtlUnit"`
}

// designEdge is a connection drawn on the canvas. The endpoints' Node field holds
// the id of a node OR a frame (e.g. a ProxySQL node linked to a PXC cluster frame).
type designEdge struct {
	ID   string  `json:"id"`
	From edgeEnd `json:"from"`
	To   edgeEnd `json:"to"`
	// "directional" — a data-flow link (e.g. backend cluster → ProxySQL).
	// "async"/"bidir" — a cross-cluster replication link between two cluster members
	// (From is the source, To the replica; "bidir" replicates both ways). See
	// replication.go.
	Type string `json:"type"`
}

type edgeEnd struct {
	Node string `json:"node"`
	Port string `json:"port"`
}

// designFrame is a group container on the canvas: a PXC cluster frame (holds PXC
// nodes) or a ProxySQL cluster frame (holds ProxySQL nodes), carrying the
// cluster-wide configuration for its members.
type designFrame struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"` // "pxc" | "proxysql"
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	// PXC cluster config.
	OS           string `json:"os"`           // os family: "oraclelinux" | "ubuntu"
	OSVersion    string `json:"osVersion"`    // e.g. "9" | "24.04"
	Arch         string `json:"arch"`         // "amd64" | "arm64"
	PXCMajor     string `json:"pxcMajor"`     // "8.0" | "8.4"
	PXCVersion   string `json:"pxcVersion"`   // minor (e.g. 8.0.45-36.1); "" → latest
	RootPassword string `json:"rootPassword"` // "" → auto-generated
	PMMNodeID    string `json:"pmmNodeId"`    // PMM node that monitors this cluster (optional)
	UseProxy     bool   `json:"useProxy"`     // route egress via the Intranet Squid proxy
	GTID         bool   `json:"gtid"`         // enable GTID (default on)
	GenerateCert bool   `json:"generateCert"` // per-node certs signed by the Intranet CA
	CertTTLValue int    `json:"certTtlValue"`
	CertTTLUnit  string `json:"certTtlUnit"`
	// ProxySQL cluster frame config (Type=="proxysql"; reuses OS/OSVersion/Arch,
	// PMMNodeID, UseProxy above).
	ProxySQLMajor   string `json:"proxysqlMajor"`   // "2" | "3"
	ProxySQLVersion string `json:"proxysqlVersion"` // minor; "" → latest
	Mode            string `json:"mode"`            // "singlewrite" | "loadbal"
	// MySQL replication frame config (Type=="mysql"; reuses OS/OSVersion/Arch,
	// RootPassword, PMMNodeID, UseProxy, GTID, GenerateCert/CertTTL above).
	PSMajor   string `json:"psMajor"`   // Percona Server "8.0" | "8.4"
	PSVersion string `json:"psVersion"` // minor; "" → latest
	ReplMode  string `json:"replMode"`  // mysql: "async"|"semisync" · innodb: "innodbcluster"|"groupreplication"
	// InnoDB / Group Replication frame config (Type=="innodb"; reuses OS/OSVersion/
	// Arch, RootPassword, PMMNodeID, UseProxy, GenerateCert/CertTTL, ReplMode above;
	// GTID is always on). The Percona Server version comes from the PDPS repo.
	PDPSRepo    string `json:"pdpsRepo"`    // percona-release repo, e.g. "pdps-84-lts"
	MySQLRouter bool   `json:"mysqlRouter"` // install + run MySQL Router on each member
}

type designDoc struct {
	Nodes  []designNode  `json:"nodes"`
	Frames []designFrame `json:"frames"`
	Edges  []designEdge  `json:"edges"`
}

// backendFrameForProxySQL returns the backend database cluster frame a ProxySQL
// node/cluster is associated with — a PXC cluster *or* a MySQL replication frame —
// plus its type ("pxc"|"mysql"). It walks the canvas association graph (undirected),
// so a ProxySQL chained behind another ProxySQL still resolves the upstream backend.
func backendFrameForProxySQL(doc designDoc, startID string) (designFrame, string, bool) {
	frames := map[string]designFrame{}
	for _, f := range doc.Frames {
		if f.Type == "pxc" || f.Type == "mysql" {
			frames[f.ID] = f
		}
	}
	adj := map[string][]string{}
	for _, e := range doc.Edges {
		adj[e.From.Node] = append(adj[e.From.Node], e.To.Node)
		adj[e.To.Node] = append(adj[e.To.Node], e.From.Node)
	}
	visited := map[string]bool{startID: true}
	queue := []string{startID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nb := range adj[cur] {
			if f, ok := frames[nb]; ok {
				return f, f.Type, true
			}
			if !visited[nb] {
				visited[nb] = true
				queue = append(queue, nb)
			}
		}
	}
	return designFrame{}, "", false
}

// nodeConfig is the non-secret profile shown for a deployed node.
type nodeConfig struct {
	Domain      string   `json:"domain"`
	BaseDN      string   `json:"baseDN"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Alias       string   `json:"alias"`
	Hostname    string   `json:"hostname"`
	FQDN        string   `json:"fqdn"`
	LDAPAdminDN string   `json:"ldapAdminDN"`
	Services    []string `json:"services"`
	WebmailPort int      `json:"webmailPort,omitempty"`
}

// provProgress is the live provisioning status surfaced to the deployment console.
type provProgress struct {
	Percent int      `json:"percent"`
	Phase   string   `json:"phase"`
	Log     []string `json:"log"`
	Message string   `json:"message,omitempty"`
}

// provStep is one idempotent provisioning step (retried up to 10×).
type provStep struct {
	Name   string
	Script string
}

// nodeSecrets holds generated credentials for a deployed node.
type nodeSecrets struct {
	Domain            string `json:"domain"`
	BaseDN            string `json:"baseDN"`
	LDAPAdminDN       string `json:"ldapAdminDN"`
	LDAPAdminPassword string `json:"ldapAdminPassword"`
	MailAdminUser     string `json:"mailAdminUser"`
	MailAdminPassword string `json:"mailAdminPassword"`
}

type issue struct {
	Level   string `json:"level"` // info | warning | error
	Message string `json:"message"`
}

func hasError(issues []issue) bool {
	for _, i := range issues {
		if i.Level == "error" {
			return true
		}
	}
	return false
}

func networkName(stackID int64) string { return fmt.Sprintf("dbcanvas-stack-%d", stackID) }

func containerName(stackID int64, nodeID string) string {
	return fmt.Sprintf("dbcanvas-%d-%s", stackID, sanitizeName(nodeID))
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// domainToDN turns "example.net" into "dc=example,dc=net".
func domainToDN(domain string) string {
	parts := strings.Split(domain, ".")
	for i, p := range parts {
		parts[i] = "dc=" + p
	}
	return strings.Join(parts, ",")
}

// rsyslogScript{RHEL,Debian} install rsyslog if missing and enable+start it, so
// every systemd-image node has system logging. Best-effort.
const rsyslogScriptRHEL = `set -e
command -v rsyslogd >/dev/null 2>&1 || dnf -y -q install rsyslog >/dev/null
systemctl enable --now rsyslog >/dev/null 2>&1 || true`

const rsyslogScriptDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v rsyslogd >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get install -y -qq rsyslog >/dev/null; }
systemctl enable --now rsyslog >/dev/null 2>&1 || true`

// ensureRsyslog installs (if needed) + enables rsyslog on a systemd-image node.
// Best-effort: a failure is logged but never fails the deployment.
func (a *App) ensureRsyslog(ctx context.Context, id, os string, logln func(string)) {
	s := rsyslogScriptRHEL
	if isDebianOS(os) {
		s = rsyslogScriptDebian
	}
	if _, err := a.docker.Exec(ctx, id, []string{"bash", "-c", s}, nil); err != nil {
		logln("rsyslog setup skipped: " + err.Error())
	} else {
		logln("rsyslog installed + enabled")
	}
}

// genSecret returns prefix + 8 uppercase hex chars (e.g. LdapAdm!A02FB5C6).
func genSecret(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + strings.ToUpper(hex.EncodeToString(b))
}

// archOr returns the node's chosen arch, falling back to the host arch.
func archOr(a string) string {
	if a == "amd64" || a == "arm64" {
		return a
	}
	return hostArch()
}

func intranetImage(arch string) string {
	return "dbcanvas-systemd:oraclelinux-9-" + archOr(arch)
}

// --- validation ---

func (a *App) validateStack(ctx context.Context, st Stack) []issue {
	var out []issue
	if err := a.docker.Ping(ctx); err != nil {
		return append(out, issue{"error", "Docker is not reachable: " + err.Error()})
	}
	if osEnv := envOr("DOMAIN", ""); osEnv == "" {
		out = append(out, issue{"warning", "DOMAIN is not set; using default example.net"})
	}
	var doc designDoc
	if err := json.Unmarshal(st.Design, &doc); err != nil {
		return append(out, issue{"error", "stack design is invalid"})
	}
	if len(doc.Nodes) == 0 {
		out = append(out, issue{"warning", "Stack has no nodes to deploy"})
	}
	intranet := 0
	others := 0
	labels := map[string]int{}
	seenImg := map[string]bool{}
	exportReq := map[int][]string{} // requested host port → node labels (PXC + ProxySQL)
	pmmCat := loadPMMCatalog()
	for _, n := range doc.Nodes {
		labels[strings.TrimSpace(n.Label)]++
		switch n.Type {
		case "intranet":
			intranet++
			img := intranetImage(n.Arch)
			if !seenImg[img] {
				seenImg[img] = true
				if ok, _ := a.docker.ImageExists(ctx, img); !ok {
					out = append(out, issue{"error", "Missing image " + img + " — run `make images` first"})
				}
			}
		case "pmm":
			others++
			if !pmmCat.validPMMTag(n.Version) {
				out = append(out, issue{"warning", "Unknown PMM version " + n.Version + " for node " + n.Label + " — run `make versions`"})
			}
		case "proxysql":
			others++
			if n.FrameID != "" {
				break // ProxySQL cluster member — validated via its frame below
			}
			img := pxcImage(n.OS, n.OSVersion, n.Arch)
			if !seenImg[img] {
				seenImg[img] = true
				if ok, _ := a.docker.ImageExists(ctx, img); !ok {
					out = append(out, issue{"error", "Missing image " + img + " — run `make images` first"})
				}
			}
			if _, _, ok := backendFrameForProxySQL(doc, n.ID); !ok {
				out = append(out, issue{"error", "ProxySQL node " + n.Label + " must be linked to a PXC or MySQL cluster — draw an association line from one to it"})
			}
			if n.ExportEnabled && n.ExportHostPort > 0 {
				exportReq[n.ExportHostPort] = append(exportReq[n.ExportHostPort], n.Label)
			}
		case "ps":
			others++
			img := pxcImage(n.OS, n.OSVersion, n.Arch)
			if !seenImg[img] {
				seenImg[img] = true
				if ok, _ := a.docker.ImageExists(ctx, img); !ok {
					out = append(out, issue{"error", "Missing image " + img + " — run `make images` first"})
				}
			}
			if n.ExportEnabled && n.ExportHostPort > 0 {
				exportReq[n.ExportHostPort] = append(exportReq[n.ExportHostPort], n.Label)
			}
		default:
			others++
		}
	}
	if intranet > 1 {
		out = append(out, issue{"error", "Only one Intranet node is allowed per stack"})
	}
	// The Intranet provides DNS, mail, LDAP and the CA for the whole stack, so it
	// is required before any other node can be deployed.
	if others > 0 && intranet == 0 {
		out = append(out, issue{"error", "An Intranet node is required — add one before deploying other nodes"})
	}
	// Labels become DNS hostnames, so they must be present and unique — a stack
	// with duplicate (or blank) labels cannot be deployed.
	if labels[""] > 0 {
		out = append(out, issue{"error", "Every node must have a label"})
	}
	for l, c := range labels {
		if c > 1 && l != "" {
			out = append(out, issue{"error", "Duplicate node label: " + l + " — labels must be unique"})
		}
	}

	// --- PXC cluster frames ---
	clusterNames := map[string]int{}
	var usedPorts map[int]string
	for _, f := range doc.Frames {
		if f.Type != "pxc" {
			continue
		}
		clusterNames[strings.TrimSpace(f.Label)]++
		regs, total := 0, 0
		for _, n := range doc.Nodes {
			if n.FrameID != f.ID || n.Type != "pxc" {
				continue
			}
			total++
			if n.Role != "arbitrator" {
				regs++
			}
			if n.ExportEnabled && n.ExportHostPort > 0 {
				exportReq[n.ExportHostPort] = append(exportReq[n.ExportHostPort], n.Label)
			}
		}
		if regs == 0 {
			out = append(out, issue{"error", "PXC cluster " + f.Label + " needs at least one regular (data) node"})
		} else if regs < 3 {
			out = append(out, issue{"warning", "PXC cluster " + f.Label + ": at least 3 regular nodes are recommended for high availability"})
		}
		if total%2 == 0 && total > 0 {
			out = append(out, issue{"warning", "PXC cluster " + f.Label + ": an odd number of nodes keeps quorum on a split network"})
		}
	}
	for name, c := range clusterNames {
		if c > 1 && name != "" {
			out = append(out, issue{"error", "Duplicate PXC cluster name: " + name})
		}
	}

	// --- ProxySQL cluster frames ---
	proxyClusterNames := map[string]int{}
	for _, f := range doc.Frames {
		if f.Type != "proxysql" {
			continue
		}
		proxyClusterNames[strings.TrimSpace(f.Label)]++
		members := 0
		for _, n := range doc.Nodes {
			if n.FrameID != f.ID || n.Type != "proxysql" {
				continue
			}
			members++
			if n.ExportEnabled && n.ExportHostPort > 0 {
				exportReq[n.ExportHostPort] = append(exportReq[n.ExportHostPort], n.Label)
			}
		}
		if members == 0 {
			out = append(out, issue{"error", "ProxySQL cluster " + f.Label + " needs at least one ProxySQL node"})
		}
		img := pxcImage(f.OS, f.OSVersion, f.Arch)
		if !seenImg[img] {
			seenImg[img] = true
			if ok, _ := a.docker.ImageExists(ctx, img); !ok {
				out = append(out, issue{"error", "Missing image " + img + " — run `make images` first"})
			}
		}
		if _, _, ok := backendFrameForProxySQL(doc, f.ID); !ok {
			out = append(out, issue{"error", "ProxySQL cluster " + f.Label + " must be linked to a PXC or MySQL cluster — draw an association line from one to it"})
		}
	}
	for name, c := range proxyClusterNames {
		if c > 1 && name != "" {
			out = append(out, issue{"error", "Duplicate ProxySQL cluster name: " + name})
		}
	}

	// --- MySQL replication frames ---
	mysqlNames := map[string]int{}
	for _, f := range doc.Frames {
		if f.Type != "mysql" {
			continue
		}
		mysqlNames[strings.TrimSpace(f.Label)]++
		primaries, secondaries := 0, 0
		for _, n := range doc.Nodes {
			if n.FrameID != f.ID || n.Type != "mysql" {
				continue
			}
			if n.Role == "primary" {
				primaries++
			} else {
				secondaries++
			}
			if n.ExportEnabled && n.ExportHostPort > 0 {
				exportReq[n.ExportHostPort] = append(exportReq[n.ExportHostPort], n.Label)
			}
		}
		if primaries != 1 {
			out = append(out, issue{"error", fmt.Sprintf("MySQL replication %s must have exactly one primary (has %d)", f.Label, primaries)})
		}
		if secondaries == 0 {
			out = append(out, issue{"error", "MySQL replication " + f.Label + " needs at least one secondary"})
		}
		img := pxcImage(f.OS, f.OSVersion, f.Arch)
		if !seenImg[img] {
			seenImg[img] = true
			if ok, _ := a.docker.ImageExists(ctx, img); !ok {
				out = append(out, issue{"error", "Missing image " + img + " — run `make images` first"})
			}
		}
	}
	for name, c := range mysqlNames {
		if c > 1 && name != "" {
			out = append(out, issue{"error", "Duplicate MySQL replication name: " + name})
		}
	}

	// --- InnoDB / Group Replication frames ---
	innodbNames := map[string]int{}
	for _, f := range doc.Frames {
		if f.Type != "innodb" {
			continue
		}
		innodbNames[strings.TrimSpace(f.Label)]++
		members := 0
		for _, n := range doc.Nodes {
			if n.FrameID != f.ID || n.Type != "innodb" {
				continue
			}
			members++
			if n.ExportEnabled && n.ExportHostPort > 0 {
				exportReq[n.ExportHostPort] = append(exportReq[n.ExportHostPort], n.Label)
			}
		}
		if members == 0 {
			out = append(out, issue{"error", "InnoDB/GR cluster " + f.Label + " needs at least one node"})
		} else if members < 3 {
			out = append(out, issue{"warning", "InnoDB/GR cluster " + f.Label + ": at least 3 nodes are recommended for quorum"})
		} else if members%2 == 0 {
			out = append(out, issue{"warning", "InnoDB/GR cluster " + f.Label + ": an odd number of nodes keeps quorum on a split network"})
		}
		img := pxcImage(f.OS, f.OSVersion, f.Arch)
		if !seenImg[img] {
			seenImg[img] = true
			if ok, _ := a.docker.ImageExists(ctx, img); !ok {
				out = append(out, issue{"error", "Missing image " + img + " — run `make images` first"})
			}
		}
	}
	for name, c := range innodbNames {
		if c > 1 && name != "" {
			out = append(out, issue{"error", "Duplicate InnoDB/GR cluster name: " + name})
		}
	}

	// --- cross-cluster replication links (async / bidirectional) ---
	// Each replication edge must connect two replication-capable members in
	// *different* clusters, both with GTID enabled (auto-positioning); a server-id
	// collision between the endpoints breaks replication.
	replPairs := map[string]bool{}
	for _, e := range doc.Edges {
		if !isReplEdge(e) {
			continue
		}
		src, fa, ok1 := replMember(doc, e.From.Node)
		dst, fb, ok2 := replMember(doc, e.To.Node)
		if !ok1 || !ok2 {
			out = append(out, issue{"error", "A replication link must connect two PXC or Percona Server cluster members"})
			continue
		}
		if fa.ID == fb.ID {
			out = append(out, issue{"error", fmt.Sprintf("Replication link %s ↔ %s must connect members in different clusters", src.Label, dst.Label)})
			continue
		}
		key := src.ID + "|" + dst.ID
		rev := dst.ID + "|" + src.ID
		if replPairs[key] || replPairs[rev] {
			out = append(out, issue{"error", fmt.Sprintf("Duplicate replication link between %s and %s", src.Label, dst.Label)})
		}
		replPairs[key] = true
		if !fa.GTID || !fb.GTID {
			out = append(out, issue{"warning", fmt.Sprintf("Replication link %s ↔ %s uses binary-log file/position (GTID off on a cluster) — only writes made after deploy replicate; seed existing data first", src.Label, dst.Label)})
		}
		if memberServerID(src) == memberServerID(dst) {
			out = append(out, issue{"warning", fmt.Sprintf("Replication link %s ↔ %s: both resolve to server-id %d — rename one so the ids differ", src.Label, dst.Label, memberServerID(src))})
		}
		if e.Type == "bidir" {
			out = append(out, issue{"warning", fmt.Sprintf("Bidirectional replication %s ↔ %s is multi-writer — avoid writing the same rows on both sides", src.Label, dst.Label)})
		}
	}

	// Export host-port conflicts: within the design, and against ports already
	// published by other containers (the stack's own containers are excluded so a
	// redeploy doesn't flag itself).
	if len(exportReq) > 0 {
		usedPorts, _ = a.docker.ListPublishedPorts(ctx)
		selfPrefix := fmt.Sprintf("dbcanvas-%d-", st.ID)
		for port, who := range exportReq {
			if len(who) > 1 {
				out = append(out, issue{"error", fmt.Sprintf("Export host port %d requested by multiple nodes: %s", port, strings.Join(who, ", "))})
			}
			if owner, taken := usedPorts[port]; taken && !strings.HasPrefix(owner, selfPrefix) {
				out = append(out, issue{"error", fmt.Sprintf("Export host port %d is already in use (by %s)", port, owner)})
			}
		}
	}

	if len(out) == 0 {
		out = append(out, issue{"info", "All checks passed"})
	}
	return out
}

func (a *App) handleValidateStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	issues := a.validateStack(r.Context(), st)
	writeJSON(w, http.StatusOK, map[string]any{"ok": !hasError(issues), "issues": issues})
}

// --- deploy ---

func (a *App) handleDeployStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	bg := context.Background()
	issues := a.validateStack(bg, st)
	if hasError(issues) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "issues": issues})
		return
	}

	var doc designDoc
	json.Unmarshal(st.Design, &doc)

	if err := a.docker.NetworkEnsure(bg, networkName(st.ID)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create network: "+err.Error())
		return
	}

	deps, _ := a.store.ListDeployments(st.ID)
	existing := map[string]Deployment{}
	for _, d := range deps {
		existing[d.NodeID] = d
	}
	inDesign := map[string]bool{}
	for _, n := range doc.Nodes {
		inDesign[n.ID] = true
	}

	// Remove containers for nodes deleted from the canvas.
	removed := false
	for _, d := range deps {
		if !inDesign[d.NodeID] {
			if d.ContainerID != "" {
				a.docker.ContainerRemove(bg, d.ContainerID)
			}
			a.store.DeleteDeployment(st.ID, d.NodeID)
			removed = true
		}
	}
	// Drop removed hosts from the Intranet DNS zones.
	if removed {
		a.reconcileStackDNS(bg, st.ID)
	}

	// Create newly added nodes; keep already-running ones (redeploy). Cluster
	// members (PXC or ProxySQL, identified by FrameID) are provisioned as a unit by
	// their frame, not individually.
	for _, n := range doc.Nodes {
		if n.FrameID != "" {
			continue
		}
		if d, ok := existing[n.ID]; ok && d.State == DeployRunning {
			continue
		}
		switch n.Type {
		case "intranet":
			a.provisionIntranet(st, n)
		case "pmm":
			a.provisionPMM(st, n, doc)
		case "proxysql":
			a.provisionProxySQL(st, n, doc)
		case "ps":
			a.provisionPerconaServer(st, n, doc)
		}
	}

	// Cluster frames: (re)provision a frame unless all its member nodes are already
	// running. PXC formation is sequential/all-or-nothing; ProxySQL members are
	// independent but treated the same for the redeploy gate.
	for _, f := range doc.Frames {
		memberType := ""
		switch f.Type {
		case "pxc":
			memberType = "pxc"
		case "proxysql":
			memberType = "proxysql"
		case "mysql":
			memberType = "mysql"
		case "innodb":
			memberType = "innodb"
		default:
			continue
		}
		members := 0
		running := 0
		for _, n := range doc.Nodes {
			if n.FrameID == f.ID && n.Type == memberType {
				members++
				if d, ok := existing[n.ID]; ok && d.State == DeployRunning {
					running++
				}
			}
		}
		if members > 0 && running == members {
			continue
		}
		switch f.Type {
		case "pxc":
			a.provisionPXCFrame(st, f, doc)
		case "proxysql":
			a.provisionProxySQLFrame(st, f, doc)
		case "mysql":
			a.provisionMySQLFrame(st, f, doc)
		case "innodb":
			a.provisionInnoDBFrame(st, f, doc)
		}
	}

	// Final phase: configure cross-cluster replication links (async / bidirectional)
	// drawn between cluster members. It waits for the clusters to come up, then
	// reconciles channels (creating new ones, pruning removed ones) on each redeploy.
	go a.reconcileReplication(st, doc)

	a.store.SetStackStatus(st.ID, StackDeployed)
	out, _ := a.store.ListDeployments(st.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"deployments": out})
}

// provisionIntranet records the deployment and starts an async provisioning
// goroutine for an Intranet node.
func (a *App) provisionIntranet(st Stack, n designNode) {
	domain := envOr("DOMAIN", "example.net")
	baseDN := domainToDN(domain)
	adminDN := "cn=admin," + baseDN

	// reuse secrets if this node was deployed before (keeps creds stable)
	var sec nodeSecrets
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		json.Unmarshal(dep.Secrets, &sec)
	}
	if sec.LDAPAdminPassword == "" {
		sec = nodeSecrets{
			Domain:            domain,
			BaseDN:            baseDN,
			LDAPAdminDN:       adminDN,
			LDAPAdminPassword: genSecret("LdapAdm!"),
			MailAdminUser:     "admin@" + domain,
			MailAdminPassword: genSecret("MailAdm!"),
		}
	}
	cfg := nodeConfig{
		Domain: domain, BaseDN: baseDN, OS: "oel9", Arch: archOr(n.Arch),
		Alias: "intranet", Hostname: "intranet", FQDN: "intranet." + domain, LDAPAdminDN: adminDN,
		Services: []string{"Squid proxy", "DNS", "SMTP", "IMAP", "Webmail (RoundCube)", "OpenLDAP", "Self-signing CA"},
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	// Each node provisions in its own goroutine, so one failing never blocks the
	// others. Steps are retried up to 10×; progress is published for the console.
	go func() {
		ctx := context.Background()
		prog := &provProgress{Percent: 0, Phase: "Starting", Log: []string{}}
		save := func() { b, _ := json.Marshal(prog); a.store.SetDeploymentProgress(st.ID, n.ID, b) }
		logln := func(s string) {
			prog.Log = append(prog.Log, s)
			if len(prog.Log) > 200 {
				prog.Log = prog.Log[len(prog.Log)-200:]
			}
			save()
		}
		setPhase := func(p string, pct int) { prog.Phase = p; prog.Percent = pct; save() }
		failNode := func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			log.Printf("stack %d node %s: %s", st.ID, n.ID, msg)
			prog.Phase = "failed"
			prog.Message = msg
			save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployError)
		}

		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
		setPhase("Creating container", 3)

		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		// Pin the Intranet to a stable address (host .2 of the stack subnet) so it
		// stays a reliable DNS resolver / SMTP relay for the other nodes across
		// restarts. The FQDN alias also lets peers reach it as intranet.<domain>.
		subnet, _ := a.docker.NetworkSubnet(ctx, networkName(st.ID))
		id, err := a.docker.ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: intranetImage(n.Arch), Hostname: "intranet",
			Network: networkName(st.ID), Aliases: []string{"intranet", "intranet." + domain},
			Privileged: true, PublishPort: 80, IPv4Address: staticIntranetIP(subnet),
		})
		if err != nil {
			failNode("create container: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			failNode("start container: %v", err)
			return
		}

		// record the auto-assigned (unused) host port for RoundCube
		if hp, e := a.docker.ContainerPort(ctx, id, "80/tcp"); e == nil && hp != "" {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.WebmailPort = p
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		logln(fmt.Sprintf("container started (webmail host port %d)", cfg.WebmailPort))

		setPhase("Waiting for systemd", 8)
		if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
			failNode("systemd did not start: %v", err)
			return
		}

		env := []string{
			"DOMAIN=" + sec.Domain,
			"BASE_DN=" + sec.BaseDN,
			"LDAP_ADMIN_DN=" + sec.LDAPAdminDN,
			"LDAP_ADMIN_PW=" + sec.LDAPAdminPassword,
			"MAIL_ADMIN=admin",
			"MAIL_ADMIN_PW=" + sec.MailAdminPassword,
		}
		steps := intranetSteps()
		for i, step := range steps {
			setPhase(step.Name, 10+i*88/len(steps))
			lastErr := ""
			ok := false
			for attempt := 1; attempt <= 10; attempt++ {
				res, err := a.docker.Exec(ctx, id, []string{"bash", "-c", step.Script}, env)
				if err == nil && res.Code == 0 {
					ok = true
					break
				}
				if err != nil {
					lastErr = err.Error()
				} else if lastErr = strings.TrimSpace(res.Stderr); lastErr == "" {
					lastErr = strings.TrimSpace(res.Stdout)
				}
				logln(fmt.Sprintf("%s: attempt %d/10 failed: %s", step.Name, attempt, lastLines(lastErr, 160)))
				time.Sleep(2 * time.Second)
			}
			if !ok {
				failNode("step %q failed after 10 attempts: %s", step.Name, lastLines(lastErr, 160))
				return
			}
			logln(step.Name + ": ok")
		}

		// Configure bind as the authoritative resolver and publish DNS records for
		// every host in the stack (including the Intranet itself).
		setPhase("Publishing DNS records", 98)
		a.reconcileStackDNS(ctx, st.ID)
		logln("DNS zones published")

		setPhase("Running", 100)
		prog.Message = "provisioned"
		save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		log.Printf("stack %d node %s: provisioned", st.ID, n.ID)
	}()
}

// intranetSteps is the ordered, idempotent provisioning sequence. Each step is
// run via `bash -c` inside the container and may be retried.
func intranetSteps() []provStep {
	return []provStep{
		{"Enable repositories", `set -e
dnf -y install oracle-epel-release-el9 >/dev/null 2>&1 || dnf -y install epel-release >/dev/null 2>&1 || true
dnf config-manager --set-enabled ol9_codeready_builder >/dev/null 2>&1 || true`},

		{"Install packages", `set -e
dnf -y install rsyslog squid bind bind-utils postfix dovecot openldap-servers openldap-clients httpd php php-fpm roundcubemail mod_ssl openssl net-tools >/dev/null`},

		{"Create CA", `set -e
install -d -m 0755 /etc/pki/dbcanvas
if [ ! -f /etc/pki/dbcanvas/ca.crt ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -keyout /etc/pki/dbcanvas/ca.key -out /etc/pki/dbcanvas/ca.crt -subj "/O=DBCanvas/CN=DBCanvas CA" >/dev/null 2>&1
fi
chmod 600 /etc/pki/dbcanvas/ca.key 2>/dev/null || true`},

		{"Configure OpenLDAP", `set -e
chown -R ldap:ldap /var/lib/ldap 2>/dev/null || true
systemctl enable --now slapd
for i in $(seq 1 20); do ldapsearch -Y EXTERNAL -H ldapi:/// -b cn=config -s base >/dev/null 2>&1 && break; sleep 1; done
HASH=$(slappasswd -s "$LDAP_ADMIN_PW")
cat >/tmp/db.ldif <<EOF
dn: olcDatabase={2}mdb,cn=config
changetype: modify
replace: olcSuffix
olcSuffix: $BASE_DN
-
replace: olcRootDN
olcRootDN: $LDAP_ADMIN_DN
-
replace: olcRootPW
olcRootPW: $HASH
EOF
ldapmodify -Y EXTERNAL -H ldapi:/// -f /tmp/db.ldif
for s in cosine inetorgperson nis; do ldapadd -Y EXTERNAL -H ldapi:/// -f "/etc/openldap/schema/$s.ldif" >/dev/null 2>&1 || true; done`},

		{"Seed LDAP directory", `set -e
DC="${BASE_DN%%,*}"; DC="${DC#dc=}"
cat >/tmp/base.ldif <<EOF
dn: $BASE_DN
objectClass: top
objectClass: dcObject
objectClass: organization
o: $DOMAIN
dc: $DC

dn: ou=People,$BASE_DN
objectClass: organizationalUnit
ou: People

dn: ou=Groups,$BASE_DN
objectClass: organizationalUnit
ou: Groups
EOF
ldapadd -x -D "$LDAP_ADMIN_DN" -w "$LDAP_ADMIN_PW" -f /tmp/base.ldif 2>/dev/null || ldapsearch -x -D "$LDAP_ADMIN_DN" -w "$LDAP_ADMIN_PW" -b "$BASE_DN" -s base dn >/dev/null`},

		{"Configure mail", `set -e
getent group vmail >/dev/null || groupadd -g 5000 vmail
id vmail >/dev/null 2>&1 || useradd -g vmail -u 5000 -d /var/mail/vhosts -s /sbin/nologin vmail
install -d -o vmail -g vmail "/var/mail/vhosts/$DOMAIN"
postconf -e "myhostname = intranet.$DOMAIN" "mydomain = $DOMAIN" "myorigin = \$mydomain" "inet_interfaces = all" "inet_protocols = ipv4" "virtual_mailbox_domains = $DOMAIN" "virtual_mailbox_base = /var/mail/vhosts" "virtual_mailbox_maps = hash:/etc/postfix/vmailbox" "virtual_minimum_uid = 5000" "virtual_uid_maps = static:5000" "virtual_gid_maps = static:5000"
touch /etc/postfix/vmailbox
grep -q "^$MAIL_ADMIN@$DOMAIN " /etc/postfix/vmailbox || echo "$MAIL_ADMIN@$DOMAIN $DOMAIN/$MAIL_ADMIN/" >> /etc/postfix/vmailbox
postmap /etc/postfix/vmailbox
install -d /etc/dovecot
[ -f /etc/dovecot/users ] || echo "$MAIL_ADMIN@$DOMAIN:{PLAIN}$MAIL_ADMIN_PW::::::" > /etc/dovecot/users
# Wire dovecot to authenticate the virtual users (passwd-file) over plaintext
# IMAP on localhost, with maildirs matching postfix's virtual_mailbox_base.
cat > /etc/dovecot/conf.d/99-dbcanvas.conf <<'DCONF'
protocols = imap
ssl = no
disable_plaintext_auth = no
auth_mechanisms = plain login
mail_location = maildir:/var/mail/vhosts/%d/%n
first_valid_uid = 5000
passdb {
  driver = passwd-file
  args = scheme=PLAIN username_format=%u /etc/dovecot/users
}
userdb {
  driver = static
  args = uid=vmail gid=vmail home=/var/mail/vhosts/%d/%n
}
DCONF`},

		{"Configure webmail", `set -e
install -d -o apache -g apache /var/lib/roundcubemail
RC=/etc/roundcubemail/config.inc.php
cat > "$RC" <<'RCCFG'
<?php
$config = [];
$config['db_dsnw'] = 'sqlite:////var/lib/roundcubemail/roundcube.db?mode=0646';
$config['imap_host'] = 'localhost';
$config['imap_port'] = 143;
// SMTP: localhost:25 with no auth (delivery permitted via postfix mynetworks).
// smtp_server/smtp_port are the RoundCube 1.5 keys; smtp_host is the 1.6 name.
$config['smtp_server'] = 'localhost';
$config['smtp_port'] = 25;
$config['smtp_host'] = 'localhost:25';
$config['smtp_user'] = '';
$config['smtp_pass'] = '';
$config['des_key'] = 'dbcanvasRoundcube24key!!';
$config['enable_installer'] = false;
$config['support_url'] = '';
$config['product_name'] = 'DBCanvas Webmail';
RCCFG
chown apache:apache "$RC" 2>/dev/null || true
php -r '$f="/var/lib/roundcubemail/roundcube.db"; if(!file_exists($f)){$db=new PDO("sqlite:".$f); $db->exec(file_get_contents("/usr/share/roundcubemail/SQL/sqlite.initial.sql"));}' 2>/dev/null || true
chown -R apache:apache /var/lib/roundcubemail 2>/dev/null || true
CONF=/etc/httpd/conf.d/roundcubemail.conf
[ -f "$CONF" ] && sed -i 's/Require local/Require all granted/g' "$CONF" || true
true`},

		{"Configure Squid", `set -e
CONF=/etc/squid/squid.conf
grep -q '^maximum_object_size 150 MB$' "$CONF" || echo 'maximum_object_size 150 MB' >> "$CONF"
grep -q '^cache_dir ufs /var/spool/squid ' "$CONF" || echo 'cache_dir ufs /var/spool/squid 4000 16 256' >> "$CONF"
install -d -o squid -g squid /var/spool/squid 2>/dev/null || true`},
		// NOTE: the cache_dir swap directories are initialized by the squid.service's
		// own ExecStartPre (cache_swap.sh) on start — do NOT run "squid -z" here: it
		// leaves a detached instance + /run/squid.pid that makes the subsequent
		// systemctl start fail with "Squid is already running" (Result: protocol).

		{"Enable services", `set -e
echo "ServerName intranet.$DOMAIN" > /etc/httpd/conf.d/servername.conf
for svc in rsyslog slapd squid named postfix dovecot php-fpm httpd; do
  systemctl enable "$svc" >/dev/null 2>&1 || true
  systemctl restart "$svc" >/dev/null 2>&1 || true
done`},
	}
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

// --- lifecycle + profile ---

func (a *App) handleGetNode(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	dep, err := a.store.GetDeployment(st.ID, r.PathValue("nid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "node is not deployed")
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (a *App) handleNodeAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, _, ok := a.loadOwnedStack(w, r)
		if !ok {
			return
		}
		nid := r.PathValue("nid")
		dep, err := a.store.GetDeployment(st.ID, nid)
		if err != nil || dep.ContainerID == "" {
			writeErr(w, http.StatusNotFound, "node is not deployed")
			return
		}
		ctx := r.Context()
		switch action {
		case "start":
			err = a.docker.ContainerStart(ctx, dep.ContainerID)
			if err == nil {
				a.store.SetDeploymentState(st.ID, nid, DeployRunning)
				a.refreshPublishedPorts(ctx, st, nid, dep)
				a.restoreNodeResolver(ctx, st, nid, dep)
				a.reconcileStackDNS(ctx, st.ID)
			}
		case "stop":
			err = a.docker.ContainerStop(ctx, dep.ContainerID)
			if err == nil {
				a.store.SetDeploymentState(st.ID, nid, DeployStopped)
			}
		case "restart":
			err = a.docker.ContainerRestart(ctx, dep.ContainerID)
			if err == nil {
				a.store.SetDeploymentState(st.ID, nid, DeployRunning)
				a.refreshPublishedPorts(ctx, st, nid, dep)
				a.restoreNodeResolver(ctx, st, nid, dep)
				a.reconcileStackDNS(ctx, st.ID)
			}
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		updated, _ := a.store.GetDeployment(st.ID, nid)
		writeJSON(w, http.StatusOK, updated)
	}
}

// refreshPublishedPorts re-reads a node container's auto-assigned host ports and
// persists them into the stored config. Containers are created with an empty
// HostPort binding, so Docker hands out a *new* ephemeral host port every time
// the container starts — meaning a stop/start or restart changes the published
// port and would otherwise leave the recorded access links (Intranet webmail,
// PMM 8080/8443) pointing at the old, now-invalid port. Called after start and
// restart for both node types.
func (a *App) refreshPublishedPorts(ctx context.Context, st Stack, nid string, dep Deployment) {
	if dep.ContainerID == "" {
		return
	}
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	typ := ""
	for _, n := range doc.Nodes {
		if n.ID == nid {
			typ = n.Type
			break
		}
	}
	readPort := func(portProto string) (int, bool) {
		hp, err := a.docker.ContainerPort(ctx, dep.ContainerID, portProto)
		if err != nil || hp == "" {
			return 0, false
		}
		v, err := strconv.Atoi(hp)
		return v, err == nil
	}
	save := func(cfg any) {
		b, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{
			StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID,
			State: DeployRunning, Config: b, Secrets: dep.Secrets,
		})
	}
	switch typ {
	case "intranet":
		var cfg nodeConfig
		json.Unmarshal(dep.Config, &cfg)
		if p, ok := readPort("80/tcp"); ok {
			cfg.WebmailPort = p
		}
		save(cfg)
	case "pmm":
		var cfg pmmConfig
		json.Unmarshal(dep.Config, &cfg)
		if p, ok := readPort("8080/tcp"); ok {
			cfg.HTTPPort = p
		}
		if p, ok := readPort("8443/tcp"); ok {
			cfg.HTTPSPort = p
		}
		save(cfg)
	case "proxysql":
		var cfg proxysqlConfig
		json.Unmarshal(dep.Config, &cfg)
		if p, ok := readPort(fmt.Sprintf("%d/tcp", proxysqlMySQLPort)); ok {
			cfg.MySQLPort = p
		}
		if p, ok := readPort(fmt.Sprintf("%d/tcp", proxysqlAdminPort)); ok {
			cfg.AdminPort = p
		}
		save(cfg)
	case "mysql", "ps":
		var cfg mysqlConfig
		json.Unmarshal(dep.Config, &cfg)
		if p, ok := readPort("3306/tcp"); ok {
			cfg.ExportPort = p
		}
		save(cfg)
	case "innodb":
		var cfg innodbConfig
		json.Unmarshal(dep.Config, &cfg)
		if p, ok := readPort("6446/tcp"); ok {
			cfg.RWPort = p
		}
		if p, ok := readPort("6447/tcp"); ok {
			cfg.ROPort = p
		}
		save(cfg)
	}
}

// handleDestroyStack tears down the deployment (all containers + the per-stack
// network), clears the deployment records, and returns the stack to draft so it
// can be redeployed fresh. The stack design is preserved; post-deployment-only
// node state (generated credentials, LDAP/email users, certificates) is reset
// because the deployment rows and containers are removed.
func (a *App) handleDestroyStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	a.teardownStack(st.ID)
	a.store.SetStackStatus(st.ID, StackDraft)
	writeJSON(w, http.StatusOK, map[string]any{"status": StackDraft, "deployments": []Deployment{}})
}

// teardownStack stops and removes every container deployed for a stack and
// removes its network. Best-effort.
func (a *App) teardownStack(stackID int64) {
	if a.docker == nil {
		return
	}
	ctx := context.Background()
	deps, _ := a.store.ListDeployments(stackID)
	for _, d := range deps {
		if d.ContainerID != "" {
			a.docker.ContainerRemove(ctx, d.ContainerID)
		}
		a.store.DeleteDeployment(stackID, d.NodeID)
	}
	a.docker.NetworkRemove(ctx, networkName(stackID))
}
