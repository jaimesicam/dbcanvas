package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Hub owns the sim's state: the timeline, the dials that are on, the event log,
// and what the browser polls. Everything the browser can change goes through it,
// so the safety rules live in one place: every hold is enforced here, Stop all
// stops everything here, and shutting down releases everything here.
type Hub struct {
	eng  Engine
	work *Workload
	cond *Conditions
	ctx  context.Context

	mu        sync.Mutex
	ready     bool
	initErr   string
	info      TargetInfo
	dials     []Dial
	points    []Point
	members   []Member
	bands     []Band
	events    []Event
	active    map[string]*active
	seq       int
	baseline  *Baseline
	datasets  []Dataset
	prep      *prepState
	recording *recording
	runs      []RunSummary
	settings  settings
	lastTick  time.Time
}

type active struct {
	dial    Dial
	run     Running
	label   string
	targets []string
	params  Params
	start   time.Time
	hold    int
	band    *Band
	stopped bool
}

// Baseline is the reference every delta on the dashboard is measured against.
type Baseline struct {
	T     int64   `json:"t"`
	WQ    float64 `json:"wq"`
	RQ    float64 `json:"rq"`
	P50   float64 `json:"p50"`
	P99   float64 `json:"p99"`
	Lag   float64 `json:"lag"`
	RecvQ float64 `json:"recvq"`
}

type prepState struct {
	Name   string  `json:"name"`
	Bytes  int64   `json:"bytes"`
	PK     bool    `json:"pk"`
	Done   float64 `json:"done"`
	cancel context.CancelFunc
}

type recording struct {
	start time.Time
	name  string
}

// RunSummary is one recorded run as the list shows it; the run itself, with its
// timeline, is fetched on demand.
type RunSummary struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Start    int64   `json:"start"`
	End      int64   `json:"end"`
	PeakLag  float64 `json:"peakLag"`
	PeakP99  float64 `json:"peakP99"`
	LowWQ    float64 `json:"lowWq"`
	BaseWQ   float64 `json:"baseWq"`
	Stressor string  `json:"stressors"`
}

// Run is a recorded run in full.
type Run struct {
	RunSummary
	Target   TargetInfo        `json:"target"`
	Baseline *Baseline         `json:"baseline,omitempty"`
	Points   []Point           `json:"points"`
	Bands    []Band            `json:"bands"`
	Events   []Event           `json:"events"`
	Params   map[string]Params `json:"params"`
}

// settings is what the dashboard remembers across a restart of the sim: the
// workload size and the last values given to each dial. Dials always come back
// off, so a restart never re-applies a lock on its own.
type settings struct {
	Threads int                 `json:"threads"`
	Params  map[string]Params   `json:"params"`
	Targets map[string][]string `json:"targets"`
}

const (
	historySeconds = 900
	maxEvents      = 200
	stopTimeout    = 30 * time.Second
)

func NewHub(ctx context.Context, e Engine) *Hub {
	return &Hub{eng: e, work: newWorkload(e), cond: newConditions(), ctx: ctx, active: map[string]*active{},
		settings: settings{Threads: defaultThreads, Params: map[string]Params{}, Targets: map[string][]string{}}}
}

// Run connects, then samples once a second until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	for {
		err := h.eng.Init(ctx)
		if err == nil {
			break
		}
		h.mu.Lock()
		h.initErr = err.Error()
		h.mu.Unlock()
		log.Printf("perfsim: connecting: %v", err)
		sleepCtx(ctx, 3*time.Second)
		if ctx.Err() != nil {
			return
		}
	}
	h.loadSaved(ctx)
	h.mu.Lock()
	h.ready, h.initErr = true, ""
	h.info = h.eng.Info()
	h.dials = h.allDials()
	threads := h.settings.Threads
	h.mu.Unlock()
	h.event("neutral", fmt.Sprintf("Connected to %s (%s %s, %s)", h.info.Name, h.info.Flavor, h.info.Version, h.info.Topology))
	h.work.SetThreads(ctx, threads)
	h.refreshDatasets(ctx)

	go h.heartbeat(ctx)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	h.lastTick = time.Now()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			h.tick(ctx, now)
			n++
			if n%10 == 0 {
				h.refreshDatasets(ctx)
			}
			if n%30 == 0 {
				// Topology can change underneath: a failover, a member added.
				h.mu.Lock()
				h.info = h.eng.Info()
				h.dials = h.allDials()
				h.mu.Unlock()
			}
		}
	}
}

func (h *Hub) heartbeat(ctx context.Context) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			h.eng.Heartbeat(c)
			cancel()
		}
	}
}

func (h *Hub) tick(ctx context.Context, now time.Time) {
	sctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	members, srv := h.eng.Sample(sctx)
	cancel()

	h.mu.Lock()
	elapsed := now.Sub(h.lastTick)
	h.lastTick = now
	h.mu.Unlock()
	wq, rq, p50, p99, errs := h.work.take(elapsed)

	p := Point{T: now.Unix(), Lag: map[string]float64{}, RecvQ: map[string]float64{},
		WQ: wq, RQ: rq, P50: p50, P99: p99, Errs: errs,
		LockW: srv.LockWaits, Thr: srv.Threads, Conns: srv.Conns, FC: srv.FC, SendQ: srv.SendQ,
		Backlog: srv.Backlog, DiskReads: srv.DiskReads}
	for _, m := range members {
		if m.Lag >= 0 {
			p.Lag[m.ID] = m.Lag
		}
		p.RecvQ[m.ID] = m.RecvQ
	}

	h.mu.Lock()
	h.members = members
	h.points = append(h.points, p)
	if len(h.points) > historySeconds {
		h.points = h.points[len(h.points)-historySeconds:]
	}
	var expired, finished []string
	for id, a := range h.active {
		st := a.run.Status()
		if st.Done {
			finished = append(finished, id)
		} else if a.hold > 0 && now.Sub(a.start) >= time.Duration(a.hold)*time.Second {
			expired = append(expired, id)
		}
	}
	h.mu.Unlock()

	for _, id := range expired {
		h.Stop(id, "hold")
	}
	for _, id := range finished {
		h.Stop(id, "done")
	}
}

// Start turns a dial on.
func (h *Hub) Start(id string, p Params, targets []string) error {
	h.mu.Lock()
	if !h.ready {
		h.mu.Unlock()
		return errf("not connected to the database yet")
	}
	if _, on := h.active[id]; on {
		h.mu.Unlock()
		return errf("already on")
	}
	h.mu.Unlock()
	var d Dial
	color := 0
	for i, x := range h.allDials() {
		if x.ID == id {
			d, color = x, i%4
		}
	}
	if d.ID == "" {
		return errf("no such stressor %q", id)
	}
	if !d.Available {
		return errf("%s", d.Why)
	}
	p = withDefaults(d, p)
	if d.Members == "" {
		targets = nil
	} else if len(targets) == 0 {
		return errf("choose at least one member")
	} else if d.Single && len(targets) > 1 {
		targets = targets[:1]
	}

	label := d.Name
	if len(targets) > 0 {
		label += " · " + strings.Join(targets, ", ")
	}
	emit := func(level, text string) { h.event(level, text) }
	var run Running
	if h.cond.owns(id) {
		run = h.cond.start(id, p, targets, p.Int("hold", 0))
	} else {
		var err error
		if run, err = h.eng.Start(h.ctx, id, p, targets, emit); err != nil {
			return err
		}
	}

	h.mu.Lock()
	h.seq++
	band := &Band{Seq: h.seq, ID: id, Label: label, Start: time.Now().Unix(), Color: color}
	h.bands = append(h.bands, *band)
	h.active[id] = &active{dial: d, run: run, label: label, targets: targets, params: p,
		start: time.Now(), hold: p.Int("hold", 0), band: &h.bands[len(h.bands)-1]}
	h.settings.Params[id] = p
	if len(targets) > 0 {
		h.settings.Targets[id] = targets
	}
	if h.baseline == nil {
		h.baseline = h.baselineLocked()
	}
	h.trimBandsLocked()
	h.mu.Unlock()
	h.event("info", label+" started")
	h.saveSettings()
	return nil
}

// Update changes the settings of a dial that is on: the next large
// transaction uses the new size, a new maximum hold counts from when the dial
// was turned on. Members cannot change while it runs; turn it off and on.
func (h *Hub) Update(id string, p Params) error {
	h.mu.Lock()
	a, ok := h.active[id]
	if !ok {
		h.mu.Unlock()
		return errf("not on")
	}
	merged := Params{}
	for k, v := range a.params {
		merged[k] = v
	}
	for k, v := range p {
		merged[k] = v
	}
	a.params = withDefaults(a.dial, merged)
	a.hold = a.params.Int("hold", 0)
	h.settings.Params[id] = a.params
	params := a.params
	run := a.run
	h.mu.Unlock()
	if u, ok := run.(interface{ Update(Params) }); ok {
		u.Update(params)
	}
	h.saveSettings()
	return nil
}

// Stop turns a dial off. why is "user", "hold", "done" or "all".
func (h *Hub) Stop(id, why string) error {
	h.mu.Lock()
	a, ok := h.active[id]
	if !ok || a.stopped {
		h.mu.Unlock()
		if !ok {
			return errf("not on")
		}
		return nil
	}
	a.stopped = true
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	err := a.run.Stop(ctx)
	cancel()
	st := a.run.Status()

	h.mu.Lock()
	delete(h.active, id)
	end := time.Now().Unix()
	for i := range h.bands {
		if h.bands[i].Seq == a.band.Seq {
			h.bands[i].End = &end
		}
	}
	h.mu.Unlock()

	switch {
	case err != nil:
		h.event("warn", fmt.Sprintf("%s: could not release cleanly: %v", a.label, err))
	case st.Err != "":
		h.event("warn", fmt.Sprintf("%s stopped: %s", a.label, st.Err))
	case why == "hold":
		h.event("warn", a.label+" released after its maximum hold")
	case why == "done":
		h.event("ok", a.label+" finished")
	default:
		h.event("info", a.label+" stopped")
	}
	return err
}

// StopAll turns every dial off and releases everything they hold.
func (h *Hub) StopAll(why string) int {
	h.mu.Lock()
	ids := make([]string, 0, len(h.active))
	for id := range h.active {
		ids = append(ids, id)
	}
	prep := h.prep
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) { defer wg.Done(); h.Stop(id, why) }(id)
	}
	wg.Wait()
	if prep != nil && why == "shutdown" {
		prep.cancel()
	}
	return len(ids)
}

// Close releases everything on shutdown.
func (h *Hub) Close() {
	if n := h.StopAll("shutdown"); n > 0 {
		log.Printf("perfsim: released %d stressors on shutdown", n)
	}
	h.work.SetThreads(context.Background(), 0)
	h.eng.Close()
}

// allDials is the engine's stressors followed by the network and disk ones.
func (h *Hub) allDials() []Dial {
	return append(h.eng.Dials(), h.cond.Dials()...)
}

func (h *Hub) SetThreads(n int) {
	h.work.SetThreads(h.ctx, n)
	h.mu.Lock()
	h.settings.Threads = h.work.Threads()
	h.mu.Unlock()
	h.saveSettings()
}

// SetBaseline measures the last 20 seconds as the reference.
func (h *Hub) SetBaseline() *Baseline {
	h.mu.Lock()
	h.baseline = h.baselineLocked()
	b := h.baseline
	h.mu.Unlock()
	h.event("neutral", "Baseline set from the last 20 seconds")
	return b
}

func (h *Hub) baselineLocked() *Baseline {
	w := h.points
	if len(w) > 20 {
		w = w[len(w)-20:]
	}
	if len(w) == 0 {
		return nil
	}
	b := &Baseline{T: time.Now().Unix()}
	for _, p := range w {
		b.WQ += p.WQ
		b.RQ += p.RQ
		b.P50 += p.P50
		b.P99 += p.P99
		b.Lag += maxOf(p.Lag)
		b.RecvQ += maxOf(p.RecvQ)
	}
	n := float64(len(w))
	b.WQ, b.RQ, b.P50, b.P99, b.Lag, b.RecvQ = b.WQ/n, b.RQ/n, b.P50/n, b.P99/n, b.Lag/n, b.RecvQ/n
	return b
}

func maxOf(m map[string]float64) float64 {
	v := 0.0
	for _, x := range m {
		v = math.Max(v, x)
	}
	return v
}

func (h *Hub) event(level, text string) {
	h.mu.Lock()
	h.events = append(h.events, Event{T: time.Now().Unix(), Level: level, Text: text})
	if len(h.events) > maxEvents {
		h.events = h.events[len(h.events)-maxEvents:]
	}
	h.mu.Unlock()
	log.Printf("perfsim: %s", text)
}

// trimBandsLocked drops bands that ended before the timeline's start.
func (h *Hub) trimBandsLocked() {
	if len(h.points) == 0 {
		return
	}
	first := h.points[0].T
	out := h.bands[:0]
	for _, b := range h.bands {
		if b.End == nil || *b.End >= first {
			out = append(out, b)
		}
	}
	h.bands = out
	for _, a := range h.active {
		for i := range h.bands {
			if h.bands[i].Seq == a.band.Seq {
				a.band = &h.bands[i]
			}
		}
	}
}

// --- datasets ---

func (h *Hub) refreshDatasets(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	ds, err := h.eng.Datasets(c)
	cancel()
	if err != nil {
		return
	}
	h.mu.Lock()
	h.datasets = ds
	h.mu.Unlock()
}

// Prepare builds a table of about the given size in the background.
func (h *Hub) Prepare(gb float64, pk bool) error {
	if gb <= 0 || gb > 200 {
		return errf("size must be between 0.1 and 200 GB")
	}
	h.mu.Lock()
	if h.prep != nil {
		h.mu.Unlock()
		return errf("a table is already being prepared")
	}
	name := fmt.Sprintf("big_%s", sizeName(gb))
	if !pk {
		name = fmt.Sprintf("nopk_%s", sizeName(gb))
	}
	for _, d := range h.datasets {
		if d.Name == name {
			h.mu.Unlock()
			return errf("%s already exists; drop it first to rebuild it", name)
		}
	}
	ctx, cancel := context.WithCancel(h.ctx)
	ps := &prepState{Name: name, Bytes: int64(gb * (1 << 30)), PK: pk, cancel: cancel}
	h.prep = ps
	h.mu.Unlock()
	h.event("neutral", fmt.Sprintf("Preparing %s (%s)", name, fmtBytes(ps.Bytes)))
	go func() {
		err := h.eng.Prepare(ctx, name, ps.Bytes, pk, func(f float64) {
			h.mu.Lock()
			ps.Done = f
			h.mu.Unlock()
		})
		h.mu.Lock()
		h.prep = nil
		h.mu.Unlock()
		switch {
		case ctx.Err() != nil:
			h.event("warn", name+": preparation cancelled; the rows written so far stay")
		case err != nil:
			h.event("warn", fmt.Sprintf("%s: preparation failed: %v", name, err))
		default:
			h.event("ok", name+" is ready")
		}
		h.refreshDatasets(h.ctx)
	}()
	return nil
}

func (h *Hub) CancelPrepare() {
	h.mu.Lock()
	if h.prep != nil {
		h.prep.cancel()
	}
	h.mu.Unlock()
}

func (h *Hub) Drop(name string) error {
	h.mu.Lock()
	for _, a := range h.active {
		if a.params.Str("table", "") == name {
			h.mu.Unlock()
			return errf("%s is in use by %s", name, a.label)
		}
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Minute)
	defer cancel()
	if err := h.eng.Drop(ctx, name); err != nil {
		return err
	}
	h.event("neutral", name+" dropped")
	h.refreshDatasets(h.ctx)
	return nil
}

func sizeName(gb float64) string {
	if gb < 1 {
		return fmt.Sprintf("%dm", int(gb*1024))
	}
	return fmt.Sprintf("%gg", gb)
}

func fmtBytes(b int64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%d MB", b>>20)
}

// --- sessions ---

func (h *Hub) Sessions(ctx context.Context) ([]Session, error) {
	h.mu.Lock()
	held := map[string][]string{}
	for _, a := range h.active {
		for m, ids := range a.run.Holders() {
			held[m] = append(held[m], ids...)
		}
	}
	h.mu.Unlock()
	return h.eng.Sessions(ctx, held)
}

// --- recording ---

func (h *Hub) RecordStart() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.recording != nil {
		return errf("already recording")
	}
	h.recording = &recording{start: time.Now()}
	return nil
}

func (h *Hub) RecordStop(name string) (*RunSummary, error) {
	h.mu.Lock()
	r := h.recording
	h.recording = nil
	if r == nil {
		h.mu.Unlock()
		return nil, errf("not recording")
	}
	start, end := r.start.Unix(), time.Now().Unix()
	run := Run{Target: h.info, Baseline: h.baseline, Params: map[string]Params{}}
	for _, p := range h.points {
		if p.T >= start {
			run.Points = append(run.Points, p)
		}
	}
	var names []string
	for _, b := range h.bands {
		if b.Start <= end && (b.End == nil || *b.End >= start) {
			run.Bands = append(run.Bands, b)
			names = append(names, b.Label)
		}
	}
	for _, e := range h.events {
		if e.T >= start {
			run.Events = append(run.Events, e)
		}
	}
	for id, p := range h.settings.Params {
		run.Params[id] = p
	}
	h.mu.Unlock()

	if name == "" {
		name = strings.Join(names, " + ")
		if name == "" {
			name = "Workload only"
		}
	}
	run.RunSummary = RunSummary{ID: fmt.Sprintf("%d", start), Name: name, Start: start, End: end, Stressor: strings.Join(names, " + "), LowWQ: -1}
	if run.Baseline != nil {
		run.BaseWQ = run.Baseline.WQ
	}
	for _, p := range run.Points {
		run.PeakLag = math.Max(run.PeakLag, maxOf(p.Lag))
		run.PeakP99 = math.Max(run.PeakP99, p.P99)
		if run.LowWQ < 0 || p.WQ < run.LowWQ {
			run.LowWQ = p.WQ
		}
	}
	b, _ := json.Marshal(run)
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := h.eng.Save(ctx, "run:"+run.ID, b); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.runs = append([]RunSummary{run.RunSummary}, h.runs...)
	h.mu.Unlock()
	h.event("ok", "Run saved: "+name)
	return &run.RunSummary, nil
}

func (h *Hub) RunJSON(ctx context.Context, id string) ([]byte, error) {
	all, err := h.eng.Load(ctx, "run:"+id)
	if err != nil {
		return nil, err
	}
	b, ok := all["run:"+id]
	if !ok {
		return nil, errf("no such run")
	}
	return b, nil
}

func (h *Hub) DeleteRun(ctx context.Context, id string) error {
	if err := h.eng.Delete(ctx, "run:"+id); err != nil {
		return err
	}
	h.mu.Lock()
	out := h.runs[:0]
	for _, r := range h.runs {
		if r.ID != id {
			out = append(out, r)
		}
	}
	h.runs = out
	h.mu.Unlock()
	return nil
}

// --- saved settings ---

func (h *Hub) loadSaved(ctx context.Context) {
	all, err := h.eng.Load(ctx, "")
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if b, ok := all["settings"]; ok {
		var s settings
		if json.Unmarshal(b, &s) == nil {
			if s.Params == nil {
				s.Params = map[string]Params{}
			}
			if s.Targets == nil {
				s.Targets = map[string][]string{}
			}
			h.settings = s
		}
	}
	for k, b := range all {
		if !strings.HasPrefix(k, "run:") {
			continue
		}
		var r Run
		if json.Unmarshal(b, &r) == nil {
			h.runs = append(h.runs, r.RunSummary)
		}
	}
	sort.Slice(h.runs, func(i, j int) bool { return h.runs[i].Start > h.runs[j].Start })
}

func (h *Hub) saveSettings() {
	h.mu.Lock()
	b, _ := json.Marshal(h.settings)
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	if err := h.eng.Save(ctx, "settings", b); err != nil {
		log.Printf("perfsim: saving settings: %v", err)
	}
}

// withDefaults fills every param the browser left out with the dial's default.
func withDefaults(d Dial, p Params) Params {
	out := Params{}
	for _, pr := range d.Params {
		if v, ok := p[pr.Key]; ok && v != nil && v != "" {
			out[pr.Key] = v
		} else {
			out[pr.Key] = pr.Def
		}
	}
	if v, ok := p["table"]; ok {
		out["table"] = v
	}
	return out
}
