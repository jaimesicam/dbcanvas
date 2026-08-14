package main

// ftdcsummary.go — turning diagnostic.data into the eight or nine charts that answer
// something.
//
// A decoded FTDC file holds about four thousand distinct metrics per sample. Almost none of
// them are worth a chart, and a page that offered all four thousand would be a metric
// browser — which is a thing an engineer uses when they already know what they are looking
// for, and useless when they do not. The point of this page is the other case: something
// happened, here is the file, what was the server doing.
//
// So the charts are chosen, not enumerated, and each one is here because it answers a
// question people actually ask of a MongoDB server:
//
//	member state       who was primary, and when did that change
//	replication lag    was a secondary behind, and by how much   ← not in the log at all
//	oplog window       could a member that was away still catch up
//	tickets            was the storage engine the bottleneck
//	queue length       were operations waiting to get in
//	connections        did the driver pool run away
//	cache              was WiredTiger evicting under pressure
//	operations         what the server was actually asked to do
//	cpu                was any of this the machine rather than the database
//
// Every key below was read out of a real file from a live PSMDB 8.0.28-12 member rather
// than from documentation, which matters more here than it sounds: 8.0 moved the ticket
// counters from `wiredTiger.concurrentTransactions` to `queues.execution`, and a chart
// built on the documented-in-2019 name is a chart that is silently always empty.

import (
	"fmt"
	"sort"
	"strings"
)

// fdSeries is one line on a chart.
type fdSeries struct {
	Name   string    `json:"name"`
	Points []float64 `json:"points"`
	// Dashed marks a reference line — a configured maximum rather than a measurement.
	Dashed bool `json:"dashed,omitempty"`
}

// fdChart is one chart: what it shows and why anybody should look at it.
type fdChart struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Unit   string     `json:"unit"`
	Why    string     `json:"why"`
	Series []fdSeries `json:"series"`
	// Stack draws the series as a stacked area rather than lines, for parts of a whole.
	Stack bool `json:"stack,omitempty"`
	// Advice is the advisor for this chart: what this capture's numbers actually say.
	Advice *fdAdvice `json:"advice,omitempty"`
}

// fdAdvice is one chart's reading, in the same shape the Stalk Summary uses.
type fdAdvice struct {
	Level    string `json:"level"` // ok | warn | crit | info
	Headline string `json:"headline"`
	Detail   string `json:"detail,omitempty"`
	Action   string `json:"action,omitempty"`
}

// fdModel is what the page renders.
type fdModel struct {
	Host    string    `json:"host,omitempty"`
	Version string    `json:"version,omitempty"`
	ReplSet string    `json:"replSet,omitempty"`
	From    float64   `json:"from"`
	To      float64   `json:"to"`
	TS      []float64 `json:"ts"`
	Samples int       `json:"samples"`
	Chunks  int       `json:"chunks"`
	Metrics int       `json:"metrics"`
	Skipped int       `json:"skipped,omitempty"`
	Charts  []fdChart `json:"charts"`
	Notes   []string  `json:"notes,omitempty"`
}

// fdMaxPoints is how many points a chart carries. A day of one-second samples is 86,400,
// which no SVG line needs and no browser enjoys; downsampling to this keeps every chart
// legible and the payload sane.
const fdMaxPoints = 1200

// ftdcSummarise builds the model from decoded data.
func ftdcSummarise(d *ftdcData) *fdModel {
	from, to := d.window()
	m := &fdModel{
		Host: d.Meta["host"], Version: d.Meta["version"], ReplSet: d.Meta["replSet"],
		From: from, To: to, Samples: d.Samples, Chunks: d.Chunks,
		Metrics: len(d.Series), Skipped: d.Skipped,
		TS: fdDownsample(d.TS, fdMaxPoints),
	}
	if d.Skipped > 0 {
		m.Notes = append(m.Notes, fmt.Sprintf("%d chunk(s) would not decode and were skipped. A truncated metrics.interim is normal — it is the file mongod is writing right now.", d.Skipped))
	}
	for _, build := range []func(*ftdcData) *fdChart{
		fdChartMemberState,
		fdChartReplLag,
		fdChartOplog,
		fdChartTickets,
		fdChartQueues,
		fdChartConnections,
		fdChartCache,
		fdChartOps,
		fdChartCPU,
	} {
		if c := build(d); c != nil && len(c.Series) > 0 {
			for i := range c.Series {
				c.Series[i].Points = fdDownsample(c.Series[i].Points, fdMaxPoints)
			}
			m.Charts = append(m.Charts, *c)
		}
	}
	if len(m.Charts) == 0 {
		m.Notes = append(m.Notes, "None of the metrics this page charts were present. That usually means the file is from a mongos (which has no storage engine and no replica-set status) rather than a mongod.")
	}
	return m
}

// fdDownsample thins a series by taking every nth point. Deliberately not averaging: these
// are gauges as often as counters, and an averaged spike is a spike that did not happen.
// Taking every nth can miss a spike; averaging INVENTS a shape, which is worse in a
// diagnostic tool.
func fdDownsample(v []float64, max int) []float64 {
	if len(v) <= max || max <= 0 {
		return v
	}
	step := float64(len(v)) / float64(max)
	out := make([]float64, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, v[int(float64(i)*step)])
	}
	return out
}

// fdFloats lifts a metric to float64, or nil when it is absent.
func fdFloats(d *ftdcData, key string, scale float64) []float64 {
	s := d.Series[key]
	if s == nil {
		return nil
	}
	out := make([]float64, len(s.Values))
	for i, v := range s.Values {
		out[i] = float64(v) * scale
	}
	return out
}

// fdRate turns a monotonic counter into a per-second rate using the real sample spacing,
// which is not always one second — mongod slows FTDC down under load, and assuming a
// second there would overstate every rate exactly when it matters most.
func fdRate(d *ftdcData, key string) []float64 {
	s := d.Series[key]
	if s == nil || len(s.Values) < 2 {
		return nil
	}
	out := make([]float64, len(s.Values))
	for i := 1; i < len(s.Values); i++ {
		dt := d.TS[i] - d.TS[i-1]
		if dt <= 0 {
			continue
		}
		delta := s.Values[i] - s.Values[i-1]
		if delta < 0 {
			continue // a counter reset — the server restarted; a negative rate is a lie
		}
		out[i] = float64(delta) / dt
	}
	if len(out) > 1 {
		out[0] = out[1]
	}
	return out
}

func fdMax(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func fdMin(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v {
		if x < m {
			m = x
		}
	}
	return m
}

// fdMemberIdx lists the replica-set member indices present in the data.
//
// FTDC stores numbers only, so the members are `members.0`, `members.1`, `members.2` and
// nothing says which host each one is — the names are strings and strings are not metrics.
// That limitation is stated on the chart rather than papered over: an index is honest, an
// invented name is not.
func fdMemberIdx(d *ftdcData) []int {
	seen := map[int]bool{}
	for k := range d.Series {
		if !strings.HasPrefix(k, "replSetGetStatus.members.") {
			continue
		}
		rest := strings.TrimPrefix(k, "replSetGetStatus.members.")
		i := strings.Index(rest, ".")
		if i <= 0 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(rest[:i], "%d", &n); err == nil {
			seen[n] = true
		}
	}
	var out []int
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// fdSelfIdx finds which member index is the server the file came from.
func fdSelfIdx(d *ftdcData) int {
	for _, n := range fdMemberIdx(d) {
		if v, ok := d.last(fmt.Sprintf("replSetGetStatus.members.%d.self", n)); ok && v == 1 {
			return n
		}
	}
	return -1
}

// ---------------------------------------------------------------- the charts

// fdChartMemberState — who was primary, drawn as the state number each member reported.
//
// The numbers are rs.status()'s own: 1 PRIMARY, 2 SECONDARY, 3 RECOVERING, 5 STARTUP2,
// 8 DOWN, 9 ROLLBACK. Drawing them as a line is a slight abuse of a chart — the values are
// categories, not quantities — but it is the fastest way to see a failover in a day of
// data, which is what the chart is for.
func fdChartMemberState(d *ftdcData) *fdChart {
	c := &fdChart{ID: "memberState", Title: "Replica-set member state", Unit: "state",
		Why: "1 PRIMARY · 2 SECONDARY · 3 RECOVERING · 5 STARTUP2 · 8 DOWN · 9 ROLLBACK. A step from 1 to 2 on one line while another steps 2→1 is a failover. FTDC records the members by position and not by name — the names are strings, and FTDC stores only numbers — so these are the indices this member saw them in."}
	self := fdSelfIdx(d)
	for _, n := range fdMemberIdx(d) {
		key := fmt.Sprintf("replSetGetStatus.members.%d.state", n)
		if pts := fdFloats(d, key, 1); pts != nil {
			name := fmt.Sprintf("member %d", n)
			if n == self {
				name += " (this one)"
			}
			c.Series = append(c.Series, fdSeries{Name: name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	// A failover inside the window is the single most useful thing to say here.
	changes := 0
	for _, s := range c.Series {
		for i := 1; i < len(s.Points); i++ {
			if s.Points[i] != s.Points[i-1] {
				changes++
			}
		}
	}
	c.Advice = &fdAdvice{Level: "ok", Headline: "No member changed state in this window"}
	if changes > 0 {
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%d member state change(s) in this window", changes),
			Detail:   "Every state change is a moment when the set's answer to 'who takes writes' was different. If one of them is a 1→2 with another 2→1 close to it, that is a failover and the application saw write errors across it.",
			Action:   "Line this up against the same window in Log Summary: the log says WHY the state changed, which FTDC cannot."}
	}
	return c
}

// fdChartReplLag — the chart that exists because the log does not have it.
//
// Lag is the difference between the freshest optime any member reported and each member's
// own. Taking the maximum rather than "the primary's" is deliberate: during a failover
// there may be no primary at all, and a lag chart that goes blank exactly when the incident
// happens is the wrong chart.
func fdChartReplLag(d *ftdcData) *fdChart {
	idx := fdMemberIdx(d)
	if len(idx) < 2 {
		return nil
	}
	cols := map[int][]float64{}
	for _, n := range idx {
		if pts := fdFloats(d, fmt.Sprintf("replSetGetStatus.members.%d.optimeDate", n), 0.001); pts != nil {
			cols[n] = pts
		}
	}
	if len(cols) < 2 {
		return nil
	}
	n := len(d.TS)
	c := &fdChart{ID: "replLag", Title: "Replication lag", Unit: "s",
		Why: "How far behind the freshest member each member was, second by second. This is the number the server log never writes down — a member can be an hour behind through a whole window and leave no trace in mongod.log. Measured against the newest optime rather than against the primary's, so it keeps working across a failover when there is briefly no primary at all."}
	self := fdSelfIdx(d)
	worst := 0.0
	for _, m := range idx {
		col, ok := cols[m]
		if !ok {
			continue
		}
		lag := make([]float64, n)
		for i := 0; i < n && i < len(col); i++ {
			newest := 0.0
			for _, other := range cols {
				if i < len(other) && other[i] > newest {
					newest = other[i]
				}
			}
			if col[i] > 0 && newest > col[i] {
				lag[i] = newest - col[i]
			}
		}
		if mx := fdMax(lag); mx > worst {
			worst = mx
		}
		name := fmt.Sprintf("member %d", m)
		if m == self {
			name += " (this one)"
		}
		c.Series = append(c.Series, fdSeries{Name: name, Points: lag})
	}
	switch {
	case worst >= 60:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("A member was %s behind at its worst", lsDur(worst)),
			Detail:   "A secondary this far behind is not a usable read target and is not a usable failover target either — promoting it means losing everything it had not applied. It also eats the oplog window: if it falls further behind than the oplog reaches, it needs a full resync.",
			Action:   "Find out whether it is the member (slow disk, a long-running index build) or the workload (a bulk write the applier cannot keep up with). The queue and ticket charts on this page separate those two."}
	case worst >= 10:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Peak replication lag %s", lsDur(worst)),
			Detail:   "Enough to matter for reads from secondaries and for how much is at risk if the primary is lost, but not obviously a fault."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Members stayed within %s of each other", lsDur(worst))}
	}
	return c
}

// fdChartOplog — how much room the oplog has, which decides whether a member that goes away
// can come back cheaply.
func fdChartOplog(d *ftdcData) *fdChart {
	size := fdFloats(d, "local.oplog.rs.stats.storageStats.size", 1.0/(1024*1024))
	maxSize := fdFloats(d, "local.oplog.rs.stats.storageStats.maxSize", 1.0/(1024*1024))
	if size == nil {
		return nil
	}
	c := &fdChart{ID: "oplog", Title: "Oplog size", Unit: "MiB",
		Why: "The oplog is a capped collection: once it is full, the oldest entries go, and a member that has been away longer than the oplog reaches back cannot catch up incrementally at all — it needs a full resync. The solid line is what is in it, the dashed line is the cap."}
	c.Series = append(c.Series, fdSeries{Name: "oplog used", Points: size})
	if maxSize != nil {
		c.Series = append(c.Series, fdSeries{Name: "configured maximum", Points: maxSize, Dashed: true})
		if mx := fdMax(maxSize); mx > 0 {
			used := fdMax(size) / mx * 100
			c.Advice = &fdAdvice{Level: "ok",
				Headline: fmt.Sprintf("Oplog is %.0f%% of its %.0f MiB cap", used, mx)}
			if used > 99 {
				c.Advice = &fdAdvice{Level: "info",
					Headline: fmt.Sprintf("Oplog is full and rolling, at its %.0f MiB cap", mx),
					Detail:   "Full is the normal state for a healthy oplog — it fills once and then rolls. What matters is not the fullness but the WINDOW: how much wall-clock time those bytes cover, which shrinks as the write rate rises.",
					Action:   "rs.printReplicationInfo() reports the window directly. If it is shorter than the longest outage a member might have, an ordinary restart turns into a full resync — replSetResizeOplog changes the cap without a restart."}
			}
		}
	}
	return c
}

// fdChartTickets — the storage engine's concurrency limit, and how close to it the server ran.
//
// The keys moved in 8.0: `wiredTiger.concurrentTransactions` became `queues.execution`.
// Both are read here so the page works on either, which is the sort of thing that is
// invisible until somebody opens a file from an older server and every chart is empty.
func fdChartTickets(d *ftdcData) *fdChart {
	c := &fdChart{ID: "tickets", Title: "Execution tickets available", Unit: "tickets",
		Why: "WiredTiger admits only so many operations at once; the rest wait. Tickets falling to zero is the storage engine saying it is the bottleneck — everything above it queues, and latency rises with no single slow query to blame."}
	for _, p := range []struct{ name, k80, kOld string }{
		{"read", "serverStatus.queues.execution.read.available", "serverStatus.wiredTiger.concurrentTransactions.read.available"},
		{"write", "serverStatus.queues.execution.write.available", "serverStatus.wiredTiger.concurrentTransactions.write.available"},
	} {
		pts := fdFloats(d, p.k80, 1)
		if pts == nil {
			pts = fdFloats(d, p.kOld, 1)
		}
		if pts != nil {
			c.Series = append(c.Series, fdSeries{Name: p.name + " available", Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	low := 1e18
	for _, s := range c.Series {
		if m := fdMin(s.Points); m < low {
			low = m
		}
	}
	switch {
	case low == 0:
		c.Advice = &fdAdvice{Level: "crit", Headline: "Tickets ran out",
			Detail: "At least once in this window there were no tickets left, so operations waited purely to get into the storage engine. Everything is slow when this happens and nothing in the slow-query log explains why.",
			Action: "Either the workload is genuinely beyond the server, or something is holding tickets far longer than it should — a collection scan, a missing index, or a disk that has become slow. Check the queue chart below and the operation mix above."}
	case low <= 5:
		c.Advice = &fdAdvice{Level: "warn", Headline: fmt.Sprintf("Tickets fell to %.0f", low),
			Detail: "Close enough to exhaustion that a burst would have hit it."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Never fewer than %.0f tickets free", low)}
	}
	return c
}

// fdChartQueues — operations waiting, which is the symptom tickets running out produces.
func fdChartQueues(d *ftdcData) *fdChart {
	c := &fdChart{ID: "queues", Title: "Operations queued", Unit: "ops", Stack: true,
		Why: "How many operations were waiting rather than running. A queue that is occasionally non-zero is a busy server; a queue that never empties is a server that is behind and will not catch up on its own."}
	for _, p := range []struct{ name, k80, kOld string }{
		{"readers", "serverStatus.queues.execution.read.normalPriority.queueLength", "serverStatus.globalLock.currentQueue.readers"},
		{"writers", "serverStatus.queues.execution.write.normalPriority.queueLength", "serverStatus.globalLock.currentQueue.writers"},
	} {
		pts := fdFloats(d, p.k80, 1)
		if pts == nil {
			pts = fdFloats(d, p.kOld, 1)
		}
		if pts != nil {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	worst := 0.0
	for _, s := range c.Series {
		if m := fdMax(s.Points); m > worst {
			worst = m
		}
	}
	c.Advice = &fdAdvice{Level: "ok", Headline: "Nothing queued in this window"}
	if worst > 0 {
		lvl := "warn"
		if worst >= 50 {
			lvl = "crit"
		}
		c.Advice = &fdAdvice{Level: lvl, Headline: fmt.Sprintf("Up to %.0f operations queued at once", worst),
			Action: "Read this together with the ticket chart: queued operations with tickets at zero is the storage engine saturating. Queued operations with tickets to spare is a lock — usually a long write holding a collection."}
	}
	return c
}

// fdChartConnections — the pool, which is the most common cause of a "MongoDB is down" that
// is nothing of the kind.
func fdChartConnections(d *ftdcData) *fdChart {
	cur := fdFloats(d, "serverStatus.connections.current", 1)
	if cur == nil {
		return nil
	}
	c := &fdChart{ID: "connections", Title: "Connections", Unit: "conns",
		Why: "Client connections held open. Drivers pool, so this should be roughly flat at the sum of the pools; a line that climbs and never falls is a pool leak, and hitting the limit makes the server refuse new clients while looking perfectly healthy to the ones it has."}
	c.Series = append(c.Series, fdSeries{Name: "current", Points: cur})
	if av := fdFloats(d, "serverStatus.connections.available", 1); av != nil {
		total := make([]float64, len(cur))
		for i := range cur {
			if i < len(av) {
				total[i] = cur[i] + av[i]
			}
		}
		c.Series = append(c.Series, fdSeries{Name: "limit", Points: total, Dashed: true})
		if mx := fdMax(total); mx > 0 {
			pct := fdMax(cur) / mx * 100
			c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Peak %.0f of %.0f connections (%.0f%%)", fdMax(cur), mx, pct)}
			if pct > 80 {
				c.Advice.Level = "crit"
				c.Advice.Detail = "Close enough to the limit that new clients were at risk of being refused."
				c.Advice.Action = "Check the driver pool sizes across every application instance — maxPoolSize multiplied by instance count is the number that matters, and it is usually larger than anybody expects."
			}
		}
	}
	return c
}

// fdChartCache — WiredTiger's cache, where a MongoDB performance problem usually lives.
func fdChartCache(d *ftdcData) *fdChart {
	inCache := fdFloats(d, "serverStatus.wiredTiger.cache.bytes currently in the cache", 1.0/(1024*1024))
	if inCache == nil {
		return nil
	}
	c := &fdChart{ID: "cache", Title: "WiredTiger cache", Unit: "MiB",
		Why: "How much of the cache is in use and how much of that is dirty. Dirty pages above roughly 20% of the cache means eviction is struggling to keep up with writes, and application threads get pulled in to help — which shows up as everything becoming slow at once."}
	c.Series = append(c.Series, fdSeries{Name: "in cache", Points: inCache})
	if dirty := fdFloats(d, "serverStatus.wiredTiger.cache.tracked dirty bytes in the cache", 1.0/(1024*1024)); dirty != nil {
		c.Series = append(c.Series, fdSeries{Name: "dirty", Points: dirty})
	}
	if maxB := fdFloats(d, "serverStatus.wiredTiger.cache.maximum bytes configured", 1.0/(1024*1024)); maxB != nil {
		c.Series = append(c.Series, fdSeries{Name: "configured", Points: maxB, Dashed: true})
		if mx := fdMax(maxB); mx > 0 {
			fill := fdMax(inCache) / mx * 100
			c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Cache peaked at %.0f%% of its %.0f MiB", fill, mx)}
			if len(c.Series) > 2 {
				if dirtyPct := fdMax(c.Series[1].Points) / mx * 100; dirtyPct > 20 {
					c.Advice = &fdAdvice{Level: "crit",
						Headline: fmt.Sprintf("Dirty pages reached %.0f%% of the cache", dirtyPct),
						Detail:   "Past about 20% dirty, WiredTiger starts making application threads do eviction work themselves. The symptom is that everything slows down together, with no individual operation looking guilty.",
						Action:   "Either the write rate is beyond what the disk can absorb, or the cache is too small for the working set. The CPU chart's iowait and the queue chart together say which."}
				}
			}
		}
	}
	return c
}

// fdChartOps — what the server was actually asked to do.
func fdChartOps(d *ftdcData) *fdChart {
	c := &fdChart{ID: "ops", Title: "Operations", Unit: "ops/s", Stack: true,
		Why: "The operation mix, as rates. Worth reading first: a chart of a server doing nothing looks exactly like a chart of a server that is broken, and this is the one that tells them apart."}
	for _, k := range []string{"insert", "query", "update", "delete", "getmore", "command"} {
		if pts := fdRate(d, "serverStatus.opcounters."+k); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: k, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	total := 0.0
	for _, s := range c.Series {
		total += fdMax(s.Points)
	}
	c.Advice = &fdAdvice{Level: "info", Headline: fmt.Sprintf("Peak roughly %.0f operations/s across all types", total)}
	return c
}

// fdChartCPU — whether any of this was the machine rather than the database.
func fdChartCPU(d *ftdcData) *fdChart {
	c := &fdChart{ID: "cpu", Title: "CPU", Unit: "%", Stack: true,
		Why: "Host CPU, from /proc. iowait is the one to look at first on a database: high iowait with low user time means the disk is the problem and nothing about the query layer will fix it."}
	cores := 1.0
	if v, ok := d.last("systemMetrics.cpu.num_cores_available_to_process"); ok && v > 0 {
		cores = float64(v)
	}
	for _, p := range []struct{ name, key string }{
		{"user", "systemMetrics.cpu.user_ms"},
		{"system", "systemMetrics.cpu.system_ms"},
		{"iowait", "systemMetrics.cpu.iowait_ms"},
	} {
		if pts := fdRate(d, p.key); pts != nil {
			// _ms counters tick in milliseconds of CPU time per second of wall clock,
			// summed over every core, so dividing by 10 gives a percentage of one core
			// and dividing again by the core count gives a percentage of the machine.
			for i := range pts {
				pts[i] = pts[i] / 10 / cores
			}
			if fdMax(pts) > 0 {
				c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
			}
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	for _, s := range c.Series {
		if s.Name == "iowait" && fdMax(s.Points) > 20 {
			c.Advice = &fdAdvice{Level: "warn",
				Headline: fmt.Sprintf("iowait peaked at %.0f%% of the machine", fdMax(s.Points)),
				Detail:   "The CPU spent that proportion of its time waiting for the disk. On a database that is usually the whole story, and no amount of query tuning moves it.",
				Action:   "Check the disk's own latency, and whether the working set has outgrown the WiredTiger cache — a cache too small for the data turns every read into a disk read."}
		}
	}
	if c.Advice == nil {
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Peak CPU %.0f%% of %.0f core(s)", fdMax(c.Series[0].Points), cores)}
	}
	return c
}
