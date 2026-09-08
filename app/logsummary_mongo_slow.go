package main

// logsummary_mongo_slow.go — what the slow-query lines add up to.
//
// id 51803 is the single most common line in a busy mongod log and, until now, the least
// used: the catalogue dropped it as noise, which it is one line at a time. On the capture
// this was written against — a three-member 8.0 replica set under a deliberately oversized
// workload — the primary wrote 6,334,432 of them in 86 minutes. Nobody reads six million
// lines, and a page that turned each into an event would be unusable.
//
// But every one of them carries numbers that exist nowhere else: which collection, which
// plan, how many documents were examined to return how many, how many bytes came off the
// disk to do it, how long it waited to be admitted, and how long it waited for other
// members to acknowledge the write. Those are per-operation facts; FTDC has the totals for
// every operation but cannot say which collection or which plan. So they are ACCUMULATED
// here rather than listed — one summary per source, plus an event for the handful that were
// slow enough to be worth a row on the timeline.
//
// Verified against the real thing: the totals below reproduce FTDC's own counters for the
// same window to within 0.02% (docsExamined against metrics.queryExecutor.scannedObjects,
// storage.data.bytesRead against wiredTiger.cache "bytes read into cache"), which is what
// makes it safe to pair them on one screen.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// lsSlowEventMs is how slow an operation has to be to earn its own row on the timeline.
// A second is the threshold because it is the point at which a human notices: below it the
// summary is the honest representation, above it the individual operation is worth naming.
const lsSlowEventMs = 1000

// lsSlowKeep is how many of the worst operations to keep per source. Enough to see a
// pattern, few enough that a pathological log cannot fill the page.
const lsSlowKeep = 12

// lsMongoStats is one source's slow-query arithmetic, plus what the file cost to write.
type lsMongoStats struct {
	Ops      int   `json:"ops"`      // slow-query lines seen
	Millis   int64 `json:"millis"`   // Σ durationMillis
	WaitedMs int64 `json:"waitedMs"` // Σ (durationMillis − workingMillis): time not working
	Reads    int   `json:"reads"`
	Writes   int   `json:"writes"`
	Docs     int64 `json:"docsExamined"`
	Keys     int64 `json:"keysExamined"`
	Returned int64 `json:"returned"`
	// Bytes/ReadMicros are what the operations pulled off the device, attributed to the
	// operation that pulled it — which FTDC's cache counters cannot do.
	Bytes       int64 `json:"bytesRead"`
	ReadMicros  int64 `json:"timeReadingMicros"`
	Yields      int64 `json:"yields"`
	Conflicts   int64 `json:"writeConflicts"`
	WriteConMs  int64 `json:"writeConcernMs"`
	WriteConOps int   `json:"writeConcernOps"`
	Collscans   int   `json:"collscans"`
	// CollDocs is how many documents those scans examined. The count of scans alone is
	// misleading: a COLLSCAN over a twelve-document admin collection is the right plan,
	// and on a busy member most of them are exactly that. The documents are the cost.
	CollDocs int64 `json:"collscanDocs"`
	CollRet  int64 `json:"collscanReturned"`
	NoPlan   int   `json:"noPlan"` // commands with no plan summary (inserts, admin)
	// Debug is how many D-level lines the file holds. It is not about slow queries at all;
	// it is the answer to "why is this log 10 GiB", and this is the one pass over every
	// line, so it is counted here rather than in a second one.
	Debug int `json:"debugLines"`
	// Top namespaces and plans, biggest first, capped.
	Namespaces []lsSlowCount `json:"namespaces,omitempty"`
	Plans      []lsSlowCount `json:"plans,omitempty"`
}

// lsSlowCount is one namespace or plan and how many slow operations it accounted for.
type lsSlowCount struct {
	Name  string `json:"name"`
	Ops   int    `json:"ops"`
	Docs  int64  `json:"docs,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}

// lsMongoScanSlow walks a member's records once, accumulating the summary and returning the
// operations slow enough to deserve their own event.
//
// Deliberately a separate pass from lsClassifyMongo rather than a hook inside it: the
// classifier answers "is this record an event", and the answer for a slow query is no. This
// answers a different question about the same bytes.
func lsMongoScanSlow(recs []lsMongoRecord) (lsMongoStats, []lsEvent) {
	var st lsMongoStats
	ns := map[string]*lsSlowCount{}
	plans := map[string]*lsSlowCount{}
	var worst []lsEvent
	for _, r := range recs {
		if strings.HasPrefix(r.Level, "D") {
			st.Debug++
		}
		if r.ID != 51803 {
			continue
		}
		st.Ops++
		dur, _ := r.num("durationMillis")
		st.Millis += dur
		if work, ok := r.num("workingMillis"); ok && dur > work {
			st.WaitedMs += dur - work
		}
		docs, _ := r.num("docsExamined")
		keys, _ := r.num("keysExamined")
		ret, _ := r.num("nreturned")
		st.Docs += docs
		st.Keys += keys
		st.Returned += ret
		if y, ok := r.num("numYields"); ok {
			st.Yields += y
		}
		if c, ok := r.num("writeConflicts"); ok {
			st.Conflicts += c
		}
		if w, ok := r.num("waitForWriteConcernDurationMillis"); ok && w > 0 {
			st.WriteConMs += w
			st.WriteConOps++
		}
		bytes, micros := lsSlowStorage(r)
		st.Bytes += bytes
		st.ReadMicros += micros
		switch r.Comp {
		case "WRITE":
			st.Writes++
		default:
			st.Reads++
		}
		plan := r.str("planSummary")
		switch {
		case plan == "":
			st.NoPlan++
		default:
			if strings.Contains(plan, "COLLSCAN") {
				st.Collscans++
				st.CollDocs += docs
				st.CollRet += ret
			}
			p := plans[plan]
			if p == nil {
				p = &lsSlowCount{Name: plan}
				plans[plan] = p
			}
			p.Ops++
			p.Docs += docs
		}
		if name := r.str("ns"); name != "" {
			n := ns[name]
			if n == nil {
				n = &lsSlowCount{Name: name}
				ns[name] = n
			}
			n.Ops++
			n.Docs += docs
			n.Bytes += bytes
		}
		if dur >= lsSlowEventMs {
			worst = append(worst, lsSlowEvent(r, dur, docs, ret, bytes))
		}
	}
	if st.Ops == 0 && st.Debug == 0 {
		return lsMongoStats{}, nil
	}
	st.Namespaces = lsSlowTop(ns, 6)
	st.Plans = lsSlowTop(plans, 6)
	// Keep the worst by duration, then put them back in time order — a timeline reads in
	// time order, and picking by duration is only how they were chosen.
	sort.SliceStable(worst, func(i, j int) bool { return worst[i].Members > worst[j].Members })
	if len(worst) > lsSlowKeep {
		worst = worst[:lsSlowKeep]
	}
	sort.SliceStable(worst, func(i, j int) bool { return worst[i].TS < worst[j].TS })
	return st, worst
}

// lsSlowStorage digs the storage sub-document out of a slow-query line: how many bytes the
// operation read from the device and how long it spent doing it. Absent on an operation
// that was served entirely from cache, which is itself the useful signal.
func lsSlowStorage(r lsMongoRecord) (bytes, micros int64) {
	raw, ok := r.Attr["storage"]
	if !ok {
		return 0, 0
	}
	var st struct {
		Data struct {
			BytesRead         int64 `json:"bytesRead"`
			TimeReadingMicros int64 `json:"timeReadingMicros"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &st) != nil {
		return 0, 0
	}
	return st.Data.BytesRead, st.Data.TimeReadingMicros
}

// lsSlowEvent turns one exceptionally slow operation into a timeline row.
//
// Members carries the duration in milliseconds. That field is a small abuse — it exists for
// cluster sizes — but it is the only numeric slot on lsEvent, it is what the sort above
// needs, and inventing a second one for this would change a struct four engines share.
func lsSlowEvent(r lsMongoRecord, dur, docs, ret, bytes int64) lsEvent {
	e := lsEvent{
		TS: r.TS, Line: r.Line, Time: r.Time, Level: lsMongoLevel(r.Level),
		Code: strconv.Itoa(r.ID), Subsys: r.Comp, Class: lsClassClient,
		Sev: lsSevWarn, Members: int(dur),
		Label: fmt.Sprintf("Slow operation — %s", lsMongoDur(float64(dur)/1000)),
		Meaning: "One operation the server took longer than a second over. The numbers beside it are " +
			"that operation's own: how much it had to read to answer, and how much of that came off the disk.",
	}
	parts := []string{}
	if ns := r.str("ns"); ns != "" {
		parts = append(parts, ns)
	}
	if plan := r.str("planSummary"); plan != "" {
		parts = append(parts, plan)
	}
	if docs > 0 {
		parts = append(parts, fmt.Sprintf("%d examined → %d returned", docs, ret))
	}
	if bytes > 0 {
		parts = append(parts, fmt.Sprintf("%s read from disk", lsBytesShort(bytes)))
	}
	e.Message = strings.Join(parts, " · ")
	if strings.Contains(r.str("planSummary"), "COLLSCAN") {
		e.Sev = lsSevBad
	}
	return e
}

// lsSlowTop orders a counter map and keeps the head of it.
func lsSlowTop(m map[string]*lsSlowCount, n int) []lsSlowCount {
	out := make([]lsSlowCount, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ops != out[j].Ops {
			return out[i].Ops > out[j].Ops
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// lsBytesShort renders a byte count for a one-line label.
func lsBytesShort(b int64) string {
	f := float64(b)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.1f GiB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f MiB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f KiB", f/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
