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

type vsModel struct {
	Source struct {
		Host       string `json:"host"`
		Engine     string `json:"engine"` // mysql | pxc
		CapturedAt string `json:"capturedAt,omitempty"`
	} `json:"source"`
	Summary struct {
		Facts    map[string]string  `json:"facts"`    // static: cpus, ram, version…
		Findings map[string]float64 `json:"findings"` // headline peaks
	} `json:"summary"`
	CPU         *vsTabbed            `json:"cpu,omitempty"`
	Disk        *vsTabbed            `json:"disk,omitempty"`
	Series      map[string]*vsSeries `json:"series"` // memory, swap, bufferPool, …
	LongQueries []map[string]string  `json:"longQueries,omitempty"`
	Deadlock    *vsDeadlock          `json:"deadlock,omitempty"`
	Available   map[string]bool      `json:"available"`
	Notes       []string             `json:"notes,omitempty"`
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

	// Static facts for the text summary.
	parsePtSummary(m, flatOf(bySuffix, "pt-summary.out"))
	parsePtMysqlSummary(m, flatOf(bySuffix, "pt-mysql-summary.out"))

	computeFindings(m)
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
	tabs := map[string]*vsSeries{}
	var order []string
	over := &vsSeries{Metrics: []string{"rKBs", "wKBs", "util"}, Unit: ""}
	for _, f := range files {
		block := 0
		blockDevs := map[string]bool{}
		var sumR, sumW, sumUtil float64
		nUtil := 0
		flush := func() {
			if len(blockDevs) == 0 {
				return
			}
			t := f.ts.Add(time.Duration(block) * time.Second).Unix()
			avg := 0.0
			if nUtil > 0 {
				avg = sumUtil / float64(nUtil)
			}
			over.Points = append(over.Points, vsPoint{T: t, V: map[string]float64{"rKBs": sumR, "wKBs": sumW, "util": math.Round(avg*10) / 10}})
			block++
			blockDevs = map[string]bool{}
			sumR, sumW, sumUtil, nUtil = 0, 0, 0, 0
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
			rs, rkb, rawait := num(c[1]), num(c[2]), num(c[5])
			ws, wkb, wawait := num(c[7]), num(c[8]), num(c[11])
			util := num(c[22])
			sumR += rkb
			sumW += wkb
			sumUtil += util
			nUtil++
			s := tabs[dev]
			if s == nil {
				s = &vsSeries{Metrics: []string{"rs", "ws", "rKBs", "wKBs", "await", "util"}, Unit: ""}
				tabs[dev] = s
				order = append(order, dev)
			}
			t := f.ts.Add(time.Duration(block) * time.Second).Unix()
			s.Points = append(s.Points, vsPoint{T: t, V: map[string]float64{
				"rs": rs, "ws": ws, "rKBs": rkb, "wKBs": wkb, "await": math.Max(rawait, wawait), "util": util}})
		}
		flush()
	}
	if len(over.Points) == 0 {
		return nil
	}
	sort.Strings(order)
	return &vsTabbed{Overall: over, Tabs: tabs, Order: order}
}

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
					// wsrep_flow_control_paused is a 0..1 ratio since last status; delta≈per-interval.
					d := sn.v["wsrep_flow_control_paused"] - snaps[i-1].v["wsrep_flow_control_paused"]
					v["flowControlPausedPct"] = math.Round(math.Max(d, 0)/dt*1000) / 10
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

func parseInnodbStatus(m *vsModel, groups ...[]namedFile) {
	var files []namedFile
	for _, g := range groups {
		files = append(files, g...)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ts.Before(files[j].ts) })
	hist := &vsSeries{Metrics: []string{"value"}, Unit: ""}
	var dead vsDeadlock
	for _, f := range files {
		text := string(f.data)
		if mm := histRe.FindStringSubmatch(text); mm != nil {
			hist.Points = append(hist.Points, vsPoint{T: f.ts.Unix(), V: map[string]float64{"value": num(mm[1])}})
		}
		if i := strings.Index(text, "LATEST DETECTED DEADLOCK"); i >= 0 {
			seg := text[i:]
			if j := strings.Index(seg, "------------\n"); j > 0 {
				seg = seg[:j+12]
			}
			// A deadlock section with an actual timestamp line (not the "no deadlock" note).
			if strings.Contains(seg, "TRANSACTION") {
				dead.Detected = true
				dead.When = f.ts.UTC().Format(time.RFC3339)
				dead.Text = lastLines(seg, 1600)
			}
		}
	}
	if len(hist.Points) > 0 {
		m.Series["historyList"] = hist
		m.Available["historyList"] = true
	}
	if dead.Detected {
		m.Deadlock = &dead
		m.Available["deadlock"] = true
	}
}

// ---- replication ----

func parseReplication(m *vsModel, files []namedFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].ts.Before(files[j].ts) })
	s := &vsSeries{Metrics: []string{"seconds"}, Unit: "s"}
	for _, f := range files {
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "Seconds_Behind_Master:") || strings.HasPrefix(line, "Seconds_Behind_Source:") {
				val := strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])
				if val == "" || val == "NULL" {
					continue
				}
				s.Points = append(s.Points, vsPoint{T: f.ts.Unix(), V: map[string]float64{"seconds": num(val)}})
			}
		}
	}
	if len(s.Points) > 0 {
		m.Series["replicationLag"] = s
		m.Available["replicationLag"] = true
	}
}

// ---- processlist: long-running queries + collapsed thread-state timeline ----

var tsLineRe = regexp.MustCompile(`^TS\s+[\d.]+\s+(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)

func parseProcesslist(m *vsModel, files []namedFile) {
	states := &vsSeries{Unit: ""}
	stateKeys := map[string]bool{}
	var longQ []map[string]string
	type row struct {
		user, db, command, state, info string
		timeSec                        float64
	}

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
				if rr.command == "Daemon" || rr.command == "Sleep" {
					continue
				}
				st := collapseState(rr.state)
				counts[st]++
				stateKeys[st] = true
				if rr.timeSec >= 5 && rr.command != "" && strings.ToUpper(rr.info) != "NULL" && rr.info != "" {
					longQ = append(longQ, map[string]string{
						"time": strconv.FormatFloat(rr.timeSec, 'f', 0, 64), "user": rr.user, "db": rr.db,
						"state": rr.state, "info": truncate(rr.info, 300)})
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
	if len(longQ) > 0 {
		sort.Slice(longQ, func(i, j int) bool { return num(longQ[i]["time"]) > num(longQ[j]["time"]) })
		if len(longQ) > 20 {
			longQ = longQ[:20]
		}
		m.LongQueries = longQ
		m.Available["longQueries"] = true
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
	}
	if s := m.Series["swap"]; s != nil {
		f["peakSwapUsedMB"] = round1(seriesMax(s, "used"))
	}
	if m.Disk != nil && m.Disk.Overall != nil {
		f["peakDiskUtilPct"] = round1(seriesMax(m.Disk.Overall, "util"))
	}
	if s := m.Series["bufferPool"]; s != nil {
		f["peakBpMissRatioPct"] = round1(seriesMax(s, "missRatioPct"))
	}
	if s := m.Series["historyList"]; s != nil {
		f["maxHistoryListLength"] = seriesMax(s, "value")
	}
	if s := m.Series["replicationLag"]; s != nil {
		f["maxReplicationLagSec"] = seriesMax(s, "seconds")
	}
	if s := m.Series["handlerReadRndNext"]; s != nil {
		f["peakHandlerReadRndNextPerSec"] = round1(seriesMax(s, "perSec"))
	}
	if m.Deadlock != nil && m.Deadlock.Detected {
		f["deadlockDetected"] = 1
	}
	if len(m.LongQueries) > 0 {
		f["maxLongQuerySec"] = num(m.LongQueries[0]["time"])
	}
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
	if !a.fileExists(dep.ContainerID, ptStalkFile) {
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
