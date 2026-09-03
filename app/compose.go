package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// compose.go — build a stack from a short description instead of a canvas document.
//
// THE PROBLEM THIS SOLVES. A stack's design is one `designDoc`: nodes carrying about
// 120 type-discriminated fields, cluster frames carrying their own set, x/y/w/h
// coordinates, and cross-references by generated id (`pmmNodeId`, `ldapDirNodeId`,
// `frameId`). That is the right shape for a canvas the browser draws and the wrong
// shape for a person at a terminal. Before this file, "three PXC 8.4.5 nodes on EL8, a
// Percona Server 8.0.45 on EL9 with LDAP, and a PMM monitoring both" could be had two
// ways: find a template that happens to be it, or hand-author the JSON — which means
// knowing that the package is `8.4.5-5.1` on Oracle Linux and `8.4.5-5-1` on Ubuntu,
// that a cluster is a frame plus members whose `Type` equals the frame's, and that
// monitoring is a field holding another node's id rather than a line on the canvas.
//
// So: a spec that says what you want, and a resolver that works out the rest.
//
//	{"nodes": [
//	  {"kind":"pxc", "count":3, "version":"8.4.5", "os":"el8", "monitor":true},
//	  {"kind":"ps",  "version":"8.0.45", "os":"el9", "ldap":true, "monitor":true},
//	  {"kind":"pmm", "version":"3"}
//	]}
//
// WHAT IT DELIBERATELY DOES NOT DO. It is not a second way to express everything the
// canvas can express — that would be a second design format to keep in step with the
// first. It covers the standalone engines and the cluster shapes whose wiring is
// uniform (see composeKinds), and refuses the rest by name with the reason, because a
// sharded PSMDB's shard/config/mongos roles or an All-in-One's per-instance list are
// exactly the things a terse spec would get subtly and silently wrong. For those,
// start from a template and `PUT` the design.

// ------------------------------------------------------------- the spec

// composeSpec is the request body of POST /api/stacks/compose.
type composeSpec struct {
	Name string `json:"name"`
	TTL  string `json:"ttl"`
	// DryRun resolves and validates without creating anything, so a caller can show
	// the plan — which versions it settled on, what it added for you — before
	// committing. This is what `dbcanvas stack compose --dry-run` prints.
	DryRun bool              `json:"dryRun"`
	Nodes  []composeNodeSpec `json:"nodes"`
}

// composeNodeSpec is one entry. The zero value of every option means "the default the
// designer would have given you", so a spec stays as short as the thing being asked
// for.
type composeNodeSpec struct {
	Kind string `json:"kind"`
	// Name is the label, and the base for generated ids. "" derives one from the
	// kind ("ps-01", "pxc-cluster-01"), numbered when a kind appears twice.
	Name string `json:"name"`
	// Count is cluster members for a frame kind, or how many copies of a standalone
	// node to create. 0 → the kind's default (3 for a cluster, 1 otherwise).
	Count int `json:"count"`
	// Version accepts what a person would type: a full package version
	// ("8.4.5-5.1"), the upstream release ("8.4.5"), a series ("8.4"), or "" for the
	// newest this installation can install. Resolved against versions.yaml for the
	// chosen OS — which is the point, since the package suffix differs by OS family.
	Version string `json:"version"`
	// OS accepts "el8", "ol9", "oraclelinux:8", "ubuntu24.04", "noble", "debian12"…
	// See osAliases. "" → the compose default (Oracle Linux 9).
	OS   string `json:"os"`
	Arch string `json:"arch"`

	// The relationships. Each is a boolean that resolves to a reference to another
	// node in the same spec, and each has a "…With" partner for naming which one when
	// the spec has more than one candidate. See composeLinks for what provides what.
	Monitor          bool   `json:"monitor"`          // register with a PMM node
	MonitorWith      string `json:"monitorWith"`      //
	LDAP             bool   `json:"ldap"`             // authenticate against a directory
	LDAPWith         string `json:"ldapWith"`         //
	OIDC             bool   `json:"oidc"`             // single sign-on through Keycloak
	OIDCWith         string `json:"oidcWith"`         //
	Kerberos         bool   `json:"kerberos"`         // GSSAPI against a Samba AD DC
	KerberosWith     string `json:"kerberosWith"`     //
	Vault            bool   `json:"vault"`            // data-at-rest keys in OpenBao
	VaultWith        string `json:"vaultWith"`        //
	Backup           bool   `json:"backup"`           // back up to a SeaweedFS S3 node
	BackupWith       string `json:"backupWith"`       //
	Orchestrator     bool   `json:"orchestrator"`     // topology discovery + failover
	OrchestratorWith string `json:"orchestratorWith"` //

	// To names the node or cluster this one associates with on the canvas — the
	// backend a proxy fronts, the database a simulator drives. Omit it and compose
	// picks the single legal target if there is exactly one, and says which it chose;
	// with none or several it refuses and names them.
	To string `json:"to"`

	// Resource shaping. Nothing else in compose reaches these, and they are close to
	// the point of the product: a slow disk or a lossy link between Galera members is
	// the thing you came here to reproduce, and it is not reproducible by choosing a
	// version. Applied per node; see netem.go and the blkio limits in vagrant.go.
	//
	// NetLatencyMS and NetLossPct are the two that produce Galera flow control and,
	// past evs.suspect_timeout, eviction. NetAllTraffic widens the shaping from the
	// node's own database ports to every packet it sends — which models a bad NIC
	// rather than a bad link, and can make the node fail its own provisioning.
	NetLatencyMS    int     `json:"netLatencyMs"`
	NetJitterMS     int     `json:"netJitterMs"`
	NetLossPct      float64 `json:"netLossPct"`
	NetRateMbit     int     `json:"netRateMbit"`
	NetAllTraffic   bool    `json:"netAllTraffic"`
	DeviceReadMBps  int     `json:"deviceReadMbps"`
	DeviceWriteMBps int     `json:"deviceWriteMbps"`

	// Per-engine behaviour, each of which selects a genuinely different topology or
	// mode rather than a tuning detail.
	ReplMode    string   `json:"replMode"`    // mysql: async|semisync · innodb: innodbcluster|groupreplication
	Mode        string   `json:"mode"`        // proxysql: singlewrite|loadbal
	Setup       string   `json:"setup"`       // psmdb: standard|minimum
	MySQLRouter bool     `json:"mysqlRouter"` // innodb frames: run MySQL Router on each member
	Buckets     []string `json:"buckets"`     // seaweedfs: 1-10 bucket names
	TLS         bool     `json:"tls"`         // seaweedfs: serve S3 over HTTPS
	AlertEmail  string   `json:"alertEmail"`  // orchestrator: mailbox for failure alerts
	Dataset     string   `json:"dataset"`     // marketchaos: small|medium|large
	CertTTL     string   `json:"certTtl"`     // "365d", "30m", "2h" — short ones expire on purpose

	Export     bool `json:"export"`     // publish the database port to the host
	ExportPort int  `json:"exportPort"` // 0 → Docker picks a free one
	Cert       bool `json:"cert"`       // mint TLS certificates from the Intranet CA
	Proxy      bool `json:"proxy"`      // package downloads via the Intranet Squid proxy
	// GTID is a pointer because its default is *on* for the engines that have it, so
	// "unset" and "false" have to be distinguishable.
	GTID     *bool `json:"gtid"`
	CPUs     int   `json:"cpus"`
	MemoryGB int   `json:"memoryGb"`
}

// link reads one relationship by name, so the wiring pass can be table-driven while
// the JSON stays explicit — a map would have made the API's shape undiscoverable.
func (s composeNodeSpec) link(option string) (bool, string) {
	switch option {
	case "monitor":
		return s.Monitor, s.MonitorWith
	case "ldap":
		return s.LDAP, s.LDAPWith
	case "oidc":
		return s.OIDC, s.OIDCWith
	case "kerberos":
		return s.Kerberos, s.KerberosWith
	case "vault":
		return s.Vault, s.VaultWith
	case "backup":
		return s.Backup, s.BackupWith
	case "orchestrator":
		return s.Orchestrator, s.OrchestratorWith
	}
	return false, ""
}

// composeResult is what comes back: what was built, what it resolved to, and what it
// decided on the caller's behalf.
type composeResult struct {
	Stack    *Stack            `json:"stack,omitempty"` // nil on a dry run
	Design   json.RawMessage   `json:"design"`
	Resolved []composeResolved `json:"resolved"`
	Added    []string          `json:"added,omitempty"` // things compose added itself
	Issues   []issue           `json:"issues"`
	OK       bool              `json:"ok"`
}

// composeResolved reports, per entry, what a short version string became. A caller
// that asked for "8.4.5" wants to be told it got 8.4.5-5.1 on oraclelinux 8 — that is
// the answer to "which build am I actually reproducing on".
type composeResolved struct {
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	NodeIDs   []string `json:"nodeIds"`
	FrameID   string   `json:"frameId,omitempty"`
	OS        string   `json:"os"`
	OSVersion string   `json:"osVersion"`
	Arch      string   `json:"arch"`
	Major     string   `json:"major,omitempty"`
	Version   string   `json:"version,omitempty"`
	// Links is what compose wired up, as "option→provider" — "monitor→pmm-01",
	// "oidc→keycloak-01". One field rather than one per relationship, so adding an
	// eighth does not change the response shape.
	Links []string `json:"links,omitempty"`
}

// ------------------------------------------------------------- OS aliases

// osAliases maps what people type to the (family, release) pair a design needs.
//
// "el8" is in here because that is what the platform is called in every bug report,
// release note and support ticket this app exists to reproduce — nobody writes
// "oraclelinux" with a separate "8" unless a form makes them.
var osAliases = map[string][2]string{
	"el8": {"oraclelinux", "8"}, "ol8": {"oraclelinux", "8"}, "oel8": {"oraclelinux", "8"},
	"rhel8": {"oraclelinux", "8"}, "oraclelinux8": {"oraclelinux", "8"}, "oraclelinux:8": {"oraclelinux", "8"},
	"el9": {"oraclelinux", "9"}, "ol9": {"oraclelinux", "9"}, "oel9": {"oraclelinux", "9"},
	"rhel9": {"oraclelinux", "9"}, "oraclelinux9": {"oraclelinux", "9"}, "oraclelinux:9": {"oraclelinux", "9"},
	"el10": {"oraclelinux", "10"}, "ol10": {"oraclelinux", "10"},
	"rhel10": {"oraclelinux", "10"}, "oraclelinux10": {"oraclelinux", "10"}, "oraclelinux:10": {"oraclelinux", "10"},

	"ubuntu22": {"ubuntu", "22.04"}, "ubuntu22.04": {"ubuntu", "22.04"},
	"ubuntu:22.04": {"ubuntu", "22.04"}, "jammy": {"ubuntu", "22.04"},
	"ubuntu24": {"ubuntu", "24.04"}, "ubuntu24.04": {"ubuntu", "24.04"},
	"ubuntu:24.04": {"ubuntu", "24.04"}, "noble": {"ubuntu", "24.04"},

	"debian12": {"debian", "12"}, "debian:12": {"debian", "12"}, "bookworm": {"debian", "12"},
	"debian13": {"debian", "13"}, "debian:13": {"debian", "13"}, "trixie": {"debian", "13"},
}

// composeDefaultOS is what an entry with no `os` gets. Oracle Linux 9 because it is
// the most broadly stocked image in the catalogue and what most templates use, so a
// spec that says nothing about the OS is least likely to hit a version that is not
// published.
var composeDefaultOS = [2]string{"oraclelinux", "9"}

// resolveOS turns a written OS into a family/release pair.
func resolveOS(s string) (string, string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return composeDefaultOS[0], composeDefaultOS[1], nil
	}
	if v, ok := osAliases[s]; ok {
		return v[0], v[1], nil
	}
	// "family/release" and "family release" as a last resort, so an exact pair from
	// the catalogue always works even if it is not aliased.
	for _, sep := range []string{"/", " "} {
		if i := strings.Index(s, sep); i > 0 {
			return s[:i], s[i+1:], nil
		}
	}
	keys := make([]string, 0, len(osAliases))
	for k := range osAliases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "", "", fmt.Errorf("unknown os %q — try one of %s, or \"family/release\"",
		s, strings.Join(keys, ", "))
}

// ------------------------------------------------------------- kinds

// composeLink is a node-to-node relationship a spec expresses as a boolean.
//
// These are the hard part of writing a design by hand — each one is a field holding
// another node's GENERATED id, so you cannot write it until something has invented
// the ids. They were `monitor` and `ldap` as two hand-written special cases, which
// was fine until it turned out that Keycloak SSO, Kerberos, OpenBao keyrings, S3
// backups and Orchestrator are all the same shape and none of them were reachable.
// One table, seven rows, and an eighth is a row rather than a branch.
type composeLink struct {
	Option string // what the spec writes: "monitor", "oidc", …
	// Provides names the kinds that can satisfy it. More than one where the choice
	// is real: a directory is the Intranet's OpenLDAP or a Samba AD DC.
	Provides []string
	// Apply writes the reference. A cluster's relationship usually lives on the
	// FRAME and a standalone node's on the node, so both are offered and each row
	// uses whichever it has.
	Apply func(providerID string, n *designNode, f *designFrame)
	// Missing is what to tell somebody who asked for it with no provider in the spec.
	Missing string
}

var composeLinks = []composeLink{
	{Option: "monitor", Provides: []string{"pmm"},
		Missing: `add {"kind":"pmm"} to the spec`,
		Apply: func(id string, n *designNode, f *designFrame) {
			if f != nil {
				f.PMMNodeID = id
			} else {
				n.PMMNodeID = id
			}
		}},
	{Option: "ldap", Provides: []string{"intranet", "sambaad"},
		Missing: `add {"kind":"intranet"} or {"kind":"sambaad"}`,
		Apply: func(id string, n *designNode, f *designFrame) {
			if n != nil {
				n.LdapAuth, n.LdapDirNodeID = true, id
			} else {
				f.UseLDAP = true // valkeycluster wires its members from the frame
			}
		}},
	{Option: "oidc", Provides: []string{"keycloak"},
		Missing: `add {"kind":"keycloak"} to the spec`,
		Apply: func(id string, n *designNode, f *designFrame) {
			if n != nil {
				n.EnableOIDC, n.KeycloakNodeID = true, id
			}
		}},
	{Option: "kerberos", Provides: []string{"sambaad"},
		Missing: `Kerberos needs a directory that issues tickets — add {"kind":"sambaad"}`,
		Apply: func(id string, n *designNode, f *designFrame) {
			if n != nil {
				// GSSAPI is layered on the same directory binding, so the node has to
				// be pointed at it as well.
				n.KerberosAuth, n.LdapDirNodeID = true, id
			}
		}},
	{Option: "vault", Provides: []string{"openbao"},
		Missing: `add {"kind":"openbao"} to the spec`,
		Apply: func(id string, n *designNode, f *designFrame) {
			if n != nil {
				n.EnableVault, n.OpenBaoNodeID = true, id
			}
		}},
	{Option: "backup", Provides: []string{"seaweedfs"},
		Missing: `backups go to an S3 store — add {"kind":"seaweedfs"}`,
		Apply: func(id string, n *designNode, f *designFrame) {
			// The flag differs per engine — pgBackRest, Barman and PBM are three
			// different tools — but the S3 target is the same field on all of them.
			if f != nil {
				f.SeaweedFSNodeID = id
				switch f.Type {
				case "patroni":
					f.UsePgBackRest = true
				case "repmgr":
					f.UseBarman = true
				case "psmrs", "psmdb":
					f.EnablePBM = true
				}
				return
			}
			n.UsePgBackRest, n.SeaweedFSNodeID = true, id
		}},
	{Option: "orchestrator", Provides: []string{"orchestrator"},
		Missing: `add {"kind":"orchestrator"} to the spec`,
		Apply: func(id string, n *designNode, f *designFrame) {
			if f != nil {
				f.OrchestratorNodeID = id
			}
		}},
}

func composeLinkByOption(option string) (composeLink, bool) {
	for _, l := range composeLinks {
		if l.Option == option {
			return l, true
		}
	}
	return composeLink{}, false
}

// composeKind describes one thing a spec can ask for.
//
// A table rather than a switch for the same reason api_routes.go is one: the set is
// long, every entry needs the same handful of facts, and a missing fact should be
// visible as a hole in a row rather than a branch somebody forgot to write.
type composeKind struct {
	Kind string // what the spec writes
	Type string // designNode.Type, and designFrame.Type for a cluster
	// Frame means a cluster: one designFrame plus Count member nodes whose Type
	// equals the frame's (which is uniformly true — see the memberType switch in
	// intranet.go).
	Frame      bool
	Members    int // default member count
	MinMembers int
	MaxMembers int
	// Catalog is the versions.yaml section that lists what is installable, "" for a
	// kind that is a pulled image or has no version choice.
	Catalog string
	// SetVersion writes the resolved major/minor into whichever pair of fields this
	// engine uses. A closure rather than field names as strings, so a renamed field
	// is a compile error rather than a silently ignored design.
	SetVersion func(major, minor string, n *designNode, f *designFrame)
	// Roles assigns each member's Role. nil → none.
	Roles func(i, of int) string
	// ImageOnly means the kind runs a pulled or pre-baked image, so it has no OS to
	// choose — PMM, SeaweedFS, Keycloak, OpenBao, Watchtower and the Intranet all
	// ignore the field. Carrying a resolved OS on one of these puts noise in the
	// design and a lie in the plan table ("pmm-01 … oraclelinux 9"), so it is
	// cleared. The built-in templates do the same: their pmm and seaweedfs nodes
	// carry no os key at all.
	ImageOnly bool
	// PinOS fixes the OS for a kind that only works on one, so a spec cannot compose
	// something the validator will immediately reject. OpenBao installs from EPEL,
	// which is wired up for Oracle Linux 9 alone; the VNC desktop is a pre-baked
	// Ubuntu 24.04 image.
	PinOS [2]string
	// Singleton kinds the validator allows exactly one of. Compose refuses a second
	// here rather than composing a design that cannot deploy.
	Singleton bool
	// EdgeTo names the kinds this one associates with by a line ON THE CANVAS, as
	// opposed to a field on a node. Non-empty means the node does not function without
	// one: the provisioners find a ProxySQL's backend and a simulator's database by
	// walking the edge graph (backendFrameForProxySQL, haproxyClusterFrames), never by
	// reading a field.
	//
	// This was the largest hole in compose. It emitted `"edges": []` for every stack,
	// so a composed ProxySQL had no backend, an HAProxy could not even validate
	// (haproxyBackend requires exactly one associated cluster), and every app simulator
	// came up with nothing to drive.
	//
	// Targets are per kind because they genuinely differ — an Airline Sim speaks MySQL,
	// a Car Rental Sim speaks PostgreSQL — and the lists here mirror the validator's
	// own rules in app/intranet.go.
	EdgeTo []string
	// NoSizing marks a kind that never calls applyVMSize, so cpus/memoryGb would be
	// silently ignored on it. Refused instead — the audit found nine such types
	// quietly accepting both.
	NoSizing bool
	// CanShape marks a kind netemPortsFor (netem.go) knows ports for. Traffic shaping
	// on anything else is a no-op, so it is refused rather than accepted.
	CanShape bool
	// PDPSRepo marks the one kind whose "version" is not a major/minor pair from the
	// version catalogue but a percona-release repo name (pdps-84-lts, pdps-9.7.1) —
	// which is what innodb.go's `enable` verb actually takes. Resolved against
	// loadPDPSCatalog() instead of the section catalogue.
	PDPSRepo bool
	// Topology, for the one frame whose members are not "N of the same thing". A
	// sharded PSMDB cluster is exactly one mongos, a config replica set and three
	// shard replica sets, each member carrying the role and shard index that
	// provisionMongoDBFrame selects on. "count" is meaningless there — the size comes
	// from "setup" — so this returns the whole list and the count option is refused.
	Topology func(setup string) []composeMember
	// Scalars are the per-engine options this kind accepts beyond the universal ones
	// — replMode, mode, setup, mysqlRouter, buckets, tls, alertEmail, dataset. A list
	// rather than eight more booleans, for the same reason Links is one.
	Scalars []string
	// Links are the relationships this kind supports, by option name. Asking for one
	// that is not here is an error naming the kind, rather than a flag that quietly
	// does nothing.
	Links     []string
	CanExport bool
	CanCert   bool
	CanGTID   bool
	About     string
}

// unsupportedKinds are the canvas features compose deliberately does not model, with
// the reason. Listed rather than omitted so the error can say why and what to do
// instead — "unknown kind" would be a lie.
var unsupportedKinds = map[string]string{
	"k3d":         "a Kubernetes frame needs an operator choice, node budget and expose tiers",
	"aio":         "an All-in-One node is a list of per-instance configurations",
	"stocksim":    "the Stock Market Sim has three connection modes and a dozen lab knobs",
	"linuxclient": "a core-dump host needs host directory paths and the crashed build's version",
}

var composeKinds = []composeKind{
	{Kind: "intranet", Type: "intranet", NoSizing: true, Singleton: true, ImageOnly: true,
		About: "DNS, mail, OpenLDAP, a Squid proxy and the certificate authority. Every stack needs one; compose adds it for you."},

	{Kind: "pmm", Type: "pmm", NoSizing: true, ImageOnly: true, Catalog: "pmm", CanCert: true,
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { n.Version = minor },
		Links:      []string{"oidc"},
		About:      "A PMM server. Point other nodes at it with \"monitor\": true."},

	// --- standalone engines -------------------------------------------------
	{Kind: "ps", Type: "ps", CanShape: true, Catalog: "percona_server",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { n.PSMajor, n.PSVersion = major, minor }, CanExport: true, CanCert: true, CanGTID: true,
		Links: []string{"monitor", "ldap", "oidc", "vault"},
		About: "Percona Server for MySQL, standalone."},
	{Kind: "pg", Type: "pg", CanShape: true, Catalog: "percona_postgresql",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { n.PGMajor, n.PGVersion = major, minor }, CanExport: true, CanCert: true,
		Links: []string{"monitor", "ldap", "oidc", "kerberos", "backup"},
		About: "Percona Distribution for PostgreSQL, standalone."},
	{Kind: "psm", Type: "psm", CanShape: true, Catalog: "percona_server_mongodb",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { n.PSMDBMajor, n.PSMDBVersion = major, minor }, CanExport: true, CanCert: true,
		Links: []string{"monitor", "ldap", "oidc", "kerberos", "vault"},
		About: "Percona Server for MongoDB, standalone."},
	{Kind: "mariadb", Type: "mariadb", CanShape: true, Catalog: "mariadb",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) {
			n.MariaDBMajor, n.MariaDBVersion = major, minor
		}, CanExport: true, CanCert: true, CanGTID: true,
		Links: []string{"monitor"},
		About: "MariaDB Server, standalone."},
	{Kind: "mysql", Type: "mysqlce", CanShape: true, Catalog: "mysql_community",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) {
			n.MySQLCEMajor, n.MySQLCEVersion = major, minor
		}, CanExport: true, CanCert: true, CanGTID: true,
		Links: []string{"monitor"},
		About: "MySQL Community Server, standalone."},
	{Kind: "valkey", Type: "valkey", CanShape: true, Catalog: "percona_valkey",
		// Only the version: valkey.go reads ValkeyVersion and nothing reads
		// ValkeyMajor, on either struct. Writing it would look like it mattered.
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { n.ValkeyVersion = minor },
		CanExport:  true,
		Links:      []string{"monitor", "ldap"},
		About:      "Valkey, standalone."},

	// --- infrastructure -----------------------------------------------------
	{Kind: "proxysql", Type: "proxysql", CanShape: true, Scalars: []string{"mode"}, Catalog: "proxysql",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) {
			n.ProxySQLMajor, n.ProxySQLVersion = major, minor
		}, CanExport: true,
		Links:  []string{"monitor"},
		EdgeTo: []string{"pxc", "ps-repl"},
		About:  "ProxySQL, in front of a PXC or Percona Server replication cluster."},
	{Kind: "orchestrator", Type: "orchestrator", Scalars: []string{"alertEmail"}, Catalog: "percona_orchestrator",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { n.OrchestratorVersion = minor },
		CanExport:  true,
		About:      "Percona Orchestrator, for MySQL replication topologies."},
	{Kind: "haproxy", Type: "haproxy", CanShape: true, CanExport: true,
		// Exactly one: haproxyClusterFrames treats two as ambiguous, not as two pools.
		EdgeTo: []string{"patroni", "repmgr", "spock", "pxc", "ps-repl"},
		About:  "HAProxy, in front of one Patroni, repmgr, Spock, PXC or PS replication cluster."},
	{Kind: "seaweedfs", Type: "seaweedfs", NoSizing: true, Scalars: []string{"buckets", "tls"}, ImageOnly: true, CanCert: true,
		About: "SeaweedFS S3, as a backup target."},
	{Kind: "keycloak", Type: "keycloak", NoSizing: true, Singleton: true, ImageOnly: true, About: "Keycloak, as an OIDC identity provider."},
	{Kind: "openbao", Type: "openbao", NoSizing: true, Singleton: true, PinOS: [2]string{"oraclelinux", "9"}, About: "OpenBao, for data-at-rest encryption keys."},
	{Kind: "sambaad", Type: "sambaad", NoSizing: true, About: "Samba AD DC, as a directory and Kerberos realm."},
	{Kind: "vnc", Type: "vnc", NoSizing: true, Singleton: true, PinOS: [2]string{"ubuntu", "24.04"}, About: "An Ubuntu desktop with a browser and the database clients."},
	{Kind: "watchtower", Type: "watchtower", NoSizing: true, Singleton: true, ImageOnly: true, About: "Watchtower, for in-app PMM upgrades."},

	// --- clusters -----------------------------------------------------------
	{Kind: "pxc", Type: "pxc", CanShape: true, Frame: true, Members: 3, MinMembers: 1, MaxMembers: 9,
		Catalog:    "percona_xtradb_cluster",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.PXCMajor, f.PXCVersion = major, minor },
		Roles:      func(i, of int) string { return "regular" }, CanCert: true, CanGTID: true, CanExport: true,
		Links: []string{"monitor"},
		About: "Percona XtraDB Cluster. \"count\" is the member count (default 3)."},
	{Kind: "ps-repl", Type: "mysql", CanShape: true, Scalars: []string{"replMode"}, Frame: true, Members: 3, MinMembers: 2, MaxMembers: 9,
		Catalog:    "percona_server",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.PSMajor, f.PSVersion = major, minor },
		Roles:      primaryFirst, CanCert: true, CanGTID: true, CanExport: true,
		Links: []string{"monitor", "orchestrator"},
		About: "Percona Server asynchronous replication."},
	{Kind: "patroni", Type: "patroni", CanShape: true, Frame: true, Members: 3, MinMembers: 2, MaxMembers: 7,
		Catalog:    "percona_postgresql",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.PGMajor, f.PGVersion = major, minor }, CanCert: true,
		Links: []string{"monitor", "backup"},
		About: "PostgreSQL HA with Patroni and etcd."},
	{Kind: "repmgr", Type: "repmgr", CanShape: true, Frame: true, Members: 3, MinMembers: 2, MaxMembers: 7,
		Catalog:    "percona_postgresql",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.PGMajor, f.PGVersion = major, minor }, CanCert: true,
		Links: []string{"monitor", "backup"},
		About: "PostgreSQL streaming replication with repmgr."},
	{Kind: "psmrs", Type: "psmrs", CanShape: true, Frame: true, Members: 3, MinMembers: 1, MaxMembers: 9,
		Catalog:    "percona_server_mongodb",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.PSMDBMajor, f.PSMDBVersion = major, minor }, CanCert: true,
		Links: []string{"monitor", "backup"},
		About: "A Percona Server for MongoDB replica set."},
	{Kind: "valkey-cluster", Type: "valkeycluster", CanShape: true, Frame: true, Members: 3, MinMembers: 3, MaxMembers: 7,
		Catalog:    "percona_valkey",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.ValkeyVersion = minor },
		Links:      []string{"monitor", "ldap"},
		About:      "A Valkey cluster of all-master shards."},
	{Kind: "mariadb-galera", Type: "mariadbgalera", CanShape: true, Frame: true, Members: 3, MinMembers: 3, MaxMembers: 9,
		Catalog: "mariadb",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) {
			f.MariaDBMajor, f.MariaDBVersion = major, minor
		}, CanCert: true,
		Links: []string{"monitor"},
		About: "MariaDB Galera Cluster."},
	{Kind: "mariadb-repl", Type: "mariadbrepl", CanShape: true, Scalars: []string{"replMode"}, Frame: true, Members: 3, MinMembers: 2, MaxMembers: 9,
		Catalog: "mariadb",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) {
			f.MariaDBMajor, f.MariaDBVersion = major, minor
		},
		Roles: primaryFirst, CanCert: true, CanGTID: true,
		Links: []string{"monitor", "orchestrator"},
		About: "MariaDB asynchronous replication."},
	{Kind: "mysql-repl", Type: "mysqlcerepl", CanShape: true, Scalars: []string{"replMode"}, Frame: true, Members: 3, MinMembers: 2, MaxMembers: 9,
		Catalog: "mysql_community",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) {
			f.MySQLCEMajor, f.MySQLCEVersion = major, minor
		},
		Roles: primaryFirst, CanCert: true, CanGTID: true,
		Links: []string{"monitor", "orchestrator"},
		About: "MySQL Community asynchronous replication."},
	{Kind: "innodb", Type: "innodb", CanShape: true, Scalars: []string{"replMode", "mysqlRouter"}, Frame: true, Members: 3, MinMembers: 1, MaxMembers: 9,
		// Not a version pair: innodb.go installs from a percona-release repo name, so
		// version= takes one of those (pdps-84-lts, pdps-9.7.1). `dbcanvas versions`
		// lists them.
		PDPSRepo:   true,
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.PDPSRepo = minor },
		CanCert:    true, CanExport: true,
		Links: []string{"monitor"},
		About: "Percona InnoDB Cluster / Group Replication. replMode=innodbcluster|groupreplication."},
	{Kind: "mysql-innodb", Type: "mysqlceinnodb", CanShape: true, Scalars: []string{"replMode", "mysqlRouter"}, Frame: true, Members: 3, MinMembers: 3, MaxMembers: 9,
		Catalog: "mysql_community",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) {
			f.MySQLCEMajor, f.MySQLCEVersion = major, minor
		}, CanCert: true, CanGTID: true, CanExport: true,
		Links: []string{"monitor"},
		About: "MySQL Community InnoDB Cluster / Group Replication (3 or more members)."},
	{Kind: "spock", Type: "spock", CanShape: true, Frame: true, Members: 3, MinMembers: 2, MaxMembers: 7,
		Catalog:    "percona_postgresql",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.PGMajor, f.PGVersion = major, minor },
		CanCert:    true,
		Links:      []string{"monitor"},
		About:      "PostgreSQL multi-master with Spock."},
	{Kind: "proxysql-cluster", Type: "proxysql", CanShape: true, Scalars: []string{"mode"}, Frame: true, Members: 3, MinMembers: 1, MaxMembers: 9,
		Catalog: "proxysql",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) {
			f.ProxySQLMajor, f.ProxySQLVersion = major, minor
		},
		Links:  []string{"monitor"},
		EdgeTo: []string{"pxc", "ps-repl"},
		About:  "A ProxySQL cluster, in front of a PXC or Percona Server replication cluster."},
	{Kind: "psmdb", Type: "psmdb", CanShape: true, Scalars: []string{"setup"}, Frame: true,
		// No member count: the topology is fixed and comes from setup=. See
		// psmdbTopology, which mirrors what validateStack requires.
		Topology:   psmdbTopology,
		Catalog:    "percona_server_mongodb",
		SetVersion: func(major, minor string, n *designNode, f *designFrame) { f.PSMDBMajor, f.PSMDBVersion = major, minor },
		CanCert:    true,
		Links:      []string{"monitor", "backup"},
		About:      "A sharded PS MongoDB cluster. setup=standard (13 nodes) or minimum (5)."},

	// --- application simulators ---------------------------------------------
	//
	// Every one of these needs an association line to the database it drives; that is
	// an error in validateStack, not a warning, so EdgeTo is non-empty and compose
	// either resolves the target or refuses. The legal targets differ per sim because
	// each speaks one protocol, and the lists here are the validator's own.
	{Kind: "trafficsim", Type: "trafficsim", NoSizing: true, ImageOnly: true,
		EdgeTo: []string{"valkey", "valkey-cluster"},
		About:  "Traffic Sim — drives load at a Valkey node or cluster."},
	{Kind: "hotelsim", Type: "hotelsim", NoSizing: true, ImageOnly: true,
		EdgeTo: []string{"psm", "psmrs", "psmdb"},
		About:  "Hotel Sim — a booking workload on PS MongoDB."},
	{Kind: "airlinesim", Type: "airlinesim", NoSizing: true, ImageOnly: true,
		EdgeTo: []string{"ps", "ps-repl", "pxc", "proxysql", "haproxy"},
		About:  "Airline Sim — a reservation workload on MySQL."},
	{Kind: "carsim", Type: "carsim", NoSizing: true, ImageOnly: true,
		EdgeTo: []string{"pg", "patroni", "repmgr", "spock", "haproxy"},
		About:  "Car Rental Sim — a rental workload on PostgreSQL."},
	{Kind: "marketchaos", Type: "marketchaos", NoSizing: true, Scalars: []string{"dataset"}, ImageOnly: true,
		EdgeTo: []string{"ps", "pxc", "ps-repl", "haproxy"},
		About:  "MarketChaos — a trading workload on MySQL. dataset= picks the size."},
}

// composeMember is one member of a fixed-topology frame.
type composeMember struct {
	Suffix string // label suffix — the hostname's tail, so it must stay unique
	Role   string
	Shard  int
}

// psmdbTopology mirrors the shape validateStack demands of a sharded PSMDB frame
// (app/intranet.go): one mongos, a config replica set, and exactly three shards.
// "standard" is 3 config members and 3 members per shard (13 containers); "minimum"
// is 1 and 1 (5 containers), which is what fits on a laptop.
func psmdbTopology(setup string) []composeMember {
	cfg, per := 3, 3
	if setup == "minimum" {
		cfg, per = 1, 1
	}
	out := []composeMember{{Suffix: "mongos", Role: "mongos"}}
	for i := 1; i <= cfg; i++ {
		out = append(out, composeMember{Suffix: fmt.Sprintf("cfg%d", i), Role: "config"})
	}
	for sh := 0; sh < 3; sh++ {
		for r := 1; r <= per; r++ {
			out = append(out, composeMember{
				Suffix: fmt.Sprintf("s%dr%d", sh+1, r), Role: "shard", Shard: sh})
		}
	}
	return out
}

func primaryFirst(i, of int) string {
	if i == 0 {
		return "primary"
	}
	return "secondary"
}

// takes reports whether this kind accepts a per-engine scalar option.
func (k composeKind) takes(option string) bool {
	return slices.Contains(k.Scalars, option)
}

// supports reports whether this kind has a relationship.
func (k composeKind) supports(option string) bool {
	for _, l := range k.Links {
		if l == option {
			return true
		}
	}
	return false
}

func composeKindByName(name string) (composeKind, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, k := range composeKinds {
		if k.Kind == name {
			return k, true
		}
	}
	return composeKind{}, false
}

// ------------------------------------------------------------- version resolution

// resolveCatalogVersion turns what somebody typed into the (major, minor) pair a
// design needs, against what this installation can actually install.
//
// This is the single most useful thing compose does. The packaging suffix is not
// guessable — Percona Server 8.0.45 is `8.0.45-36.1` on Oracle Linux and
// `8.0.45-36-1` on Ubuntu — and the series a release belongs to is not either, since
// nothing about "8.4.5" says the major key is "8.4" until you look. A caller who
// writes the upstream release gets the right package; one who writes nothing gets the
// newest; one who writes something unavailable gets told what *is*.
func resolveCatalogVersion(images []PXCImage, os, osVer, arch, want string) (string, string, error) {
	var entry *PXCImage
	for i := range images {
		if images[i].OS == os && images[i].OSVersion == osVer &&
			(arch == "" || images[i].Arch == arch) {
			entry = &images[i]
			break
		}
	}
	if entry == nil {
		return "", "", fmt.Errorf("this installation has no %s %s image — run `make images`, "+
			"or see GET /api/catalog/images for what it does have", os, osVer)
	}
	if len(entry.Versions) == 0 {
		return "", "", fmt.Errorf("nothing installable on %s %s for this engine "+
			"(the catalogue is empty — has `make versions` run?)", os, osVer)
	}

	majors := make([]string, 0, len(entry.Versions))
	for m, vs := range entry.Versions {
		if len(vs) > 0 {
			majors = append(majors, m)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(majors)))
	if len(majors) == 0 {
		return "", "", fmt.Errorf("no series has any version on %s %s", os, osVer)
	}

	want = strings.TrimSpace(want)

	// Nothing asked for: the newest series, and its newest build. "" as the minor is
	// meaningful in a design — it means "latest at deploy time" — so it is kept
	// rather than pinned, which is what the designer's default does too.
	if want == "" {
		return majors[0], "", nil
	}

	// A whole series ("8.0", "8.4", "16"): that series, latest build.
	if vs, ok := entry.Versions[want]; ok && len(vs) > 0 {
		return want, "", nil
	}

	// Otherwise a specific build. Exact match wins; then a release prefix, where the
	// boundary has to be a separator so "8.0.4" cannot match "8.0.45".
	type hit struct{ major, version string }
	var exact, prefix []hit
	for _, m := range majors {
		for _, v := range entry.Versions[m] {
			switch {
			case v == want:
				exact = append(exact, hit{m, v})
			case strings.HasPrefix(v, want) && len(v) > len(want) &&
				(v[len(want)] == '-' || v[len(want)] == '.'):
				prefix = append(prefix, hit{m, v})
			}
		}
	}
	for _, set := range [][]hit{exact, prefix} {
		switch len(set) {
		case 1:
			return set[0].major, set[0].version, nil
		case 0:
			continue
		default:
			// Several builds of one release is normal (a rebuild); take the newest,
			// since the catalogue is newest-first within a series.
			return set[0].major, set[0].version, nil
		}
	}

	// Not available. The error is the useful part: say what is, in the series they
	// were evidently aiming at.
	series := want
	if i := strings.LastIndex(want, "."); i > 0 {
		series = want[:i]
	}
	if vs, ok := entry.Versions[series]; ok && len(vs) > 0 {
		show := vs
		if len(show) > 6 {
			show = show[:6]
		}
		return "", "", fmt.Errorf("no %q on %s %s. Available in %s: %s%s",
			want, os, osVer, series, strings.Join(show, ", "),
			map[bool]string{true: " …", false: ""}[len(vs) > len(show)])
	}
	return "", "", fmt.Errorf("no %q on %s %s. Series available there: %s "+
		"(see GET /api/catalog/… for the full list)",
		want, os, osVer, strings.Join(majors, ", "))
}

// composeCatalog loads a versions.yaml section by name.
func composeCatalog(section string) []PXCImage { return loadImageCatalog(section) }

// resolvePMMVersion is separate because PMM is a pulled image with a flat version
// list rather than a per-OS, per-series catalogue.
func resolvePMMVersion(want string) (string, error) {
	cat := loadPMMCatalog()
	want = strings.TrimSpace(want)
	if want == "" {
		return "", nil // "" → the catalog default tag
	}
	for _, v := range cat.Versions {
		if v == want {
			return v, nil
		}
	}
	// A bare major ("3") is the image tag PMM itself publishes, and the catalogue's
	// own default, so accept it as written.
	if want == "3" || want == cat.DefaultTag {
		return want, nil
	}
	// A release prefix: newest 3.9.x for "3.9".
	for _, v := range cat.Versions {
		if strings.HasPrefix(v, want+".") {
			return v, nil
		}
	}
	show := cat.Versions
	if len(show) > 8 {
		show = show[:8]
	}
	return "", fmt.Errorf("no PMM %q. Available: %s …", want, strings.Join(show, ", "))
}

// resolvePDPSRepo picks a percona-release repository for an InnoDB Cluster frame.
// Unlike every other engine this is not a major/minor pair from the version
// catalogue: innodb.go's `enable` verb takes a repo name, and the names are
// inconsistent by design (pdps-84-lts alongside pdps-9.7.1), so they are matched
// literally against the catalogue rather than parsed.
func resolvePDPSRepo(want string) (string, error) {
	repos := loadPDPSCatalog()
	want = strings.TrimSpace(want)
	if want == "" {
		return "", nil // "" → the provisioner's own default
	}
	if slices.Contains(repos, want) {
		return want, nil
	}
	// "8.4" or "84-lts" for pdps-84-lts: a bare version is what people type.
	for _, r := range repos {
		if strings.TrimPrefix(r, "pdps-") == want || r == "pdps-"+want {
			return r, nil
		}
	}
	if len(repos) == 0 {
		return "", fmt.Errorf("no PDPS repositories in versions.yaml")
	}
	return "", fmt.Errorf("no PDPS repository %q. Available: %s", want, strings.Join(repos, ", "))
}
