package main

import (
	"context"
	"encoding/json"
)

// clusterauth.go — LDAP (authentication_ldap_simple) and Keycloak OIDC (auth_openid_connect)
// logins on the two Percona MySQL cluster frames: Percona Server asynchronous replication
// (Type=="mysql") and PXC (Type=="pxc"). The per-server work is the standalone node's —
// applyDirectoryAuthStep (dbauth.go) and applyMySQLOIDCStep (mysqloidc.go) — and this file is
// only about the order a cluster has to do it in.
//
// The order matters because the accounts replicate. CREATE USER … IDENTIFIED WITH
// auth_openid_connect names a plugin, and a server that has not loaded that plugin refuses the
// statement — on an async replica that stops the SQL thread, and on a Galera member it is an
// apply failure that gets the member voted out. So every member loads the plugins first and the
// accounts are created once, last, where the cluster replicates them from:
//
//   - Async replication. The members are running and replicating by the time this runs, so each
//     replica is restarted into the plugins (read_only and super_read_only are SET PERSIST, and
//     the replication threads restart with the server), then the primary is — and only the
//     primary creates the demo schema and the accounts. If a replica could not load a plugin,
//     the primary's step is skipped rather than break that replica. A member that joins later
//     is copied from the primary, so it has the accounts already and only needs the plugins.
//   - PXC. Restarting a member of a formed cluster means leaving and rejoining it, and the
//     bootstrap member runs mysql@bootstrap, which must not be restarted once others have
//     joined: it would bootstrap a second cluster. So the bootstrap member is configured right
//     after it bootstraps, while it is still alone (the same window its certificate uses), and
//     creates the accounts; every joiner gets the option block written before its first start,
//     and its SST brings the accounts with it. A joiner only stages what a running member
//     already has live, so a step the bootstrap member skipped is not forced onto the rest.
//
// Both plugins are startup-configured (their variables are read-only at runtime), so there is
// no restart-free way to add them to a member that is already up.

// mysqlAuthStep says how one Percona Server takes an auth step.
type mysqlAuthStep struct {
	Unit    string // systemd unit restarted to load the plugin
	Restart bool   // false: only write the option block, for a server yet to start
	SQL     bool   // OIDC: create the demo schema and accounts (once per cluster)
	// For the login guides: where an account has to be created on this member's cluster
	// ("primary" | "galera"; "" standalone), and the primary's FQDN for "primary".
	Scope       string
	PrimaryFQDN string
}

// mysqlAuthFrameType reports whether a frame type (or its members' node type — the same
// string for both) carries its logins on the frame: Percona Server replication and PXC.
func mysqlAuthFrameType(t string) bool { return t == "mysql" || t == "pxc" }

// clusterAuthFrame reports whether f is a frame this file has logins to wire on.
func clusterAuthFrame(f designFrame) bool {
	return mysqlAuthFrameType(f.Type) && (f.LdapAuth || f.EnableOIDC)
}

// clusterAuthMember is member n as the per-server steps expect a node: the frame's settings on
// the member's own id, so what they persist lands on the member's deployment.
func clusterAuthMember(f designFrame, n designNode) designNode {
	n.LdapAuth, n.LdapDirNodeID, n.KerberosAuth = f.LdapAuth, f.LdapDirNodeID, false
	n.EnableOIDC, n.KeycloakNodeID, n.OIDCRealm = f.EnableOIDC, f.KeycloakNodeID, f.OIDCRealm
	n.OS = f.OS
	n.PSMajor = frameMajor(f) // only the OIDC step's "needs version X" message reads it
	return n
}

// clusterAuthLive reports which of the frame's logins are live on a member other than skip —
// what a joining member has to match.
func (a *App) clusterAuthLive(st Stack, doc designDoc, f designFrame, skip string) (ldap, oidc bool) {
	for _, n := range doc.Nodes {
		if n.FrameID != f.ID || n.ID == skip || n.Role == "arbitrator" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || len(dep.Config) == 0 {
			continue
		}
		var cfg struct {
			DirAuth *dirAuthInfo `json:"dirAuth"`
			OIDC    *oidcInfo    `json:"oidc"`
		}
		json.Unmarshal(dep.Config, &cfg)
		ldap = ldap || cfg.DirAuth != nil && cfg.DirAuth.Ldap
		oidc = oidc || cfg.OIDC != nil && cfg.OIDC.Enabled
	}
	return ldap, oidc
}

// mysqlFrameAuth wires a freshly built replication frame's logins: replicas first, then the
// primary, which alone creates the OIDC accounts. Failures are logged and skipped, as on a
// standalone node — the cluster itself is fine without them.
func (a *App) mysqlFrameAuth(ctx context.Context, st Stack, frame designFrame, doc designDoc, primary designNode, secondaries []designNode, sec pxcSecrets, progs map[string]*pxcProg) {
	if !clusterAuthFrame(frame) {
		return
	}
	domain := envOr("DOMAIN", "example.net")
	primaryFQDN := fqdnOf(stackHostnames(doc)[primary.ID], domain)
	unit := mysqlUnit(frame.OS)
	step := func(sql bool) mysqlAuthStep {
		return mysqlAuthStep{Unit: unit, Restart: true, SQL: sql, Scope: "primary", PrimaryFQDN: primaryFQDN}
	}
	members := append(append([]designNode{}, secondaries...), primary)
	if frame.LdapAuth {
		for _, n := range members {
			if err := a.applyDirectoryAuthStep(ctx, st, clusterAuthMember(frame, n), doc, a.containerOf(st.ID, n.ID), "ps", sec.RootPassword, step(false), progs[n.ID]); err != nil {
				progs[n.ID].logln("directory authentication skipped: " + err.Error())
			}
		}
	}
	if frame.EnableOIDC {
		ok := true
		for _, n := range secondaries {
			if err := a.applyMySQLOIDCStep(ctx, st, clusterAuthMember(frame, n), doc, a.containerOf(st.ID, n.ID), sec, step(false), progs[n.ID]); err != nil {
				progs[n.ID].logln("Keycloak OIDC skipped: " + err.Error())
				ok = false
			}
		}
		pr := progs[primary.ID]
		if !ok {
			// Its CREATE USER … auth_openid_connect would stop the replica that has no plugin.
			pr.logln("Keycloak OIDC skipped: a replica could not load auth_openid_connect, and the accounts the primary would create replicate to it")
		} else if err := a.applyMySQLOIDCStep(ctx, st, clusterAuthMember(frame, primary), doc, a.containerOf(st.ID, primary.ID), sec, step(true), pr); err != nil {
			pr.logln("Keycloak OIDC skipped: " + err.Error())
		}
	}
	// The primary's restarts dropped every replica's connection, and a replica waits out
	// SOURCE_CONNECT_RETRY (60s) before it tries again — so the cluster would report running
	// while its replicas sit disconnected, the accounts just created not yet on them. Restart
	// the receiver threads now instead.
	verb := "REPLICA"
	if frameMajor(frame) == "8.0" {
		verb = "SLAVE" // REPLICA arrived in 8.0.22; SLAVE works on every 8.0 build
	}
	for _, n := range secondaries {
		if err := a.runStep(ctx, a.containerOf(st.ID, n.ID), mysqlReconnectScript, []string{"ROOTPW=" + sec.RootPassword, "VERB=" + verb}, progs[n.ID].logln); err != nil {
			progs[n.ID].logln("replica reconnect: " + err.Error())
		}
	}
}

// mysqlReconnectScript restarts a replica's receiver thread and waits for it to connect.
const mysqlReconnectScript = `mysql -uroot -p"$ROOTPW" -e "STOP $VERB IO_THREAD; START $VERB IO_THREAD;" 2>/dev/null
for i in $(seq 1 30); do
  mysql -uroot -p"$ROOTPW" --vertical -e "SHOW $VERB STATUS" 2>/dev/null | grep -qE "(Replica|Slave)_IO_Running: Yes" && { echo "replica reconnected to the primary"; exit 0; }
  sleep 2
done
echo "replica receiver thread is not running yet — it keeps retrying on its own"; exit 1`

// mysqlJoinAuth loads the cluster's live logins on a replica that joined a running replication
// frame. Its data was copied from the primary, accounts included, so it only needs the plugins.
func (a *App) mysqlJoinAuth(ctx context.Context, st Stack, frame designFrame, doc designDoc, n designNode, primaryFQDN string, sec pxcSecrets, pr *pxcProg) {
	if !clusterAuthFrame(frame) {
		return
	}
	ldap, oidc := a.clusterAuthLive(st, doc, frame, n.ID)
	step := mysqlAuthStep{Unit: mysqlUnit(frame.OS), Restart: true, Scope: "primary", PrimaryFQDN: primaryFQDN}
	m := clusterAuthMember(frame, n)
	if ldap && frame.LdapAuth {
		if err := a.applyDirectoryAuthStep(ctx, st, m, doc, a.containerOf(st.ID, n.ID), "ps", sec.RootPassword, step, pr); err != nil {
			pr.logln("directory authentication skipped: " + err.Error())
		}
	}
	if oidc && frame.EnableOIDC {
		if err := a.applyMySQLOIDCStep(ctx, st, m, doc, a.containerOf(st.ID, n.ID), sec, step, pr); err != nil {
			pr.logln("Keycloak OIDC skipped: " + err.Error())
		}
	}
}

// pxcBootstrapAuth wires the logins on a PXC cluster's bootstrap member while it is the only
// member, and creates the OIDC accounts the joiners will receive with their SST.
func (a *App) pxcBootstrapAuth(ctx context.Context, st Stack, frame designFrame, doc designDoc, n designNode, sec pxcSecrets, pr *pxcProg) {
	if !clusterAuthFrame(frame) {
		return
	}
	step := mysqlAuthStep{Unit: "mysql@bootstrap", Restart: true, Scope: "galera"}
	m := clusterAuthMember(frame, n)
	if frame.LdapAuth {
		if err := a.applyDirectoryAuthStep(ctx, st, m, doc, a.containerOf(st.ID, n.ID), "ps", sec.RootPassword, step, pr); err != nil {
			pr.logln("directory authentication skipped: " + err.Error())
		}
	}
	if frame.EnableOIDC {
		step.SQL = true
		if err := a.applyMySQLOIDCStep(ctx, st, m, doc, a.containerOf(st.ID, n.ID), sec, step, pr); err != nil {
			pr.logln("Keycloak OIDC skipped: " + err.Error())
		}
	}
}

// pxcStageAuth writes the cluster's live logins into a PXC joiner's options before its first
// start, so it joins with the plugins loaded and no restart.
func (a *App) pxcStageAuth(ctx context.Context, st Stack, frame designFrame, doc designDoc, n designNode, sec pxcSecrets, pr *pxcProg) {
	if !clusterAuthFrame(frame) || n.Role == "arbitrator" {
		return
	}
	ldap, oidc := a.clusterAuthLive(st, doc, frame, n.ID)
	step := mysqlAuthStep{Unit: "mysql", Scope: "galera"}
	m := clusterAuthMember(frame, n)
	if ldap && frame.LdapAuth {
		if err := a.applyDirectoryAuthStep(ctx, st, m, doc, a.containerOf(st.ID, n.ID), "ps", sec.RootPassword, step, pr); err != nil {
			pr.logln("directory authentication skipped: " + err.Error())
		}
	}
	if oidc && frame.EnableOIDC {
		if err := a.applyMySQLOIDCStep(ctx, st, m, doc, a.containerOf(st.ID, n.ID), sec, step, pr); err != nil {
			pr.logln("Keycloak OIDC skipped: " + err.Error())
		}
	}
}

// frameAuthIssues validates a replication or PXC frame's login choices: a directory and an
// SSL Keycloak picked, and a server series that carries each plugin.
func frameAuthIssues(f designFrame, dirNodes map[string]string, keycloakIDs, keycloakSSL map[string]bool) []issue {
	var out []issue
	label, product, major, version := "MySQL replication cluster "+f.Label, "Percona Server", psMajorOf(f.PSMajor), f.PSVersion
	if f.Type == "pxc" {
		label, product, major, version = "PXC cluster "+f.Label, "Percona XtraDB Cluster", frameMajor(f), f.PXCVersion
		if version == "" {
			// "Latest" is whatever this installation can install, and PXC's 9.7 series still
			// trails the release that added auth_openid_connect — so look rather than assume.
			version = catalogNewest("percona_xtradb_cluster", f.OS, f.OSVersion, f.Arch, major)
		}
	}
	if f.LdapAuth {
		if _, ok := dirNodes[f.LdapDirNodeID]; !ok {
			out = append(out, issue{Level: "error", Message: label + " has LDAP integration enabled but no directory is selected — add an Intranet or Samba AD DC node and pick it"})
		}
		if major == "5.7" {
			out = append(out, issue{Level: "error", Message: label + ": Percona Server 5.7 has no authentication_ldap_simple — choose 8.0 or newer, or turn LDAP off"})
		}
	}
	if f.EnableOIDC {
		switch ok, min := mysqlOIDCSupported(major, version); {
		case !keycloakIDs[f.KeycloakNodeID]:
			out = append(out, issue{Level: "error", Message: label + " has Keycloak SSO enabled but is not linked to a Keycloak node — add a Keycloak node and select it"})
		case !keycloakSSL[f.KeycloakNodeID]:
			out = append(out, issue{Level: "error", Message: label + " uses Keycloak OIDC, which requires an HTTPS issuer — enable \"Use Intranet CA SSL\" on the Keycloak node"})
		case min == "":
			out = append(out, issue{Level: "error", Message: label + " uses Keycloak OIDC (auth_openid_connect), which Percona ships from 8.4.11-11 and 9.7.2-2 on — " + product + " " + major + " does not carry the plugin; set the major to 8.4 or 9.7"})
		case !ok:
			out = append(out, issue{Level: "error", Message: label + " runs " + product + " " + version + ", which predates auth_openid_connect — choose " + min + " or newer"})
		}
	}
	return out
}

// catalogNewest is the newest build of a series this installation offers for an image, or ""
// when it offers none — the version a design's empty "latest" minor resolves to at deploy.
func catalogNewest(section, os, osVer, arch, major string) string {
	for _, img := range loadImageCatalog(section) {
		if img.OS == os && img.OSVersion == osVer && (arch == "" || img.Arch == archOr(arch)) {
			if vs := img.Versions[major]; len(vs) > 0 {
				return vs[0]
			}
		}
	}
	return ""
}
