package main

// ftdc.go — reading MongoDB's diagnostic.data.
//
// Every mongod writes one, always, with no configuration and almost no cost: once a second
// it captures serverStatus, replSetGetStatus, the WiredTiger statistics, the host's CPU,
// memory and disk counters, and writes them to `<dbPath>/diagnostic.data/`. It is the black
// box recorder. When somebody asks "what was the server doing at 04:12 last Tuesday", this
// is the only artefact that can answer, and unlike pt-stalk nobody has to have thought to
// turn it on beforehand.
//
// It is also undocumented and awkward, which is presumably why so few people read their
// own. The format, established by decoding a real file from a live 8.0.28-12 replica set:
//
//	the file        a bare sequence of BSON documents, back to back, no header
//	each document   { _id: <date>, type: <0|1>, data: <binary> }
//	                  type 0 — metadata: the document is a one-off (build info, config)
//	                  type 1 — a metric CHUNK, which is where everything lives
//	chunk data      [uint32 LE uncompressed length][zlib stream]
//	the chunk       [reference document (BSON)]
//	                [uint32 LE metric count][uint32 LE delta count]
//	                [varint stream]
//
// The reference document is one complete sample — the full serverStatus tree — and it
// defines both the metric set and its order. Every metric is then a column: the varint
// stream is COLUMN-major, all of metric 0's deltas, then all of metric 1's, and each delta
// is added to the running value of that metric. So a metric that does not change costs
// almost nothing, which is the trick that makes a per-second capture of ten thousand
// counters fit in a few hundred kilobytes.
//
// The rest of the trick is the zero encoding. A varint of 0 is not one zero: it is followed
// by a second varint saying how many MORE zeros come after it. A gauge that sits still for
// an hour is two bytes. This is also the part that punishes a careless decoder — read the
// run length as a value and every metric after it in the chunk is silently wrong, which is
// exactly the failure a test against a real file catches and a synthetic one does not.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// ftdcSeries is one metric over time: a dotted path and one value per sample.
//
// Values are int64 because that is what FTDC stores — doubles are truncated on the way in
// by mongod itself, not by this decoder, so a cache-fill ratio arrives as 0 or 1 and a
// byte counter arrives exact. Anything that needs a fraction is derived from two counters.
type ftdcSeries struct {
	Key    string  `json:"key"`
	Values []int64 `json:"values"`
}

// ftdcData is a decoded diagnostic.data set: one timestamp column, and every metric that
// appeared, aligned to it.
type ftdcData struct {
	// TS is the sample time in epoch seconds, one per sample, ascending. It is FTDC's own
	// `start` field: the moment the collector BEGAN gathering that sample.
	TS []float64 `json:"ts"`
	// TSEnd is the `end` field of the same sample — when the collector finished. On a
	// healthy server the two are milliseconds apart and the distinction does not matter.
	// On a stalled one it matters enormously: a round that has to wait for a server which
	// cannot answer serverStatus takes as long as the stall, and the counters it reads have
	// advanced by that whole time. Rates are therefore computed from END to END, because
	// start-to-start spacing understates the elapsed time exactly when the server is worst.
	//
	// Measured, not assumed: on a member being driven into a fatal RSTL timeout, one round
	// started at 00:51:31 and ended at 00:52:49 while the next started at 00:51:34 — every
	// per-second rate over that interval came out 26x too high before this existed.
	TSEnd []float64 `json:"-"`
	// Series is keyed by the dotted path, e.g. "serverStatus.connections.current".
	Series map[string]*ftdcSeries `json:"series"`
	// Meta holds the type-0 documents' useful fields — build version, host, replica-set
	// name — which is how a bundle can label a file without being told what it is.
	Meta map[string]string `json:"meta,omitempty"`
	// Chunks and Samples are reported because they are the first thing to check when a
	// file looks wrong, and because "how much is here" is a question the UI has to answer
	// before it draws anything.
	Chunks  int `json:"chunks"`
	Samples int `json:"samples"`
	// Skipped counts chunks that would not decode. A truncated metrics.interim is normal —
	// it is the file mongod is currently writing — so this is reported rather than fatal.
	Skipped int `json:"skipped,omitempty"`
}

// ftdcMaxSamples caps a decode. A day of one-second samples is 86,400 per metric and a
// busy 8.0 server carries well over 3,000 metrics; decoding a week without a ceiling is
// how a diagnostic tool becomes the outage.
const ftdcMaxSamples = 200000

// ftdcParse decodes one or more diagnostic.data files, in the order given.
//
// Files are expected in chronological order (their names sort that way, which is why
// mongod names them after their first sample). Samples are appended, so a caller handing
// over a whole directory gets one continuous series.
func ftdcParse(files [][]byte) (*ftdcData, error) {
	d := &ftdcData{Series: map[string]*ftdcSeries{}, Meta: map[string]string{}}
	for _, raw := range files {
		if err := d.readFile(raw); err != nil {
			return nil, err
		}
	}
	if len(d.TS) == 0 {
		return nil, fmt.Errorf("no metric samples found — is this a diagnostic.data file?")
	}
	d.unwrapRoles()
	// A metric that appeared late (a replica-set field that only exists once the member
	// has joined, say) is short. Left-pad it so every series is the same length as the
	// timestamp column and a chart can index them together without bounds checks.
	for _, s := range d.Series {
		if n := len(d.TS) - len(s.Values); n > 0 {
			s.Values = append(make([]int64, n), s.Values...)
		}
	}
	return d, nil
}

// readFile walks the back-to-back BSON documents of one file.
func (d *ftdcData) readFile(raw []byte) error {
	for off := 0; off+4 <= len(raw); {
		n := int(int32(binary.LittleEndian.Uint32(raw[off:])))
		if n < 5 || off+n > len(raw) {
			// A short final document is a file mongod is still writing. Everything
			// before it decoded fine and is worth keeping.
			break
		}
		doc := bson.Raw(raw[off : off+n])
		off += n
		switch typ, _ := doc.Lookup("type").Int32OK(); typ {
		case 0:
			d.readMetadata(doc)
		case 1:
			if err := d.readChunk(doc); err != nil {
				d.Skipped++
			}
		}
		if len(d.TS) >= ftdcMaxSamples {
			break
		}
	}
	return nil
}

// readMetadata reads the type-0 document — the one written when a metrics file opens, and
// never again.
//
// It used to keep four fields, on the grounds that the rest is a verbatim copy of buildInfo
// and getCmdLineOpts. That was wrong in a specific way: it is the ONLY place a capture says
// what the server was, and half of what looks like a mystery in the charts is a setting. A
// cache sized from 30 GiB of host memory inside a 3 GiB container, transparent huge pages
// left on, a file-descriptor ceiling of 1024, authorization disabled — none of those are
// metrics, all of them are here, and all of them change what the charts mean.
//
// Every field below was read out of a real 8.0.28-12 capture rather than from documentation.
// Values are stored as strings because that is what they are: this map is for display, and
// anything that needed arithmetic would be a metric in a chunk instead.
func (d *ftdcData) readMetadata(doc bson.Raw) {
	inner, ok := doc.Lookup("doc").DocumentOK()
	if !ok {
		return
	}
	put := func(key, val string) {
		if val != "" && d.Meta[key] == "" {
			d.Meta[key] = val
		}
	}
	// 8.0 groups a sharded deployment's capture by role, and the metadata document is
	// grouped with it: buildInfo and hostInfo move from the top of `doc` to `doc.common`.
	// Read both, so an 8.0 sharded capture still knows what version and host it came from
	// rather than arriving anonymous.
	for _, prefix := range [][]string{nil, {"common"}} {
		at := func(path ...string) bson.RawValue {
			full := append(append([]string{}, prefix...), path...)
			v, err := inner.LookupErr(full...)
			if err != nil {
				return bson.RawValue{}
			}
			return v
		}
		str := func(key string, path ...string) {
			if s, ok := at(path...).StringValueOK(); ok {
				put(key, s)
			}
		}
		num := func(key, suffix string, path ...string) float64 {
			v := at(path...)
			f, ok := ftdcNumOf(v)
			if !ok {
				return 0
			}
			put(key, ftdcFmtNum(f)+suffix)
			return f
		}
		boolean := func(key string, path ...string) {
			if b, ok := at(path...).BooleanOK(); ok {
				put(key, map[bool]string{true: "yes", false: "no"}[b])
			}
		}

		str("version", "buildInfo", "version")
		str("psmdbVersion", "buildInfo", "psmdbVersion")
		str("gitVersion", "buildInfo", "gitVersion")
		str("allocator", "buildInfo", "allocator")
		str("openssl", "buildInfo", "openssl", "running")
		str("host", "hostInfo", "system", "hostname")
		str("os", "hostInfo", "os", "name")
		str("kernel", "hostInfo", "extra", "kernelVersion")
		str("cpu", "hostInfo", "extra", "cpuString")
		str("thp", "hostInfo", "extra", "thp_enabled")
		num("cores", "", "hostInfo", "system", "numCores")
		num("coresAvailable", "", "hostInfo", "system", "numCoresAvailableToProcess")
		memSize := num("memSizeMB", " MiB", "hostInfo", "system", "memSizeMB")
		memLimit := num("memLimitMB", " MiB", "hostInfo", "system", "memLimitMB")
		boolean("numa", "hostInfo", "system", "numaEnabled")
		num("maxOpenFiles", "", "hostInfo", "extra", "maxOpenFiles")
		num("fileDescriptors", "", "ulimits", "fileDescriptors", "soft")
		str("replSet", "getCmdLineOpts", "parsed", "replication", "replSetName")
		str("process", "getCmdLineOpts", "parsed", "processManagement", "pidFilePath")
		str("dbPath", "getCmdLineOpts", "parsed", "storage", "dbPath")
		str("logPath", "getCmdLineOpts", "parsed", "systemLog", "path")
		str("authorization", "getCmdLineOpts", "parsed", "security", "authorization")
		str("keyFile", "getCmdLineOpts", "parsed", "security", "keyFile")
		str("clusterRole", "getCmdLineOpts", "parsed", "sharding", "clusterRole")
		num("port", "", "getCmdLineOpts", "parsed", "net", "port")
		num("cacheSizeGB", " GiB", "getCmdLineOpts", "parsed", "storage", "wiredTiger", "engineConfig", "cacheSizeGB")
		str("defaultReadConcern", "getDefaultRWConcern", "defaultReadConcern", "level")
		num("defaultWriteConcern", "", "getDefaultRWConcern", "defaultWriteConcern", "w")

		// The trap this exists to surface: mongod sizes its cache from what it believes the
		// machine has, and inside a container that is usually the HOST's memory. When the
		// two agree and nothing pinned the cache, say so — a reader whose member was OOM
		// killed with a "healthy" cache chart needs that sentence, and it cannot be
		// inferred from any counter in the file.
		if memSize > 0 && memLimit > 0 && memSize == memLimit && d.Meta["cacheSizeGB"] == "" {
			put("memNote", "mongod sized its cache from "+ftdcFmtNum(memSize)+" MiB of visible memory; "+
				"a smaller container or cgroup limit is invisible to it and to this file")
		}
	}
}

// ftdcNumOf reads a BSON number of any width, because the same field arrives as an int32 on
// one build and a double on the next and a reader that only handles one silently drops it.
func ftdcNumOf(v bson.RawValue) (float64, bool) {
	switch v.Type {
	case bson.TypeInt32:
		return float64(v.Int32()), true
	case bson.TypeInt64:
		return float64(v.Int64()), true
	case bson.TypeDouble:
		return v.Double(), true
	}
	return 0, false
}

// ftdcFmtNum renders a metadata number the way a person writes it: no decimal point on a
// whole number, no exponent, thousands left alone (these are core counts and megabytes,
// not measurements).
func ftdcFmtNum(f float64) string {
	if f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ftdcRoleGroups are the wrappers MongoDB 8.0 puts around a sharded deployment's sample.
//
// Up to and including 7.0 a sample is one flat tree: serverStatus, systemMetrics,
// replSetGetStatus, local, config, and on a router connPoolStats. In 8.0, every process in a
// SHARDED cluster nests all of it by role — `common.serverStatus`, `router.connPoolStats`,
// `shard.replSetGetStatus` — while a plain replica-set member is unchanged.
//
// Which means an 8.0 sharded capture matches none of the keys this page reads, on any of its
// three kinds of process. Not a chart missing: every chart missing, on a file that decodes
// perfectly and reports several thousand metrics.
//
// The wrapper is stripped here rather than fixed in eighty chart keys, because it is exactly
// a prefix and nothing else: the groups partition the tree, so removing them reproduces the
// pre-8.0 layout precisely. Verified against a live 8.0.28-12 sharded cluster — the only
// names appearing under more than one group are `start` and `end`, which are FTDC's own
// per-section collection timestamps and are not metrics anybody charts.
var ftdcRoleGroups = []string{"common.", "router.", "shard."}

// ftdcUnwrapRole strips an 8.0 role group from a metric path, and says whether it did.
func ftdcUnwrapRole(key string) (string, bool) {
	for _, g := range ftdcRoleGroups {
		if rest, ok := strings.CutPrefix(key, g); ok {
			return rest, true
		}
	}
	return key, false
}

// readChunk decodes one type-1 document: the reference sample plus every delta after it.
func (d *ftdcData) readChunk(doc bson.Raw) error {
	_, data, ok := doc.Lookup("data").BinaryOK()
	if !ok || len(data) < 4 {
		return fmt.Errorf("chunk has no data")
	}
	// The first four bytes are the uncompressed length. zlib follows.
	want := int(binary.LittleEndian.Uint32(data[:4]))
	zr, err := zlib.NewReader(bytes.NewReader(data[4:]))
	if err != nil {
		return fmt.Errorf("chunk is not zlib: %w", err)
	}
	defer zr.Close()
	buf, err := io.ReadAll(io.LimitReader(zr, int64(want)+1))
	if err != nil {
		return fmt.Errorf("inflate: %w", err)
	}

	// The reference document is the first sample, complete.
	if len(buf) < 4 {
		return fmt.Errorf("chunk too short")
	}
	refLen := int(int32(binary.LittleEndian.Uint32(buf)))
	if refLen < 5 || refLen > len(buf) {
		return fmt.Errorf("reference document length %d out of range", refLen)
	}
	ref := bson.Raw(buf[:refLen])
	keys, values := ftdcFlatten(ref)
	if len(keys) == 0 {
		return fmt.Errorf("reference document holds no metrics")
	}

	rest := buf[refLen:]
	if len(rest) < 8 {
		return fmt.Errorf("chunk header truncated")
	}
	nMetrics := int(binary.LittleEndian.Uint32(rest[0:4]))
	nDeltas := int(binary.LittleEndian.Uint32(rest[4:8]))
	rest = rest[8:]
	// mongod's own count and the count implied by the reference document must agree. When
	// they do not, the reference document was not parsed the way mongod wrote it, and
	// every column after the first discrepancy would be attributed to the wrong metric —
	// so the chunk is refused rather than half-read.
	if nMetrics != len(keys) {
		return fmt.Errorf("chunk declares %d metrics, reference document yields %d", nMetrics, len(keys))
	}

	d.Chunks++
	// The reference sample itself is sample zero of this chunk.
	d.appendSample(keys, values)
	if nDeltas == 0 {
		return nil
	}

	// Column-major: every delta for metric 0, then every delta for metric 1. Decoding
	// into a [metric][sample] grid first and appending afterwards is what keeps the
	// series aligned — the samples arrive in the wrong order to append as they are read.
	grid := make([][]int64, nMetrics)
	pos := 0
	zeros := 0 // zeros still owed by a run-length pair
	for m := 0; m < nMetrics; m++ {
		col := make([]int64, nDeltas)
		cur := values[m]
		for s := 0; s < nDeltas; s++ {
			var delta uint64
			if zeros > 0 {
				zeros--
			} else {
				v, n := ftdcUvarint(rest[pos:])
				if n <= 0 {
					return fmt.Errorf("varint stream ended inside metric %d sample %d", m, s)
				}
				pos += n
				if v == 0 {
					// A zero is a run: the next varint is how many MORE follow.
					run, n2 := ftdcUvarint(rest[pos:])
					if n2 <= 0 {
						return fmt.Errorf("zero run-length truncated at metric %d", m)
					}
					pos += n2
					zeros = int(run)
				} else {
					delta = v
				}
			}
			// Deltas are stored as unsigned and wrap: a counter that goes down (a reset,
			// or a gauge falling) arrives as a very large number that is the two's
			// complement of the negative delta. Adding it with wraparound is correct.
			cur += int64(delta)
			col[s] = cur
		}
		grid[m] = col
	}
	for s := 0; s < nDeltas; s++ {
		sample := make([]int64, nMetrics)
		for m := 0; m < nMetrics; m++ {
			sample[m] = grid[m][s]
		}
		d.appendSample(keys, sample)
	}
	return nil
}

// ftdcTimeKey is the metric FTDC uses as its clock. It is present in every sample of every
// version this has been run against, and it is what the whole series is indexed by.
const ftdcTimeKey = "start"

// ftdcEndKey is the same sample's completion time. See ftdcData.TSEnd.
const ftdcEndKey = "end"

// appendSample adds one sample: its timestamp, and every metric's value.
func (d *ftdcData) appendSample(keys []string, values []int64) {
	if len(d.TS) >= ftdcMaxSamples {
		return
	}
	ts, end := 0.0, 0.0
	for i, k := range keys {
		switch k {
		case ftdcTimeKey:
			// `start` is a BSON date in milliseconds.
			ts = float64(values[i]) / 1000
		case ftdcEndKey:
			end = float64(values[i]) / 1000
		}
	}
	if ts <= 0 {
		return // a sample with no clock cannot be put on a timeline
	}
	if end < ts {
		end = ts // a build that does not write `end`, or a truncated sample
	}
	d.TS = append(d.TS, ts)
	d.TSEnd = append(d.TSEnd, end)
	d.Samples++
	n := len(d.TS)
	for i, k := range keys {
		s := d.Series[k]
		if s == nil {
			// A metric first seen at sample n starts with n-1 zeros behind it, filled in
			// at the end of the parse.
			s = &ftdcSeries{Key: k}
			d.Series[k] = s
		}
		for len(s.Values) < n-1 {
			s.Values = append(s.Values, 0)
		}
		s.Values = append(s.Values, values[i])
	}
}

// ftdcUvarint reads one unsigned LEB128 value. It is binary.Uvarint's algorithm, kept here
// because FTDC's stream is not framed: a malformed run length must be reported as a short
// read rather than panicking a chunk that is otherwise fine.
func ftdcUvarint(b []byte) (uint64, int) {
	var x uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < 0x80 {
			if i > 9 || (i == 9 && c > 1) {
				return 0, -1
			}
			return x | uint64(c)<<shift, i + 1
		}
		x |= uint64(c&0x7f) << shift
		shift += 7
	}
	return 0, 0
}

// ftdcFlatten walks a reference document and returns the metrics in mongod's own order.
//
// The order is the contract: the varint columns are matched to metrics by position and
// nothing else, so this traversal has to reproduce exactly what mongod's encoder did.
// Document order, depth first, arrays included, and:
//
//	bool          0 or 1
//	int32/int64   as-is
//	double        truncated toward zero — mongod stores metrics as int64
//	datetime      milliseconds
//	timestamp     TWO metrics, seconds then increment, in that order
//	anything else skipped entirely — strings, oids, nulls are not metrics
//
// The timestamp-is-two-metrics rule is the one that is easy to miss and impossible to
// notice later: getting it wrong shifts every subsequent column by one, which produces a
// decode that looks plausible and is entirely wrong. The metric-count check in readChunk
// is there to catch precisely that.
func ftdcFlatten(doc bson.Raw) ([]string, []int64) {
	var keys []string
	var vals []int64
	var walk func(prefix string, raw bson.Raw)
	walk = func(prefix string, raw bson.Raw) {
		elems, err := raw.Elements()
		if err != nil {
			return
		}
		for _, e := range elems {
			key := e.Key()
			if prefix != "" {
				key = prefix + "." + key
			}
			v := e.Value()
			switch v.Type {
			case bson.TypeEmbeddedDocument:
				walk(key, v.Document())
			case bson.TypeArray:
				walk(key, bson.Raw(v.Array()))
			case bson.TypeBoolean:
				b := int64(0)
				if v.Boolean() {
					b = 1
				}
				keys, vals = append(keys, key), append(vals, b)
			case bson.TypeInt32:
				keys, vals = append(keys, key), append(vals, int64(v.Int32()))
			case bson.TypeInt64:
				keys, vals = append(keys, key), append(vals, v.Int64())
			case bson.TypeDouble:
				f := v.Double()
				if math.IsNaN(f) || math.IsInf(f, 0) {
					f = 0
				}
				keys, vals = append(keys, key), append(vals, int64(f))
			case bson.TypeDateTime:
				keys, vals = append(keys, key), append(vals, v.DateTime())
			case bson.TypeTimestamp:
				t, i := v.Timestamp()
				keys = append(keys, key, key+".inc")
				vals = append(vals, int64(t), int64(i))
			}
		}
	}
	walk("", doc)
	return keys, vals
}

// ---------------------------------------------------------------- accessors

// at returns a metric's value at a sample index, and whether the metric exists at all.
func (d *ftdcData) at(key string, i int) (int64, bool) {
	s := d.Series[key]
	if s == nil || i < 0 || i >= len(s.Values) {
		return 0, false
	}
	return s.Values[i], true
}

// has reports whether a metric is present and ever non-zero. "Present" alone is not a
// useful test: a replica-set field exists on a standalone as a row of zeros, and a chart
// of it says nothing except that somebody drew it.
func (d *ftdcData) has(key string) bool {
	s := d.Series[key]
	if s == nil {
		return false
	}
	for _, v := range s.Values {
		if v != 0 {
			return true
		}
	}
	return false
}

// last returns the final value of a metric, which for a gauge is "how it ended".
func (d *ftdcData) last(key string) (int64, bool) {
	s := d.Series[key]
	if s == nil || len(s.Values) == 0 {
		return 0, false
	}
	return s.Values[len(s.Values)-1], true
}

// keysWithPrefix lists the metrics under a subtree, sorted, which is how the UI offers a
// metric picker without shipping three thousand names it does not need.
func (d *ftdcData) keysWithPrefix(prefix string) []string {
	var out []string
	for k := range d.Series {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// window returns the first and last sample times.
func (d *ftdcData) window() (float64, float64) {
	if len(d.TS) == 0 {
		return 0, 0
	}
	return d.TS[0], d.TS[len(d.TS)-1]
}

// span is the wall-clock length of the capture.
func (d *ftdcData) span() time.Duration {
	a, b := d.window()
	return time.Duration((b - a) * float64(time.Second))
}

// unwrapRoles flattens 8.0's per-role grouping back to the layout every other version uses.
//
// Done once after decoding rather than in each accessor: the charts should not have to know
// that a capture came from a sharded 8.0 cluster, and a fallback key per chart would be
// eighty places to get it wrong instead of one.
func (d *ftdcData) unwrapRoles() {
	var wrapped []string
	for k := range d.Series {
		if _, ok := ftdcUnwrapRole(k); ok {
			wrapped = append(wrapped, k)
		}
	}
	if len(wrapped) == 0 {
		return
	}
	for _, k := range wrapped {
		flat, _ := ftdcUnwrapRole(k)
		// `start` and `end` exist under every group. They are collection timestamps rather
		// than metrics; keep the first and drop the rest instead of letting one group's
		// clobber another's.
		if _, clash := d.Series[flat]; clash {
			delete(d.Series, k)
			continue
		}
		s := d.Series[k]
		s.Key = flat
		d.Series[flat] = s
		delete(d.Series, k)
	}
}
