package game

import (
	"math"
	"sort"
	"time"
)

// Save is what outlives a game container: DBCanvas keeps it on the node's deployment.
type Save struct {
	HighScores     []HighScore      `json:"highScores,omitempty"`
	Medals         map[string]Medal `json:"medals,omitempty"`
	Runs           []RunSummary     `json:"runs,omitempty"`
	CategoryClears map[string]int   `json:"categoryClears,omitempty"`
}

type HighScore struct {
	Initials string `json:"initials"`
	Score    int    `json:"score"`
	Mode     string `json:"mode"`
	Rank     string `json:"rank"`
	Stage    int    `json:"stage"`
	Seed     int64  `json:"seed"`
	At       string `json:"at"`
}

type Medal struct {
	Title string `json:"title"`
	Why   string `json:"why"`
	At    string `json:"at"`
}

type RunSummary struct {
	Mode    string `json:"mode"`
	Outcome string `json:"outcome"`
	Score   int    `json:"score"`
	Rank    string `json:"rank"`
	Stage   int    `json:"stage"`
	Cleared int    `json:"cleared"`
	Tickets int    `json:"tickets"`
	Seed    int64  `json:"seed"`
	At      string `json:"at"`
}

func (s *Save) qualifies(mode string, score int) bool {
	n := 0
	for _, h := range s.HighScores {
		if h.Mode == mode {
			n++
			if score > h.Score {
				return true
			}
		}
	}
	return n < highScoreSlots
}

func (s *Save) addScore(h HighScore) {
	s.HighScores = append(s.HighScores, h)
	sort.SliceStable(s.HighScores, func(i, j int) bool { return s.HighScores[i].Score > s.HighScores[j].Score })
	per := map[string]int{}
	kept := s.HighScores[:0]
	for _, x := range s.HighScores {
		if per[x.Mode] < highScoreSlots {
			kept = append(kept, x)
			per[x.Mode]++
		}
	}
	s.HighScores = kept
}

func (s *Save) addRun(r RunSummary) {
	s.Runs = append([]RunSummary{r}, s.Runs...)
	if len(s.Runs) > 50 {
		s.Runs = s.Runs[:50]
	}
}

// View is everything the page draws. Solutions appear only in the debrief, and a hint
// only once it is bought.
type View struct {
	Phase      string     `json:"phase"`
	Mode       string     `json:"mode,omitempty"`
	Connected  bool       `json:"connected"`
	Waiting    string     `json:"waiting,omitempty"` // why the game cannot start
	Stack      StackRef   `json:"stack"`
	Nodes      []Node     `json:"nodes"`
	Env        Env        `json:"env"`
	BankTitle  string     `json:"bankTitle,omitempty"`
	BankErrors []string   `json:"bankErrors,omitempty"`
	Categories []Category `json:"categories,omitempty"`
	Tasks      []TaskInfo `json:"tasks,omitempty"`
	Diag       *Diag      `json:"diag,omitempty"`

	Clock      Clock        `json:"clock"`
	Lives      int          `json:"lives"`
	MaxLives   int          `json:"maxLives"`
	Score      int          `json:"score"`
	Streak     int          `json:"streak"`
	Multiplier float64      `json:"multiplier"`
	Pauses     int          `json:"pausesLeft"`
	Tickets    []TicketView `json:"tickets"`
	Events     []Event      `json:"events"`
	Seq        int          `json:"seq"`

	Debrief      *Debrief    `json:"debrief,omitempty"`
	HighScores   []HighScore `json:"highScores"`
	Medals       []MedalView `json:"medals"`
	EnterInitial bool        `json:"enterInitials,omitempty"`
}

type TaskInfo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Kind        string `json:"kind"`
	Difficulty  int    `json:"difficulty"`
	Boss        bool   `json:"boss,omitempty"`
	Unsupported string `json:"unsupported,omitempty"`
}

type Clock struct {
	Played     float64 `json:"played"`
	Shift      float64 `json:"shift"`
	Stage      int     `json:"stage"`
	Stages     int     `json:"stages"`
	StageLeft  float64 `json:"stageLeft"`
	ShiftLeft  float64 `json:"shiftLeft"`
	Practicing bool    `json:"practicing,omitempty"`
}

type TicketView struct {
	*Ticket
	Remaining float64       `json:"remaining"`
	Hints     []string      `json:"hints,omitempty"`
	Checking  bool          `json:"checking"`
	Checks    []CheckResult `json:"checks,omitempty"`
	Monitor   *MonitorInfo  `json:"monitor,omitempty"`
}

type MedalView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Why   string `json:"why"`
	At    string `json:"at"`
	New   bool   `json:"new,omitempty"`
}

type Debrief struct {
	Outcome    string           `json:"outcome"`
	Headline   string           `json:"headline"`
	Mode       string           `json:"mode"`
	Score      int              `json:"score"`
	Rank       string           `json:"rank"`
	Stage      int              `json:"stage"`
	Seed       int64            `json:"seed"`
	BestStreak int              `json:"bestStreak"`
	HintsUsed  int              `json:"hintsUsed"`
	Played     float64          `json:"played"`
	NewMedals  []string         `json:"newMedals"`
	ByCategory []CategoryResult `json:"byCategory"`
	Tickets    []DebriefTicket  `json:"tickets"`
	CleaningUp bool             `json:"cleaningUp"`
}

type CategoryResult struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Cleared int    `json:"cleared"`
	Total   int    `json:"total"`
}

type DebriefTicket struct {
	Title      string        `json:"title"`
	Category   string        `json:"category"`
	Kind       string        `json:"kind"`
	Difficulty int           `json:"difficulty"`
	Boss       bool          `json:"boss,omitempty"`
	Stage      int           `json:"stage"`
	Outcome    string        `json:"outcome"`
	Took       float64       `json:"took"`
	Limit      float64       `json:"limit"`
	HintsUsed  int           `json:"hintsUsed"`
	Earned     int           `json:"earned"`
	Brief      string        `json:"brief"`
	Wrong      []CheckResult `json:"wrong"` // checks that did not pass at the last look
	Solution   string        `json:"solution"`
	Explain    string        `json:"explain"`
	Error      string        `json:"error,omitempty"`
}

// View renders the game for the page. since is the last event the page has seen.
func (g *Game) View(since int) View {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tick()
	v := View{Phase: g.phase, Mode: g.mode, Stack: g.stack, Nodes: g.nodes, Env: g.env,
		BankErrors: g.bankErrors, Diag: g.diag, Lives: g.lives, Score: g.score, Streak: g.streak, Seq: g.seq,
		EnterInitial: g.enterInitial, Tickets: []TicketView{}, Events: []Event{}, HighScores: g.save.HighScores, Medals: []MedalView{}}
	v.Connected = !g.lastSync.IsZero() && g.now().Sub(g.lastSync) <= connectedFor
	if err := g.connectedErr(); err != nil {
		v.Waiting = err.Error()
	}
	if g.bank != nil {
		v.BankTitle, v.Categories, v.MaxLives = g.bank.Title, g.bank.Categories, g.bank.Lives
		for _, t := range g.bank.Tasks {
			v.Tasks = append(v.Tasks, TaskInfo{ID: t.ID, Title: t.Title, Category: t.Category, Kind: t.Kind,
				Difficulty: t.Difficulty, Boss: t.Boss, Unsupported: t.Unsupported})
		}
	}
	if v.HighScores == nil {
		v.HighScores = []HighScore{}
	}
	v.Multiplier = math.Min(3, 1+0.25*float64(max(g.streak, 1)-1))
	if g.mode == ModeArcade {
		v.Pauses = maxPauses - g.pauses
	} else if g.mode == ModePractice {
		v.Pauses = 99
	}
	shift := g.shift()
	stages := 1
	if g.bank != nil && len(g.bank.Stages) > 0 {
		stages = len(g.bank.Stages)
	}
	v.Clock = Clock{Played: g.played.Seconds(), Shift: shift.Seconds(), Stage: g.stage + 1, Stages: stages,
		ShiftLeft: math.Max(0, (shift - g.played).Seconds()), Practicing: g.mode == ModePractice}
	v.Clock.StageLeft = math.Max(0, (time.Duration(g.stage+1)*g.stageLen() - g.played).Seconds())
	for _, e := range g.events {
		if e.Seq > since {
			v.Events = append(v.Events, e)
		}
	}
	if g.phase == PhasePlaying || g.phase == PhasePaused || g.phase == PhaseEnding {
		for _, tk := range g.tickets {
			if tk.State != TicketOpen && tk.State != TicketIncoming && !g.lingering(tk) {
				continue
			}
			tv := TicketView{Ticket: tk, Remaining: math.Max(0, g.remaining(tk)), Checking: tk.checkID != ""}
			if tk.Monitor != nil {
				m := *tk.Monitor
				m.Age = math.Round(g.now().Sub(tk.pageAt).Seconds())
				tv.Monitor = &m
			}
			tv.Hints = append([]string(nil), tk.hints[:min(tk.HintsBought, len(tk.hints))]...)
			// A guard is a hint in disguise until it fails, so it shows only then.
			for _, c := range tk.Checks {
				if !c.Guard || !c.OK {
					tv.Checks = append(tv.Checks, c)
				}
			}
			v.Tickets = append(v.Tickets, tv)
		}
	}
	for id, m := range g.save.Medals {
		mv := MedalView{ID: id, Title: m.Title, Why: m.Why, At: m.At}
		for _, n := range g.newMedals {
			if n == m.Title {
				mv.New = true
			}
		}
		v.Medals = append(v.Medals, mv)
	}
	sort.Slice(v.Medals, func(i, j int) bool { return v.Medals[i].At < v.Medals[j].At })
	if g.phase == PhaseEnding || g.phase == PhaseOver {
		v.Debrief = g.debrief()
	}
	return v
}

func (g *Game) debrief() *Debrief {
	d := &Debrief{Outcome: g.outcome, Headline: outcomeText(g.outcome), Mode: g.mode, Score: g.score, Rank: g.rank,
		Stage: g.stage + 1, Seed: g.seed, BestStreak: g.bestStreak, HintsUsed: g.hintsUsed, Played: g.played.Seconds(),
		NewMedals: g.newMedals, CleaningUp: g.phase == PhaseEnding}
	if d.NewMedals == nil {
		d.NewMedals = []string{}
	}
	cats := map[string]*CategoryResult{}
	if g.bank != nil {
		for _, c := range g.bank.Categories {
			cats[c.ID] = &CategoryResult{ID: c.ID, Title: c.Title}
		}
	}
	for _, tk := range g.tickets {
		if tk.State == TicketSkipped {
			d.Tickets = append(d.Tickets, DebriefTicket{Title: tk.Title, Category: tk.Category, Kind: tk.Kind,
				Difficulty: tk.Difficulty, Stage: tk.Stage, Outcome: tk.State, Error: tk.Error, Wrong: []CheckResult{}})
			continue
		}
		dt := DebriefTicket{Title: tk.Title, Category: tk.Category, Kind: tk.Kind, Difficulty: tk.Difficulty, Boss: tk.Boss,
			Stage: tk.Stage, Outcome: tk.State, Took: tk.TookSecs, Limit: tk.Limit, HintsUsed: tk.HintsBought,
			Earned: tk.Earned, Brief: tk.Brief, Solution: tk.solution, Explain: tk.explain, Wrong: []CheckResult{}}
		if tk.State == TicketIncoming {
			dt.Outcome = "not reached"
		}
		if tk.State != TicketCleared {
			for _, c := range tk.Checks {
				if !c.OK {
					dt.Wrong = append(dt.Wrong, c)
				}
			}
		}
		if c := cats[tk.Category]; c != nil && tk.State != TicketIncoming {
			c.Total++
			if tk.State == TicketCleared {
				c.Cleared++
			}
		}
		d.Tickets = append(d.Tickets, dt)
	}
	if g.bank != nil {
		for _, c := range g.bank.Categories {
			if r := cats[c.ID]; r.Total > 0 {
				d.ByCategory = append(d.ByCategory, *r)
			}
		}
	}
	if d.Tickets == nil {
		d.Tickets = []DebriefTicket{}
	}
	if d.ByCategory == nil {
		d.ByCategory = []CategoryResult{}
	}
	return d
}
