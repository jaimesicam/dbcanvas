package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
)

// Per-node network conditions — latency, jitter, packet loss and a bandwidth
// cap, applied to a running node with tc.
//
// This is the fourth resource a node can be constrained on. CPU and memory are
// container limits set at create time (ContainerSpec.CPUs/MemoryMB) and disk is
// blk-throttle (blkio.go); the network had nothing at all, which left the two
// pathologies that break a synchronous cluster — latency and loss between
// members — impossible to produce.
//
// # Why latency and loss rather than bandwidth
//
// Bandwidth is the least interesting of the four for a replicated database. A
// bandwidth cap mostly slows a state transfer down. What breaks Galera is delay
// and loss, and measuring it on a real three-node cluster showed the mechanism
// is not quite the one usually described:
//
//   - A degraded link (200ms ±40ms, 10% loss, 1 Mbit on the cluster ports)
//     backs up the *sender*: wsrep_local_send_queue on the writer rises and a
//     bulk insert that took 22ms unimpaired ran for minutes. Receiver-side flow
//     control did not fire — wsrep_flow_control_paused/sent stayed at zero and
//     the impaired node's recv queue stayed empty. That is consistent rather
//     than surprising: flow control is a receiver telling the group it cannot
//     *apply* fast enough, so it is provoked by a slow node (CPU, disk), while
//     a slow *link* starves the receiver instead of flooding it. Both are worth
//     reproducing; they are not the same experiment.
//   - Severing the link (100% loss) evicts the member. Measured: 8 seconds from
//     applying the loss to the majority side reporting cluster_size 2 / Primary
//     while the isolated node reported cluster_size 1 / non-Primary and stopped
//     accepting writes. Clearing the shaping rejoined it inside 11 seconds.
//
// So the honest summary is that this knob reliably produces a partition and a
// sender-side stall, and that receiver-side flow control needs a slow node
// rather than a slow link. Bandwidth is offered too, because a saturated link is
// real, but it is the third knob rather than the first.
//
// # Why tc rather than a container limit
//
// Docker has no bandwidth or latency setting — there is no --net-bps the way
// there is --device-read-bps. Shaping has to happen inside the container's
// network namespace, which needs NET_ADMIN; stack nodes already run privileged,
// so tc can simply be exec'd into the node.
//
// That turns out to be an advantage over the disk limits rather than a
// compromise. blk-throttle values are create-time only and the update API
// silently drops them, so changing a disk limit means recreating the node. A tc
// qdisc is applied, changed and removed at runtime on a live node, and the
// result is readable back out of the kernel with `tc -s qdisc show` — so
// "did this actually apply" is a measurement rather than an assumption.
//
// # Why the shaping is scoped to ports
//
// `tc qdisc add dev eth0 root netem delay 200ms loss 10%` shapes *everything*
// the container sends: DNS to the intranet node, LDAP, the package mirror, and
// any health check that goes over the network. A node given a realistic WAN
// impairment on all traffic looks broken rather than slow, and can fail its own
// provisioning. So the qdisc is an htb tree with an unshaped default class, and
// only traffic on the node's database and cluster ports is filtered into the
// impaired class. netemAllTraffic is available for the case where the whole NIC
// is meant to be bad, and says so on the canvas.

// netemDev is the interface shaped inside the container. Stack nodes are
// attached to exactly one network, so the default route's device is the one
// carrying both client and cluster traffic. Resolved at apply time from inside
// the container rather than assumed to be eth0.
const netemDefaultDev = "eth0"

// Bounds. These are sanity rails on a control whose entire purpose is to make a
// node behave badly, not opinions about what is realistic.
const (
	// A full second of one-way delay is far past the point where any cluster
	// stays formed; beyond that the number is a typo.
	netemMaxLatencyMS = 1000
	netemMaxJitterMS  = 500
	// 100% loss is a legitimate setting — it is how you model a severed link
	// without touching the network — so the cap is the real maximum.
	netemMaxLossPct = 100
	// Ten gigabit is the "unshaped" rate used for the default class, so a cap
	// above it would be meaningless.
	netemMaxRateMbit = 10000
	netemMinRateMbit = 1
)

// netemSpec is one node's network conditions, already clamped.
type netemSpec struct {
	LatencyMS int     `json:"latencyMs,omitempty"`
	JitterMS  int     `json:"jitterMs,omitempty"`
	LossPct   float64 `json:"lossPct,omitempty"`
	RateMbit  int     `json:"rateMbit,omitempty"`
	// AllTraffic shapes every packet the node sends rather than only its
	// database and cluster ports. Off by default — see the package comment.
	AllTraffic bool `json:"allTraffic,omitempty"`
	// Ports is what AllTraffic=false filters on, derived from the node type.
	Ports []int `json:"ports,omitempty"`
}

// Empty reports whether this spec asks for nothing, in which case the node is
// left alone (and any existing qdisc is cleared).
func (s netemSpec) Empty() bool {
	return s.LatencyMS <= 0 && s.JitterMS <= 0 && s.LossPct <= 0 && s.RateMbit <= 0
}

// Impaired reports whether anything netem itself has to do is asked for, as
// opposed to a pure bandwidth cap which htb handles on its own.
func (s netemSpec) Impaired() bool {
	return s.LatencyMS > 0 || s.JitterMS > 0 || s.LossPct > 0
}

// clampNetem normalises a requested set of conditions.
//
// Jitter is clamped to the latency for a reason worth stating: netem draws each
// packet's delay from latency ± jitter, so a jitter larger than the latency
// produces negative delays, which netem clamps to zero and — worse — reorders
// packets, since a later packet can be scheduled before an earlier one. TCP
// reads that reordering as loss and the result no longer models the link that
// was asked for.
func clampNetem(latency, jitter int, loss float64, rate int, all bool, ports []int) netemSpec {
	s := netemSpec{AllTraffic: all}
	switch {
	case latency < 0:
		s.LatencyMS = 0
	case latency > netemMaxLatencyMS:
		s.LatencyMS = netemMaxLatencyMS
	default:
		s.LatencyMS = latency
	}
	switch {
	case jitter < 0:
		s.JitterMS = 0
	case jitter > netemMaxJitterMS:
		s.JitterMS = netemMaxJitterMS
	default:
		s.JitterMS = jitter
	}
	if s.JitterMS > s.LatencyMS {
		s.JitterMS = s.LatencyMS
	}
	switch {
	case loss < 0:
		s.LossPct = 0
	case loss > netemMaxLossPct:
		s.LossPct = netemMaxLossPct
	default:
		s.LossPct = loss
	}
	switch {
	case rate <= 0:
		s.RateMbit = 0
	case rate < netemMinRateMbit:
		s.RateMbit = netemMinRateMbit
	case rate > netemMaxRateMbit:
		s.RateMbit = netemMaxRateMbit
	default:
		s.RateMbit = rate
	}
	if !all {
		s.Ports = normalisePorts(ports)
	}
	return s
}

func normalisePorts(ports []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if p <= 0 || p > 65535 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// netemPortsFor is the set of ports shaping applies to for a node type: the one
// clients connect on, plus whatever the members use to talk to each other.
//
// The cluster ports are the point. Impairing 3306 alone models a bad link to the
// application; impairing 4567 models a bad link between members, which is what
// produces flow control and eviction, and those are different experiments.
func netemPortsFor(nodeType string) []int {
	switch nodeType {
	// Galera, in both its flavours: client, group communication, IST, SST.
	case "pxc", "mariadbgalera":
		return []int{3306, 4444, 4567, 4568}
	// MySQL family replication — asynchronous or group, all over the client port.
	case "ps", "mysql", "innodb", "mysqlce", "mysqlcerepl", "mysqlceinnodb",
		"mariadb", "mariadbrepl":
		return []int{3306}
	// PostgreSQL: streaming replication shares 5432; Patroni's REST API on 8008
	// is how members agree who is primary, so a partition there is its own
	// failure mode. repmgr and spock use 5432 only.
	case "patroni":
		return []int{5432, 8008}
	case "pg", "repmgr", "spock":
		return []int{5432}
	// MongoDB replica sets and shards all speak on the client port.
	case "psm", "psmdb", "psmrs":
		return []int{27017}
	// Valkey cluster bus is the client port + 10000.
	case "valkey", "valkeycluster":
		return []int{6379, 16379}
	// Routers: the port clients arrive on and the port they are sent to.
	case "proxysql":
		return []int{3306, 6032, 6033}
	case "haproxy":
		return []int{3306, 5000, 5432}
	}
	return nil
}

// netemScript builds and applies the qdisc tree. Written as one shell script so
// the whole change is a single exec and cannot half-apply across round trips.
//
// The shape is an htb tree with two classes:
//
//	1:10  default, 10gbit — everything not matched, i.e. unshaped
//	1:20  the impaired class, rate-capped if asked, with netem beneath it
//
// and u32 filters steering the node's database/cluster ports into 1:20. Both
// source and destination are matched: a reply from the node leaves with the
// cluster port as its *source*, and shaping only one direction would impair a
// request but not its response.
//
// Everything is torn down first so the operation is idempotent — applying twice
// must not stack two netem qdiscs, which would double the delay.
const netemScript = `set -e
DEV="${DEV:-eth0}"
if ! command -v tc >/dev/null 2>&1; then
  echo "tc is not installed in this image" >&2
  exit 127
fi
# Resolve the interface actually carrying traffic rather than trusting a name.
if ! ip link show "$DEV" >/dev/null 2>&1; then
  DEV=$(ip -o route get 1.1.1.1 2>/dev/null | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1)
  [ -n "$DEV" ] || DEV=$(ip -o link show 2>/dev/null | awk -F': ' '$2!="lo"{print $2; exit}')
fi
[ -n "$DEV" ] || { echo "no network interface to shape" >&2; exit 1; }

# Idempotent: remove whatever is there before building. A missing qdisc is not
# an error, so this must not trip set -e.
tc qdisc del dev "$DEV" root 2>/dev/null || true
if [ -z "$SPEC" ]; then
  echo "cleared $DEV"
  exit 0
fi

# quantum is set explicitly on both classes. Without it htb derives one from the
# rate and prints "quantum of class ... is big. Consider r2q change." to stderr
# for anything near 10gbit — harmless, but it would land in the deploy log on
# every node and read like a fault. 200000 is htb's maximum.
tc qdisc add dev "$DEV" root handle 1: htb default 10
tc class add dev "$DEV" parent 1: classid 1:10 htb rate 10gbit quantum 200000
tc class add dev "$DEV" parent 1: classid 1:20 htb rate "$RATE" quantum 200000
if [ -n "$NETEM" ]; then
  # shellcheck disable=SC2086
  tc qdisc add dev "$DEV" parent 1:20 handle 20: netem $NETEM
fi
for p in $PORTS; do
  tc filter add dev "$DEV" protocol ip parent 1:0 prio 1 u32 \
     match ip dport "$p" 0xffff flowid 1:20
  tc filter add dev "$DEV" protocol ip parent 1:0 prio 1 u32 \
     match ip sport "$p" 0xffff flowid 1:20
done
if [ -z "$PORTS" ]; then
  # All traffic: send everything to the impaired class instead of filtering.
  tc filter add dev "$DEV" protocol ip parent 1:0 prio 2 u32 \
     match u32 0 0 flowid 1:20
fi
echo "shaped $DEV"
tc -s qdisc show dev "$DEV"`

// netemArgs renders the netem parameter string. Order matters to tc: delay and
// its jitter come as positional arguments, loss is a keyword.
func (s netemSpec) netemArgs() string {
	if !s.Impaired() {
		return ""
	}
	var b strings.Builder
	if s.LatencyMS > 0 {
		fmt.Fprintf(&b, "delay %dms", s.LatencyMS)
		if s.JitterMS > 0 {
			// A normal distribution rather than tc's default uniform one: real
			// links cluster around a typical RTT with occasional outliers, and
			// uniform jitter produces a flat spread nothing actually looks like.
			fmt.Fprintf(&b, " %dms distribution normal", s.JitterMS)
		}
	}
	if s.LossPct > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "loss %s%%", trimFloat(s.LossPct))
	}
	return b.String()
}

// rateArg is the htb ceiling for the impaired class. With no cap asked for it
// is the same 10gbit as the default class, so htb is a pass-through and netem
// alone does the work.
func (s netemSpec) rateArg() string {
	if s.RateMbit <= 0 {
		return "10gbit"
	}
	return strconv.Itoa(s.RateMbit) + "mbit"
}

func (s netemSpec) portsArg() string {
	if s.AllTraffic {
		return ""
	}
	out := make([]string, 0, len(s.Ports))
	for _, p := range s.Ports {
		out = append(out, strconv.Itoa(p))
	}
	return strings.Join(out, " ")
}

// trimFloat renders a percentage without a trailing ".0", because tc accepts
// both but "10%" reads better than "10.0%" in the log line the UI shows.
func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return s
}

// netemEnv is the environment netemScript reads. SPEC is the on/off switch:
// empty means tear down and leave the interface alone.
func (s netemSpec) netemEnv(dev string) []string {
	spec := ""
	if !s.Empty() {
		spec = "on"
	}
	if dev == "" {
		dev = netemDefaultDev
	}
	return []string{
		"DEV=" + dev,
		"SPEC=" + spec,
		"RATE=" + s.rateArg(),
		"NETEM=" + s.netemArgs(),
		"PORTS=" + s.portsArg(),
	}
}

// applyNetem puts the conditions on a running container, or clears them when the
// spec is empty. Returns tc's own description of what the kernel now has, which
// is what the deploy log shows — an applied qdisc that reports itself is the
// difference between shaping and hoping.
func applyNetem(ctx context.Context, eng Engine, containerID string, s netemSpec) (string, error) {
	res, err := eng.Exec(ctx, containerID, []string{"sh", "-c", netemScript}, s.netemEnv(""))
	if err != nil {
		return "", fmt.Errorf("apply network conditions: %w", err)
	}
	if res.Code != 0 {
		return "", fmt.Errorf("tc exited %d: %s", res.Code,
			strings.TrimSpace(res.Stderr+" "+res.Stdout))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// describeNetem is the one-line summary for the deploy log and the node panel.
func describeNetem(s netemSpec) string {
	if s.Empty() {
		return "none"
	}
	parts := []string{}
	if s.LatencyMS > 0 {
		p := fmt.Sprintf("%dms latency", s.LatencyMS)
		if s.JitterMS > 0 {
			p += fmt.Sprintf(" ±%dms", s.JitterMS)
		}
		parts = append(parts, p)
	}
	if s.LossPct > 0 {
		parts = append(parts, trimFloat(s.LossPct)+"% loss")
	}
	if s.RateMbit > 0 {
		parts = append(parts, fmt.Sprintf("%d Mbit/s", s.RateMbit))
	}
	scope := "on cluster traffic"
	if s.AllTraffic {
		scope = "on all traffic"
	} else if len(s.Ports) > 0 {
		ports := make([]string, 0, len(s.Ports))
		for _, p := range s.Ports {
			ports = append(ports, strconv.Itoa(p))
		}
		scope = "on ports " + strings.Join(ports, "/")
	}
	return strings.Join(parts, ", ") + " " + scope
}

// ---------------------------------------------------------------- reconcile

// nodeNetemSpec resolves a node's requested conditions against its type.
func nodeNetemSpec(n designNode) netemSpec {
	return clampNetem(n.NetLatencyMS, n.NetJitterMS, n.NetLossPct, n.NetRateMbit,
		n.NetAllTraffic, netemPortsFor(n.Type))
}

// netemSupported reports whether shaping is offered for a node type. It is the
// database and router nodes: they run the systemd image, which carries tc, and
// they are the ones with cluster traffic worth impairing. The first-party sim
// containers are scratch images with no shell, let alone tc.
func netemSupported(nodeType string) bool { return len(netemPortsFor(nodeType)) > 0 }

// reconcileNetem is the deploy's last phase: once the clusters are up, impair
// the links that were asked to be impaired.
//
// It runs after everything else, and that ordering is the whole design. Shaping
// applied *during* provisioning would be in force while Galera runs its state
// transfer, and a 200ms/10%-loss link fails SST — so the cluster would never
// form and the result would be a broken stack rather than a degraded one. Form
// the cluster first, then break the network under it. That is also the sequence
// of the real incident being modelled.
//
// Every supported node is visited, not only the ones asking for impairment,
// because clearing has to happen too: a node whose conditions were removed on a
// redeploy must lose its qdisc, and applying an empty spec is what does that.
// The script is idempotent and tearing down an interface that was never shaped
// is a no-op, so the cost of visiting an unshaped node is one cheap exec.
func (a *App) reconcileNetem(ctx context.Context, st Stack, doc designDoc) {
	type target struct {
		node designNode
		spec netemSpec
	}
	var targets []target
	for _, n := range doc.Nodes {
		if netemSupported(n.Type) {
			targets = append(targets, target{node: n, spec: nodeNetemSpec(n)})
		}
	}
	if len(targets) == 0 {
		return
	}

	eng := a.eng(st)
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		if !a.waitNodeRunning(st.ID, t.node.ID, deployTimeout()) {
			if !t.spec.Empty() {
				log.Printf("stack %d netem: node %s not running; network conditions not applied",
					st.ID, t.node.Label)
			}
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, t.node.ID)
		if err != nil || dep.ContainerID == "" {
			continue
		}
		out, err := applyNetem(ctx, eng, dep.ContainerID, t.spec)
		if err != nil {
			// A node that could not be impaired is worth saying out loud: the
			// experiment it was deployed for will silently not be running.
			log.Printf("stack %d netem: %s: %v", st.ID, t.node.Label, err)
			if !t.spec.Empty() {
				a.notifyStack(st.ID, "stack.netem", "warning", "Network conditions not applied",
					fmt.Sprintf("%s: %v. The node is running, but its link is not impaired.",
						t.node.Label, err), t.node.ID)
			}
			continue
		}
		if !t.spec.Empty() {
			log.Printf("stack %d netem: %s — %s", st.ID, t.node.Label, describeNetem(t.spec))
			a.notifyStack(st.ID, "stack.netem", "info", "Network conditions applied",
				fmt.Sprintf("%s now has %s.", t.node.Label, describeNetem(t.spec)), t.node.ID)
		}
		_ = out
	}
}

// netemIssues validates a node's requested conditions on the canvas, so a
// setting that will not do what it looks like it does is caught before deploy
// rather than found by wondering why a cluster is fine.
func netemIssues(n designNode) []issue {
	asked := n.NetLatencyMS > 0 || n.NetJitterMS > 0 || n.NetLossPct > 0 || n.NetRateMbit > 0
	if !asked && !n.NetAllTraffic {
		return nil
	}
	var out []issue
	if !netemSupported(n.Type) {
		return []issue{{"warning", fmt.Sprintf(
			"Node %s asks for network conditions, which will be ignored: shaping is offered "+
				"only on database and router nodes, which run an image carrying tc.", n.Label)}}
	}

	switch {
	case n.NetLatencyMS < 0 || n.NetJitterMS < 0 || n.NetRateMbit < 0 || n.NetLossPct < 0:
		out = append(out, issue{"error", fmt.Sprintf(
			"Node %s has a negative network condition — leave a field at 0 to switch it off", n.Label)})
	case n.NetLatencyMS > netemMaxLatencyMS:
		out = append(out, issue{"warning", fmt.Sprintf(
			"Node %s asks for %dms of latency; it will be capped at %dms",
			n.Label, n.NetLatencyMS, netemMaxLatencyMS)})
	}
	if n.NetJitterMS > n.NetLatencyMS && n.NetLatencyMS > 0 {
		// Not cosmetic: netem draws each delay from latency ± jitter, so jitter
		// above the latency yields negative delays that reorder packets, and TCP
		// reads reordering as loss.
		out = append(out, issue{"warning", fmt.Sprintf(
			"Node %s has jitter (%dms) above its latency (%dms); it will be capped at the latency, "+
				"because a larger jitter reorders packets instead of delaying them",
			n.Label, n.NetJitterMS, n.NetLatencyMS)})
	}
	if n.NetJitterMS > 0 && n.NetLatencyMS == 0 {
		out = append(out, issue{"warning", fmt.Sprintf(
			"Node %s sets jitter with no latency, which does nothing — jitter is a spread around "+
				"a delay, so give it a latency to vary", n.Label)})
	}
	if n.NetLossPct >= 100 {
		out = append(out, issue{"info", fmt.Sprintf(
			"Node %s will drop 100%% of its cluster traffic — the link is severed while the node "+
				"stays up, which is how you model a partition rather than a crash", n.Label)})
	}
	if n.NetAllTraffic {
		out = append(out, issue{"warning", fmt.Sprintf(
			"Node %s shapes all traffic, not just its database and cluster ports. DNS, LDAP and "+
				"anything else it talks to are impaired too, so it may look broken rather than "+
				"slow — and on a redeploy it can fail its own provisioning.", n.Label)})
	}
	if asked && len(out) == 0 {
		spec := nodeNetemSpec(n)
		out = append(out, issue{"info", fmt.Sprintf(
			"Node %s will run with %s, applied after the cluster forms.", n.Label, describeNetem(spec))})
	}
	return out
}
