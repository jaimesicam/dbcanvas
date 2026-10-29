package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Network and disk conditions are the stressors the sim cannot apply itself:
// latency and loss live in a member's network namespace (tc netem), bandwidth
// limits in its cgroup (io.max), and neither is reachable over a database
// connection. DBCanvas applies them. The sim only says what it wants; DBCanvas
// polls it (POST /api/conditions/sync), applies the difference to the members
// it resolved for this sim, and reports what the kernel now has. DBCanvas also
// enforces every maximum hold and clears everything if the sim stops answering,
// so a condition never outlives the sim that asked for it.
//
// Until DBCanvas has polled, the dials are listed as unavailable with the
// reason; when it says it cannot apply one kind (a cgroup v1 host has no
// io.max, a Vagrant VM is not a container), that kind stays unavailable with
// DBCanvas's own reason.

// NetSpec and DiskSpec are one member's requested conditions.
type NetSpec struct {
	LatencyMS int     `json:"latencyMs,omitempty"`
	JitterMS  int     `json:"jitterMs,omitempty"`
	LossPct   float64 `json:"lossPct,omitempty"`
	RateMbit  int     `json:"rateMbit,omitempty"`
}

type DiskSpec struct {
	ReadMBps  int `json:"readMBps,omitempty"`
	WriteMBps int `json:"writeMBps,omitempty"`
	WriteIOPS int `json:"writeIops,omitempty"`
}

// Desired is what the sim asks DBCanvas for, per member.
type Desired struct {
	Member string    `json:"member"`
	Net    *NetSpec  `json:"net,omitempty"`
	Disk   *DiskSpec `json:"disk,omitempty"`
	Until  int64     `json:"until"`
}

// Applied is what DBCanvas reports for a member: the kernel's own description
// of what is in force, or why it could not apply it.
type Applied struct {
	Net  string `json:"net,omitempty"`
	Disk string `json:"disk,omitempty"`
	Err  string `json:"err,omitempty"`
}

// Caps is what DBCanvas says it can apply for this sim.
type Caps struct {
	Net     bool   `json:"net"`
	Disk    bool   `json:"disk"`
	NetWhy  string `json:"netWhy,omitempty"`
	DiskWhy string `json:"diskWhy,omitempty"`
	// DiskWarn is shown on the disk stressors when they work but have a catch
	// on this host (members under emulation).
	DiskWarn string `json:"diskWarn,omitempty"`
}

type Conditions struct {
	mu       sync.Mutex
	caps     *Caps
	lastSync time.Time
	applied  map[string]Applied
	wants    map[string]condWant // by dial id
}

type condWant struct {
	targets []string
	params  func() Params
	until   time.Time
}

// syncStale is how long after DBCanvas's last poll the dials count as
// unavailable again: it polls every two seconds.
const syncStale = 20 * time.Second

func newConditions() *Conditions {
	return &Conditions{applied: map[string]Applied{}, wants: map[string]condWant{}}
}

const (
	groupNet  = "Network"
	groupDisk = "Disk"
)

var condHold = Param{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{30, 60, 120, 300, 600}, Def: 120, Unit: "s"}

// Dials lists the network and disk stressors with whether they can be set now.
func (c *Conditions) Dials() []Dial {
	c.mu.Lock()
	caps, fresh := c.caps, time.Since(c.lastSync) < syncStale
	c.mu.Unlock()
	avail := func(d Dial, kind string) Dial {
		switch {
		case caps == nil || !fresh:
			d.Why = "DBCanvas applies network and disk conditions, and it has not connected to this sim yet. It does within a few seconds of the sim starting."
		case kind == "net" && !caps.Net:
			d.Why = nz(caps.NetWhy, "DBCanvas cannot shape these members' network.")
		case kind == "disk" && !caps.Disk:
			d.Why = nz(caps.DiskWhy, "DBCanvas cannot limit these members' disk.")
		default:
			d.Available = true
			if kind == "disk" {
				d.Warn = caps.DiskWarn
			}
		}
		return d
	}
	net := "Applied by DBCanvas with tc netem on the chosen members, on their database and cluster ports only."
	disk := "Applied by DBCanvas through the chosen members' cgroup io.max, on the device behind Docker's storage."
	return []Dial{
		avail(Dial{ID: "netlat", Group: groupNet, Name: "Network latency", Kind: "toggle", Members: "all",
			Desc:  "Delays every packet on the chosen members' database and cluster ports. Anything that waits for a reply from them pays the round trip: commits on Galera and semi-sync, the replica's receiver, the application on the writer.",
			Watch: []string{"p99 latency", "Replication lag"}, Note: net,
			Params: []Param{{Key: "ms", Label: "Added delay", Type: "steps", Opts: []any{5, 20, 50, 100, 200, 500}, Def: 50, Unit: "ms"},
				{Key: "jitter", Label: "Jitter", Type: "pills", Opts: []any{0, 10, 25}, Def: 10, Unit: "%"}, condHold}}, "net"),
		avail(Dial{ID: "netloss", Group: groupNet, Name: "Packet loss", Kind: "toggle", Members: "all",
			Desc:  "Drops a share of packets on the chosen members' database and cluster ports. Retransmits add latency; at 100% the members are cut off, which on Galera evicts them.",
			Watch: []string{"p99 latency", "Errors"}, Note: net,
			Params: []Param{{Key: "loss", Label: "Packets dropped", Type: "steps", Opts: []any{1, 5, 10, 30, 100}, Def: 5, Unit: "%"}, condHold}}, "net"),
		avail(Dial{ID: "netbw", Group: groupNet, Name: "Bandwidth cap", Kind: "toggle", Members: "all",
			Desc:  "Caps what the chosen members send on their database and cluster ports. A primary's replication stream is what it sends, so cap the primary to slow its replicas; capping a replica slows only its acknowledgements and its own clients' replies.",
			Watch: []string{"Replication lag"}, Note: net,
			Params: []Param{{Key: "mbit", Label: "Rate", Type: "steps", Opts: []any{100, 10, 1}, Def: 10, Unit: "Mbit/s"}, condHold}}, "net"),
		avail(Dial{ID: "diskw", Group: groupDisk, Name: "Write bandwidth cap", Kind: "toggle", Members: "all",
			Desc:  "Caps how fast the chosen members write. Commits wait on the flush, a replica that writes slowly falls behind, and a slow Galera node sends flow control.",
			Watch: []string{"Replication lag", "p99 latency"}, Note: disk,
			Params: []Param{{Key: "wmb", Label: "Writes up to", Type: "steps", Opts: []any{100, 20, 5, 1}, Def: 5, Unit: "MB/s"}, condHold}}, "disk"),
		avail(Dial{ID: "diskiops", Group: groupDisk, Name: "Write IOPS cap", Kind: "toggle", Members: "all",
			Desc:  "Caps write operations per second. Each fsync waits its turn, so commit latency rises even when little data is written.",
			Watch: []string{"p99 latency", "Replication lag"}, Note: disk,
			Params: []Param{{Key: "iops", Label: "Write operations per second", Type: "steps", Opts: []any{1000, 200, 50, 20}, Def: 200, Unit: "ops"}, condHold}}, "disk"),
		avail(Dial{ID: "diskr", Group: groupDisk, Name: "Read bandwidth cap", Kind: "toggle", Members: "all",
			Desc:  "Caps reads that reach the disk. Reads served from memory are not affected, so it bites when the working set does not fit.",
			Watch: []string{"Read QPS", "p99 latency"}, Note: disk,
			Params: []Param{{Key: "rmb", Label: "Reads up to", Type: "steps", Opts: []any{100, 20, 5}, Def: 20, Unit: "MB/s"}, condHold}}, "disk"),
	}
}

func (c *Conditions) owns(id string) bool {
	switch id {
	case "netlat", "netloss", "netbw", "diskw", "diskiops", "diskr":
		return true
	}
	return false
}

// desired merges every dial that is on into one request per member.
func (c *Conditions) desired() []Desired {
	c.mu.Lock()
	defer c.mu.Unlock()
	by := map[string]*Desired{}
	get := func(m string) *Desired {
		if by[m] == nil {
			by[m] = &Desired{Member: m}
		}
		return by[m]
	}
	for id, w := range c.wants {
		p := w.params()
		for _, m := range w.targets {
			d := get(m)
			if d.Until == 0 || w.until.Unix() < d.Until {
				d.Until = w.until.Unix()
			}
			switch id {
			case "netlat", "netloss", "netbw":
				if d.Net == nil {
					d.Net = &NetSpec{}
				}
				switch id {
				case "netlat":
					d.Net.LatencyMS = p.Int("ms", 50)
					d.Net.JitterMS = d.Net.LatencyMS * p.Int("jitter", 0) / 100
				case "netloss":
					d.Net.LossPct = float64(p.Int("loss", 5))
				case "netbw":
					d.Net.RateMbit = p.Int("mbit", 10)
				}
			default:
				if d.Disk == nil {
					d.Disk = &DiskSpec{}
				}
				switch id {
				case "diskw":
					d.Disk.WriteMBps = p.Int("wmb", 5)
				case "diskiops":
					d.Disk.WriteIOPS = p.Int("iops", 200)
				case "diskr":
					d.Disk.ReadMBps = p.Int("rmb", 20)
				}
			}
		}
	}
	out := make([]Desired, 0, len(by))
	for _, d := range by {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Member < out[j].Member })
	return out
}

// sync is DBCanvas's poll: it reports what it can do and what is applied, and
// gets back what is wanted.
func (c *Conditions) sync(caps Caps, applied map[string]Applied) []Desired {
	c.mu.Lock()
	c.caps, c.lastSync = &caps, time.Now()
	if applied == nil {
		applied = map[string]Applied{}
	}
	c.applied = applied
	c.mu.Unlock()
	return c.desired()
}

// condJob is a network or disk dial that is on: an entry in wants, removed on stop.
type condJob struct {
	c      *Conditions
	id     string
	kind   string
	tgts   []string
	mu     sync.Mutex
	params Params
	start  time.Time
}

func (c *Conditions) start(id string, p Params, targets []string, hold int) *condJob {
	j := &condJob{c: c, id: id, kind: map[bool]string{true: "net", false: "disk"}[strings.HasPrefix(id, "net")], tgts: targets, params: p, start: time.Now()}
	until := time.Now().Add(time.Duration(hold) * time.Second)
	if hold <= 0 {
		until = time.Now().Add(time.Hour)
	}
	c.mu.Lock()
	c.wants[id] = condWant{targets: targets, params: j.param, until: until}
	c.mu.Unlock()
	return j
}

func (j *condJob) param() Params {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.params
}

func (j *condJob) Update(p Params) {
	j.mu.Lock()
	j.params = p
	j.mu.Unlock()
	j.c.mu.Lock()
	if w, ok := j.c.wants[j.id]; ok {
		if hold := p.Int("hold", 0); hold > 0 {
			w.until = j.start.Add(time.Duration(hold) * time.Second)
		}
		j.c.wants[j.id] = w
	}
	j.c.mu.Unlock()
}

// Stop withdraws the request. DBCanvas clears it on its next poll, within a
// couple of seconds; the sim does not wait for that.
func (j *condJob) Stop(ctx context.Context) error {
	j.c.mu.Lock()
	delete(j.c.wants, j.id)
	j.c.mu.Unlock()
	return nil
}

func (j *condJob) Status() RunStatus {
	j.c.mu.Lock()
	defer j.c.mu.Unlock()
	var got, errs []string
	for _, m := range j.tgts {
		a := j.c.applied[m]
		v := a.Net
		if j.kind == "disk" {
			v = a.Disk
		}
		if a.Err != "" {
			errs = append(errs, m+": "+a.Err)
		}
		if v != "" {
			got = append(got, m+": "+v)
		}
	}
	st := RunStatus{Progress: -1}
	switch {
	case len(got) == 0 && len(errs) == 0:
		st.Phase = "Waiting for DBCanvas to apply it"
	default:
		st.Phase = "Applied on " + strings.Join(j.tgts, ", ")
		st.Note = "Kernel reports — " + strings.Join(got, " · ")
	}
	if len(errs) > 0 {
		st.Err = strings.Join(errs, "; ")
	}
	return st
}

func (j *condJob) Holders() map[string][]string { return nil }

// syncHandler is POST /api/conditions/sync.
func (c *Conditions) syncHandler(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Caps    Caps               `json:"caps"`
		Applied map[string]Applied `json:"applied"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, 400, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	writeJSON(w, map[string]any{"conditions": c.sync(in.Caps, in.Applied)})
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
