// Package core is the engine-independent half of Performance Sim: the dial
// registry, the once-a-second timeline, the steady workload whose latency the
// dashboard shows, and the state the browser polls. An Engine (see engine.go)
// supplies everything that has to speak a database's own language.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// Param is one setting of a dial, described for the browser to draw. Steps is a
// slider over fixed values, Pills a row of buttons; Unit tells the browser how to
// print a value ("rows", "s", "ms", "MB/s", "%", "Mbit/s", "ops", "" for plain).
type Param struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"` // steps | pills
	Opts  []any  `json:"opts"`
	Def   any    `json:"def"`
	Unit  string `json:"unit,omitempty"`
}

// Dial is one stressor. Kind "toggle" stays on until turned off or its hold
// runs out; "run" is one action with a progress bar. Members says which members
// it acts on: "replicas", "all", or "" for none (it acts on the writer). A dial
// the target cannot support is still listed, with Available false and Why
// saying so, so the dashboard can explain the gap instead of hiding it.
type Dial struct {
	ID         string   `json:"id"`
	Group      string   `json:"group"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Desc       string   `json:"desc"`
	Watch      []string `json:"watch,omitempty"`
	Params     []Param  `json:"params"`
	Members    string   `json:"members,omitempty"`
	Single     bool     `json:"single,omitempty"`     // exactly one member at a time
	NeedsTable string   `json:"needsTable,omitempty"` // "", "any", "pk" or "nopk"
	SQL        string   `json:"sql,omitempty"`        // template; {key} is replaced by the param
	Note       string   `json:"note,omitempty"`
	Warn       string   `json:"warn,omitempty"`
	Available  bool     `json:"available"`
	Why        string   `json:"why,omitempty"`
}

// Params is what the browser sent for a dial, keyed by Param.Key.
type Params map[string]any

func (p Params) Int(k string, def int) int {
	switch v := p[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}

func (p Params) Str(k, def string) string {
	switch v := p[k].(type) {
	case string:
		if v != "" {
			return v
		}
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return def
}

// RunStatus is what a running dial reports each second. Progress is 0–1, or
// negative when there is no way to know. Done means it ended on its own (a run
// that finished, or a toggle whose work failed and cannot continue).
type RunStatus struct {
	Phase    string  `json:"phase,omitempty"`
	Progress float64 `json:"progress"`
	Note     string  `json:"note,omitempty"`
	Done     bool    `json:"done,omitempty"`
	Err      string  `json:"err,omitempty"`
}

// Running is a dial that has been started. Stop must release everything it
// holds and be safe to call more than once.
type Running interface {
	Stop(ctx context.Context) error
	Status() RunStatus
	// Holders are the connection ids, per member, that this dial keeps open on
	// purpose (a held lock, an open transaction), so the session view can mark
	// them as the blockers they are.
	Holders() map[string][]string
}

// Member is one database server of the linked target as last sampled.
type Member struct {
	ID       string      `json:"id"`
	Role     string      `json:"role"`
	Up       bool        `json:"up"`
	Lag      float64     `json:"lag"`   // seconds behind the writer, from the heartbeat; -1 unknown
	RecvQ    float64     `json:"recvq"` // Galera receive queue
	SendQ    float64     `json:"sendq"`
	Chip     string      `json:"chip"` // ok | warn | bad | neutral
	ChipText string      `json:"chipText"`
	Fields   [][2]string `json:"fields"`
	Evicted  bool        `json:"evicted,omitempty"`
	// Source is the member this one replicates from, when an engine knows it
	// and it is not simply the writer: a Valkey Cluster has one per shard.
	Source string `json:"source,omitempty"`
}

// ServerSample is the writer-side and cluster-wide numbers of one sample.
type ServerSample struct {
	LockWaits float64
	Threads   float64
	Conns     float64
	FC        float64 // share of the last second Galera flow control paused the cluster
	SendQ     float64
}

// Point is one second of the timeline.
type Point struct {
	T     int64              `json:"t"`
	Lag   map[string]float64 `json:"lag"`
	RecvQ map[string]float64 `json:"recvq,omitempty"`
	WQ    float64            `json:"wq"`
	RQ    float64            `json:"rq"`
	P50   float64            `json:"p50"`
	P99   float64            `json:"p99"`
	LockW float64            `json:"lockw"`
	Thr   float64            `json:"thr"`
	Conns float64            `json:"conns"`
	Errs  float64            `json:"errs"`
	FC    float64            `json:"fc"`
	SendQ float64            `json:"sendq"`
	Fsync float64            `json:"fsync"`
	RTT   float64            `json:"rtt"`
}

// Band is the time a dial was on, for the timeline's shading.
type Band struct {
	Seq   int    `json:"seq"`
	ID    string `json:"id"`
	Label string `json:"label"`
	Start int64  `json:"start"`
	End   *int64 `json:"end"`
	Color int    `json:"color"`
}

// Event is one line of the event log.
type Event struct {
	T     int64  `json:"t"`
	Level string `json:"level"` // info | warn | ok | neutral
	Text  string `json:"text"`
}

// Dataset is a table the dials can use.
type Dataset struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Rows  int64  `json:"rows"`
	PK    bool   `json:"pk"`
}

// Session is one row of the session view.
type Session struct {
	ID     string `json:"id"`
	Member string `json:"member"`
	User   string `json:"user"`
	State  string `json:"state"`
	Time   int64  `json:"time"`
	Waits  string `json:"waits,omitempty"`
	Query  string `json:"query"`
	Kind   string `json:"kind,omitempty"` // blocker | blocked | ""
}

// TargetInfo describes what the sim is linked to.
type TargetInfo struct {
	Name     string `json:"name"`
	Engine   string `json:"engine"` // mysql
	Flavor   string `json:"flavor"` // percona | mysql | mariadb
	Version  string `json:"version"`
	Topology string `json:"topology"` // standalone | async | gr | galera
	Writer   string `json:"writer"`
	LagTitle string `json:"lagTitle"`
	LagSub   string `json:"lagSub"`
	Status   string `json:"statusTab"`
}

// errf is fmt.Errorf; kept short because dials build many messages.
func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
