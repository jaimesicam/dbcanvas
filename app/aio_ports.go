package main

import (
	"sort"
	"strconv"
	"strings"
)

// aio_ports.go — the All-in-One node's feature catalog and port allocator.
//
// An All-in-One node is ONE container running many database feature instances
// side by side. Two things follow from that and are decided here:
//
//   1. Nothing may listen on its product's default port. A stock Percona Server
//      wants 3306, a stock PostgreSQL 5432 — put two of either in one container
//      and the second fails to bind. Every instance therefore gets a private
//      *slot* of 10 consecutive ports, allocated per family, well away from the
//      defaults.
//   2. Which feature kinds exist, which family each belongs to, and how many
//      members each may have — the single source of truth the form, the
//      validator and the provisioners all read.
//
// The slot math is mirrored in web/src/lib/aioPorts.js so the designer can show
// the ports before a deploy exists. Change one, change the other; TestAIOPortsJS
// keeps them honest.

// ---------------------------------------------------------------- kinds

// AiO feature families. A family maps 1:1 onto "one package install serves every
// instance of it", which is why the version picker is per family, not per
// instance (see aioFamilyVersionShared).
const (
	famMySQL   = "mysql"
	famPG      = "postgres"
	famMongo   = "mongodb"
	famValkey  = "valkey"
	famProxy   = "proxysql"
	famHAProxy = "haproxy"
	famOrch    = "orchestrator"
)

// aioKind describes one feature a user can add to an All-in-One node.
type aioKind struct {
	Kind    string // the id stored in aioInstance.Kind
	Label   string // UI label
	Family  string // fam* above — decides packages, version sharing and port base
	Cluster bool   // true → the instance has Members>1 and gets one slot per member
	MinMem  int    // member count bounds (Cluster only)
	MaxMem  int
	DefMem  int
	OddOnly bool // member count must be odd (quorum-based topologies)
	// MemChoices, when set, is the ONLY member counts the kind accepts — a
	// fixed-topology kind, where a count in between would not describe a cluster
	// anyone would build. PSMDB Sharded is the case: its two counts are the two
	// setups the dedicated frame offers (see aioMongoShardedTopo).
	MemChoices []int
	// EstMemMB is a rough per-member resident footprint, used only to warn the
	// user before they ask one container to run thirty daemons.
	EstMemMB int
}

// aioKinds is the catalog. Order is the order the "Add feature" menu shows.
var aioKinds = []aioKind{
	{Kind: "ps", Label: "Percona Server", Family: famMySQL, EstMemMB: 700},
	{Kind: "psrepl", Label: "PS Replication", Family: famMySQL, Cluster: true, MinMem: 2, MaxMem: 5, DefMem: 3, EstMemMB: 700},
	{Kind: "innodb", Label: "InnoDB Cluster / GR", Family: famMySQL, Cluster: true, MinMem: 3, MaxMem: 9, DefMem: 3, OddOnly: true, EstMemMB: 800},
	{Kind: "pxc", Label: "PXC Cluster", Family: famMySQL, Cluster: true, MinMem: 2, MaxMem: 5, DefMem: 3, EstMemMB: 900},
	{Kind: "mysqlce", Label: "MySQL Community", Family: famMySQL, EstMemMB: 700},
	{Kind: "mysqlcerepl", Label: "MySQL Replication", Family: famMySQL, Cluster: true, MinMem: 2, MaxMem: 5, DefMem: 3, EstMemMB: 700},
	{Kind: "mysqlceinnodb", Label: "MySQL InnoDB / GR", Family: famMySQL, Cluster: true, MinMem: 3, MaxMem: 9, DefMem: 3, OddOnly: true, EstMemMB: 800},
	{Kind: "mariadb", Label: "MariaDB", Family: famMySQL, EstMemMB: 600},
	{Kind: "mariadbrepl", Label: "MariaDB Replication", Family: famMySQL, Cluster: true, MinMem: 2, MaxMem: 5, DefMem: 3, EstMemMB: 600},
	{Kind: "mariadbgalera", Label: "MariaDB Galera", Family: famMySQL, Cluster: true, MinMem: 3, MaxMem: 5, DefMem: 3, OddOnly: true, EstMemMB: 800},
	{Kind: "pg", Label: "PostgreSQL", Family: famPG, EstMemMB: 300},
	{Kind: "patroni", Label: "Patroni Cluster", Family: famPG, Cluster: true, MinMem: 2, MaxMem: 5, DefMem: 3, EstMemMB: 450},
	{Kind: "repmgr", Label: "repmgr Cluster", Family: famPG, Cluster: true, MinMem: 2, MaxMem: 5, DefMem: 3, EstMemMB: 350},
	{Kind: "spock", Label: "Spock Cluster", Family: famPG, Cluster: true, MinMem: 2, MaxMem: 5, DefMem: 2, EstMemMB: 350},
	{Kind: "psmdb", Label: "PSMDB", Family: famMongo, EstMemMB: 500},
	{Kind: "psmrs", Label: "PSMDB Replica Set", Family: famMongo, Cluster: true, MinMem: 3, MaxMem: 7, DefMem: 3, OddOnly: true, EstMemMB: 500},
	{Kind: "psmdbsharded", Label: "PSMDB Sharded", Family: famMongo, Cluster: true, MinMem: 5, MaxMem: 13, DefMem: 5, MemChoices: []int{5, 13}, EstMemMB: 400},
	{Kind: "valkey", Label: "Valkey", Family: famValkey, EstMemMB: 120},
	{Kind: "valkeycluster", Label: "Valkey Cluster", Family: famValkey, Cluster: true, MinMem: 3, MaxMem: 7, DefMem: 3, EstMemMB: 120},
	{Kind: "proxysql", Label: "ProxySQL", Family: famProxy, Cluster: true, MinMem: 1, MaxMem: 3, DefMem: 1, EstMemMB: 200},
	{Kind: "haproxy", Label: "HAProxy", Family: famHAProxy, EstMemMB: 80},
	{Kind: "orchestrator", Label: "Orchestrator", Family: famOrch, EstMemMB: 150},
}

var aioKindByID = func() map[string]aioKind {
	m := make(map[string]aioKind, len(aioKinds))
	for _, k := range aioKinds {
		m[k.Kind] = k
	}
	return m
}()

// aioKindOf looks up a kind; ok is false for an unknown kind id.
func aioKindOf(kind string) (aioKind, bool) {
	k, ok := aioKindByID[kind]
	return k, ok
}

// aioFamilyOf is the family a kind belongs to ("" when unknown).
func aioFamilyOf(kind string) string {
	if k, ok := aioKindByID[kind]; ok {
		return k.Family
	}
	return ""
}

// aioMemberCount is how many slots (daemons) an instance occupies, clamped to
// the kind's bounds. A non-cluster kind is always 1.
func aioMemberCount(kind string, members int) int {
	k, ok := aioKindByID[kind]
	if !ok {
		return 1
	}
	if !k.Cluster {
		return 1
	}
	if len(k.MemChoices) > 0 {
		// A fixed-topology kind snaps to the largest topology the count covers,
		// rather than clamping into a shape that does not exist. Snapping instead
		// of failing keeps a design saved before the choices narrowed deployable;
		// validateStack still flags the count so the user can correct it.
		best := 0
		for _, c := range k.MemChoices {
			if members >= c && c > best {
				best = c
			}
		}
		if best == 0 {
			return k.DefMem
		}
		return best
	}
	if members < k.MinMem {
		return k.DefMem
	}
	if members > k.MaxMem {
		return k.MaxMem
	}
	return members
}

// aioMemberChoiceOK reports whether a declared member count is one the kind
// accepts. Kinds without MemChoices accept anything inside their bounds, so the
// range/odd checks in validateStack remain the test for those.
func aioMemberChoiceOK(k aioKind, members int) bool {
	if len(k.MemChoices) == 0 {
		return true
	}
	for _, c := range k.MemChoices {
		if members == c {
			return true
		}
	}
	return false
}

// aioMemberChoicesText lists a kind's allowed member counts for a message —
// "5 or 13".
func aioMemberChoicesText(k aioKind) string {
	var parts []string
	for _, c := range k.MemChoices {
		parts = append(parts, strconv.Itoa(c))
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}

// ---------------------------------------------------------------- MySQL flavor

// The MySQL family is the one place where feature kinds cannot share a container.
// Every one of these server packages declares Provides: mysql-server and conflicts
// with the others at the package level, so an AiO node has at most ONE MySQL
// flavor, derived from the instances present rather than chosen by the user.
const (
	flavorNone    = ""
	flavorPS      = "ps"      // percona-server-server
	flavorPXC     = "pxc"     // percona-xtradb-cluster-server
	flavorMariaDB = "mariadb" // MariaDB-server (mariadb.org)
	flavorMySQLCE = "mysqlce" // mysql-community-server (repo.mysql.com)
)

// A kind's *shape* is its topology, independent of whose build provides it: the
// three flavors that speak MySQL run the same standalone / replication / Group
// Replication code, and Galera is Galera whether PXC or MariaDB ships it. Keeping
// shape separate from flavor is what lets MySQL Community reuse the Percona paths
// outright — only the packages differ.
const (
	shapeNone   = ""
	shapeSingle = "single" // one standalone server
	shapeRepl   = "repl"   // primary + replicas, classic async/semi-sync
	shapeGR     = "gr"     // Group Replication (optionally InnoDB Cluster)
	shapeGalera = "galera" // Galera / wsrep
)

// aioMySQLFlavorOfKind is the flavor a single MySQL-family kind requires
// (flavorNone for kinds outside the family).
func aioMySQLFlavorOfKind(kind string) string {
	switch kind {
	case "ps", "psrepl", "innodb":
		return flavorPS
	case "pxc":
		return flavorPXC
	case "mariadb", "mariadbrepl", "mariadbgalera":
		return flavorMariaDB
	case "mysqlce", "mysqlcerepl", "mysqlceinnodb":
		return flavorMySQLCE
	}
	return flavorNone
}

// aioMySQLShape is a MySQL-family kind's topology (shapeNone outside the family).
func aioMySQLShape(kind string) string {
	switch kind {
	case "ps", "mariadb", "mysqlce":
		return shapeSingle
	case "psrepl", "mariadbrepl", "mysqlcerepl":
		return shapeRepl
	case "innodb", "mysqlceinnodb":
		return shapeGR
	case "pxc", "mariadbgalera":
		return shapeGalera
	}
	return shapeNone
}

// aioIsMariaDB reports whether a flavor speaks MariaDB's dialect rather than
// MySQL's. This is the only axis on which MariaDB genuinely differs here: its GTIDs
// are domain-server-seq, it attaches replicas with MASTER_USE_GTID, it has no SET
// PERSIST, and its client/daemon are mariadb/mariadbd.
func aioIsMariaDB(flavor string) bool { return flavor == flavorMariaDB }

// aioFlavorLabel names a flavor for progress lines and validation messages.
func aioFlavorLabel(flavor string) string {
	switch flavor {
	case flavorPS:
		return "Percona Server"
	case flavorPXC:
		return "Percona XtraDB Cluster"
	case flavorMariaDB:
		return "MariaDB"
	case flavorMySQLCE:
		return "MySQL Community"
	}
	return "MySQL"
}

// ---------------------------------------------------------------- ports

// Family port bases. Each instance member takes a 10-port slot starting at
// base + slotIndex*10, so no product ever sees its default port and the
// families cannot reach each other's ranges (aioSlotsPerFamily caps them).
var aioFamilyBase = map[string]int{
	famMySQL:   13000,
	famPG:      15000,
	famProxy:   16000,
	famMongo:   17000,
	famHAProxy: 18000,
	famValkey:  19000,
	famOrch:    20000,
}

// aioSlotWidth is how many ports one member owns. Everything a single daemon
// needs (client port, cluster transport, admin/REST, a co-located etcd) fits.
const aioSlotWidth = 10

// aioSlotsPerFamily bounds a family to 100 members (1000 ports) so ranges never
// collide. Far past what fits in one container; validation rejects earlier.
const aioSlotsPerFamily = 100

// aioPorts is one member's resolved port set. Zero means "not used by this kind".
type aioPorts struct {
	Base int `json:"base"` // slot base — also the primary/client port

	Client  int `json:"client"`            // mysqld 3306 / postgres 5432 / mongod 27017 / valkey 6379 / proxysql mysql iface
	Admin   int `json:"admin,omitempty"`   // mysqlx, proxysql admin, haproxy stats, orchestrator web
	Group   int `json:"group,omitempty"`   // galera gcomm / GR local address / valkey cluster bus / proxysql cluster iface
	IST     int `json:"ist,omitempty"`     // galera IST
	SST     int `json:"sst,omitempty"`     // galera SST
	Check   int `json:"check,omitempty"`   // clustercheck / haproxy read port
	REST    int `json:"rest,omitempty"`    // patroni REST API
	EtcdCli int `json:"etcdCli,omitempty"` // patroni's co-located etcd, client
	EtcdPr  int `json:"etcdPr,omitempty"`  // ...and peer
}

// aioPortsFor resolves the port set for member `member` (0-based) of an instance
// whose first slot is `slot`. Offsets inside a slot are fixed per family so a
// port's meaning is stable and readable in `aioctl ports`.
func aioPortsFor(kind string, slot, member int) aioPorts {
	fam := aioFamilyOf(kind)
	base := aioFamilyBase[fam] + (slot+member)*aioSlotWidth
	p := aioPorts{Base: base, Client: base}
	switch fam {
	case famMySQL:
		p.Admin = base + 1 // mysqlx
		p.Group = base + 2 // wsrep gcomm / group_replication_local_address
		p.IST = base + 3
		p.SST = base + 4
		p.Check = base + 5 // clustercheck
	case famPG:
		p.REST = base + 1 // patroni
		p.EtcdCli = base + 2
		p.EtcdPr = base + 3
	case famMongo:
		// mongod/mongos need one port each; the rest of the slot is headroom.
	case famValkey:
		// Valkey's cluster bus defaults to port+10000, which would land in
		// another family's range (19000+10000). Pin it inside the slot instead
		// and set cluster-port explicitly in the config.
		p.Group = base + 1
	case famProxy:
		p.Admin = base + 1
		p.Group = base + 2 // proxysql cluster interface
	case famHAProxy:
		p.Check = base + 1 // read/replica port
		p.Admin = base + 2 // stats
	case famOrch:
		// Orchestrator listens on one HTTP port (moved off its default :3000).
	}
	return p
}

// aioAllPorts lists every TCP port a member actually binds, for conflict checks
// and for the `aioctl ports` table.
func (p aioPorts) list() []int {
	var out []int
	for _, v := range []int{p.Client, p.Admin, p.Group, p.IST, p.SST, p.Check, p.REST, p.EtcdCli, p.EtcdPr} {
		if v != 0 {
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// aioAssignSlots walks a node's instances in order and assigns each one its
// first slot index within its family, returning the slots keyed by instance id.
// Allocation is positional and deterministic: the same design always produces
// the same ports, so a redeploy never moves a running instance's port. A
// cluster instance consumes one slot per member.
func aioAssignSlots(instances []aioInstance) map[string]int {
	next := map[string]int{}
	out := make(map[string]int, len(instances))
	for _, in := range instances {
		fam := aioFamilyOf(in.Kind)
		if fam == "" {
			continue
		}
		out[in.ID] = next[fam]
		next[fam] += aioMemberCount(in.Kind, in.Members)
	}
	return out
}

// aioSlotsUsed is how many slots a family consumes for a node's instances —
// used by validation to reject a design that would overflow its range.
func aioSlotsUsed(instances []aioInstance, family string) int {
	n := 0
	for _, in := range instances {
		if aioFamilyOf(in.Kind) == family {
			n += aioMemberCount(in.Kind, in.Members)
		}
	}
	return n
}

// ---------------------------------------------------------------- web endpoints

// aioWebEndpoint is an HTTP interface an instance serves: which of its ports, and
// what to call it.
type aioWebEndpoint struct {
	Label string `json:"label"`
	Port  int    `json:"port"`
	Path  string `json:"path"`
}

// aioWebEndpoints lists the HTTP interfaces one member serves.
//
// These exist so the manager can offer a real link instead of a host:port string a
// user has to assemble by hand — and so aioCreateContainer knows to publish them.
// Only the client port was ever published, which left HAProxy's stats page and
// Patroni's REST API unreachable from the host even with the export ticked.
func aioWebEndpoints(kind string, p aioPorts) []aioWebEndpoint {
	switch kind {
	case "orchestrator":
		// Orchestrator's web UI and its API are the same listener.
		return []aioWebEndpoint{{Label: "Orchestrator", Port: p.Client, Path: "/"}}
	case "haproxy":
		return []aioWebEndpoint{{Label: "HAProxy stats", Port: p.Admin, Path: "/stats"}}
	case "patroni":
		// JSON rather than a UI, but it is how you read cluster state over HTTP.
		return []aioWebEndpoint{{Label: "Patroni REST", Port: p.REST, Path: "/cluster"}}
	}
	return nil
}

// aioPublishPorts is every container port an instance should publish when it opts
// into an export: the client port, plus any HTTP endpoint that is on a different
// port (a web UI is useless if only the client port is mapped).
func aioPublishPorts(kind string, p aioPorts) []int {
	out := []int{p.Client}
	seen := map[int]bool{p.Client: true}
	for _, w := range aioWebEndpoints(kind, p) {
		if w.Port > 0 && !seen[w.Port] {
			out = append(out, w.Port)
			seen[w.Port] = true
		}
	}
	return out
}
