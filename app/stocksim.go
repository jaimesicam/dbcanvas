package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stock Market Sim (Type=="stocksim"): a small stock-exchange application with
// full browser CRUD, a live dashboard and a printable report, running against
// whichever database it was pointed at. Runs dbcanvas's own first-party
// dbcanvas-stocksim:latest image (built by `make stocksim-image`), not a
// systemd OS image — no product installed on top, no PMM monitoring. Its
// dashboard port is published to the host on a fixed, auto-assigned port that's
// reused across a redeploy, so it's reachable directly from the host browser
// with no VNC desktop needed.
//
// Two things make it unlike its five sibling simulators.
//
// First, it is not tied to one database engine. The same image speaks MySQL,
// PostgreSQL, MongoDB and Valkey; which one is decided here, at deploy, and
// passed in as DB_ENGINE — either from the linked node's own type, or from the
// engine chosen on the node's form.
//
// Second, it does not have to be linked to anything on the canvas. In "linked"
// mode it resolves a drawn association line exactly as carsim/airlinesim do. In
// "manual" mode it takes connection options straight from the node's own form
// and waits for nothing, so it can drive a database that is not part of the
// stack — on the Docker host, on the LAN, or a managed cloud instance. That is
// why this is the only provisioner that sets ExtraHosts, and the only node type
// with a pre-deploy connection test (see handleStockSimTest).
const (
	stockSimImage = "dbcanvas-stocksim:latest"
	stockSimPort  = 8093
)

// stockSimImplementedEngines mirrors store.Implemented() inside the sim image.
// The node form offers exactly this set, so a user can never configure a target
// the binary would then refuse at startup. Adding an engine means changing both.
var stockSimImplementedEngines = []string{"mysql", "postgres", "mongodb", "valkey"}

func stockSimEngineImplemented(engine string) bool {
	for _, e := range stockSimImplementedEngines {
		if e == engine {
			return true
		}
	}
	return false
}

// stockSimDefaultPort mirrors store.DefaultPort in the sim image.
func stockSimDefaultPort(engine string) int {
	switch engine {
	case "mysql":
		return pxcMySQLPort
	case "postgres":
		return patroniPGPort
	case "mongodb":
		return mongoPort
	case "valkey":
		return valkeyPort
	}
	return 0
}

// stockSimReservedDatabases mirrors the sim image's own refusal list. Checking
// here too means the user is told at validation time rather than watching a
// container start and immediately fail.
var stockSimReservedDatabases = map[string]bool{
	"mysql": true, "sys": true, "information_schema": true, "performance_schema": true,
	"postgres": true, "template0": true, "template1": true,
	"admin": true, "local": true, "config": true,
}

// stockSimGrowable mirrors store.CanGrowToSize in the sim image: whether an
// engine's dataset can be driven to a chosen size by writing to it. Only Valkey
// cannot — its tick history is a length-capped stream, so writing harder rolls
// old entries off rather than growing, and uncapping it would fill memory
// rather than disk. The node form hides the size field for that engine.
func stockSimGrowable(engine string) bool { return engine != "valkey" }

// stockSimDefaultTargetBytes mirrors sim.DefaultTargetBytes.
const stockSimDefaultTargetBytes int64 = 5 << 30

// Thread-count bounds, mirroring store.DefaultThreads/MaxThreads in the image.
const (
	stockSimDefaultThreads = 4
	stockSimMaxThreads     = 64
)

// stockSimDefaultWorkingSetPct mirrors sim.DefaultWorkingSetPct: half the
// dataset kept hot when the node says nothing.
const stockSimDefaultWorkingSetPct = 0.5

// stockSimConfig is the non-secret profile shown for a deployed Stock Market
// Sim node. Credentials never appear here — see stockSimSecrets.
type stockSimConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	Mode       string `json:"mode"`       // "linked" | "manual"
	Engine     string `json:"engine"`     // mysql | postgres | mongodb | valkey
	Database   string `json:"database"`   // schema / database / key prefix it owns
	TargetKind string `json:"targetKind"` // "ps" (linked) | "external-<engine>" (manual)
	TargetName string `json:"targetName"` // linked node label, or host:port
	HTTPPort   int    `json:"httpPort"`   // host port mapped to the container's dashboard port
	// TargetBytes is the dataset size the sim grows to at the High load level,
	// 0 when it will not grow at all. Resolved here rather than left as the
	// user's string so the node panel shows the size that is actually in force.
	TargetBytes int64 `json:"targetBytes"`
	// WorkingSet is how much of that dataset is kept under continuous random
	// read, as the sim was told it ("50%", "2.5 GiB", "off"), and Threads how
	// many concurrent database workers it runs. Both resolved here for the same
	// reason TargetBytes is.
	WorkingSet string `json:"workingSet"`
	Threads    int    `json:"threads"`
}

// stockSimSecrets holds the credentials a manual-mode node was configured with,
// kept out of stockSimConfig so the node properties panel never renders them.
type stockSimSecrets struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	DSN      string `json:"dsn,omitempty"`
}

// stockSimMode normalises the node's connection mode; an unset mode means
// linked, which is what every stack designed before manual mode existed gets.
func stockSimMode(n designNode) string {
	switch n.SSMode {
	case "manual", "aio":
		return n.SSMode
	}
	return "linked"
}

func stockSimEngine(n designNode) string {
	if e := strings.ToLower(strings.TrimSpace(n.SSEngine)); e != "" {
		return e
	}
	return "mysql"
}

func stockSimDatabase(n designNode) string {
	if d := strings.TrimSpace(n.SSDatabase); d != "" {
		return d
	}
	return "stocksim"
}

// stockSimParseSize mirrors sim.ParseSize in the image, so a mistyped size is
// caught on the canvas rather than in a container log nobody is watching. Both
// the decimal and binary suffixes mean the binary quantity, and an empty string
// means "not set" rather than zero — see the original for why.
func stockSimParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	up := strings.TrimSuffix(strings.ToUpper(s), "B")
	unit := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{
		{"KI", 1 << 10}, {"MI", 1 << 20}, {"GI", 1 << 30}, {"TI", 1 << 40},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	} {
		if rest, ok := strings.CutSuffix(up, u.suffix); ok {
			unit, up = u.mult, rest
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(up), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size — write it like 5G, 512Mi or a plain byte count", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return int64(n * float64(unit)), nil
}

// stockSimEngineForKind maps a linked canvas target's type to the engine the
// sim will actually speak to it. In linked mode this — not the node's own
// SSEngine field, which only manual mode fills in — is what the sim ends up
// running, so it decides both the driver and whether a size target is possible.
//
// The two routers are absent on purpose: HAProxy fronts MySQL-family *or*
// PostgreSQL clusters and ProxySQL only MySQL-family ones, so their engine is a
// property of the backend behind them. stockSimEngineForTarget resolves those.
func stockSimEngineForKind(kind string) string {
	switch kind {
	case "ps", "mariadb", "mysqlce",
		"pxc", "mysql", "innodb", "mariadbrepl", "mariadbgalera", "mysqlcerepl", "mysqlceinnodb":
		return "mysql"
	case "pg", "patroni", "repmgr", "spock", "k3d":
		return "postgres"
	case "psm", "psmrs", "psmdb":
		return "mongodb"
	case "valkey", "valkeycluster":
		return "valkey"
	}
	return ""
}

// stockSimEngineForTarget is stockSimEngineForKind with the router cases
// resolved, which needs the design to see what the router fronts. Returns "" if
// the target is a router with no single identifiable backend — a design error
// the validator reports in its own words rather than guessing an engine for.
func stockSimEngineForTarget(doc designDoc, kind, targetID string) string {
	switch kind {
	case "haproxy":
		if _, backKind, ok := haproxyBackend(doc, targetID); ok {
			return stockSimEngineForKind(backKind)
		}
		return ""
	case "proxysql":
		// ProxySQL is a MySQL-protocol proxy and fronts nothing else.
		return "mysql"
	}
	return stockSimEngineForKind(kind)
}

// stockSimTargetBytes resolves the size this node's dataset should grow to at
// the High load level, for an already-resolved engine. Zero means it never
// grows: either the engine cannot be grown, or the user asked for that
// explicitly. A value that does not parse falls back to the default here and
// is reported by stockSimSizeIssues, which blocks the deploy — so a typo never
// silently changes what gets deployed.
func stockSimTargetBytes(n designNode, engine string) int64 {
	if !stockSimGrowable(engine) {
		return 0
	}
	raw := strings.TrimSpace(n.SSTargetSize)
	if raw == "" {
		return stockSimDefaultTargetBytes
	}
	if strings.EqualFold(raw, "off") || strings.EqualFold(raw, "none") {
		return 0
	}
	size, err := stockSimParseSize(raw)
	if err != nil {
		return stockSimDefaultTargetBytes
	}
	return size
}

// stockSimWorkingSet is the resolved WORKING_SET value for a node, on an
// already-resolved engine. It mirrors sim.ParseWorkingSet closely enough to
// reject what that would reject, but returns the string to pass through rather
// than a parsed shape — the sim is the one that has to act on it, and a share
// of a dataset whose size is not known until deploy cannot be resolved here.
//
// "off" on an engine that cannot grow, because reading a capped stream in a
// loop is not a working set; see stockSimGrowable.
func stockSimWorkingSet(n designNode, engine string) string {
	if !stockSimGrowable(engine) {
		return "off"
	}
	raw := strings.TrimSpace(n.SSWorkingSet)
	if raw == "" {
		return stockSimFormatWorkingSet(stockSimDefaultWorkingSetPct)
	}
	if _, err := stockSimParseWorkingSet(raw); err != nil {
		// A typo is blocked by stockSimWorkingSetIssues; until it is, the
		// default stands rather than something the user did not ask for.
		return stockSimFormatWorkingSet(stockSimDefaultWorkingSetPct)
	}
	return raw
}

// stockSimParseWorkingSet mirrors sim.ParseWorkingSet: "50%", a bare share of
// one or less, a size like "2G", or "off". It returns the share when one was
// given and 0 for an absolute size — the caller only needs to know whether the
// value is usable, not what it resolves to.
func stockSimParseWorkingSet(s string) (float64, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return stockSimDefaultWorkingSetPct, nil
	case strings.EqualFold(s, "off"), strings.EqualFold(s, "none"):
		return 0, nil
	}
	invalid := fmt.Errorf("%q is not a working set — write it like 50%%, 0.5 or 2G", s)
	if pct, ok := strings.CutSuffix(s, "%"); ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
		if err != nil || n < 0 || n > 100 {
			return 0, invalid
		}
		return n / 100, nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		switch {
		case n < 0:
			return 0, fmt.Errorf("%q is negative", s)
		case n <= 1:
			return n, nil
		}
	}
	if _, err := stockSimParseSize(s); err != nil {
		return 0, invalid
	}
	return 0, nil
}

// stockSimFormatWorkingSet renders a share the way the node panel and the sim's
// own environment both want it.
func stockSimFormatWorkingSet(share float64) string {
	return fmt.Sprintf("%.0f%%", share*100)
}

// stockSimThreads resolves how many concurrent database workers the sim runs,
// mirroring store.ClampThreads.
func stockSimThreads(n designNode) int {
	switch {
	case n.SSThreads <= 0:
		return stockSimDefaultThreads
	case n.SSThreads > stockSimMaxThreads:
		return stockSimMaxThreads
	}
	return n.SSThreads
}

// stockSimEngineAndIssues resolves which engine a Stock Market Sim node will
// actually run against, and reports every reason it might not resolve at all.
// One function for all three connection modes because the caller needs both
// halves and they are decided together: the engine is a consequence of the
// target, so a target that cannot be identified has no engine either, and an
// empty engine is exactly the case where an issue has been raised.
func stockSimEngineAndIssues(doc designDoc, n designNode) (string, []issue) {
	switch stockSimMode(n) {
	case "manual":
		return stockSimEngine(n), stockSimIssues(n)

	case "aio":
		in, ok := stockSimAIODeclared(doc, n)
		if !ok {
			return "", []issue{{"error", "Stock Market Sim node " + n.Label +
				" is set to use an All in One instance but none is selected — choose the node and the instance, or switch it to another connection mode"}}
		}
		engine := aioEngineForKind(in.Kind)
		if engine == "" {
			return "", []issue{{"error", fmt.Sprintf(
				"Stock Market Sim node %s is pointed at All in One instance %q, which is a %s instance — this application can drive an All in One node's MySQL, PostgreSQL and MongoDB instances, but not its proxies, Orchestrator or Valkey",
				n.Label, in.Name, in.Kind)}}
		}
		return engine, nil

	default:
		kind, targetID, ok := stockSimTarget(doc, n.ID)
		if !ok {
			return "", []issue{{"error", "Stock Market Sim node " + n.Label +
				" must be linked to a database — a standalone Percona Server, MariaDB, MySQL, PostgreSQL, PS MongoDB or Valkey node, any MySQL, PostgreSQL, MongoDB or Valkey cluster frame, a CloudNativePG Kubernetes frame, or a ProxySQL/HAProxy node fronting one. Draw an association line from one to it, or switch the node to an All in One instance or a manual connection"}}
		}
		engine := stockSimEngineForTarget(doc, kind, targetID)
		if engine == "" {
			// Only reachable for a router: every other kind maps to an engine
			// unconditionally.
			return "", []issue{{"error", "Stock Market Sim node " + n.Label +
				" is linked to an HAProxy node that does not front exactly one database cluster, so there is no database to connect to — link the cluster frame directly instead"}}
		}
		return engine, nil
	}
}

// stockSimSizeIssues validates the growth target. Called for both connection
// modes — the size applies either way — with the engine already resolved,
// which for a linked node means the type of the node it is linked to.
func stockSimSizeIssues(n designNode, engine string) []issue {
	raw := strings.TrimSpace(n.SSTargetSize)
	if raw == "" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "none") {
		return nil
	}
	if !stockSimGrowable(engine) {
		return []issue{{"warning", fmt.Sprintf(
			"Stock Market Sim node %s has a dataset size of %s, which will be ignored: %s keeps its tick history in a length-capped stream, so writing to it does not make it bigger. Point the node at MySQL, PostgreSQL or MongoDB to test storage under load.",
			n.Label, raw, engineDisplayLabel(engine))}}
	}
	size, err := stockSimParseSize(raw)
	if err != nil {
		return []issue{{"error", fmt.Sprintf(
			"Stock Market Sim node %s has an invalid dataset size: %v", n.Label, err)}}
	}
	// Below a gibibyte the data fits in a normal instance's buffer pool and the
	// disk is never really asked anything, which is the opposite of the point.
	if size > 0 && size < 1<<30 {
		return []issue{{"info", fmt.Sprintf(
			"Stock Market Sim node %s will grow its dataset to %s, which most servers will hold entirely in memory — raise it past a few GiB if you are measuring storage rather than throughput",
			n.Label, stockSimFormatSize(size))}}
	}
	return nil
}

// stockSimLoadIssues validates the two knobs that decide how hard the sim works
// its target: how much of the dataset it keeps hot, and how many workers it
// uses to do it. Both are checked against the resolved engine, because on an
// engine that cannot grow a dataset neither one does anything.
func stockSimLoadIssues(n designNode, engine string) []issue {
	var out []issue
	raw := strings.TrimSpace(n.SSWorkingSet)
	growable := stockSimGrowable(engine)

	if raw != "" && growable {
		share, err := stockSimParseWorkingSet(raw)
		switch {
		case err != nil:
			out = append(out, issue{"error", fmt.Sprintf(
				"Stock Market Sim node %s has an invalid working set: %v", n.Label, err)})
		case share == 0 && (strings.EqualFold(raw, "off") || strings.EqualFold(raw, "none") || raw == "0"):
			// Worth saying plainly, because a dataset nothing reads back is the
			// exact configuration in which buffer pool size appears not to
			// matter — the question this option exists to answer.
			out = append(out, issue{"info", fmt.Sprintf(
				"Stock Market Sim node %s will write its dataset but never read it back, so the target's cache will show a near-perfect hit rate whatever it is sized at — set a working set to make cache size measurable",
				n.Label)})
		}
	}
	if raw != "" && !growable {
		out = append(out, issue{"warning", fmt.Sprintf(
			"Stock Market Sim node %s has a working set of %s, which will be ignored: %s keeps its tick history in a length-capped stream, so there is no cold data to read back.",
			n.Label, raw, engineDisplayLabel(engine))})
	}

	switch {
	case n.SSThreads < 0:
		out = append(out, issue{"error", fmt.Sprintf(
			"Stock Market Sim node %s has a negative thread count — leave it blank for the default of %d",
			n.Label, stockSimDefaultThreads)})
	case n.SSThreads > stockSimMaxThreads:
		out = append(out, issue{"warning", fmt.Sprintf(
			"Stock Market Sim node %s asks for %d database threads; it will be capped at %d",
			n.Label, n.SSThreads, stockSimMaxThreads)})
	}
	return out
}

// engineDisplayLabel mirrors engineDisplayName in the sim image.
func engineDisplayLabel(engine string) string {
	switch engine {
	case "mysql":
		return "MySQL"
	case "postgres":
		return "PostgreSQL"
	case "mongodb":
		return "MongoDB"
	case "valkey":
		return "Valkey"
	}
	return engine
}

// stockSimFormatSize mirrors sim.FormatBytes, for the node panel's benefit.
func stockSimFormatSize(n int64) string {
	switch {
	case n <= 0:
		return "no growth"
	case n >= 1<<40:
		return fmt.Sprintf("%.2f TiB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// stockSimStandaloneTargets are the standalone database node types a Stock
// Market Sim node can be linked to. The key is the node type, which is also the
// coarse kind, because for a standalone node the two coincide.
var stockSimStandaloneTargets = map[string]bool{
	"ps": true, "mariadb": true, "mysqlce": true, // MySQL family
	"pg":     true, // PostgreSQL
	"psm":    true, // MongoDB
	"valkey": true, // Valkey
}

// stockSimFrameTargets are the cluster frame types it can be linked to. Every
// one resolves to a single write endpoint in waitStockSimTarget — the primary,
// the leader, the router or the mongos, depending on what the cluster is.
var stockSimFrameTargets = map[string]bool{
	// MySQL family.
	"pxc": true, "mysql": true, "innodb": true,
	"mariadbrepl": true, "mariadbgalera": true,
	"mysqlcerepl": true, "mysqlceinnodb": true,
	// PostgreSQL.
	"patroni": true, "repmgr": true, "spock": true, "k3d": true,
	// MongoDB.
	"psmrs": true, "psmdb": true,
	// Valkey.
	"valkeycluster": true,
}

// stockSimTarget resolves the coarse kind and target id a stocksim node is
// linked to via a drawn edge. Mirrors carSimTarget's undirected-edge walk, but
// over a much wider set: this is the only sim that is not tied to one engine,
// so it accepts every database node, cluster frame and router dbcanvas can
// deploy. The kind returned is the node or frame type itself, except for the
// two routers, which are named for what they are rather than what they front —
// waitStockSimTarget works that out from the backend they are attached to.
//
// A Stock Market Sim node still drives exactly one database; the singleOutgoing
// rule on the canvas is what enforces that.
func stockSimTarget(doc designDoc, startID string) (kind, targetID string, ok bool) {
	for _, e := range doc.Edges {
		var other string
		switch startID {
		case e.From.Node:
			other = e.To.Node
		case e.To.Node:
			other = e.From.Node
		default:
			continue
		}
		for _, n := range doc.Nodes {
			if n.ID != other || n.FrameID != "" {
				continue
			}
			switch {
			case stockSimStandaloneTargets[n.Type]:
				return n.Type, n.ID, true
			case n.Type == "haproxy", n.Type == "proxysql":
				return n.Type, n.ID, true
			}
		}
		for _, f := range doc.Frames {
			if f.ID == other && stockSimFrameTargets[f.Type] {
				return f.Type, f.ID, true
			}
		}
	}
	return "", "", false
}

// stockSimConfigError reports the first setting that the sim binary itself
// would refuse at startup, or "" if the configuration is at least startable.
// Mirrors store.Config.Validate inside the image — both this and
// stockSimIssues below are built on it so a node's validation, its
// Test-connection button and the container all agree.
func stockSimConfigError(n designNode) string {
	engine := stockSimEngine(n)
	if !stockSimEngineImplemented(engine) {
		return fmt.Sprintf("database engine %q is not implemented in this build yet — only %s available",
			engine, strings.Join(stockSimImplementedEngines, " or ")+" is")
	}
	if stockSimReservedDatabases[strings.ToLower(stockSimDatabase(n))] {
		return fmt.Sprintf(
			"refusing to use %q as this application's namespace: it is a reserved system database. Choose another name (the default is \"stocksim\")",
			stockSimDatabase(n))
	}
	if strings.TrimSpace(n.SSHost) == "" && strings.TrimSpace(n.SSDSN) == "" {
		return "no target: set a host to connect to, or a full connection string"
	}
	return ""
}

// stockSimIssues returns the validation problems for one stocksim node, in the
// user's own vocabulary. Split out of validateStack because manual mode has
// several distinct ways to be incomplete and inlining them all would bury the
// switch it lives in.
func stockSimIssues(n designNode) []issue {
	var out []issue
	engine := stockSimEngine(n)
	if !stockSimEngineImplemented(engine) {
		out = append(out, issue{"error", fmt.Sprintf(
			"Stock Market Sim node %s is set to %s, which this build does not implement yet — choose %s",
			n.Label, engine, strings.Join(stockSimImplementedEngines, " or "))})
	}
	if stockSimReservedDatabases[strings.ToLower(stockSimDatabase(n))] {
		out = append(out, issue{"error", fmt.Sprintf(
			"Stock Market Sim node %s cannot use %q as its database — that is a reserved system database. Choose another name (the default is \"stocksim\")",
			n.Label, stockSimDatabase(n))})
	}
	if strings.TrimSpace(n.SSHost) == "" && strings.TrimSpace(n.SSDSN) == "" {
		out = append(out, issue{"error",
			"Stock Market Sim node " + n.Label + " is set to a manual connection but has no host — enter one, or paste a full connection string under Advanced"})
	}
	if n.SSPort < 0 || n.SSPort > 65535 {
		out = append(out, issue{"error",
			"Stock Market Sim node " + n.Label + " has an invalid port — leave it blank for the engine default"})
	}
	// Not an error: dbcanvas cannot reach out and check an endpoint it does not
	// manage, so say so plainly rather than pretend the design is verified.
	out = append(out, issue{"info",
		"Stock Market Sim node " + n.Label + " connects to a database outside this stack — dbcanvas cannot verify it before deploying, so use Test connection on the node to check it now"})
	return out
}

// provisionStockSim records the deployment then brings up the sim container.
func (a *App) provisionStockSim(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the previously published host port across a redeploy so the
	// dashboard URL shown in node properties stays stable — mirrors
	// provisionPMM's own reused-host-port pattern.
	httpPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old stockSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	mode := stockSimMode(n)
	cfg := stockSimConfig{
		Image: stockSimImage, Hostname: host, FQDN: fqdn, HTTPPort: httpPort,
		Mode: mode, Engine: stockSimEngine(n), Database: stockSimDatabase(n),
	}

	coarseKind, targetID := "", ""
	if mode == "linked" {
		var ok bool
		coarseKind, targetID, ok = stockSimTarget(doc, n.ID)
		if !ok {
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError, Config: mustJSON(cfg)})
			return
		}
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg)})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, stockSimImage); !ok {
			pr.fail("image %s not found — run `make stocksim-image` first", stockSimImage)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		var env []string
		var sec stockSimSecrets
		var extraHosts []string

		switch mode {
		case "linked":
			pr.phase("Waiting for linked database", 20)
			e, werr := a.waitStockSimTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, deployTimeout())
			if werr != nil {
				pr.fail("%v", werr)
				return
			}
			cfg.Engine, cfg.TargetKind, cfg.TargetName = e.engine, e.kind, e.displayName
			env, sec = e.env, e.secrets
			pr.logln(fmt.Sprintf("target: %s %s (%s:%d)", cfg.TargetKind, cfg.TargetName, e.host, e.port))

		case "aio":
			pr.phase("Waiting for the selected All in One instance", 20)
			e, werr := a.stockSimAIOTarget(ctx, st, doc, n, deployTimeout())
			if werr != nil {
				pr.fail("%v", werr)
				return
			}
			cfg.Engine, cfg.TargetKind, cfg.TargetName = e.engine, e.kind, e.displayName
			env, sec = e.env, e.secrets
			pr.logln(fmt.Sprintf("target: %s %s (%s:%d)", cfg.TargetKind, cfg.TargetName, e.host, e.port))

		default:
			pr.phase("Using the connection configured on this node", 20)
			env, sec, cfg.TargetName = stockSimManualEnv(n)
			cfg.TargetKind = "external-" + cfg.Engine
			// Only manual mode gets host.docker.internal: it is the one case
			// where the database may be running on the Docker host rather than
			// inside the stack's own network, and without this the name does
			// not resolve at all.
			extraHosts = []string{"host.docker.internal:host-gateway"}
			pr.logln("target: " + cfg.TargetKind + " " + cfg.TargetName + " (not verified by dbcanvas)")
		}

		// Resolved only now, because in linked mode the engine is the linked
		// node's type rather than anything set on this node, and whether a
		// dataset can be grown at all depends on which engine it turned out to be.
		cfg.TargetBytes = stockSimTargetBytes(n, cfg.Engine)
		cfg.WorkingSet = stockSimWorkingSet(n, cfg.Engine)
		cfg.Threads = stockSimThreads(n)
		env = append(env,
			"DB_NAME="+cfg.Database,
			"TARGET_KIND="+cfg.TargetKind,
			"TARGET_LABEL="+cfg.TargetName,
			fmt.Sprintf("TARGET_BYTES=%d", cfg.TargetBytes),
			"WORKING_SET="+cfg.WorkingSet,
			fmt.Sprintf("DB_THREADS=%d", cfg.Threads),
			fmt.Sprintf("PORT=%d", stockSimPort),
		)
		if cfg.TargetBytes > 0 {
			pr.logln(fmt.Sprintf("dataset grows to %s at the High load level, "+
				"working set %s, %d database threads",
				stockSimFormatSize(cfg.TargetBytes), cfg.WorkingSet, cfg.Threads))
		}

		pr.phase("Creating container", 45)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: stockSimImage, Hostname: host, Env: env,
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: stockSimPort, HostPort: httpPort}},
			DNS:        []string{intranetIP}, DNSSearch: []string{domain},
			ExtraHosts: extraHosts,
		})
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", stockSimPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{
			StackID: st.ID, NodeID: n.ID, ContainerID: id,
			State: DeployProvisioning, Config: mustJSON(cfg), Secrets: mustJSON(sec),
		})

		pr.phase("Waiting for the application", 80)
		if err := a.waitStockSimHealthy(ctx, id, 60*time.Second); err != nil {
			pr.fail("stocksim did not become ready: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{
			StackID: st.ID, NodeID: n.ID, ContainerID: id,
			State: DeployRunning, Config: mustJSON(cfg), Secrets: mustJSON(sec),
		})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// stockSimResolved is what a linked target resolves to.
type stockSimResolved struct {
	env         []string
	secrets     stockSimSecrets
	engine      string
	kind        string
	displayName string
	host        string
	port        int
}

// waitStockSimTarget resolves a linked canvas target down to a connectable
// endpoint and credentials, blocking until it is actually running.
//
// It dispatches on the *engine* rather than on the kind, because everything
// after the endpoint has been found — the DSN dialect, the driver, the
// credentials that exist — is a property of the engine, and everything before
// it is a property of the kind. The four family resolvers below each do the
// second half for one engine.
func (a *App) waitStockSimTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, coarseKind, targetID string, timeout time.Duration) (stockSimResolved, error) {
	switch stockSimEngineForTarget(doc, coarseKind, targetID) {
	case "mysql":
		return a.stockSimMySQLTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, timeout)
	case "postgres":
		return a.stockSimPostgresTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, timeout)
	case "mongodb":
		return a.stockSimMongoTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, timeout)
	case "valkey":
		return a.stockSimValkeyTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, timeout)
	}
	if coarseKind == "haproxy" {
		return stockSimResolved{}, fmt.Errorf(
			"the linked HAProxy node does not front exactly one database cluster, so there is nothing to connect to — link the cluster frame directly instead")
	}
	return stockSimResolved{}, fmt.Errorf("unresolved Stock Market Sim target %q", coarseKind)
}

// stockSimMySQLTarget resolves every MySQL-family shape to one write endpoint.
func (a *App) stockSimMySQLTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, kind, targetID string, timeout time.Duration) (stockSimResolved, error) {
	var (
		h    string
		port = pxcMySQLPort
		s    pxcSecrets
		name = nodeLabel(doc, targetID)
		err  error
	)
	frame := frameByID(doc, targetID)
	if frame.ID != "" {
		name = frame.Label
	}

	switch kind {
	case "ps", "mariadb", "mysqlce":
		h, s, err = a.waitMySQLFamilyNode(ctx, st.ID, targetID, hosts, domain, kind, timeout)

	case "pxc":
		h, s, err = a.waitPXCRunning(ctx, st.ID, frame, doc, domain, timeout)

	case "mysql":
		h, _, s, err = a.waitMySQLRunning(ctx, st.ID, frame, doc, domain, timeout)

	case "mariadbrepl", "mysqlcerepl":
		// Asynchronous replication: only the primary takes writes.
		h, s, err = a.waitMySQLFamilyFrame(ctx, st.ID, frame, doc, domain, true, timeout)

	case "mariadbgalera":
		// Galera is multi-master — every member is a write endpoint.
		h, s, err = a.waitMySQLFamilyFrame(ctx, st.ID, frame, doc, domain, false, timeout)

	case "innodb", "mysqlceinnodb":
		// Group Replication installs MySQL Router on every member, and the
		// cluster exposes no endpoint of its own (see innodb.go). Connecting to
		// any member's read/write router port lands on the current primary
		// wherever it is, which is what an application wants and what makes a
		// failover invisible to the sim.
		h, s, err = a.waitMySQLFamilyFrame(ctx, st.ID, frame, doc, domain, false, timeout)
		port = routerRWPort

	case "haproxy":
		back, backKind, ok := haproxyBackend(doc, targetID)
		if !ok {
			return stockSimResolved{}, fmt.Errorf("the linked HAProxy node does not front exactly one cluster")
		}
		if !a.waitNodeRunning(st.ID, targetID, timeout) {
			return stockSimResolved{}, fmt.Errorf("linked HAProxy node did not become ready within %s", timeout)
		}
		switch backKind {
		case "pxc":
			_, s, err = a.waitPXCRunning(ctx, st.ID, back, doc, domain, timeout)
		case "mysql":
			_, _, s, err = a.waitMySQLRunning(ctx, st.ID, back, doc, domain, timeout)
		default:
			_, s, err = a.waitMySQLFamilyFrame(ctx, st.ID, back, doc, domain, false, timeout)
		}
		h, port, kind = fqdnOf(hosts[targetID], domain), haproxyWritePort, "haproxy-"+backKind

	case "proxysql":
		var backKind string
		h, backKind, s, name, err = a.waitProxySQLRunning(ctx, st.ID, doc, hosts, domain, targetID, timeout)
		port, kind = proxysqlMySQLPort, "proxysql-"+backKind

	default:
		return stockSimResolved{}, fmt.Errorf("unresolved MySQL-family target %q", kind)
	}
	if err != nil {
		return stockSimResolved{}, err
	}

	// Connect as the application user, never root: root@localhost cannot
	// connect over TCP, which is what every sibling sim learned the hard way.
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/?tls=false", s.AppUser, s.AppPassword, h, port)
	return stockSimResolved{
		env:     []string{"DB_ENGINE=mysql", "MYSQL_DSN=" + dsn},
		secrets: stockSimSecrets{User: s.AppUser, Password: s.AppPassword},
		engine:  "mysql", kind: kind, displayName: name,
		host: h, port: port,
	}, nil
}

// stockSimPostgresTarget resolves every PostgreSQL shape to one write endpoint.
func (a *App) stockSimPostgresTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, kind, targetID string, timeout time.Duration) (stockSimResolved, error) {
	var (
		h    string
		port = patroniPGPort
		s    pgSecrets
		name = nodeLabel(doc, targetID)
		err  error
	)
	frame := frameByID(doc, targetID)
	if frame.ID != "" {
		name = frame.Label
	}

	switch kind {
	case "pg":
		h, s, err = a.waitPgNodeRunning(ctx, st.ID, targetID, hosts, domain, timeout)

	case "patroni":
		var fqdns []string
		fqdns, s, err = a.waitPatroniRunning(ctx, st.ID, frame, doc, domain, timeout)
		if err == nil {
			h = a.leaderOrFirst(ctx, st, frame, doc, hosts, domain, fqdns, a.patroniLeaderContainer(ctx, st, frame, doc))
		}

	case "repmgr":
		var fqdns []string
		fqdns, s, err = a.waitRepmgrRunning(ctx, st.ID, frame, doc, domain, timeout)
		if err == nil {
			h = a.leaderOrFirst(ctx, st, frame, doc, hosts, domain, fqdns, a.repmgrPrimaryContainer(ctx, st, frame, doc))
		}

	case "spock":
		var fqdns []string
		fqdns, s, err = a.waitSpockRunning(ctx, st.ID, frame, doc, domain, timeout)
		if err == nil {
			if len(fqdns) == 0 {
				return stockSimResolved{}, fmt.Errorf("associated Spock cluster %s has no running member", frame.Label)
			}
			h = fqdns[0] // symmetric peers — any member is a valid write target
		}

	case "k3d":
		// CloudNativePG inside the k3s cluster. Unlike every other target this
		// one is not on the stack's Docker network by name: §227 recorded the
		// endpoint the operator's Service resolved to, and that recording is
		// the only way in from outside Kubernetes.
		return a.stockSimCNPGTarget(ctx, st, doc, targetID, timeout)

	case "haproxy":
		back, backKind, ok := haproxyBackend(doc, targetID)
		if !ok {
			return stockSimResolved{}, fmt.Errorf("the linked HAProxy node does not front exactly one cluster")
		}
		if !a.waitNodeRunning(st.ID, targetID, timeout) {
			return stockSimResolved{}, fmt.Errorf("linked HAProxy node did not become ready within %s", timeout)
		}
		switch backKind {
		case "patroni":
			_, s, err = a.waitPatroniRunning(ctx, st.ID, back, doc, domain, timeout)
		case "repmgr":
			_, s, err = a.waitRepmgrRunning(ctx, st.ID, back, doc, domain, timeout)
		default:
			_, s, err = a.waitSpockRunning(ctx, st.ID, back, doc, domain, timeout)
		}
		h, port, kind = fqdnOf(hosts[targetID], domain), haproxyWritePort, "haproxy-"+backKind

	default:
		return stockSimResolved{}, fmt.Errorf("unresolved PostgreSQL target %q", kind)
	}
	if err != nil {
		return stockSimResolved{}, err
	}

	// The superuser, because a stack node provisions no other PostgreSQL role
	// and the app needs to create a database or a schema of its own.
	dsn := (&url.URL{
		Scheme: "postgres", User: url.UserPassword(s.SuperUser, s.SuperPassword),
		Host: fmt.Sprintf("%s:%d", h, port), Path: "/postgres",
		RawQuery: "sslmode=prefer&connect_timeout=10",
	}).String()
	return stockSimResolved{
		env:     []string{"DB_ENGINE=postgres", "POSTGRES_DSN=" + dsn},
		secrets: stockSimSecrets{User: s.SuperUser, Password: s.SuperPassword},
		engine:  "postgres", kind: kind, displayName: name,
		host: h, port: port,
	}, nil
}

// stockSimMongoTarget resolves a standalone node, a replica set or a sharded
// cluster. The last two reuse hotelsim's resolver outright — it already returns
// exactly the URI this app needs, including waiting for a replica-set majority
// and picking a running mongos.
func (a *App) stockSimMongoTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, kind, targetID string, timeout time.Duration) (stockSimResolved, error) {
	if kind == "psm" {
		dep, werr := a.waitStockSimNode(ctx, st.ID, targetID, timeout, "MongoDB")
		if werr != nil {
			return stockSimResolved{}, werr
		}
		user, pass := hotelSimCreds(dep)
		h := fqdnOf(hosts[targetID], domain)
		uri := fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
			url.QueryEscape(user), url.QueryEscape(pass), h, mongoPort)
		return stockSimResolved{
			env:     []string{"DB_ENGINE=mongodb", "MONGO_URI=" + uri},
			secrets: stockSimSecrets{User: user, Password: pass},
			engine:  "mongodb", kind: "psm", displayName: nodeLabel(doc, targetID),
			host: h, port: mongoPort,
		}, nil
	}

	frame := frameByID(doc, targetID)
	// hotelsim addresses members by bare hostname; this app's container resolves
	// those through the same Intranet DNS, so the URI is usable as returned.
	uri, werr := a.waitHotelSimTarget(ctx, st.ID, hosts, "", frame, kind, doc, timeout)
	if werr != nil {
		return stockSimResolved{}, werr
	}
	user, pass := "", ""
	if u, uerr := url.Parse(uri); uerr == nil && u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return stockSimResolved{
		env:     []string{"DB_ENGINE=mongodb", "MONGO_URI=" + uri},
		secrets: stockSimSecrets{User: user, Password: pass},
		engine:  "mongodb", kind: kind, displayName: frame.Label,
		host: frame.Label, port: mongoPort,
	}, nil
}

// stockSimValkeyTarget resolves a standalone node or a cluster frame. The
// cluster case reuses trafficsim's resolver, and the sim's own store hands the
// address list to a UniversalClient, which picks cluster mode from the count —
// so nothing downstream needs to know which of the two this was.
func (a *App) stockSimValkeyTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, kind, targetID string, timeout time.Duration) (stockSimResolved, error) {
	if kind == "valkey" {
		dep, werr := a.waitStockSimNode(ctx, st.ID, targetID, timeout, "Valkey")
		if werr != nil {
			return stockSimResolved{}, werr
		}
		pw := valkeyPasswordFor(dep)
		h := fqdnOf(hosts[targetID], domain)
		return stockSimResolved{
			env: []string{
				"DB_ENGINE=valkey",
				fmt.Sprintf("VALKEY_ADDRS=%s:%d", h, valkeyPort),
				"VALKEY_PASSWORD=" + pw,
			},
			secrets: stockSimSecrets{Password: pw},
			engine:  "valkey", kind: "valkey", displayName: nodeLabel(doc, targetID),
			host: h, port: valkeyPort,
		}, nil
	}

	frame := frameByID(doc, targetID)
	addrs, pw, werr := a.waitTrafficSimTarget(ctx, st.ID, hosts, "", frame, true, timeout)
	if werr != nil {
		return stockSimResolved{}, werr
	}
	if len(addrs) == 0 {
		return stockSimResolved{}, fmt.Errorf("associated Valkey cluster %s has no running member", frame.Label)
	}
	return stockSimResolved{
		env: []string{
			"DB_ENGINE=valkey",
			"VALKEY_ADDRS=" + strings.Join(addrs, ","),
			"VALKEY_PASSWORD=" + pw,
		},
		secrets: stockSimSecrets{Password: pw},
		engine:  "valkey", kind: "valkeycluster", displayName: frame.Label,
		host: addrs[0], port: valkeyPort,
	}, nil
}

// stockSimCNPGTarget resolves a CloudNativePG cluster running inside a K3D
// frame. Three things make it unlike every other target:
//
//   - There is no canvas node to wait on. A K3D frame's members are k3s hosts,
//     and the database is a Kubernetes object inside them, so the endpoint has
//     to come from what §227 recorded on the server node's own k3dConfig.
//   - It is only reachable if the cluster was exposed as a LoadBalancer. The
//     ClusterIP fallback is a `.svc` name that means nothing outside
//     Kubernetes, and the k3d cluster is on the stack network precisely so a
//     MetalLB address *is* routable from a sibling container.
//   - CNPG generates the application role's password itself and keeps it only
//     in a Secret, so it is read out of the cluster at deploy time rather than
//     taken from a stored value.
//
// That role is not a superuser, so the sim will find it cannot create a
// database of its own and will claim a schema inside CNPG's application
// database instead — the fallback exists for exactly this shape of target.
func (a *App) stockSimCNPGTarget(ctx context.Context, st Stack, doc designDoc, frameID string, timeout time.Duration) (stockSimResolved, error) {
	frame := frameByID(doc, frameID)

	deadline := time.Now().Add(timeout)
	for {
		cfg, serverID, ok := a.k3dServerConfig(st.ID, doc, frameID)
		switch {
		case ok && cfg.Operator != "cnpg":
			return stockSimResolved{}, fmt.Errorf(
				"the linked Kubernetes cluster %s does not run CloudNativePG — a Stock Market Sim node can only use a CNPG frame", frame.Label)
		case ok && cfg.CNPGEndpoint != "" && cfg.CNPGEndpoint != "pending":
			if cfg.CNPGExpose != "LoadBalancer" {
				return stockSimResolved{}, fmt.Errorf(
					"CloudNativePG in %s is only exposed inside Kubernetes (%s) — set the cluster to expose a LoadBalancer so this application can reach it",
					frame.Label, cfg.CNPGEndpoint)
			}
			user := cfg.CNPGAppUser
			if user == "" {
				user = "app"
			}
			db := cfg.CNPGAppDB
			if db == "" {
				db = "app"
			}
			pass := a.cnpgSecretValue(ctx, serverID, cfg.Namespace, cfg.CNPGAppSecret, "password")
			if pass == "" {
				return stockSimResolved{}, fmt.Errorf(
					"could not read the CloudNativePG application password out of Secret %q in namespace %q",
					cfg.CNPGAppSecret, cfg.Namespace)
			}
			host, port := splitHostPortDefault(cfg.CNPGEndpoint, cnpgPostgresPort)
			dsn := (&url.URL{
				Scheme: "postgres", User: url.UserPassword(user, pass),
				Host: cfg.CNPGEndpoint, Path: "/" + db,
				RawQuery: "sslmode=prefer&connect_timeout=10",
			}).String()
			return stockSimResolved{
				env:     []string{"DB_ENGINE=postgres", "POSTGRES_DSN=" + dsn},
				secrets: stockSimSecrets{User: user, Password: pass},
				engine:  "postgres", kind: "k3d", displayName: frame.Label,
				host: host, port: port,
			}, nil
		}
		if time.Now().After(deadline) {
			return stockSimResolved{}, fmt.Errorf(
				"CloudNativePG in %s did not report a reachable endpoint within %s", frame.Label, timeout)
		}
		select {
		case <-ctx.Done():
			return stockSimResolved{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// stockSimAIODeclared finds the instance the node's picker names, in the AIO
// node's *design*. Working from the design rather than from a deployment is
// what lets the engine — and so the size target and the validation — be known
// before anything is provisioned.
func stockSimAIODeclared(doc designDoc, n designNode) (aioInstance, bool) {
	for _, other := range doc.Nodes {
		if other.ID != n.SSAIONode || other.Type != "aio" {
			continue
		}
		for _, in := range other.AIOInstances {
			if in.Name == n.SSAIOInstance {
				return in, true
			}
		}
	}
	return aioInstance{}, false
}

// stockSimAIOMember picks the concrete running member to connect to for a
// declared instance name.
//
// The picker names an instance as the user declared it ("pxc-cluster-01"); a
// deployed cluster instance is several members with generated names
// ("pxc-cluster-01-n2"), tied back by aioInstanceRuntime.Group. A standalone
// instance is one member whose Inst is the declared name and whose Group is
// empty. Where the members are not interchangeable the write endpoint is
// preferred, for the same reason waitMySQLFamilyFrame prefers a primary.
func stockSimAIOMember(dep Deployment, declared string) (aioInstanceRuntime, bool) {
	var fallback aioInstanceRuntime
	found := false
	for _, m := range aioTargetableInstances(dep) {
		if m.Inst != declared && m.Group != declared {
			continue
		}
		switch m.Role {
		case "primary", "bootstrap", "mongos":
			return m, true
		}
		if !found {
			fallback, found = m, true
		}
	}
	return fallback, found
}

// stockSimAIOTarget resolves an All in One instance to a connection, reusing
// the same helpers the Query Runner, Benchmark and Data Generator use so an
// instance behaves identically wherever it is addressed from.
func (a *App) stockSimAIOTarget(ctx context.Context, st Stack, doc designDoc, n designNode, timeout time.Duration) (stockSimResolved, error) {
	label := nodeLabel(doc, n.SSAIONode)

	deadline := time.Now().Add(timeout)
	for {
		dep, err := a.store.GetDeployment(st.ID, n.SSAIONode)
		switch {
		case err == nil && dep.State == DeployError:
			return stockSimResolved{}, fmt.Errorf("the selected All in One node %s failed to provision", label)
		case err == nil && dep.State == DeployRunning && dep.ContainerID != "":
			m, ok := stockSimAIOMember(dep, n.SSAIOInstance)
			if !ok {
				return stockSimResolved{}, fmt.Errorf(
					"All in One node %s has no ready instance called %q — check the instance is still declared on that node and finished starting",
					label, n.SSAIOInstance)
			}
			engine, port, user, pass := aioInstanceCreds(dep, m)
			host := m.FQDN
			display := aioTargetLabel(label, m)

			res := stockSimResolved{
				secrets: stockSimSecrets{User: user, Password: pass},
				engine:  engine, kind: "aio-" + engine, displayName: display,
				host: host, port: port,
			}
			switch engine {
			case "mysql":
				res.env = []string{"DB_ENGINE=mysql", fmt.Sprintf(
					"MYSQL_DSN=%s:%s@tcp(%s:%d)/?tls=false", user, pass, host, port)}
			case "postgres":
				res.env = []string{"DB_ENGINE=postgres", "POSTGRES_DSN=" + (&url.URL{
					Scheme: "postgres", User: url.UserPassword(user, pass),
					Host: fmt.Sprintf("%s:%d", host, port), Path: "/postgres",
					RawQuery: "sslmode=prefer&connect_timeout=10",
				}).String()}
			case "mongodb":
				res.env = []string{"DB_ENGINE=mongodb", fmt.Sprintf(
					"MONGO_URI=mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
					url.QueryEscape(user), url.QueryEscape(pass), host, port)}
			default:
				return stockSimResolved{}, fmt.Errorf(
					"All in One instance %q is a %s instance, which this application cannot drive", n.SSAIOInstance, m.Kind)
			}
			return res, nil
		}
		if time.Now().After(deadline) {
			return stockSimResolved{}, fmt.Errorf(
				"the selected All in One node %s did not become ready within %s", label, timeout)
		}
		select {
		case <-ctx.Done():
			return stockSimResolved{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// k3dServerConfig returns the stored k3dConfig of a K3D frame's server node —
// the one that carries the cluster-wide record, and whose container is the one
// kubectl runs in — along with that container's id.
func (a *App) k3dServerConfig(stackID int64, doc designDoc, frameID string) (k3dConfig, string, bool) {
	for _, n := range doc.Nodes {
		if n.FrameID != frameID || n.Type != "k3d" {
			continue
		}
		dep, err := a.store.GetDeployment(stackID, n.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" || len(dep.Config) == 0 {
			continue
		}
		var cfg k3dConfig
		if json.Unmarshal(dep.Config, &cfg) != nil || cfg.Role != "server" {
			continue
		}
		return cfg, dep.ContainerID, true
	}
	return k3dConfig{}, "", false
}

// splitHostPortDefault splits "host:port", falling back to def when there is no
// port to read.
func splitHostPortDefault(addr string, def int) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, def
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return h, def
	}
	return h, n
}

// waitMySQLFamilyNode blocks until a standalone MySQL-family node is running
// and returns its FQDN plus its stored credentials. Percona Server, MariaDB and
// MySQL CE all provision the same credential set (mysqlFamilySecrets), so one
// helper covers all three; only the wording of a timeout differs.
func (a *App) waitMySQLFamilyNode(ctx context.Context, stackID int64, nodeID string, hosts map[string]string, domain, kind string, timeout time.Duration) (string, pxcSecrets, error) {
	dep, err := a.waitStockSimNode(ctx, stackID, nodeID, timeout, engineDisplayLabel(stockSimEngineForKind(kind))+" ("+kind+")")
	if err != nil {
		return "", pxcSecrets{}, err
	}
	var sec pxcSecrets
	json.Unmarshal(dep.Secrets, &sec)
	return fqdnOf(hosts[nodeID], domain), sec, nil
}

// waitMySQLFamilyFrame blocks until a MySQL-family cluster frame has a usable
// write endpoint, and returns that member's FQDN plus the frame's credentials.
//
// Members of every one of these frames are nodes whose own type equals the
// frame's, which is what lets one helper serve MariaDB replication, Galera,
// MySQL CE replication and both flavours of Group Replication. wantPrimary
// selects the member marked primary — required where writes only work there
// (asynchronous replication) and pointless where they work anywhere (Galera,
// and Group Replication reached through its router).
func (a *App) waitMySQLFamilyFrame(ctx context.Context, stackID int64, frame designFrame, doc designDoc, domain string, wantPrimary bool, timeout time.Duration) (string, pxcSecrets, error) {
	hosts := stackHostnames(doc)
	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == frame.Type {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		return "", pxcSecrets{}, fmt.Errorf("associated cluster %s has no members", frame.Label)
	}
	// Stable order so a redeploy picks the same member, matching how every
	// provisioner in this codebase orders a frame's nodes.
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })

	deadline := time.Now().Add(timeout)
	for {
		deps, err := a.store.ListDeployments(stackID)
		if err == nil {
			byNode := map[string]Deployment{}
			for _, d := range deps {
				byNode[d.NodeID] = d
			}
			othersUp := false
			for _, n := range members {
				d, ok := byNode[n.ID]
				if !ok || d.State != DeployRunning || d.ContainerID == "" {
					continue
				}
				if !wantPrimary || n.Role == "primary" {
					var sec pxcSecrets
					json.Unmarshal(d.Secrets, &sec)
					return fqdnOf(hosts[n.ID], domain), sec, nil
				}
				othersUp = true
			}
			// A replication frame whose primary is down still has replicas up.
			// Writing to one would fail, so once the wait is over say what is
			// actually wrong rather than blaming readiness in general.
			if wantPrimary && othersUp && time.Now().After(deadline) {
				return "", pxcSecrets{}, fmt.Errorf(
					"associated cluster %s has running members but no primary — writes have nowhere to go", frame.Label)
			}
		}
		if time.Now().After(deadline) {
			return "", pxcSecrets{}, fmt.Errorf("associated cluster %s did not become ready within %s", frame.Label, timeout)
		}
		select {
		case <-ctx.Done():
			return "", pxcSecrets{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitStockSimNode blocks until a standalone node is running and returns its
// deployment, so the caller can read that node type's own stored secrets.
// Neither MongoDB nor Valkey standalone nodes have a dedicated wait helper —
// their sibling sims poll inline — so this is the shared one.
func (a *App) waitStockSimNode(ctx context.Context, stackID int64, nodeID string, timeout time.Duration, what string) (Deployment, error) {
	deadline := time.Now().Add(timeout)
	for {
		if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
			if dep.State == DeployError {
				return Deployment{}, fmt.Errorf("linked %s node failed to provision", what)
			}
			if dep.State == DeployRunning && dep.ContainerID != "" {
				return dep, nil
			}
		}
		if time.Now().After(deadline) {
			return Deployment{}, fmt.Errorf("linked %s node did not become ready within %s", what, timeout)
		}
		select {
		case <-ctx.Done():
			return Deployment{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// stockSimManualEnv builds the container environment from the node's own
// connection fields. Nothing is contacted here — that is the point of manual
// mode, and it is why the node form offers Test connection instead.
func stockSimManualEnv(n designNode) (env []string, sec stockSimSecrets, displayName string) {
	engine := stockSimEngine(n)
	port := n.SSPort
	if port == 0 {
		port = stockSimDefaultPort(engine)
	}
	host := strings.TrimSpace(n.SSHost)
	dsn := strings.TrimSpace(n.SSDSN)

	env = []string{
		"DB_ENGINE=" + engine,
		"DB_HOST=" + host,
		fmt.Sprintf("DB_PORT=%d", port),
		"DB_USER=" + n.SSUser,
		"DB_PASSWORD=" + n.SSPassword,
		"DB_TLS=" + stockSimTLS(n),
		"DB_PARAMS=" + n.SSParams,
	}
	if dsn != "" {
		env = append(env, "DB_DSN="+dsn)
	}
	sec = stockSimSecrets{User: n.SSUser, Password: n.SSPassword, DSN: dsn}

	switch {
	case strings.TrimSpace(n.SSLabel) != "":
		displayName = strings.TrimSpace(n.SSLabel)
	case host != "":
		displayName = fmt.Sprintf("%s:%d", host, port)
	default:
		displayName = "external " + engine
	}
	return env, sec, displayName
}

func stockSimTLS(n designNode) string {
	switch n.SSTLS {
	case "disable", "prefer", "require":
		return n.SSTLS
	}
	return "prefer"
}

// handleStockSimTest checks a connection *before* anything is deployed with
// it. An external endpoint is exactly the case where a typo should not be
// discovered via a failed deploy, so this starts a throwaway sim container with
// the candidate settings, execs `stocksim -testconn` inside it, and returns
// that one line of output verbatim.
//
// It runs the check inside a container rather than dialing from the dbcanvas
// server on purpose: the container is what will actually have to reach the
// database, and it sits on the stack's own bridge network with the stack's own
// DNS. Testing from anywhere else would be testing the wrong path.
func (a *App) handleStockSimTest(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		Engine   string `json:"engine"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
		TLS      string `json:"tls"`
		Params   string `json:"params"`
		DSN      string `json:"dsn"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	n := designNode{
		SSEngine: body.Engine, SSHost: body.Host, SSPort: body.Port,
		SSUser: body.User, SSPassword: body.Password, SSDatabase: body.Database,
		SSTLS: body.TLS, SSParams: body.Params, SSDSN: body.DSN,
	}
	// Settings the sim binary would refuse outright are answered here rather
	// than by starting a container: those failures are fatal at startup, so the
	// container would exit and there would be nothing left to exec the check
	// in. Everything that remains is a real connection question, which only the
	// container can answer.
	if msg := stockSimConfigError(n); msg != "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "error: " + msg})
		return
	}

	ctx := r.Context()
	eng := a.engCtx(ctx)
	if ok, _ := eng.ImageExists(ctx, stockSimImage); !ok {
		writeErr(w, http.StatusPreconditionFailed,
			"image "+stockSimImage+" not found — run `make stocksim-image` first")
		return
	}

	env, _, _ := stockSimManualEnv(n)
	env = append(env, "DB_NAME="+stockSimDatabase(n), fmt.Sprintf("PORT=%d", stockSimPort))

	// Join the stack's own network when it exists, so the test takes the same
	// path the deployed node will. Before a stack's first deploy that network
	// has not been created yet — and testing a connection *before* deploying is
	// exactly what this endpoint is for — so fall back to Docker's default
	// bridge. An external database is reachable either way: egress and
	// host-gateway both work on the default bridge, and a target inside the
	// stack could not have been running before the stack was deployed anyway.
	network := networkName(st.ID)
	if _, nerr := eng.NetworkSubnet(ctx, network); nerr != nil {
		network = ""
	}

	// A distinct name per attempt, so two people testing at once cannot
	// collide, and a leftover from a crashed attempt never blocks a new one.
	name := fmt.Sprintf("dbcanvas-%d-stocksim-testconn-%d", st.ID, time.Now().UnixNano())
	id, err := eng.ContainerCreate(ctx, ContainerSpec{
		Name: name, Image: stockSimImage, Env: env,
		Network:    network,
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		NoRestart:  true,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create test container: "+err.Error())
		return
	}
	defer eng.ContainerRemove(context.Background(), id)

	if err := eng.ContainerStart(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "start test container: "+err.Error())
		return
	}
	res, err := eng.Exec(ctx, id, []string{"/stocksim", "-testconn"}, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "run connection test: "+err.Error())
		return
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = strings.TrimSpace(res.Stderr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.Code == 0, "message": out})
}

// waitStockSimHealthy polls until the container's own /healthz answers 200. The
// runtime image is distroless (no shell, no curl) — matching waitCarSimHealthy,
// this execs the check inside the container: `stocksim -healthcheck` exits 0
// only if /healthz answers 200.
func (a *App) waitStockSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/stocksim", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}
