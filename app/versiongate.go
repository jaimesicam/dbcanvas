package main

import "strings"

// versiongate.go — refuse to deploy a database onto an OS image the version catalogue
// says cannot install it.
//
// A version field left empty means "latest at deploy time", so an empty field is not
// itself the problem: the problem is an image whose catalogue entry for the chosen series
// is an empty list. That is what versions.yaml records when the probe found nothing —
// Oracle Linux 8 recorded `percona_postgresql: {"16": []}` for months because AppStream's
// postgresql module hid the packages — and a deploy there would only fail inside dnf,
// minutes in, with "No match for argument". Validation says so up front instead.
//
// Only a catalogue entry that exists and is empty blocks. An image with no entry at all
// (the other architecture, which `make versions` does not probe, or a hand-built image) is
// left to the deploy, as it always was.

// dbVersionSpec is one database a node or cluster frame installs: its catalogue section,
// its series and the image it installs onto. A series the design leaves blank is resolved
// to the default the provisioner would install; "" is left only for the engines that have
// no series choice (Valkey, Orchestrator), and means any series will do.
type dbVersionSpec struct {
	Section, Product, Major string
	OS, OSVersion, Arch     string
}

// nodeDBVersionSpec is the database a standalone node installs, if it installs one from
// the per-image catalogue.
func nodeDBVersionSpec(n designNode) (dbVersionSpec, bool) {
	s := dbVersionSpec{OS: n.OS, OSVersion: n.OSVersion, Arch: n.Arch}
	switch n.Type {
	case "ps":
		s.Section, s.Product, s.Major = "percona_server", "Percona Server", psMajorOf(n.PSMajor)
	case "pg":
		s.Section, s.Product, s.Major = "percona_postgresql", "PostgreSQL", ppgMajorOf(n.PGMajor)
	case "psm":
		s.Section, s.Product, s.Major = "percona_server_mongodb", "PS MongoDB", majorOr(n.PSMDBMajor, "8.0")
	case "mariadb":
		s.Section, s.Product, s.Major = "mariadb", "MariaDB", mariadbMajorOf(n.MariaDBMajor)
	case "mysqlce":
		s.Section, s.Product, s.Major = "mysql_community", "MySQL Community", mysqlceMajorOf(n.MySQLCEMajor)
	case "valkey":
		s.Section, s.Product, s.Major = "percona_valkey", "Valkey", n.ValkeyMajor
	case "proxysql":
		if n.FrameID != "" {
			return s, false // a ProxySQL cluster member: the frame carries the version
		}
		s.Section, s.Product, s.Major = "proxysql", "ProxySQL", proxysqlMajorOf(n.ProxySQLMajor)
	case "orchestrator":
		s.Section, s.Product = "percona_orchestrator", "Orchestrator"
	default:
		return s, false
	}
	return s, true
}

// frameDBVersionSpec is the database a cluster frame's members install.
func frameDBVersionSpec(f designFrame) (dbVersionSpec, bool) {
	s := dbVersionSpec{OS: f.OS, OSVersion: f.OSVersion, Arch: f.Arch}
	switch f.Type {
	case "pxc":
		s.Section, s.Product, s.Major = "percona_xtradb_cluster", "PXC", majorOr(f.PXCMajor, "8.0")
	case "mysql":
		s.Section, s.Product, s.Major = "percona_server", "Percona Server", psMajorOf(f.PSMajor)
	case "patroni", "repmgr":
		s.Section, s.Product, s.Major = "percona_postgresql", "PostgreSQL", ppgMajorOf(f.PGMajor)
	case "spock":
		s.Section, s.Product, s.Major = "spock", "PostgreSQL (Spock)", ppgMajorOf(f.PGMajor)
	case "psmrs", "psmdb":
		s.Section, s.Product, s.Major = "percona_server_mongodb", "PS MongoDB", majorOr(f.PSMDBMajor, "8.0")
	case "valkeycluster":
		s.Section, s.Product, s.Major = "percona_valkey", "Valkey", f.ValkeyMajor
	case "mariadbgalera", "mariadbrepl":
		s.Section, s.Product, s.Major = "mariadb", "MariaDB", mariadbMajorOf(f.MariaDBMajor)
	case "mysqlcerepl", "mysqlceinnodb":
		s.Section, s.Product, s.Major = "mysql_community", "MySQL Community", mysqlceMajorOf(f.MySQLCEMajor)
	case "proxysql":
		s.Section, s.Product, s.Major = "proxysql", "ProxySQL", proxysqlMajorOf(f.ProxySQLMajor)
	default:
		return s, false
	}
	return s, true
}

// emptyCatalogReason returns why spec cannot be installed — "" when it can, or when the
// catalogue has no entry for the image and so cannot say.
func emptyCatalogReason(images []PXCImage, s dbVersionSpec) string {
	var entry *PXCImage
	for i := range images {
		if images[i].OS == s.OS && images[i].OSVersion == s.OSVersion &&
			(s.Arch == "" || images[i].Arch == s.Arch) {
			entry = &images[i]
			break
		}
	}
	if entry == nil {
		return ""
	}
	on := s.OS + " " + s.OSVersion
	if s.Arch != "" {
		on += " (" + s.Arch + ")"
	}
	if major := strings.TrimSpace(s.Major); major != "" {
		if len(entry.Versions[major]) == 0 {
			return "no " + s.Product + " " + major + " builds are available on " + on
		}
		return ""
	}
	for _, vs := range entry.Versions {
		if len(vs) > 0 {
			return ""
		}
	}
	return "no " + s.Product + " builds are available on " + on
}

// catalogVersionIssues reports every node, All-in-One package set and populated cluster
// frame whose database has no installable version on its image.
func catalogVersionIssues(doc designDoc) []issue {
	var out []issue
	cats := map[string][]PXCImage{}
	check := func(what string, s dbVersionSpec) {
		if s.OS == "" || s.OSVersion == "" {
			return
		}
		imgs, ok := cats[s.Section]
		if !ok {
			imgs = loadImageCatalog(s.Section)
			cats[s.Section] = imgs
		}
		if why := emptyCatalogReason(imgs, s); why != "" {
			out = append(out, issue{Level: "error", Message: what + ": " + why +
				" — the version catalogue is empty for it. Pick another OS or series, or run `make versions` to re-probe"})
		}
	}
	populated := map[string]bool{}
	for _, n := range doc.Nodes {
		if n.FrameID != "" {
			populated[n.FrameID] = true
		}
		if s, ok := nodeDBVersionSpec(n); ok && n.FrameID == "" {
			check("Node "+n.Label, s)
		}
		if n.Type == "aio" {
			for _, s := range aioDBVersionSpecs(n) {
				check("All-in-One "+n.Label, s)
			}
		}
	}
	for _, f := range doc.Frames {
		if !populated[f.ID] {
			continue
		}
		if s, ok := frameDBVersionSpec(f); ok {
			check("Cluster "+f.Label, s)
		}
	}
	return out
}

// majorOr is major, or def when it is blank — for the engines whose default series is
// written inline at the install site rather than in a *MajorOf helper.
func majorOr(major, def string) string {
	if m := strings.TrimSpace(major); m != "" {
		return m
	}
	return def
}

// aioDBVersionSpecs are the databases an All-in-One node installs, one per package set:
// every family but PostgreSQL shares a single node-level series, and PostgreSQL installs
// once per major its instances use. Each series is resolved the way the provisioner
// resolves it (aio_*.go), so the check covers exactly what the deploy would install.
func aioDBVersionSpecs(n designNode) []dbVersionSpec {
	var out []dbVersionSpec
	seen := map[string]bool{}
	add := func(section, product, major string) {
		key := section + "\x00" + major
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, dbVersionSpec{Section: section, Product: product, Major: major,
			OS: n.OS, OSVersion: n.OSVersion, Arch: n.Arch})
	}
	for _, in := range n.AIOInstances {
		switch flavor := aioMySQLFlavorOfKind(in.Kind); flavor {
		case flavorPS:
			add("percona_server", "Percona Server", psMajorOf(n.AIOPSMajor))
		case flavorPXC:
			m, _ := aioPXCMajor(n)
			add("percona_xtradb_cluster", "PXC", m)
		case flavorMariaDB:
			add("mariadb", "MariaDB", mariadbMajorOf(n.AIOMariaDBMajor))
		case flavorMySQLCE:
			add("mysql_community", "MySQL Community", mysqlceMajorOf(n.AIOMySQLCEMajor))
		}
		switch in.Kind {
		case "pg", "patroni", "repmgr":
			add("percona_postgresql", "PostgreSQL", ppgMajorOf(in.PGMajor))
		case "spock":
			add("spock", "PostgreSQL (Spock)", ppgMajorOf(in.PGMajor))
		case "psmdb", "psmrs", "psmdbsharded":
			add("percona_server_mongodb", "PS MongoDB", aioPSMDBMajor(n))
		case "valkey", "valkeycluster":
			add("percona_valkey", "Valkey", n.AIOValkeyMajor)
		case "proxysql":
			add("proxysql", "ProxySQL", majorOr(n.AIOProxySQLMajor, "2"))
		case "orchestrator":
			add("percona_orchestrator", "Orchestrator", "")
		}
	}
	return out
}
