package main

// ftdcsummary.go — turning diagnostic.data into the twenty-odd charts that answer something.
//
// A decoded FTDC file holds thousands of distinct metrics per sample — 5,673 on the live 8.0
// member this was built against. Almost none of them are worth a chart, and a page that
// offered all of them would be a metric browser, which is a thing an engineer uses when they
// already know what they are looking for and useless when they do not. The point of this page
// is the other case: something happened, here is the file, what was the server doing.
//
// So the charts are chosen, not enumerated, and grouped — Replication, Work, Storage engine,
// Host — because twenty charts in one flat column is a column nobody reads to the bottom of.
// Each is here because it answers a question people actually ask of a MongoDB server, and
// the ones that matter most are the ones a first pass at this page missed entirely:
// operation latency, the oplog APPLY rate that separates "cannot keep up" from "receiving
// nothing", Linux PSI pressure, and per-device disk service time.
//
// Several of the best are not metrics at all but ratios of two cumulative counters — see
// fdRatio. opLatencies stores total microseconds and a total operation count, and neither is
// a latency; charting either draws a line going up and to the right for ever.
//
// Every key below was read out of a real file rather than from documentation, which matters
// more here than it sounds: 8.0 moved the ticket counters from
// `wiredTiger.concurrentTransactions` to `queues.execution`, and a chart built on the
// documented-in-2019 name is a chart that is silently always empty.

import (
	"fmt"
	"sort"
	"strconv"
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
	ID    string `json:"id"`
	Title string `json:"title"`
	// Group is the section heading a chart sits under. Nineteen charts in one flat list
	// is a list nobody reads to the bottom of; four labelled groups is a page somebody
	// can skip through to the part they care about.
	Group  string     `json:"group"`
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
	// Role is what kind of process wrote this — a mongos, a config server, a shard member.
	// Stated because "twelve charts" means something completely different depending on it.
	Role    string    `json:"role,omitempty"`
	Skipped int       `json:"skipped,omitempty"`
	Charts  []fdChart `json:"charts"`
	Notes   []string  `json:"notes,omitempty"`
	// Server is the type-0 metadata document, read back as a list of facts. It is not a
	// chart and it is not decoration: it is what the server WAS, and a chart read without
	// it is a chart read without knowing how many cores, how much memory, which cache
	// size or which file-descriptor ceiling produced it.
	Server []fdFact `json:"server,omitempty"`
}

// fdFact is one line of the capture header.
type fdFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
	// Note is the sentence that makes the value mean something, and is set only for the
	// handful of facts that need one.
	Note string `json:"note,omitempty"`
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
		Metrics: len(d.Series), Skipped: d.Skipped, Role: fdRole(d),
		TS: fdDownsample(d.TS, fdMaxPoints),
	}
	if d.Skipped > 0 {
		m.Notes = append(m.Notes, fmt.Sprintf("%d chunk(s) would not decode and were skipped. A truncated metrics.interim is normal — it is the file mongod is writing right now.", d.Skipped))
	}
	m.Notes = append(m.Notes, fdGapNote(d)...)
	m.Server = fdServerFacts(d)
	// Say when the lines are thinned. The advisor under each chart reads every sample; the
	// chart draws fdMaxPoints of them, so an advisor can legitimately name a peak that
	// falls between two drawn points. Silently is the wrong way for a diagnostic page to
	// do that.
	if len(d.TS) > fdMaxPoints {
		m.Notes = append(m.Notes, fmt.Sprintf(
			"Charts draw %d of this capture's %d samples (every %dth). The advice under each chart was computed from all of them, so a peak it names can fall between two drawn points — zoom into a window to see it.",
			fdMaxPoints, len(d.TS), len(d.TS)/fdMaxPoints))
	}
	// Ordered by group, and within a group by how often it is the answer.
	builders := []func(*ftdcData) *fdChart{
		// Replication — the questions only a replica set raises.
		fdChartMemberState,
		fdChartQuorum,
		fdChartReplLag,
		fdChartCommitLag,
		fdChartWriteConcern,
		fdChartOplogApply,
		fdChartOplog,
		fdChartOplogWindow,
		fdChartCatchUp,
		fdChartFlowControl,
		fdChartReplNetwork,
		fdChartSyncSource,
		fdChartElections,
		// Work — what the server was asked to do and how well it did it.
		fdChartLatency,
		fdChartWaiting,
		fdChartOps,
		fdChartCommandMix,
		fdChartReadPreference,
		fdChartSessionStore,
		fdChartIndexEfficiency,
		fdChartContention,
		fdChartErrors,
		fdChartConnections,
		fdChartNetwork,
		// Storage engine — where a MongoDB performance problem usually lives.
		fdChartTickets,
		fdChartQueues,
		fdChartAdmission,
		fdChartCache,
		fdChartCachePressure,
		fdChartEviction,
		fdChartJournal,
		fdChartEngineIO,
		fdChartCheckpoint,
		fdChartHistoryStore,
		fdChartOplogCache,
		fdChartIndexBuild,
		fdChartDataHandles,
		fdChartHeap,
		fdChartMemory,
		// Sharding — only on a sharded cluster, and different on each of its three roles.
		fdChartTargeting,
		fdChartShardPing,
		fdChartCatalogCache,
		fdChartMigrations,
		fdChartCriticalSection,
		fdChartRangeDeleter,
		fdChartRouterPool,
		fdChartRouterLatency,
		fdChartRouterHosts,
		// Host — whether any of it was the machine rather than the database.
		fdChartCPU,
		fdChartPressure,
		fdChartProcessPressure,
		fdChartTCP,
		fdChartHostMemory,
		fdChartFaults,
		fdChartDiskSpace,
	}
	builders = append(builders, fdDiskCharts(d)...)
	for _, build := range builders {
		if c := build(d); c != nil && len(c.Series) > 0 {
			for i := range c.Series {
				c.Series[i].Points = fdDownsample(c.Series[i].Points, fdMaxPoints)
			}
			m.Charts = append(m.Charts, *c)
		}
	}
	if len(m.Charts) == 0 {
		m.Notes = append(m.Notes, "None of the metrics this page charts were present. Either this is not a MongoDB capture, or it is from a build whose metric names have moved further than the fallbacks here reach.")
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
	c := &fdChart{ID: "memberState", Group: "Replication", Title: "Replica-set member state", Unit: "state",
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
	c := &fdChart{ID: "replLag", Group: "Replication", Title: "Replication lag", Unit: "s",
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
	// 7.0 moved FTDC's collStats sample under a storageStats sub-document; on 6.0 the
	// fields sit directly on stats.
	size := fdFloats(d, fdAnyKey(d,
		"local.oplog.rs.stats.storageStats.size", "local.oplog.rs.stats.size"), 1.0/(1024*1024))
	maxSize := fdFloats(d, fdAnyKey(d,
		"local.oplog.rs.stats.storageStats.maxSize", "local.oplog.rs.stats.maxSize"), 1.0/(1024*1024))
	if size == nil {
		return nil
	}
	c := &fdChart{ID: "oplog", Group: "Replication", Title: "Oplog size", Unit: "MiB",
		Why: "The oplog is a capped collection: once it is full, the oldest entries go, and a member that has been away longer than the oplog reaches back cannot catch up incrementally at all — it needs a full resync. The solid line is what is in it, the dashed line is the cap."}
	c.Series = append(c.Series, fdSeries{Name: "oplog used", Points: size})
	if maxSize != nil {
		c.Series = append(c.Series, fdSeries{Name: "configured maximum", Points: maxSize, Dashed: true})
		if mx := fdMax(maxSize); mx > 0 {
			c.Advice = &fdAdvice{Level: "ok",
				Headline: fmt.Sprintf("Oplog holds %s MiB, %s of its %.0f MiB cap", fdAmt(fdMax(size)), fdPct(fdMax(size), mx), mx)}
			if fdMax(size)/mx*100 > 99 {
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
	c := &fdChart{ID: "tickets", Group: "Storage engine", Title: "Execution tickets available", Unit: "tickets",
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
	c := &fdChart{ID: "queues", Group: "Storage engine", Title: "Operations queued", Unit: "ops", Stack: true,
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
	c := &fdChart{ID: "connections", Group: "Work", Title: "Connections", Unit: "conns",
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
			c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Peak %.0f of %.0f connections (%s)", fdMax(cur), mx, fdPct(fdMax(cur), mx))}
			if fdMax(cur)/mx*100 > 80 {
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
	c := &fdChart{ID: "cache", Group: "Storage engine", Title: "WiredTiger cache", Unit: "MiB",
		Why: "How much of the cache is in use and how much of that is dirty. Dirty pages above roughly 20% of the cache means eviction is struggling to keep up with writes, and application threads get pulled in to help — which shows up as everything becoming slow at once."}
	c.Series = append(c.Series, fdSeries{Name: "in cache", Points: inCache})
	if dirty := fdFloats(d, "serverStatus.wiredTiger.cache.tracked dirty bytes in the cache", 1.0/(1024*1024)); dirty != nil {
		c.Series = append(c.Series, fdSeries{Name: "dirty", Points: dirty})
	}
	if maxB := fdFloats(d, "serverStatus.wiredTiger.cache.maximum bytes configured", 1.0/(1024*1024)); maxB != nil {
		c.Series = append(c.Series, fdSeries{Name: "configured", Points: maxB, Dashed: true})
		if mx := fdMax(maxB); mx > 0 {
			c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Cache peaked at %s MiB, %s of its %.0f MiB", fdAmt(fdMax(inCache)), fdPct(fdMax(inCache), mx), mx)}
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
	c := &fdChart{ID: "ops", Group: "Work", Title: "Operations", Unit: "ops/s", Stack: true,
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
	c := &fdChart{ID: "cpu", Group: "Host", Title: "CPU", Unit: "%", Stack: true,
		Why: "Host CPU, from /proc. iowait is the one to look at first on a database: high iowait with low user time means the disk is the problem and nothing about the query layer will fix it."}
	// 6.0 calls this `num_cpus`; 7.0 renamed it and added a cgroup-aware variant. Getting
	// this wrong is not a missing chart but a wrong one — the divisor falls back to 1 and a
	// twenty-core box reads "Peak CPU 1400% of 1 core".
	cores := 1.0
	if k := fdAnyKey(d,
		"systemMetrics.cpu.num_cores_available_to_process",
		"systemMetrics.cpu.num_logical_cores",
		"systemMetrics.cpu.num_cpus"); k != "" {
		if v, ok := d.last(k); ok && v > 0 {
			cores = float64(v)
		}
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

// ---------------------------------------------------------------- derived helpers

// fdRatio turns a pair of cumulative counters into a per-interval average — Δnumerator over
// Δdenominator.
//
// This is how the most useful numbers in FTDC are stored, and it is why a naive reading of
// them is wrong. `opLatencies.reads.latency` is total microseconds ever spent on reads and
// `.ops` is the number of reads ever done; neither is a latency. Their DELTAS divided give
// the average latency of the reads that happened in that interval, which is what somebody
// wants when they ask how slow the server was at 04:12.
//
// Intervals with no operations produce no number rather than a zero: a zero would draw a
// line to the floor and read as "instant", when the truth is that nothing was measured.
func fdRatio(d *ftdcData, numKey, denKey string, scale float64) []float64 {
	num, den := d.Series[numKey], d.Series[denKey]
	if num == nil || den == nil || len(num.Values) < 2 {
		return nil
	}
	out := make([]float64, len(num.Values))
	last := 0.0
	for i := 1; i < len(num.Values) && i < len(den.Values); i++ {
		dn := num.Values[i] - num.Values[i-1]
		dd := den.Values[i] - den.Values[i-1]
		if dd > 0 && dn >= 0 {
			last = float64(dn) / float64(dd) * scale
		}
		// Carry the previous value through idle intervals rather than dropping to zero.
		out[i] = last
	}
	if len(out) > 1 {
		out[0] = out[1]
	}
	return out
}

// fdAmt renders a magnitude without rounding a real quantity down to "0". An advisor
// reporting "the history store peaked at 0.0 MiB" reads as a broken chart; "under 0.1 MiB"
// reads as an idle server, which is what it is.
func fdAmt(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	case v >= 0.1:
		return fmt.Sprintf("%.2f", v)
	case v > 0:
		return "under 0.1"
	default:
		return "0"
	}
}

// fdPctV is fdPct for a value that is already a percentage.
func fdPctV(p float64) string { return fdPct(p, 100) }

// fdAnyKey returns the first of these metrics that exists, which is how a chart survives a
// key being renamed between server versions.
func fdAnyKey(d *ftdcData, keys ...string) string {
	for _, k := range keys {
		if d.Series[k] != nil {
			return k
		}
	}
	return ""
}

// fdPct renders a percentage that never reads as "0%" when it is merely small. An advisor
// saying "the cache peaked at 0% of its 14527 MiB" looks like a broken chart rather than an
// idle server, and the number that matters to the reader is usually the absolute one anyway.
func fdPct(part, whole float64) string {
	if whole <= 0 {
		return "0%"
	}
	p := part / whole * 100
	switch {
	case p >= 10:
		return fmt.Sprintf("%.0f%%", p)
	case p >= 1:
		return fmt.Sprintf("%.1f%%", p)
	case p > 0:
		return "under 1%"
	}
	return "0%"
}

// ---------------------------------------------------------------- work

// fdChartLatency — how long operations actually took.
//
// The headline performance number, and the one the first version of this page missed
// entirely. opLatencies holds cumulative microseconds and cumulative operation counts, so
// neither series is a latency on its own; the ratio of their deltas is.
func fdChartLatency(d *ftdcData) *fdChart {
	c := &fdChart{ID: "latency", Group: "Work", Title: "Average operation latency", Unit: "ms",
		Why: "How long the average operation took, per interval, split by kind. This is the number an application experiences. It is derived: FTDC stores total microseconds and a total count, and the average is the ratio of their deltas — so an idle interval carries the previous value forward rather than dropping to zero, which would read as 'instant' when nothing was measured at all."}
	for _, p := range []struct{ name, kind string }{
		{"reads", "reads"}, {"writes", "writes"}, {"commands", "commands"},
	} {
		pts := fdRatio(d, "serverStatus.opLatencies."+p.kind+".latency",
			"serverStatus.opLatencies."+p.kind+".ops", 0.001)
		if pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	worst, which := 0.0, ""
	for _, s := range c.Series {
		if m := fdMax(s.Points); m > worst {
			worst, which = m, s.Name
		}
	}
	switch {
	case worst >= 100:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%s averaged %.0f ms at their worst", which, worst),
			Detail:   "An average — not a slow outlier. Half the operations in that interval were slower still.",
			Action:   "Work down the page: tickets and queues say whether the storage engine was saturated, the index-efficiency chart says whether the work was avoidable, and the disk charts say whether it was the hardware."}
	case worst >= 10:
		c.Advice = &fdAdvice{Level: "warn", Headline: fmt.Sprintf("%s peaked at %.0f ms average", which, worst)}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Latency stayed under %.1f ms on average", worst)}
	}
	return c
}

// fdChartIndexEfficiency — how much work each returned document cost.
//
// The classic MongoDB diagnosis, and it needs two counters held together: documents examined
// against documents returned. A ratio near 1 means the indexes are doing their job; a ratio
// in the thousands means a query is reading the collection to find a handful of rows.
func fdChartIndexEfficiency(d *ftdcData) *fdChart {
	scanned := fdAnyKey(d, "serverStatus.metrics.queryExecutor.scannedObjects", "serverStatus.metrics.queryExecutor.scanned")
	if scanned == "" || d.Series["serverStatus.metrics.document.returned"] == nil {
		return nil
	}
	c := &fdChart{ID: "indexEfficiency", Group: "Work", Title: "Documents examined per document returned", Unit: "ratio",
		Why: "Documents read to produce one document of answer. 1 is a perfectly indexed query. Hundreds or thousands means the server is scanning to find a few rows, and no amount of hardware fixes that — it is an index that does not exist."}
	if pts := fdRatio(d, scanned, "serverStatus.metrics.document.returned", 1); pts != nil {
		c.Series = append(c.Series, fdSeries{Name: "examined : returned", Points: pts})
	}
	if pts := fdRate(d, "serverStatus.metrics.queryExecutor.collectionScans.total"); pts != nil && fdMax(pts) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "collection scans/s", Points: pts})
	}
	if len(c.Series) == 0 {
		return nil
	}
	worst := fdMax(c.Series[0].Points)
	switch {
	case worst >= 1000:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Up to %.0f documents examined per document returned", worst),
			Detail:   "The server read that many documents to answer with one. This is the single most common cause of a MongoDB server that looks overloaded and is really just under-indexed.",
			Action:   "Find the queries: db.setProfilingLevel or the slow-query log with planSummary: COLLSCAN. Every one of those is a missing index."}
	case worst >= 100:
		c.Advice = &fdAdvice{Level: "warn", Headline: fmt.Sprintf("Up to %.0f documents examined per one returned", worst)}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Peak %.0f documents examined per one returned", worst)}
	}
	return c
}

// fdChartNetwork — bytes on and off the wire.
func fdChartNetwork(d *ftdcData) *fdChart {
	c := &fdChart{ID: "network", Group: "Work", Title: "Client network throughput", Unit: "MiB/s",
		Why: "Bytes to and from clients. Worth a glance when latency is high and the server looks idle: a query returning far more data than anybody needs shows up here and nowhere else."}
	for _, p := range []struct{ name, key string }{
		{"in", "serverStatus.network.bytesIn"}, {"out", "serverStatus.network.bytesOut"},
	} {
		if pts := fdRate(d, p.key); pts != nil && fdMax(pts) > 0 {
			for i := range pts {
				pts[i] /= 1024 * 1024
			}
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	c.Advice = &fdAdvice{Level: "info", Headline: fmt.Sprintf("Peak %.1f MiB/s out to clients", fdMax(c.Series[len(c.Series)-1].Points))}
	return c
}

// ---------------------------------------------------------------- replication

// fdChartOplogApply — the secondary's side of replication lag.
//
// The lag chart says a member was behind; this says whether it was behind because it could
// not apply fast enough, or because nothing was reaching it. Those have opposite fixes and
// the two charts together separate them, which neither does alone.
func fdChartOplogApply(d *ftdcData) *fdChart {
	c := &fdChart{ID: "oplogApply", Group: "Replication", Title: "Oplog application", Unit: "ops/s · ms",
		Why: "How fast this member applied the oplog, and how long each batch took. Read with the lag chart: lag with a high apply rate is a member that cannot keep up with the write volume, and lag with a near-zero apply rate is a member that is not receiving anything — a network problem, or a sync source that has gone away."}
	if pts := fdRate(d, "serverStatus.metrics.repl.apply.ops"); pts != nil && fdMax(pts) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "ops applied/s", Points: pts})
	}
	if pts := fdRatio(d, "serverStatus.metrics.repl.apply.batches.totalMillis",
		"serverStatus.metrics.repl.apply.batches.num", 1); pts != nil && fdMax(pts) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "ms per batch", Points: pts})
	}
	if len(c.Series) == 0 {
		return nil
	}
	c.Advice = &fdAdvice{Level: "info", Headline: fmt.Sprintf("Peak %.0f oplog operations applied per second", fdMax(c.Series[0].Points))}
	return c
}

// fdChartReplNetwork — what the member pulled from its sync source.
func fdChartReplNetwork(d *ftdcData) *fdChart {
	pts := fdRate(d, "serverStatus.metrics.repl.network.bytes")
	if pts == nil || fdMax(pts) == 0 {
		return nil
	}
	for i := range pts {
		pts[i] /= 1024 * 1024
	}
	c := &fdChart{ID: "replNetwork", Group: "Replication", Title: "Oplog fetched from the sync source", Unit: "MiB/s",
		Why: "Bytes this member pulled from whichever member it replicates from. A secondary with lag and a flat zero here is not receiving the oplog at all, which is a very different problem from one that is receiving it and cannot apply it fast enough."}
	c.Series = append(c.Series, fdSeries{Name: "oplog fetched", Points: pts})
	c.Advice = &fdAdvice{Level: "info", Headline: fmt.Sprintf("Peak %.2f MiB/s fetched", fdMax(pts))}
	return c
}

// fdChartElections — how often the set went looking for a primary, and why.
func fdChartElections(d *ftdcData) *fdChart {
	c := &fdChart{ID: "elections", Group: "Replication", Title: "Elections called", Unit: "count",
		Why: "Cumulative election counters. electionTimeout is the one that matters: it counts elections called because the primary stopped answering, which is an unplanned failover rather than a maintenance handover."}
	for _, p := range []struct{ name, key string }{
		{"called on timeout", "serverStatus.electionMetrics.electionTimeout.called"},
		{"succeeded on timeout", "serverStatus.electionMetrics.electionTimeout.successful"},
		{"step-up requests", "serverStatus.electionMetrics.stepUpCmd.called"},
	} {
		if pts := fdFloats(d, p.key, 1); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	called := fdMax(c.Series[0].Points)
	lvl := "ok"
	if called > 0 {
		lvl = "warn"
	}
	c.Advice = &fdAdvice{Level: lvl,
		Headline: fmt.Sprintf("%.0f election(s) called because the primary stopped answering", called),
		Action:   "Each one is a window when the set took no writes. Line them up against the member-state chart above and the same period in Log Summary, which says what happened to the primary."}
	// A step-down kills the operations that were running on the old primary. That is the
	// half of a failover an application actually notices, and it is a separate counter.
	// 8.0 renamed userOperationsKilled to totalOperationsKilled.
	if k := fdFloats(d, fdAnyKey(d,
		"serverStatus.metrics.repl.stateTransition.totalOperationsKilled",
		"serverStatus.metrics.repl.stateTransition.userOperationsKilled"), 1); k != nil {
		if n := fdMax(k) - fdMin(k); n > 0 {
			c.Advice.Detail = fmt.Sprintf("%.0f in-progress operation(s) were killed by a state transition. That is what the application saw: not a slow request, but a connection whose work was terminated mid-flight.", n)
		}
	}
	return c
}

// ---------------------------------------------------------------- storage engine

// fdChartCheckpoint — how long WiredTiger's checkpoints took.
func fdChartCheckpoint(d *ftdcData) *fdChart {
	// WiredTiger grew a top-level `checkpoint` statistics category in WT 11.2 (WT-11171,
	// MongoDB 7.1). Before that the same numbers lived under `transaction`, prefixed
	// "transaction checkpoint" — so 6.0 and 7.0 need the old names or this chart is
	// silently empty on both.
	maxK := fdAnyKey(d,
		"serverStatus.wiredTiger.checkpoint.max time (msecs)",
		"serverStatus.wiredTiger.transaction.transaction checkpoint max time (msecs)")
	if maxK == "" {
		return nil
	}
	c := &fdChart{ID: "checkpoint", Group: "Storage engine", Title: "Checkpoint duration", Unit: "ms",
		Why: "WiredTiger writes a checkpoint every 60 seconds by default. A checkpoint that takes longer than the interval between checkpoints means the storage engine never gets to rest, and the symptom is a server that stalls periodically for no reason visible in any query."}
	if pts := fdFloats(d, maxK, 1); pts != nil {
		c.Series = append(c.Series, fdSeries{Name: "longest so far", Points: pts})
	}
	if k := fdAnyKey(d,
		"serverStatus.wiredTiger.checkpoint.min time (msecs)",
		"serverStatus.wiredTiger.transaction.transaction checkpoint min time (msecs)"); k != "" {
		if pts := fdFloats(d, k, 1); pts != nil {
			c.Series = append(c.Series, fdSeries{Name: "shortest so far", Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	worst := fdMax(c.Series[0].Points)
	lvl := "ok"
	if worst > 60000 {
		lvl = "crit"
	} else if worst > 20000 {
		lvl = "warn"
	}
	c.Advice = &fdAdvice{Level: lvl, Headline: fmt.Sprintf("Longest checkpoint %.1f s", worst/1000)}
	if lvl != "ok" {
		c.Advice.Detail = "Checkpoints run every 60 seconds. One taking a large fraction of that leaves the disk no idle time, and the server stalls in a way no slow-query log explains."
		c.Advice.Action = "Usually the disk rather than the database: check the per-device latency charts below. A cache far larger than the disk can flush produces the same shape."
	}
	return c
}

// fdChartMemory — where the server's memory went.
func fdChartMemory(d *ftdcData) *fdChart {
	c := &fdChart{ID: "memory", Group: "Storage engine", Title: "Memory", Unit: "MiB",
		Why: "Resident set against the WiredTiger cache and the allocator's heap. Resident far above the cache size is the rest of mongod — connections, aggregation buffers, the allocator holding freed memory — and it is what gets a server killed by the OOM killer on a box sized only for the cache."}
	for _, p := range []struct {
		name  string
		key   string
		scale float64
	}{
		{"resident", "serverStatus.mem.resident", 1},
		{"WT cache in use", "serverStatus.wiredTiger.cache.bytes currently in the cache", 1.0 / (1024 * 1024)},
		{"allocator heap", "serverStatus.tcmalloc.generic.heap_size", 1.0 / (1024 * 1024)},
	} {
		if pts := fdFloats(d, p.key, p.scale); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	c.Advice = &fdAdvice{Level: "info", Headline: fmt.Sprintf("Peak resident %.0f MiB", fdMax(c.Series[0].Points))}
	return c
}

// ---------------------------------------------------------------- host

// fdChartPressure — Linux pressure stall information, which is the best single answer to
// "was this the machine".
//
// PSI measures the time tasks spent STALLED waiting for a resource, which is a different and
// far more useful question than how busy the resource was. A disk can be 100% utilised and
// nothing waiting; a disk at 40% with everything queued behind it is the problem. `some` is
// the share of time at least one task was stalled, `full` the share when everything was.
func fdChartPressure(d *ftdcData) *fdChart {
	c := &fdChart{ID: "pressure", Group: "Host", Title: "Resource pressure (PSI)", Unit: "% of time stalled",
		Why: "The share of time work was stalled waiting for a resource, straight from the kernel. This answers 'was this the machine' better than utilisation does — a disk can be fully utilised with nothing waiting on it, and busy is not the same as blocked. io.some rising with database latency is as close to proof as this page gets."}
	// PSI lives under systemMetrics on every version that has it (6.0.8 and 7.0 onwards,
	// SERVER-45255). 8.0 additionally copies it into serverStatus.extra_info, byte for
	// byte identical — so systemMetrics is the one to read, and reading only extra_info
	// left this chart silently empty on 6.0 and 7.0.
	for _, p := range []struct{ name, key, alt string }{
		{"cpu (some)", "systemMetrics.pressure.cpu.some.totalMicros", "serverStatus.extra_info.pressure.cpu.some.totalMicros"},
		{"io (some)", "systemMetrics.pressure.io.some.totalMicros", "serverStatus.extra_info.pressure.io.some.totalMicros"},
		{"io (full)", "systemMetrics.pressure.io.full.totalMicros", "serverStatus.extra_info.pressure.io.full.totalMicros"},
		{"memory (some)", "systemMetrics.pressure.memory.some.totalMicros", "serverStatus.extra_info.pressure.memory.some.totalMicros"},
	} {
		if pts := fdRate(d, fdAnyKey(d, p.key, p.alt)); pts != nil && fdMax(pts) > 0 {
			// A rate of stalled microseconds per second is a fraction of wall clock.
			for i := range pts {
				pts[i] = pts[i] / 10000
			}
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	worst, which := 0.0, ""
	for _, s := range c.Series {
		if m := fdMax(s.Points); m > worst {
			worst, which = m, s.Name
		}
	}
	switch {
	case worst >= 40:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%s stalled work %.0f%% of the time at its worst", which, worst),
			Detail:   "Work on this machine spent that proportion of the interval waiting rather than running. Whatever the database looks like it is doing, this is the ceiling it is doing it under.",
			Action:   "Match the resource to the fix: io pressure is the disk (see the per-device charts), memory pressure means the working set does not fit and the machine is reclaiming, cpu pressure means genuine contention — often with a neighbour on shared hardware."}
	case worst >= 10:
		c.Advice = &fdAdvice{Level: "warn", Headline: fmt.Sprintf("%s stalled work up to %.0f%% of the time", which, worst)}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Nothing stalled for more than %.1f%% of the time", worst)}
	}
	return c
}

// fdDiskCharts builds one utilisation chart and one latency chart per block device that
// actually did something.
//
// Per device rather than aggregated, and discovered from the data rather than assumed: a
// database host routinely has the data on one device and the journal or the OS on another,
// and averaging them hides the one that is the problem. Devices with no I/O at all in the
// window are skipped, which on a typical host removes most of them.
func fdDiskCharts(d *ftdcData) []func(*ftdcData) *fdChart {
	devs := map[string]bool{}
	for k := range d.Series {
		rest, ok := strings.CutPrefix(k, "systemMetrics.disks.")
		if !ok {
			continue
		}
		i := strings.Index(rest, ".")
		if i <= 0 {
			continue
		}
		dev := rest[:i]
		// A device earns a chart by doing something IN THIS WINDOW, not by existing.
		// has() is not enough: io_time_ms is cumulative, so a disk that was busy once at
		// boot and has been idle ever since still has a non-zero value forever. A typical
		// host has four or five block devices and the database is on one of them; charting
		// the other four at a flat zero is four charts of nothing.
		if r := fdRate(d, "systemMetrics.disks."+dev+".io_time_ms"); fdMax(r) > 0.5 {
			devs[dev] = true
		}
	}
	var names []string
	for dev := range devs {
		names = append(names, dev)
	}
	// Busiest first: on a host where several devices are active, the one the database is
	// waiting on is the one worth seeing without scrolling.
	sort.Slice(names, func(i, j int) bool {
		bi := fdMax(fdRate(d, "systemMetrics.disks."+names[i]+".io_time_ms"))
		bj := fdMax(fdRate(d, "systemMetrics.disks."+names[j]+".io_time_ms"))
		if bi != bj {
			return bi > bj
		}
		return names[i] < names[j]
	})
	// More than a handful of busy devices is a host doing something unusual, and a page of
	// forty disk charts helps nobody.
	if len(names) > 4 {
		names = names[:4]
	}
	var out []func(*ftdcData) *fdChart
	for _, dev := range names {
		dev := dev
		out = append(out, func(d *ftdcData) *fdChart { return fdChartDisk(d, dev) })
	}
	return out
}

// fdChartDisk — one device's utilisation, queue depth and average service time.
//
// Read from /proc/diskstats the way iostat reads it: io_time_ms is milliseconds the device
// was busy, so its rate divided by ten is percent utilisation; read_time_ms over reads is
// the average time a read took, including its wait.
func fdChartDisk(d *ftdcData, dev string) *fdChart {
	base := "systemMetrics.disks." + dev + "."
	util := fdRate(d, base+"io_time_ms")
	if util == nil {
		return nil
	}
	for i := range util {
		util[i] /= 10
	}
	c := &fdChart{ID: "disk-" + dev, Group: "Host", Title: "Disk " + dev, Unit: "% busy · ms",
		Why: "Utilisation is the share of time the device had at least one request in flight — the same number iostat calls %util. The latencies are total service time divided by operation count, so they include queueing, which is what the database actually waits for. A device at 100% with low latency is working; a device at 100% with rising latency is the bottleneck. On a multi-queue or virtual device the kernel can report more busy time than wall clock and this reads above 100%, exactly as iostat does — past that point it means saturated and the number itself stops meaning anything."}
	c.Series = append(c.Series, fdSeries{Name: "% busy", Points: util})
	for _, p := range []struct{ name, num, den string }{
		{"read ms", base + "read_time_ms", base + "reads"},
		{"write ms", base + "write_time_ms", base + "writes"},
	} {
		if pts := fdRatio(d, p.num, p.den, 1); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	busy := fdMax(util)
	slowest := 0.0
	for _, s := range c.Series[1:] {
		if m := fdMax(s.Points); m > slowest {
			slowest = m
		}
	}
	switch {
	case slowest >= 50:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%s served requests in %.0f ms at its worst, %s", dev, slowest, fdBusy(busy)),
			Detail:   "Service time that high is the storage, not the database. Every read that misses the WiredTiger cache waits this long.",
			Action:   "Check what the device actually is and what else shares it. On a cloud volume this is usually an IOPS or throughput limit being hit; the burst-credit shape is a period of normal latency followed by a cliff."}
	case busy >= 90:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%s was %s at peak, serving in %.1f ms", dev, fdBusy(busy), slowest),
			Detail:   "Fully utilised but still fast. That is a device doing its job, with no headroom left for more."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("%s peaked at %s, %.1f ms", dev, fdBusy(busy), slowest)}
	}
	return c
}

// ------------------------------------------------- the second sweep of the namespace
//
// Everything below came from enumerating all 5,665 metrics in a real capture rather than
// from picking the familiar ones. The first pass at this page charted the metrics anybody
// would think of — lag, cache, tickets, CPU — and missed most of what FTDC actually knows.
//
// The recurring theme is that the important number is rarely a metric. Write-concern wait,
// journal fsync latency, the majority commit point, the share of eviction being done by
// application threads: all of them are two counters that have to be divided, and all of them
// are invisible in the server log at any verbosity.

// fdConst builds a flat reference line — a configured threshold, not a measurement.
func fdConst(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// fdSumRates adds together the per-second rates of every metric whose key matches.
//
// Used where the interesting quantity is spread across a family of keys — one per command,
// one per lock type — and the total is the thing worth a line. Rates are summed rather than
// the raw counters, so a metric that appears part-way through the window does not contribute
// its whole history as a single spike.
func fdSumRates(d *ftdcData, match func(key string) bool) []float64 {
	var out []float64
	for k := range d.Series {
		if !match(k) {
			continue
		}
		r := fdRate(d, k)
		if r == nil {
			continue
		}
		if out == nil {
			out = make([]float64, len(r))
		}
		for i := range r {
			if i < len(out) {
				out[i] += r[i]
			}
		}
	}
	return out
}

// fdSpanOf reports how much wall-clock time the samples where pick() is true add up to.
// Used for "a member was unreachable for 45 s", which is the number somebody wants rather
// than "17 samples had health 0".
func fdSpanOf(d *ftdcData, pick func(i int) bool) float64 {
	total := 0.0
	for i := 1; i < len(d.TS); i++ {
		if pick(i) {
			total += d.TS[i] - d.TS[i-1]
		}
	}
	return total
}

// ---------------------------------------------------------------- replication, deeper

// fdChartQuorum — how many members could actually acknowledge a write.
//
// `writableVotingMembersCount` looks like this answer and is not: it comes from the replica
// set CONFIG, so it reads 3 throughout an outage in which two members are unreachable. The
// honest version has to be counted per sample from each member's health and state, which is
// what this does — a member acknowledges a write only if this node can reach it and it is
// carrying data.
//
// This is the chart for the failure everybody meets eventually and nobody recognises: the
// primary is up, the server log says nothing much, and every write hangs, because w:majority
// cannot be satisfied.
func fdChartQuorum(d *ftdcData) *fdChart {
	idx := fdMemberIdx(d)
	if len(idx) == 0 {
		return nil
	}
	need := fdFloats(d, "replSetGetStatus.writeMajorityCount", 1)
	if need == nil {
		return nil
	}
	n := len(d.TS)
	avail := make([]float64, n)
	for i := 0; i < n; i++ {
		for _, m := range idx {
			h, okH := d.at(fmt.Sprintf("replSetGetStatus.members.%d.health", m), i)
			st, okS := d.at(fmt.Sprintf("replSetGetStatus.members.%d.state", m), i)
			// 1 PRIMARY, 2 SECONDARY. A member in RECOVERING or ROLLBACK is up, is not
			// refusing heartbeats, and still cannot acknowledge a write.
			if okH && okS && h == 1 && (st == 1 || st == 2) {
				avail[i]++
			}
		}
	}
	c := &fdChart{ID: "quorum", Group: "Replication", Title: "Members able to acknowledge a write", Unit: "members",
		Why: "Counted per sample from each member's heartbeat health and state, not from the configuration. When the solid line drops below the dashed one, a w:majority write cannot be acknowledged and the application hangs — while the primary is up, serving reads, and logging nothing that explains it."}
	c.Series = append(c.Series,
		fdSeries{Name: "reachable and carrying data", Points: avail},
		fdSeries{Name: "needed for a majority write", Points: need, Dashed: true})

	short := fdSpanOf(d, func(i int) bool { return i < len(need) && avail[i] < need[i] })
	// Which members went away, and for how long — the aggregate says a majority was lost,
	// this says who to go and look at.
	var gone []string
	for _, m := range idx {
		key := fmt.Sprintf("replSetGetStatus.members.%d.health", m)
		down := fdSpanOf(d, func(i int) bool { v, ok := d.at(key, i); return ok && v == 0 })
		if down > 0 {
			gone = append(gone, fmt.Sprintf("member %d for %s", m, lsDur(down)))
		}
	}
	switch {
	case short > 0:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("The set could not acknowledge a majority write for %s", lsDur(short)),
			Detail:   "For that long, every w:majority write waited and then timed out, readConcern:majority stopped advancing, and the majority commit point froze. An application sees hung writes; the primary sees nothing wrong with itself.",
			Action:   "Find out why the members went away — the member-state chart and the same window in Log Summary. A set that cannot form a write majority does not heal by restarting the primary."}
		if len(gone) > 0 {
			c.Advice.Detail += " Unreachable in this window: " + strings.Join(gone, ", ") + "."
		}
	case len(gone) > 0:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: "A member was unreachable but the majority held — " + strings.Join(gone, ", "),
			Detail:   "Writes carried on, with no redundancy left to lose. One more member away and the set stops accepting them."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("%.0f members available throughout, %.0f needed", fdMin(avail), fdMax(need))}
	}
	return c
}

// fdChartCommitLag — how far the majority commit point trailed this member's own writes.
//
// The replication-lag chart is per member. This is the number the CLIENT waits on: a
// w:majority write is acknowledged when the commit point passes it, and a readConcern
// "majority" read cannot see anything past it. A set can have every member within a second
// of the primary and still have a commit point that has stopped, which is what a lost
// majority looks like from inside the primary.
func fdChartCommitLag(d *ftdcData) *fdChart {
	applied := d.Series["replSetGetStatus.optimes.appliedOpTime.ts"]
	if applied == nil {
		return nil
	}
	c := &fdChart{ID: "commitLag", Group: "Replication", Title: "Majority commit point lag", Unit: "s",
		Why: "The gap between what this member has written and what the set has committed to a majority. Writes with w:majority wait for exactly this gap to close, and reads with readConcern majority cannot see past it. It is the only number that says whether the replica set is still functioning as one."}
	for _, p := range []struct{ name, key string }{
		{"behind the majority commit point", "replSetGetStatus.optimes.lastCommittedOpTime.ts"},
		{"not yet journalled", "replSetGetStatus.optimes.durableOpTime.ts"},
	} {
		s := d.Series[p.key]
		if s == nil {
			continue
		}
		gap := make([]float64, len(applied.Values))
		for i := range applied.Values {
			if i < len(s.Values) {
				// Both are the seconds half of a BSON timestamp, so the difference is
				// already in seconds. It can go slightly negative between samples of a
				// tree that is not captured atomically; clamp rather than draw it.
				if g := applied.Values[i] - s.Values[i]; g > 0 {
					gap[i] = float64(g)
				}
			}
		}
		if fdMax(gap) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: gap})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	worst := fdMax(c.Series[0].Points)
	switch {
	case worst >= 10:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("The commit point fell %s behind", lsDur(worst)),
			Detail:   "Every majority write issued in that window waited at least that long before it was acknowledged, and majority reads were that far in the past. The cause is always a member: the commit point moves at the speed of the slowest member needed to make up a majority.",
			Action:   "The quorum chart says whether a majority existed at all; the lag chart says which member was holding it up."}
	case worst >= 2:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Commit point trailed by up to %s", lsDur(worst)),
			Detail:   "Enough to be felt by anything writing with w:majority, which includes every transaction and every causally consistent session."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Commit point stayed within %s", lsDur(worst))}
	}
	return c
}

// fdChartWriteConcern — what write concern cost the application, in milliseconds.
//
// mongod records the total time spent waiting for write concern and the number of writes
// that waited. Neither is useful; divided, they are the average time a w>1 write spent
// waiting for other members after the primary had already done its part. That is replication
// lag expressed as something an application developer can recognise.
func fdChartWriteConcern(d *ftdcData) *fdChart {
	ms := fdRatio(d, "serverStatus.metrics.getLastError.wtime.totalMillis",
		"serverStatus.metrics.getLastError.wtime.num", 1)
	if ms == nil || fdMax(ms) == 0 {
		return nil
	}
	c := &fdChart{ID: "writeConcern", Group: "Replication", Title: "Time spent waiting for write concern", Unit: "ms · writes/s",
		Why: "The average time a write with w greater than 1 waited for other members to acknowledge it, after the primary had already done its work. An application that reports slow writes while every server-side latency looks fine is usually waiting here, and no slow-query log records it."}
	c.Series = append(c.Series, fdSeries{Name: "ms waiting", Points: ms})
	if n := fdRate(d, "serverStatus.metrics.getLastError.wtime.num"); n != nil && fdMax(n) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "writes that waited/s", Points: n})
	}
	timeouts := 0.0
	if v, ok := d.last("serverStatus.metrics.getLastError.wtimeouts"); ok {
		timeouts = float64(v)
	}
	worst := fdMax(ms)
	switch {
	case timeouts > 0:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%.0f write(s) gave up waiting for write concern", timeouts),
			Detail:   "A wtimeout means the write was applied on the primary and then reported as failed to the application. It is not rolled back. Anything that retried has written twice.",
			Action:   "Check the quorum chart for the same window — a write concern that cannot be satisfied is almost always a member that is unreachable rather than one that is slow."}
	case worst >= 100:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Writes waited an average of %.0f ms for other members", worst),
			Detail:   "This is added to every single write the application makes with w greater than 1, and it is paid on the client's clock.",
			Action:   "It is the secondaries, not the primary: look at their apply rate and their disks. If one member is consistently the straggler, and it exists only for redundancy, its votes and its priority are worth reviewing."}
	case worst >= 20:
		c.Advice = &fdAdvice{Level: "warn", Headline: fmt.Sprintf("Writes waited up to %.0f ms for write concern", worst)}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Write concern cost at most %.1f ms per write", worst)}
	}
	return c
}

// fdChartSyncSource — who each member was replicating from, and whether it kept changing.
//
// Replication in MongoDB is a tree, not a star: a secondary may sync from another secondary,
// and it chooses for itself. That choice is invisible in every other view, and two of the
// more baffling failures are visible only here — a member that cannot find any sync source
// at all, and a chain that has quietly re-rooted itself through a slow member so that
// everything downstream of it lags for no reason that member's own charts explain.
func fdChartSyncSource(d *ftdcData) *fdChart {
	idx := fdMemberIdx(d)
	if len(idx) == 0 {
		return nil
	}
	self := fdSelfIdx(d)
	c := &fdChart{ID: "syncSource", Group: "Replication", Title: "Sync source", Unit: "member index",
		Why: "Which member each one was pulling the oplog from, by index; -1 means it had none. Replication is a tree and each member picks its own parent, so a slow member can end up with two others chained behind it. A line that keeps moving is a member that cannot settle on a source, which costs it a re-fetch every time."}
	for _, m := range idx {
		pts := fdFloats(d, fmt.Sprintf("replSetGetStatus.members.%d.syncSourceId", m), 1)
		if pts == nil {
			continue
		}
		name := fmt.Sprintf("member %d", m)
		if m == self {
			name += " (this one)"
		}
		c.Series = append(c.Series, fdSeries{Name: name, Points: pts})
	}
	if len(c.Series) == 0 {
		return nil
	}
	// numTimesCouldNotFind is the count of sync-source selections that came back empty.
	notFound, changed := 0.0, 0.0
	if r := fdFloats(d, "serverStatus.metrics.repl.syncSource.numTimesCouldNotFind", 1); r != nil {
		notFound = fdMax(r) - fdMin(r)
	}
	if r := fdFloats(d, "serverStatus.metrics.repl.syncSource.numTimesChoseDifferent", 1); r != nil {
		changed = fdMax(r) - fdMin(r)
	}
	switch {
	case notFound > 0:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("This member failed to find a sync source %.0f time(s)", notFound),
			Detail:   "Each failure is ten seconds in which the member fetched no oplog at all and fell further behind. A member with no eligible source is usually one whose candidates are all too far ahead, all unreachable, or all behind it.",
			Action:   "Check the quorum and lag charts for the same window. If it persists, replSetSyncFrom names a source explicitly, but that is a diagnosis aid rather than a fix."}
	case changed > 1:
		c.Advice = &fdAdvice{Level: "info",
			Headline: fmt.Sprintf("Sync source changed %.0f times", changed),
			Detail:   "Some churn is normal after an election. Continuous churn means no candidate is comfortably better than the others, and every change costs a re-fetch."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: "Sync sources were stable across the window"}
	}
	return c
}

// ---------------------------------------------------------------- work, deeper

// fdChartCommandMix — which commands the server actually spent its time on.
//
// opcounters lumps almost everything into "command". This breaks it out by name, which
// answers a question the operation chart cannot: whether the server was doing the
// application's work at all. A server whose busiest command is `hello` is being monitored,
// not used; one whose busiest is `replSetHeartbeat` is talking to itself.
func fdChartCommandMix(d *ftdcData) *fdChart {
	const pre = "serverStatus.metrics.commands."
	type cmd struct {
		name string
		pts  []float64
		peak float64
	}
	var all []cmd
	for k := range d.Series {
		name, ok := strings.CutPrefix(k, pre)
		if !ok || !strings.HasSuffix(name, ".total") {
			continue
		}
		pts := fdRate(d, k)
		if pts == nil || fdMax(pts) == 0 {
			continue
		}
		all = append(all, cmd{strings.TrimSuffix(name, ".total"), pts, fdMax(pts)})
	}
	if len(all) == 0 {
		return nil
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].peak != all[j].peak {
			return all[i].peak > all[j].peak
		}
		return all[i].name < all[j].name
	})
	// Eight lines is the most a stacked chart stays readable with; the tail of a command
	// distribution is always long and always uninteresting.
	if len(all) > 8 {
		all = all[:8]
	}
	c := &fdChart{ID: "commandMix", Group: "Work", Title: "Commands by name", Unit: "commands/s", Stack: true,
		Why: "The busiest eight commands, as rates. opcounters calls nearly all of these 'command' and stops there. Worth knowing which of them are not the application: replSetHeartbeat is the set checking on itself, and hello is every driver in the estate monitoring its topology — both scale with the number of clients rather than with the work being done."}
	for _, x := range all {
		c.Series = append(c.Series, fdSeries{Name: x.name, Points: x.pts})
	}
	total := 0.0
	for _, x := range all {
		total += x.peak
	}
	c.Advice = &fdAdvice{Level: "info",
		Headline: fmt.Sprintf("Busiest command was %s, peaking at %.1f/s", all[0].name, all[0].peak),
		Detail:   fmt.Sprintf("Out of roughly %.1f commands/s across the eight shown.", total)}
	if n := all[0].name; n == "hello" || n == "isMaster" || n == "replSetHeartbeat" || n == "ping" {
		c.Advice.Level = "info"
		c.Advice.Detail += " That command is monitoring rather than application work — this server was mostly being asked how it was."
	}
	return c
}

// fdChartErrors — errors the server returned to clients.
//
// asserts.user counts every error handed back to a client: duplicate keys, failed
// validation, refused authentication, a query the server would not run. None of it reaches
// the log at default verbosity, so an application failing every second can look, from the
// server's side, exactly like an application working perfectly.
func fdChartErrors(d *ftdcData) *fdChart {
	c := &fdChart{ID: "errors", Group: "Work", Title: "Errors returned to clients", Unit: "errors/s",
		Why: "Errors mongod handed back to a client, and internal assertions. The server log does not record these at default verbosity, which is why an application that is failing on every request can look completely healthy from the database side. A step change here that lines up with a deployment is usually the deployment."}
	for _, p := range []struct{ name, key string }{
		{"returned to clients", "serverStatus.asserts.user"},
		{"internal assertions", "serverStatus.asserts.regular"},
		{"warnings", "serverStatus.asserts.warning"},
		{"messages", "serverStatus.asserts.msg"},
	} {
		if pts := fdRate(d, p.key); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if failed := fdSumRates(d, func(k string) bool {
		return strings.HasPrefix(k, "serverStatus.metrics.commands.") && strings.HasSuffix(k, ".failed")
	}); fdMax(failed) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "commands that failed", Points: failed})
	}
	if len(c.Series) == 0 {
		return nil
	}
	worst := fdMax(c.Series[0].Points)
	switch {
	case worst >= 10:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Peak %.0f errors/s returned to clients", worst),
			Detail:   "An error rate this high is a client being told no, over and over. Duplicate keys from a retry loop, an expired credential, and a driver failing authentication all look like this.",
			Action:   "The server will not say which without raising the log level. Start from the application's own error log for the same minute — it has the message, and this has the rate."}
	case worst > 0:
		c.Advice = &fdAdvice{Level: "info", Headline: fmt.Sprintf("Up to %.1f errors/s returned to clients", worst)}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: "No errors returned to clients"}
	}
	return c
}

// fdChartContention — work the server did that it did not have to.
//
// Three counters that are each a different kind of waste, and none of which appear in a
// slow-query log because none of them belongs to a single slow query:
//
//	writeConflicts — two updates hit the same document, WiredTiger rolled one back, and
//	                 mongod retried it silently. The client pays in latency and sees nothing.
//	scanAndOrder   — a sort with no index to support it, so the results were sorted in memory.
//	lock waits     — an operation that had to queue for a lock before it could start.
func fdChartContention(d *ftdcData) *fdChart {
	c := &fdChart{ID: "contention", Group: "Work", Title: "Contention and avoidable work", Unit: "events/s",
		Why: "Write conflicts are concurrent updates to the same document being retried invisibly — latency the client pays and nothing logs. In-memory sorts are a sort with no index behind it. Lock waits are operations queuing before they can even start. All three rise with concurrency rather than with any one query, which is why they are so hard to find from a slow-query log."}
	for _, p := range []struct{ name, key string }{
		{"write conflicts", "serverStatus.metrics.operation.writeConflicts"},
		{"in-memory sorts", "serverStatus.metrics.operation.scanAndOrder"},
	} {
		if pts := fdRate(d, p.key); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if waits := fdSumRates(d, func(k string) bool {
		return strings.HasPrefix(k, "serverStatus.locks.") && strings.Contains(k, ".acquireWaitCount.")
	}); fdMax(waits) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "lock waits", Points: waits})
	}
	if len(c.Series) == 0 {
		return nil
	}
	// Time actually lost to lock waiting, which is the number worth quoting — a wait that
	// resolves in microseconds is not a problem however often it happens.
	lockMs := fdSumRates(d, func(k string) bool {
		return strings.HasPrefix(k, "serverStatus.locks.") && strings.Contains(k, ".timeAcquiringMicros.")
	})
	worstLock := fdMax(lockMs) / 1000
	conflicts := fdMax(fdRate(d, "serverStatus.metrics.operation.writeConflicts"))
	switch {
	case conflicts >= 10:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Peak %.0f write conflicts/s", conflicts),
			Detail:   "Concurrent writers competing for the same documents. Each conflict is a retry, so the work is done more than once and the client waits for all of it.",
			Action:   "Look for a hot document — a counter row, a job queue, a single-document lock. Spreading the write across more documents is the fix; more hardware is not."}
	case worstLock >= 100:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Up to %.0f ms per second spent waiting for locks", worstLock),
			Detail:   "That is time operations spent queued before they could start, which no per-operation timing attributes to anything."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Little contention — peak %.1f events/s, %.1f ms/s waiting for locks", fdMax(c.Series[0].Points), worstLock)}
	}
	return c
}

// ---------------------------------------------------------------- storage engine, deeper

// fdChartJournal — how long a durable write actually took to reach the disk.
//
// Every write with j:true, and every majority write on a default deployment, waits for a
// WiredTiger log fsync. The counters are cumulative total microseconds and a cumulative
// count; divided, they are the average fsync latency, and that is the floor under every
// durable write on this server. It appears nowhere else — not in opLatencies, not in the
// slow-query log, not in the server log at any verbosity.
func fdChartJournal(d *ftdcData) *fdChart {
	ms := fdRatio(d, "serverStatus.wiredTiger.log.log sync time duration (usecs)",
		"serverStatus.wiredTiger.log.log sync operations", 1.0/1000)
	if ms == nil || fdMax(ms) == 0 {
		return nil
	}
	c := &fdChart{ID: "journal", Group: "Storage engine", Title: "Journal sync latency", Unit: "ms · MiB/s",
		Why: "The average time a WiredTiger journal fsync took. Every j:true write and every majority write waits for one of these, so this is the floor under durable write latency on this server — and it is a property of the disk, not of the query. A journal on the same device as the data competes with checkpoints for it."}
	c.Series = append(c.Series, fdSeries{Name: "ms per sync", Points: ms})
	if b := fdRate(d, "serverStatus.wiredTiger.log.log bytes written"); b != nil && fdMax(b) > 0 {
		for i := range b {
			b[i] /= 1024 * 1024
		}
		c.Series = append(c.Series, fdSeries{Name: "MiB/s to journal", Points: b})
	}
	worst := fdMax(ms)
	switch {
	case worst >= 20:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Journal syncs took up to %.0f ms", worst),
			Detail:   "Every durable write on this server waited at least this long, on top of everything else it did. Nothing in the query layer moves this number.",
			Action:   "This is the storage. Check the disk charts for the device the dbPath is on, and whether it is shared with the data files or with another workload. On a cloud volume it is usually an IOPS limit or a burst balance running out."}
	case worst >= 5:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Journal syncs averaged up to %.1f ms", worst),
			Detail:   "Slower than a local NVMe device should be. It is paid by every majority write."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Journal syncs stayed under %.1f ms", worst)}
	}
	return c
}

// fdChartCachePressure — the cache as a percentage, against the thresholds that change
// WiredTiger's behaviour.
//
// The MiB chart says how full the cache is; this says which side of the line it is on, and
// the lines are what matter. WiredTiger changes behaviour at fixed percentages: it starts
// evicting at 80% full or 5% dirty, and past 95% full or 20% dirty it stops being polite
// about it and makes the threads running user operations do the eviction themselves.
func fdChartCachePressure(d *ftdcData) *fdChart {
	maxB := fdFloats(d, "serverStatus.wiredTiger.cache.maximum bytes configured", 1)
	inCache := fdFloats(d, "serverStatus.wiredTiger.cache.bytes currently in the cache", 1)
	if maxB == nil || inCache == nil || fdMax(maxB) == 0 {
		return nil
	}
	pct := func(v []float64) []float64 {
		out := make([]float64, len(v))
		for i := range v {
			if i < len(maxB) && maxB[i] > 0 {
				out[i] = v[i] / maxB[i] * 100
			}
		}
		return out
	}
	c := &fdChart{ID: "cachePressure", Group: "Storage engine", Title: "Cache pressure against WiredTiger's thresholds", Unit: "% of cache",
		Why: "The same cache as the chart above, expressed the way WiredTiger reasons about it. Eviction begins at 80% full or 5% dirty; past 95% full or 20% dirty it becomes urgent and application threads are conscripted to do it, at which point every operation slows down together and none of them looks guilty."}
	used := pct(inCache)
	c.Series = append(c.Series, fdSeries{Name: "in cache", Points: used})
	var dirtyPct []float64
	if dirty := fdFloats(d, "serverStatus.wiredTiger.cache.tracked dirty bytes in the cache", 1); dirty != nil {
		dirtyPct = pct(dirty)
		c.Series = append(c.Series, fdSeries{Name: "dirty", Points: dirtyPct})
	}
	n := len(used)
	c.Series = append(c.Series,
		fdSeries{Name: "eviction urgent (95%)", Points: fdConst(n, 95), Dashed: true},
		fdSeries{Name: "dirty urgent (20%)", Points: fdConst(n, 20), Dashed: true})

	pk, pkDirty := fdMax(used), fdMax(dirtyPct)
	switch {
	case pk >= 95 || pkDirty >= 20:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Cache reached %.0f%% full and %.0f%% dirty", pk, pkDirty),
			Detail:   "Past these points WiredTiger makes the threads running user operations evict pages before it will let them proceed. The signature is everything becoming slow at once, with no individual operation to blame.",
			Action:   "Either the write rate is beyond what the disk can absorb — the journal and disk charts say so — or the working set has outgrown the cache. The eviction chart below says which of the two it is."}
	case pk >= 80 || pkDirty >= 5:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Cache reached %.0f%% full and %.0f%% dirty — eviction was active", pk, pkDirty),
			Detail:   "Normal for a busy server; this is the range WiredTiger is designed to sit in. Worth knowing that there is no headroom above it."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Cache peaked at %s full, %s dirty — below every eviction threshold", fdPctV(pk), fdPctV(pkDirty))}
	}
	return c
}

// fdChartEviction — who was doing the eviction.
//
// WiredTiger has threads whose job is evicting pages. When they cannot keep up it conscripts
// the threads that are running user operations, and those operations pay for the eviction
// out of their own latency. The share of eviction being done by application threads is
// therefore a direct measure of whether the cache is coping, and it is one of the few places
// where a cause can be read off rather than inferred.
func fdChartEviction(d *ftdcData) *fdChart {
	const pre = "serverStatus.wiredTiger.cache."
	app := fdRate(d, pre+"application threads page write from cache to disk count")
	total := fdRate(d, pre+"pages written from cache")
	if app == nil && total == nil {
		return nil
	}
	c := &fdChart{ID: "eviction", Group: "Storage engine", Title: "Eviction", Unit: "pages/s",
		Why: "Pages moving in and out of the cache, split by who moved them. WiredTiger has dedicated eviction threads; when they fall behind it makes the threads running user operations do the work instead, and that shows up to the client as latency with no slow operation behind it. Pages read into cache is the other direction — the working set not fitting."}
	for _, p := range []struct {
		name string
		pts  []float64
	}{
		{"written by eviction, total", total},
		{"written by application threads", app},
		{"read into cache", fdRate(d, pre+"pages read into cache")},
	} {
		if p.pts != nil && fdMax(p.pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: p.pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	// The share has to be taken over the WINDOW, not peak against peak: the two peaks are
	// rarely the same interval, and dividing one by the other invents a ratio that never
	// happened. Cumulative counters make the honest version easy — first sample to last.
	span := 1.0
	if s := d.span().Seconds(); s > 0 {
		span = s
	}
	appPages := fdMax(fdFloats(d, pre+"application threads page write from cache to disk count", 1)) -
		fdMin(fdFloats(d, pre+"application threads page write from cache to disk count", 1))
	allPages := fdMax(fdFloats(d, pre+"pages written from cache", 1)) -
		fdMin(fdFloats(d, pre+"pages written from cache", 1))
	share := 0.0
	if allPages > 0 {
		share = appPages / allPages * 100
	}
	sustained := allPages / span
	// Both halves of the test matter. On a near-idle server almost all of the little
	// eviction there is gets done by application threads, simply because the eviction
	// threads have nothing to wake up for — a 90% share of half a page a second is not a
	// finding, and reporting it as one would make this chart noise.
	switch {
	case share >= 20 && sustained >= 20:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Application threads did %.0f%% of the eviction", share),
			Detail:   "Operations were made to evict pages before they were allowed to proceed. Everything slows together and no single operation looks slow, which is what makes this so hard to find any other way.",
			Action:   "Give eviction more room or less to do: a larger cache, a faster device under the dbPath, or a lower sustained write rate. Raising eviction thread counts helps only when the disk is not already the limit — the disk charts say whether it is."}
	case fdMax(fdRate(d, pre+"pages read into cache")) >= 100:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Up to %.0f pages/s read from disk into the cache", fdMax(fdRate(d, pre+"pages read into cache"))),
			Detail:   "Sustained reads into the cache mean the working set does not fit in it. Every one of those pages is a query waiting on the disk."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Eviction kept up — %s pages/s sustained, %s of it by application threads", fdAmt(sustained), fdPctV(share))}
	}
	return c
}

// fdChartEngineIO — what the storage engine itself read and wrote.
//
// Separate from the disk charts, which measure the whole machine. Reads here are cache
// misses going to the device; writes are almost entirely checkpoints. A server with a
// working set that fits reads nearly nothing after it has warmed up, so a steady read line
// is the clearest statement this page makes that the cache is too small.
func fdChartEngineIO(d *ftdcData) *fdChart {
	c := &fdChart{ID: "engineIO", Group: "Storage engine", Title: "Storage engine I/O", Unit: "MiB/s",
		Why: "Bytes WiredTiger itself moved to and from the disk, as opposed to the device counters further down which include everything else on the machine. Reads are cache misses; a server whose working set fits in the cache reads almost nothing once it is warm. Writes are dominated by checkpoints, which arrive every sixty seconds as a burst rather than a trickle."}
	for _, p := range []struct{ name, key string }{
		{"read", "serverStatus.wiredTiger.block-manager.bytes read"},
		{"written", "serverStatus.wiredTiger.block-manager.bytes written"},
		{"of which checkpoint", "serverStatus.wiredTiger.block-manager.bytes written for checkpoint"},
	} {
		pts := fdRate(d, p.key)
		if pts == nil || fdMax(pts) == 0 {
			continue
		}
		for i := range pts {
			pts[i] /= 1024 * 1024
		}
		c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
	}
	if len(c.Series) == 0 {
		return nil
	}
	rd := fdMax(fdRate(d, "serverStatus.wiredTiger.block-manager.bytes read")) / (1024 * 1024)
	if rd >= 5 {
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Peak %.1f MiB/s read from disk into the engine", rd),
			Detail:   "Sustained reading means the data being queried is not in the cache. Every one of those bytes is an operation blocked on the device.",
			Action:   "Compare with the cache chart: a cache that is full and still reading is a cache too small for the working set."}
	} else {
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Engine read %s MiB/s at peak, wrote %s MiB/s", fdAmt(rd), fdAmt(fdMax(fdRate(d, "serverStatus.wiredTiger.block-manager.bytes written"))/(1024*1024)))}
	}
	return c
}

// fdChartHistoryStore — the cost of keeping old versions of documents readable.
//
// WiredTiger keeps superseded versions of a document in the history store so that snapshots
// opened before the change can still see them. It is what makes readConcern:majority and
// long-running transactions possible, and it is unbounded: one forgotten cursor or one
// transaction nobody committed pins the snapshot, and the history store grows until the
// cache is full of it. The incident reads as "the cache filled up and the server slowed
// down, with no traffic", which is baffling from every other chart on this page.
func fdChartHistoryStore(d *ftdcData) *fdChart {
	const pre = "serverStatus.wiredTiger.cache."
	inCache := fdFloats(d, pre+"bytes belonging to the history store table in the cache", 1.0/(1024*1024))
	if inCache == nil {
		return nil
	}
	c := &fdChart{ID: "historyStore", Group: "Storage engine", Title: "History store", Unit: "MiB",
		Why: "Older versions of documents, kept so that snapshots opened before a change can still read them. It is what makes readConcern majority and long transactions work, and it has no ceiling: a cursor nobody closed or a transaction nobody committed pins the snapshot and this grows until it has eaten the cache. A cache that fills on a server with no traffic is usually this."}
	c.Series = append(c.Series, fdSeries{Name: "in cache", Points: inCache})
	if disk := fdFloats(d, pre+"history store table on-disk size", 1.0/(1024*1024)); disk != nil && fdMax(disk) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "on disk", Points: disk})
	}
	window := 0.0
	if v, ok := d.last("serverStatus.wiredTiger.snapshot-window-settings.current available snapshot window size in seconds"); ok {
		window = float64(v)
	}
	share := 0.0
	if maxB := fdMax(fdFloats(d, pre+"maximum bytes configured", 1.0/(1024*1024))); maxB > 0 {
		share = fdMax(inCache) / maxB * 100
	}
	switch {
	case share >= 10:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("History store held %.0f%% of the cache", share),
			Detail:   "That much cache spent on old document versions is cache not spent on the data being queried, and it usually means something is holding a snapshot open far longer than it should.",
			Action:   "Look for long-running transactions and idle cursors — currentOp with secs_running set is the direct answer. The majority commit point stalling does the same thing, so check the commit-lag chart too."}
	case window > 0:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("History store peaked at %s MiB, snapshot window %.0f s", fdAmt(fdMax(inCache)), window),
			Detail:   "The snapshot window is how far back a reader may still look. It shrinks under cache pressure, which is WiredTiger protecting itself at the cost of long-running reads."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("History store peaked at %s MiB", fdAmt(fdMax(inCache)))}
	}
	return c
}

// ---------------------------------------------------------------- host, deeper

// fdChartHostMemory — the machine's memory, not the process's.
//
// The memory chart above is mongod's own. This is what the kernel had left, and it is the
// one that decides whether the OOM killer arrives — an event that appears in dmesg, does not
// appear in the mongod log at all, and looks from the database's side like a clean restart
// with nothing before it.
func fdChartHostMemory(d *ftdcData) *fdChart {
	avail := fdFloats(d, "systemMetrics.memory.MemAvailable_kb", 1.0/1024)
	if avail == nil {
		return nil
	}
	c := &fdChart{ID: "hostMemory", Group: "Host", Title: "Host memory", Unit: "MiB",
		Why: "What the kernel had available, against what the machine has. MemAvailable falling towards zero is how a mongod gets OOM-killed, and that leaves nothing in the server log — the last line before the restart is whatever it happened to be doing. Swap in use on a database host is a machine already past the point of running well."}
	c.Series = append(c.Series, fdSeries{Name: "available", Points: avail})
	if cached := fdFloats(d, "systemMetrics.memory.Cached_kb", 1.0/1024); cached != nil {
		c.Series = append(c.Series, fdSeries{Name: "page cache", Points: cached})
	}
	total := fdFloats(d, "systemMetrics.memory.MemTotal_kb", 1.0/1024)
	swapUsed := []float64(nil)
	st := fdFloats(d, "systemMetrics.memory.SwapTotal_kb", 1.0/1024)
	sf := fdFloats(d, "systemMetrics.memory.SwapFree_kb", 1.0/1024)
	if st != nil && sf != nil {
		swapUsed = make([]float64, len(st))
		for i := range st {
			if i < len(sf) && st[i] > sf[i] {
				swapUsed[i] = st[i] - sf[i]
			}
		}
		if fdMax(swapUsed) > 0 {
			c.Series = append(c.Series, fdSeries{Name: "swap in use", Points: swapUsed})
		}
	}
	if total != nil {
		c.Series = append(c.Series, fdSeries{Name: "installed", Points: total, Dashed: true})
	}
	tot, low := fdMax(total), fdMin(avail)
	switch {
	case tot > 0 && low/tot*100 < 5:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Only %.0f MiB of %.0f MiB was available at the worst point", low, tot),
			Detail:   "At that margin the kernel is reclaiming continuously and the OOM killer is one allocation away. If this capture ends abruptly, that is very likely what happened.",
			Action:   "Check dmesg for an oom-kill line at the end of the window. A WiredTiger cache sized at half of RAM leaves the other half for connections, aggregation buffers and the page cache, and that is often not enough."}
	case fdMax(swapUsed) > 64:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%.0f MiB of swap in use", fdMax(swapUsed)),
			Detail:   "Any part of a database's working memory that reaches swap is read back at disk speed. The symptom is latency with no corresponding work."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("At least %.0f MiB of %.0f MiB stayed available", low, tot)}
	}
	return c
}

// fdChartFaults — memory that had to come from the disk.
//
// A major fault is the process touching memory that is not resident, so the kernel fetches
// it from the device while the thread waits. On a database that is the working set not
// fitting, expressed in the most direct terms the kernel has. Swap in and out on the same
// chart because they are the same failure one stage further along.
func fdChartFaults(d *ftdcData) *fdChart {
	c := &fdChart{ID: "faults", Group: "Host", Title: "Major faults and swapping", Unit: "pages/s",
		Why: "A major fault is memory the process asked for that had to be fetched from disk while the thread waited. Minor faults are free and are not shown. Any sustained rate here on a database host means the working set does not fit in RAM; pages moving to or from swap means it already did not, some time ago."}
	for _, p := range []struct{ name, key string }{
		{"major faults", "systemMetrics.vmstat.pgmajfault"},
		{"swapped in", "systemMetrics.vmstat.pswpin"},
		{"swapped out", "systemMetrics.vmstat.pswpout"},
	} {
		if pts := fdRate(d, p.key); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	maj := fdMax(fdRate(d, "systemMetrics.vmstat.pgmajfault"))
	swap := fdMax(fdRate(d, "systemMetrics.vmstat.pswpin")) + fdMax(fdRate(d, "systemMetrics.vmstat.pswpout"))
	switch {
	case swap > 0:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Pages moved to or from swap, peaking at %.0f/s", swap),
			Detail:   "Swapping on a database host means memory pressure the kernel could not resolve any other way. Everything that touches a swapped page waits on the disk for it.",
			Action:   "Size the WiredTiger cache so that it plus connections plus the page cache fits in RAM with room to spare. Turning swap off does not fix this — it converts it into an OOM kill."}
	case maj >= 10:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Peak %.0f major faults/s", maj),
			Detail:   "Each one is a thread stopped while the kernel fetched a page from the device."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Almost no major faults — peak %.1f/s, no swapping", maj)}
	}
	return c
}

// fdBusy words a device utilisation instead of printing it, because past 100% the number is
// no longer a percentage of anything.
//
// /proc/diskstats accumulates busy time per queue, so a multi-queue NVMe or a virtio device
// under a hypervisor can report more busy milliseconds than the wall clock had. iostat shows
// the same thing and it is not a decode error. "317% busy" reads as a broken chart; the
// truthful reading is that the device was saturated, so that is what it says.
func fdBusy(pct float64) string {
	if pct >= 100 {
		return "saturated"
	}
	return fmt.Sprintf("%.0f%% busy", pct)
}

// fdGapNote reports holes in the capture.
//
// This matters more than it sounds. A chart drawn across a gap joins the sample before it to
// the sample after with a straight line, and a straight line reads as "nothing changed" when
// what it actually means is "nothing was recorded". mongod writes FTDC only while it is
// running, so a gap is almost always a period when the server was down — which is usually
// the most interesting thing in the file.
func fdGapNote(d *ftdcData) []string {
	if len(d.TS) < 3 {
		return nil
	}
	// The threshold is relative to how often this server actually samples: the default is
	// once a second, but a tuned deployment can be far slower, and calling every ordinary
	// interval a gap would be worse than saying nothing.
	spacing := make([]float64, 0, len(d.TS)-1)
	for i := 1; i < len(d.TS); i++ {
		spacing = append(spacing, d.TS[i]-d.TS[i-1])
	}
	sort.Float64s(spacing)
	median := spacing[len(spacing)/2]
	if median <= 0 {
		median = 1
	}
	threshold := median * 10
	if threshold < 60 {
		threshold = 60
	}
	n, total, longest := 0, 0.0, 0.0
	for _, g := range spacing {
		if g > threshold {
			n++
			total += g
			if g > longest {
				longest = g
			}
		}
	}
	if n == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"The capture is not continuous: %d gap(s) with no samples, %s in total and %s at the longest. "+
			"mongod writes diagnostic.data only while it is running, so a gap is usually a period when it was not. "+
			"Every chart draws a straight line across one, which is the absence of data rather than the absence of change.",
		n, lsDur(total), lsDur(longest))}
}

// ---------------------------------------------------------------- sharded clusters
//
// A sharded cluster has three kinds of process and they capture three different things.
// A shard member and a config-server member are ordinary mongods: every chart above works
// on them unchanged. A mongos is not — it has no storage engine and no replica set, so
// two thirds of this page is correctly empty on one, and what it does have instead is the
// only view of the cluster as a whole that exists anywhere.
//
// The charts below appear only when their metrics do, so a replica-set capture is
// unaffected by any of it.
//
// One of them can do something no other chart on this page can. Member names are not in a
// mongod's FTDC — strings are not metrics, so replSetGetStatus.members.0 has a state and a
// ping and no name. A mongos keeps its connection-pool statistics keyed BY HOSTNAME, so a
// capture from the router names every shard member in the cluster and says how far away
// each one was.

// fdRole guesses what kind of process this capture came from.
//
// Worth stating on the page rather than leaving the reader to infer it from which charts
// are missing: "twelve charts" from a mongos is complete, and "twelve charts" from a shard
// member means something is wrong.
func fdRole(d *ftdcData) string {
	switch {
	case d.Series["connPoolStats.totalInUse"] != nil && d.Series["serverStatus.wiredTiger.cache.bytes currently in the cache"] == nil:
		return "mongos router"
	case d.Meta["replSet"] == "cfg" || d.Meta["replSet"] == "config":
		return "config server"
	case d.Series["serverStatus.shardingStatistics.countDonorMoveChunkStarted"] != nil:
		return "shard member"
	case d.Series["replSetGetStatus.myState"] != nil:
		return "replica-set member"
	}
	return ""
}

// fdChartTargeting — how many shards each operation had to touch.
//
// The single most useful thing a mongos records. An operation that carries the shard key
// goes to one shard; one that does not goes to EVERY shard, and every shard does the whole
// query. The cluster then costs more than one server would and gets slower as it grows,
// which is the opposite of the reason anybody shards.
//
// It is the sharded twin of "documents examined per document returned": both measure work
// that did not have to happen, and neither appears in a slow-query log because no single
// operation looks slow.
func fdChartTargeting(d *ftdcData) *fdChart {
	const pre = "serverStatus.shardingStatistics.numHostsTargeted."
	c := &fdChart{ID: "targeting", Group: "Sharding", Title: "Operations by how many shards they touched", Unit: "ops/s", Stack: true,
		Why: "An operation that carries the shard key is routed to one shard. One that does not is broadcast to every shard, and every shard runs the whole query — so the work multiplies by the shard count and gets worse as the cluster grows. A large 'all shards' share is a query missing its shard key, and it is the most common reason a sharded cluster is slower than the single server it replaced."}
	for _, kind := range []string{"oneShard", "manyShards", "allShards", "unsharded"} {
		var sum []float64
		for _, op := range []string{"find", "insert", "update", "delete", "aggregate"} {
			r := fdRate(d, pre+op+"."+kind)
			if r == nil {
				continue
			}
			if sum == nil {
				sum = make([]float64, len(r))
			}
			for i := range r {
				if i < len(sum) {
					sum[i] += r[i]
				}
			}
		}
		if fdMax(sum) > 0 {
			c.Series = append(c.Series, fdSeries{Name: kind, Points: sum})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	one, all, busiest, busiestN := 0.0, 0.0, "", 0.0
	for _, s := range c.Series {
		if m := fdMax(s.Points); m > busiestN {
			busiestN, busiest = m, s.Name
		}
		switch s.Name {
		case "oneShard":
			one = fdMax(s.Points)
		case "allShards":
			all = fdMax(s.Points)
		}
	}
	switch {
	case all > 0 && all >= one:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Broadcast to every shard peaked at %.1f ops/s, against %.1f routed to one", all, one),
			Detail:   "More work was broadcast than routed. Each broadcast operation runs on every shard and the router merges the results, so the cluster is doing N times the work a single server would and adding shards makes it worse rather than better.",
			Action:   "Find the queries with no shard key in them. explain() on a mongos names the shards it targeted, and the shard key is chosen once and cannot be changed cheaply — so this is worth resolving before the collection grows."}
	case all > 0:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Some work was broadcast to every shard — peak %.1f ops/s", all),
			Detail:   "Normal in small amounts: anything that genuinely spans the key range has to fan out. Worth knowing what proportion it is."}
	default:
		// Not "peak 0.0 ops/s to a single shard" on a cluster whose traffic was all
		// against unsharded collections: name whichever kind actually carried the work.
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Nothing was broadcast to every shard — busiest was %q at %s ops/s", busiest, fdAmt(busiestN))}
	}
	return c
}

// fdChartShardPing — the router's own round-trip time to every member of every shard.
//
// The one chart on this page that names hosts. A mongod's FTDC cannot: strings are not
// metrics, so its members are "member 0" and "member 1". A mongos keeps these keyed by
// replica set and hostname, which makes a router capture the fastest way there is to answer
// "which shard is the slow one" — and it answers it for the config servers too.
func fdChartShardPing(d *ftdcData) *fdChart {
	const pre = "connPoolStats.replicaSetPingTimesMillis."
	keys := d.keysWithPrefix(pre)
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	c := &fdChart{ID: "shardPing", Group: "Sharding", Title: "Round-trip time to each shard member", Unit: "ms",
		Why: "How far away the router found each member of each shard, measured by the router itself. This is the only chart here that can name a host — a mongod's own capture cannot, because names are strings and strings are not metrics. One member consistently slower than its peers is the one to go and look at; a whole replica set slower than the others is usually the network between them."}
	worst, who := 0.0, ""
	for _, k := range keys {
		pts := fdFloats(d, k, 1)
		if pts == nil {
			continue
		}
		// "rs1.s1r2.example.net:27017" — the set, then the host. The set is worth keeping:
		// it is what says whether the slow member is a shard or a config server.
		name := strings.TrimPrefix(k, pre)
		if i := strings.Index(name, "."); i > 0 {
			name = name[:i] + " / " + lsMongoShortHost(name[i+1:])
		}
		c.Series = append(c.Series, fdSeries{Name: name, Points: pts})
		if m := fdMax(pts); m > worst {
			worst, who = m, name
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	switch {
	case worst >= 50:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%s was %.0f ms away at its worst", who, worst),
			Detail:   "Every operation the router sent there paid that on top of whatever the shard itself did. On a cluster inside one data centre this is a network or a saturated host rather than a distance."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Every member stayed within %.0f ms of the router (%d members across %d)", worst, len(c.Series), len(c.Series))}
	}
	return c
}

// fdChartCatalogCache — how often the routing table had to be fetched, and how often it was
// found to be wrong.
//
// Every process in a sharded cluster caches where the chunks are. When a chunk moves, that
// cache is stale, and the next operation to use it gets a StaleConfig error, refreshes and
// retries. The client never sees the error — it sees the latency of all three steps.
func fdChartCatalogCache(d *ftdcData) *fdChart {
	const pre = "serverStatus.shardingStatistics.catalogCache."
	c := &fdChart{ID: "catalogCache", Group: "Sharding", Title: "Routing table refreshes", Unit: "per second · ms",
		Why: "Everything in a sharded cluster caches which shard holds which range. A chunk moving makes that cache wrong, and the next operation to use it fails with StaleConfig, refreshes, and runs again — the client sees none of that, only the time all three took. A steady stream of stale-config errors is a cluster whose chunks are moving faster than its routers can keep up with."}
	for _, p := range []struct{ name, key string }{
		{"stale-config errors", pre + "countStaleConfigErrors"},
		{"full refreshes", pre + "countFullRefreshesStarted"},
		{"incremental refreshes", pre + "countIncrementalRefreshesStarted"},
		{"failed refreshes", pre + "countFailedRefreshes"},
	} {
		if pts := fdRate(d, p.key); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	// Time operations actually spent blocked waiting for routing information, which is the
	// number that matters — a refresh nobody waited on cost nothing.
	if ms := fdRatio(d, pre+"totalRefreshWaitTimeMicros", pre+"countIncrementalRefreshesStarted", 1.0/1000); ms != nil && fdMax(ms) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "ms waiting per refresh", Points: ms})
	}
	if len(c.Series) == 0 {
		return nil
	}
	failed := fdMax(fdRate(d, pre+"countFailedRefreshes"))
	stale := fdMax(fdRate(d, pre+"countStaleConfigErrors"))
	switch {
	case failed > 0:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Routing table refreshes FAILED, peaking at %.1f/s", failed),
			Detail:   "A refresh that fails means this process could not read the chunk map from the config servers. Until it can, it cannot route anything it has not already cached, and operations fail rather than slow down.",
			Action:   "The config servers are the first thing to check — their replica set having no primary stops the whole cluster's metadata, while every shard carries on looking perfectly healthy."}
	case stale >= 1:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Peak %.1f stale-config errors/s", stale),
			Detail:   "Chunks were moving while work was running. Each error is an operation that ran, was told its routing was out of date, refreshed and ran again."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: "The routing table was stable"}
	}
	return c
}

// fdChartMigrations — chunk migrations, from the shard's own side.
func fdChartMigrations(d *ftdcData) *fdChart {
	const pre = "serverStatus.shardingStatistics."
	c := &fdChart{ID: "migrations", Group: "Sharding", Title: "Chunk migrations", Unit: "count",
		Why: "Migrations this shard sent and received, cumulatively. Started against committed is the pair to read: a balancer that keeps starting migrations and aborting them is doing all of the work and none of the good, and it will keep trying. Aborts usually mean the migration collided with something — an index build, a long-running write, or a critical section it could not enter."}
	for _, p := range []struct{ name, key string }{
		{"sent (started)", pre + "countDonorMoveChunkStarted"},
		{"sent (committed)", pre + "countDonorMoveChunkCommitted"},
		{"sent (aborted)", pre + "countDonorMoveChunkAborted"},
		{"received (started)", pre + "countRecipientMoveChunkStarted"},
	} {
		if pts := fdFloats(d, p.key, 1); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	started := fdMax(fdFloats(d, pre+"countDonorMoveChunkStarted", 1))
	aborted := fdMax(fdFloats(d, pre+"countDonorMoveChunkAborted", 1))
	switch {
	case started > 0 && aborted >= started/2:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%.0f of %.0f migrations sent from this shard were aborted", aborted, started),
			Detail:   "The balancer is spending its time on migrations that do not finish. The data does not move and the work is paid anyway.",
			Action:   "Aborts collide with something holding the collection — an index build, a long transaction, or a write that would not yield. The balancer window exists for exactly this: run it when the collection is quiet."}
	default:
		c.Advice = &fdAdvice{Level: "info",
			Headline: fmt.Sprintf("%.0f migration(s) sent, %.0f received", started, fdMax(fdFloats(d, pre+"countRecipientMoveChunkStarted", 1)))}
	}
	return c
}

// fdChartCriticalSection — how long writes were blocked so a migration could commit.
//
// The part of a chunk migration nobody expects. Copying the documents is online; committing
// is not. At the end of every migration the donor takes a critical section on that chunk's
// range and writes to it BLOCK until the config servers have acknowledged the new owner. It
// is short when everything is healthy and unbounded when the config servers are not.
func fdChartCriticalSection(d *ftdcData) *fdChart {
	const pre = "serverStatus.shardingStatistics."
	total := fdFloats(d, pre+"totalCriticalSectionTimeMillis", 1)
	if total == nil {
		return nil
	}
	c := &fdChart{ID: "criticalSection", Group: "Sharding", Title: "Writes blocked for migration commit", Unit: "ms",
		Why: "Copying a chunk is online. Committing it is not: at the end of every migration, writes to that range block until the config servers confirm the new owner. Normally milliseconds. If the config replica set has no primary it is however long that lasts, and the symptom is writes to one range of one collection hanging while everything else is fine."}
	c.Series = append(c.Series, fdSeries{Name: "total time in critical section", Points: total})
	if commit := fdFloats(d, pre+"totalCriticalSectionCommitTimeMillis", 1); commit != nil && fdMax(commit) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "of which waiting for the commit", Points: commit})
	}
	// Per migration, which is what a write to that range actually waited.
	per := 0.0
	if n := fdMax(fdFloats(d, pre+"countDonorMoveChunkCommitted", 1)); n > 0 {
		per = fdMax(total) / n
	}
	switch {
	case per >= 1000:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Around %.0f ms of blocked writes per migration", per),
			Detail:   "Every write to the range being moved waited that long, and the application has no way to tell that from the database being down.",
			Action:   "A long critical section is nearly always the config servers being slow to acknowledge. Check their replica set — commit lag and write concern on the config members, not on the shard."}
	case fdMax(total) > 0:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("%.0f ms of blocked writes in total, about %.0f ms per migration", fdMax(total), per)}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: "No migration blocked a write in this window"}
	}
	return c
}

// fdChartRangeDeleter — the orphans left behind after a migration.
//
// When a chunk moves, the donor's copy is not deleted with it: it is queued for the range
// deleter and removed afterwards. Until then those documents are still on disk, and a
// backlog that never drains is a shard whose disk usage does not fall after a rebalance and
// whose queries pay for documents that belong to somebody else.
func fdChartRangeDeleter(d *ftdcData) *fdChart {
	const pre = "serverStatus.shardingStatistics."
	c := &fdChart{ID: "rangeDeleter", Group: "Sharding", Title: "Orphan cleanup after migrations", Unit: "docs/s · tasks",
		Why: "A migrated chunk's documents are not removed from the donor as it moves — they are queued and deleted afterwards. The queue is the number to watch: while it is not empty the donor still holds data it has given away, so its disk does not shrink and its queries still read past the leftovers."}
	if pts := fdRate(d, pre+"countDocsDeletedByRangeDeleter"); pts != nil && fdMax(pts) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "documents deleted/s", Points: pts})
	}
	if pts := fdFloats(d, pre+"rangeDeleterTasks", 1); pts != nil && fdMax(pts) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "cleanup tasks queued", Points: pts})
	}
	for _, p := range []struct{ name, key string }{
		{"documents cloned out/s", pre + "countDocsClonedOnDonor"},
		{"documents cloned in/s", pre + "countDocsClonedOnRecipient"},
	} {
		if pts := fdRate(d, p.key); pts != nil && fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	queued := fdMax(fdFloats(d, pre+"rangeDeleterTasks", 1))
	if queued > 0 && fdMin(fdFloats(d, pre+"rangeDeleterTasks", 1)) > 0 {
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Orphan cleanup never emptied — up to %.0f task(s) queued throughout", queued),
			Detail:   "This shard held documents it had already given to another shard for the whole window. Disk usage does not fall until the queue drains, and until then a collection scan reads them.",
			Action:   "Cleanup yields to live traffic by design, so a queue that never empties usually means the shard has no idle time. cleanupOrphaned reports what is left."}
	} else {
		c.Advice = &fdAdvice{Level: "ok", Headline: fmt.Sprintf("Orphan cleanup kept up (peak %.0f task(s) queued)", queued)}
	}
	return c
}

// fdChartRouterPool — the router's connections to the shards, which is a different pool from
// the client connections on the Work chart.
func fdChartRouterPool(d *ftdcData) *fdChart {
	inUse := fdFloats(d, "connPoolStats.totalInUse", 1)
	if inUse == nil {
		return nil
	}
	c := &fdChart{ID: "routerPool", Group: "Sharding", Title: "Router connections to the shards", Unit: "conns · per second",
		Why: "The pool the router keeps OUT to the shards, which is not the pool of clients coming in. Connections being created continuously rather than reused is the shape to look for: it means the pool is being torn down and rebuilt, usually because a shard keeps changing primary or dropping connections, and every rebuild costs a handshake and an authentication."}
	c.Series = append(c.Series, fdSeries{Name: "in use", Points: inUse})
	if pts := fdFloats(d, "connPoolStats.totalAvailable", 1); pts != nil {
		c.Series = append(c.Series, fdSeries{Name: "available", Points: pts})
	}
	if pts := fdRate(d, "connPoolStats.totalCreated"); pts != nil && fdMax(pts) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "created/s", Points: pts})
	}
	if pts := fdFloats(d, "connPoolStats.totalRefreshing", 1); pts != nil && fdMax(pts) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "refreshing", Points: pts})
	}
	// Sustained rather than peak. A pool rebuilds in a burst after any shard changes
	// primary, and calling that burst a leak would fire on every healthy failover; what is
	// worth reporting is a pool that never stops rebuilding.
	span := 1.0
	if sp := d.span().Seconds(); sp > 0 {
		span = sp
	}
	made := fdFloats(d, "connPoolStats.totalCreated", 1)
	sustained := (fdMax(made) - fdMin(made)) / span
	if sustained >= 2 {
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%s connections/s to the shards, sustained across the window", fdAmt(sustained)),
			Detail:   "A healthy pool creates its connections once and reuses them. This one kept rebuilding, and every rebuild is a TCP handshake plus an authentication before any work happens. A shard that keeps changing primary does this, and so does a pool sized below what the workload needs."}
	} else {
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Peak %.0f connections in use to the shards, %s/s created", fdMax(inUse), fdAmt(sustained))}
	}
	return c
}

// ---------------------------------------------------------------- capture header

// fdServerFacts turns the type-0 metadata document into the header this page shows above
// the charts.
//
// The ordering is the order somebody reads a stranger's capture in: what is it, what is it
// running on, how is it configured, and what is it part of. Anything absent is simply left
// out rather than rendered as an em dash — a capture from a build that does not record a
// field should not look like a capture from a server that has none.
func fdServerFacts(d *ftdcData) []fdFact {
	get := func(k string) string { return d.Meta[k] }
	add := func(out []fdFact, label, key, note string) []fdFact {
		if v := get(key); v != "" {
			return append(out, fdFact{Label: label, Value: v, Note: note})
		}
		return out
	}
	var out []fdFact
	// What it is.
	if v := get("psmdbVersion"); v != "" {
		out = append(out, fdFact{Label: "Server", Value: "Percona Server for MongoDB " + v})
	} else {
		out = add(out, "Server", "version", "")
	}
	out = add(out, "Build", "gitVersion", "")
	out = add(out, "Allocator", "allocator", "")
	out = add(out, "OpenSSL", "openssl", "")
	// What it runs on.
	out = add(out, "Host", "host", "")
	out = add(out, "OS", "os", "")
	out = add(out, "Kernel", "kernel", "")
	out = add(out, "CPU", "cpu", "")
	if c, avail := get("cores"), get("coresAvailable"); c != "" {
		v := c + " cores"
		note := ""
		if avail != "" && avail != c {
			v = avail + " of " + c + " cores"
			note = "the process is restricted to fewer cores than the machine has — every per-core rate on this page is against the smaller number"
		}
		out = append(out, fdFact{Label: "Cores", Value: v, Note: note})
	}
	out = add(out, "Memory", "memSizeMB", get("memNote"))
	if lim := get("memLimitMB"); lim != "" && lim != get("memSizeMB") {
		out = append(out, fdFact{Label: "Memory limit", Value: lim,
			Note: "mongod sizes its cache from this, not from the machine's total"})
	}
	if thp := get("thp"); thp == "always" {
		out = append(out, fdFact{Label: "Transparent huge pages", Value: thp,
			Note: "MongoDB asks for this to be off; left on it inflates resident memory and lengthens allocation stalls"})
	} else {
		out = add(out, "Transparent huge pages", "thp", "")
	}
	out = add(out, "NUMA", "numa", "")
	// How it is configured.
	out = add(out, "Replica set", "replSet", "")
	out = add(out, "Cluster role", "clusterRole", "")
	out = add(out, "Port", "port", "")
	out = add(out, "dbPath", "dbPath", "")
	out = add(out, "Log", "logPath", "the file to read beside this capture; the two share a clock")
	if cache := get("cacheSizeGB"); cache != "" {
		out = append(out, fdFact{Label: "Cache configured", Value: cache,
			Note: "pinned in the config rather than derived from memory"})
	}
	if auth := get("authorization"); auth != "" && auth != "enabled" {
		out = append(out, fdFact{Label: "Authorization", Value: auth,
			Note: "this server accepted unauthenticated connections"})
	} else {
		out = add(out, "Authorization", "authorization", "")
	}
	out = add(out, "Key file", "keyFile", "")
	if fd := get("fileDescriptors"); fd != "" {
		note := ""
		if n, err := strconv.Atoi(fd); err == nil && n < 64000 {
			note = "below MongoDB's recommended 64000 — connection storms hit this ceiling before they hit any limit in the server"
		}
		out = append(out, fdFact{Label: "File descriptors", Value: fd, Note: note})
	}
	out = add(out, "Default read concern", "defaultReadConcern", "")
	out = add(out, "Default write concern", "defaultWriteConcern", "")
	return out
}

// ---------------------------------------------------------------- charts added after the
// anatomy pass (see the FTDC anatomy report): five questions the file could always answer
// and this page could not.

// fdChartOplogWindow — how far back the oplog reaches, in time.
//
// The oplog chart above draws size against the configured maximum, which answers "is it
// full". On a healthy server the answer is always yes: the oplog is a ring buffer and it is
// supposed to be full. The question people actually ask is how long a member can be down
// before it needs an initial sync, and that is a duration, not a size — two timestamps in
// every sample, subtracted.
//
// A window that shrinks under write load is the specific failure this catches: the same
// 50 GiB oplog that held twelve hours yesterday holds forty minutes during a bulk load, and
// nothing about its size changed.
func fdChartOplogWindow(d *ftdcData) *fdChart {
	early := fdFloats(d, "serverStatus.oplog.earliestOptime", 1)
	late := fdFloats(d, "serverStatus.oplog.latestOptime", 1)
	if early == nil || late == nil {
		return nil
	}
	n := len(early)
	if len(late) < n {
		n = len(late)
	}
	mins := make([]float64, n)
	for i := 0; i < n; i++ {
		// Both are BSON timestamps stored as epoch seconds. A zero on either side is a
		// sample taken before the member had an oplog at all — startup, or a member that
		// has not finished initial sync — and a window computed from it is a fiction.
		if early[i] <= 0 || late[i] <= 0 || late[i] < early[i] {
			continue
		}
		mins[i] = (late[i] - early[i]) / 60
	}
	if fdMax(mins) == 0 {
		return nil
	}
	// The narrowest the window got — but not while it was still FILLING. A fresh oplog
	// grows from zero to its retention for as long as it takes to wrap for the first
	// time, and reporting that ramp as "the window fell to 0 minutes" is a false alarm on
	// every newly built member. Skip the leading run that only ever increases; what
	// matters is the first time it went DOWN and everything after.
	first := 0
	for i := 1; i < n; i++ {
		if mins[i] > 0 && mins[i] < mins[i-1] {
			first = i
			break
		}
	}
	filling := first == 0
	lowest, lowestSet := 0.0, false
	for i := first; i < n; i++ {
		if mins[i] <= 0 {
			continue
		}
		if !lowestSet || mins[i] < lowest {
			lowest, lowestSet = mins[i], true
		}
	}
	if !lowestSet {
		return nil
	}
	c := &fdChart{ID: "oplogWindow", Group: "Replication", Title: "Oplog window", Unit: "minutes",
		Why: "How much history the oplog still holds, in time: the newest entry's timestamp minus the oldest. This is the real recovery budget — a member down for longer than this needs a full initial sync rather than catching up — and it shrinks as write volume rises even though the oplog's size never changes."}
	c.Series = append(c.Series, fdSeries{Name: "window", Points: mins})
	last := mins[n-1]
	switch {
	case filling:
		// It only ever grew, so there is no narrowest yet — the honest reading is what it
		// reaches, plus the fact that this member has not been running long enough to
		// have wrapped its oplog even once.
		c.Advice = &fdAdvice{Level: "info",
			Headline: fmt.Sprintf("The oplog window grew to %.0f minutes and has not wrapped yet", last),
			Detail:   "It has only ever got longer during this capture, so the retention this server settles at is not visible here — it is still filling for the first time."}
	case lowest < 20:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("The oplog window fell to %.0f minutes", lowest),
			Detail:   "Any member unreachable for longer than that cannot catch up from the oplog. It has to be resynced from scratch, which on a large dataset is hours of copying and a lot of extra load on whichever member it syncs from.",
			Action:   "Either raise the oplog size (it can be changed on a running member with replSetResizeOplog) or reduce the write rate. The window is the number to watch afterwards — not the size."}
	case lowest < 60:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("The oplog window dipped to %.0f minutes (now %.0f)", lowest, last),
			Detail:   "That is the longest a member can be down — a restart, a patch, a slow disk — and still rejoin without an initial sync."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Oplog window %.0f minutes at its narrowest, %.0f at the end", lowest, last)}
	}
	return c
}

// fdChartWaiting — how much of an operation's life was spent working.
//
// 8.0 records two totals for the same operations: opLatencies, which is wall-clock from the
// moment the server took the request, and opWorkingTime, which excludes the time it spent
// waiting to be admitted or blocked behind something else. Neither is interesting alone.
// The GAP between them is queueing, and it is the difference between "the work is expensive"
// and "the server would not let it start" — a distinction that otherwise takes a slow-query
// line per operation to make, and cannot be made at all for the operations under slowms.
//
// Both are cumulative microseconds over a cumulative operation count, so each line is a
// ratio of two deltas (fdRatio), not a metric.
func fdChartWaiting(d *ftdcData) *fdChart {
	type pair struct{ name, lat, work string }
	pairs := []pair{
		{"reads", "serverStatus.opLatencies.reads", "serverStatus.opWorkingTime.reads"},
		{"writes", "serverStatus.opLatencies.writes", "serverStatus.opWorkingTime.writes"},
		{"commands", "serverStatus.opLatencies.commands", "serverStatus.opWorkingTime.commands"},
	}
	c := &fdChart{ID: "waiting", Group: "Work", Title: "Time operations spent waiting", Unit: "ms per operation",
		Why: "The part of each operation's life the server was NOT working on it — total latency minus working time, per operation. Waiting for an execution ticket, for a lock, for something else to finish. The latency chart above shows the whole cost; this shows the part of it no amount of query tuning will remove. It exists for every operation, not only the ones that crossed slowms and got a log line."}
	worstGap, worstName := 0.0, ""
	for _, p := range pairs {
		if d.Series[p.work+".latency"] == nil {
			continue // pre-8.0 capture: opWorkingTime does not exist
		}
		lat := fdRatio(d, p.lat+".latency", p.lat+".ops", 0.001)
		work := fdRatio(d, p.work+".latency", p.lat+".ops", 0.001)
		if lat == nil || work == nil || fdMax(lat) == 0 {
			continue
		}
		wait := make([]float64, len(lat))
		for i := range lat {
			if i < len(work) && lat[i] > work[i] {
				wait[i] = lat[i] - work[i]
			}
		}
		if fdMax(wait) > 0 {
			c.Series = append(c.Series, fdSeries{Name: p.name, Points: wait})
		}
		// The share is taken over the window from the raw totals, not from the peaks:
		// dividing one peak by another invents a ratio that never happened.
		latTot := fdMax(fdFloats(d, p.lat+".latency", 1)) - fdMin(fdFloats(d, p.lat+".latency", 1))
		workTot := fdMax(fdFloats(d, p.work+".latency", 1)) - fdMin(fdFloats(d, p.work+".latency", 1))
		if latTot > 0 {
			if gap := (latTot - workTot) / latTot * 100; gap > worstGap {
				worstGap, worstName = gap, p.name
			}
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	worstMs := 0.0
	for _, ser := range c.Series {
		if m := fdMax(ser.Points); m > worstMs {
			worstMs = m
		}
	}
	switch {
	case worstGap >= 40:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%.0f%% of %s time was spent waiting, not working", worstGap, worstName),
			Detail:   "The server had these operations for far longer than it worked on them. Time like this is invisible to query tuning: the query is not slow, it is queued.",
			Action:   "The tickets and queue charts say what it was queued behind. If tickets sit at zero the limit is admission control; if they do not, look for a lock or a long-running operation holding one."}
	case worstGap >= 15:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%.0f%% of %s time was waiting rather than working (peak %s ms per operation)", worstGap, worstName, fdAmt(worstMs))}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Operations spent %s of their time working; waiting peaked at %s ms", fdPctV(100-worstGap), fdAmt(worstMs))}
	}
	return c
}

// fdChartAdmission — what admission control actually cost, and how it kept resizing itself.
//
// The tickets chart shows how many are left; zero left looks identical whether the wait for
// one was a microsecond or a second. This is the wait: total queued microseconds over the
// number of admissions granted, per second of capture. 8.0 also RESIZES the ticket pool
// while it runs, and the number of times it did is the clearest signal that execution
// control was fighting the workload rather than sitting at a limit.
func fdChartAdmission(d *ftdcData) *fdChart {
	const pre = "serverStatus.queues.execution."
	c := &fdChart{ID: "admission", Group: "Storage engine", Title: "Time waiting for an execution ticket", Unit: "ms per admission",
		Why: "How long an operation waited to be let in, averaged over the operations let in during that interval. A ticket count of zero says the pool is exhausted; this says what that cost. 8.0 resizes the pool as it goes, so the two together are how you tell a hard limit from a moving one."}
	worst := 0.0
	for _, side := range []string{"read", "write"} {
		ms := fdRatio(d, pre+side+".normalPriority.totalTimeQueuedMicros",
			pre+side+".normalPriority.finishedProcessing", 0.001)
		if ms == nil || fdMax(ms) == 0 {
			continue
		}
		c.Series = append(c.Series, fdSeries{Name: side + " queued", Points: ms})
		if m := fdMax(ms); m > worst {
			worst = m
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	up := fdMax(fdFloats(d, pre+"monitor.timesIncreased", 1)) - fdMin(fdFloats(d, pre+"monitor.timesIncreased", 1))
	down := fdMax(fdFloats(d, pre+"monitor.timesDecreased", 1)) - fdMin(fdFloats(d, pre+"monitor.timesDecreased", 1))
	resizes := up + down
	switch {
	case worst >= 100:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Operations waited up to %.0f ms just to be admitted", worst),
			Detail:   fmt.Sprintf("Admission control resized the pool %.0f times during this capture (%.0f up, %.0f down), which is it trying to find a size that works and not finding one.", resizes, up, down),
			Action:   "This is a symptom of the storage engine being the limit, not a setting to raise. The cache and disk charts say which."}
	case worst >= 10:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Up to %.1f ms waiting for a ticket; the pool was resized %.0f times", worst, resizes)}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Admission cost at most %s ms per operation", fdAmt(worst))}
	}
	return c
}

// fdChartTCP — the network underneath the replica set, from the host's own counters.
//
// A replica set is a distributed system on TCP, and mongod captures /proc/net/snmp every
// second along with everything else. Retransmits, refused connects and resets are the
// difference between "the peer was slow" and "the network dropped it", and none of them
// appear in the server log at any verbosity: a heartbeat failure is logged, the reason is
// not.
func fdChartTCP(d *ftdcData) *fdChart {
	const pre = "systemMetrics.netstat."
	c := &fdChart{ID: "tcp", Group: "Host", Title: "TCP trouble", Unit: "segments/s · events/s",
		Why: "Retransmitted segments, failed connection attempts and resets, straight from the host's network counters. A member that logs heartbeat failures with a clean line here has a slow peer; one with retransmits climbing has a network."}
	for _, m := range []struct{ name, key string }{
		{"retransmitted segments", pre + "Tcp:RetransSegs"},
		{"failed connects", pre + "Tcp:AttemptFails"},
		{"connections reset", pre + "Tcp:EstabResets"},
		{"listen queue overflows", pre + "TcpExt:ListenOverflows"},
	} {
		if r := fdRate(d, m.key); r != nil && fdMax(r) > 0 {
			c.Series = append(c.Series, fdSeries{Name: m.name, Points: r})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	retrans := fdFloats(d, pre+"Tcp:RetransSegs", 1)
	out := fdFloats(d, pre+"Tcp:OutSegs", 1)
	sent := fdMax(out) - fdMin(out)
	lost := fdMax(retrans) - fdMin(retrans)
	share := 0.0
	if sent > 0 {
		share = lost / sent * 100
	}
	overflow := fdMax(fdFloats(d, pre+"TcpExt:ListenOverflows", 1)) - fdMin(fdFloats(d, pre+"TcpExt:ListenOverflows", 1))
	switch {
	case overflow > 0:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%.0f connection(s) were dropped by a full listen queue", overflow),
			Detail:   "The server never saw them. To the client this is a connection timeout with nothing whatsoever in the mongod log, because the kernel refused it before mongod was involved.",
			Action:   "A connection storm, or net.listenBacklog set below what the client pool asks for. The connections chart shows which."}
	case share >= 1:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%s of segments sent were retransmitted", fdPctV(share)),
			Detail:   "Above about 1% the network is losing packets in a way that shows up as latency everywhere at once — heartbeats, replication and client traffic all pay it.",
			Action:   "This is not a MongoDB problem to tune. Take it to whoever owns the path between these hosts."}
	case share >= 0.1:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%.0f retransmitted segments (%s of traffic)", lost, fdPctV(share)),
			Detail:   "Not enough to explain a stall on its own, but it is loss, and loss on a replica-set link shows up as heartbeat and replication latency before it shows up anywhere else."}
	case lost > 0:
		// A few retransmits over an hour is a working network, not a finding. Say the
		// number so it is not hidden, and do not colour it as a problem.
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("%.0f retransmitted segments — %s of traffic", lost, fdPctV(share))}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: "No retransmits, refused connects or resets"}
	}
	return c
}

// fdChartProcessPressure — how much of the time THIS process was stalled.
//
// The host PSI chart above measures the machine. These are the same counters scoped to
// mongod's own cgroup, and on a shared machine — or in a container — they are the ones that
// answer "was it us". A member that is stalled 30% of the time on I/O while the host looks
// half idle is a member competing with something else for the disk.
func fdChartProcessPressure(d *ftdcData) *fdChart {
	const pre = "serverStatus.extra_info.pressure."
	c := &fdChart{ID: "processPressure", Group: "Host", Title: "Pressure on the mongod process", Unit: "% of time stalled",
		Why: "Linux pressure-stall information for mongod's own cgroup rather than for the machine: the share of each second at least one of its threads spent waiting for I/O, CPU or memory. Where this is high and the host chart is not, the contention is inside this container."}
	for _, m := range []struct{ name, key string }{
		{"I/O (some)", pre + "io.some.totalMicros"},
		{"I/O (full)", pre + "io.full.totalMicros"},
		{"CPU (some)", pre + "cpu.some.totalMicros"},
		{"memory (full)", pre + "memory.full.totalMicros"},
	} {
		// totalMicros per second of wall clock, as a percentage.
		if r := fdRate(d, m.key); r != nil && fdMax(r) > 0 {
			pct := make([]float64, len(r))
			for i, v := range r {
				pct[i] = v / 10000
			}
			c.Series = append(c.Series, fdSeries{Name: m.name, Points: pct})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	io := fdFloats(d, pre+"io.some.totalMicros", 1)
	stalled := (fdMax(io) - fdMin(io)) / 1e6
	span := d.span().Seconds()
	share := 0.0
	if span > 0 {
		share = stalled / span * 100
	}
	faults := fdMax(fdFloats(d, "serverStatus.extra_info.page_faults", 1)) -
		fdMin(fdFloats(d, "serverStatus.extra_info.page_faults", 1))
	switch {
	case share >= 20:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("mongod was stalled on I/O for %s of the capture", fdPctV(share)),
			Detail:   fmt.Sprintf("%.0f seconds of the %.0f in this window, and %.0f major page fault(s) alongside it. Every one of those is a thread that had work to do and was waiting for a device.", stalled, span, faults),
			Action:   "Compare with the device charts. If the device is not busy, something else in this cgroup is taking the I/O budget; if it is, the working set does not fit and the cache chart will say so too."}
	case share >= 5:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%s of the capture was spent stalled on I/O", fdPctV(share))}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Process stalled on I/O for %s of the window", fdPctV(share))}
	}
	return c
}

// ---------------------------------------------------------------- second pass
//
// Nine more, from the same anatomy work: the questions the capture could answer and the
// page still could not. Each one was written against a real 8.0.28-12 capture and reads
// metrics verified to exist in it — see ftdc_charts_test.go, which draws them from a
// fixture rather than from a hand-built series.

// fdChartIndexBuild — a build that takes minutes, and what it spilled doing it.
//
// An index build on a large collection is a scheduled event even when nobody scheduled it:
// it scans the collection end to end, sorts every key, spills the sort to disk when it will
// not fit in memory, and competes with the workload for the same cache and the same device
// throughout. The log narrates it in seven lines; the shape between them is here.
func fdChartIndexBuild(d *ftdcData) *fdChart {
	spill := fdFloats(d, "serverStatus.indexBulkBuilder.bytesSpilled", 1/1048576.0)
	mem := fdFloats(d, "serverStatus.indexBulkBuilder.memUsage", 1/1048576.0)
	if fdMax(spill) == 0 && fdMax(mem) == 0 {
		return nil
	}
	c := &fdChart{ID: "indexBuild", Group: "Storage engine", Title: "Index build: the external sort", Unit: "MiB",
		Why: "What a running index build is doing to the machine. Keys are sorted in memory until they will not fit and then spilled to disk in ranges, which is read back and merged at the end — so a build that spills is doing two passes over the device on top of the collection scan itself."}
	if fdMax(mem) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "sort memory in use", Points: mem})
	}
	if fdMax(spill) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "spilled to disk", Points: spill})
	}
	if len(c.Series) == 0 {
		return nil
	}
	// How long the sort was actually working, measured from the series rather than from the
	// phase metrics: indexBuilds.phases.* are CUMULATIVE COUNTERS of how many builds passed
	// through each phase, not gauges saying which phase is running now. Read as gauges they
	// reported "1.0 h scanning the collection" for an 8-minute build, because a counter
	// stays where it finished.
	// Elapsed between the first and last time the sort counter moved. NOT the sum of the
	// samples in which it moved: the bulk builder publishes numSorted once per spilled
	// range, so a build that sorted for eight minutes across fourteen ranges shows
	// fourteen one-second increments, and adding those up reports "14 s of sorting".
	busy := 0.0
	if sr := d.Series["serverStatus.indexBulkBuilder.numSorted"]; sr != nil {
		first, last := -1, -1
		for i := 1; i < len(sr.Values) && i < len(d.TS); i++ {
			if sr.Values[i] > sr.Values[i-1] {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		if first >= 0 && last > first {
			busy = d.TS[last] - d.TS[first]
		}
	}
	sorted := fdMax(fdFloats(d, "serverStatus.indexBulkBuilder.numSorted", 1)) -
		fdMin(fdFloats(d, "serverStatus.indexBulkBuilder.numSorted", 1))
	ranges := fdMax(fdFloats(d, "serverStatus.indexBulkBuilder.spilledRanges", 1))
	builds := fdMax(fdFloats(d, "serverStatus.indexBuilds.phases.scanCollection", 1)) -
		fdMin(fdFloats(d, "serverStatus.indexBuilds.phases.scanCollection", 1))
	spilled := fdMax(spill)
	switch {
	case spilled >= 64:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("A build spilled %s MiB to disk across %s range(s)", fdAmt(spilled), fdCount(ranges)),
			Detail: fmt.Sprintf("%s keys sorted, over a build that ran for %s. Everything spilled is written once and read back once to be merged, on the device the database is already using.",
				fdCount(sorted), fdDur(busy)),
			Action: "maxIndexBuildMemoryUsageMegabytes decides whether a build spills at all. Raising it takes memory the cache would otherwise have; building when the server is quiet takes nothing."}
	case sorted > 0:
		c.Advice = &fdAdvice{Level: "info",
			Headline: fmt.Sprintf("%s index build(s): %s keys sorted in memory over %s, nothing spilled to disk", fdCount(builds), fdCount(sorted), fdDur(busy))}
	default:
		c.Advice = &fdAdvice{Level: "info", Headline: fmt.Sprintf("Bulk index building peaked at %s MiB of sort memory", fdAmt(fdMax(mem)))}
	}
	return c
}

// fdChartOplogCache — how much of the cache the oplog is holding.
//
// The oplog is a collection like any other and it competes for the same cache. On a
// write-heavy server it can hold a third of it — cache the working set needed — and nothing
// on this page said so, because the oplog chart measures the oplog's SIZE on disk and the
// cache chart measures the cache as a whole.
func fdChartOplogCache(d *ftdcData) *fdChart {
	const oplog = "local.oplog.rs.stats.storageStats.wiredTiger.cache.bytes currently in the cache"
	inCache := fdFloats(d, oplog, 1/1048576.0)
	if fdMax(inCache) == 0 {
		return nil
	}
	c := &fdChart{ID: "oplogCache", Group: "Storage engine", Title: "The oplog's share of the cache", Unit: "MiB",
		Why: "The oplog is a collection and it lives in the same cache as the data. Every write goes into it, so on a write-heavy server it can hold a large part of the cache — and that part is not available to the working set, however the cache chart looks."}
	c.Series = append(c.Series, fdSeries{Name: "oplog in cache", Points: inCache})
	if used := fdFloats(d, "serverStatus.wiredTiger.cache.bytes currently in the cache", 1/1048576.0); fdMax(used) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "whole cache in use", Points: used})
	}
	if maxb := fdFloats(d, "serverStatus.wiredTiger.cache.maximum bytes configured", 1/1048576.0); fdMax(maxb) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "cache configured", Points: maxb, Dashed: true})
	}
	// Share per sample, not peak against peak: wiredTigerEngineRuntimeConfig can resize the
	// cache while the server runs (the capture this was written against does exactly that,
	// 14.19 GiB → 768 MiB), and dividing the largest oplog footprint by the largest cache
	// the capture ever saw reported a fifth of the cache as under two per cent of it.
	conf := fdFloats(d, "serverStatus.wiredTiger.cache.maximum bytes configured", 1/1048576.0)
	peak, share := 0.0, 0.0
	for i := range inCache {
		if inCache[i] > peak {
			peak = inCache[i]
		}
		if i < len(conf) && conf[i] > 0 {
			if sh := inCache[i] / conf[i] * 100; sh > share {
				share = sh
			}
		}
	}
	switch {
	case share >= 25:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("The oplog held %s of the cache at its peak (%s MiB)", fdPctV(share), fdAmt(peak)),
			Detail:   "That is cache the working set did not get. It is the normal consequence of a heavy write rate rather than a misconfiguration, but it changes what 'the cache is full' means on the charts above.",
			Action:   "Either the cache is too small for the combined working set and write rate, or the write rate is higher than this server was sized for. The eviction and read-into-cache charts say which of the two is hurting."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("The oplog held at most %s MiB of cache (%s of it)", fdAmt(peak), fdPctV(share))}
	}
	return c
}

// fdChartCatchUp — what happened after an election, which is when the outage actually ended.
//
// An election is over in a second; the new primary then applies everything the old one had
// already replicated before it accepts a write. That catch-up is the part the application
// experiences, and these counters are the only record of it — they survive the event, which
// is what makes them worth charting rather than reading live.
func fdChartCatchUp(d *ftdcData) *fdChart {
	const pre = "serverStatus.electionMetrics."
	c := &fdChart{ID: "catchUp", Group: "Replication", Title: "Catch-up after an election", Unit: "count",
		Why: "After winning an election a member applies whatever the old primary had already replicated before it starts accepting writes. These counters say how often that was needed, how often it finished, and how often it was skipped or abandoned — the difference between an election the application barely noticed and one it did."}
	for _, m := range []struct{ name, key string }{
		{"catch-ups attempted", pre + "numCatchUps"},
		{"succeeded", pre + "numCatchUpsSucceeded"},
		{"already caught up", pre + "numCatchUpsAlreadyCaughtUp"},
		{"skipped", pre + "numCatchUpsSkipped"},
		{"failed with error", pre + "numCatchUpsFailedWithError"},
		{"failed with a new term", pre + "numCatchUpsFailedWithNewTerm"},
	} {
		pts := fdFloats(d, m.key, 1)
		if fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: m.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	total := fdMax(fdFloats(d, pre+"numCatchUps", 1))
	failed := fdMax(fdFloats(d, pre+"numCatchUpsFailedWithError", 1)) + fdMax(fdFloats(d, pre+"numCatchUpsFailedWithNewTerm", 1))
	avgOps := fdMax(fdFloats(d, pre+"averageCatchUpOps", 1))
	switch {
	case failed > 0:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%.0f catch-up(s) did not finish", failed),
			Detail:   "A catch-up that fails means the new primary gave up applying what the old one had replicated. Anything it had not applied is rolled back when the old primary rejoins — acknowledged writes, discarded.",
			Action:   "The rollback record on the other member says how much went. If this repeats, catchUpTimeoutMillis is too short for how far behind the secondaries run."}
	case total > 0:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%.0f catch-up(s), averaging %s operation(s) to replay", total, fdAmt(avgOps)),
			Detail:   "The set was without a writable primary for the election plus this replay. On the application's clock those are one outage, not two."}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: "No catch-up was needed after any election here"}
	}
	return c
}

// fdChartDiskSpace — free space per filesystem, and where it is heading.
//
// Six metrics that predict the one outage nobody debugs twice. mongod records the free and
// available bytes of every filesystem it can see, once a second, and a slope over an hour
// is a date.
func fdChartDiskSpace(d *ftdcData) *fdChart {
	// One series per mount that actually moved, biggest first; a container sees a dozen
	// mounts and eleven of them are read-only.
	type mount struct {
		name string
		pts  []float64
	}
	var mounts []mount
	for key := range d.Series {
		rest, ok := strings.CutPrefix(key, "systemMetrics.mounts.")
		if !ok || !strings.HasSuffix(rest, ".free") {
			continue
		}
		name := strings.TrimSuffix(rest, ".free")
		pts := fdFloats(d, key, 1/(1024*1024*1024.0))
		if fdMax(pts) == 0 || fdMax(pts) == fdMin(pts) {
			continue // a mount whose free space never changed is not worth a line
		}
		mounts = append(mounts, mount{name, pts})
	}
	if len(mounts) == 0 {
		return nil
	}
	// A container sees the same filesystem several times: /, and then /etc/hosts,
	// /etc/hostname and /etc/resolv.conf, which are bind-mounted files ON it. They carry
	// identical free space, and charting four copies of one line — then naming the coming
	// outage after /etc/hosts — is worse than not charting it.
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].name) < len(mounts[j].name) })
	seen := map[string]bool{}
	uniq := mounts[:0]
	for _, m := range mounts {
		sig := fmt.Sprintf("%.0f|%.0f|%.0f", m.pts[0], m.pts[len(m.pts)-1], fdMin(m.pts))
		if seen[sig] {
			continue
		}
		seen[sig] = true
		uniq = append(uniq, m)
	}
	mounts = uniq
	sort.Slice(mounts, func(i, j int) bool { return fdMax(mounts[i].pts) > fdMax(mounts[j].pts) })
	if len(mounts) > 4 {
		mounts = mounts[:4]
	}
	c := &fdChart{ID: "diskSpace", Group: "Host", Title: "Free space", Unit: "GiB",
		Why: "Free space on every filesystem this process can see, sampled every second like everything else. A database that fills its disk stops in a way that is expensive to undo, and the slope here is the only warning that exists."}
	worstName, worstHours, worstFree := "", 0.0, 0.0
	for _, m := range mounts {
		c.Series = append(c.Series, fdSeries{Name: m.name, Points: m.pts})
		// Straight-line projection over the window. Deliberately naive and stated as
		// such: a trend is not a forecast, and the number people need is "hours, days or
		// months", which a straight line gets right often enough to be worth saying.
		first, last := m.pts[0], m.pts[len(m.pts)-1]
		span := d.span().Hours()
		if span <= 0 || last >= first {
			continue
		}
		rate := (first - last) / span // GiB per hour
		if rate <= 0 {
			continue
		}
		hours := last / rate
		if worstName == "" || hours < worstHours {
			worstName, worstHours, worstFree = m.name, hours, last
		}
	}
	switch {
	case worstName != "" && worstHours < 24:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("%s has %s GiB left — under a day at this rate", worstName, fdAmt(worstFree)),
			Detail:   "Straight-line projection from this window only. It is not a forecast, but the direction is measured rather than assumed.",
			Action:   "A mongod that runs out of space on its dbPath does not degrade, it stops — and on a replica set it takes its member out of the majority with it."}
	case worstName != "" && worstHours < 24*14:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%s is filling: %s GiB left, roughly %.0f day(s) at this rate", worstName, fdAmt(worstFree), worstHours/24)}
	default:
		free := fdMin(mounts[0].pts)
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("%s never fell below %s GiB free", mounts[0].name, fdAmt(free))}
	}
	return c
}

// fdChartDataHandles — the cost of having a great many collections.
//
// Every collection and every index is a WiredTiger data handle with a cursor cache behind
// it, and a server with thousands of them spends real time sweeping them. The failure it
// produces — memory that grows with the catalogue rather than with the data, and a sweep
// that never keeps up — has no other signal on this page.
func fdChartDataHandles(d *ftdcData) *fdChart {
	active := fdFloats(d, "serverStatus.wiredTiger.data-handle.connection data handles currently active", 1)
	if fdMax(active) == 0 {
		return nil
	}
	c := &fdChart{ID: "dataHandles", Group: "Storage engine", Title: "Data handles and the catalogue", Unit: "count",
		Why: "One data handle per collection and per index, each with its own cursor cache, all of them swept periodically. This is what 'too many collections' looks like from inside the storage engine, and it grows with the catalogue rather than with the data."}
	c.Series = append(c.Series, fdSeries{Name: "handles active", Points: active})
	for _, m := range []struct{ name, key string }{
		{"collections", "serverStatus.catalogStats.collections"},
		{"indexes", "serverStatus.indexStats.count"},
	} {
		if pts := fdFloats(d, m.key, 1); fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: m.name, Points: pts})
		}
	}
	peak := fdMax(active)
	swept := fdMax(fdFloats(d, "serverStatus.wiredTiger.data-handle.connection sweep dhandles closed", 1)) -
		fdMin(fdFloats(d, "serverStatus.wiredTiger.data-handle.connection sweep dhandles closed", 1))
	colls := fdMax(fdFloats(d, "serverStatus.catalogStats.collections", 1))
	switch {
	case peak >= 5000:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%s data handles open at the peak, across %s collection(s)", fdCount(peak), fdCount(colls)),
			Detail:   fmt.Sprintf("The sweep closed %s of them during this window. Each open handle costs memory outside the cache, and the sweep itself takes locks.", fdCount(swept)),
			Action:   "This is a schema question rather than a tuning one: a collection per customer or per day reaches these numbers quickly, and nothing in the server makes it cheap."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Peak %s data handles across %s collection(s)", fdCount(peak), fdCount(colls))}
	}
	return c
}

// fdChartFlowControl — the primary throttling itself so the secondaries can keep up.
//
// When the majority commit point falls too far behind, the primary takes flow-control
// tickets away from its own writers. To the application this is writes getting slower for
// no reason the primary's own charts explain — the reason is on another machine.
func fdChartFlowControl(d *ftdcData) *fdChart {
	locks := fdFloats(d, "serverStatus.flowControl.locksPerKiloOp", 1)
	if fdMax(locks) == 0 {
		return nil
	}
	c := &fdChart{ID: "flowControl", Group: "Replication", Title: "Flow control", Unit: "locks per 1000 ops",
		Why: "The primary's own brake. When the majority commit point lags, the primary makes its writers wait so the secondaries can catch up — deliberately slowing the application to keep the set consistent. It is invisible in every server-side latency measurement, because the wait happens before the operation starts."}
	c.Series = append(c.Series, fdSeries{Name: "flow-control locks", Points: locks})
	laggedFor := fdMax(fdFloats(d, "serverStatus.flowControl.isLaggedTimeMicros", 1)) -
		fdMin(fdFloats(d, "serverStatus.flowControl.isLaggedTimeMicros", 1))
	acquiring := fdMax(fdFloats(d, "serverStatus.flowControl.timeAcquiringMicros", 1)) -
		fdMin(fdFloats(d, "serverStatus.flowControl.timeAcquiringMicros", 1))
	engaged := fdMax(fdFloats(d, "serverStatus.flowControl.isLagged", 1)) > 0
	switch {
	case engaged && laggedFor >= 60e6:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Flow control engaged for %s", fdDur(laggedFor/1e6)),
			Detail:   fmt.Sprintf("Writers spent %s waiting for flow-control tickets. The primary was healthy and slow on purpose, because the majority could not keep up with it.", fdDur(acquiring/1e6)),
			Action:   "The fix is on the secondaries — their apply rate, their disks, or the network to them. Turning flow control off just moves the problem to replication lag."}
	case engaged || acquiring >= 5e6:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Flow control engaged for %s; writers waited %s in total", fdDur(laggedFor/1e6), fdDur(acquiring/1e6)),
			Detail:   "Brief, but it is the primary deliberately slowing itself because the majority was behind — the same mechanism that becomes an outage when the secondaries stay behind."}
	case acquiring > 0:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Flow control cost writers %s in total", fdDur(acquiring/1e6))}
	default:
		c.Advice = &fdAdvice{Level: "ok", Headline: "Flow control never had to slow the writers"}
	}
	return c
}

// fdChartSessionStore — the bookkeeping collections retryable writes leave behind.
//
// config.transactions and config.image_collection are written by the server, for the
// server, on every retryable write and every findAndModify with a stored pre-image. FTDC
// carries a full collStats for both — 400-odd metrics that nothing has ever charted — and
// the case they exist to catch is one of these growing until it is the working set.
func fdChartSessionStore(d *ftdcData) *fdChart {
	c := &fdChart{ID: "sessionStore", Group: "Work", Title: "Session and retryable-write storage", Unit: "MiB",
		Why: "The two collections the server keeps for itself: config.transactions records retryable writes so a retry is not applied twice, and config.image_collection stores pre-images for findAndModify. Both are written on the hot path and both are in the same cache as the data."}
	for _, m := range []struct{ name, key string }{
		{"config.transactions", "config.transactions.stats.storageStats.size"},
		{"config.transactions in cache", "config.transactions.stats.storageStats.wiredTiger.cache.bytes currently in the cache"},
		{"config.image_collection", "config.image_collection.stats.storageStats.size"},
		{"config.image_collection in cache", "config.image_collection.stats.storageStats.wiredTiger.cache.bytes currently in the cache"},
	} {
		if pts := fdFloats(d, m.key, 1/1048576.0); fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: m.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	docs := fdMax(fdFloats(d, "config.transactions.stats.storageStats.count", 1))
	biggest := 0.0
	for _, s := range c.Series {
		if m := fdMax(s.Points); m > biggest {
			biggest = m
		}
	}
	switch {
	case biggest >= 512:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Session bookkeeping reached %s MiB", fdAmt(biggest)),
			Detail:   fmt.Sprintf("%s session record(s). These collections are reaped in the background; when they grow like this the reaper is not keeping up with the rate of retryable writes.", fdCount(docs)),
			Action:   "transactionLifetimeLimitSeconds and the logical session timeout decide how long these records live. Until they expire they are cache and I/O the workload does not get."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Session bookkeeping stayed under %s MiB (%s records)", fdAmt(biggest), fdCount(docs))}
	}
	return c
}

// fdChartReadPreference — where the reads actually went.
//
// The counters split every read by the preference it carried and the member it ran on,
// which is the only way to check the sentence "we moved reads to the secondaries" against
// what the servers did. Internal reads (the members talking to each other) are counted
// separately from client ones, and mixing them would hide exactly the traffic in question.
func fdChartReadPreference(d *ftdcData) *fdChart {
	const pre = "serverStatus.readPreferenceCounters."
	c := &fdChart{ID: "readPreference", Group: "Work", Title: "Reads by preference and where they ran", Unit: "reads/s",
		Why: "Every read carries a read preference, and this counts them by the preference asked for and the member that served it. It is the difference between a secondary that is taking read traffic and one that is only replicating."}
	for _, m := range []struct{ name, match string }{
		{"on the primary, preference primary", pre + "executedOnPrimary.primary."},
		{"on the primary, other preferences", pre + "executedOnPrimary."},
		{"on a secondary", pre + "executedOnSecondary."},
	} {
		match := m.match
		pts := fdSumRates(d, func(k string) bool {
			if !strings.HasPrefix(k, match) {
				return false
			}
			// The middle series is "everything on the primary that was not preference
			// primary", so exclude that subtree from it rather than counting it twice.
			if match == pre+"executedOnPrimary." && strings.HasPrefix(k, pre+"executedOnPrimary.primary.") {
				return false
			}
			return true
		})
		if fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: m.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	onSec := fdSumRates(d, func(k string) bool { return strings.HasPrefix(k, pre+"executedOnSecondary.") })
	onPri := fdSumRates(d, func(k string) bool { return strings.HasPrefix(k, pre+"executedOnPrimary.") })
	secPeak, priPeak := fdMax(onSec), fdMax(onPri)
	switch {
	case priPeak > 0 && secPeak/priPeak < 0.01:
		c.Advice = &fdAdvice{Level: "info",
			Headline: fmt.Sprintf("Reads went almost entirely to the primary (peak %s/s against %s/s on secondaries)", fdAmt(priPeak), fdAmt(secPeak)),
			Detail:   "Which is the default and often correct. It is worth knowing when somebody believes otherwise: a secondaryPreferred connection string that was never picked up leaves exactly this shape."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Peak %s reads/s on the primary, %s/s on secondaries", fdAmt(priPeak), fdAmt(secPeak))}
	}
	return c
}

// fdChartHeap — the memory the allocator is holding that nothing is using.
//
// The memory chart draws resident and cache. This is the gap underneath it: tcmalloc's heap
// against what the application actually has allocated. A server whose resident memory keeps
// climbing while its cache does not is usually looking at this — memory the allocator has
// not returned, which counts against the container's limit exactly like memory in use.
func fdChartHeap(d *ftdcData) *fdChart {
	heap := fdFloats(d, "serverStatus.tcmalloc.generic.heap_size", 1/1048576.0)
	inUse := fdFloats(d, "serverStatus.tcmalloc.generic.bytes_in_use_by_app", 1/1048576.0)
	if fdMax(heap) == 0 || fdMax(inUse) == 0 {
		return nil
	}
	c := &fdChart{ID: "heap", Group: "Storage engine", Title: "Allocator heap against memory in use", Unit: "MiB",
		Why: "What tcmalloc has taken from the operating system, and how much of it the server is actually using. The gap between the lines is held by the allocator: not free to anything else, and counted in full against a container's memory limit."}
	c.Series = append(c.Series,
		fdSeries{Name: "heap", Points: heap},
		fdSeries{Name: "in use by the server", Points: inUse})
	if free := fdFloats(d, "serverStatus.tcmalloc.tcmalloc.pageheap_free_bytes", 1/1048576.0); fdMax(free) > 0 {
		c.Series = append(c.Series, fdSeries{Name: "free in the page heap", Points: free})
	}
	// The worst gap over the window, taken sample by sample rather than peak against peak:
	// the two peaks are rarely the same instant.
	worst, atHeap := 0.0, 0.0
	for i := range heap {
		if i >= len(inUse) || heap[i] <= 0 {
			continue
		}
		if g := (heap[i] - inUse[i]) / heap[i] * 100; g > worst {
			worst, atHeap = g, heap[i]
		}
	}
	switch {
	case worst >= 40 && atHeap >= 512:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Up to %s of a %s MiB heap was held but not in use", fdPctV(worst), fdAmt(atHeap)),
			Detail:   "Fragmentation, or memory the allocator has chosen not to return. Either way it is resident, and a container limit counts it.",
			Action:   "Compare with the resident line on the memory chart. If resident tracks the heap rather than the cache, the allocator is the thing growing — not the workload."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Heap stayed within %s of what the server was using", fdPctV(worst))}
	}
	return c
}

// fdCount renders a whole number of things — handles, keys, records — with thousands
// separators and no decimal point. fdAmt is for magnitudes and renders 74 as "74.0", which
// reads as a measurement rather than a count of records.
func fdCount(v float64) string {
	n := int64(v)
	str := strconv.FormatInt(n, 10)
	if n < 0 {
		return str
	}
	var out []byte
	for i := 0; i < len(str); i++ {
		if i > 0 && (len(str)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, str[i])
	}
	return string(out)
}

// fdDur renders a duration for an advisor sentence.
func fdDur(sec float64) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%.1f h", sec/3600)
	case sec >= 90:
		return fmt.Sprintf("%.0f min", sec/60)
	case sec >= 1:
		return fmt.Sprintf("%.0f s", sec)
	case sec > 0:
		return fmt.Sprintf("%.0f ms", sec*1000)
	default:
		return "0s"
	}
}

// ---------------------------------------------------------------- the router's own view
//
// A mongos capture has no replSetGetStatus, no WiredTiger and no oplog — it stores nothing.
// What it does have, and no other kind of member does, is `networkInterfaceStats`: a latency
// HISTOGRAM of everything its task executors sent to the shards, and per-pool, per-host
// connection counts. That is the router's own measurement of how slow the shards were, from
// the only place in the deployment that talks to all of them.

// fdChartRouterLatency — how long the shards took to answer, as the router experienced it.
//
// Bucketed rather than averaged, because a mean hides the shape that matters here: a router
// whose work is 99% under a millisecond and 1% over a second is a router with one slow shard,
// and its average looks fine.
func fdChartRouterLatency(d *ftdcData) *fdChart {
	// The bucket names are the histogram's own, in order. Anything the build does not have
	// is simply absent — the set has grown between releases.
	buckets := []struct{ name, suffix string }{
		{"under 1 ms", "0-999us"},
		{"1–49 ms", "1-49ms"},
		{"50–99 ms", "50-99ms"},
		{"100–499 ms", "100-149ms"},
		{"500–999 ms", "500-549ms"},
		{"over a second", "1000ms+"},
	}
	c := &fdChart{ID: "routerLatency", Group: "Sharding", Title: "Shard operations by how long they took", Unit: "ops/s", Stack: true,
		Why: "Every operation this router sent to a shard, bucketed by duration. It is the router's own measurement of shard latency, which is the one that matches what the application waited for — and a bucket chart says which shard behaviour is happening rather than averaging it away."}
	for _, b := range buckets {
		suffix := "." + b.suffix
		pts := fdSumRates(d, func(k string) bool {
			return strings.HasPrefix(k, "networkInterfaceStats.") && strings.HasSuffix(k, suffix)
		})
		if fdMax(pts) > 0 {
			c.Series = append(c.Series, fdSeries{Name: b.name, Points: pts})
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	slow := fdSumRates(d, func(k string) bool {
		return strings.HasPrefix(k, "networkInterfaceStats.") &&
			(strings.HasSuffix(k, ".1000ms+") || strings.HasSuffix(k, ".500-549ms"))
	})
	// The router's own topology probes, which is a different kind of slow: a hello that
	// takes 20 ms is a shard that is busy answering rather than one that is busy working.
	hello := fdMax(fdFloats(d, "connPoolStats.replicaSetMonitor.hello.maxLatencyMicros", 1/1000.0))
	switch {
	case fdMax(slow) > 0:
		c.Advice = &fdAdvice{Level: "crit",
			Headline: fmt.Sprintf("Up to %s operation(s)/s took over half a second on a shard", fdAmt(fdMax(slow))),
			Detail:   "The router waited for these and so did the client. Nothing on the shard's own charts has to look slow for this to be true — a shard that is unreachable for a second answers this way too.",
			Action:   "The round-trip chart says whether it was the network; that shard member's own capture says whether it was the work."}
	case hello >= 100:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("Shard work was fast, but a topology probe took %s ms", fdAmt(hello)),
			Detail:   "The router discovers which member of each shard is primary by polling them. A slow reply there delays every routing decision that follows it."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Shard operations stayed fast — peak %s/s, all of it under 50 ms", fdAmt(fdMax(c.Series[0].Points)))}
	}
	return c
}

// fdChartRouterHosts — which shard member the router's connections are busy against.
//
// connPoolStats keeps a pool per host, and the in-use count per host is the router saying
// where its work is going. On a healthy cluster the lines sit together; one line above the
// others is one shard doing all the work, which is the shape a bad shard key produces.
func fdChartRouterHosts(d *ftdcData) *fdChart {
	const pre = "connPoolStats.pools."
	// Collect per-host in-use series across every pool: the key is
	// connPoolStats.pools.<pool>.<host:port>.inUse, and the pool name is an implementation
	// detail while the host is the thing being asked to do work.
	byHost := map[string][]float64{}
	for key := range d.Series {
		rest, ok := strings.CutPrefix(key, pre)
		if !ok || !strings.HasSuffix(rest, ".inUse") {
			continue
		}
		// connPoolStats.pools.<pool>.<host:port>.inUse — and the host is an FQDN, so it
		// has dots of its own. Split off the pool name (which has none) rather than
		// splitting the whole path, or every host reads as "net".
		body := strings.TrimSuffix(rest, ".inUse")
		_, hostPort, ok := strings.Cut(body, ".")
		if !ok || hostPort == "" {
			continue
		}
		if strings.HasPrefix(hostPort, "pool") {
			continue // the pool's own totals, not a host
		}
		host := hostPort
		if h, _, found := strings.Cut(hostPort, ":"); found {
			host = h
		}
		if short, _, found := strings.Cut(host, "."); found {
			host = short // the short name is what every other view of a node calls it
		}
		pts := fdFloats(d, key, 1)
		acc := byHost[host]
		if acc == nil {
			byHost[host] = pts
			continue
		}
		for i := range pts {
			if i < len(acc) {
				acc[i] += pts[i]
			}
		}
	}
	if len(byHost) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	c := &fdChart{ID: "routerHosts", Group: "Sharding", Title: "Router connections in use, by shard member", Unit: "connections",
		Why: "Where this router's work is actually going. A pool per host, and the in-use count is how many operations it has in flight against that member right now — one line consistently above the others is one shard carrying the cluster."}
	busiest, busiestHost := 0.0, ""
	for _, h := range hosts {
		pts := byHost[h]
		if fdMax(pts) == 0 {
			continue
		}
		c.Series = append(c.Series, fdSeries{Name: h, Points: pts})
		if m := fdMax(pts); m > busiest {
			busiest, busiestHost = m, h
		}
	}
	if len(c.Series) == 0 {
		return nil
	}
	// Is one host taking most of it? Compare the busiest against the mean of the rest.
	others, n := 0.0, 0
	for _, s := range c.Series {
		if s.Name == busiestHost {
			continue
		}
		others += fdMax(s.Points)
		n++
	}
	avg := 0.0
	if n > 0 {
		avg = others / float64(n)
	}
	switch {
	case n > 0 && busiest >= 4 && busiest > avg*3:
		c.Advice = &fdAdvice{Level: "warn",
			Headline: fmt.Sprintf("%s carried %s connections at once against %s elsewhere", busiestHost, fdAmt(busiest), fdAmt(avg)),
			Detail:   "The router had several times more work in flight against this member than any other. On a sharded cluster that is either a shard key that sends everything one way, or a member that is slow enough to accumulate work.",
			Action:   "The targeting chart says whether the operations were aimed at one shard or broadcast; that member's own capture says whether it was slow."}
	default:
		c.Advice = &fdAdvice{Level: "ok",
			Headline: fmt.Sprintf("Work was spread across %d member(s), peak %s connections in use", len(c.Series), fdAmt(busiest))}
	}
	return c
}
