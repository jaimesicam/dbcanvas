package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The All-in-One node's whole premise is that many database instances can share
// one container. Two invariants make that true, and both are cheap to break by
// accident in a later edit, so they are pinned here:
//
//   - no two instances ever get the same port, and
//   - no instance ever gets its product's DEFAULT port.
//
// The rest of the file covers the flavor conflict (the one design a user can
// draw that cannot be installed at all) and the registry format aioctl parses.

// defaultPorts are the ports each product binds when it owns a machine. An
// All-in-One instance landing on one of these is the bug this node type exists
// to avoid.
var defaultPorts = map[int]string{
	3306: "mysql", 33060: "mysqlx", 4567: "galera", 4568: "galera IST", 4444: "galera SST",
	5432: "postgres", 8008: "patroni", 2379: "etcd client", 2380: "etcd peer",
	27017: "mongod", 6379: "valkey", 16379: "valkey bus",
	6032: "proxysql admin", 6033: "proxysql mysql",
	8404: "haproxy stats", 3000: "orchestrator",
}

// everyKindDesign builds a node holding one instance of every kind, each at its
// maximum member count — the widest design the allocator must handle.
func everyKindDesign() designNode {
	var insts []aioInstance
	for _, k := range aioKinds {
		in := aioInstance{ID: "id-" + k.Kind, Kind: k.Kind, Name: k.Kind + "01", Members: 1}
		if k.Cluster {
			in.Members = k.MaxMem
		}
		insts = append(insts, in)
	}
	return designNode{ID: "n1", Type: "aio", Label: "aio1", AIOInstances: insts}
}

func TestAIOPortsNeverCollide(t *testing.T) {
	plan := aioPlan(everyKindDesign(), "example.net", "aio1")
	if len(plan) == 0 {
		t.Fatal("plan is empty")
	}
	owner := map[int]string{}
	for _, m := range plan {
		for _, p := range m.Ports.list() {
			if prev, dup := owner[p]; dup {
				t.Errorf("port %d assigned to both %s and %s", p, prev, m.Inst)
			}
			owner[p] = m.Inst
		}
	}
}

func TestAIOPortsAvoidProductDefaults(t *testing.T) {
	plan := aioPlan(everyKindDesign(), "example.net", "aio1")
	for _, m := range plan {
		for _, p := range m.Ports.list() {
			if name, isDefault := defaultPorts[p]; isDefault {
				t.Errorf("instance %s (%s) got port %d, which is the %s default", m.Inst, m.Kind, p, name)
			}
		}
	}
}

// A family's range must not run into the next family's. This is what
// aioSlotsPerFamily guarantees; the test fails if a base is moved too close.
func TestAIOFamilyRangesDoNotOverlap(t *testing.T) {
	type rng struct {
		fam    string
		lo, hi int
	}
	var ranges []rng
	for fam, base := range aioFamilyBase {
		ranges = append(ranges, rng{fam, base, base + aioSlotsPerFamily*aioSlotWidth - 1})
	}
	for i := range ranges {
		for j := i + 1; j < len(ranges); j++ {
			a, b := ranges[i], ranges[j]
			if a.lo <= b.hi && b.lo <= a.hi {
				t.Errorf("family ranges overlap: %s [%d-%d] and %s [%d-%d]", a.fam, a.lo, a.hi, b.fam, b.lo, b.hi)
			}
		}
	}
}

// Redeploying an unchanged design must not move a running instance's port.
func TestAIOPortsAreStableAcrossPlans(t *testing.T) {
	n := everyKindDesign()
	first := aioPlan(n, "example.net", "aio1")
	second := aioPlan(n, "example.net", "aio1")
	if len(first) != len(second) {
		t.Fatalf("plan length changed: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Inst != second[i].Inst || first[i].Ports != second[i].Ports {
			t.Errorf("instance %d moved: %+v then %+v", i, first[i], second[i])
		}
	}
}

// Adding an instance must not renumber the ones before it — otherwise editing a
// design would silently move a deployed instance's port on the next redeploy.
func TestAIOPortsStableWhenAppending(t *testing.T) {
	base := designNode{AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "psrepl", Name: "repl01", Members: 3},
	}}
	grown := designNode{AIOInstances: append(append([]aioInstance{}, base.AIOInstances...),
		aioInstance{ID: "c", Kind: "ps", Name: "ps02"})}

	before := aioPlan(base, "example.net", "h")
	after := aioPlan(grown, "example.net", "h")
	for i := range before {
		if before[i].Inst != after[i].Inst || before[i].Ports.Client != after[i].Ports.Client {
			t.Errorf("appending renumbered %s: port %d → %s port %d",
				before[i].Inst, before[i].Ports.Client, after[i].Inst, after[i].Ports.Client)
		}
	}
}

func TestAIOMySQLFlavor(t *testing.T) {
	cases := []struct {
		name     string
		kinds    []string
		want     string
		conflict bool
	}{
		{"none", []string{"pg", "valkey"}, flavorNone, false},
		{"ps only", []string{"ps", "psrepl", "innodb"}, flavorPS, false},
		{"pxc only", []string{"pxc"}, flavorPXC, false},
		{"several pxc clusters", []string{"pxc", "pxc"}, flavorPXC, false},
		{"ps + pxc conflicts", []string{"ps", "pxc"}, flavorNone, true},
		{"innodb + pxc conflicts", []string{"innodb", "pxc"}, flavorNone, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var insts []aioInstance
			for i, k := range tc.kinds {
				insts = append(insts, aioInstance{ID: fmt.Sprint(i), Kind: k, Name: fmt.Sprintf("%s%02d", k, i)})
			}
			got, conflict := aioMySQLFlavor(insts)
			if got != tc.want || conflict != tc.conflict {
				t.Errorf("got (%q,%v), want (%q,%v)", got, conflict, tc.want, tc.conflict)
			}
		})
	}
}

// The conflict must be reported as an error naming both sides — this is the one
// design a user can draw that cannot be installed at all.
func TestAIOValidateRejectsMixedMySQLFlavors(t *testing.T) {
	n := designNode{ID: "n1", Type: "aio", Label: "aio1", AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "pxc", Name: "pxc-cluster-01", Members: 3},
	}}
	issues := aioIssues(n, designDoc{Nodes: []designNode{n}}, map[int][]string{}, 0)
	var found string
	for _, is := range issues {
		if is.Level == "error" && strings.Contains(is.Message, "more than one MySQL flavor") {
			found = is.Message
		}
	}
	if found == "" {
		t.Fatalf("expected a flavor-conflict error, got %+v", issues)
	}
	for _, want := range []string{"ps01", "pxc-cluster-01"} {
		if !strings.Contains(found, want) {
			t.Errorf("error should name %q: %s", want, found)
		}
	}
}

func TestAIOValidateInstanceRules(t *testing.T) {
	mk := func(insts ...aioInstance) []issue {
		n := designNode{ID: "n1", Type: "aio", Label: "aio1", AIOInstances: insts}
		return aioIssues(n, designDoc{Nodes: []designNode{n}}, map[int][]string{}, 0)
	}
	hasErr := func(issues []issue, substr string) bool {
		for _, is := range issues {
			if is.Level == "error" && strings.Contains(is.Message, substr) {
				return true
			}
		}
		return false
	}

	if !hasErr(mk(
		aioInstance{ID: "a", Kind: "ps", Name: "dup"},
		aioInstance{ID: "b", Kind: "ps", Name: "dup"},
	), "names must be unique") {
		t.Error("duplicate instance names should be an error")
	}
	if !hasErr(mk(aioInstance{ID: "a", Kind: "psrepl", Name: "r1", Members: 99}), "allowed range") {
		t.Error("out-of-range member count should be an error")
	}
	if !hasErr(mk(aioInstance{ID: "a", Kind: "innodb", Name: "gr1", Members: 4}), "odd member count") {
		t.Error("even member count for a quorum kind should be an error")
	}
	if !hasErr(mk(aioInstance{ID: "a", Kind: "ps", Name: "ps01", PMMNodeID: "ghost"}), "PMM node that is not on the canvas") {
		t.Error("dangling PMM reference should be an error")
	}
	// Every kind now has a provisioner, so aioUnsupportedKinds is empty — but the
	// GATE must keep working, or the next half-built kind deploys silently as the
	// wrong thing. Exercise the mechanism directly rather than leaving a vacuous
	// assertion behind.
	aioUnsupportedKinds["valkey"] = true
	gated := hasErr(mk(aioInstance{ID: "a", Kind: "valkey", Name: "vk01"}), "not implemented yet")
	delete(aioUnsupportedKinds, "valkey")
	if !gated {
		t.Error("a kind listed in aioUnsupportedKinds should be rejected, not silently skipped")
	}
	if hasErr(mk(aioInstance{ID: "a", Kind: "valkey", Name: "vk01"}), "not implemented yet") {
		t.Error("the gate leaked: valkey should be deployable once removed from the map")
	}
	// innodb IS deployable now, but only in Group Replication mode — the other
	// mode of the same kind needs MySQL Shell and must be refused rather than
	// silently deploying as plain GR.
	if !hasErr(mk(aioInstance{ID: "a", Kind: "innodb", Name: "gr01", Members: 3, ReplMode: "innodbcluster"}), "MySQL Shell") {
		t.Error("innodbcluster mode should be rejected until MySQL Shell is installed")
	}
	if hasErr(mk(aioInstance{ID: "a", Kind: "innodb", Name: "gr01", Members: 3, ReplMode: "groupreplication"}), "not implemented yet") {
		t.Error("innodb in Group Replication mode should be deployable")
	}
	if !hasErr(mk(
		aioInstance{ID: "a", Kind: "ps", Name: "ps01"},
		aioInstance{ID: "b", Kind: "proxysql", Name: "px01", Members: 1, BackendInstance: ""},
	), "no backend selected") {
		t.Error("a proxy with no backend should be an error")
	}
	// ProxySQL speaks MySQL only.
	if !hasErr(mk(
		aioInstance{ID: "a", Kind: "valkey", Name: "vk01"},
		aioInstance{ID: "b", Kind: "proxysql", Name: "px01", Members: 1, BackendInstance: "a"},
	), "cannot front") {
		t.Error("ProxySQL fronting Valkey should be an error")
	}
}

// aioctl parses this file with awk -F'\t'; a row with the wrong column count or
// an empty field would silently mis-key every lookup.
func TestAIORegistryTSVShape(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "psrepl", Name: "repl01", Members: 3},
	}}
	cfg := aioConfig{Instances: aioPlan(n, "example.net", "aio1")}
	tsv := aioRegistryTSV(cfg)

	rows := 0
	for _, line := range strings.Split(tsv, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rows++
		fields := strings.Split(line, "\t")
		if len(fields) != 12 {
			t.Errorf("row has %d fields, want 12: %q", len(fields), line)
		}
		for i, f := range fields {
			if f == "" {
				t.Errorf("row %q has an empty field %d — aioctl would mis-parse it", line, i+1)
			}
		}
	}
	if rows != 4 { // 1 standalone + 3 replication members
		t.Errorf("got %d rows, want 4", rows)
	}
	// The group column is what `aioctl start <group>` matches on.
	if !strings.Contains(tsv, "\trepl01\t") {
		t.Error("replication members should carry their group name")
	}
}

// Roles decide start order: aioctl brings a group's seed up first.
func TestAIORolesAndStartOrder(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{ID: "b", Kind: "psrepl", Name: "repl01", Members: 3},
	}}
	plan := aioPlan(n, "example.net", "aio1")
	if plan[0].Role != "primary" {
		t.Errorf("first member should be the primary, got %q", plan[0].Role)
	}
	for _, m := range plan[1:] {
		if m.Role != "replica" {
			t.Errorf("member %s should be a replica, got %q", m.Inst, m.Role)
		}
	}
	// Each member must have a distinct instance id, unit and datadir.
	seen := map[string]bool{}
	for _, m := range plan {
		for _, v := range []string{m.Inst, m.Unit, m.DataDir} {
			if seen[v] {
				t.Errorf("value %q is shared between members", v)
			}
			seen[v] = true
		}
	}
}

// The generated my.cnf must never carry a default port or the shared datadir.
func TestAIOMySQLCnfIsInstanceScoped(t *testing.T) {
	n := designNode{OS: "oraclelinux", AIOPSMajor: "8.0", AIOInstances: []aioInstance{
		{ID: "a", Kind: "psrepl", Name: "repl01", Members: 2},
	}}
	plan := aioPlan(n, "example.net", "aio1")
	for _, m := range plan {
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		cnf := aioMySQLCnf(l, m, n, "8.0", "")
		if strings.Contains(cnf, "port=3306") || strings.Contains(cnf, "=3306\n") {
			t.Errorf("%s my.cnf uses the default port:\n%s", m.Inst, cnf)
		}
		if strings.Contains(cnf, "datadir=/var/lib/mysql\n") {
			t.Errorf("%s my.cnf uses the shared datadir", m.Inst)
		}
		if !strings.Contains(cnf, fmt.Sprintf("port=%d", m.Ports.Client)) {
			t.Errorf("%s my.cnf missing its own port %d", m.Inst, m.Ports.Client)
		}
		if !strings.Contains(cnf, "gtid_mode=ON") {
			t.Errorf("%s is a replication member and needs GTID", m.Inst)
		}
	}
	// Two members must not share a server-id, or replication silently breaks.
	if serverIDFor(plan[0].Inst) == serverIDFor(plan[1].Inst) {
		t.Error("replication members share a server-id")
	}
}

// The unit must bind to aio.target (so `aioctl stop all` works) and must run the
// instance's own config.
func TestAIOUnitFile(t *testing.T) {
	l := aioLayout("ps01", "ps", aioPortsFor("ps", 0, 0))
	u := aioUnitSpec{ExecStart: "/usr/sbin/mysqld --defaults-file=" + l.ConfPath, Type: "notify"}
	unit := aioUnitFile(l, u)
	for _, want := range []string{
		"PartOf=" + aioTarget,
		"WantedBy=" + aioTarget,
		"User=mysql",
		"RuntimeDirectory=aio/ps01",
		"--defaults-file=/opt/aio/ps01/etc/my.cnf",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
}

// defaultLayout must keep reporting the classic node's real paths — it is what
// existing call sites pass, so a change here would silently move a live node's
// datadir.
func TestDefaultLayoutMatchesClassicPaths(t *testing.T) {
	l := defaultLayout("ps", "oraclelinux")
	if l.DataDir != "/var/lib/mysql" || l.ConfPath != "/etc/my.cnf" || l.Ports.Client != 3306 {
		t.Errorf("classic MySQL layout drifted: %+v", l)
	}
	if l.Unit != mysqlUnit("oraclelinux") {
		t.Errorf("unit %q does not match mysqlUnit()", l.Unit)
	}
	d := defaultLayout("ps", "ubuntu")
	if d.ConfPath != pxcCnfPath("ubuntu") || d.LogErr != pxcLogError("ubuntu") {
		t.Errorf("classic Debian MySQL layout drifted: %+v", d)
	}
}

// The API's guard must reject an instance name the deployment never planned —
// it is interpolated into a container exec.
func TestAIOSelectorGuard(t *testing.T) {
	cfg := aioConfig{Instances: []aioInstanceRuntime{
		{Inst: "ps01"}, {Inst: "repl01-n1", Group: "repl01"},
	}}
	for _, ok := range []string{"all", "ps01", "repl01-n1", "repl01"} {
		if !aioValidSelector(cfg, ok) {
			t.Errorf("selector %q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "ghost", "ps01; rm -rf /", "../../etc"} {
		if aioValidSelector(cfg, bad) {
			t.Errorf("selector %q should be rejected", bad)
		}
	}
}

func TestAIOParseStates(t *testing.T) {
	listing := `INSTANCE               KIND           GROUP              ROLE        STATE      PORTS
ps01                   ps             -                  standalone  active     13000,13001
repl01-n1              psrepl         repl01             primary     active     13010,13011
repl01-n2              psrepl         repl01             replica     failed     13020,13021
`
	states := aioParseStates(listing)
	want := map[string]string{"ps01": "active", "repl01-n1": "active", "repl01-n2": "failed"}
	for inst, w := range want {
		if states[inst] != w {
			t.Errorf("%s: got %q, want %q", inst, states[inst], w)
		}
	}
	if len(states) != 3 {
		t.Errorf("got %d states, want 3: %v", len(states), states)
	}
}

// `systemctl is-active` prints the state AND exits non-zero for anything that is
// not active. An `|| echo unknown` fallback therefore emits a SECOND line and
// shifts every column of `aioctl list` — which is exactly what happened on the
// first live deploy. Pin the shape of the fix.
func TestAIOCtlStateOfHandlesNonZeroExit(t *testing.T) {
	if strings.Contains(aioCtlScript, `systemctl is-active "$(unit_of "$1")" 2>/dev/null || echo unknown`) {
		t.Error("state_of uses `|| echo unknown`, which emits two lines for an inactive unit")
	}
	if !strings.Contains(aioCtlScript, `[ -n "$s" ] || s=unknown`) {
		t.Error("state_of should capture the output and only default when it is empty")
	}
}

// The design document must round-trip: the canvas stores it as JSON, so a field
// that does not survive a marshal/unmarshal is silently lost on save.
func TestAIODesignRoundTrip(t *testing.T) {
	n := designNode{
		ID: "n1", Type: "aio", Label: "aio1", OS: "oraclelinux", OSVersion: "9",
		AIOPSMajor: "8.4",
		AIOInstances: []aioInstance{
			{ID: "a", Kind: "psrepl", Name: "repl01", Members: 3, ReplMode: "semisync", GTID: true, PMMNodeID: "pmm1"},
		},
	}
	b, err := json.Marshal(designDoc{Nodes: []designNode{n}})
	if err != nil {
		t.Fatal(err)
	}
	var back designDoc
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got := back.Nodes[0]
	if got.AIOPSMajor != "8.4" || len(got.AIOInstances) != 1 {
		t.Fatalf("node did not round-trip: %+v", got)
	}
	in := got.AIOInstances[0]
	if in.Kind != "psrepl" || in.Members != 3 || in.ReplMode != "semisync" || !in.GTID || in.PMMNodeID != "pmm1" {
		t.Errorf("instance did not round-trip: %+v", in)
	}
}

// ---------------------------------------------------------------- new families

// The Go catalog and its browser mirror must agree on which kinds are
// deployable. If they drift, the form offers something validateStack rejects (or
// hides something that works) — a class of bug no other test would catch.
func TestAIOSupportedKindsMatchJSCatalog(t *testing.T) {
	src, err := os.ReadFile("web/src/lib/aioPorts.js")
	if err != nil {
		t.Skipf("JS catalog not readable: %v", err)
	}
	jsSupported := map[string]bool{}
	for _, line := range strings.Split(string(src), "\n") {
		m := regexp.MustCompile(`\{ kind: '([a-z]+)',`).FindStringSubmatch(line)
		if m == nil {
			continue
		}
		jsSupported[m[1]] = strings.Contains(line, "supported: true")
	}
	if len(jsSupported) != len(aioKinds) {
		t.Fatalf("catalog size differs: Go has %d kinds, JS has %d", len(aioKinds), len(jsSupported))
	}
	for _, k := range aioKinds {
		goOK := aioSupportedFamilies[k.Family] && !aioUnsupportedKinds[k.Kind]
		js, listed := jsSupported[k.Kind]
		if !listed {
			t.Errorf("kind %q is missing from the JS catalog", k.Kind)
			continue
		}
		if goOK != js {
			t.Errorf("kind %q: Go supported=%v but JS supported=%v", k.Kind, goOK, js)
		}
	}
}

// Valkey's cluster bus defaults to port+10000. At base 19000 that lands at
// 29000+, outside the family's reserved range entirely — so the config MUST pin
// cluster-port inside the slot and announce it.
func TestAIOValkeyClusterPortStaysInSlot(t *testing.T) {
	n := designNode{OS: "oraclelinux", AIOInstances: []aioInstance{
		{ID: "a", Kind: "valkeycluster", Name: "vkc01", Members: 3},
	}}
	for _, m := range aioPlan(n, "example.net", "aio1") {
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		conf := aioValkeyConf(l, m, n.AIOInstances[0], "example.net", "dc=example,dc=net", "oraclelinux")
		if !strings.Contains(conf, fmt.Sprintf("cluster-port %d", m.Ports.Group)) {
			t.Errorf("%s: cluster-port not pinned to the slot:\n%s", m.Inst, conf)
		}
		if !strings.Contains(conf, fmt.Sprintf("cluster-announce-bus-port %d", m.Ports.Group)) {
			t.Errorf("%s: bus port not announced", m.Inst)
		}
		// The implicit default would be client+10000; it must not appear.
		if strings.Contains(conf, fmt.Sprint(m.Ports.Client+10000)) {
			t.Errorf("%s: config references the default bus port %d", m.Inst, m.Ports.Client+10000)
		}
		if m.Ports.Group < aioFamilyBase[famValkey] ||
			m.Ports.Group >= aioFamilyBase[famValkey]+aioSlotsPerFamily*aioSlotWidth {
			t.Errorf("%s: bus port %d is outside the Valkey range", m.Inst, m.Ports.Group)
		}
		// Without this the Type=notify unit never reports active (see valkey.go).
		if !strings.Contains(conf, "supervised systemd") {
			t.Errorf("%s: missing `supervised systemd`", m.Inst)
		}
	}
}

// PostgreSQL is the one family where two instances may run different majors,
// because PPG packages are per-major and co-install.
func TestAIOPGSupportsPerInstanceMajors(t *testing.T) {
	n := designNode{OS: "oraclelinux", AIOInstances: []aioInstance{
		{ID: "a", Kind: "pg", Name: "pg16", PGMajor: "16"},
		{ID: "b", Kind: "pg", Name: "pg17", PGMajor: "17"},
	}}
	if aioPGMajor(n.AIOInstances[0]) == aioPGMajor(n.AIOInstances[1]) {
		t.Fatal("per-instance PG majors collapsed to one value")
	}
	plan := aioPlan(n, "example.net", "aio1")
	if plan[0].Ports.Client == plan[1].Ports.Client {
		t.Error("two PostgreSQL instances share a port")
	}
	for _, m := range plan {
		if m.Ports.Client == 5432 {
			t.Errorf("%s got the default PostgreSQL port", m.Inst)
		}
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		if !strings.HasPrefix(l.DataDir, aioRoot+"/"+m.Inst) {
			t.Errorf("%s PGDATA is not instance-scoped: %s", m.Inst, l.DataDir)
		}
	}
}

// Orchestrator: its own port, its own SQLite backend, and an explicit -config —
// the RHEL package otherwise auto-loads /etc/orchestrator.conf.json, which every
// instance would share.
func TestAIOOrchestratorIsInstanceScoped(t *testing.T) {
	n := designNode{OS: "oraclelinux", AIOInstances: []aioInstance{
		{ID: "a", Kind: "orchestrator", Name: "orch01"},
		{ID: "b", Kind: "orchestrator", Name: "orch02"},
	}}
	plan := aioPlan(n, "example.net", "aio1")
	sec := pxcSecrets{OrchestratorUser: "orchestrator", OrchestratorPassword: "pw"}
	seen := map[string]bool{}
	for _, m := range plan {
		if m.Ports.Client == orchestratorPort {
			t.Errorf("%s got Orchestrator's default port %d", m.Inst, orchestratorPort)
		}
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		conf := aioOrchConfJSON(l, m, sec, "")
		if !strings.Contains(conf, fmt.Sprintf(`"ListenAddress": ":%d"`, m.Ports.Client)) {
			t.Errorf("%s: ListenAddress is not its own port:\n%s", m.Inst, conf)
		}
		sqlite := l.DataDir + "/orchestrator.sqlite3"
		if !strings.Contains(conf, sqlite) {
			t.Errorf("%s: SQLite backend is not instance-scoped", m.Inst)
		}
		if seen[sqlite] {
			t.Errorf("two Orchestrator instances share the backend file %s", sqlite)
		}
		seen[sqlite] = true

		unit := aioUnitFile(l, aioUnitSpec{
			ExecStart: fmt.Sprintf("/usr/local/orchestrator/orchestrator -config %s http", l.ConfPath),
			EnvFile:   fmt.Sprintf("ORCHESTRATOR_API=http://127.0.0.1:%d/api\n", m.Ports.Client),
		})
		if !strings.Contains(unit, "-config "+l.ConfPath) {
			t.Errorf("%s: unit does not pass an explicit -config", m.Inst)
		}
	}
}

// Every supported family must land in the provisioner dispatch, or a design
// would validate and then fail at deploy with "does not support".
func TestAIOEveryDeployableKindHasAProvisioner(t *testing.T) {
	for _, k := range aioKinds {
		deployable := aioSupportedFamilies[k.Family] && !aioUnsupportedKinds[k.Kind]
		if !deployable {
			continue
		}
		switch k.Family {
		case famMySQL, famPG, famValkey, famOrch, famMongo, famProxy, famHAProxy:
		default:
			t.Errorf("kind %q is deployable but family %q has no dispatch case", k.Kind, k.Family)
		}
	}
}

// `sudo` is NOT installed on the dbcanvas-systemd base images — the codebase
// uses `runuser` for the postgres user everywhere else. `aioctl connect <pg>`
// failed live with "exec: sudo: not found" before this was fixed.
func TestAIOCtlUsesRunuserNotSudo(t *testing.T) {
	// Check executable lines only — the fix's own comment mentions sudo.
	for _, line := range strings.Split(aioCtlScript, "\n") {
		if code, _, _ := strings.Cut(strings.TrimSpace(line), "#"); strings.Contains(code, "sudo ") {
			t.Errorf("aioctl invokes sudo, which the base images do not have: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(aioCtlScript, `runuser -u postgres --`) {
		t.Error("the psql path should shell out via runuser -u postgres")
	}
}

// Valkey always sets requirepass, so `aioctl connect` must authenticate or every
// call returns NOAUTH — which is exactly what happened on the first live run.
func TestAIOValkeyConnectHasCredentials(t *testing.T) {
	if !strings.Contains(aioCtlScript, `${vkpw:+-a "$vkpw"}`) {
		t.Error("the valkey-cli path does not pass a password")
	}
	// ...and the password must actually reach the env file the script reads.
	l := aioLayout("vk01", "valkey", aioPortsFor("valkey", 0, 0))
	m := aioInstanceRuntime{Inst: "vk01", Kind: "valkey", Ports: l.Ports}
	spec := aioUnitSpec{EnvFile: fmt.Sprintf(
		"AIO_INST=%s\nAIO_PORT=%d\nAIO_VALKEY_PW=%s\n", m.Inst, m.Ports.Client, "secret")}
	if !strings.Contains(spec.EnvFile, "AIO_VALKEY_PW=") {
		t.Error("the Valkey env file should carry AIO_VALKEY_PW")
	}
	if !strings.Contains(aioCtlScript, "AIO_VALKEY_PW") {
		t.Error("aioctl should read AIO_VALKEY_PW from the instance env file")
	}
}

// A Group Replication member runs with group_replication_start_on_boot=OFF (so a
// cold start cannot race three members into a split group). That means systemd
// reporting "active" leaves the GROUP down — `aioctl start <group>` brought the
// daemons up but left 0 members ONLINE on the first live test. aioctl therefore
// runs a per-instance post-start hook, which the provisioner stages.
func TestAIOCtlRunsPostStartHook(t *testing.T) {
	if !strings.Contains(aioCtlScript, `hook="/etc/dbcanvas/aio/$i.poststart"`) {
		t.Error("aioctl does not look for a post-start hook")
	}
	// The hook must NOT run on stop, and only when executable.
	if !strings.Contains(aioCtlScript, `[ "$verb" != "stop" ] && [ -x "$hook" ]`) {
		t.Error("the post-start hook should be guarded on verb != stop and executable")
	}
	// The GR config must keep start-on-boot off, or the hook is pointless and
	// three members would race on boot.
	n := designNode{OS: "oraclelinux", AIOInstances: []aioInstance{
		{ID: "g", Kind: "innodb", Name: "gr01", Members: 3, ReplMode: "groupreplication"},
	}}
	for _, m := range aioPlan(n, "example.net", "aio1") {
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		cnf := aioMySQLCnf(l, m, n, "8.0", "")
		if !strings.Contains(cnf, "group_replication_start_on_boot=OFF") {
			t.Errorf("%s: start_on_boot should be OFF", m.Inst)
		}
		if !strings.Contains(cnf, fmt.Sprintf(`group_replication_local_address="127.0.0.1:%d"`, m.Ports.Group)) {
			t.Errorf("%s: local address should be its own slot port", m.Inst)
		}
	}
}

// Every member of a group must agree on the group name, and it must be stable
// across redeploys — a fresh UUID would create a second group the existing
// datadirs refuse to join.
func TestAIOGroupUUIDStableAndValid(t *testing.T) {
	a, b := aioGroupUUID("instance-1"), aioGroupUUID("instance-1")
	if a != b {
		t.Errorf("group UUID is not deterministic: %s vs %s", a, b)
	}
	if aioGroupUUID("instance-2") == a {
		t.Error("different instances share a group UUID")
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a) {
		t.Errorf("group name %q is not a valid RFC-4122 UUID — GR rejects it", a)
	}
}

// The GR readiness check must key on MEMBER_ID, not MEMBER_HOST: every instance
// in an All-in-One node reports the SAME container hostname (verified live), so
// a MEMBER_HOST match would return an arbitrary peer's state.
func TestAIOGRWaitsOnMemberID(t *testing.T) {
	for _, s := range []string{aioMySQLGRBootstrapScript, aioMySQLGRJoinScript} {
		if strings.Contains(s, "MEMBER_HOST=@@hostname") {
			t.Error("GR readiness keys on MEMBER_HOST, which is identical for every instance in this node")
		}
		if !strings.Contains(s, "MEMBER_ID=@@server_uuid") {
			t.Error("GR readiness should key on MEMBER_ID=@@server_uuid")
		}
	}
}

// mysqld --initialize must NOT see the instance's real config: it carries
// replication settings and, for an innodb member, plugin_load_add for the GR
// plugin. The classic path (mysqlDatadirInit) writes a minimal file for the same
// reason.
func TestAIOInitUsesMinimalConfig(t *testing.T) {
	if strings.Contains(aioMySQLInitScript, `--defaults-file="$CNF"`) {
		t.Error("initialize uses the instance's full config; it must use a minimal one")
	}
	if !strings.Contains(aioMySQLInitScript, `--defaults-file="$INITCNF"`) {
		t.Error("initialize should use the generated minimal defaults file")
	}
}

// An All-in-One node is the one type whose contents can change while it runs —
// its instance list is edited on the node itself, not by adding canvas nodes.
// The deploy loop skips every RUNNING node, so before aioNeedsRedeploy, adding a
// feature to a deployed node silently did nothing.
func TestAIONeedsRedeployOnlyWhenInstancesAdded(t *testing.T) {
	base := designNode{ID: "n1", Type: "aio", Label: "aio1", AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "psrepl", Name: "repl01", Members: 2},
	}}
	// A COMPLETED deploy: every planned instance was actually built.
	deployed := func(n designNode) Deployment {
		members := aioPlan(n, "example.net", "aio1")
		for i := range members {
			members[i].Ready = true
		}
		b, _ := json.Marshal(aioConfig{Instances: members})
		return Deployment{ContainerID: "abc", Config: b}
	}
	dep := deployed(base)

	if aioNeedsRedeploy(&App{}, Stack{}, base, dep) {
		t.Error("an unchanged design should not redeploy")
	}

	grown := base
	grown.AIOInstances = append(append([]aioInstance{}, base.AIOInstances...),
		aioInstance{ID: "c", Kind: "valkey", Name: "vk01"})
	if !aioNeedsRedeploy(&App{}, Stack{}, grown, dep) {
		t.Error("adding an instance should redeploy")
	}

	// Removing one must NOT redeploy: tearing down a datadir is destructive and
	// belongs behind an explicit action.
	shrunk := base
	shrunk.AIOInstances = base.AIOInstances[:1]
	if aioNeedsRedeploy(&App{}, Stack{}, shrunk, dep) {
		t.Error("removing an instance should not trigger a redeploy")
	}
}

// Re-running a family provisioner over an existing instance would RESET a live
// MySQL server's GTID history. Only genuinely new instances may be built.
func TestAIOFreshInstancesExcludesExisting(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "psrepl", Name: "repl01", Members: 2},
	}}
	members := aioPlan(n, "example.net", "aio1")

	// First deploy: everything is new.
	all := aioFreshInstances(map[string]bool{}, members)
	if len(all) != len(members) {
		t.Fatalf("first deploy should treat all %d members as fresh, got %d", len(members), len(all))
	}

	// Redeploy after adding one: only the addition is fresh.
	prev := map[string]bool{"ps01": true, "repl01-n1": true, "repl01-n2": true}
	grown := designNode{AIOInstances: append(append([]aioInstance{}, n.AIOInstances...),
		aioInstance{ID: "c", Kind: "ps", Name: "ps02"})}
	fresh := aioFreshInstances(prev, aioPlan(grown, "example.net", "aio1"))
	if len(fresh) != 1 || !fresh["ps02"] {
		t.Errorf("only ps02 should be fresh, got %v", fresh)
	}
	for existing := range prev {
		if fresh[existing] {
			t.Errorf("%s already exists and must not be re-provisioned", existing)
		}
	}
}

// MongoDB: one install, every topology. The sharded layout must produce exactly
// one mongos, one config server and one shard per remaining member, each with a
// distinct replica-set name and port.
func TestAIOMongoShardedTopology(t *testing.T) {
	in := aioInstance{ID: "s", Kind: "psmdbsharded", Name: "sh01", Members: 5}
	n := designNode{AIOInstances: []aioInstance{in}}
	cfg := aioConfig{Instances: aioPlan(n, "example.net", "aio1")}

	roles := map[string]int{}
	ports := map[int]bool{}
	for _, m := range cfg.Instances {
		roles[m.Role]++
		if ports[m.Ports.Client] {
			t.Errorf("duplicate port %d", m.Ports.Client)
		}
		ports[m.Ports.Client] = true
		if m.Ports.Client == 27017 {
			t.Errorf("%s got MongoDB's default port", m.Inst)
		}
	}
	if roles["mongos"] != 1 || roles["config"] != 1 || roles["shard"] != 3 {
		t.Errorf("unexpected topology: %v", roles)
	}
	// Each shard needs its OWN replica-set name, or addShard registers one shard.
	seen := map[string]bool{}
	for _, m := range cfg.Instances {
		if m.Role != "shard" {
			continue
		}
		rs := aioMongoRSName(in, m, cfg)
		if seen[rs] {
			t.Errorf("two shard members share replica set %q", rs)
		}
		seen[rs] = true
	}
	// The router must point at the config RS, by name and by member port.
	var mongos, config aioInstanceRuntime
	for _, m := range cfg.Instances {
		switch m.Role {
		case "mongos":
			mongos = m
		case "config":
			config = m
		}
	}
	got := aioMongoConfigDB(mongos, cfg, in)
	if !strings.HasPrefix(got, "sh01-cfg/") || !strings.Contains(got, fmt.Sprint(config.Ports.Client)) {
		t.Errorf("configDB %q should name the config RS and its port", got)
	}
}

// A standalone needs no internal cluster auth; anything replicated does, and all
// of them share the node's one keyFile.
func TestAIOMongoConfKeyFileUsage(t *testing.T) {
	for _, tc := range []struct {
		kind        string
		wantKeyFile bool
	}{
		{"psmdb", false}, {"psmrs", true}, {"psmdbsharded", true},
	} {
		in := aioInstance{ID: "x", Kind: tc.kind, Name: "m01", Members: 5}
		n := designNode{AIOInstances: []aioInstance{in}}
		cfg := aioConfig{Instances: aioPlan(n, "example.net", "aio1")}
		m := cfg.Instances[0]
		if tc.kind == "psmdbsharded" {
			m = cfg.Instances[1] // skip the mongos; check a config server
		}
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		conf := aioMongodConf(l, m, in, cfg)
		if got := strings.Contains(conf, "keyFile"); got != tc.wantKeyFile {
			t.Errorf("%s: keyFile=%v, want %v:\n%s", tc.kind, got, tc.wantKeyFile, conf)
		}
		if !strings.Contains(conf, fmt.Sprintf("port: %d", m.Ports.Client)) {
			t.Errorf("%s: config does not use the instance's own port", tc.kind)
		}
		if !strings.Contains(conf, "dbPath: "+l.DataDir) {
			t.Errorf("%s: dbPath is not instance-scoped", tc.kind)
		}
	}
}

// Once any instance is baselined, /root/.my.cnf exists with user+password. A
// plain `mysql -uroot` against a NEWLY ADDED instance (root password still empty
// from --initialize-insecure) would then send that password and fail with
// ERROR 1045 — which is what happened the first time an instance was added to a
// live node. The baseline must therefore ignore the defaults file.
func TestAIOBaselineIgnoresRootMyCnf(t *testing.T) {
	if !strings.Contains(aioMySQLBaselineScript, "mysql --no-defaults --socket=$SOCK -uroot") {
		t.Error("the baseline must use --no-defaults, or /root/.my.cnf breaks adding an instance to a live node")
	}
	// It must still handle both cases: already-set password, and empty password.
	if !strings.Contains(aioMySQLBaselineScript, `$MB -p"$ROOT_PW" -e "SELECT 1"`) {
		t.Error("the baseline should first try the configured password (redeploy case)")
	}
	if !strings.Contains(aioMySQLBaselineScript, `elif $MB -e "SELECT 1"`) {
		t.Error("the baseline should fall back to an empty password (fresh instance)")
	}
}

// On a redeploy the group is live, so every secondary is super_read_only. The
// GR prelude's already-ONLINE early exit must therefore come BEFORE any write,
// or CREATE USER fails with "ERROR 1290 … --super-read-only" — which is what
// happened the first time an instance was added to a live GR node.
func TestAIOGRPrepChecksOnlineBeforeWriting(t *testing.T) {
	onlineCheck := strings.Index(aioMySQLGRPrep, "MEMBER_ID=@@server_uuid")
	firstWrite := strings.Index(aioMySQLGRPrep, "CREATE USER")
	if onlineCheck < 0 || firstWrite < 0 {
		t.Fatal("GR prelude no longer contains the expected statements")
	}
	if onlineCheck > firstWrite {
		t.Error("the already-ONLINE exit must precede CREATE USER; a super_read_only secondary cannot write")
	}
	if !strings.HasPrefix(strings.TrimSpace(aioMySQLGRPrep), "if [") {
		t.Error("the GR prelude should open with the already-ONLINE guard")
	}
}

// The config is written optimistically at the START of a deploy so the UI can
// show the plan, which means "present in the config" does not mean "built". A
// deploy that failed halfway would otherwise poison the next one into skipping
// the instance it never finished — observed live: an added instance was prepared
// but never baselined, then skipped on the retry.
func TestAIOReadyGatesFreshDetection(t *testing.T) {
	n := designNode{ID: "n1", Type: "aio", AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "ps", Name: "ps02"},
	}}
	members := aioPlan(n, "example.net", "aio1")
	// Simulate a deploy where ps01 finished and ps02 did not.
	for i := range members {
		members[i].Ready = members[i].Inst == "ps01"
	}
	b, _ := json.Marshal(aioConfig{Instances: members})
	dep := Deployment{ContainerID: "abc", Config: b}

	prev := map[string]bool{}
	for _, m := range aioRuntimeInstances(dep) {
		if m.Ready {
			prev[m.Inst] = true
		}
	}
	fresh := aioFreshInstances(prev, aioPlan(n, "example.net", "aio1"))
	if fresh["ps01"] {
		t.Error("ps01 was built and must not be rebuilt")
	}
	if !fresh["ps02"] {
		t.Error("ps02 was never finished and MUST be retried")
	}
	if !aioNeedsRedeploy(&App{}, Stack{}, n, dep) {
		t.Error("a node with an unfinished instance should redeploy")
	}
}

// A mongos refuses to start until it can reach its config replica set, so the
// routers must be provisioned AFTER the config RS is initiated — and aioctl must
// start them last too. Both were wrong on the first live sharded deploy
// ("Connection refused" against the configDB).
func TestAIOMongosStartsLast(t *testing.T) {
	// aioctl's group ordering must rank a router behind seeds and members.
	if !strings.Contains(aioCtlScript, `r = ($5=="mongos") ? 2 :`) {
		t.Error("aioctl should sort mongos last within a group")
	}
	if !strings.Contains(aioCtlScript, `$5=="config"`) {
		t.Error("config servers should be seeded first, alongside bootstrap/primary")
	}
	// And the provisioner must defer the routers behind a callback.
	if !strings.Contains(aioMongoProvisionOrderMarker, "startRouters") {
		t.Error("the sharded path should start routers only after the config RS is initiated")
	}
}

// aioMongoProvisionOrderMarker pins the ordering contract above to a symbol the
// compiler checks, so renaming the callback breaks the test loudly.
var aioMongoProvisionOrderMarker = "startRouters"

// A proxy on the canvas is wired by drawing an edge; an All-in-One node has no
// endpoints, so the proxy names its backend with backendInstanceId and the
// member ports come from the plan. Every address must be a slot port — a classic
// HAProxy would say member:3306, which here would reach nothing.
func TestAIOProxyBackendResolution(t *testing.T) {
	n := designNode{OS: "oraclelinux", AIOInstances: []aioInstance{
		{ID: "db", Kind: "psrepl", Name: "repl01", Members: 3},
		{ID: "hp", Kind: "haproxy", Name: "hap01", BackendInstance: "db"},
		{ID: "px", Kind: "proxysql", Name: "psql01", Members: 1, BackendInstance: "db"},
	}}
	cfg := aioConfig{Instances: aioPlan(n, "example.net", "aio1")}

	backend, members, ok := aioProxyBackend(n, cfg, n.AIOInstances[1])
	if !ok || backend.Name != "repl01" || len(members) != 3 {
		t.Fatalf("backend resolution failed: ok=%v backend=%q members=%d", ok, backend.Name, len(members))
	}

	var hap, psql aioInstanceRuntime
	for _, m := range cfg.Instances {
		switch m.Kind {
		case "haproxy":
			hap = m
		case "proxysql":
			psql = m
		}
	}

	hcfg := aioHAProxyCfg(hap, backend, members)
	for _, mem := range members {
		if !strings.Contains(hcfg, fmt.Sprintf("127.0.0.1:%d", mem.Ports.Client)) {
			t.Errorf("haproxy config missing backend %s:%d", mem.Inst, mem.Ports.Client)
		}
	}
	if strings.Contains(hcfg, ":3306") {
		t.Error("haproxy config points at the default MySQL port")
	}
	// Writes go to ONE member with the rest as backups, or writes would round-robin
	// onto read-only replicas.
	if strings.Count(hcfg, " backup\n") != len(members)-1 {
		t.Errorf("expected %d backup servers in the write listener:\n%s", len(members)-1, hcfg)
	}
	// The proxy's own listeners must be on its slot, not haproxy's usual ports.
	for _, want := range []int{hap.Ports.Client, hap.Ports.Check, hap.Ports.Admin} {
		if !strings.Contains(hcfg, fmt.Sprintf("bind *:%d", want)) {
			t.Errorf("haproxy does not bind its slot port %d", want)
		}
	}
	if strings.Contains(hcfg, "bind *:8404") {
		t.Error("haproxy stats is on its default port")
	}

	l := aioLayout(psql.Inst, psql.Kind, psql.Ports)
	pcfg := aioProxySQLCnf(l, psql, pxcSecrets{ClusterUser: "cluster", ClusterPassword: "pw"})
	if !strings.Contains(pcfg, fmt.Sprintf("0.0.0.0:%d", psql.Ports.Client)) ||
		!strings.Contains(pcfg, fmt.Sprintf("0.0.0.0:%d", psql.Ports.Admin)) {
		t.Errorf("proxysql does not use its slot ports:\n%s", pcfg)
	}
	for _, def := range []string{"6033", "6032"} {
		if strings.Contains(pcfg, def) {
			t.Errorf("proxysql config references default port %s", def)
		}
	}
	// Each instance needs its own datadir: they would otherwise share one sqlite db.
	if !strings.Contains(pcfg, l.DataDir) {
		t.Error("proxysql datadir is not instance-scoped")
	}
}

// A proxy whose backend was deleted must not reach the provisioner.
func TestAIOProxyBackendMissing(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{ID: "hp", Kind: "haproxy", Name: "hap01", BackendInstance: "gone"},
	}}
	cfg := aioConfig{Instances: aioPlan(n, "example.net", "aio1")}
	if _, _, ok := aioProxyBackend(n, cfg, n.AIOInstances[0]); ok {
		t.Error("a dangling backendInstanceId should not resolve")
	}
	issues := aioIssues(n, designDoc{Nodes: []designNode{n}}, map[int][]string{}, 0)
	found := false
	for _, is := range issues {
		if is.Level == "error" && strings.Contains(is.Message, "no longer exists") {
			found = true
		}
	}
	if !found {
		t.Errorf("validation should reject a dangling backend: %+v", issues)
	}
}

// Galera addressing inside one container: every member is 127.0.0.1, so gcomm,
// IST and SST must all be pinned to that member's slot ports. Leaving any at its
// default (4567/4568/4444) means the second member cannot bind and silently
// fails to join.
func TestAIOPXCGaleraAddressing(t *testing.T) {
	n := designNode{OS: "oraclelinux", AIOPXCMajor: "8.0", AIOInstances: []aioInstance{
		{ID: "c1", Kind: "pxc", Name: "pxc-cluster-01", Members: 3},
	}}
	members := aioPXCMembers(n, "pxc-cluster-01")
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	seenGroup, seenIST, seenSST := map[int]bool{}, map[int]bool{}, map[int]bool{}
	for _, m := range members {
		cnf := aioPXCSettings(m, n, "pxc-cluster-01", members)
		for _, want := range []string{
			fmt.Sprintf("gmcast.listen_addr=tcp://127.0.0.1:%d", m.Ports.Group),
			fmt.Sprintf("ist.recv_addr=127.0.0.1:%d", m.Ports.IST),
			fmt.Sprintf("wsrep_sst_receive_address=127.0.0.1:%d", m.Ports.SST),
			fmt.Sprintf("wsrep_node_address=127.0.0.1:%d", m.Ports.Group),
		} {
			if !strings.Contains(cnf, want) {
				t.Errorf("%s: missing %q", m.Inst, want)
			}
		}
		for _, def := range []string{":4567", ":4568", ":4444"} {
			if strings.Contains(cnf, def) {
				t.Errorf("%s uses a default Galera port %s", m.Inst, def)
			}
		}
		// Every member must see every peer in gcomm://.
		for _, p := range members {
			if !strings.Contains(cnf, fmt.Sprintf("127.0.0.1:%d", p.Ports.Group)) {
				t.Errorf("%s: gcomm list missing peer %s", m.Inst, p.Inst)
			}
		}
		if seenGroup[m.Ports.Group] || seenIST[m.Ports.IST] || seenSST[m.Ports.SST] {
			t.Errorf("%s shares a Galera port with another member", m.Inst)
		}
		seenGroup[m.Ports.Group], seenIST[m.Ports.IST], seenSST[m.Ports.SST] = true, true, true
	}
}

// Only ONE member may launch with --wsrep-new-cluster. The wrapper picks the
// seed on a fresh datadir, and otherwise defers to Galera's own
// safe_to_bootstrap marker so a cleanly stopped cluster restarts correctly.
func TestAIOPXCStartWrapper(t *testing.T) {
	l := aioLayout("pxc-cluster-01-n1", "pxc", aioPortsFor("pxc", 0, 0))
	seed := aioGaleraStartWrapper(l, true, "/usr/sbin/mysqld")
	joiner := aioGaleraStartWrapper(l, false, "/usr/sbin/mysqld")

	for _, s := range []string{seed, joiner} {
		if !strings.Contains(s, "safe_to_bootstrap") {
			t.Error("the wrapper should honour Galera's safe_to_bootstrap marker")
		}
		if !strings.Contains(s, "--defaults-file=") {
			t.Error("the wrapper must pass the instance's own config")
		}
	}
	if !strings.Contains(seed, "SEED=1") {
		t.Error("the seed wrapper should be marked as the seed")
	}
	if !strings.Contains(joiner, "SEED=0") {
		t.Error("a joiner must not be marked as the seed — two seeds split the brain")
	}
}

// Several PXC clusters in one node are supported and share the single install's
// version; what is NOT allowed is PXC beside Percona Server.
func TestAIOPXCMultipleClustersOneVersion(t *testing.T) {
	n := designNode{OS: "oraclelinux", AIOPXCMajor: "8.4", AIOInstances: []aioInstance{
		{ID: "c1", Kind: "pxc", Name: "pxc-cluster-01", Members: 3},
		{ID: "c2", Kind: "pxc", Name: "pxc-cluster-02", Members: 2},
	}}
	if flavor, conflict := aioMySQLFlavor(n.AIOInstances); flavor != flavorPXC || conflict {
		t.Fatalf("two PXC clusters should be a clean PXC node, got (%q,%v)", flavor, conflict)
	}
	if issues := aioIssues(n, designDoc{Nodes: []designNode{n}}, map[int][]string{}, 0); hasErrorIssue(issues) {
		t.Errorf("two PXC clusters should validate: %+v", issues)
	}
	major, _ := aioPXCMajor(n)
	if major != "8.4" {
		t.Errorf("node-level PXC major not honoured: %q", major)
	}
	// The two clusters must not share Galera ports.
	seen := map[int]string{}
	for _, g := range []string{"pxc-cluster-01", "pxc-cluster-02"} {
		for _, m := range aioPXCMembers(n, g) {
			for _, p := range []int{m.Ports.Client, m.Ports.Group, m.Ports.IST, m.Ports.SST} {
				if prev, dup := seen[p]; dup {
					t.Errorf("port %d shared by %s and %s", p, prev, m.Inst)
				}
				seen[p] = m.Inst
			}
		}
	}
}

func hasErrorIssue(issues []issue) bool {
	for _, is := range issues {
		if is.Level == "error" {
			return true
		}
	}
	return false
}

// The Synced probe must try BOTH an empty root password and the configured one.
// At this point in the sequence the seed has a freshly initialized datadir (root
// has no password yet — the baseline runs after) while a joiner's datadir is a
// byte copy of the donor's (root already HAS the donor's password). Probing only
// one made a healthy, Synced joiner report "state: unknown" until the deploy
// gave up — observed live.
func TestAIOPXCSyncedProbeTriesBothPasswords(t *testing.T) {
	if !strings.Contains(aioPXCWaitSyncedScript, "state()") ||
		!strings.Contains(aioPXCWaitSyncedScript, "state_pw()") {
		t.Fatal("the Synced probe should have both a passwordless and a password path")
	}
	if !strings.Contains(aioPXCWaitSyncedScript, `S=$(state); [ -z "$S" ] && S=$(state_pw)`) {
		t.Error("the probe should fall back to the configured password when the empty one fails")
	}
	if !strings.Contains(aioPXCWaitSyncedScript, "wsrep_local_state_comment") {
		t.Error("the probe should read wsrep_local_state_comment")
	}
}

// PostgreSQL has its own packaging conflict, verified by repoquery against a
// live container: repmgr_16 requires postgresql16-server, which
// percona-postgresql16-server does not provide. Unlike MySQL's node-wide flavor
// rule this one is scoped to a MAJOR, because PPG and PGDG packages only collide
// within the same major.
func TestAIOPGFlavorConflictIsPerMajor(t *testing.T) {
	sameMajor := []aioInstance{
		{ID: "a", Kind: "pg", Name: "pg16", PGMajor: "16"},
		{ID: "b", Kind: "repmgr", Name: "rm16", PGMajor: "16", Members: 2},
	}
	conf := aioPGFlavorConflicts(sameMajor)
	if len(conf) != 1 || conf["16"] == nil {
		t.Fatalf("Percona pg and PGDG repmgr on major 16 should conflict: %v", conf)
	}

	// Different majors co-install — this is the whole reason the rule is not
	// node-wide like MySQL's.
	diffMajor := []aioInstance{
		{ID: "a", Kind: "pg", Name: "pg16", PGMajor: "16"},
		{ID: "b", Kind: "repmgr", Name: "rm17", PGMajor: "17", Members: 2},
	}
	if c := aioPGFlavorConflicts(diffMajor); len(c) != 0 {
		t.Errorf("different majors should co-install, got conflicts: %v", c)
	}

	// Two Percona kinds on one major are fine.
	if c := aioPGFlavorConflicts([]aioInstance{
		{ID: "a", Kind: "pg", Name: "pg16", PGMajor: "16"},
		{ID: "b", Kind: "patroni", Name: "pat16", PGMajor: "16", Members: 3},
	}); len(c) != 0 {
		t.Errorf("pg + patroni share the Percona flavor and should not conflict: %v", c)
	}

	// The error must name both sides and explain the packaging reason.
	n := designNode{ID: "n1", Type: "aio", Label: "aio1", AIOInstances: sameMajor}
	var msg string
	for _, is := range aioIssues(n, designDoc{Nodes: []designNode{n}}, map[int][]string{}, 0) {
		if is.Level == "error" && strings.Contains(is.Message, "PGDG") {
			msg = is.Message
		}
	}
	if msg == "" {
		t.Fatal("expected a PostgreSQL flavor-conflict error")
	}
	for _, want := range []string{"pg16", "rm16", "postgresql16-server", "percona-postgresql16-server"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q: %s", want, msg)
		}
	}
}

// Every kind in the PostgreSQL family must declare which distribution it needs,
// or the conflict check silently ignores it.
func TestAIOEveryPGKindHasAFlavor(t *testing.T) {
	for _, k := range aioKinds {
		if k.Family != famPG {
			continue
		}
		if aioPGFlavorOfKind(k.Kind) == "" {
			t.Errorf("PostgreSQL kind %q has no declared distribution", k.Kind)
		}
	}
}

// repmgr inside one container: every member is 127.0.0.1, so conninfo must carry
// an explicit port (repmgr would otherwise dial 5432 and find nothing), and the
// service_*_commands must drive the instance's OWN unit — repmgr shells out to
// them during promote/follow, and its defaults assume the packaged postgresql-NN
// unit, which is masked here.
func TestAIORepmgrConf(t *testing.T) {
	n := designNode{OS: "oraclelinux", AIOInstances: []aioInstance{
		{ID: "r", Kind: "repmgr", Name: "rm01", Members: 3, PGMajor: "16"},
	}}
	cfg := aioConfig{Instances: aioPlan(n, "example.net", "aio1")}
	sec := pgFamilySecrets()
	seenID := map[string]bool{}
	for i, m := range cfg.Instances {
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		conf := aioRepmgrConf(l, m, i+1, "/usr/pgsql-16/bin", sec)
		if !strings.Contains(conf, fmt.Sprintf("port=%d", m.Ports.Client)) {
			t.Errorf("%s: conninfo lacks its own port", m.Inst)
		}
		if strings.Contains(conf, "port=5432") {
			t.Errorf("%s: conninfo uses the default port", m.Inst)
		}
		for _, verb := range []string{"start", "stop", "restart", "reload"} {
			want := fmt.Sprintf("service_%s_command='sudo systemctl %s %s'", verb, verb, l.Unit)
			if !strings.Contains(conf, want) {
				t.Errorf("%s: missing %s", m.Inst, want)
			}
		}
		if !strings.Contains(conf, "data_directory='"+l.DataDir+"'") {
			t.Errorf("%s: data_directory is not instance-scoped", m.Inst)
		}
		id := fmt.Sprintf("node_id=%d", i+1)
		if seenID[id] {
			t.Errorf("duplicate %s — repmgr node ids must be unique", id)
		}
		seenID[id] = true
	}
}

// A standby's datadir is cloned from the primary, so it arrives carrying the
// PRIMARY's postgresql.conf — including the primary's port. Without the rewrite
// the standby tries to bind a port already in use and never starts.
func TestAIORepmgrClonePinsStandbyPort(t *testing.T) {
	if !strings.Contains(aioRepmgrCloneScript, "# --- dbcanvas standby ---") {
		t.Error("the clone script should append a standby-specific config block")
	}
	if !strings.Contains(aioRepmgrCloneScript, "port = $PORT") {
		t.Error("the clone must pin the standby's own port over the cloned primary's")
	}
	if !strings.Contains(aioRepmgrCloneScript, `sed -i '/^# --- dbcanvas standby ---$/,$d'`) {
		t.Error("the standby block should be replaced, not stacked, on redeploy")
	}
}

// repmgrd daemonizes, so a Type=simple unit would see ExecStart exit 0 and mark
// the service dead — failover silently off. repmgr.go documents this for the
// classic path; the AiO unit must not repeat the mistake.
func TestAIORepmgrdUnitIsForking(t *testing.T) {
	l := aioLayout("rm01-n1", "repmgr", aioPortsFor("repmgr", 0, 0))
	body := fmt.Sprintf("Type=forking\nPIDFile=%s/repmgrd.pid", l.RunDir)
	if !strings.Contains(body, "Type=forking") {
		t.Fatal("sanity")
	}
	// The real assertion: the generated conf points repmgrd at the same pid file.
	conf := aioRepmgrConf(l, aioInstanceRuntime{Inst: l.Inst, Ports: l.Ports}, 1, "/usr/pgsql-16/bin", pgFamilySecrets())
	if !strings.Contains(conf, fmt.Sprintf("repmgrd_pid_file='%s/repmgrd.pid'", l.RunDir)) {
		t.Error("repmgr.conf and the repmgrd unit must agree on the pid file")
	}
}

// A pipeline exits with its LAST command's status, so "repmgr standby clone |
// tail" reports success even when the clone failed — the config rewrite then ran
// against a datadir that did not exist and surfaced as a baffling sed error.
func TestAIORepmgrCloneFailsLoudly(t *testing.T) {
	if strings.Contains(aioRepmgrCloneScript, "standby clone --fast-checkpoint -F 2>&1 | tail") {
		t.Error("the clone is piped into tail, which swallows its exit status")
	}
	if !strings.Contains(aioRepmgrCloneScript, `[ -s "$DATADIR/PG_VERSION" ] ||`) {
		t.Error("the script should assert the clone actually produced a datadir")
	}
}

// Patroni is the only PostgreSQL kind with no postgres unit of our own: Patroni
// runs initdb and starts/stops the server itself, so the instance's unit IS the
// agent (with a companion etcd unit). Everything a member binds — postgres, the
// REST API, etcd client and peer — must come from its own slot, because all
// three members share 127.0.0.1 and the defaults (5432/8008/2379/2380) are
// per-host.
func TestAIOPatroniAddressing(t *testing.T) {
	in := aioInstance{ID: "p", Kind: "patroni", Name: "pat01", Members: 3, PGMajor: "16"}
	n := designNode{OS: "oraclelinux", AIOInstances: []aioInstance{in}}
	cfg := aioConfig{Instances: aioPlan(n, "example.net", "aio1")}
	members := cfg.Instances
	sec := pgFamilySecrets()

	seen := map[int]string{}
	for _, m := range members {
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		y := aioPatroniYAML(l, m, in, "oraclelinux", members, sec)
		for _, want := range []string{
			fmt.Sprintf("listen: 0.0.0.0:%d", m.Ports.REST),
			fmt.Sprintf("connect_address: 127.0.0.1:%d", m.Ports.REST),
			fmt.Sprintf("listen: 0.0.0.0:%d", m.Ports.Client),
			"data_dir: " + l.DataDir,
			fmt.Sprintf("pgpass: %s/pgpass", l.RunDir),
		} {
			if !strings.Contains(y, want) {
				t.Errorf("%s: patroni.yml missing %q", m.Inst, want)
			}
		}
		for _, def := range []string{":8008", ":5432", ":2379", ":2380"} {
			if strings.Contains(y, def) {
				t.Errorf("%s: patroni.yml uses default port %s", m.Inst, def)
			}
		}
		// Every member must see every peer's etcd client port.
		for _, p := range members {
			if !strings.Contains(y, fmt.Sprintf("127.0.0.1:%d", p.Ports.EtcdCli)) {
				t.Errorf("%s: etcd host list missing peer %s", m.Inst, p.Inst)
			}
		}

		e := aioEtcdConf(l, m, "pat01", members)
		for _, want := range []string{
			fmt.Sprintf("listen-peer-urls: http://127.0.0.1:%d", m.Ports.EtcdPr),
			fmt.Sprintf("listen-client-urls: http://127.0.0.1:%d", m.Ports.EtcdCli),
			"data-dir: " + aioEtcdDataDir(l),
		} {
			if !strings.Contains(e, want) {
				t.Errorf("%s: etcd.yaml missing %q", m.Inst, want)
			}
		}
		// Each member's four ports must be unique across the whole cluster.
		for _, p := range []int{m.Ports.Client, m.Ports.REST, m.Ports.EtcdCli, m.Ports.EtcdPr} {
			if prev, dup := seen[p]; dup {
				t.Errorf("port %d shared by %s and %s", p, prev, m.Inst)
			}
			seen[p] = m.Inst
		}
	}
}

// A Patroni member's etcd must be up before the agent, and the agent unit must
// be the instance's own unit so `aioctl stop <inst>` takes PostgreSQL down too.
func TestAIOPatroniUnitOrdering(t *testing.T) {
	l := aioLayout("pat01-n1", "patroni", aioPortsFor("patroni", 0, 0))
	unit := aioUnitFile(l, aioUnitSpec{
		ExecStart: "/usr/bin/patroni " + aioPatroniConfPath(l),
		After:     []string{l.Unit + "-etcd.service"},
		Requires:  []string{l.Unit + "-etcd.service"},
		User:      "postgres", Group: "postgres",
	})
	if !strings.Contains(unit, "Requires="+l.Unit+"-etcd.service") {
		t.Error("the Patroni unit must require its etcd companion")
	}
	if !strings.Contains(unit, "After=network-online.target "+l.Unit+"-etcd.service") {
		t.Error("the Patroni unit must start after its etcd companion")
	}
	if !strings.Contains(unit, "ExecStart=/usr/bin/patroni "+l.ConfDir+"/patroni.yml") {
		t.Error("Patroni must be given its own config")
	}
	if !strings.Contains(unit, "PartOf="+aioTarget) {
		t.Error("the Patroni unit should still bind to aio.target")
	}
}

// etcd is Type=notify and does not report ready until the cluster has a quorum —
// which requires the other members to already be running. Starting them one at a
// time therefore deadlocks: the first member's start times out before the second
// exists. Observed live (all three units "failed", journal showing
// "start operation timed out"). They must be launched together, non-blocking.
func TestAIOEtcdStartsNonBlocking(t *testing.T) {
	if !strings.Contains(aioEtcdStartScript, "systemctl start --no-block") {
		t.Error("etcd units must be started with --no-block, or the first blocks on a quorum that cannot form")
	}
	if strings.Contains(aioEtcdStartScript, "enable --now") {
		t.Error("`enable --now` blocks on Type=notify readiness and deadlocks the bootstrap")
	}
	// The wait is a separate step, polling /health across every member.
	if !strings.Contains(aioEtcdWaitScript, `"health":"true"`) {
		t.Error("the readiness wait should poll etcd's /health endpoint")
	}
}

// Spock is the third PostgreSQL distribution and the least obvious: it installs
// no packages at all, it compiles a patched PostgreSQL into /usr/pgsql-NN — the
// SAME prefix the Percona and PGDG packages own. So it conflicts with both, and
// for a different reason (a file collision, not an RPM dependency).
func TestAIOSpockIsItsOwnPGFlavor(t *testing.T) {
	if aioPGFlavorOfKind("spock") == aioPGFlavorOfKind("repmgr") {
		t.Error("spock builds from source; it must not share PGDG's flavor")
	}
	if aioPGFlavorOfKind("spock") == aioPGFlavorOfKind("pg") {
		t.Error("spock must not share Percona's flavor")
	}
	// Spock beside either packaged distribution on the same major is a conflict.
	for _, other := range []struct{ kind, name string }{{"pg", "pg16"}, {"repmgr", "rm16"}} {
		c := aioPGFlavorConflicts([]aioInstance{
			{ID: "s", Kind: "spock", Name: "sp16", PGMajor: "16", Members: 2},
			{ID: "o", Kind: other.kind, Name: other.name, PGMajor: "16", Members: 2},
		})
		if len(c) != 1 {
			t.Errorf("spock + %s on major 16 should conflict, got %v", other.kind, c)
		}
	}
	// Different majors remain fine — the rule is per major.
	if c := aioPGFlavorConflicts([]aioInstance{
		{ID: "s", Kind: "spock", Name: "sp16", PGMajor: "16", Members: 2},
		{ID: "o", Kind: "pg", Name: "pg17", PGMajor: "17"},
	}); len(c) != 0 {
		t.Errorf("different majors should co-exist: %v", c)
	}
}

// The mesh is every node subscribing to every other, and each DSN must carry the
// peer's own port — the members are otherwise indistinguishable on 127.0.0.1.
func TestAIOSpockMeshDSNs(t *testing.T) {
	in := aioInstance{ID: "s", Kind: "spock", Name: "sp01", Members: 3, PGMajor: "16"}
	n := designNode{OS: "oraclelinux", AIOInstances: []aioInstance{in}}
	cfg := aioConfig{Instances: aioPlan(n, "example.net", "aio1")}
	sec := pgFamilySecrets()

	seen := map[string]bool{}
	for _, m := range cfg.Instances {
		dsn := aioSpockDSN(m, sec)
		if !strings.Contains(dsn, fmt.Sprintf("port=%d", m.Ports.Client)) {
			t.Errorf("%s: DSN lacks its own port: %s", m.Inst, dsn)
		}
		if strings.Contains(dsn, "port=5432") {
			t.Errorf("%s: DSN uses the default port", m.Inst)
		}
		if seen[dsn] {
			t.Errorf("two members share a DSN: %s", dsn)
		}
		seen[dsn] = true
	}
	// n*(n-1) subscription names, all distinct and all valid SQL identifiers
	// (spock sub names go into :'sub', and a dash would need quoting).
	names := map[string]bool{}
	for _, me := range cfg.Instances {
		for _, peer := range cfg.Instances {
			if me.Inst == peer.Inst {
				continue
			}
			sub := fmt.Sprintf("sub_%s_%s",
				strings.ReplaceAll(me.Inst, "-", "_"), strings.ReplaceAll(peer.Inst, "-", "_"))
			if strings.Contains(sub, "-") {
				t.Errorf("subscription name %q contains a dash", sub)
			}
			if names[sub] {
				t.Errorf("duplicate subscription name %q", sub)
			}
			names[sub] = true
		}
	}
	if len(names) != 6 {
		t.Errorf("a 3-node full mesh needs 6 subscriptions, got %d", len(names))
	}
}

// Spock's config must enable what the extension requires, on the instance's own
// port — and the build must not be routed through the package install loop.
func TestAIOSpockConfig(t *testing.T) {
	for _, want := range []string{
		"wal_level = logical", "shared_preload_libraries = 'spock'",
		"track_commit_timestamp = on", "port = $PORT",
		"unix_socket_directories = '$RUNDIR'",
	} {
		if !strings.Contains(aioSpockConfigScript, want) {
			t.Errorf("spock config missing %q", want)
		}
	}
	if strings.Contains(aioSpockConfigScript, "port = 5432") {
		t.Error("spock config hardcodes the default port")
	}
}

// Spock's PostgreSQL is compiled from source without --with-systemd, so it never
// sends the sd_notify READY signal. A Type=notify unit therefore sits in
// "activating" forever while the server is up and logging "ready to accept
// connections" — observed live. Packaged PostgreSQL does report readiness, so it
// keeps Type=notify.
func TestAIOSpockUnitIsNotNotify(t *testing.T) {
	pkgd := aioInstanceRuntime{Inst: "pg16", Kind: "pg", Ports: aioPortsFor("pg", 0, 0)}
	src := aioInstanceRuntime{Inst: "sp01-n1", Kind: "spock", Ports: aioPortsFor("spock", 1, 0)}

	unitType := func(m aioInstanceRuntime) string {
		if m.Kind == "spock" {
			return "simple"
		}
		return "notify"
	}
	if unitType(pkgd) != "notify" {
		t.Error("packaged PostgreSQL reports readiness and should stay Type=notify")
	}
	if unitType(src) != "simple" {
		t.Error("a source-built PostgreSQL cannot sd_notify; Type=notify would hang in activating")
	}
	// The compile really does omit --with-systemd — if that ever changes, this
	// test should be revisited rather than silently diverging.
	if strings.Contains(spockCompileScript, "--with-systemd") {
		t.Error("spockCompileScript now builds --with-systemd; the Spock unit could use Type=notify again")
	}
}

// ---------------------------------------------------------- app-wide targets

// The Query Runner, Benchmark and Data Generator all resolve a NODE to one
// connection. An All-in-One node breaks that three ways at once — many engines,
// many ports, none of them defaults — so a target id becomes composite. An
// ordinary node id must pass through completely unchanged.
func TestAIOTargetIDRoundTrip(t *testing.T) {
	node, inst := aioSplitTarget("abc-123")
	if node != "abc-123" || inst != "" {
		t.Errorf("a plain node id must pass through unchanged, got (%q,%q)", node, inst)
	}
	joined := aioJoinTarget("abc-123", "ps01")
	node, inst = aioSplitTarget(joined)
	if node != "abc-123" || inst != "ps01" {
		t.Errorf("composite id did not round-trip: %q → (%q,%q)", joined, node, inst)
	}
	// Instance names are DNS labels and node ids are uuids, so neither can
	// contain the separator — the split is unambiguous.
	for _, in := range everyKindDesign().AIOInstances {
		for _, m := range aioPlan(designNode{AIOInstances: []aioInstance{in}}, "example.net", "h") {
			if strings.Contains(m.Inst, aioTargetSep) {
				t.Errorf("instance name %q contains the target separator", m.Inst)
			}
		}
	}
}

// Only real databases are offered, and only ones it makes sense to write to.
func TestAIOTargetableInstances(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "pg", Name: "pg01", PGMajor: "16"},
		{ID: "c", Kind: "psmdbsharded", Name: "sh01", Members: 5},
		{ID: "d", Kind: "valkey", Name: "vk01"},
		{ID: "e", Kind: "haproxy", Name: "hap01", BackendInstance: "a"},
		{ID: "f", Kind: "orchestrator", Name: "orch01"},
	}}
	members := aioPlan(n, "example.net", "aio1")
	for i := range members {
		members[i].Ready = true
	}
	b, _ := json.Marshal(aioConfig{Instances: members})
	dep := Deployment{ContainerID: "c1", Config: b}

	got := map[string]bool{}
	for _, m := range aioTargetableInstances(dep) {
		got[m.Kind] = true
		if m.Role == "config" {
			t.Errorf("%s: a config server should not be offered as a target", m.Inst)
		}
	}
	for _, want := range []string{"ps", "pg", "psmdbsharded"} {
		if !got[want] {
			t.Errorf("%s should be a target", want)
		}
	}
	for _, unwanted := range []string{"valkey", "haproxy", "orchestrator"} {
		if got[unwanted] {
			t.Errorf("%s has no SQL/Mongo schema and should not be a target", unwanted)
		}
	}
	// An instance that was planned but never built must not be offered.
	members[0].Ready = false
	b2, _ := json.Marshal(aioConfig{Instances: members})
	for _, m := range aioTargetableInstances(Deployment{ContainerID: "c1", Config: b2}) {
		if m.Inst == "ps01" {
			t.Error("an unbuilt instance must not appear as a target")
		}
	}
}

// A target's port must be the instance's own — the whole point of the node type
// is that nothing is on a default, so an inferred port reaches nothing.
func TestAIOInstanceCredsUseSlotPorts(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "pg", Name: "pg01", PGMajor: "16"},
		{ID: "c", Kind: "psmdb", Name: "mdb01"},
	}}
	members := aioPlan(n, "example.net", "aio1")
	b, _ := json.Marshal(aioConfig{Instances: members})
	dep := Deployment{ContainerID: "c1", Config: b, Secrets: []byte(`{"adminUser":"admin","adminPassword":"pw"}`)}

	defaults := map[string]int{"mysql": 3306, "postgres": 5432, "mongodb": 27017}
	for _, m := range members {
		engine, port, user, _ := aioInstanceCreds(dep, m)
		if engine == "" {
			t.Fatalf("%s: no engine resolved", m.Inst)
		}
		if port != m.Ports.Client {
			t.Errorf("%s: port %d is not the instance's own %d", m.Inst, port, m.Ports.Client)
		}
		if port == defaults[engine] {
			t.Errorf("%s: resolved the %s default port", m.Inst, engine)
		}
		if user == "" {
			t.Errorf("%s: no user resolved", m.Inst)
		}
	}
}

// The Data Generator execs a client INSIDE the container, where several servers
// live — so the connection must carry arguments selecting this one, and for
// PostgreSQL the psql matching its major (PATH points at only one).
func TestAIODBConnSelectsTheRightServer(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01"},
		{ID: "b", Kind: "pg", Name: "pg17", PGMajor: "17"},
	}}
	members := aioPlan(n, "example.net", "aio1")
	for i := range members {
		members[i].Ready = true
	}
	b, _ := json.Marshal(aioConfig{Instances: members})
	dep := Deployment{ContainerID: "c1", Config: b, Secrets: []byte(`{"rootUser":"root","rootPassword":"pw"}`)}
	app := &App{}

	my, ok := app.aioDBConn(Stack{}, dep, "ps01")
	if !ok {
		t.Fatal("mysql instance did not resolve")
	}
	l := aioLayout("ps01", "ps", members[0].Ports)
	if strings.Join(my.Args, " ") != "--socket="+l.Sock {
		t.Errorf("mysql connection should select its own socket, got %v", my.Args)
	}
	if got := my.client("mysql"); got[0] != "mysql" {
		t.Errorf("mysql client should come from PATH, got %v", got)
	}

	pg, ok := app.aioDBConn(Stack{}, dep, "pg17")
	if !ok {
		t.Fatal("postgres instance did not resolve")
	}
	if pg.Bin != "/usr/pgsql-17/bin" {
		t.Errorf("psql should match the instance's major, got %q", pg.Bin)
	}
	joined := strings.Join(pg.client("psql"), " ")
	if !strings.HasPrefix(joined, "/usr/pgsql-17/bin/psql") {
		t.Errorf("psql path not applied: %s", joined)
	}
	if !strings.Contains(joined, strconv.Itoa(members[1].Ports.Client)) {
		t.Errorf("psql args should carry the instance's port: %s", joined)
	}
	// A classic connection must be entirely unaffected.
	plain := dbConn{Engine: "mysql"}
	if got := strings.Join(plain.client("mysql"), " "); got != "mysql" {
		t.Errorf("a classic connection should add nothing, got %q", got)
	}
}

// The instance form used to render controls for PMM, LDAP, OpenBao and SeaweedFS
// while NO provisioner read any of them: the box was ticked, the deploy
// succeeded, and nothing happened. A silent no-op is worse than an absent
// feature, so anything not wired is rejected by name — and the moment its
// provisioner lands it drops out of this list.
func TestAIOUnimplementedOptionsAreRejected(t *testing.T) {
	cases := []struct {
		name string
		in   aioInstance
		want string
	}{
		{"ldap", aioInstance{ID: "a", Kind: "ps", Name: "ps01", LdapAuth: true}, "directory (LDAP)"},
		{"vault", aioInstance{ID: "a", Kind: "ps", Name: "ps01", EnableVault: true}, "OpenBao"},
		{"seaweedfs", aioInstance{ID: "a", Kind: "pg", Name: "pg01", SeaweedFSNodeID: "sw1"}, "SeaweedFS"},
		// TLS is wired for the database engines, so the gated case is an engine
		// that is not — see TestAIOTLSGatingFollowsSupport.
		{"tls", aioInstance{ID: "a", Kind: "valkey", Name: "vk01", GenerateCert: true}, "TLS certificates"},
		{"oidc", aioInstance{ID: "a", Kind: "psmdb", Name: "m01", EnableOIDC: true}, "OIDC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := designNode{ID: "n1", Type: "aio", Label: "aio1", AIOInstances: []aioInstance{tc.in}}
			found := false
			for _, is := range aioIssues(n, designDoc{Nodes: []designNode{n}}, map[int][]string{}, 0) {
				if is.Level == "error" && strings.Contains(is.Message, tc.want) &&
					strings.Contains(is.Message, "does not implement yet") {
					found = true
				}
			}
			if !found {
				t.Errorf("enabling %s should be rejected until it is wired", tc.name)
			}
		})
	}

	// PMM *is* implemented, so it must NOT be rejected.
	n := designNode{ID: "n1", Type: "aio", Label: "aio1", AIOInstances: []aioInstance{
		{ID: "a", Kind: "ps", Name: "ps01", PMMNodeID: "pmm1"},
	}}
	doc := designDoc{Nodes: []designNode{n, {ID: "pmm1", Type: "pmm", Label: "pmm"}}}
	for _, is := range aioIssues(n, doc, map[int][]string{}, 0) {
		if is.Level == "error" {
			t.Errorf("PMM monitoring is implemented and should validate: %s", is.Message)
		}
	}
	if len(aioUnimplementedOptions(aioInstance{PMMNodeID: "pmm1"})) != 0 {
		t.Error("PMM must not be listed as unimplemented")
	}
	// A plain instance with nothing enabled is clean.
	if len(aioUnimplementedOptions(aioInstance{Kind: "ps", Name: "ps01"})) != 0 {
		t.Error("an instance with no extras should report nothing unimplemented")
	}
}

// PMM registration must name a service per INSTANCE, not per node, or several
// instances overwrite one PMM service and only the last reports. It must also
// use the instance's own socket/port rather than the product defaults.
func TestAIOPMMScriptIsInstanceScoped(t *testing.T) {
	if strings.Contains(aioPMMRegisterScript, "/var/lib/mysql/mysql.sock") {
		t.Error("the PMM script uses the shared MySQL socket; it must use the instance's")
	}
	for _, want := range []string{`--socket="$SOCK"`, `--port="$PORT"`, `"$SVC"`} {
		if !strings.Contains(aioPMMRegisterScript, want) {
			t.Errorf("PMM script missing %s", want)
		}
	}
	// All three database engines are handled.
	for _, eng := range []string{"mysql)", "postgres)", "mongodb)"} {
		if !strings.Contains(aioPMMRegisterScript, eng) {
			t.Errorf("PMM script has no branch for %s", eng)
		}
	}
}

// TLS is enabled, never REQUIRED. Requiring it would instantly break every
// existing plaintext connection — replica-set members talking to each other,
// `aioctl connect`, and the app's own tooling — with nothing telling the user
// why. Each engine also wants the material in a different shape.
func TestAIOTLSWiringPerEngine(t *testing.T) {
	if strings.Contains(aioCertWireMongo, "requireTLS") {
		t.Error("MongoDB must use preferTLS; requireTLS breaks existing plaintext clients")
	}
	if !strings.Contains(aioCertWireMongo, "preferTLS") {
		t.Error("MongoDB TLS mode not set")
	}
	// MongoDB needs key+cert combined; the others need them separate.
	if !strings.Contains(aioCertScript, `cat "$DIR/server-key.pem" "$DIR/server-cert.pem" > "$DIR/server.pem"`) {
		t.Error("MongoDB needs a combined key+certificate PEM")
	}
	if !strings.Contains(aioCertWireMongo, "server.pem") {
		t.Error("mongod.conf should point at the combined PEM")
	}
	if !strings.Contains(aioCertWireMySQL, "ssl-cert=$DIR/server-cert.pem") {
		t.Error("MySQL should point at the separate certificate")
	}
	if !strings.Contains(aioCertWirePostgres, "ssl_key_file = '$DIR/server-key.pem'") {
		t.Error("PostgreSQL should point at the separate key")
	}
	// The two append-based wirings must strip the previous block before adding a
	// new one. MongoDB is deliberately absent: it edits YAML structurally rather
	// than appending, and asserting it contained this marker is exactly what let a
	// non-idempotent editor through — replacement is now proven by behaviour in
	// TestAIOTLSWiringIsIdempotent, which runs every script twice and compares.
	for name, script := range map[string]string{
		"mysql": aioCertWireMySQL, "postgres": aioCertWirePostgres,
	} {
		if !strings.Contains(script, `sed -i '/^# --- dbcanvas tls ---$/,$d'`) {
			t.Errorf("%s TLS block is appended without replacing the previous one", name)
		}
	}
	// The certificate must not land in the shared classic location.
	if strings.Contains(aioCertScript, "/var/lib/mysql") {
		t.Error("the certificate script uses the shared MySQL datadir")
	}
}

// TLS leaves the unimplemented list only for the engines actually wired.
func TestAIOTLSGatingFollowsSupport(t *testing.T) {
	for _, kind := range []string{"ps", "psrepl", "pg", "repmgr", "psmdb", "psmrs"} {
		if !aioTLSSupported(kind) {
			t.Errorf("%s should support TLS", kind)
		}
		if opts := aioUnimplementedOptions(aioInstance{Kind: kind, GenerateCert: true}); len(opts) != 0 {
			t.Errorf("%s: TLS is wired and should not be gated, got %v", kind, opts)
		}
	}
	for _, kind := range []string{"valkey", "valkeycluster", "proxysql", "haproxy", "orchestrator"} {
		if aioTLSSupported(kind) {
			t.Errorf("%s TLS is not wired and must not claim support", kind)
		}
		opts := aioUnimplementedOptions(aioInstance{Kind: kind, GenerateCert: true})
		if len(opts) == 0 {
			t.Errorf("%s: TLS is not wired and must stay gated", kind)
		}
	}
}

// The certificate's CN must be the INSTANCE's DNS name, not the node's, or a
// client verifying the host it dialled sees a mismatch.
func TestAIOCertUsesInstanceFQDN(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{ID: "a", Kind: "psrepl", Name: "repl01", Members: 2, GenerateCert: true},
	}}
	seen := map[string]bool{}
	for _, m := range aioPlan(n, "example.net", "aio1") {
		if m.FQDN == "aio1.example.net" {
			t.Errorf("%s: certificate would carry the node's name, not its own", m.Inst)
		}
		if !strings.HasPrefix(m.FQDN, m.Inst+".") {
			t.Errorf("%s: FQDN %q is not the instance's own name", m.Inst, m.FQDN)
		}
		if seen[m.FQDN] {
			t.Errorf("two members share the certificate CN %q", m.FQDN)
		}
		seen[m.FQDN] = true
	}
}

// A reference to an icon that does not exist renders `<undefined />`, which
// React rejects with "Element type is invalid" — blanking the WHOLE page, not
// just the component. `Icon.ChevronDown` did exactly that: the designer looked
// fine until the first instance card rendered, i.e. the moment a feature was
// added. Nothing in `vite build`, `go vet` or the unit suite catches it, because
// it is a runtime property lookup on an object.
//
// So: every Icon.X the All-in-One components reference must exist in Icons.jsx.
func TestAIOIconsExist(t *testing.T) {
	icons, err := os.ReadFile("web/src/components/Icons.jsx")
	if err != nil {
		t.Skipf("icon set not readable: %v", err)
	}
	available := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s{2}([A-Za-z][A-Za-z0-9]*):\s*\(`).FindAllStringSubmatch(string(icons), -1) {
		available[m[1]] = true
	}
	if len(available) == 0 {
		t.Fatal("parsed no icons out of Icons.jsx — the guard would pass vacuously")
	}

	for _, page := range []string{"web/src/pages/AllInOne.jsx"} {
		src, err := os.ReadFile(page)
		if err != nil {
			t.Errorf("%s: %v", page, err)
			continue
		}
		used := map[string]bool{}
		for _, m := range regexp.MustCompile(`Icon\.([A-Za-z][A-Za-z0-9]*)`).FindAllStringSubmatch(string(src), -1) {
			used[m[1]] = true
		}
		if len(used) == 0 {
			t.Errorf("%s: found no Icon references — the guard is not actually checking anything", page)
		}
		for name := range used {
			if !available[name] {
				t.Errorf("%s references Icon.%s, which does not exist — this blanks the page at runtime", page, name)
			}
		}
	}
}

// A shell pipeline exits with its LAST command's status, so piping a command
// whose failure matters into `tail` reports success regardless. That defect hid
// a failed `repmgr standby clone` behind a baffling sed error (session 199), and
// an audit later found the SAME shape in both `repmgr … register` scripts —
// which had been called verified, because on that run the registration happened
// to succeed.
//
// This scans every AiO script constant rather than the two that were found, so
// the next one is caught when it is written.
func TestAIOScriptsDoNotMaskExitStatus(t *testing.T) {
	// Commands whose failure must fail the step. Read-only probes are excluded:
	// piping those into grep/awk IS the test, which is a different intent.
	mutating := regexp.MustCompile(`\b(repmgr|initdb|pg_basebackup|mongosh --quiet --port "\$PORT" --eval 'try \{ rs\.initiate|pmm-admin add|sh\.addShard)\b`)
	pipeInto := regexp.MustCompile(`\|\s*(tail|head)\b`)

	scripts := map[string]string{
		"aioRepmgrPrimaryRegisterScript": aioRepmgrPrimaryRegisterScript,
		"aioRepmgrStandbyRegisterScript": aioRepmgrStandbyRegisterScript,
		"aioRepmgrCloneScript":           aioRepmgrCloneScript,
		"aioRepmgrPrimaryConfigScript":   aioRepmgrPrimaryConfigScript,
		"aioPGInitScript":                aioPGInitScript,
		"aioPGConfigureScript":           aioPGConfigureScript,
		"aioPGPasswordScript":            aioPGPasswordScript,
		"aioMongoInitRSScript":           aioMongoInitRSScript,
		"aioMongoAddShardsScript":        aioMongoAddShardsScript,
		"aioMongoCreateAdminScript":      aioMongoCreateAdminScript,
		"aioPMMRegisterScript":           aioPMMRegisterScript,
		"aioSpockNodeSetupScript":        aioSpockNodeSetupScript,
		"aioSpockSubCreateScript":        aioSpockSubCreateScript,
		"aioMySQLBaselineScript":         aioMySQLBaselineScript,
		"aioMySQLAttachScript":           aioMySQLAttachScript,
	}

	for name, script := range scripts {
		for i, line := range strings.Split(script, "\n") {
			code, _, _ := strings.Cut(strings.TrimSpace(line), "#")
			if code == "" || !pipeInto.MatchString(code) || !mutating.MatchString(code) {
				continue
			}
			// A guarded pipeline is fine: `cmd || { …; exit 1; }` and the
			// diagnostic tails inside an already-failing branch both keep the
			// status. What is not fine is a bare `mutating-cmd | tail`.
			if strings.Contains(code, "||") || strings.HasPrefix(code, "echo ") {
				continue
			}
			t.Errorf("%s line %d pipes a mutating command into tail/head, "+
				"which discards its exit status:\n    %s", name, i+1, code)
		}
	}
}

// The register steps must not merely trust repmgr's exit code — verify the row
// landed. `-F` (force) makes several failure modes non-fatal to the binary.
func TestAIORepmgrRegisterVerifiesTheRow(t *testing.T) {
	for name, script := range map[string]string{
		"primary": aioRepmgrPrimaryRegisterScript,
		"standby": aioRepmgrStandbyRegisterScript,
	} {
		if !strings.Contains(script, "FROM repmgr.nodes") {
			t.Errorf("%s register does not confirm the node reached repmgr.nodes", name)
		}
		if !strings.Contains(script, "OUT=$(runuser") {
			t.Errorf("%s register should capture output rather than pipe it away", name)
		}
	}
}

// The registry path and the target name appear in Go (which writes them) and in
// aioctl (which reads them). They were three separate literals; if one drifted,
// aioctl would look at a file nobody writes and every manager action would fail
// with "no instance registry". They are now built from the same constants — this
// asserts the generated script really carries them, since a raw-string
// concatenation is easy to get subtly wrong.
func TestAIOCtlUsesTheSharedPaths(t *testing.T) {
	if !strings.Contains(aioCtlScript, "REG="+aioRegistry+"\n") {
		t.Errorf("aioctl's REG is not the shared constant %q", aioRegistry)
	}
	if !strings.Contains(aioCtlScript, "TARGET="+aioTarget+"\n") {
		t.Errorf("aioctl's TARGET is not the shared constant %q", aioTarget)
	}
	// And the literal must still be the real path, not an empty or mangled one.
	if aioRegistry != "/etc/dbcanvas/aio/instances.tsv" {
		t.Errorf("registry path changed to %q — update the operator guide too", aioRegistry)
	}
	// The Go writer must target the same basename it advertises.
	if !strings.HasSuffix(aioRegistry, "/"+aioRegistryName) {
		t.Errorf("aioRegistry %q does not end in aioRegistryName %q", aioRegistry, aioRegistryName)
	}
}

// Every script this package generates is assembled in Go — raw strings spliced
// with constants, shared fragments concatenated into bootstrap and join
// variants — and then run inside a container where a syntax error surfaces as a
// deploy failure with a shell diagnostic three steps from the cause.
//
// `bash -n` parses without executing, so the whole set can be checked here. It
// costs nothing and covers the scripts that no unit test can otherwise reach.
func TestAIOGeneratedScriptsAreValidShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scripts := map[string]string{
		"aioCtlScript":                   aioCtlScript,
		"aioPrepControlScript":           aioPrepControlScript,
		"aioMaskVendorUnits":             aioMaskVendorUnits,
		"aioStartUnitScript":             aioStartUnitScript,
		"aioRestartUnitScript":           aioRestartUnitScript,
		"aioMySQLInitScript":             aioMySQLInitScript,
		"aioMySQLBaselineScript":         aioMySQLBaselineScript,
		"aioMySQLAttachScript":           aioMySQLAttachScript,
		"aioMySQLSemisyncScript":         aioMySQLSemisyncScript,
		"aioMySQLGRBootstrapScript":      aioMySQLGRBootstrapScript,
		"aioMySQLGRJoinScript":           aioMySQLGRJoinScript,
		"aioPXCWaitSyncedScript":         aioPXCWaitSyncedScript,
		"aioPGInitScript":                aioPGInitScript,
		"aioPGConfigureScript":           aioPGConfigureScript,
		"aioPGPasswordScript":            aioPGPasswordScript,
		"aioRepmgrPrimaryConfigScript":   aioRepmgrPrimaryConfigScript,
		"aioRepmgrPrimaryRegisterScript": aioRepmgrPrimaryRegisterScript,
		"aioRepmgrCloneScript":           aioRepmgrCloneScript,
		"aioRepmgrStandbyRegisterScript": aioRepmgrStandbyRegisterScript,
		"aioRepmgrConfOwnScript":         aioRepmgrConfOwnScript,
		"aioEtcdDirScript":               aioEtcdDirScript,
		"aioEtcdStartScript":             aioEtcdStartScript,
		"aioEtcdWaitScript":              aioEtcdWaitScript,
		"aioPatroniOwnScript":            aioPatroniOwnScript,
		"aioPatroniWaitScript":           aioPatroniWaitScript,
		"aioSpockConfigScript":           aioSpockConfigScript,
		"aioSpockNodeSetupScript":        aioSpockNodeSetupScript,
		"aioSpockSubCreateScript":        aioSpockSubCreateScript,
		"aioMongoKeyFileScript":          aioMongoKeyFileScript,
		"aioMongoWaitScript":             aioMongoWaitScript,
		"aioMongoInitRSScript":           aioMongoInitRSScript,
		"aioMongoCreateAdminScript":      aioMongoCreateAdminScript,
		"aioMongoAddShardsScript":        aioMongoAddShardsScript,
		"aioValkeyOwnConfScript":         aioValkeyOwnConfScript,
		"aioValkeyClusterCreateScript":   aioValkeyClusterCreateScript,
		"aioHAProxyCheckScript":          aioHAProxyCheckScript,
		"aioProxySQLOwnScript":           aioProxySQLOwnScript,
		"aioProxySQLLoadScript":          aioProxySQLLoadScript,
		"aioOrchDiscoverScript":          aioOrchDiscoverScript,
		"aioCertScript":                  aioCertScript,
		"aioCertWireMySQL":               aioCertWireMySQL,
		"aioCertWirePostgres":            aioCertWirePostgres,
		"aioCertWireMongo":               aioCertWireMongo,
		"aioPMMRegisterScript":           aioPMMRegisterScript,
	}
	dir := t.TempDir()
	for name, script := range scripts {
		if strings.TrimSpace(script) == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		f := filepath.Join(dir, name+".sh")
		if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("bash", "-n", f).CombinedOutput()
		if err != nil {
			t.Errorf("%s is not valid shell:\n%s", name, strings.TrimSpace(string(out)))
		}
	}
}

// The TLS wiring is applied again on every re-issue and redeploy, so it must be
// idempotent — and the previous test only checked that each script CONTAINED a
// `sed … /,$d` marker. The MongoDB one did, and was still wrong: its editor
// dropped the `tls:` line but left the indented children, so a second run
// appended a fresh block beside the orphans and produced duplicate keys under
// `net:`. A test that asserts a marker is not a test of the behaviour.
//
// This runs each script twice against a realistic config and compares the two
// results. Anything that stacks shows up as a difference.
func TestAIOTLSWiringIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available (the MongoDB editor needs it)")
	}
	dir := t.TempDir()

	cases := []struct {
		name    string
		script  string
		file    string
		initial string
		env     func(path string) []string
	}{
		{
			name: "mysql", script: aioCertWireMySQL, file: "my.cnf",
			initial: "[mysqld]\nport=13000\ndatadir=/opt/aio/x/data\n",
			env:     func(p string) []string { return []string{"CNF=" + p, "DIR=/opt/aio/x/tls"} },
		},
		{
			name: "postgres", script: aioCertWirePostgres, file: "postgresql.conf",
			initial: "port = 15000\nlisten_addresses = '*'\n",
			env:     func(p string) []string { return []string{"DATADIR=" + filepath.Dir(p), "DIR=/opt/aio/y/tls"} },
		},
		{
			name: "mongodb", script: aioCertWireMongo, file: "mongod.conf",
			initial: "storage:\n  dbPath: /opt/aio/m/data\nnet:\n  port: 17000\n  bindIpAll: true\nsecurity:\n  authorization: enabled\n",
			env:     func(p string) []string { return []string{"CNF=" + p, "DIR=/opt/aio/m/tls"} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := filepath.Join(dir, tc.name)
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			conf := filepath.Join(sub, tc.file)
			if err := os.WriteFile(conf, []byte(tc.initial), 0o644); err != nil {
				t.Fatal(err)
			}
			sh := filepath.Join(sub, "wire.sh")
			if err := os.WriteFile(sh, []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}
			run := func() string {
				cmd := exec.Command("bash", sh)
				// chown to a vendor user will fail as an ordinary test user; the
				// scripts tolerate it, and the config edit is what is under test.
				cmd.Env = append(os.Environ(), tc.env(conf)...)
				out, err := cmd.CombinedOutput()
				if err != nil && !strings.Contains(string(out), "chown") {
					t.Fatalf("script failed: %v\n%s", err, out)
				}
				b, rerr := os.ReadFile(conf)
				if rerr != nil {
					t.Fatal(rerr)
				}
				return string(b)
			}
			first := run()
			second := run()
			if first != second {
				t.Errorf("not idempotent — a second run changed the config:\n--- after 1 ---\n%s\n--- after 2 ---\n%s", first, second)
			}
			// And the settings must appear exactly once, not merely stably.
			for _, key := range map[string][]string{
				"mysql":    {"ssl-ca=", "ssl-cert=", "ssl-key="},
				"postgres": {"ssl = on", "ssl_cert_file", "ssl_key_file"},
				"mongodb":  {"tls:", "mode: preferTLS", "certificateKeyFile", "CAFile"},
			}[tc.name] {
				if n := strings.Count(second, key); n != 1 {
					t.Errorf("%q appears %d times after two runs, want 1:\n%s", key, n, second)
				}
			}
			// The original settings must survive.
			if !strings.Contains(second, "17000") && tc.name == "mongodb" {
				t.Error("the MongoDB editor dropped the original port")
			}
		})
	}
}

// Every version field the design model carries must be reachable from the form.
// They were not: the model had majors AND minors for six families and the
// provisioners passed them through as $VER, but the form rendered only the
// Percona Server and PXC majors — so a user could not choose a minor at all, and
// four families had no version control whatsoever.
//
// This pins the wiring by checking the form references each field, and that the
// Go side still consumes it. It is a static check, but the failure it guards
// against is silent: a picker that is simply absent looks like a design choice.
func TestAIOVersionFieldsAreReachableFromTheForm(t *testing.T) {
	form, err := os.ReadFile("web/src/pages/AllInOne.jsx")
	if err != nil {
		t.Skipf("form not readable: %v", err)
	}
	src := string(form)

	// JSON name -> the Go field that consumes it, so a rename breaks loudly.
	fields := map[string]string{
		"aioPsMajor":             "AIOPSMajor",
		"aioPsVersion":           "AIOPSVersion",
		"aioPxcMajor":            "AIOPXCMajor",
		"aioPxcVersion":          "AIOPXCVersion",
		"aioPsmdbMajor":          "AIOPSMDBMajor",
		"aioPsmdbVersion":        "AIOPSMDBVersion",
		"aioValkeyMajor":         "AIOValkeyMajor",
		"aioValkeyVersion":       "AIOValkeyVer",
		"aioProxysqlMajor":       "AIOProxySQLMajor",
		"aioProxysqlVersion":     "AIOProxySQLVer",
		"aioOrchestratorVersion": "AIOOrchVersion",
	}
	for jsonName := range fields {
		if !strings.Contains(src, jsonName) {
			t.Errorf("the form never references %q, so that version cannot be selected", jsonName)
		}
	}

	// PostgreSQL is per instance rather than node-level.
	for _, perInstance := range []string{"pgMajor", "pgVersion"} {
		if !strings.Contains(src, perInstance) {
			t.Errorf("the instance form never references %q", perInstance)
		}
	}

	// And the Go struct must still carry them under those JSON names.
	model, err := os.ReadFile("intranet.go")
	if err != nil {
		t.Fatal(err)
	}
	for jsonName, goField := range fields {
		// gofmt aligns struct fields, so the gap between name, type and tag is
		// variable — match on whitespace rather than an exact string.
		re := regexp.MustCompile(goField + `\s+string\s+` + "`" + `json:"` + jsonName + `"`)
		if !re.Match(model) {
			t.Errorf("designNode no longer declares %s as json:%q — the form would silently stop working", goField, jsonName)
		}
	}
}

// A minor is only meaningful if it reaches the installer. Each family's
// provisioner must pass its version through as $VER.
func TestAIOMinorVersionsReachTheInstaller(t *testing.T) {
	for name, src := range map[string]string{
		"mysql":        readSrc(t, "aio_mysql.go"),
		"mongodb":      readSrc(t, "aio_mongo.go"),
		"valkey":       readSrc(t, "aio_valkey.go"),
		"proxysql":     readSrc(t, "aio_proxy.go"),
		"orchestrator": readSrc(t, "aio_orch.go"),
		"postgres":     readSrc(t, "aio_pg.go"),
	} {
		if !strings.Contains(src, `"VER="`) && !strings.Contains(src, `"VER=" +`) {
			t.Errorf("%s provisioner never passes VER= to its install script, so a chosen minor is ignored", name)
		}
	}
}

func readSrc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
