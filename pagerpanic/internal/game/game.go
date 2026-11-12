package game

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// Phases of a game.
const (
	PhaseTitle    = "title"    // waiting for a mode, or for DBCanvas
	PhaseChecking = "checking" // the systems check is running
	PhaseReady    = "ready"    // the check passed; press start
	PhasePlaying  = "playing"
	PhasePaused   = "paused"
	PhaseEnding   = "ending" // the shift is over; tickets are being cleaned up
	PhaseOver     = "over"   // the debrief
)

// Modes.
const (
	ModeArcade   = "arcade"
	ModeMarathon = "marathon"
	ModePractice = "practice"
)

// Ticket states.
const (
	TicketIncoming = "incoming" // being set up on the cluster; its clock has not started
	TicketOpen     = "open"
	TicketCleared  = "cleared"
	TicketExpired  = "expired"
	TicketGivenUp  = "given-up"
	TicketUnsolved = "unsolved" // still open when the shift ended
	TicketSkipped  = "skipped"  // its setup failed; it never counted
)

const (
	checkEvery     = 10 * time.Second
	connectedFor   = 15 * time.Second
	endingTimeout  = 4 * time.Minute
	maxPauses      = 3
	firstSpawnAt   = 4 * time.Second
	highScoreSlots = 10

	// With no ticket left in play, the next one arrives within emptyQueueWait.
	emptyQueueWait = 30 * time.Second
	// A ticket cleared within fastClear of its time limit gives the next ticket a head
	// start: half the time it left unused, up to half of the next ticket's own limit.
	fastClear = 0.5
)

// Ticket is one drawn task.
type Ticket struct {
	ID         string  `json:"id"`
	TaskID     string  `json:"task"`
	Title      string  `json:"title"`
	Brief      string  `json:"brief"`
	Kind       string  `json:"kind"`
	Category   string  `json:"category"`
	Difficulty int     `json:"difficulty"`
	Points     int     `json:"points"`
	Boss       bool    `json:"boss,omitempty"`
	Stage      int     `json:"stage"`
	State      string  `json:"state"`
	Limit      float64 `json:"limit"`           // seconds; 0 is untimed
	Extra      float64 `json:"extra,omitempty"` // seconds of Limit that are a head start
	Worker     string  `json:"worker,omitempty"`
	Error      string  `json:"error,omitempty"`

	HintCosts   []int         `json:"hintCosts,omitempty"`
	HintsBought int           `json:"hintsBought"`
	Checks      []CheckResult `json:"checks,omitempty"`
	Earned      int           `json:"earned"`
	TookSecs    float64       `json:"took"`

	openedAt  time.Duration // played time when its clock started
	closedAt  time.Duration
	hints     []string
	solution  string
	explain   string
	checkedAt time.Time
	checkID   string
	cleanID   string
	cleaned   bool
	setupID   string
	locks     []string
	blocks    []string
	needs     []string
	hintSpent int
}

// Event is something the page animates: a ticket arriving, a clear, a lost life.
type Event struct {
	Seq    int    `json:"seq"`
	Kind   string `json:"kind"` // spawn | clear | expire | giveup | hint | stage | lifeup | victory | gameover | quit | timeup | practice-done | medal | error
	Text   string `json:"text,omitempty"`
	Points int    `json:"points,omitempty"`
	Ticket string `json:"ticket,omitempty"`
}

// Game is one Pager Panic server's state. Every method locks it.
type Game struct {
	mu  sync.Mutex
	now func() time.Time

	// What DBCanvas last said.
	lastSync   time.Time
	stack      StackRef
	nodes      []Node
	env        Env
	bank       *Bank
	bankHash   string
	bankErrors []string
	clusterErr string

	phase    string
	mode     string
	practice string
	diag     *Diag

	seed        int64
	rng         *rand.Rand
	played      time.Duration
	lastTick    time.Time
	nextSpawn   time.Duration
	stage       int
	stageLost   int
	bossSpawned bool
	pauses      int
	headStart   float64 // seconds the next ticket gets on top of its limit

	lives      int
	score      int
	streak     int
	bestStreak int
	hintsUsed  int
	tickets    []*Ticket
	drawn      map[string]int
	catTurn    int

	outcome      string
	endedAt      time.Time
	newMedals    []string
	enterInitial bool
	rank         string

	events []Event
	seq    int

	// Work for DBCanvas. boot keeps one run of the game's IDs apart from another's: DBCanvas
	// remembers finished actions for a while, so an ID the last run used would be skipped.
	boot    string
	actions map[string]Action
	acked   []string
	nextID  int

	save      Save
	haveSave  bool
	saveDirty bool
}

// Diag is the systems check as the page shows it.
type Diag struct {
	ID    string     `json:"-"`
	Steps []DiagStep `json:"steps"`
	Done  bool       `json:"done"`
	OK    bool       `json:"ok"`
	Error string     `json:"error,omitempty"`
}

func New(now func() time.Time) *Game {
	if now == nil {
		now = time.Now
	}
	return &Game{now: now, phase: PhaseTitle, actions: map[string]Action{}, drawn: map[string]int{},
		boot: fmt.Sprintf("%06x", rand.Int63()&0xffffff)}
}

// ---------------------------------------------------------------- DBCanvas sync

// Sync takes in what DBCanvas sent and answers with the work to do next.
func (g *Game) Sync(req SyncRequest) SyncResponse {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.now()
	g.lastSync = t
	g.stack, g.nodes, g.env = req.Stack, req.Nodes, req.Env
	g.bankErrors, g.clusterErr = req.BankErrors, req.ClusterErr
	if req.Bank != nil {
		b := *req.Bank
		g.bank, g.bankHash = &b, req.BankHash
	}
	if len(req.Saved) > 0 && !g.haveSave {
		var s Save
		if json.Unmarshal(req.Saved, &s) == nil {
			g.save = s
		}
		g.haveSave = true
	}
	var ack []string
	for _, r := range req.Results {
		if g.apply(r) {
			ack = append(ack, r.ID)
		}
	}
	g.tick()
	resp := SyncResponse{Ack: ack, NeedBank: g.bank == nil || g.bankHash != req.BankHash, NeedSaved: !g.haveSave}
	for _, a := range g.actions {
		resp.Actions = append(resp.Actions, a)
	}
	sort.Slice(resp.Actions, func(i, j int) bool { return resp.Actions[i].ID < resp.Actions[j].ID })
	for _, tk := range g.tickets {
		if !tk.cleaned && tk.State != TicketSkipped {
			resp.Active = append(resp.Active, tk.setupID)
		}
	}
	if resp.Active == nil {
		resp.Active = []string{}
	}
	if resp.Ack == nil {
		resp.Ack = []string{}
	}
	if resp.Actions == nil {
		resp.Actions = []Action{}
	}
	if g.saveDirty && g.haveSave {
		resp.Save, _ = json.Marshal(g.save)
		g.saveDirty = false
	}
	return resp
}

func (g *Game) newAction(op string, fill func(*Action)) Action {
	g.nextID++
	a := Action{ID: fmt.Sprintf("%s-%s-%d", op, g.boot, g.nextID), Op: op}
	if fill != nil {
		fill(&a)
	}
	g.actions[a.ID] = a
	return a
}

// apply takes one result in. It reports whether the result is final, so DBCanvas can
// forget it.
func (g *Game) apply(r Result) bool {
	if _, want := g.actions[r.ID]; !want {
		// Not ours (any more): acknowledge it so it stops coming.
		return r.Done
	}
	switch r.Op {
	case "diagnose":
		if g.diag != nil && g.diag.ID == r.ID {
			g.diag.Steps = r.Steps
			if r.Done {
				g.diag.Done, g.diag.OK, g.diag.Error = true, r.OK && r.Error == "", r.Error
				if r.Env != nil {
					g.env = *r.Env
				}
				if g.phase == PhaseChecking {
					if g.diag.OK && g.playable() > 0 {
						g.phase = PhaseReady
					} else {
						if g.diag.OK {
							g.diag.OK, g.diag.Error = false, "no task in the bank can run on this cluster"
						}
						g.phase = PhaseTitle
					}
				}
			}
		}
	case "setup":
		if r.Done {
			g.setupDone(r)
		}
	case "check":
		if r.Done {
			g.checkDone(r)
		}
	case "cleanup":
		if r.Done {
			for _, tk := range g.tickets {
				if tk.cleanID == r.ID {
					tk.cleaned = true
				}
			}
		}
	}
	if r.Done {
		delete(g.actions, r.ID)
	}
	return r.Done
}

// ---------------------------------------------------------------- the player's moves

// CheckSystem starts the systems check.
func (g *Game) CheckSystem() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.phase {
	case PhaseTitle, PhaseReady, PhaseOver:
	default:
		return fmt.Errorf("a game is in progress")
	}
	if err := g.connectedErr(); err != nil {
		return err
	}
	if g.diag != nil && !g.diag.Done {
		return nil
	}
	a := g.newAction("diagnose", nil)
	g.diag = &Diag{ID: a.ID}
	g.phase = PhaseChecking
	g.resetRun()
	return nil
}

// Begin starts a shift. A mode needs a passed systems check, at most 10 minutes old.
func (g *Game) Begin(mode, task string, seed int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.phase != PhaseReady {
		return fmt.Errorf("run the systems check first")
	}
	if g.bank == nil {
		return fmt.Errorf("no task bank yet")
	}
	switch mode {
	case ModeArcade, ModeMarathon:
	case ModePractice:
		pt, ok := g.publicTask(task)
		if !ok {
			return fmt.Errorf("no task %q", task)
		}
		if pt.Unsupported != "" {
			return fmt.Errorf("%s: %s", pt.Title, pt.Unsupported)
		}
	default:
		return fmt.Errorf("mode is arcade, marathon or practice")
	}
	g.resetRun()
	if seed == 0 {
		seed = g.now().UnixNano() & 0x7fffffff
	}
	g.seed, g.rng = seed, rand.New(rand.NewSource(seed))
	g.mode, g.practice = mode, task
	g.lives = g.bank.Lives
	g.phase = PhasePlaying
	g.lastTick = g.now()
	g.nextSpawn = firstSpawnAt
	if mode == ModePractice {
		pt, _ := g.publicTask(task)
		g.spawn(pt)
	}
	return nil
}

func (g *Game) resetRun() {
	g.tickets, g.drawn, g.events = nil, map[string]int{}, nil
	g.played, g.stage, g.stageLost, g.bossSpawned, g.pauses, g.headStart = 0, 0, 0, false, 0, 0
	g.score, g.streak, g.bestStreak, g.hintsUsed, g.catTurn = 0, 0, 0, 0, 0
	g.outcome, g.newMedals, g.enterInitial, g.rank = "", nil, false, ""
	for id, a := range g.actions {
		if a.Op != "diagnose" && a.Op != "cleanup" {
			delete(g.actions, id)
		}
	}
}

func (g *Game) Pause() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.phase != PhasePlaying {
		return fmt.Errorf("not playing")
	}
	if g.mode == ModeMarathon {
		return fmt.Errorf("Marathon has no pause")
	}
	if g.mode == ModeArcade && g.pauses >= maxPauses {
		return fmt.Errorf("no pauses left this shift")
	}
	g.tick()
	g.pauses++
	g.phase = PhasePaused
	return nil
}

func (g *Game) Resume() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.phase != PhasePaused {
		return fmt.Errorf("not paused")
	}
	g.phase = PhasePlaying
	g.lastTick = g.now()
	return nil
}

func (g *Game) Hint(id string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	tk := g.ticket(id)
	if tk == nil || tk.State != TicketOpen {
		return "", fmt.Errorf("no open ticket %s", id)
	}
	if g.mode == ModeMarathon {
		return "", fmt.Errorf("Marathon has no hints")
	}
	if tk.HintsBought >= len(tk.hints) {
		return "", fmt.Errorf("no more hints for this ticket")
	}
	cost := 0
	if tk.HintsBought < len(tk.HintCosts) {
		cost = tk.HintCosts[tk.HintsBought]
	}
	text := tk.hints[tk.HintsBought]
	tk.HintsBought++
	tk.hintSpent += cost
	g.hintsUsed++
	if g.mode != ModePractice {
		g.score = max(0, g.score-cost)
	}
	g.emit(Event{Kind: "hint", Text: text, Points: -cost, Ticket: tk.ID})
	return text, nil
}

func (g *Game) GiveUp(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	tk := g.ticket(id)
	if tk == nil || tk.State != TicketOpen {
		return fmt.Errorf("no open ticket %s", id)
	}
	g.close(tk, TicketGivenUp)
	g.streak = 0
	g.emit(Event{Kind: "giveup", Text: "Given up: " + tk.Title, Ticket: tk.ID})
	if g.mode == ModePractice {
		g.end("practice-done")
	}
	return nil
}

func (g *Game) CheckNow(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	tk := g.ticket(id)
	if tk == nil || tk.State != TicketOpen {
		return fmt.Errorf("no open ticket %s", id)
	}
	if tk.checkID == "" {
		g.requestCheck(tk)
	}
	return nil
}

func (g *Game) Quit() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.phase {
	case PhasePlaying, PhasePaused:
		g.end("quit")
		return nil
	case PhaseChecking, PhaseReady:
		g.phase = PhaseTitle
		return nil
	}
	return fmt.Errorf("nothing to quit")
}

// Title goes back to the title screen from the debrief.
func (g *Game) Title() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.phase != PhaseOver && g.phase != PhaseReady {
		return fmt.Errorf("finish the shift first")
	}
	g.phase = PhaseTitle
	g.enterInitial = false
	return nil
}

// Initials puts the last score in the high-score table.
func (g *Game) Initials(name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.phase != PhaseOver || !g.enterInitial {
		return fmt.Errorf("no score to enter")
	}
	name = strings.ToUpper(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() == 3 {
			break
		}
	}
	if b.Len() == 0 {
		return fmt.Errorf("type up to 3 letters")
	}
	g.save.addScore(HighScore{Initials: b.String(), Score: g.score, Mode: g.mode, Rank: g.rank,
		Stage: g.stage + 1, Seed: g.seed, At: g.now().UTC().Format(time.RFC3339)})
	g.enterInitial = false
	g.saveDirty = true
	return nil
}

// ---------------------------------------------------------------- the clock

// Tick advances the game; the server calls it a few times a second.
func (g *Game) Tick() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tick()
}

func (g *Game) tick() {
	t := g.now()
	if g.phase == PhasePlaying {
		if !g.lastTick.IsZero() {
			g.played += t.Sub(g.lastTick)
		}
	}
	g.lastTick = t
	switch g.phase {
	case PhasePlaying:
		g.playTick(t)
	case PhaseEnding:
		g.endingTick(t)
	}
}

func (g *Game) shift() time.Duration {
	if g.bank == nil || g.bank.Shift <= 0 {
		return time.Hour
	}
	return time.Duration(g.bank.Shift * float64(time.Second))
}

func (g *Game) stageLen() time.Duration {
	n := 1
	if g.bank != nil && len(g.bank.Stages) > 0 {
		n = len(g.bank.Stages)
	}
	return g.shift() / time.Duration(n)
}

func (g *Game) curStage() Stage {
	if g.bank == nil || len(g.bank.Stages) == 0 {
		return Stage{Open: 1, SpawnEvery: 120, TimeScale: 1, Difficulty: []int{1, 2, 3}}
	}
	return g.bank.Stages[min(g.stage, len(g.bank.Stages)-1)]
}

func (g *Game) playTick(t time.Time) {
	// Tickets: timers, then checks.
	for _, tk := range g.tickets {
		if tk.State != TicketOpen {
			continue
		}
		if tk.Limit > 0 && g.remaining(tk) <= 0 {
			g.expire(tk)
			if g.phase != PhasePlaying {
				return
			}
			continue
		}
		if tk.checkID == "" && t.Sub(tk.checkedAt) >= checkEvery {
			g.requestCheck(tk)
		}
	}
	if g.mode == ModePractice {
		return
	}
	// The shift clock.
	if g.played >= g.shift() {
		if g.mode == ModeArcade {
			g.end("victory")
		} else {
			g.end("timeup")
		}
		return
	}
	if s := int(g.played / g.stageLen()); s > g.stage && g.bank != nil && s < len(g.bank.Stages) {
		g.stageClear(s)
	}
	// The boss arrives when the last stage starts.
	if g.mode == ModeArcade && !g.bossSpawned && g.bank != nil && g.stage == len(g.bank.Stages)-1 {
		g.bossSpawned = true
		if pt, ok := g.pick(true); ok {
			g.spawn(pt)
			g.nextSpawn = g.played + g.spawnGap()
		}
	}
	if g.played >= g.nextSpawn {
		if g.inPlay() < g.curStage().Open {
			if pt, ok := g.pick(false); ok {
				g.spawn(pt)
				g.nextSpawn = g.played + g.spawnGap()
			} else {
				g.nextSpawn = g.played + 10*time.Second
			}
		} else {
			// A slot frees up when a ticket closes; look again shortly after.
			g.nextSpawn = g.played + 5*time.Second
		}
	}
}

func (g *Game) spawnGap() time.Duration {
	base := g.curStage().SpawnEvery
	if base <= 0 {
		base = 120
	}
	f := 0.75 + g.rng.Float64()*0.5
	return time.Duration(base * f * float64(time.Second))
}

func (g *Game) inPlay() int {
	n := 0
	for _, tk := range g.tickets {
		if tk.State == TicketOpen || tk.State == TicketIncoming {
			n++
		}
	}
	return n
}

func (g *Game) remaining(tk *Ticket) float64 {
	if tk.State != TicketOpen || tk.Limit <= 0 {
		return tk.Limit
	}
	return tk.Limit - (g.played - tk.openedAt).Seconds()
}

func (g *Game) stageClear(next int) {
	bonus := 0
	text := fmt.Sprintf("STAGE %d CLEAR", g.stage+1)
	if g.mode == ModeArcade {
		bonus = 250 * (g.stage + 1)
		if g.stageLost == 0 {
			bonus += 250
			text += " · NO LIFE LOST"
			if g.lives < g.bank.Lives {
				g.lives++
				g.emit(Event{Kind: "lifeup", Text: "+1 LIFE"})
			}
		}
	}
	g.score += bonus
	g.emit(Event{Kind: "stage", Text: text, Points: bonus})
	g.stage, g.stageLost = next, 0
}

// ---------------------------------------------------------------- drawing tickets

func (g *Game) publicTask(id string) (PublicTask, bool) {
	if g.bank == nil {
		return PublicTask{}, false
	}
	for _, t := range g.bank.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return PublicTask{}, false
}

func (g *Game) playable() int {
	if g.bank == nil {
		return 0
	}
	n := 0
	for _, t := range g.bank.Tasks {
		if t.Unsupported == "" {
			n++
		}
	}
	return n
}

// pick chooses the next task: supported, allowed in this stage, clear of every open
// ticket's locks and blocked capabilities, and drawn as few times as possible so far.
func (g *Game) pick(boss bool) (PublicTask, bool) {
	if g.bank == nil {
		return PublicTask{}, false
	}
	locks, blocked, needed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, tk := range g.tickets {
		if tk.State == TicketOpen || tk.State == TicketIncoming || (!tk.cleaned && tk.State != TicketSkipped) {
			for _, l := range tk.locks {
				locks[l] = true
			}
			for _, b := range tk.blocks {
				blocked[b] = true
			}
			for _, n := range tk.needs {
				needed[n] = true
			}
		}
	}
	stage := g.curStage()
	allowed := map[int]bool{}
	for _, d := range stage.Difficulty {
		allowed[d] = true
	}
	var cands []PublicTask
	for _, t := range g.bank.Tasks {
		if t.Unsupported != "" || t.Boss != boss {
			continue
		}
		if !boss && g.mode == ModeArcade && !allowed[t.Difficulty] {
			continue
		}
		if clash(t.Locks, locks) || clash(t.Needs, blocked) || clash(t.Blocks, needed) {
			continue
		}
		cands = append(cands, t)
	}
	if len(cands) == 0 {
		return PublicTask{}, false
	}
	// Marathon takes the categories in turn, so the shift covers all of them.
	if g.mode == ModeMarathon && len(g.bank.Categories) > 0 {
		for i := 0; i < len(g.bank.Categories); i++ {
			cat := g.bank.Categories[(g.catTurn+i)%len(g.bank.Categories)].ID
			var inCat []PublicTask
			for _, t := range cands {
				if t.Category == cat {
					inCat = append(inCat, t)
				}
			}
			if len(inCat) > 0 {
				g.catTurn = (g.catTurn + i + 1) % len(g.bank.Categories)
				cands = inCat
				break
			}
		}
	}
	least := math.MaxInt
	for _, t := range cands {
		least = min(least, g.drawn[t.ID])
	}
	var pool []PublicTask
	for _, t := range cands {
		if g.drawn[t.ID] == least {
			pool = append(pool, t)
		}
	}
	return pool[g.rng.Intn(len(pool))], true
}

func clash(want []string, held map[string]bool) bool {
	for _, w := range want {
		if held[w] {
			return true
		}
	}
	return false
}

func (g *Game) spawn(pt PublicTask) {
	g.drawn[pt.ID]++
	scale := g.curStage().TimeScale
	if scale <= 0 || g.mode != ModeArcade {
		scale = 1
	}
	limit := pt.TimeLimit * scale
	if g.mode == ModePractice {
		limit = 0
	}
	extra := 0.0
	if limit > 0 && g.headStart > 0 {
		extra = math.Round(math.Min(g.headStart, limit*0.5))
		limit += extra
		g.headStart = 0
	}
	a := g.newAction("setup", func(a *Action) { a.Task, a.Seed = pt.ID, g.rng.Int63() })
	tk := &Ticket{ID: a.ID, setupID: a.ID, TaskID: pt.ID, Title: pt.Title, Kind: pt.Kind, Category: pt.Category,
		Difficulty: pt.Difficulty, Points: pt.Points, Boss: pt.Boss, Stage: g.stage + 1, State: TicketIncoming,
		Limit: math.Round(limit), Extra: extra, HintCosts: pt.Hints, locks: pt.Locks, blocks: pt.Blocks, needs: pt.Needs}
	g.tickets = append(g.tickets, tk)
}

func (g *Game) setupDone(r Result) {
	tk := g.ticketBySetup(r.ID)
	if tk == nil || tk.State != TicketIncoming {
		return
	}
	if !r.OK || r.View == nil {
		tk.State, tk.Error, tk.cleaned = TicketSkipped, firstNonEmpty(r.Error, "setup failed"), true
		g.emit(Event{Kind: "error", Text: "A ticket could not be set up and was skipped: " + tk.Title, Ticket: tk.ID})
		if g.mode == ModePractice {
			g.end("practice-done")
		}
		return
	}
	if g.phase == PhaseEnding || g.phase == PhaseOver {
		tk.State = TicketUnsolved
		g.requestCleanup(tk)
		return
	}
	tk.State, tk.Worker = TicketOpen, r.Worker
	tk.Title, tk.Brief = r.View.Title, r.View.Brief
	tk.hints, tk.solution, tk.explain = r.View.Hints, r.View.Solution, r.View.Explain
	if len(tk.hints) < len(tk.HintCosts) {
		tk.HintCosts = tk.HintCosts[:len(tk.hints)]
	}
	tk.openedAt = g.played
	tk.checkedAt = g.now()
	text := tk.Title
	if tk.Boss {
		text = "BOSS INCOMING · " + text
	}
	if tk.Extra > 0 {
		text += fmt.Sprintf(" · +%.0fs HEAD START", tk.Extra)
	}
	g.emit(Event{Kind: "spawn", Text: text, Ticket: tk.ID})
}

func (g *Game) requestCheck(tk *Ticket) {
	a := g.newAction("check", func(a *Action) { a.Instance = tk.setupID })
	tk.checkID = a.ID
}

func (g *Game) checkDone(r Result) {
	var tk *Ticket
	for _, t := range g.tickets {
		if t.checkID == r.ID {
			tk = t
		}
	}
	if tk == nil {
		return
	}
	tk.checkID, tk.checkedAt = "", g.now()
	if !r.OK {
		return
	}
	tk.Checks = r.Checks
	if tk.State == TicketOpen && r.Solved && (g.phase == PhasePlaying || g.phase == PhasePaused) {
		g.clear(tk)
	}
}

func (g *Game) clear(tk *Ticket) {
	g.close(tk, TicketCleared)
	g.streak++
	g.bestStreak = max(g.bestStreak, g.streak)
	base := float64(tk.Points)
	if tk.Boss {
		base *= 3
	}
	bonus := 0.0
	if tk.Limit > 0 {
		bonus = base * 0.5 * math.Max(0, 1-tk.TookSecs/tk.Limit)
	}
	mult := math.Min(3, 1+0.25*float64(g.streak-1))
	earned := int(math.Round((base + bonus) * mult))
	if g.mode == ModePractice {
		earned = 0
	}
	tk.Earned = earned
	g.score += earned
	if tk.Limit > 0 && tk.TookSecs <= tk.Limit*fastClear && g.inPlay() == 0 && g.mode != ModePractice {
		g.headStart = (tk.Limit - tk.TookSecs) / 2
	}
	g.save.CategoryClears = addCount(g.save.CategoryClears, tk.Category)
	g.saveDirty = true
	text := "CLEARED · " + tk.Title
	if g.streak > 1 {
		text = fmt.Sprintf("×%d COMBO · %s", g.streak, text)
	}
	g.emit(Event{Kind: "clear", Text: text, Points: earned, Ticket: tk.ID})
	if g.mode == ModePractice {
		g.end("practice-done")
	}
}

func (g *Game) expire(tk *Ticket) {
	g.close(tk, TicketExpired)
	g.streak = 0
	if g.mode != ModeArcade {
		g.emit(Event{Kind: "expire", Text: "TIME UP · " + tk.Title, Ticket: tk.ID})
		return
	}
	g.lives--
	g.stageLost++
	g.emit(Event{Kind: "expire", Text: "TICKET ESCALATED · " + tk.Title, Ticket: tk.ID})
	if g.lives <= 0 {
		g.end("gameover")
	}
}

func (g *Game) close(tk *Ticket, state string) {
	tk.State = state
	tk.closedAt = g.played
	tk.TookSecs = math.Round((g.played - tk.openedAt).Seconds())
	g.requestCleanup(tk)
	// An empty queue does not wait out the stage's spawn gap.
	if g.phase == PhasePlaying && g.mode != ModePractice && g.inPlay() == 0 {
		soon := g.played + emptyQueueWait/2 + time.Duration(g.rng.Int63n(int64(emptyQueueWait/2)+1))
		if soon < g.nextSpawn {
			g.nextSpawn = soon
		}
	}
}

func (g *Game) requestCleanup(tk *Ticket) {
	if tk.cleanID != "" || tk.cleaned {
		return
	}
	if tk.checkID != "" {
		delete(g.actions, tk.checkID)
		tk.checkID = ""
	}
	a := g.newAction("cleanup", func(a *Action) { a.Instance = tk.setupID })
	tk.cleanID = a.ID
}

// ---------------------------------------------------------------- the end

func (g *Game) end(outcome string) {
	g.outcome = outcome
	g.endedAt = g.now()
	for _, tk := range g.tickets {
		switch tk.State {
		case TicketOpen:
			tk.State = TicketUnsolved
			tk.closedAt = g.played
			tk.TookSecs = math.Round((g.played - tk.openedAt).Seconds())
			g.requestCleanup(tk)
		}
	}
	g.phase = PhaseEnding
	if g.bank != nil {
		g.rank = g.bankRank(g.score)
	}
	g.award()
	g.emit(Event{Kind: outcome, Text: outcomeText(outcome), Points: g.score})
	g.endingTick(g.now())
}

func outcomeText(o string) string {
	switch o {
	case "victory":
		return "SHIFT COMPLETE"
	case "gameover":
		return "GAME OVER"
	case "quit":
		return "SHIFT ENDED EARLY"
	case "timeup":
		return "TIME'S UP"
	}
	return "PRACTICE DONE"
}

func (g *Game) endingTick(t time.Time) {
	waiting := false
	for _, tk := range g.tickets {
		if tk.State == TicketIncoming {
			waiting = true
			continue
		}
		if !tk.cleaned && tk.State != TicketSkipped {
			g.requestCleanup(tk)
			waiting = true
		}
	}
	if !waiting || t.Sub(g.endedAt) > endingTimeout {
		g.phase = PhaseOver
	}
}

func (g *Game) bankRank(score int) string {
	best, bestMin := "", -1
	for _, r := range g.bank.Ranks {
		if score >= r.Min && r.Min > bestMin {
			best, bestMin = r.Title, r.Min
		}
	}
	return best
}

// award hands out this shift's medals and records the shift.
func (g *Game) award() {
	if g.mode == ModePractice {
		return
	}
	gain := func(id, title, why string) {
		if g.save.Medals == nil {
			g.save.Medals = map[string]Medal{}
		}
		if _, have := g.save.Medals[id]; have {
			return
		}
		g.save.Medals[id] = Medal{Title: title, Why: why, At: g.now().UTC().Format(time.RFC3339)}
		g.newMedals = append(g.newMedals, title)
		g.emit(Event{Kind: "medal", Text: "MEDAL · " + title})
	}
	lost := 0
	cleared := 0
	for _, tk := range g.tickets {
		if tk.State == TicketExpired {
			lost++
		}
		if tk.State == TicketCleared {
			cleared++
			if tk.Limit > 0 && tk.TookSecs <= tk.Limit*0.25 {
				gain("speedrunner", "Speedrunner", "cleared a ticket in under a quarter of its time")
			}
			if tk.Boss {
				gain("boss-slayer", "Boss Slayer", "cleared a boss ticket")
			}
		}
	}
	if g.outcome == "victory" {
		gain("shift-survivor", "Shift Survivor", "finished an Arcade shift")
		if lost == 0 {
			gain("flawless", "Flawless", "finished an Arcade shift without losing a life")
		}
		if g.hintsUsed == 0 {
			gain("no-hints", "No Hints", "finished an Arcade shift without a hint")
		}
	}
	if g.outcome == "timeup" && g.mode == ModeMarathon {
		gain("night-owl", "Night Owl", "played a Marathon to the end")
	}
	if g.bestStreak >= 5 {
		gain("combo-five", "Combo ×5", "cleared 5 tickets in a row")
	}
	if g.bank != nil {
		for _, c := range g.bank.Categories {
			if c.Medal != "" && c.MedalAt > 0 && g.save.CategoryClears[c.ID] >= c.MedalAt {
				gain("cat-"+c.ID, c.Medal, fmt.Sprintf("cleared %d %s tickets", c.MedalAt, c.Title))
			}
		}
	}
	g.save.addRun(RunSummary{Mode: g.mode, Outcome: g.outcome, Score: g.score, Rank: g.rank, Stage: g.stage + 1,
		Cleared: cleared, Tickets: len(g.tickets), Seed: g.seed, At: g.now().UTC().Format(time.RFC3339)})
	g.enterInitial = g.score > 0 && g.save.qualifies(g.mode, g.score)
	g.saveDirty = true
}

// ---------------------------------------------------------------- helpers

func (g *Game) emit(e Event) {
	g.seq++
	e.Seq = g.seq
	g.events = append(g.events, e)
	if len(g.events) > 200 {
		g.events = g.events[len(g.events)-200:]
	}
}

func (g *Game) ticket(id string) *Ticket {
	for _, tk := range g.tickets {
		if tk.ID == id {
			return tk
		}
	}
	return nil
}

func (g *Game) ticketBySetup(id string) *Ticket {
	for _, tk := range g.tickets {
		if tk.setupID == id {
			return tk
		}
	}
	return nil
}

func (g *Game) connectedErr() error {
	if g.lastSync.IsZero() || g.now().Sub(g.lastSync) > connectedFor {
		return fmt.Errorf("waiting for DBCanvas to connect")
	}
	if g.clusterErr != "" {
		return fmt.Errorf("%s", g.clusterErr)
	}
	if g.bank == nil {
		return fmt.Errorf("no task bank yet")
	}
	return nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func addCount(m map[string]int, k string) map[string]int {
	if m == nil {
		m = map[string]int{}
	}
	m[k]++
	return m
}
