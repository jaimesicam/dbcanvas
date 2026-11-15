package main

// logsummary_mongo_digest.go — the slow-query lines grouped by query shape.
//
// lsMongoScanSlow adds every slow-query line into one total per member; that says how much
// work there was, not which query did it. A digest answers the second question the way
// pt-query-digest does for MySQL: operations that differ only in their literal values are
// one shape, and the shapes are ranked by what they cost in total. A 40 ms query run fifty
// thousand times outweighs a single 30-second one, and only grouping shows it.
//
// The shape is built from the command itself rather than from queryShapeHash or queryHash:
// those exist only on 8.0 (queryShapeHash) or only for reads (queryHash), and writes —
// updates and deletes with an unindexed predicate — are as often the cost as the reads.
// Whichever hash the line carries is kept beside the shape, because it is what $queryStats
// and the plan cache are keyed by.
//
// Some operations are slow by design and would top any ranking by time without costing
// anything: a tailable awaitData cursor (the oplog fetcher, a change stream) parks for up
// to maxTimeMS on every getMore, and the connection handshake is logged at verbosity 1 for
// every client. Those are marked Internal and the page leaves them out unless asked.

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// lsDigestKeep is how many shapes a member keeps, ranked by total time. lsDigestInternalKeep
// is the same for the internal ones, which are kept only so they can be shown on request.
const (
	lsDigestKeep         = 60
	lsDigestInternalKeep = 10
	// lsDigestTrack caps the shapes followed while scanning. A shape is a few hundred bytes
	// and a real workload has dozens, but a log of ad-hoc queries with literal field names
	// could have unbounded ones; past the cap they are folded into one "other" row.
	lsDigestTrack = 4000
	lsShapeMaxLen = 320
	lsSampleMax   = 1500
)

// lsSlowShape is one query shape and what all of its slow operations added up to.
type lsSlowShape struct {
	Key      string `json:"key"`
	NS       string `json:"ns"`
	Op       string `json:"op"`
	Shape    string `json:"shape"`
	Internal bool   `json:"internal,omitempty"`
	Ops      int    `json:"ops"`
	Millis   int64  `json:"millis"`
	MaxMs    int64  `json:"maxMs"`
	WaitedMs int64  `json:"waitedMs"`
	// CPUNanos is the sum of cpuNanos (7.0+, Linux). CPUOps is how many lines carried it,
	// so the page can tell "no CPU" from "this server does not report it".
	CPUNanos   int64         `json:"cpuNanos"`
	CPUOps     int           `json:"cpuOps"`
	Docs       int64         `json:"docsExamined"`
	Keys       int64         `json:"keysExamined"`
	Returned   int64         `json:"returned"`
	Bytes      int64         `json:"bytesRead"`
	ReadMicros int64         `json:"timeReadingMicros"`
	Collscans  int           `json:"collscans"`
	InMemSorts int           `json:"inMemSorts"`
	Spilled    int           `json:"spilled"`
	Yields     int64         `json:"yields"`
	Conflicts  int64         `json:"writeConflicts"`
	Plans      []lsSlowCount `json:"plans,omitempty"`
	App        string        `json:"app,omitempty"`
	Hash       string        `json:"hash,omitempty"`
	FirstTS    float64       `json:"firstTs"`
	LastTS     float64       `json:"lastTs"`
	// Hist is the duration distribution as sparse [bucket, count] pairs, four buckets per
	// doubling (see lsDigestBucket). It is what lets the page compute p95 for one member
	// or for all of them merged, which a percentile computed here could not do.
	Hist [][2]int64 `json:"hist"`
	// The slowest single occurrence: its command with session and auth noise removed.
	Sample     string  `json:"sample,omitempty"`
	SampleLine int     `json:"sampleLine,omitempty"`
	SampleTS   float64 `json:"sampleTs,omitempty"`
	SampleMs   int64   `json:"sampleMs,omitempty"`

	plans map[string]int
	apps  map[string]int
	hist  map[int]int64
}

// lsDigest accumulates shapes for one member.
type lsDigest struct {
	shapes map[string]*lsSlowShape
}

func newLsDigest() *lsDigest { return &lsDigest{shapes: map[string]*lsSlowShape{}} }

// lsDigestBucket maps a duration to its histogram bucket: four per doubling, so a
// percentile read back from it is within about 19% of the true value.
func lsDigestBucket(ms int64) int {
	if ms < 0 {
		ms = 0
	}
	return int(math.Floor(4 * math.Log2(float64(ms)+1)))
}

// add folds one slow-query line into its shape.
func (d *lsDigest) add(r lsMongoRecord, dur, work, docs, keys, ret, bytesRead, micros int64, plan string) {
	ns := r.str("ns")
	op, shape, sample, internal := lsMongoShape(r)
	key := ns + "|" + op + "|" + shape
	s := d.shapes[key]
	if s == nil {
		if len(d.shapes) >= lsDigestTrack {
			key = "(other)"
			s = d.shapes[key]
			if s == nil {
				s = &lsSlowShape{Key: key, NS: "", Op: "(other shapes)", Shape: "more distinct shapes than the digest follows"}
				d.shapes[key] = s
			}
		} else {
			s = &lsSlowShape{Key: key, NS: ns, Op: op, Shape: shape, Internal: internal}
			d.shapes[key] = s
		}
	}
	if s.plans == nil {
		s.plans, s.apps, s.hist = map[string]int{}, map[string]int{}, map[int]int64{}
	}
	s.Ops++
	s.Millis += dur
	if dur > work && work >= 0 {
		if _, ok := r.Attr["workingMillis"]; ok {
			s.WaitedMs += dur - work
		}
	}
	if c, ok := r.num("cpuNanos"); ok {
		s.CPUNanos += c
		s.CPUOps++
	}
	s.Docs += docs
	s.Keys += keys
	s.Returned += ret
	s.Bytes += bytesRead
	s.ReadMicros += micros
	if strings.Contains(plan, "COLLSCAN") {
		s.Collscans++
	}
	if string(r.Attr["hasSortStage"]) == "true" {
		s.InMemSorts++
	}
	if string(r.Attr["usedDisk"]) == "true" {
		s.Spilled++
	}
	if y, ok := r.num("numYields"); ok {
		s.Yields += y
	}
	if c, ok := r.num("writeConflicts"); ok {
		s.Conflicts += c
	}
	if plan != "" {
		s.plans[plan]++
	}
	if app := r.str("appName"); app != "" {
		s.apps[app]++
	}
	if s.Hash == "" {
		if h := r.str("queryShapeHash"); h != "" {
			s.Hash = h
		} else if h := r.str("queryHash"); h != "" {
			s.Hash = h
		}
	}
	if s.FirstTS == 0 || (r.TS > 0 && r.TS < s.FirstTS) {
		s.FirstTS = r.TS
	}
	if r.TS > s.LastTS {
		s.LastTS = r.TS
	}
	s.hist[lsDigestBucket(dur)]++
	if dur > s.MaxMs || s.Sample == "" {
		if dur > s.MaxMs {
			s.MaxMs = dur
		}
		s.Sample, s.SampleLine, s.SampleTS, s.SampleMs = sample, r.Line, r.TS, dur
	}
}

// result returns the kept shapes, worst first by total time.
func (d *lsDigest) result() []lsSlowShape {
	var user, internal []lsSlowShape
	for _, s := range d.shapes {
		out := *s
		out.Plans = lsTopCounts(s.plans, 3)
		if apps := lsTopCounts(s.apps, 1); len(apps) > 0 {
			out.App = apps[0].Name
		}
		out.Hist = make([][2]int64, 0, len(s.hist))
		for b, n := range s.hist {
			out.Hist = append(out.Hist, [2]int64{int64(b), n})
		}
		sort.Slice(out.Hist, func(i, j int) bool { return out.Hist[i][0] < out.Hist[j][0] })
		out.plans, out.apps, out.hist = nil, nil, nil
		if out.Internal {
			internal = append(internal, out)
		} else {
			user = append(user, out)
		}
	}
	rank := func(v []lsSlowShape, n int) []lsSlowShape {
		sort.Slice(v, func(i, j int) bool {
			if v[i].Millis != v[j].Millis {
				return v[i].Millis > v[j].Millis
			}
			if v[i].Ops != v[j].Ops {
				return v[i].Ops > v[j].Ops
			}
			return v[i].Key < v[j].Key
		})
		if len(v) > n {
			v = v[:n]
		}
		return v
	}
	return append(rank(user, lsDigestKeep), rank(internal, lsDigestInternalKeep)...)
}

func lsTopCounts(m map[string]int, n int) []lsSlowCount {
	out := make([]lsSlowCount, 0, len(m))
	for k, v := range m {
		out = append(out, lsSlowCount{Name: k, Ops: v})
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

// lsShapeDrop are command fields that say how a command was sent rather than what it asks:
// sessions, cluster time, read and write concern, batching, and the replication and auth
// plumbing. They are dropped from the shape (two finds differing only in batchSize are one
// query) and from the sample (a session id or SCRAM payload is noise at best).
var lsShapeDrop = map[string]bool{
	"lsid": true, "$db": true, "$clusterTime": true, "txnNumber": true, "autocommit": true,
	"startTransaction": true, "readConcern": true, "writeConcern": true, "$readPreference": true,
	"comment": true, "maxTimeMS": true, "maxTimeMSOpOnly": true, "cursor": true, "batchSize": true,
	"shardVersion": true, "databaseVersion": true, "$audit": true, "$client": true, "client": true,
	"apiVersion": true, "apiStrict": true, "apiDeprecationErrors": true, "mayBypassWriteBlocking": true,
	"documents": true, "ordered": true, "bypassDocumentValidation": true, "singleBatch": true,
	"$replData": true, "$oplogQueryData": true, "term": true, "lastKnownCommittedOpTime": true,
	"collection": true, "tailable": true, "awaitData": true, "noCursorTimeout": true,
	"needsMerge": true, "fromMongos": true, "fromRouter": true, "$configTime": true, "$topologyTime": true,
	"runtimeConstants": true, "encryptionInformation": true, "speculativeAuthenticate": true,
	"saslSupportedMechs": true, "payload": true, "conversationId": true, "compression": true,
	"helloOk": true, "backpressure": true, "signature": true, "txnRetryCounter": true,
	"readOnce": true, "$queryOptions": true, "let": true, "allowDiskUse": true,
}

// lsInternalCmds are commands every client and every member sends as plumbing.
var lsInternalCmds = map[string]bool{
	"hello": true, "ismaster": true, "isMaster": true, "saslStart": true, "saslContinue": true,
	"authenticate": true, "ping": true, "buildInfo": true, "buildinfo": true, "getParameter": true,
	"endSessions": true, "logout": true, "replSetHeartbeat": true, "replSetUpdatePosition": true,
	"getLastError": true, "whatsmyuri": true, "_isSelf": true,
}

// lsMongoShape returns a slow-query line's operation name, its shape with every literal
// replaced by ?, a cleaned sample of the command, and whether it is internal plumbing.
func lsMongoShape(r lsMongoRecord) (op, shape, sample string, internal bool) {
	cmdRaw := r.Attr["command"]
	typ := r.str("type")
	name := lsFirstKey(cmdRaw)
	body := cmdRaw
	switch {
	case typ == "update" || typ == "remove" || typ == "insert":
		op = typ
	case name == "getMore":
		if orig, ok := r.Attr["originatingCommand"]; ok {
			op = "getMore:" + lsFirstKey(orig)
			body = orig
			var o map[string]json.RawMessage
			if json.Unmarshal(orig, &o) == nil && string(o["tailable"]) == "true" && string(o["awaitData"]) == "true" {
				internal = true
			}
		} else {
			op = "getMore"
		}
	case name != "":
		op = name
	default:
		op = typ
	}
	if lsInternalCmds[name] || r.str("appName") == "OplogFetcher" {
		internal = true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if len(body) == 0 || dec.Decode(&v) != nil {
		return op, "", "", internal
	}
	if m, ok := v.(map[string]any); ok {
		first := lsFirstKey(body)
		clean := map[string]any{}
		for k, val := range m {
			if lsShapeDrop[k] {
				continue
			}
			clean[k] = val
		}
		// The command's own key carries the collection name, which the namespace already
		// says; in the shape it is noise, in the sample it is kept.
		shaped := map[string]any{}
		for k, val := range clean {
			if k != first {
				shaped[k] = val
			}
		}
		shape = lsShapeString(shaped, 0)
		if sb, err := json.Marshal(clean); err == nil {
			sample = string(sb)
		}
	} else {
		shape = lsShapeString(v, 0)
	}
	if len(shape) > lsShapeMaxLen {
		shape = shape[:lsShapeMaxLen] + "…"
	}
	if len(sample) > lsSampleMax {
		sample = sample[:lsSampleMax] + "…"
	}
	return op, shape, sample, internal
}

// lsFirstKey returns the first key of a JSON object, in document order — for a MongoDB
// command, the command's name. A Go map cannot say which key came first.
func lsFirstKey(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	if t, err := dec.Token(); err == nil {
		if s, ok := t.(string); ok {
			return s
		}
	}
	return ""
}

// lsShapeString renders a value with its literals replaced: keys and operators are kept,
// values become ?, an array becomes the shape of its elements (or […] when they differ),
// and Extended JSON scalars ({"$oid": …}, {"$date": …}) are one ? rather than an object.
func lsShapeString(v any, depth int) string {
	if depth > 10 {
		return "…"
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			for k := range t {
				switch k {
				case "$oid", "$date", "$timestamp", "$binary", "$numberLong", "$numberDecimal",
					"$numberInt", "$numberDouble", "$regularExpression", "$uuid", "$minKey", "$maxKey":
					return "?"
				}
			}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(lsShapeString(t[k], depth+1))
		}
		b.WriteByte('}')
		return b.String()
	case []any:
		if len(t) == 0 {
			return "[]"
		}
		// A list of values ($in, $all) collapses to one element whatever its length, so
		// $in with 3 ids and with 300 is one shape. A list of differing elements — an
		// aggregation pipeline, an $or — is the query itself and is spelled out.
		parts := make([]string, len(t))
		same := true
		for i, e := range t {
			parts[i] = lsShapeString(e, depth+1)
			if parts[i] != parts[0] {
				same = false
			}
		}
		if same {
			return "[" + parts[0] + "]"
		}
		if len(parts) > 12 {
			return "[" + strings.Join(parts[:12], ", ") + ", …]"
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return "?"
	}
}
