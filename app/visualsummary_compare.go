package main

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
)

// Head-to-head comparison of two or more captures.
//
// The two-archive diff added earlier answers "what changed" by putting numbers
// side by side and leaving the reader to decide what they mean. That is the
// wrong division of labour for the question people actually take captures to
// answer, which is "did that change help?" — a question with a yes/no answer
// that the numbers support rather than replace.
//
// So a comparison carries advisors of its own. They read the *deltas* rather
// than any single capture, which lets them say things no single-capture advisor
// can: that throughput tripled and the miss ratio collapsed at the same time, so
// the buffer pool was the constraint; or that throughput moved 3% between two
// runs, which is noise and not a result.

// vsCompareMetric is one row of the comparison table.
type vsCompareMetric struct {
	Key    string    `json:"key"`
	Label  string    `json:"label"`
	Unit   string    `json:"unit"`
	Values []float64 `json:"values"` // one per capture, NaN-free (missing → null via Present)
	Have   []bool    `json:"have"`
	// ChangePct is the last capture against the first, when both are present.
	ChangePct  *float64 `json:"changePct,omitempty"`
	Better     string   `json:"better"` // "up" | "down" | "" (neither is better)
	Improved   *bool    `json:"improved,omitempty"`
	Meaningful bool     `json:"meaningful"` // moved more than noise
}

// vsComparison is the whole head-to-head payload.
type vsComparison struct {
	Captures []vsCaptureRef    `json:"captures"`
	Settings []vsSettingDiff   `json:"settings"`
	Metrics  []vsCompareMetric `json:"metrics"`
	Verdicts []vsVerdict       `json:"verdicts"`
}

type vsCaptureRef struct {
	ArchiveID  int64  `json:"archiveId,omitempty"`
	Host       string `json:"host"`
	CapturedAt string `json:"capturedAt,omitempty"`
	Note       string `json:"note,omitempty"`
}

type vsSettingDiff struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Values []string `json:"values"`
	Bytes  bool     `json:"bytes"`
}

// compareNoisePct is how much a figure has to move before it is a result rather
// than run-to-run variation.
//
// Ten percent, and the number is not arbitrary: a bit-for-bit repeat of one
// configuration an hour later measured 2.9x different on this project's own
// hardware, purely because the OS page cache had warmed. Ten percent is a floor
// under the obvious noise, not a claim that anything above it is significant.
const compareNoisePct = 10.0

// comparedSettings are the settings a comparison is nearly always about.
var comparedSettings = []struct {
	fact, label string
	bytes       bool
}{
	{"bufferPoolSize", "innodb_buffer_pool_size", true},
	{"flushMethod", "innodb_flush_method", false},
	{"redoLogCapacity", "innodb_redo_log_capacity", true},
	{"syncBinlog", "sync_binlog", false},
	{"flushLogAtTrxCommit", "innodb_flush_log_at_trx_commit", false},
}

// comparedMetrics are the figures worth putting side by side, with the direction
// that counts as better. "" means neither direction is an improvement on its own
// — CPU busy is the standing example, since it rises on a server that got faster.
var comparedMetrics = []struct {
	key, label, unit, better string
}{
	{"qps", "Throughput", "/s", "up"},
	{"bpMissRatioPct", "Buffer pool read-miss", "%", "down"},
	{"bpDiskReadPerSec", "Buffer pool misses", "/s", "down"},
	{"bpFreePages", "Buffer pool free pages", "", ""},
	{"innodbReadMiBs", "InnoDB read", " MiB/s", ""},
	{"deviceReadMiBs", "Device read", " MiB/s", ""},
	{"fsyncsPerSec", "fsyncs", "/s", ""},
	{"cpuBusyPct", "CPU busy", "%", ""},
	{"cpuIowaitPct", "CPU iowait", "%", "down"},
	{"diskUtilPct", "Disk utilisation", "%", "down"},
	{"maxCheckpointAgePctOfRedo", "Checkpoint age of redo", "%", "down"},
	{"handlerReadRndNextPerSec", "Rows scanned without index", "/s", "down"},
	{"maxHistoryListLength", "History list length", "", "down"},
	{"maxLongQuerySec", "Longest query", " s", "down"},
}

// buildComparison assembles the head-to-head. Order is the caller's: first is
// the baseline everything else is measured against.
func buildComparison(models []*vsModel) *vsComparison {
	if len(models) < 2 {
		return nil
	}
	c := &vsComparison{}
	for _, m := range models {
		c.Captures = append(c.Captures, vsCaptureRef{
			ArchiveID: m.Source.ArchiveID, Host: m.Source.Host,
			CapturedAt: m.Source.CapturedAt, Note: m.Source.Note,
		})
	}

	// Settings, but only the ones that actually differ — a table of twenty
	// identical rows hides the one that changed.
	for _, s := range comparedSettings {
		vals := make([]string, len(models))
		differs := false
		for i, m := range models {
			vals[i] = m.Summary.Facts[s.fact]
			if i > 0 && vals[i] != vals[0] {
				differs = true
			}
		}
		if differs {
			c.Settings = append(c.Settings, vsSettingDiff{Key: s.fact, Label: s.label, Values: vals, Bytes: s.bytes})
		}
	}

	for _, def := range comparedMetrics {
		row := vsCompareMetric{Key: def.key, Label: def.label, Unit: def.unit, Better: def.better}
		any := false
		for _, m := range models {
			v, ok := m.Summary.Findings[def.key]
			row.Values = append(row.Values, v)
			row.Have = append(row.Have, ok)
			any = any || ok
		}
		if !any {
			continue
		}
		// Baseline against the newest; the captures in between are shown but do
		// not define the change.
		first, last := 0, len(row.Have)-1
		if row.Have[first] && row.Have[last] && row.Values[first] != 0 {
			pct := (row.Values[last] - row.Values[first]) / math.Abs(row.Values[first]) * 100
			row.ChangePct = &pct
			row.Meaningful = math.Abs(pct) >= compareNoisePct
			if def.better != "" {
				improved := (def.better == "up" && pct > 0) || (def.better == "down" && pct < 0)
				row.Improved = &improved
			}
		}
		c.Metrics = append(c.Metrics, row)
	}
	c.Verdicts = compareVerdicts(models, c)
	return c
}

// findingOf reads one finding, reporting whether it was there.
func findingOf(m *vsModel, key string) (float64, bool) {
	v, ok := m.Summary.Findings[key]
	return v, ok
}

// delta is the last capture against the first for one finding.
func delta(models []*vsModel, key string) (from, to, pct float64, ok bool) {
	from, okA := findingOf(models[0], key)
	to, okB := findingOf(models[len(models)-1], key)
	if !okA || !okB || from == 0 {
		return from, to, 0, false
	}
	return from, to, (to - from) / math.Abs(from) * 100, true
}

// compareVerdicts is the part that answers "did it help".
func compareVerdicts(models []*vsModel, c *vsComparison) []vsVerdict {
	var out []vsVerdict

	// The headline: did throughput move, and is the move bigger than noise?
	if from, to, pct, ok := delta(models, "qps"); ok {
		v := vsVerdict{ID: "compareThroughput", Title: "Did throughput change?"}
		v.Headline = fmt.Sprintf("%s -> %s queries/s (%+.0f%%)", compactNum(from), compactNum(to), pct)
		switch {
		case math.Abs(pct) < compareNoisePct:
			v.Level = vsInfo
			v.Detail = fmt.Sprintf(
				"Inside the %.0f%% band this tool treats as noise. A bit-for-bit repeat of one "+
					"configuration measured almost three times different on this project's own "+
					"hardware once the page cache had warmed, so a move this size is not "+
					"evidence of anything. Repeat both captures before believing it.", compareNoisePct)
		case pct > 0:
			v.Level = vsOK
			v.Detail = "The later capture did more work per second. Check the settings that " +
				"differ, and the panels below, to see which of them can account for it."
		default:
			v.Level = vsWarn
			v.Detail = "The later capture did less work per second. If that was not the intent, " +
				"the changed settings below are the first place to look."
		}
		out = append(out, v)
	}

	// Buffer pool: the classic before/after, and the one where cause and effect
	// can actually be tied together.
	missFrom, missTo, missPct, missOK := delta(models, "bpMissRatioPct")
	_, _, qpsPct, qpsOK := delta(models, "qps")
	poolChanged := false
	for _, s := range c.Settings {
		if s.Key == "bufferPoolSize" {
			poolChanged = true
		}
	}
	if missOK && poolChanged {
		v := vsVerdict{ID: "comparePool", Title: "Did the buffer pool change help?"}
		v.Headline = fmt.Sprintf("read-miss %.2f%% -> %.2f%% (%+.0f%%)", missFrom, missTo, missPct)
		switch {
		case missPct < -50 && qpsOK && qpsPct > compareNoisePct:
			v.Level = vsOK
			v.Detail = fmt.Sprintf(
				"The miss ratio collapsed and throughput rose %+.0f%% with it. That is cause and "+
					"effect rather than coincidence: the working set now fits where it did not "+
					"before, and the buffer pool was the constraint.", qpsPct)
		case missPct < -50:
			v.Level = vsInfo
			v.Detail = "The miss ratio fell sharply but throughput did not follow. The pool was " +
				"not what was limiting this workload — check whether the misses were being " +
				"served by the OS page cache all along, in which case they were never costing " +
				"much. Something else is the bottleneck."
		case missPct > 50:
			v.Level = vsWarn
			v.Detail = "The miss ratio rose. If the pool was made smaller that is expected; if " +
				"it was made larger, the working set grew faster than the pool did."
		default:
			v.Level = vsInfo
			v.Detail = "The buffer pool size changed but the miss ratio barely moved, which " +
				"means the working set was already either comfortably inside it or far beyond it."
		}
		out = append(out, v)
	}

	// Durability: did relaxing it actually buy anything?
	durabilityChanged := false
	for _, s := range c.Settings {
		if s.Key == "syncBinlog" || s.Key == "flushLogAtTrxCommit" {
			durabilityChanged = true
		}
	}
	if durabilityChanged {
		v := vsVerdict{ID: "compareDurability", Title: "Was relaxing durability worth it?"}
		fsFrom, fsTo, fsPct, fsOK := delta(models, "fsyncsPerSec")
		if fsOK {
			v.Headline = fmt.Sprintf("fsyncs %s/s -> %s/s (%+.0f%%)", compactNum(fsFrom), compactNum(fsTo), fsPct)
		} else {
			v.Headline = "durability settings differ between these captures"
		}
		switch {
		case qpsOK && qpsPct >= compareNoisePct:
			v.Level = vsInfo
			v.Detail = fmt.Sprintf(
				"Throughput moved %+.0f%%. Weaker durability means a crash of the host can lose "+
					"recent commits and leave the binlog behind the data — worth it on a "+
					"rebuildable dataset, not on anything you would miss. Batching commits buys "+
					"much of the same throughput without the exposure.", qpsPct)
		case qpsOK:
			v.Level = vsWarn
			v.Detail = fmt.Sprintf(
				"Throughput moved only %+.0f%%, inside the noise band. The durability was given "+
					"up for nothing measurable — put it back.", qpsPct)
		default:
			v.Level = vsInfo
			v.Detail = "These captures have no throughput figure to judge the trade against."
		}
		out = append(out, v)
	}

	// Anything that got materially worse while something else got better.
	var worse []string
	for _, m := range c.Metrics {
		if m.Improved != nil && !*m.Improved && m.Meaningful {
			worse = append(worse, m.Label)
		}
	}
	if len(worse) > 0 {
		sort.Strings(worse)
		out = append(out, vsVerdict{
			ID: "compareRegressions", Title: "What got worse", Level: vsWarn,
			Headline: fmt.Sprintf("%d measure(s) moved the wrong way", len(worse)),
			Detail: "Worth reading before calling this an improvement: " +
				joinAnd(worse) + ". A change that helps one number at the expense of another " +
				"is a trade, not a win, and the trade should be a deliberate one.",
		})
	}
	return out
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return fmt.Sprintf("%s and %s",
		joinComma(items[:len(items)-1]), items[len(items)-1])
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// ---- handler ----

// handleVisualCompare parses two or more kept captures and returns the
// head-to-head. Ids are given in order and the first is the baseline; a caller
// listing them oldest-first gets "what did this change do", which is the
// question they were kept to answer.
func (a *App) handleVisualCompare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ArchiveIDs []int64 `json:"archiveIds"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	if len(body.ArchiveIDs) < 2 {
		writeErr(w, http.StatusBadRequest, "select at least two captures to compare")
		return
	}
	if len(body.ArchiveIDs) > 6 {
		// Past a handful the table stops being readable and every capture is
		// another archive to unpack.
		writeErr(w, http.StatusBadRequest, "compare at most six captures at once")
		return
	}
	models := make([]*vsModel, 0, len(body.ArchiveIDs))
	for _, id := range body.ArchiveIDs {
		arch, ok := a.loadOwnedArchiveByID(w, r, id)
		if !ok {
			return
		}
		data, err := os.ReadFile(arch.Path)
		if err != nil {
			writeErr(w, http.StatusNotFound,
				fmt.Sprintf("archive %d is missing from storage", id))
			return
		}
		m, err := parsePtStalk(data)
		if err != nil {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("archive %d could not be parsed: %v", id, err))
			return
		}
		m.Source.ArchiveID = arch.ID
		m.Source.Note = arch.Note
		if arch.CapturedAt != "" {
			m.Source.CapturedAt = arch.CapturedAt
		}
		models = append(models, m)
	}
	cmp := buildComparison(models)
	if cmp == nil {
		writeErr(w, http.StatusBadRequest, "nothing to compare")
		return
	}
	writeJSON(w, http.StatusOK, cmp)
}
