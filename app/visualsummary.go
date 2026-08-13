package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Visual Summary — turn a pt-stalk/pt-summary/pt-mysql-summary archive into a normalized
// JSON model of timeline series (CPU, memory, swap, disk, InnoDB/MySQL metrics) that the
// frontend renders as charts. Every parser is tolerant: a missing or malformed file just
// omits its series (flagged in Available), never fails the whole parse.

// ---- model ----

type vsPoint struct {
	T int64              `json:"t"` // unix seconds
	V map[string]float64 `json:"v"`
}

type vsSeries struct {
	Metrics []string  `json:"metrics"`        // metric keys present in V (order matters for stacks)
	Unit    string    `json:"unit,omitempty"` // e.g. "%", "MB", "/s"
	Points  []vsPoint `json:"points"`
}

// vsTabbed is an "overall" series plus per-entity tabs (per-CPU, per-disk).
type vsTabbed struct {
	Overall *vsSeries            `json:"overall,omitempty"`
	Tabs    map[string]*vsSeries `json:"tabs,omitempty"`
	Order   []string             `json:"order,omitempty"` // tab keys in natural order
}

type vsDeadlock struct {
	Detected bool   `json:"detected"`
	When     string `json:"when,omitempty"`
	Text     string `json:"text,omitempty"`
}

// vsVerdict is a conclusion rather than a measurement — the difference between
// "8.5% of reads missed the buffer pool" and "the buffer pool is too small for
// this workload, and here is what those misses actually cost".
//
// The charts below a verdict are the evidence for it and stay exactly as they
// were; this exists because a wall of correct numbers is not the same thing as
// an answer, and the questions these were built to answer ("do I need a bigger
// buffer pool?") are answered by combining several of them.
type vsVerdict struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Level    string `json:"level"`    // "ok" | "info" | "warn" | "crit"
	Headline string `json:"headline"` // the number this turns on
	Detail   string `json:"detail"`   // why, and what to do — the two below, joined
	// Means and Action are Detail's two halves kept apart, because they answer
	// different questions and a reader looking at a chart usually wants only one
	// of them: Means explains what the series on the chart *is*, Action says what
	// to do about the value it reached. Verdicts assembled by advice() set both;
	// the handful built field-by-field set only Detail, and the UI falls back to
	// it. Detail stays populated either way so nothing downstream has to care.
	Means  string `json:"means,omitempty"`
	Action string `json:"action,omitempty"`
}

type vsModel struct {
	Source struct {
		Host       string `json:"host"`
		Engine     string `json:"engine"` // mysql | pxc
		CapturedAt string `json:"capturedAt,omitempty"`
		// ArchiveID and Note identify a *kept* capture. Two captures of one host
		// minutes apart are otherwise indistinguishable, which makes them
		// impossible to tell apart in a comparison.
		ArchiveID int64  `json:"archiveId,omitempty"`
		Note      string `json:"note,omitempty"`
	} `json:"source"`
	Summary struct {
		Facts    map[string]string  `json:"facts"`    // static: cpus, ram, version…
		Findings map[string]float64 `json:"findings"` // headline peaks
	} `json:"summary"`
	CPU         *vsTabbed            `json:"cpu,omitempty"`
	Disk        *vsTabbed            `json:"disk,omitempty"`
	Series      map[string]*vsSeries `json:"series"`                // memory, swap, bufferPool, …
	Processlist []map[string]string  `json:"processlist,omitempty"` // consolidated processlist (per thread+query)
	Digests     []map[string]string  `json:"digests,omitempty"`     // statements by rows examined
	InnodbTrx   []map[string]string  `json:"innodbTrx,omitempty"`   // per-session InnoDB transactions
	// LockWaits is pt-stalk's lock-wait report: who is blocking whom, for how
	// long, and — uniquely in a capture — how long the blocker has been idle
	// inside its transaction. Only ever populated when a lock wait was actually
	// in progress; see parseLockWaits.
	LockWaits []map[string]string `json:"lockWaits,omitempty"`
	// TrxCensus is every open transaction, from pt-stalk's unfiltered
	// INFORMATION_SCHEMA.INNODB_TRX dump. Unlike LockWaits it does not require
	// anybody to be blocked, and unlike InnodbTrx its age comes from an absolute
	// trx_started rather than a counter bounded by the capture. See parseTrxCensus.
	TrxCensus []map[string]string `json:"trxCensus,omitempty"`
	// TCP is the kernel's own view of the network during the capture, and
	// ErrorLog the notable lines from the server's error log — the two sources
	// that can see a degraded link, which the database's own counters cannot.
	TCP            map[string]string   `json:"tcp,omitempty"`
	ErrorLog       []map[string]string `json:"errorLog,omitempty"`
	ErrorLogCounts map[string]string   `json:"errorLogCounts,omitempty"`
	NetQueues      []map[string]string `json:"netQueues,omitempty"` // sockets with sustained Recv-Q/Send-Q
	Deadlock       *vsDeadlock         `json:"deadlock,omitempty"`
	Verdicts       []vsVerdict         `json:"verdicts,omitempty"`
	// Advisors explain one chart each — what it measures, what this capture's
	// numbers say, and what to change. Keyed by chart. See visualsummary_advice.go.
	Advisors  map[string]vsVerdict `json:"advisors,omitempty"`
	Available map[string]bool      `json:"available"`
	Notes     []string             `json:"notes,omitempty"`

	// vars is SHOW GLOBAL VARIABLES from the capture. Not serialized — it is
	// several hundred entries and the page needs a handful — but the verdicts
	// need it, because a counter without the setting it should be judged
	// against is not interpretable: a checkpoint age of 11 MB is healthy under
	// a 1 GiB redo log and nearly full under a 100 MiB one.
	vars map[string]string
}

// namedFile is one tar member: its trigger timestamp (from the filename) + contents.
type namedFile struct {
	base string    // "<host>/YYYY_MM_DD_HH_MM_SS-suffix"
	ts   time.Time // parsed from the YYYY_MM_DD_HH_MM_SS prefix (zero if absent)
	data []byte
}

var tsPrefixRe = regexp.MustCompile(`(\d{4})_(\d{2})_(\d{2})_(\d{2})_(\d{2})_(\d{2})-`)

// parsePtStalk unpacks a .tar.gz and builds the visual model. Resilient throughout.
func parsePtStalk(gzData []byte) (*vsModel, error) {
	gz, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	// bySuffix groups members by their trailing "-suffix"; also keep flat text files.
	bySuffix := map[string][]namedFile{}
	var host string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(tr, 64<<20))
		name := hdr.Name
		if host == "" {
			if i := strings.IndexByte(name, '/'); i > 0 {
				host = strings.TrimPrefix(name[:i], "ptstalk-")
			}
		}
		base := name
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			base = name[i+1:]
		}
		var suffix string
		var ts time.Time
		if m := tsPrefixRe.FindStringSubmatch(base); m != nil {
			suffix = base[len(m[0]):]
			ts = mustTime(m[1:])
		} else {
			suffix = base // flat files: pt-summary.out, pt-mysql-summary.out
		}
		bySuffix[suffix] = append(bySuffix[suffix], namedFile{base: name, ts: ts, data: data})
	}
	if len(bySuffix) == 0 {
		return nil, fmt.Errorf("archive contained no readable files")
	}
	for _, fs := range bySuffix {
		sort.Slice(fs, func(i, j int) bool { return fs[i].ts.Before(fs[j].ts) })
	}

	m := &vsModel{Series: map[string]*vsSeries{}, Available: map[string]bool{}}
	m.Summary.Facts = map[string]string{}
	m.Summary.Findings = map[string]float64{}
	m.vars = map[string]string{}
	m.Source.Host = host
	m.Source.Engine = "mysql"

	// OS series.
	m.CPU = parseCPU(bySuffix["mpstat"], bySuffix["vmstat"])
	setAvail(m, "cpu", m.CPU != nil && m.CPU.Overall != nil)
	memTotal, swapTotal := parseMemTotals(bySuffix["meminfo"])
	if s := parseMemory(bySuffix["vmstat"], memTotal); s != nil {
		m.Series["memory"] = s
		m.Available["memory"] = true
	}
	if s := parseSwap(bySuffix["vmstat"], swapTotal); s != nil {
		m.Series["swap"] = s
		m.Available["swap"] = true
	}
	m.Disk = parseDisk(bySuffix["iostat"])
	setAvail(m, "disk", m.Disk != nil && m.Disk.Overall != nil)

	// MySQL status series (from mysqladmin ext -i1 snapshots).
	snaps := parseMysqladmin(bySuffix["mysqladmin"])
	hasWsrep := deriveMysqlSeries(m, snaps)
	if hasWsrep {
		m.Source.Engine = "pxc"
	}

	// InnoDB status (sparse): history list length + latest deadlock.
	parseInnodbStatus(m, bySuffix["innodbstatus1"], bySuffix["innodbstatus2"])

	// Replication lag: slave-status (<=8.0) or replica-status (8.4+); PXC uses wsrep queue.
	parseReplication(m, append(append([]namedFile{}, bySuffix["slave-status"]...), bySuffix["replica-status"]...))

	// Processlist: long-running queries + collapsed thread-state timeline.
	parseProcesslist(m, bySuffix["processlist"])
	parseLockWaits(m, bySuffix["lock-waits"])
	parseTrxCensus(m, bySuffix["transactions"])
	parseNetstatS(m, bySuffix["netstat_s"])
	parseErrorLog(m, bySuffix["log_error"])

	// netstat: connection-state timeline + sockets with sustained Recv-Q/Send-Q.
	parseNetstat(m, bySuffix["netstat"])

	// Which statements did the work — the cause behind the counters above.
	parseDigests(m, bySuffix["digests"])

	// Static facts for the text summary.
	parsePtSummary(m, flatOf(bySuffix, "pt-summary.out"))
	parsePtMysqlSummary(m, flatOf(bySuffix, "pt-mysql-summary.out"))
	parseVariables(m, bySuffix["variables"])

	computeFindings(m)
	computeVerdicts(m)
	computeAdvisors(m)
	if t := earliestTS(bySuffix); !t.IsZero() {
		m.Source.CapturedAt = t.UTC().Format(time.RFC3339)
	}
	if len(m.Series) == 0 && m.CPU == nil && m.Disk == nil {
		return nil, fmt.Errorf("no recognizable pt-stalk data in archive")
	}
	return m, nil
}

func mustTime(g []string) time.Time {
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	return time.Date(n(g[0]), time.Month(n(g[1])), n(g[2]), n(g[3]), n(g[4]), n(g[5]), 0, time.UTC)
}

func setAvail(m *vsModel, k string, ok bool) { m.Available[k] = ok }

func flatOf(by map[string][]namedFile, name string) []byte {
	if fs := by[name]; len(fs) > 0 {
		return fs[0].data
	}
	return nil
}

func earliestTS(by map[string][]namedFile) time.Time {
	var best time.Time
	for _, fs := range by {
		for _, f := range fs {
			if f.ts.IsZero() {
				continue
			}
			if best.IsZero() || f.ts.Before(best) {
				best = f.ts
			}
		}
	}
	return best
}

// ---- OS parsers ----

// parseCPU builds overall + per-CPU %busy series. Prefers mpstat (timestamped, per-CPU),
// falling back to vmstat's us/sy/id/wa/st columns for the overall line only.
func parseCPU(mpstat, vmstat []namedFile) *vsTabbed {
	if t := parseMpstat(mpstat); t != nil {
		return t
	}
	// vmstat fallback: overall only.
	over := &vsSeries{Metrics: []string{"usr", "sys", "iowait", "steal", "idle"}, Unit: "%"}
	for _, f := range vmstat {
		for i, row := range vmstatRows(f.data) {
			if len(row) < 17 {
				continue
			}
			us, sy, id, wa, st := row[12], row[13], row[14], row[15], row[16]
			over.Points = append(over.Points, vsPoint{T: f.ts.Add(time.Duration(i) * time.Second).Unix(),
				V: map[string]float64{"usr": num(us), "sys": num(sy), "iowait": num(wa), "steal": num(st), "idle": num(id)}})
		}
	}
	if len(over.Points) == 0 {
		return nil
	}
	return &vsTabbed{Overall: over}
}

var mpHdrRe = regexp.MustCompile(`^\d\d:\d\d:\d\d\s+CPU\s`)
var mpRowRe = regexp.MustCompile(`^(\d\d:\d\d:\d\d)\s+(all|\d+)\s+(.*)$`)

func parseMpstat(files []namedFile) *vsTabbed {
	over := &vsSeries{Metrics: []string{"usr", "sys", "iowait", "steal", "idle"}, Unit: "%"}
	tabs := map[string]*vsSeries{}
	var order []string
	for _, f := range files {
		date := f.ts
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if mpHdrRe.MatchString(line) || strings.Contains(line, "%idle") {
				continue
			}
			mm := mpRowRe.FindStringSubmatch(line)
			if mm == nil {
				continue
			}
			cols := strings.Fields(mm[3])
			// %usr %nice %sys %iowait %irq %soft %steal %guest %gnice %idle
			if len(cols) < 10 {
				continue
			}
			usr, sys, iowait, steal, idle := num(cols[0]), num(cols[2]), num(cols[3]), num(cols[6]), num(cols[9])
			t := rowTime(date, mm[1])
			pt := vsPoint{T: t, V: map[string]float64{"usr": usr, "sys": sys, "iowait": iowait, "steal": steal, "idle": idle}}
			if mm[2] == "all" {
				over.Points = append(over.Points, pt)
			} else {
				s := tabs[mm[2]]
				if s == nil {
					s = &vsSeries{Metrics: over.Metrics, Unit: "%"}
					tabs[mm[2]] = s
					order = append(order, mm[2])
				}
				s.Points = append(s.Points, pt)
			}
		}
	}
	if len(over.Points) == 0 {
		return nil
	}
	sort.Slice(order, func(i, j int) bool { return num(order[i]) < num(order[j]) })
	return &vsTabbed{Overall: over, Tabs: tabs, Order: order}
}

// rowTime combines an mpstat HH:MM:SS with the file's capture date.
func rowTime(date time.Time, hms string) int64 {
	var h, mi, s int
	fmt.Sscanf(hms, "%d:%d:%d", &h, &mi, &s)
	return time.Date(date.Year(), date.Month(), date.Day(), h, mi, s, 0, time.UTC).Unix()
}

// vmstatRows returns the numeric data rows (skipping the two header lines).
func vmstatRows(data []byte) [][]string {
	var rows [][]string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 17 {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err != nil { // header rows have non-numeric first col
			continue
		}
		rows = append(rows, f)
	}
	// The first data row is the since-boot average — drop it for rate accuracy.
	if len(rows) > 1 {
		rows = rows[1:]
	}
	return rows
}

func parseMemTotals(meminfo []namedFile) (memKB, swapKB float64) {
	if len(meminfo) == 0 {
		return 0, 0
	}
	sc := bufio.NewScanner(bytes.NewReader(meminfo[0].data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			memKB = num(fields[1])
		case "SwapTotal:":
			swapKB = num(fields[1])
		}
	}
	return
}

// parseMemory: used/cache/buff/free in MB (used = total - free - buff - cache).
func parseMemory(vmstat []namedFile, memTotalKB float64) *vsSeries {
	s := &vsSeries{Metrics: []string{"used", "cache", "buff", "free"}, Unit: "MB"}
	for _, f := range vmstat {
		for i, row := range vmstatRows(f.data) {
			free, buff, cache := num(row[3]), num(row[4]), num(row[5])
			used := memTotalKB - free - buff - cache
			if memTotalKB == 0 {
				used = 0
			}
			mb := func(kb float64) float64 { return math.Round(kb/1024*10) / 10 }
			s.Points = append(s.Points, vsPoint{T: f.ts.Add(time.Duration(i) * time.Second).Unix(),
				V: map[string]float64{"used": mb(math.Max(used, 0)), "cache": mb(cache), "buff": mb(buff), "free": mb(free)}})
		}
	}
	if len(s.Points) == 0 {
		return nil
	}
	return s
}

// parseSwap: used (MB) + swap-in/out (KB/s) from vmstat si/so.
func parseSwap(vmstat []namedFile, swapTotalKB float64) *vsSeries {
	s := &vsSeries{Metrics: []string{"used", "in", "out"}, Unit: "MB"}
	any := false
	for _, f := range vmstat {
		for i, row := range vmstatRows(f.data) {
			swpd, si, so := num(row[2]), num(row[6]), num(row[7])
			if swpd > 0 || si > 0 || so > 0 {
				any = true
			}
			s.Points = append(s.Points, vsPoint{T: f.ts.Add(time.Duration(i) * time.Second).Unix(),
				V: map[string]float64{"used": math.Round(swpd/1024*10) / 10, "in": si, "out": so}})
		}
	}
	if len(s.Points) == 0 {
		return nil
	}
	_ = any // keep the series even if idle so the chart shows a flat baseline
	return s
}

var iostatDevRe = regexp.MustCompile(`^[a-zA-Z][\w-]*\s`)

// parseDisk: per-device r/s w/s rkB/s wkB/s await %util (blank-line-separated 1s blocks),
// plus an overall series (summed throughput + avg %util).
func parseDisk(files []namedFile) *vsTabbed {
	metrics := []string{"rs", "ws", "iops", "rKBs", "wKBs", "rAwait", "wAwait", "util"}
	tabs := map[string]*vsSeries{}
	var order []string
	over := &vsSeries{Metrics: metrics, Unit: ""}
	for _, f := range files {
		block := 0
		blockDevs := map[string]bool{}
		var sumRs, sumWs, sumRkb, sumWkb, sumRAw, sumWAw, sumUtil float64
		n := 0
		flush := func() {
			if len(blockDevs) == 0 {
				return
			}
			t := f.ts.Add(time.Duration(block) * time.Second).Unix()
			avg := func(s float64) float64 {
				if n == 0 {
					return 0
				}
				return math.Round(s/float64(n)*100) / 100
			}
			over.Points = append(over.Points, vsPoint{T: t, V: map[string]float64{
				"rs": r2(sumRs), "ws": r2(sumWs), "iops": r2(sumRs + sumWs), "rKBs": r2(sumRkb), "wKBs": r2(sumWkb),
				"rAwait": avg(sumRAw), "wAwait": avg(sumWAw), "util": avg(sumUtil)}})
			block++
			blockDevs = map[string]bool{}
			sumRs, sumWs, sumRkb, sumWkb, sumRAw, sumWAw, sumUtil, n = 0, 0, 0, 0, 0, 0, 0, 0
		}
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "Device") || strings.HasPrefix(line, "Linux") {
				continue
			}
			if strings.TrimSpace(line) == "" {
				flush()
				continue
			}
			if !iostatDevRe.MatchString(line) {
				continue
			}
			c := strings.Fields(line)
			if len(c) < 23 {
				continue
			}
			dev := c[0]
			if blockDevs[dev] { // safety: same device twice ⇒ new block
				flush()
			}
			blockDevs[dev] = true
			rs, rkb, rAwait := num(c[1]), num(c[2]), num(c[5])
			ws, wkb, wAwait := num(c[7]), num(c[8]), num(c[11])
			util := num(c[22])
			sumRs += rs
			sumWs += ws
			sumRkb += rkb
			sumWkb += wkb
			sumRAw += rAwait
			sumWAw += wAwait
			sumUtil += util
			n++
			s := tabs[dev]
			if s == nil {
				s = &vsSeries{Metrics: metrics, Unit: ""}
				tabs[dev] = s
				order = append(order, dev)
			}
			t := f.ts.Add(time.Duration(block) * time.Second).Unix()
			s.Points = append(s.Points, vsPoint{T: t, V: map[string]float64{
				"rs": rs, "ws": ws, "iops": r2(rs + ws), "rKBs": rkb, "wKBs": wkb, "rAwait": rAwait, "wAwait": wAwait, "util": util}})
		}
		flush()
	}
	if len(over.Points) == 0 {
		return nil
	}
	sort.Strings(order)
	return &vsTabbed{Overall: over, Tabs: tabs, Order: order}
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

// ---- MySQL (mysqladmin ext -i1) ----

type statSnap struct {
	t int64
	v map[string]float64
}

var admRowRe = regexp.MustCompile(`^\|\s*(\w+)\s*\|\s*([^|]*?)\s*\|$`)

// parseMysqladmin splits the file into per-second SHOW GLOBAL STATUS snapshots. Each
// snapshot begins at the "| Variable_name |" header; timestamps are synthesized at 1s.
func parseMysqladmin(files []namedFile) []statSnap {
	var out []statSnap
	for _, f := range files {
		var cur map[string]float64
		idx := 0
		push := func() {
			if cur != nil {
				out = append(out, statSnap{t: f.ts.Add(time.Duration(idx) * time.Second).Unix(), v: cur})
				idx++
			}
		}
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			mm := admRowRe.FindStringSubmatch(sc.Text())
			if mm == nil {
				continue
			}
			if mm[1] == "Variable_name" { // header ⇒ new snapshot
				push()
				cur = map[string]float64{}
				continue
			}
			if cur == nil {
				cur = map[string]float64{}
			}
			if v, err := strconv.ParseFloat(mm[2], 64); err == nil {
				cur[mm[1]] = v
			}
		}
		push()
	}
	return out
}

// deriveMysqlSeries turns status snapshots into gauge + per-second-rate series. Returns
// whether wsrep_* (Galera/PXC) counters were present.
func deriveMysqlSeries(m *vsModel, snaps []statSnap) bool {
	if len(snaps) == 0 {
		return false
	}
	hasWsrep := false
	if _, ok := snaps[0].v["wsrep_cluster_size"]; ok {
		hasWsrep = true
	}
	gauge := func(key, unit string, metrics []string, pick func(v map[string]float64) map[string]float64) {
		s := &vsSeries{Metrics: metrics, Unit: unit}
		for _, sn := range snaps {
			s.Points = append(s.Points, vsPoint{T: sn.t, V: pick(sn.v)})
		}
		if hasData(s) {
			m.Series[key] = s
			m.Available[key] = true
		}
	}
	// rate builds per-second deltas between consecutive 1s snapshots.
	rate := func(key, unit string, metrics []string, cols map[string]string) {
		s := &vsSeries{Metrics: metrics, Unit: unit}
		for i := 1; i < len(snaps); i++ {
			dt := float64(snaps[i].t - snaps[i-1].t)
			if dt <= 0 || dt > 5 { // skip the gap between iterations
				continue
			}
			v := map[string]float64{}
			ok := false
			for m2, col := range cols {
				a, okA := snaps[i-1].v[col]
				b, okB := snaps[i].v[col]
				if okA && okB {
					v[m2] = math.Max((b-a)/dt, 0)
					ok = true
				}
			}
			if ok {
				s.Points = append(s.Points, vsPoint{T: snaps[i].t, V: v})
			}
		}
		if hasData(s) {
			m.Series[key] = s
			m.Available[key] = true
		}
	}

	// Buffer pool: gauge pages + rate read-requests/disk-reads + derived miss ratio.
	bp := &vsSeries{Metrics: []string{"totalPages", "dataPages", "dirtyPages", "freePages", "readReqPerSec", "diskReadPerSec", "missRatioPct"}, Unit: ""}
	for i, sn := range snaps {
		v := map[string]float64{
			"totalPages": sn.v["Innodb_buffer_pool_pages_total"],
			"dataPages":  sn.v["Innodb_buffer_pool_pages_data"],
			"dirtyPages": sn.v["Innodb_buffer_pool_pages_dirty"],
			"freePages":  sn.v["Innodb_buffer_pool_pages_free"],
		}
		if i > 0 {
			dt := float64(sn.t - snaps[i-1].t)
			if dt > 0 && dt <= 5 {
				dReq := sn.v["Innodb_buffer_pool_read_requests"] - snaps[i-1].v["Innodb_buffer_pool_read_requests"]
				dRd := sn.v["Innodb_buffer_pool_reads"] - snaps[i-1].v["Innodb_buffer_pool_reads"]
				v["readReqPerSec"] = math.Max(dReq/dt, 0)
				v["diskReadPerSec"] = math.Max(dRd/dt, 0)
				if dReq > 0 {
					v["missRatioPct"] = math.Round(math.Max(dRd, 0)/dReq*1000) / 10
				}
			}
		}
		bp.Points = append(bp.Points, vsPoint{T: sn.t, V: v})
	}
	if hasData(bp) {
		m.Series["bufferPool"] = bp
		m.Available["bufferPool"] = true
	}

	gauge("threads", "", []string{"running", "connected"}, func(v map[string]float64) map[string]float64 {
		return map[string]float64{"running": v["Threads_running"], "connected": v["Threads_connected"]}
	})
	rate("qps", "/s", []string{"questions", "select", "insert", "update", "delete"}, map[string]string{
		"questions": "Questions", "select": "Com_select", "insert": "Com_insert", "update": "Com_update", "delete": "Com_delete"})
	rate("innodbRowOps", "/s", []string{"read", "inserted", "updated", "deleted"}, map[string]string{
		"read": "Innodb_rows_read", "inserted": "Innodb_rows_inserted", "updated": "Innodb_rows_updated", "deleted": "Innodb_rows_deleted"})
	rate("handlerReadRndNext", "/s", []string{"perSec"}, map[string]string{"perSec": "Handler_read_rnd_next"})
	// InnoDB's own view of its I/O. Worth its own series because it is the half
	// of the page-cache check that comes from MySQL; the other half is iostat,
	// and the whole point is that the two can disagree by three orders of
	// magnitude. See the pageCache verdict.
	rate("innodbIO", "B/s", []string{"read", "written"}, map[string]string{
		"read": "Innodb_data_read", "written": "Innodb_data_written"})
	rate("fsyncs", "/s", []string{"data", "log"}, map[string]string{
		"data": "Innodb_data_fsyncs", "log": "Innodb_os_log_fsyncs"})
	rate("networkThroughput", "B/s", []string{"received", "sent"}, map[string]string{"received": "Bytes_received", "sent": "Bytes_sent"})
	rate("rowLockWaits", "/s", []string{"perSec"}, map[string]string{"perSec": "Innodb_row_lock_waits"})
	rate("tmpDiskTables", "/s", []string{"perSec"}, map[string]string{"perSec": "Created_tmp_disk_tables"})
	rate("slowQueries", "/s", []string{"perSec"}, map[string]string{"perSec": "Slow_queries"})
	rate("abortedConns", "/s", []string{"clients", "connects"}, map[string]string{"clients": "Aborted_clients", "connects": "Aborted_connects"})

	if hasWsrep {
		g := &vsSeries{Metrics: []string{"flowControlPausedPct", "recvQueue", "certDepsDistance", "clusterSize"}, Unit: ""}
		for i, sn := range snaps {
			v := map[string]float64{
				"recvQueue":        sn.v["wsrep_local_recv_queue"],
				"certDepsDistance": sn.v["wsrep_cert_deps_distance"],
				"clusterSize":      sn.v["wsrep_cluster_size"],
			}
			if i > 0 {
				dt := float64(sn.t - snaps[i-1].t)
				if dt > 0 && dt <= 5 {
					// Derived from wsrep_flow_control_paused_ns, a monotonic
					// nanosecond counter, NOT from wsrep_flow_control_paused.
					//
					// The latter looks like the obvious source and is not: it is
					// the *fraction of time paused since the last status reset*,
					// so as the window lengthens it DECAYS — a live capture of a
					// node being paused a third of the time read 0.778, 0.763,
					// 0.748 on consecutive samples. Differencing that gives a
					// negative number, which clamped to zero, so the metric was
					// pinned at 0 and adviseGalera reported "keeping up with
					// cluster writes" for a node that was paused 32.5% of the
					// time and had received 613 flow-control messages. A false
					// negative on the one advisor that exists to catch this.
					d := sn.v["wsrep_flow_control_paused_ns"] - snaps[i-1].v["wsrep_flow_control_paused_ns"]
					// ns paused over ns elapsed, as a percentage.
					v["flowControlPausedPct"] = math.Round(math.Max(d, 0)/1e9/dt*1000) / 10
				}
			}
			g.Points = append(g.Points, vsPoint{T: sn.t, V: v})
		}
		if hasData(g) {
			m.Series["galera"] = g
			m.Available["galera"] = true
		}
	}
	return hasWsrep
}

func hasData(s *vsSeries) bool {
	for _, p := range s.Points {
		for _, v := range p.V {
			if v != 0 {
				return len(s.Points) > 0
			}
		}
	}
	// all-zero but present: keep only if we actually have points (flat baseline is informative)
	return len(s.Points) > 1
}

// ---- InnoDB status (sparse) ----

var histRe = regexp.MustCompile(`History list length\s+(\d+)`)
var ckptAgeRe = regexp.MustCompile(`Checkpoint age\s+(\d+)`)

// innodbMonRe matches a monitor-output header, e.g.
//
//	2026-07-06 22:32:47 132830254241344 INNODB MONITOR OUTPUT
//
// The leading datetime is the true capture time.
var innodbMonRe = regexp.MustCompile(`(?m)^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) \d+ INNODB MONITOR OUTPUT`)

// parseInnodbStatus reads every "INNODB MONITOR OUTPUT" block (a file holds 2), timestamps
// it from the block header, and extracts history list length, checkpoint age, and the
// latest detected deadlock.
func parseInnodbStatus(m *vsModel, groups ...[]namedFile) {
	var files []namedFile
	for _, g := range groups {
		files = append(files, g...)
	}
	hist := &vsSeries{Metrics: []string{"value"}, Unit: ""}
	ckpt := &vsSeries{Metrics: []string{"age"}, Unit: "bytes"}
	var dead vsDeadlock
	type block struct {
		ts   int64
		text string
	}
	var blocks []block
	for _, f := range files {
		text := string(f.data)
		locs := innodbMonRe.FindAllStringSubmatchIndex(text, -1)
		if len(locs) == 0 {
			blocks = append(blocks, block{ts: f.ts.Unix(), text: text})
			continue
		}
		for i, loc := range locs {
			t, _ := time.Parse("2006-01-02 15:04:05", text[loc[2]:loc[3]])
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			blocks = append(blocks, block{ts: t.UTC().Unix(), text: text[loc[0]:end]})
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].ts < blocks[j].ts })
	// Dedup blocks sharing a timestamp (innodbstatus1/2 overlap) so counts aren't doubled.
	seenTs := map[int64]bool{}
	uniq := blocks[:0]
	for _, b := range blocks {
		if seenTs[b.ts] {
			continue
		}
		seenTs[b.ts] = true
		uniq = append(uniq, b)
	}
	blocks = uniq

	// Per-session transactions, consolidated across captures by MySQL thread id (or trx id).
	type trxAgg struct {
		trxId, threadId, status, query string
		activeSec, rowLocks            float64
		lockWait                       bool
		seen                           int
	}
	trxConsol := map[string]*trxAgg{}
	var trxOrder []string

	for _, b := range blocks {
		if mm := histRe.FindStringSubmatch(b.text); mm != nil {
			hist.Points = append(hist.Points, vsPoint{T: b.ts, V: map[string]float64{"value": num(mm[1])}})
		}
		if mm := ckptAgeRe.FindStringSubmatch(b.text); mm != nil {
			ckpt.Points = append(ckpt.Points, vsPoint{T: b.ts, V: map[string]float64{"age": num(mm[1])}})
		}
		if i := strings.Index(b.text, "LATEST DETECTED DEADLOCK"); i >= 0 {
			seg := b.text[i:]
			if j := strings.Index(seg, "------------\n"); j > 0 {
				seg = seg[:j+12]
			}
			if strings.Contains(seg, "TRANSACTION") { // an actual deadlock, not the "no deadlock" note
				dead.Detected = true
				dead.When = time.Unix(b.ts, 0).UTC().Format(time.RFC3339)
				dead.Text = lastLines(seg, 1600)
			}
		}
		for _, t := range parseInnodbTrxBlock(b.text) {
			// "not started" is a session holding no transaction at all. InnoDB
			// lists them, and carrying them through fills the table with rows
			// that have no thread, no query and no age — a live capture with one
			// real long transaction produced five rows, four of them this. On a
			// busy server they would bury the row worth reading.
			if t.status == "not started" || (t.threadId == "" && t.activeSec == 0 && t.query == "") {
				continue
			}
			key := t.threadId
			if key == "" {
				key = "trx:" + t.trxId
			}
			a := trxConsol[key]
			if a == nil {
				a = &trxAgg{threadId: t.threadId}
				trxConsol[key] = a
				trxOrder = append(trxOrder, key)
			}
			a.trxId = t.trxId
			a.status = t.status
			a.seen++
			if t.activeSec > a.activeSec {
				a.activeSec = t.activeSec
			}
			if t.rowLocks > a.rowLocks {
				a.rowLocks = t.rowLocks
			}
			a.lockWait = a.lockWait || t.lockWait
			if t.query != "" {
				a.query = t.query
			}
		}
	}
	if len(hist.Points) > 0 {
		m.Series["historyList"] = hist
		m.Available["historyList"] = true
	}
	if len(ckpt.Points) > 0 {
		m.Series["checkpointAge"] = ckpt
		m.Available["checkpointAge"] = true
	}
	if dead.Detected {
		m.Deadlock = &dead
		m.Available["deadlock"] = true
	}
	if len(trxOrder) > 0 {
		var rows []map[string]string
		for _, key := range trxOrder {
			a := trxConsol[key]
			rows = append(rows, map[string]string{
				"thread": a.threadId, "trx": a.trxId, "status": a.status,
				"active": strconv.FormatFloat(a.activeSec, 'f', 0, 64), "rowLocks": strconv.FormatFloat(a.rowLocks, 'f', 0, 64),
				"lockWait": boolStr(a.lockWait), "seen": strconv.Itoa(a.seen), "query": truncate(a.query, 400)})
		}
		sort.Slice(rows, func(i, j int) bool { return num(rows[i]["active"]) > num(rows[j]["active"]) })
		m.InnodbTrx = rows
		m.Available["innodbTrx"] = true
	}
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

// trxRec is one parsed InnoDB transaction from the "LIST OF TRANSACTIONS FOR EACH SESSION" section.
type trxRec struct {
	trxId, threadId, status, query string
	activeSec, rowLocks            float64
	lockWait                       bool
}

var trxHeadRe = regexp.MustCompile(`^(\d+), (.*)$`)
var trxActiveRe = regexp.MustCompile(`ACTIVE (\d+) sec`)
var trxThreadRe = regexp.MustCompile(`MySQL thread id (\d+)`)
var trxRowLockRe = regexp.MustCompile(`(\d+) row lock\(s\)`)

func parseInnodbTrxBlock(text string) []trxRec {
	idx := strings.Index(text, "LIST OF TRANSACTIONS FOR EACH SESSION:")
	if idx < 0 {
		return nil
	}
	seg := text[idx:]
	if e := strings.Index(seg, "\n--------"); e > 0 {
		seg = seg[:e]
	}
	parts := strings.Split(seg, "---TRANSACTION ")
	var out []trxRec
	for _, part := range parts[1:] {
		lines := strings.Split(part, "\n")
		if len(lines) == 0 {
			continue
		}
		var rec trxRec
		if mm := trxHeadRe.FindStringSubmatch(strings.TrimSpace(lines[0])); mm != nil {
			rec.trxId = mm[1]
			rec.status = strings.TrimSpace(mm[2])
		} else {
			rec.status = strings.TrimSpace(lines[0])
		}
		if mm := trxActiveRe.FindStringSubmatch(rec.status); mm != nil {
			rec.activeSec = num(mm[1])
		}
		threadLine := -1
		for i := 1; i < len(lines); i++ {
			l := strings.TrimSpace(lines[i])
			if mm := trxThreadRe.FindStringSubmatch(l); mm != nil {
				rec.threadId = mm[1]
				threadLine = i
			}
			if mm := trxRowLockRe.FindStringSubmatch(l); mm != nil {
				rec.rowLocks = num(mm[1])
			}
			if strings.HasPrefix(l, "LOCK WAIT") {
				rec.lockWait = true
			}
		}
		// The query, when present, is the first non-boilerplate line after the thread line.
		if threadLine >= 0 {
			for i := threadLine + 1; i < len(lines); i++ {
				q := strings.TrimSpace(lines[i])
				if q == "" {
					continue
				}
				if strings.HasPrefix(q, "Trx ") || strings.HasPrefix(q, "mysql tables") || strings.HasPrefix(q, "---") {
					break
				}
				rec.query = q
				break
			}
		}
		out = append(out, rec)
	}
	return out
}

// ---- replication ----

func parseReplication(m *vsModel, files []namedFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].ts.Before(files[j].ts) })
	s := &vsSeries{Metrics: []string{"seconds"}, Unit: "s"}
	for _, f := range files {
		// Each file holds ~30 SHOW REPLICA/SLAVE STATUS captures, one per second. Use the
		// "TS <epoch> <datetime>" marker when present, else synthesize at 1s per capture.
		idx := 0
		var curTS int64
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if mm := tsLineRe.FindStringSubmatch(line); mm != nil {
				if t, err := time.Parse("2006-01-02 15:04:05", mm[1]); err == nil {
					curTS = t.UTC().Unix()
				}
				continue
			}
			if strings.HasPrefix(line, "Seconds_Behind_Master:") || strings.HasPrefix(line, "Seconds_Behind_Source:") {
				val := strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])
				if val == "" || val == "NULL" {
					idx++
					continue
				}
				t := curTS
				if t == 0 {
					t = f.ts.Add(time.Duration(idx) * time.Second).Unix()
				}
				s.Points = append(s.Points, vsPoint{T: t, V: map[string]float64{"seconds": num(val)}})
				idx++
			}
		}
	}
	if len(s.Points) > 0 {
		sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].T < s.Points[j].T })
		m.Series["replicationLag"] = s
		m.Available["replicationLag"] = true
	}
}

// ---- processlist: long-running queries + collapsed thread-state timeline ----

var tsLineRe = regexp.MustCompile(`^TS\s+[\d.]+\s+(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)

func parseProcesslist(m *vsModel, files []namedFile) {
	states := &vsSeries{Unit: ""}
	stateKeys := map[string]bool{}
	type row struct {
		id, user, db, command, state, info string
		timeSec                            float64
	}
	// Consolidate identical (thread Id + query) across all captures: a long-running query
	// recurs every second while it runs — collapse to one row, keeping the longest elapsed
	// time and how many captures it appeared in (the table stays fully sortable client-side).
	type plAgg struct {
		id, user, db, command, state, info string
		maxTime                            float64
		seen                               int
	}
	consol := map[string]*plAgg{}
	var order []string

	for _, f := range files {
		var curT int64
		var rows []row
		var r row
		haveRow := false
		flush := func() {
			if !haveRow && len(rows) == 0 {
				return
			}
			if haveRow {
				rows = append(rows, r)
			}
			// State counts for this sample (collapsed).
			counts := map[string]float64{}
			for _, rr := range rows {
				key := rr.id + "\x00" + rr.info
				a := consol[key]
				if a == nil {
					a = &plAgg{id: rr.id, user: rr.user, db: rr.db, command: rr.command, info: rr.info}
					consol[key] = a
					order = append(order, key)
				}
				a.state = rr.state
				a.seen++
				if rr.timeSec > a.maxTime {
					a.maxTime = rr.timeSec
				}
				if rr.command != "Daemon" && rr.command != "Sleep" {
					st := collapseState(rr.state)
					counts[st]++
					stateKeys[st] = true
				}
			}
			if curT != 0 && len(counts) > 0 {
				states.Points = append(states.Points, vsPoint{T: curT, V: counts})
			}
			rows = nil
			r = row{}
			haveRow = false
		}
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if mm := tsLineRe.FindStringSubmatch(line); mm != nil {
				flush()
				if t, err := time.Parse("2006-01-02 15:04:05", mm[1]); err == nil {
					curT = t.UTC().Unix()
				} else {
					curT = f.ts.Unix()
				}
				continue
			}
			if strings.Contains(line, ". row ***") {
				if haveRow {
					rows = append(rows, r)
				}
				r = row{}
				haveRow = true
				continue
			}
			k, v, ok := splitColon(line)
			if !ok {
				continue
			}
			switch k {
			case "Id":
				r.id = v
			case "User":
				r.user = v
			case "db":
				r.db = v
			case "Command":
				r.command = v
			case "State":
				r.state = v
			case "Info":
				r.info = v
			case "Time":
				r.timeSec = num(v)
			}
		}
		flush()
	}
	if len(states.Points) > 0 {
		var keys []string
		for k := range stateKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		states.Metrics = keys
		m.Series["threadStates"] = states
		m.Available["threadStates"] = true
	}
	if len(order) > 0 {
		var rows []map[string]string
		for _, key := range order {
			a := consol[key]
			info := a.info
			if strings.ToUpper(info) == "NULL" {
				info = ""
			}
			rows = append(rows, map[string]string{
				"id": a.id, "user": a.user, "db": a.db, "command": a.command, "state": a.state,
				"time": strconv.FormatFloat(a.maxTime, 'f', 0, 64), "seen": strconv.Itoa(a.seen),
				"info": truncate(info, 400),
			})
		}
		sort.Slice(rows, func(i, j int) bool { return num(rows[i]["time"]) > num(rows[j]["time"]) })
		if len(rows) > 300 {
			rows = rows[:300]
		}
		m.Processlist = rows
		m.Available["processlist"] = true
	}
}

// collapseState normalizes a processlist State into a small set of buckets.
func collapseState(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "NULL") {
		return "idle"
	}
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "waiting on empty queue"):
		return "idle"
	case strings.Contains(l, "sending data"):
		return "Sending data"
	case strings.Contains(l, "copying to tmp") || strings.Contains(l, "creating tmp"):
		return "Copying to tmp table"
	case strings.Contains(l, "sorting"):
		return "Sorting"
	case strings.Contains(l, "lock"):
		return "Waiting for lock"
	case strings.Contains(l, "statistics"):
		return "statistics"
	case strings.Contains(l, "opening tables") || strings.Contains(l, "closing tables"):
		return "Opening tables"
	case strings.Contains(l, "wsrep") || strings.Contains(l, "committing") || strings.Contains(l, "commit"):
		return "Committing"
	case strings.HasPrefix(l, "waiting"):
		return "Waiting"
	default:
		return s
	}
}

// ---- netstat ----

// parseNetstat builds a connection-state timeline (counts by TCP State per capture) and a
// socket-queue timeline (count of sockets with non-zero Recv-Q / Send-Q per capture), plus
// a table of sockets that showed a sustained backlog. Each capture is a "TS <epoch> …" block.
func parseNetstat(m *vsModel, files []namedFile) {
	states := &vsSeries{Unit: ""}
	stateKeys := map[string]bool{}
	queues := &vsSeries{Metrics: []string{"recvBacklog", "sendBacklog"}, Unit: ""}
	type qe struct {
		local, foreign, state, prog string
		maxRecv, maxSend            float64
		hits                        int
	}
	qmap := map[string]*qe{}
	var qorder []string

	for _, f := range files {
		var curT int64
		var counts map[string]float64
		var recvN, sendN float64
		flush := func() {
			if curT == 0 || counts == nil {
				return
			}
			cp := map[string]float64{}
			for k, v := range counts {
				cp[k] = v
			}
			states.Points = append(states.Points, vsPoint{T: curT, V: cp})
			queues.Points = append(queues.Points, vsPoint{T: curT, V: map[string]float64{"recvBacklog": recvN, "sendBacklog": sendN}})
		}
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if mm := tsLineRe.FindStringSubmatch(line); mm != nil {
				flush()
				if t, err := time.Parse("2006-01-02 15:04:05", mm[1]); err == nil {
					curT = t.UTC().Unix()
				} else {
					curT = f.ts.Unix()
				}
				counts = map[string]float64{}
				recvN, sendN = 0, 0
				continue
			}
			c := strings.Fields(line)
			if len(c) < 6 || !strings.HasPrefix(c[0], "tcp") {
				continue
			}
			recvQ, sendQ := num(c[1]), num(c[2])
			state := c[5]
			if counts == nil {
				counts = map[string]float64{}
			}
			counts[state]++
			stateKeys[state] = true
			if recvQ > 0 {
				recvN++
			}
			if sendQ > 0 {
				sendN++
			}
			if recvQ > 0 || sendQ > 0 {
				prog := ""
				if len(c) >= 7 {
					prog = c[6]
				}
				key := c[3] + "|" + c[4] + "|" + state
				e := qmap[key]
				if e == nil {
					e = &qe{local: c[3], foreign: c[4], state: state, prog: prog}
					qmap[key] = e
					qorder = append(qorder, key)
				}
				e.hits++
				e.maxRecv = math.Max(e.maxRecv, recvQ)
				e.maxSend = math.Max(e.maxSend, sendQ)
			}
		}
		flush()
	}

	if len(states.Points) > 0 {
		var keys []string
		for k := range stateKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		states.Metrics = keys
		m.Series["netStates"] = states
		m.Available["netStates"] = true
	}
	// Only surface the queue chart when a backlog actually occurred (else it is a flat zero).
	backlog := false
	for _, p := range queues.Points {
		if p.V["recvBacklog"] > 0 || p.V["sendBacklog"] > 0 {
			backlog = true
			break
		}
	}
	if backlog {
		m.Series["sockQueues"] = queues
		m.Available["sockQueues"] = true
	}
	// Sustained-backlog sockets: appeared with a non-zero queue in ≥2 captures.
	var rows []map[string]string
	for _, k := range qorder {
		e := qmap[k]
		if e.hits < 2 {
			continue
		}
		rows = append(rows, map[string]string{
			"local": e.local, "foreign": e.foreign, "state": e.state, "prog": e.prog,
			"maxRecv": strconv.FormatFloat(e.maxRecv, 'f', 0, 64), "maxSend": strconv.FormatFloat(e.maxSend, 'f', 0, 64),
			"hits": strconv.Itoa(e.hits)})
	}
	if len(rows) > 0 {
		sort.Slice(rows, func(i, j int) bool {
			return math.Max(num(rows[i]["maxRecv"]), num(rows[i]["maxSend"])) > math.Max(num(rows[j]["maxRecv"]), num(rows[j]["maxSend"]))
		})
		if len(rows) > 20 {
			rows = rows[:20]
		}
		m.NetQueues = rows
		m.Available["netQueues"] = true
	}
}

// ---- static facts ----

func parsePtSummary(m *vsModel, data []byte) {
	if data == nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Hostname |"):
			m.Summary.Facts["host"] = afterBar(line)
		case strings.HasPrefix(line, "Processors"):
			// e.g. "Processors | physical = 1, cores = 20, virtual = 20, …"
			m.Summary.Facts["processors"] = afterBar(line)
		case strings.HasPrefix(line, "Total | ") && m.Summary.Facts["memory"] == "":
			m.Summary.Facts["memory"] = afterBar(line)
		case strings.HasPrefix(line, "Kernel |"):
			m.Summary.Facts["kernel"] = afterBar(line)
		}
	}
}

func parsePtMysqlSummary(m *vsModel, data []byte) {
	if data == nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Version |"):
			m.Summary.Facts["mysqlVersion"] = afterBar(line)
		case strings.HasPrefix(line, "Uptime |"):
			m.Summary.Facts["uptime"] = afterBar(line)
		case strings.Contains(line, "buffer_pool_size") && m.Summary.Facts["bufferPoolSize"] == "":
			m.Summary.Facts["bufferPoolSize"] = afterBar(line)
		}
	}
}

// digestCols are the columns ptDigestSnippet selects, in order. mysql's default
// batch output is tab-separated with a header row, so the header is what is
// matched against rather than trusting column positions blindly.
var digestCols = []string{
	"schema", "digest", "count", "rowsExamined", "rowsSent",
	"noIndexUsed", "tmpDiskTables", "totalSec", "avgMs",
}

// parseDigests reads the performance_schema statement summary into rows the page
// renders as a table. Newest capture wins: the digest table is cumulative since
// the server started, so the last snapshot is the most complete one.
func parseDigests(m *vsModel, files []namedFile) {
	if len(files) == 0 {
		return
	}
	f := files[len(files)-1]
	sc := bufio.NewScanner(bytes.NewReader(f.data))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	first := true
	var out []map[string]string
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if first { // the header row mysql prints in batch mode
			first = false
			continue
		}
		cells := strings.Split(line, "\t")
		if len(cells) < len(digestCols) {
			continue
		}
		row := map[string]string{}
		for i, name := range digestCols {
			row[name] = strings.TrimSpace(cells[i])
		}
		// A statement nothing ran is noise in a table sorted by cost.
		if row["count"] == "0" || row["digest"] == "" {
			continue
		}
		out = append(out, row)
	}
	if len(out) > 0 {
		m.Digests = out
		m.Available["digests"] = true
	}
}

// parseVariables reads the SHOW GLOBAL VARIABLES dump pt-stalk takes on every
// trigger. Tab-separated name/value, one per line, with a couple of header
// lines that simply do not match and are skipped.
//
// Only a handful reach the page as facts; the rest stay in m.vars for the
// verdicts, which need settings and counters together to say anything.
func parseVariables(m *vsModel, files []namedFile) {
	if len(files) == 0 {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(files[0].data))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		m.vars[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	// The settings a reader needs next to the charts to interpret them at all.
	for _, v := range []struct{ key, fact string }{
		{"innodb_buffer_pool_size", "bufferPoolSize"},
		{"innodb_flush_method", "flushMethod"},
		{"innodb_redo_log_capacity", "redoLogCapacity"},
		{"sync_binlog", "syncBinlog"},
		{"innodb_flush_log_at_trx_commit", "flushLogAtTrxCommit"},
	} {
		if s := m.vars[v.key]; s != "" {
			m.Summary.Facts[v.fact] = s
		}
	}
}

// varNum reads one variable as a number, reporting whether it was there at all
// — 0 and absent mean different things for every setting here.
func (m *vsModel) varNum(key string) (float64, bool) {
	s, ok := m.vars[key]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err == nil
}

// redoCapacityBytes is the size of the redo log, from whichever setting this
// server expresses it with: innodb_redo_log_capacity on 8.0.30 and later, and
// the file-size × file-count pair before that.
func (m *vsModel) redoCapacityBytes() (float64, bool) {
	if v, ok := m.varNum("innodb_redo_log_capacity"); ok && v > 0 {
		return v, true
	}
	size, ok1 := m.varNum("innodb_log_file_size")
	n, ok2 := m.varNum("innodb_log_files_in_group")
	if ok1 && size > 0 {
		if !ok2 || n <= 0 {
			n = 2 // the historical default, and the common case
		}
		return size * n, true
	}
	return 0, false
}

// computeFindings derives the headline peaks shown as text tiles.
func computeFindings(m *vsModel) {
	f := m.Summary.Findings
	if m.CPU != nil && m.CPU.Overall != nil {
		max := 0.0
		for _, p := range m.CPU.Overall.Points {
			busy := 100 - p.V["idle"]
			if busy > max {
				max = busy
			}
		}
		f["peakCpuBusyPct"] = round1(max)
		f["cpuBusyPct"] = round1(100 - seriesMedian(m.CPU.Overall, "idle"))
		// iowait is what separates a server working from a server waiting, and
		// is the number that makes the two CPU figures above interpretable.
		f["cpuIowaitPct"] = round1(seriesMedian(m.CPU.Overall, "iowait"))
	}
	if s := m.Series["swap"]; s != nil {
		f["peakSwapUsedMB"] = round1(seriesMax(s, "used"))
	}
	if m.Disk != nil && m.Disk.Overall != nil {
		f["peakDiskUtilPct"] = round1(seriesMax(m.Disk.Overall, "util"))
		f["diskUtilPct"] = round1(seriesMedianSkipFirst(m.Disk.Overall, "util"))
	}
	if s := m.Series["bufferPool"]; s != nil {
		f["peakBpMissRatioPct"] = round1(seriesMax(s, "missRatioPct"))
		// The sustained figure alongside the peak. A miss ratio is noisy per
		// second and one spike should not be what a reader takes away, but the
		// peak still matters when it is the spike that hurt.
		f["bpMissRatioPct"] = round1(seriesMedian(s, "missRatioPct"))
		f["bpFreePages"] = seriesMedian(s, "freePages")
		f["bpDiskReadPerSec"] = round1(seriesMedian(s, "diskReadPerSec"))
	}
	// Throughput. Without it there is no way to tell which of two captures is
	// the faster server, which is the first thing anyone comparing two of them
	// wants to know.
	if s := m.Series["qps"]; s != nil {
		f["qps"] = round1(seriesMedian(s, "questions"))
	}
	// The two halves of the page-cache check, as plain numbers, so the verdict
	// that combines them can be checked by hand.
	if s := m.Series["innodbIO"]; s != nil {
		f["innodbReadMiBs"] = round1(seriesMedian(s, "read") / (1 << 20))
	}
	if m.Disk != nil && m.Disk.Overall != nil {
		f["deviceReadMiBs"] = round1(seriesMedianSkipFirst(m.Disk.Overall, "rKBs") / 1024)
	}
	if s := m.Series["fsyncs"]; s != nil {
		f["fsyncsPerSec"] = round1(seriesMedian(s, "data"))
	}
	// Checkpoint age means nothing as a byte count — it is only ever a fraction
	// of the redo log it is measured against.
	if s := m.Series["checkpointAge"]; s != nil {
		if cap, ok := m.redoCapacityBytes(); ok && cap > 0 {
			f["maxCheckpointAgePctOfRedo"] = round1(seriesMax(s, "age") / cap * 100)
		}
	}
	if s := m.Series["historyList"]; s != nil {
		f["maxHistoryListLength"] = seriesMax(s, "value")
	}
	if s := m.Series["checkpointAge"]; s != nil {
		f["maxCheckpointAgeBytes"] = seriesMax(s, "age")
	}
	if s := m.Series["replicationLag"]; s != nil {
		f["maxReplicationLagSec"] = seriesMax(s, "seconds")
	}
	if s := m.Series["handlerReadRndNext"]; s != nil {
		f["peakHandlerReadRndNextPerSec"] = round1(seriesMax(s, "perSec"))
		f["handlerReadRndNextPerSec"] = round1(seriesMedian(s, "perSec"))
	}
	if m.Deadlock != nil && m.Deadlock.Detected {
		f["deadlockDetected"] = 1
	}
	for _, r := range m.Processlist { // sorted by time desc; first running row with a query
		if r["info"] != "" && r["command"] != "Sleep" && r["command"] != "Daemon" {
			f["maxLongQuerySec"] = num(r["time"])
			break
		}
	}
}

// ---- verdicts ----

// Verdict levels, worst first when the page sorts them.
const (
	vsOK   = "ok"
	vsInfo = "info"
	vsWarn = "warn"
	vsCrit = "crit"
)

// computeVerdicts turns the parsed series into conclusions. Every rule follows
// the same shape: say nothing at all unless the data it needs is present, then
// state the number it turns on and one sentence of what to do about it.
//
// Each of these exists because the raw findings above led a reader — this one —
// to the wrong conclusion at least once while analysing real captures.
func computeVerdicts(m *vsModel) {
	for _, rule := range []func(*vsModel) *vsVerdict{
		verdictBufferPool,
		verdictPageCache,
		verdictRedoHeadroom,
		verdictScans,
		verdictSaturation,
		verdictThroughput,
	} {
		if v := rule(m); v != nil {
			m.Verdicts = append(m.Verdicts, *v)
		}
	}
}

// bufferPoolRoomyPct is the share of a pool that has to be sitting unallocated
// before "it never filled" is a fair description of it.
//
// It is 10% and not something smaller because InnoDB's page cleaner deliberately
// keeps a *small* free list on a busy server, so that a thread needing a page
// does not have to evict one first. A thrashing pool therefore hovers at a low
// but non-zero free count rather than at zero. Found by getting it wrong: a
// 128 MiB pool measured at 342 free pages of 8192 — 4.2%, and missing 8.3% of
// its reads at 150,000/s — which an earlier 1% threshold cheerfully called
// "never filled, not the constraint" about a server that ran 3x faster the
// moment the pool was raised.
const bufferPoolRoomyPct = 10.0

// verdictBufferPool answers "does this server need a bigger buffer pool?" from
// the two signals that only mean something together.
//
// The sustained miss ratio leads, because a pool that is missing reads is under
// pressure whatever its free list looks like. Free pages then separates the two
// ways of not missing: a pool with a large unallocated share never filled and
// cannot be too small, while a full pool that nonetheless almost never misses is
// simply sized correctly.
func verdictBufferPool(m *vsModel) *vsVerdict {
	s := m.Series["bufferPool"]
	if s == nil {
		return nil
	}
	free := seriesMedian(s, "freePages")
	total := seriesMedian(s, "totalPages")
	miss := seriesMedian(s, "missRatioPct")
	reads := seriesMedian(s, "diskReadPerSec")
	roomy := total > 0 && free/total*100 >= bufferPoolRoomyPct
	v := &vsVerdict{ID: "bufferPool", Title: "Buffer pool sizing"}

	if miss >= 1 {
		if miss >= 5 {
			v.Level = vsCrit
		} else {
			v.Level = vsWarn
		}
		v.Headline = fmt.Sprintf("%.2f%% of reads miss the pool (%s/s)", miss, compactNum(reads))
		v.Detail = "A sustained share of reads is not finding its page in the pool, so the " +
			"working set is larger than the pool holds. This is the signature of an undersized " +
			"buffer pool — but check what those misses actually cost before acting on it."
		if size, ok := m.varNum("innodb_buffer_pool_size"); ok {
			v.Detail += fmt.Sprintf(" Currently %s.", humanBytes(size))
		}
		if roomy {
			// Missing while a tenth of the pool sits unallocated is not a
			// sizing problem, and saying so prevents a pointless resize.
			v.Detail += fmt.Sprintf(" Note that %.0f%% of the pool is still free, so this is not "+
				"eviction pressure — a scan, or a pool that has not warmed up yet.", free/total*100)
		}
		return v
	}

	v.Level = vsOK
	if roomy {
		v.Headline = fmt.Sprintf("%.0f of %.0f pages still free", free, total)
		v.Detail = "The pool never filled during this capture and almost nothing missed it, so " +
			"it is not the constraint — a buffer pool with pages it has never had to allocate " +
			"cannot be too small. Raising it will not change anything here."
		return v
	}
	v.Headline = fmt.Sprintf("full, and only %.2f%% of reads miss it", miss)
	v.Detail = "A full pool is the normal steady state of a busy server; what matters is that " +
		"almost nothing is missing it. Sized correctly for this workload."
	return v
}

// verdictPageCache is the one that stops a reader acting on the verdict above
// without knowing what it is worth.
//
// InnoDB counts a buffer pool miss as a read whether or not that read reached a
// device. Under innodb_flush_method=fsync every miss goes through the operating
// system's page cache, and on a machine with spare memory most of them are
// served from there — memcpy, not seek. A real capture measured 156,086
// "disk reads/s" and 1,550 MiB/s of InnoDB reads while the block devices served
// nothing at all; the same server under O_DIRECT reported 598.8 MiB/s against
// 596.7 MiB/s of real device traffic, a 99.6% match.
//
// So: compare what InnoDB thinks it read against what the disks actually
// served, and say which of the two worlds this capture is in.
func verdictPageCache(m *vsModel) *vsVerdict {
	io := m.Series["innodbIO"]
	if io == nil || m.Disk == nil || m.Disk.Overall == nil {
		return nil
	}
	innodb := seriesMedian(io, "read") / (1 << 20)
	device := seriesMedianSkipFirst(m.Disk.Overall, "rKBs") / 1024
	// Nothing is being read at all — no misses to attribute, nothing to say.
	if innodb < 1 {
		return nil
	}
	method := m.vars["innodb_flush_method"]
	v := &vsVerdict{ID: "pageCache", Title: "Do buffer pool misses reach a disk?"}
	v.Headline = fmt.Sprintf("InnoDB reads %.0f MiB/s · devices serve %.0f MiB/s", innodb, device)

	if device >= innodb*0.75 {
		v.Level = vsOK
		v.Detail = "InnoDB's read counter and the block devices agree, so buffer pool misses are " +
			"real disk I/O here and Innodb_buffer_pool_reads means what it appears to mean."
		if method != "" {
			v.Detail += fmt.Sprintf(" innodb_flush_method=%s.", method)
		}
		return v
	}
	v.Level = vsWarn
	v.Detail = fmt.Sprintf(
		"The devices served only %.0f%% of what InnoDB reported reading, so most buffer pool "+
			"misses are being satisfied by the operating system's page cache rather than by "+
			"storage. Innodb_buffer_pool_reads is a miss counter, not a disk counter: the miss "+
			"ratio still correctly says the pool is too small, but it is not costing seek time, "+
			"and the same ratio on a machine with less free memory would cost far more.",
		device/innodb*100)
	if method != "" && !strings.EqualFold(method, "O_DIRECT") {
		v.Detail += fmt.Sprintf(" innodb_flush_method=%s is what puts the page cache in the path;"+
			" under O_DIRECT these two numbers converge.", method)
	}
	return v
}

// verdictRedoHeadroom reports checkpoint age against the redo log it is measured
// against, because the byte count alone is uninterpretable: 11 MB is 1% of a
// 1 GiB log and 11% of a 100 MiB one, and a fixed byte threshold can never fire
// on a small log however full it gets.
func verdictRedoHeadroom(m *vsModel) *vsVerdict {
	s := m.Series["checkpointAge"]
	if s == nil {
		return nil
	}
	capacity, ok := m.redoCapacityBytes()
	if !ok || capacity <= 0 {
		return nil
	}
	age := seriesMax(s, "age")
	pct := age / capacity * 100
	v := &vsVerdict{ID: "redo", Title: "Redo log headroom"}
	v.Headline = fmt.Sprintf("checkpoint age peaked at %.1f%% of %s", pct, humanBytes(capacity))
	switch {
	case pct >= 75:
		v.Level = vsCrit
		v.Detail = "InnoDB is close to running out of redo space, where it must force " +
			"synchronous flushing and write throughput collapses. Raise innodb_redo_log_capacity."
	case pct >= 50:
		v.Level = vsWarn
		v.Detail = "Over half the redo log is in use at peak, which leaves little room for a " +
			"write burst before InnoDB starts forcing checkpoints."
	default:
		v.Level = vsOK
		v.Detail = "Plenty of redo headroom for this write rate; the redo log is not limiting anything."
	}
	return v
}

// Thresholds for calling a statement wasteful. The ratio is what matters — a
// statement that examines a hundred rows to return one is doing ninety-nine
// rows of work nobody asked for — but it needs a floor underneath it, because
// examining 200 rows to return one is a rounding error, not a problem.
const (
	scanExaminedPerSentWarn = 100
	scanExaminedFloor       = 1e6
	scanRowsPerSecWarn      = 10000
)

// verdictScans names the statement doing the most reading, and says how much of
// that reading was wasted.
//
// Rows examined per row sent is the signal, not Handler_read_rnd_next. That
// counter only counts full *table* scans, and the worst statement found on a
// real server here — 615 million rows examined across 2,144 executions, 32
// minutes of CPU — is an *index* scan, which never touches it. The ratio
// catches both, and it is the number that decides how much of the dataset has
// to be in cache at all.
//
// This is the gap that made the counters frustrating: pt-stalk could say
// "126,000 rows/s are being read" and never say by what, so a reader had to go
// back to a server whose interesting moment had passed. With ptDigestSnippet in
// the capture the answer travels with the archive.
func verdictScans(m *vsModel) *vsVerdict {
	if len(m.Digests) > 0 {
		d := m.Digests[0] // ordered by rows examined; see ptDigestSnippet
		examined, sent := num(d["rowsExamined"]), num(d["rowsSent"])
		ratio := examined
		if sent > 0 {
			ratio = examined / sent
		}
		if examined < scanExaminedFloor || ratio < scanExaminedPerSentWarn {
			return nil
		}
		stmt := d["digest"]
		if len(stmt) > 160 {
			stmt = stmt[:160] + "…"
		}
		v := &vsVerdict{ID: "scans", Title: "Rows read to answer a query", Level: vsWarn,
			Headline: fmt.Sprintf("%.0f rows examined per row returned", ratio)}
		v.Detail = fmt.Sprintf(
			"%s examined %s rows across %s executions to return %s, costing %ss. Reading rows "+
				"nobody asked for costs CPU rather than I/O once they are cached, so it hides "+
				"behind a healthy buffer pool while still being the most expensive thing the "+
				"server does — and it is what decides how much of the dataset has to be cached "+
				"in the first place.",
			stmt, compactNum(examined), d["count"], compactNum(sent), d["totalSec"])
		return v
	}

	// No digests in this archive: fall back to the counter, which at least
	// catches full table scans.
	s := m.Series["handlerReadRndNext"]
	if s == nil {
		return nil
	}
	rows := seriesMedian(s, "perSec")
	if rows < scanRowsPerSecWarn {
		return nil
	}
	return &vsVerdict{ID: "scans", Title: "Rows read without an index", Level: vsWarn,
		Headline: fmt.Sprintf("%s rows/s scanned", compactNum(rows)),
		Detail: "Something is walking whole tables repeatedly. This capture has no statement " +
			"digests, so which query is doing it cannot be named from the archive alone — " +
			"re-capture with a build that collects performance_schema digests to find out.",
	}
}

// verdictSaturation names what this server was limited by, which is the thing a
// bare CPU percentage cannot say.
//
// A tile reading "peak CPU busy 77.7%" in warning colours was, in a real pair of
// captures, describing the *better* configuration: the one whose buffer pool
// held its working set, did no I/O at all, and delivered three times the
// throughput of the 49.3% run next to it. Busy is what a server that is getting
// work done looks like. What distinguishes healthy from sick is whether the time
// goes on work or on waiting, so this reads user+system against iowait and disk
// utilisation, and reports the constraint rather than the number.
func verdictSaturation(m *vsModel) *vsVerdict {
	if m.CPU == nil || m.CPU.Overall == nil {
		return nil
	}
	busy := 100 - seriesMedian(m.CPU.Overall, "idle")
	iowait := seriesMedian(m.CPU.Overall, "iowait")
	util := 0.0
	if m.Disk != nil && m.Disk.Overall != nil {
		util = seriesMedianSkipFirst(m.Disk.Overall, "util")
	}
	v := &vsVerdict{ID: "saturation", Title: "What is the limit here?"}
	v.Headline = fmt.Sprintf("CPU %.0f%% busy · %.0f%% iowait · disk %.0f%% util", busy, iowait, util)

	switch {
	case iowait >= 20 || util >= 80:
		v.Level = vsWarn
		v.Detail = "The processors are spending their time waiting for storage rather than " +
			"working, so this server is I/O-bound: faster queries will come from asking the " +
			"disks for less, not from more CPU."
	case busy >= 85:
		v.Level = vsWarn
		v.Detail = "Almost all processor time is going on work rather than waiting, so this " +
			"server is CPU-bound and close to its ceiling. Note that being busy is not by " +
			"itself a fault — check throughput before treating it as one."
	case busy >= 60:
		v.Level = vsOK
		v.Detail = "Working hard with little time spent waiting on storage, which is what a " +
			"healthy, well-cached server under load looks like — high CPU here is the good " +
			"outcome, not the problem."
	default:
		v.Level = vsOK
		v.Detail = "Neither the processors nor the disks are close to their limits during this " +
			"capture; whatever is limiting throughput is not saturation of either."
	}
	return v
}

// verdictThroughput is context rather than a problem — but without it there is
// no way to tell which of two captures is the faster server, which is the first
// thing anybody comparing two of them wants to know.
func verdictThroughput(m *vsModel) *vsVerdict {
	s := m.Series["qps"]
	if s == nil {
		return nil
	}
	qps := seriesMedian(s, "questions")
	if qps <= 0 {
		return nil
	}
	v := &vsVerdict{ID: "throughput", Title: "Throughput", Level: vsInfo,
		Headline: fmt.Sprintf("%s queries/s sustained", compactNum(qps))}
	v.Detail = "What this server actually delivered while it was captured. Compare it against " +
		"another capture before concluding that any setting helped."
	if f := m.Series["fsyncs"]; f != nil {
		v.Detail += fmt.Sprintf(" %s fsyncs/s.", compactNum(seriesMedian(f, "data")))
	}
	return v
}

func compactNum(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

func humanBytes(v float64) string {
	switch {
	case v >= 1<<30:
		return fmt.Sprintf("%.2f GiB", v/(1<<30))
	case v >= 1<<20:
		return fmt.Sprintf("%.0f MiB", v/(1<<20))
	case v >= 1<<10:
		return fmt.Sprintf("%.0f KiB", v/(1<<10))
	}
	return fmt.Sprintf("%.0f B", v)
}

// ---- small helpers ----

func num(s string) float64     { v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return v }
func round1(v float64) float64 { return math.Round(v*10) / 10 }

func seriesMax(s *vsSeries, key string) float64 {
	max := 0.0
	for _, p := range s.Points {
		if p.V[key] > max {
			max = p.V[key]
		}
	}
	return max
}

// seriesMedian is the sustained value of a metric — what the server was doing
// most of the time, as against seriesMax's worst single second.
func seriesMedian(s *vsSeries, key string) float64 {
	return medianOf(s.Points, key)
}

// seriesMean is the arithmetic mean across the capture. Use it for a metric
// whose *total* is what matters and whose occurrence is bursty, where a median
// is actively misleading: flow control is the case this exists for. A node
// paused a third of the whole capture is usually paused ~100% of a few seconds
// and 0% of the rest, so the median reads zero while the mean reads a third —
// and a third is the number that describes what the cluster experienced.
func seriesMean(s *vsSeries, key string) float64 {
	sum, n := 0.0, 0
	for _, p := range s.Points {
		if v, ok := p.V[key]; ok {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// seriesMedianSkipFirst drops the first point before taking the median. iostat's
// first report is an average since boot rather than an interval, so for a disk
// series that point is not a measurement of anything that happened during the
// capture and would drag the median toward the machine's whole history.
func seriesMedianSkipFirst(s *vsSeries, key string) float64 {
	if len(s.Points) <= 1 {
		return 0
	}
	return medianOf(s.Points[1:], key)
}

func medianOf(points []vsPoint, key string) float64 {
	vals := make([]float64, 0, len(points))
	for _, p := range points {
		if v, ok := p.V[key]; ok {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	mid := len(vals) / 2
	if len(vals)%2 == 1 {
		return vals[mid]
	}
	return (vals[mid-1] + vals[mid]) / 2
}

func afterBar(line string) string {
	if i := strings.IndexByte(line, '|'); i >= 0 {
		return strings.TrimSpace(line[i+1:])
	}
	return strings.TrimSpace(line)
}

func splitColon(line string) (k, v string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- HTTP ----

func (a *App) handleVisualUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := r.ParseMultipartForm(96 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid upload")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "no file provided")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 128<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	model, err := parsePtStalk(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "parse pt-stalk archive: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model)
}

func (a *App) handleVisualNode(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, "mysql")
	if !ok {
		return
	}
	if !a.fileExists(r.Context(), dep.ContainerID, ptStalkFile) {
		writeErr(w, http.StatusNotFound, "no pt-stalk capture on this node — run one first")
		return
	}
	data, err := a.readContainerFile(r.Context(), dep.ContainerID, ptStalkFile)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read capture: "+err.Error())
		return
	}
	model, err := parsePtStalk(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "parse pt-stalk archive: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// ---------------------------------------------------------------- lock waits

// parseLockWaits reads pt-stalk's `-lock-waits` file, which is the one place a
// capture carries authoritative transaction ages.
//
// pt-stalk runs two queries once a second, joining
// performance_schema.data_lock_waits against INFORMATION_SCHEMA.INNODB_TRX, and
// what comes back is better than anything in SHOW ENGINE INNODB STATUS: the
// blocker's thread, how many transactions it is holding up, how long the longest
// waiter has waited, both statements, the table — and idle_in_trx, which is
// seconds the blocking session has been *idle inside its transaction*. That last
// one is the "idle in transaction" signal by name, and InnoDB's status text has
// no equivalent.
//
// The join is an INNER JOIN on data_lock_waits, so this file only ever describes
// transactions in an active lock wait. A long transaction blocking nobody does
// not appear here at all — that case falls back to InnodbTrx, which is why
// adviseOldestTransaction reads both.
//
// Format, sampled once per second, two vertical (\G) result blocks per sample:
//
//	TS 1786597608.002923390 2026-08-13 05:06:48
//	*************************** 1. row ***************************
//	   who_blocks: thread 8214 from localhost
//	  idle_in_trx: 0
//	max_wait_time: 14
//	  num_waiters: 1
//	*************************** 1. row ***************************
//	    waiting_trx_id: 13213
//	    ...
//	   blocking_trx_id: 13212
//	      idle_in_trx: 0
//	   blocking_query: SELECT SLEEP(300)
//
// Rows are consolidated per blocking thread across the whole capture, keeping
// the worst value seen for each number — a wait that grows to 40s and then
// clears is a 40s wait, not an average.
func parseLockWaits(m *vsModel, files []namedFile) {
	type agg struct {
		blockingThread, blockingTrx, blockingQuery string
		waitingThread, waitingTrx, waitingQuery    string
		table                                      string
		maxWait, maxIdle, maxWaiters               float64
		seen                                       int
	}
	byBlocker := map[string]*agg{}
	var order []string

	for _, f := range files {
		// Each TS line starts a fresh sample; rows before the first are noise.
		for _, sample := range strings.Split(string(f.data), "\nTS ") {
			// Split the two \G blocks apart and read each as key: value pairs.
			for _, block := range strings.Split(sample, "*************************** 1. row ***************************") {
				kv := map[string]string{}
				for _, line := range strings.Split(block, "\n") {
					i := strings.Index(line, ":")
					if i <= 0 {
						continue
					}
					k := strings.TrimSpace(line[:i])
					if strings.Contains(k, " ") && !strings.Contains(k, "_") {
						continue // "TS 1786… 2026-08-13 05:06:48" and friends
					}
					kv[k] = strings.TrimSpace(line[i+1:])
				}
				bt := kv["blocking_thread"]
				if bt == "" {
					// The summary block names the blocker inside a sentence:
					// "thread 8214 from localhost".
					if w := kv["who_blocks"]; w != "" {
						fields := strings.Fields(w)
						if len(fields) >= 2 && fields[0] == "thread" {
							bt = fields[1]
						}
					}
				}
				if bt == "" {
					continue
				}
				a := byBlocker[bt]
				if a == nil {
					a = &agg{blockingThread: bt}
					byBlocker[bt] = a
					order = append(order, bt)
				}
				a.seen++
				setIfEmpty(&a.blockingTrx, kv["blocking_trx_id"])
				setIfEmpty(&a.blockingQuery, kv["blocking_query"])
				setIfEmpty(&a.waitingThread, kv["waiting_thread"])
				setIfEmpty(&a.waitingTrx, kv["waiting_trx_id"])
				setIfEmpty(&a.waitingQuery, kv["waiting_query"])
				// The value arrives as `schema`.`table`; trimming the ends would leave
				// the middle pair, so strip them all.
				setIfEmpty(&a.table, strings.ReplaceAll(kv["waiting_table_lock"], "`", ""))
				a.maxWait = maxF(a.maxWait, num(kv["wait_time"]), num(kv["max_wait_time"]))
				a.maxIdle = maxF(a.maxIdle, num(kv["idle_in_trx"]))
				a.maxWaiters = maxF(a.maxWaiters, num(kv["num_waiters"]))
			}
		}
	}
	if len(order) == 0 {
		return
	}
	var rows []map[string]string
	for _, k := range order {
		a := byBlocker[k]
		rows = append(rows, map[string]string{
			"blockingThread": a.blockingThread, "blockingTrx": a.blockingTrx,
			"blockingQuery": truncate(a.blockingQuery, 300),
			"waitingThread": a.waitingThread, "waitingTrx": a.waitingTrx,
			"waitingQuery": truncate(a.waitingQuery, 300),
			"table":        a.table,
			"waitSeconds":  strconv.FormatFloat(a.maxWait, 'f', 0, 64),
			"idleInTrx":    strconv.FormatFloat(a.maxIdle, 'f', 0, 64),
			"waiters":      strconv.FormatFloat(a.maxWaiters, 'f', 0, 64),
			"seen":         strconv.Itoa(a.seen),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return num(rows[i]["waitSeconds"]) > num(rows[j]["waitSeconds"])
	})
	m.LockWaits = rows
	m.Available["lockWaits"] = true
}

func setIfEmpty(dst *string, v string) {
	if *dst == "" && v != "" {
		*dst = v
	}
}

func maxF(vals ...float64) float64 {
	out := 0.0
	for _, v := range vals {
		if v > out {
			out = v
		}
	}
	return out
}

// parseTrxCensus reads pt-stalk's `-transactions` file: an unfiltered
// `SELECT * FROM INFORMATION_SCHEMA.INNODB_TRX ORDER BY trx_id\G` run once a
// second. It is the authoritative view of what was open, and it is strictly
// better than the two sources this report used before it.
//
// The `-lock-waits` file is a *join* against data_lock_waits, so it only
// describes transactions someone is queued behind. SHOW ENGINE INNODB STATUS
// gives an "ACTIVE n sec" string whose n is only as old as the capture. This
// file has neither limitation, and one column is the reason:
//
//	trx_started: 2026-08-13 05:06:29
//
// An absolute start time. A transaction open for three hours reads as three
// hours old in a ninety-second capture, which is the thing the other two sources
// genuinely cannot tell you.
//
// pt-stalk gates this collector on have_lock_waits_table — it probes for
// I_S.INNODB_LOCK_WAITS (MySQL 5.x) and falls back to
// performance_schema.data_lock_waits (8.0, where the I_S tables were removed).
// So the file is present on both, and absent only where InnoDB introspection is.
func parseTrxCensus(m *vsModel, files []namedFile) {
	type agg struct {
		id, state, thread, query, isolation string
		startedAt                           time.Time
		maxAgeSec                           float64
		maxRowsLocked, maxRowsModified      float64
		waited                              bool
		seen                                int
	}
	byTrx := map[string]*agg{}
	var order []string

	for _, f := range files {
		for _, sample := range strings.Split(string(f.data), "TS ") {
			// The TS line carries the wall clock this sample was taken at; the
			// age of a transaction is that minus its trx_started.
			sampleAt := time.Time{}
			if nl := strings.IndexByte(sample, '\n'); nl > 0 {
				fields := strings.Fields(sample[:nl])
				if len(fields) >= 3 {
					if t, err := time.Parse("2006-01-02 15:04:05", fields[1]+" "+fields[2]); err == nil {
						sampleAt = t
					}
				}
			}
			for _, row := range strings.Split(sample, "*************************** ") {
				kv := map[string]string{}
				for _, line := range strings.Split(row, "\n") {
					i := strings.Index(line, ":")
					if i <= 0 {
						continue
					}
					k := strings.TrimSpace(line[:i])
					if !strings.HasPrefix(k, "trx_") {
						continue
					}
					kv[k] = strings.TrimSpace(line[i+1:])
				}
				id := kv["trx_id"]
				if id == "" {
					continue
				}
				a := byTrx[id]
				if a == nil {
					a = &agg{id: id}
					byTrx[id] = a
					order = append(order, id)
				}
				a.seen++
				a.state = kv["trx_state"]
				setIfEmpty(&a.thread, kv["trx_mysql_thread_id"])
				setIfEmpty(&a.isolation, kv["trx_isolation_level"])
				if q := kv["trx_query"]; q != "" && q != "NULL" {
					a.query = q
				}
				if kv["trx_wait_started"] != "" && kv["trx_wait_started"] != "NULL" {
					a.waited = true
				}
				a.maxRowsLocked = maxF(a.maxRowsLocked, num(kv["trx_rows_locked"]))
				a.maxRowsModified = maxF(a.maxRowsModified, num(kv["trx_rows_modified"]))
				if st, err := time.Parse("2006-01-02 15:04:05", kv["trx_started"]); err == nil {
					a.startedAt = st
					if !sampleAt.IsZero() {
						a.maxAgeSec = maxF(a.maxAgeSec, sampleAt.Sub(st).Seconds())
					}
				}
			}
		}
	}
	if len(order) == 0 {
		return
	}
	var rows []map[string]string
	for _, k := range order {
		a := byTrx[k]
		started := ""
		if !a.startedAt.IsZero() {
			started = a.startedAt.UTC().Format("2006-01-02 15:04:05")
		}
		rows = append(rows, map[string]string{
			"trx": a.id, "state": a.state, "thread": a.thread,
			"startedAt":    started,
			"ageSec":       strconv.FormatFloat(a.maxAgeSec, 'f', 0, 64),
			"rowsLocked":   strconv.FormatFloat(a.maxRowsLocked, 'f', 0, 64),
			"rowsModified": strconv.FormatFloat(a.maxRowsModified, 'f', 0, 64),
			"isolation":    a.isolation, "waited": boolStr(a.waited),
			"seen":  strconv.Itoa(a.seen),
			"query": truncate(a.query, 300),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return num(rows[i]["ageSec"]) > num(rows[j]["ageSec"]) })
	m.TrxCensus = rows
	m.Available["trxCensus"] = true
}

// compactDurationSec renders a transaction age the way a person would say it.
// "open 7h 12m" is a different sentence from "open 25920s", and this advisor's
// whole point is that the age can now be much larger than the capture.
func compactDurationSec(sec float64) string {
	switch {
	case sec >= 3600:
		h := int(sec) / 3600
		mn := (int(sec) % 3600) / 60
		return fmt.Sprintf("%dh %dm", h, mn)
	case sec >= 60:
		return fmt.Sprintf("%dm %ds", int(sec)/60, int(sec)%60)
	}
	return fmt.Sprintf("%.0fs", sec)
}

// ------------------------------------------------------- TCP + error log

// parseNetstatS reads pt-stalk's `-netstat_s` file: `netstat -s` sampled once a
// loop. The counters are cumulative since boot, so what matters is the
// difference across the capture, not the absolute values.
//
// This exists to answer a question nothing else in a capture can. §242
// established that a degraded network link produces no Galera verdict at all —
// flow control stays at zero because a slow link starves the receiver rather
// than flooding it. Retransmits are the direct signature of the loss itself,
// measured by the kernel rather than inferred from the database's behaviour.
func parseNetstatS(m *vsModel, files []namedFile) {
	// Labels as `netstat -s` prints them, all under Tcp:.
	want := map[string]string{
		"segments sent out":                             "segmentsSent",
		"segments retransmitted":                        "segmentsRetransmitted",
		"fast retransmits":                              "fastRetransmits",
		"failed connection attempts":                    "failedConnects",
		"connection resets received":                    "resetsReceived",
		"resets sent":                                   "resetsSent",
		"times the listen queue of a socket overflowed": "listenOverflows",
	}
	first, last := map[string]float64{}, map[string]float64{}
	haveFirst := false
	for _, f := range files {
		for _, sample := range strings.Split(string(f.data), "TS ") {
			cur := map[string]float64{}
			for _, line := range strings.Split(sample, "\n") {
				l := strings.TrimSpace(line)
				for label, key := range want {
					if strings.HasSuffix(l, label) {
						cur[key] = num(strings.Fields(l)[0])
					}
				}
			}
			if len(cur) == 0 {
				continue
			}
			if !haveFirst {
				first, haveFirst = cur, true
			}
			last = cur
		}
	}
	if !haveFirst {
		return
	}
	delta := map[string]float64{}
	for k, v := range last {
		// Counters reset if the machine rebooted mid-capture; a negative delta
		// is not a measurement, so fall back to the absolute value.
		if d := v - first[k]; d >= 0 {
			delta[k] = d
		} else {
			delta[k] = v
		}
	}
	sent, retrans := delta["segmentsSent"], delta["segmentsRetransmitted"]
	pct := 0.0
	if sent > 0 {
		pct = retrans / sent * 100
	}
	m.TCP = map[string]string{
		"segmentsSent":          strconv.FormatFloat(sent, 'f', 0, 64),
		"segmentsRetransmitted": strconv.FormatFloat(retrans, 'f', 0, 64),
		"retransmitPct":         strconv.FormatFloat(math.Round(pct*1000)/1000, 'f', -1, 64),
		"fastRetransmits":       strconv.FormatFloat(delta["fastRetransmits"], 'f', 0, 64),
		"failedConnects":        strconv.FormatFloat(delta["failedConnects"], 'f', 0, 64),
		"resetsReceived":        strconv.FormatFloat(delta["resetsReceived"], 'f', 0, 64),
		"resetsSent":            strconv.FormatFloat(delta["resetsSent"], 'f', 0, 64),
		"listenOverflows":       strconv.FormatFloat(delta["listenOverflows"], 'f', 0, 64),
	}
	m.Available["tcp"] = true
}

// errorLogPatterns are the lines worth pulling out of a MySQL error log, with
// the category each belongs to. Ordered: the first match wins, so the more
// specific patterns come first.
//
// Cluster membership is at the top because it is the failure §242 could produce
// and this report could not see. When a link degrades badly enough, Galera does
// not report flow control — it reports an eviction, here, in words.
var errorLogPatterns = []struct {
	re  *regexp.Regexp
	cat string
}{
	{regexp.MustCompile(`(?i)mysqld got signal|InnoDB: Assertion|Assertion failure|got exception|corrupt`), "crash"},
	{regexp.MustCompile(`(?i)suspecting node|declaring .* inactive|forgetting|evicting|left the group|no longer in the group`), "membership"},
	{regexp.MustCompile(`(?i)non-primary|Primary-Component|view\(.*memb|cluster view|partition`), "membership"},
	{regexp.MustCompile(`(?i)state transfer|SST|IST .*(started|complete|fail)`), "state transfer"},
	{regexp.MustCompile(`(?i)aborted connection|Got an error reading communication|Got timeout reading`), "connections"},
	{regexp.MustCompile(`(?i)deadlock|lock wait timeout`), "locking"},
	{regexp.MustCompile(`(?i)\[ERROR\]`), "error"},
}

// parseErrorLog reads the tail of the server's error log that pt-stalk captured
// with `tail -f` for the duration of the run. Only lines matching a curated set
// of patterns are kept: an error log is mostly startup noise, and a report that
// showed all of it would be showing nothing.
func parseErrorLog(m *vsModel, files []namedFile) {
	counts := map[string]int{}
	var rows []map[string]string
	seen := map[string]bool{}
	for _, f := range files {
		for _, line := range strings.Split(string(f.data), "\n") {
			l := strings.TrimSpace(line)
			if l == "" {
				continue
			}
			severity := ""
			if i := strings.Index(l, "[Warning]"); i >= 0 {
				severity = "warning"
			}
			if i := strings.Index(l, "[ERROR]"); i >= 0 {
				severity = "error"
				_ = i
			}
			cat := ""
			for _, p := range errorLogPatterns {
				if p.re.MatchString(l) {
					cat = p.cat
					break
				}
			}
			if cat == "" {
				continue
			}
			counts[cat]++
			// Collapse repeats: an error log that says the same thing four
			// hundred times is one finding, with a count.
			key := cat + "|" + collapseLogLine(l)
			if seen[key] {
				continue
			}
			seen[key] = true
			ts, msg := splitLogLine(l)
			rows = append(rows, map[string]string{
				"time": ts, "category": cat, "severity": severity,
				"message": truncate(msg, 300),
			})
		}
	}
	if len(rows) == 0 {
		return
	}
	if len(rows) > 60 {
		rows = rows[:60]
	}
	m.ErrorLog = rows
	m.ErrorLogCounts = map[string]string{}
	for k, v := range counts {
		m.ErrorLogCounts[k] = strconv.Itoa(v)
	}
	m.Available["errorLog"] = true
}

// splitLogLine separates the leading timestamp from the message, so the table
// can show them in their own columns.
func splitLogLine(l string) (ts, msg string) {
	fields := strings.Fields(l)
	if len(fields) > 0 && strings.Contains(fields[0], "T") && strings.Contains(fields[0], ":") {
		return fields[0], strings.TrimSpace(strings.TrimPrefix(l, fields[0]))
	}
	return "", l
}

// collapseLogLine strips the parts that differ between otherwise identical
// messages — the timestamp, the thread id, and any bare numbers — so repeats
// collapse to one row.
var logNumRe = regexp.MustCompile(`\d+`)

func collapseLogLine(l string) string {
	_, msg := splitLogLine(l)
	return logNumRe.ReplaceAllString(msg, "#")
}
