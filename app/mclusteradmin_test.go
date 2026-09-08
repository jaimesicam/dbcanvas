package main

import (
	"strings"
	"testing"
)

// The role sets are the reason this feature exists, and they are somebody else's:
// they come from MClusterAdmin's own README, worked out per dashboard. A test that
// only asserted "some roles" would let a well-meaning edit quietly drop the one that
// makes a panel tab go blank, so it asserts the list.
func TestMCARolesAreTheDocumentedSets(t *testing.T) {
	full := mcaRolesJS(false)
	for _, want := range []string{
		`{role:"clusterMonitor",db:"admin"}`,       // topology, RS + sharding status
		`{role:"clusterManager",db:"admin"}`,       // balancer, replSetReconfig
		`{role:"hostManager",db:"admin"}`,          // killOp, flow control
		`{role:"dbAdminAnyDatabase",db:"admin"}`,   // drop index, profiling
		`{role:"readAnyDatabase",db:"admin"}`,      // explain on user collections
		`{role:"userAdminAnyDatabase",db:"admin"}`, // user + role management
		`{role:"read",db:"local"}`,                 // oplog.rs, for Oplog Stats
	} {
		if !strings.Contains(full, want) {
			t.Errorf("the full role set is missing %s", want)
		}
	}
	// Upstream is explicit that it avoids these two so the panel cannot drop a
	// database. Granting root would "work" and would throw that away.
	for _, never := range []string{`role:"root"`, `role:"clusterAdmin"`, `readWriteAnyDatabase`} {
		if strings.Contains(full, never) {
			t.Errorf("the full role set grants %s — it is deliberately least-privilege", never)
		}
	}

	ro := mcaRolesJS(true)
	for _, want := range []string{`{role:"clusterMonitor",db:"admin"}`, `{role:"readAnyDatabase",db:"admin"}`, `{role:"read",db:"local"}`} {
		if !strings.Contains(ro, want) {
			t.Errorf("the read-only role set is missing %s", want)
		}
	}
	// Every role that can write is what "read-only" means here.
	for _, never := range []string{"clusterManager", "hostManager", "dbAdminAnyDatabase", "userAdminAnyDatabase"} {
		if strings.Contains(ro, never) {
			t.Errorf("the read-only role set grants %s", never)
		}
	}

	if mcaAdminUser != "madmin" || mcaReadOnlyUser != "madmin-ro" {
		t.Errorf("account names are %q/%q, want madmin/madmin-ro (upstream's own naming)",
			mcaAdminUser, mcaReadOnlyUser)
	}
}

// A redeploy runs the same step again, and a mode change rewrites the roles. Both
// go through updateUser rather than failing on "already exists" — otherwise the
// second deploy of a stack fails, and switching full→read-only would leave an
// account still holding every write privilege.
func TestMCAUserJSIsIdempotent(t *testing.T) {
	js := mcaUsersJS("pw", "ropw")
	for _, want := range []string{"createUser", "already exists", "updateUser", `"madmin"`, `"madmin-ro"`} {
		if !strings.Contains(js, want) {
			t.Errorf("the user script does not %s: %s", want, js)
		}
	}
	// The mechanism is pinned on both accounts, on create AND on update: an account
	// left over from an earlier deploy carries whatever it was made with, and the
	// update is the only thing that fixes it.
	if strings.Count(js, `mechanisms:["SCRAM-SHA-256"]`) != 4 {
		t.Errorf("SCRAM-SHA-256 is not pinned on both accounts' create and update: %s", js)
	}
	// The password reaches mongosh as a quoted literal, so a shell-hostile one is
	// not an injection: %q is what does that, and it is easy to "simplify" away.
	js = mcaUserJS("madmin", `p"w'$(id)`, mcaRolesJS(true))
	if strings.Contains(js, `$(id)`) && !strings.Contains(js, `\"`) {
		t.Errorf("the password is not quoted for mongosh: %s", js)
	}
}

// mcaSecretsFor is what decides whether an account exists at all, and it must keep
// the password stable across redeploys — a changed one silently breaks a connection
// string somebody already pasted into the panel.
func TestMCASecretsOnlyWhenAskedFor(t *testing.T) {
	var off mongoSecrets
	mcaSecretsFor(&off, false, "pw", "ropw")
	if off.MCAUser != "" || off.MCAPassword != "" {
		t.Errorf("no accounts were asked for, but got %+v", off)
	}

	var on mongoSecrets
	mcaSecretsFor(&on, true, "pw", "ropw")
	if on.MCAUser != "madmin" || on.MCAPassword != "pw" {
		t.Errorf("got %q/%q, want madmin/pw", on.MCAUser, on.MCAPassword)
	}
	if on.MCAReadOnlyUser != "madmin-ro" || on.MCAReadOnlyPassword != "ropw" {
		t.Errorf("got %q/%q, want madmin-ro/ropw", on.MCAReadOnlyUser, on.MCAReadOnlyPassword)
	}
}

// The panel takes no association at all now — the URI is typed into its own UI — so
// a spec with a MongoDB in it must NOT quietly draw one, and the panel must still
// compose on its own.
func TestComposeMClusterAdminDrawsNoLine(t *testing.T) {
	composeCatalogFixture(t)

	doc, _, _, _, err := buildCompose(composeSpec{Name: "panel", Nodes: []composeNodeSpec{
		{Kind: "mclusteradmin"},
	}})
	if err != nil {
		t.Fatalf("a panel on its own refused to compose: %v", err)
	}
	if len(doc.Edges) != 0 {
		t.Errorf("drew %d edges with nothing to link to", len(doc.Edges))
	}

	doc, _, _, _, err = buildCompose(composeSpec{Name: "panel", Nodes: []composeNodeSpec{
		{Kind: "psm", OS: "el9", MCA: true},
		{Kind: "mclusteradmin", ViewOnly: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Edges) != 0 {
		t.Errorf("drew %d association lines; this kind has no ports for one", len(doc.Edges))
	}
	var panel, mongo designNode
	for _, n := range doc.Nodes {
		switch n.Type {
		case "mclusteradmin":
			panel = n
		case "psm":
			mongo = n
		}
	}
	if !panel.ViewOnly {
		t.Error("viewOnly=true did not reach the panel node")
	}
	if !mongo.MCACredentials {
		t.Error("mca=true did not reach the MongoDB node")
	}
	// And `to=` is refused rather than silently ignored, the way it is for every
	// other kind with nothing to associate with.
	if _, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "psm", OS: "el9"},
		{Kind: "mclusteradmin", To: "psm-01"},
	}}); err == nil {
		t.Error("to= was accepted on a kind that associates with nothing")
	}
}

// mca on a kind that has no such option is refused rather than ignored.
func TestComposeRefusesMCAOnANonMongoKind(t *testing.T) {
	composeCatalogFixture(t)
	if _, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pg", OS: "el9", MCA: true},
	}}); err == nil {
		t.Fatal("mca on a PostgreSQL node was accepted")
	}
}

// The half of this feature that actually lets the panel log in. The accounts are
// created with SCRAM-SHA-256 credentials, and a mongod whose authenticationMechanisms
// do not include SCRAM-SHA-256 refuses them with an error that names the mechanism
// and reads like a wrong password.
func TestMCAWritesTheSCRAMMechanismIntoTheConfig(t *testing.T) {
	block := mongoMCASetParams(true, false, false)
	if !strings.Contains(block, "authenticationMechanisms") || !strings.Contains(block, "SCRAM-SHA-256") {
		t.Fatalf("the accounts are enabled but the mechanism is not configured: %q", block)
	}
	// SCRAM-SHA-1 stays listed: admin, PMM and PBM are created without a mechanisms
	// list, so they hold credentials for both, and dropping it would lock them out.
	if !strings.Contains(block, "SCRAM-SHA-1") {
		t.Errorf("the block drops SCRAM-SHA-1, which the other accounts still use: %q", block)
	}
	if got := mongoMCASetParams(false, false, false); got != "" {
		t.Errorf("no accounts asked for, but a block was written: %q", got)
	}

	// mongod.conf may carry only ONE setParameter block. OIDC renders its own and
	// directory auth appends its own at deploy — both already list SCRAM-SHA-256 —
	// so this one stands down rather than producing a duplicate YAML key that stops
	// mongod from starting at all.
	if got := mongoMCASetParams(true, true, false); got != "" {
		t.Errorf("wrote a second setParameter block alongside OIDC's: %q", got)
	}
	if got := mongoMCASetParams(true, false, true); got != "" {
		t.Errorf("wrote a second setParameter block alongside directory auth's: %q", got)
	}
	if !strings.Contains(mongoOIDCSetParameter("https://kc:8443/realms/mongodb", "cid", "", false), "SCRAM-SHA-256") {
		t.Error("OIDC's own block no longer lists SCRAM-SHA-256 — the stand-down above is now a hole")
	}

	// And it reaches both files a panel can be pointed at: a replica-set member, and
	// the mongos a sharded cluster is administered through.
	conf := mongodConfYAML("rs0", "", true, mongoMCASetParams(true, false, false), "")
	if strings.Count(conf, "setParameter:") != 1 || !strings.Contains(conf, "SCRAM-SHA-256") {
		t.Errorf("mongod.conf is wrong:\n%s", conf)
	}
	router := mongosConfYAML("cfg/h1:27017", mongoMCASetParams(true, false, false))
	if strings.Count(router, "setParameter:") != 1 || !strings.Contains(router, "SCRAM-SHA-256") {
		t.Errorf("mongos.conf is wrong:\n%s", router)
	}
	if strings.Contains(mongosConfYAML("cfg/h1:27017", ""), "setParameter") {
		t.Error("a mongos with no panel accounts should carry no setParameter block")
	}
}

// The passwords are the panel's, so both ends have to resolve the same two without
// either being told by the other.
func TestMCAPasswordsComeFromThePanelNode(t *testing.T) {
	defAdmin, defRO := mcaDefaultPasswords()
	if defAdmin != "madmin_password" || defRO != "madmin_ro_password" {
		t.Fatalf("defaults are %q/%q, want madmin_password/madmin_ro_password", defAdmin, defRO)
	}

	// No panel in the stack: the database still creates the accounts, at the
	// documented defaults, so adding the panel later needs no redeploy of it.
	if a, r := mcaPasswordsFor(designDoc{Nodes: []designNode{{ID: "m1", Type: "psm"}}}, "m1"); a != defAdmin || r != defRO {
		t.Errorf("with no panel: %q/%q, want the defaults", a, r)
	}

	panel := designNode{ID: "p1", Type: "mclusteradmin", MCAAdminPassword: "set-by-hand", MCAReadOnlyPassword: "ro-by-hand"}
	one := designDoc{Nodes: []designNode{{ID: "m1", Type: "psm"}, panel}}
	if a, r := mcaPasswordsFor(one, "m1"); a != "set-by-hand" || r != "ro-by-hand" {
		t.Errorf("the only panel's passwords were not used: %q/%q", a, r)
	}
	// And the panel resolves the same pair for itself, or the connection string it
	// hands over would not match the account the database created.
	if a, r := mcaPasswords(panel); a != "set-by-hand" || r != "ro-by-hand" {
		t.Errorf("the panel resolves %q/%q for itself", a, r)
	}
	// A half-filled panel takes the default for the field left empty.
	if a, r := mcaPasswords(designNode{MCAAdminPassword: "only-admin"}); a != "only-admin" || r != defRO {
		t.Errorf("half-filled panel gave %q/%q", a, r)
	}

	// Two panels is ambiguous, and there is no line to break the tie any more: the
	// documented defaults are used rather than one of the two at random, so the
	// mismatch a user then sees is against a password that is written down.
	two := designDoc{Nodes: []designNode{
		{ID: "m1", Type: "psm"}, panel,
		{ID: "p2", Type: "mclusteradmin", MCAAdminPassword: "other"},
	}}
	if a, _ := mcaPasswordsFor(two, "m1"); a != defAdmin {
		t.Errorf("two panels picked %q instead of the default", a)
	}
}

// A sharded cluster refused the panel where a replica set accepted it, because the
// accounts went on the config replica set only — and the panel connects straight to
// each shard for its per-shard views, where a shard's own user database has no such
// user. "Could not log in to rs0" is what that looks like.
func TestMCAAccountsReachEveryShard(t *testing.T) {
	config := []designNode{{ID: "c1", Label: "cfg-1"}, {ID: "c2", Label: "cfg-2"}}
	shards := map[int][]designNode{
		0: {{ID: "s0a", Label: "rs0-1"}, {ID: "s0b", Label: "rs0-2"}},
		1: {{ID: "s1a", Label: "rs1-1"}},
		2: {{ID: "s2a", Label: "rs2-1"}},
	}
	hosts := mcaShardedAccountHosts(config, shards, []int{0, 1, 2})

	var got []string
	for _, h := range hosts {
		got = append(got, h.ID)
	}
	want := []string{"c1", "s0a", "s1a", "s2a"}
	if len(got) != len(want) {
		t.Fatalf("accounts land on %v, want one member per replica set: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("host %d is %q, want %q", i, got[i], want[i])
		}
	}
	// One member per replica set, not every member: the account replicates within
	// its own set, so writing it three times to one shard would be three times the
	// work for the same result.
	for _, h := range hosts {
		if h.ID == "s0b" || h.ID == "c2" {
			t.Errorf("%s is a secondary in a set already covered", h.ID)
		}
	}
	// A topology with no config servers is not a sharded cluster; nothing to do
	// rather than a panic on config[0].
	if hs := mcaShardedAccountHosts(nil, shards, []int{0}); hs != nil {
		t.Errorf("an incomplete topology returned %v", hs)
	}
	// A shard index with no members (a design mid-edit) is skipped, not indexed into.
	if hs := mcaShardedAccountHosts(config, map[int][]designNode{0: nil}, []int{0}); len(hs) != 1 {
		t.Errorf("an empty shard was not skipped: %v", hs)
	}
}
