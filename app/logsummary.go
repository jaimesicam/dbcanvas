package main

// logsummary.go — the Log Summary's HTTP surface and bundle registry.
//
// A bundle is a set of database server logs read together: uploaded, or tailed from the
// nodes of a running stack in one request. Like the Packet Inspector's captures they live
// in memory — a parsed bundle is derived data, worthless once the incident is over, and
// nothing here needs to survive a restart. Unlike them the raw text is kept as well, so
// "show me this record in the file" and "download exactly what I gave you" both work.
//
// Every list endpoint is range-addressable, for the same reason the Packet Inspector's
// are: the browser holds one page of what may be a hundred thousand events, and the
// swimlane asks for bucket counts over exactly the window it is drawing.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- registry

// lsBundleRec is one set of logs and its parse.
type lsBundleRec struct {
	ID      string     `json:"id"`
	Label   string     `json:"label"`
	Created string     `json:"created"`
	Origin  string     `json:"origin"` // upload | node
	StackID int64      `json:"stackId,omitempty"`
	Stack   string     `json:"stackName,omitempty"`
	Sources []lsSource `json:"sources"`
	Summary lsSummary  `json:"summary"`
	Note    string     `json:"note,omitempty"`

	owner  int64
	bundle *lsBundle
	raw    map[int][]byte // source index → the bytes as supplied
	mu     *sync.Mutex
}

var lsBundles = struct {
	sync.Mutex
	m     map[string]*lsBundleRec
	order []string
}{m: map[string]*lsBundleRec{}}

// lsKeep is how many bundles are retained per app process. Each one holds its raw text,
// so this is a memory budget as much as a history length.
const lsKeep = 12

func lsRemember(rec *lsBundleRec) {
	lsBundles.Lock()
	defer lsBundles.Unlock()
	lsBundles.m[rec.ID] = rec
	lsBundles.order = append(lsBundles.order, rec.ID)
	for len(lsBundles.order) > lsKeep {
		old := lsBundles.order[0]
		lsBundles.order = lsBundles.order[1:]
		delete(lsBundles.m, old)
	}
}

func lsGet(id string) *lsBundleRec {
	lsBundles.Lock()
	defer lsBundles.Unlock()
	return lsBundles.m[id]
}

func lsForget(id string) {
	lsBundles.Lock()
	defer lsBundles.Unlock()
	delete(lsBundles.m, id)
	for i, x := range lsBundles.order {
		if x == id {
			lsBundles.order = append(lsBundles.order[:i], lsBundles.order[i+1:]...)
			break
		}
	}
}

func lsListFor(u User) []*lsBundleRec {
	lsBundles.Lock()
	defer lsBundles.Unlock()
	out := []*lsBundleRec{}
	for i := len(lsBundles.order) - 1; i >= 0; i-- {
		rec := lsBundles.m[lsBundles.order[i]]
		if rec == nil {
			continue
		}
		if rec.owner == u.ID || u.Role == RoleAdmin {
			out = append(out, rec)
		}
	}
	return out
}

// public is a copy safe to serialise: no mutex, no raw text, no parsed events.
func (rec *lsBundleRec) public() lsBundleRec {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	cp := *rec
	cp.bundle, cp.raw, cp.mu = nil, nil, nil
	return cp
}

func (rec *lsBundleRec) parsed() *lsBundle {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.bundle
}

// lsNewBundle parses a set of inputs and registers the result.
func lsNewBundle(u User, label, origin string, stackID int64, stack string, inputs []lsInput) *lsBundleRec {
	b := lsBuild(inputs)
	raw := map[int][]byte{}
	for i, in := range inputs {
		raw[i] = in.Data
	}
	rec := &lsBundleRec{
		ID:      fmt.Sprintf("log-%d", time.Now().UnixNano()),
		Label:   label,
		Created: time.Now().UTC().Format(time.RFC3339),
		Origin:  origin, StackID: stackID, Stack: stack,
		Sources: b.Sources, Summary: b.Summary,
		owner: u.ID, bundle: b, raw: raw, mu: &sync.Mutex{},
	}
	lsRemember(rec)
	return rec
}

// ---------------------------------------------------------------- targets

// handleLogTargets lists nodes whose server log can be read.
//
// Every engine the Packet Inspector can capture also has a log this can read, and the
// list is deliberately the same one: a node worth watching on the wire is a node worth
// reading the log of, and the two pages then name the same things in the same order.
func (a *App) handleLogTargets(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, a.listPktTargets(u))
}

// ---------------------------------------------------------------- collect

// lsCollectReq asks for the logs of one or more nodes in a stack.
//
// Several nodes in ONE request rather than one node per request, because the whole point
// is the comparison: three members' logs pulled seconds apart are three views of the same
// cluster, and a UI that made you fetch and combine them by hand would be a worse version
// of `tail`.
type lsCollectReq struct {
	StackID int64    `json:"stackId"`
	NodeIDs []string `json:"nodeIds"`
	Lines   int      `json:"lines"`
	Label   string   `json:"label"`
}

// lsMaxLines bounds a tail. A PXC node writes ~1000 lines per restart cycle, so 20000 is
// several incidents' worth; the ceiling exists so one request cannot pull a gigabyte of a
// log that has been running at debug level for a month.
const lsMaxLines = 200000

func (a *App) handleLogCollect(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req lsCollectReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.NodeIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "choose at least one node")
		return
	}
	lines := req.Lines
	if lines <= 0 {
		lines = 5000
	}
	if lines > lsMaxLines {
		lines = lsMaxLines
	}

	st, err := a.store.GetStack(req.StackID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "stack not found")
		return
	}
	if st.OwnerID != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your stack")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	var inputs []lsInput
	var failures []string
	for _, nodeID := range req.NodeIDs {
		engine, containerID, label, _, _, _, err := a.pktResolveTarget(u, req.StackID, nodeID)
		if err != nil {
			failures = append(failures, nodeID+": "+err.Error())
			continue
		}
		text, path, err := a.lsTailRaw(ctx, containerID, engine, lines)
		if err != nil {
			failures = append(failures, label+": "+err.Error())
			continue
		}
		inputs = append(inputs, lsInput{
			Name: label, Path: path, Origin: "node", Engine: engine, Data: []byte(text),
		})
	}
	if len(inputs) == 0 {
		writeErr(w, http.StatusBadGateway, "no logs could be read — "+strings.Join(failures, "; "))
		return
	}

	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = fmt.Sprintf("%s · %d node(s)", st.Name, len(inputs))
	}
	rec := lsNewBundle(u, label, "node", st.ID, st.Name, inputs)
	if len(failures) > 0 {
		rec.Note = "could not read: " + strings.Join(failures, "; ")
	}
	writeJSON(w, http.StatusOK, rec.public())
}

// lsTailRawScript reads the tail of a log file without classifying anything — the Log
// Summary needs the raw text, because its parser folds multi-line records that a
// line-at-a-time reader would throw away.
//
// It is deliberately a sibling of pktLogTailScript rather than a reuse of it: that one
// prints a PATH= marker and is consumed by a classifier, this one prints the text and is
// consumed by a parser, and sharing them would mean one of the two had to strip the
// other's framing back out.
const lsTailScript = `set -e
for f in $PATHS; do
  if [ -r "$f" ]; then echo "#lsummary-path:$f" >&2; tail -n "$LINES" "$f"; exit 0; fi
done
echo "no readable log found (tried: $PATHS)" >&2
exit 1`

// lsTailRaw returns the raw tail of a node's server log and the path it came from.
func (a *App) lsTailRaw(ctx context.Context, containerID, engine string, lines int) (string, string, error) {
	paths := pktServerLogPaths
	script := lsTailScript
	var extra []string
	switch engine {
	case pktEnginePostgres:
		// PostgreSQL's own log AND the cluster manager's, together, as one source.
		//
		// A Patroni member writes two logs and neither is the whole story: Patroni decides
		// the failover and PostgreSQL carries it out, so the decision is in one file and
		// its consequence in the other, seconds apart. Read separately they are two lanes
		// for one server; read together they are the sentence somebody opened the logs for.
		//
		// And Patroni does not write a file at all in a systemd deployment — it logs to the
		// journal, so /var/log/patroni exists and is empty. Looking only where
		// pktPatroniLogPaths points finds nothing on a perfectly ordinary Patroni node.
		// `journalctl -o cat` is what makes this work: it prints the message alone, without
		// the syslog prefix, which is exactly the shape Patroni's own format already has.
		paths = append(append([]string{}, pktPGLogPaths...), pktPatroniLogPaths...)
		script = lsPGTailScript
		extra = []string{"UNITS=" + strings.Join(lsPGManagerUnits, " ")}
	case pktEngineMongoDB:
		paths = pktMongoLogPaths
	case pktEngineValkey:
		// Valkey is the one engine whose log is not a file by default — dbcanvas sets no
		// `logfile`, so it goes to stdout and systemd keeps it. Fall through to journalctl
		// when none of the file paths exist.
		paths = pktValkeyLogPaths
		script = lsValkeyTailScript
		extra = []string{"UNITS=" + strings.Join(pktValkeyLogUnits, " ")}
	}
	env := append([]string{
		"PATHS=" + strings.Join(paths, " "),
		"LINES=" + strconv.Itoa(lines),
	}, extra...)
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", script}, env)
	if err != nil {
		return "", "", err
	}
	if res.Code != 0 {
		return "", "", fmt.Errorf("%s", strings.TrimSpace(res.Stderr+res.Stdout))
	}
	path := ""
	for _, line := range strings.Split(res.Stderr, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "#lsummary-path:"); ok {
			path = p
		}
	}
	return res.Stdout, path, nil
}

// lsPGManagerUnits are the cluster managers whose log is a journal rather than a file.
// repmgrd is here for the same reason Patroni is: on a systemd deployment /var/log/repmgr
// is a directory that stays empty.
var lsPGManagerUnits = []string{"patroni", "repmgrd", "repmgr"}

// lsPGTailScript reads PostgreSQL's log and appends the cluster manager's journal.
//
// Appended rather than chosen between, which is the difference from every other engine
// here: both halves are wanted, and the parser folds the two formats into one stream
// because lsFoldPostgres recognises each line by its own shape. The manager's records are
// marked with a subsystem so a rule can still require one or the other.
const lsPGTailScript = `set -e
found=
for f in $PATHS; do
  if [ -r "$f" ]; then echo "#lsummary-path:$f" >&2; tail -n "$LINES" "$f"; found=$f; break; fi
done
for u in $UNITS; do
  if systemctl is-enabled "$u" >/dev/null 2>&1 || systemctl is-active "$u" >/dev/null 2>&1; then
    if journalctl -o cat -u "$u" -n "$LINES" --no-pager >/tmp/.lspg 2>/dev/null && [ -s /tmp/.lspg ]; then
      echo "#lsummary-path:${found:-}+journal:$u" >&2; cat /tmp/.lspg; rm -f /tmp/.lspg
    fi
  fi
done
if [ -z "$found" ]; then
  # No PostgreSQL log. The manager's journal alone is still worth having — it is where a
  # failover decision lives — so this is only an error when neither was readable.
  if [ ! -s /tmp/.lspg ]; then echo "no readable PostgreSQL log or manager journal found (tried: $PATHS)" >&2; exit 1; fi
fi
exit 0`

// lsValkeyTailScript adds the journal fallback for the engine that has no log file.
const lsValkeyTailScript = `set -e
for f in $PATHS; do
  if [ -r "$f" ]; then echo "#lsummary-path:$f" >&2; tail -n "$LINES" "$f"; exit 0; fi
done
for u in $UNITS; do
  if journalctl -u "$u" -n "$LINES" --no-pager >/tmp/.lsvk 2>/dev/null && [ -s /tmp/.lsvk ]; then
    echo "#lsummary-path:journal:$u" >&2; cat /tmp/.lsvk; rm -f /tmp/.lsvk; exit 0
  fi
done
echo "no readable Valkey log or journal found (tried files: $PATHS; units: $UNITS)" >&2
exit 1`

// ---------------------------------------------------------------- upload

// lsMaxUploadBytes bounds one uploaded log. A rotated MySQL error log is a few MB; a
// Galera member's under wsrep_debug can be much larger, and this leaves room for that
// without letting an upload eat the app's heap.
const lsMaxUploadBytes = 128 << 20

// lsMaxUploadFiles is how many logs one bundle may hold. Beyond a handful the swimlane
// stops being readable, and beyond that the comparison is not the tool anybody wants.
const lsMaxUploadFiles = 12

// handleLogUpload takes several log files at once and makes one bundle of them.
func (a *App) handleLogUpload(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "could not read the upload: "+err.Error())
		return
	}
	engine := strings.TrimSpace(r.FormValue("engine"))
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		files = r.MultipartForm.File["file"]
	}
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "choose at least one log file")
		return
	}
	if len(files) > lsMaxUploadFiles {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("at most %d logs at a time — beyond that the timeline stops being readable", lsMaxUploadFiles))
		return
	}
	var inputs []lsInput
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, fh.Filename+": "+err.Error())
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, lsMaxUploadBytes+1))
		f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, fh.Filename+": "+err.Error())
			return
		}
		if len(data) > lsMaxUploadBytes {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("%s is larger than %d MB", fh.Filename, lsMaxUploadBytes>>20))
			return
		}
		inputs = append(inputs, lsInput{Name: fh.Filename, Origin: "upload", Engine: engine, Data: data})
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		label = fmt.Sprintf("%d uploaded log(s)", len(inputs))
	}
	writeJSON(w, http.StatusOK, lsNewBundle(u, label, "upload", 0, "", inputs).public())
}

// ---------------------------------------------------------------- read

func (a *App) handleLogList(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	out := []lsBundleRec{}
	for _, rec := range lsListFor(u) {
		out = append(out, rec.public())
	}
	writeJSON(w, http.StatusOK, out)
}

// lsOwned resolves the bundle in the URL and checks the caller may see it.
func (a *App) lsOwned(w http.ResponseWriter, r *http.Request) (*lsBundleRec, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	rec := lsGet(r.PathValue("id"))
	if rec == nil {
		writeErr(w, http.StatusNotFound,
			"log bundle not found (bundles are kept in memory and do not survive an app restart)")
		return nil, false
	}
	if rec.owner != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your log bundle")
		return nil, false
	}
	return rec, true
}

func (a *App) handleLogGet(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.lsOwned(w, r)
	if !ok {
		return
	}
	b := rec.parsed()
	writeJSON(w, http.StatusOK, map[string]any{
		"bundle": rec.public(), "findings": b.Finding, "phases": b.Phases,
	})
}

func (a *App) handleLogDelete(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.lsOwned(w, r)
	if !ok {
		return
	}
	lsForget(rec.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// lsQuery is the range and filter a list or timeline request asks for.
type lsQuery struct {
	FromTS  float64
	ToTS    float64
	Src     int      // -1 = all
	Sev     []string // empty = all
	Class   string
	Search  string
	Limit   int
	Offset  int
	Buckets int
	// Around asks for the page holding the event nearest an instant, instead of a page
	// at a fixed offset — what a click on the timeline needs.
	Around float64
}

func lsParseQuery(r *http.Request) lsQuery {
	q := r.URL.Query()
	out := lsQuery{
		Src:    atoiDef(q.Get("src"), -1),
		Class:  strings.TrimSpace(q.Get("class")),
		Search: strings.ToLower(strings.TrimSpace(q.Get("q"))),
		Limit:  pktClamp(atoiDef(q.Get("limit"), 0), 1, 2000, 200),
		Offset: atoiDef(q.Get("offset"), 0),
	}
	out.Buckets = pktClamp(atoiDef(q.Get("buckets"), 0), 2, 1000, 180)
	out.FromTS, _ = strconv.ParseFloat(q.Get("fromTs"), 64)
	out.ToTS, _ = strconv.ParseFloat(q.Get("toTs"), 64)
	out.Around, _ = strconv.ParseFloat(q.Get("around"), 64)
	for _, s := range strings.Split(q.Get("sev"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out.Sev = append(out.Sev, s)
		}
	}
	return out
}

func (q lsQuery) match(e lsEvent) bool {
	if q.FromTS > 0 && lsEndOf(e) < q.FromTS {
		return false
	}
	if q.ToTS > 0 && e.TS > q.ToTS {
		return false
	}
	if q.Src >= 0 && e.Src != q.Src {
		return false
	}
	if q.Class != "" && e.Class != q.Class {
		return false
	}
	if len(q.Sev) > 0 {
		hit := false
		for _, s := range q.Sev {
			if s == e.Sev {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if q.Search != "" {
		hay := strings.ToLower(e.Label + " " + e.Message + " " + e.Detail + " " +
			e.Code + " " + e.Peer + " " + e.Subsys + " " + e.Meaning)
		if !strings.Contains(hay, q.Search) {
			return false
		}
	}
	return true
}

// handleLogEvents returns one page of the event list for a range.
func (a *App) handleLogEvents(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.lsOwned(w, r)
	if !ok {
		return
	}
	b := rec.parsed()
	q := lsParseQuery(r)

	// Which events pass the filters, in order.
	idx := make([]int, 0, 256)
	for i, e := range b.Events {
		if q.match(e) {
			idx = append(idx, i)
		}
	}
	nearestNo := 0
	if q.Around > 0 && len(idx) > 0 {
		best, at := 0.0, -1
		for pos, i := range idx {
			d := b.Events[i].TS - q.Around
			if d < 0 {
				d = -d
			}
			if at < 0 || d < best {
				best, at = d, pos
			}
		}
		nearestNo = b.Events[idx[at]].No
		if q.Offset = at - q.Limit/2; q.Offset < 0 {
			q.Offset = 0
		}
	}
	out := make([]lsEvent, 0, q.Limit)
	for pos := q.Offset; pos < len(idx) && len(out) < q.Limit; pos++ {
		out = append(out, b.Events[idx[pos]])
	}
	res := map[string]any{
		"events": out, "matched": len(idx), "offset": q.Offset, "limit": q.Limit,
		"total": len(b.Events),
	}
	if nearestNo > 0 {
		res["nearestNo"] = nearestNo
	}
	writeJSON(w, http.StatusOK, res)
}

// handleLogTimeline returns the swimlane: per-source severity counts over a time grid,
// plus the state phases clipped to the window.
func (a *App) handleLogTimeline(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.lsOwned(w, r)
	if !ok {
		return
	}
	b := rec.parsed()
	q := lsParseQuery(r)
	from, to := b.Summary.FirstTS, b.Summary.LastTS
	if q.FromTS > 0 {
		from = q.FromTS
	}
	if q.ToTS > 0 {
		to = q.ToTS
	}
	if to <= from {
		to = from + 1
	}
	// Filters apply to the ticks, not to the state track: hiding the "info" events must
	// not repaint a node's state, because its state is not an event.
	var shown []lsEvent
	for _, e := range b.Events {
		if q.match(e) {
			shown = append(shown, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fromTs": from, "toTs": to, "buckets": lsBucketise(shown, b.Sources, from, to, q.Buckets),
		"phases": lsClipPhases(b.Phases, from, to), "matched": len(shown),
		"spanFirstTs": b.Summary.FirstTS, "spanLastTs": b.Summary.LastTS,
	})
}

// lsClipPhases trims the state track to a window so the browser draws only what it shows.
func lsClipPhases(phases []lsPhase, from, to float64) []lsPhase {
	out := []lsPhase{}
	for _, p := range phases {
		if p.To <= from || p.From >= to {
			continue
		}
		if p.From < from {
			p.From = from
		}
		if p.To > to {
			p.To = to
		}
		out = append(out, p)
	}
	return out
}

// handleLogAt answers "what was every node doing at this instant" — the question three
// logs are opened to answer, and the one that is genuinely tedious to answer by hand.
func (a *App) handleLogAt(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.lsOwned(w, r)
	if !ok {
		return
	}
	b := rec.parsed()
	at, _ := strconv.ParseFloat(r.URL.Query().Get("at"), 64)
	if at <= 0 {
		writeErr(w, http.StatusBadRequest, "at=<epoch seconds> is required")
		return
	}
	type nodeAt struct {
		Src     int     `json:"src"`
		Node    string  `json:"node"`
		State   string  `json:"state"`
		Sev     string  `json:"sev"`
		Meaning string  `json:"meaning,omitempty"`
		Members int     `json:"members,omitempty"`
		Primary string  `json:"primary,omitempty"`
		Since   float64 `json:"since,omitempty"`
		Until   float64 `json:"until,omitempty"`
		Covered bool    `json:"covered"` // the log actually spans this instant
	}
	out := []nodeAt{}
	agree := true
	members := -1
	for _, s := range b.Sources {
		n := nodeAt{Src: s.Idx, Node: s.Node, State: "UNKNOWN", Sev: lsSevInfo,
			Covered: s.FirstTS > 0 && at >= s.FirstTS && at <= s.LastTS}
		if p, ok := lsStateAt(b.Phases, s.Idx, at); ok {
			n.State, n.Sev, n.Members, n.Primary = p.State, p.Sev, p.Members, p.Primary
			n.Since, n.Until = p.From, p.To
			n.Meaning = lsStateMeaning[p.State]
		}
		if n.Members > 0 {
			if members >= 0 && members != n.Members {
				agree = false
			}
			members = n.Members
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Src < out[j].Src })

	// The nearest event on each side, so "why" is one click away from "what".
	var before, after *lsEvent
	for i := range b.Events {
		e := &b.Events[i]
		if e.Sev == lsSevInfo {
			continue
		}
		if e.TS <= at {
			before = e
		} else {
			after = e
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"at": at, "nodes": out, "agree": agree,
		"before": before, "after": after,
	})
}

// handleLogRaw serves a source's original text, so any record can be read in context and
// the file can be taken away whole.
func (a *App) handleLogRaw(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.lsOwned(w, r)
	if !ok {
		return
	}
	idx := atoiDef(r.PathValue("src"), -1)
	rec.mu.Lock()
	data, has := rec.raw[idx]
	rec.mu.Unlock()
	if !has {
		writeErr(w, http.StatusNotFound, "no such source in this bundle")
		return
	}
	// A window around a line, for "show me this record in the file". Without it the
	// browser would have to download a 100 MB log to display twenty lines of context.
	if around := atoiDef(r.URL.Query().Get("line"), 0); around > 0 {
		ctxLines := pktClamp(atoiDef(r.URL.Query().Get("context"), 0), 1, 500, 40)
		lines := strings.Split(string(data), "\n")
		lo, hi := around-1-ctxLines, around-1+ctxLines
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines) {
			hi = len(lines)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"from": lo + 1, "to": hi, "line": around, "text": strings.Join(lines[lo:hi], "\n"),
		})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", lsRawFilename(rec, idx)))
	w.Write(data)
}

func lsRawFilename(rec *lsBundleRec, idx int) string {
	for _, s := range rec.Sources {
		if s.Idx == idx {
			name := s.Name
			if name == "" {
				name = s.Node
			}
			if !strings.Contains(name, ".") {
				name += ".log"
			}
			return name
		}
	}
	return "source.log"
}

// handleLogOffset sets a source's clock offset and reparses the bundle.
//
// Reparsing rather than shifting the events in place, because an offset changes which
// records collapse together and which phases overlap — everything derived from the
// timeline has to be derived again from the corrected one.
func (a *App) handleLogOffset(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.lsOwned(w, r)
	if !ok {
		return
	}
	var body struct {
		Src    int     `json:"src"`
		Offset float64 `json:"offset"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rec.mu.Lock()
	found := false
	for i := range rec.Sources {
		if rec.Sources[i].Idx == body.Src {
			rec.Sources[i].Offset = body.Offset
			found = true
		}
	}
	if !found {
		rec.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such source in this bundle")
		return
	}
	inputs := make([]lsInput, 0, len(rec.Sources))
	for _, s := range rec.Sources {
		inputs = append(inputs, lsInput{
			Name: s.Name, Path: s.Path, Origin: s.Origin, Engine: s.Engine,
			Data: rec.raw[s.Idx], Offset: s.Offset,
		})
	}
	rec.mu.Unlock()

	b := lsBuild(inputs)
	rec.mu.Lock()
	rec.bundle, rec.Sources, rec.Summary = b, b.Sources, b.Summary
	rec.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"bundle": rec.public(), "findings": b.Finding, "phases": b.Phases,
	})
}
