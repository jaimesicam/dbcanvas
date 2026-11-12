package game

import (
	"strings"
	"testing"
	"time"
)

// fake plays DBCanvas: it answers every action the game asks for.
type fake struct {
	t      *testing.T
	g      *Game
	clock  time.Time
	solved map[string]bool // instance → the cluster is fixed
	setups int
	cleans map[string]bool
	fail   map[string]bool // task → setup fails
	locks  map[string][]string
}

func newFake(t *testing.T, bank Bank) *fake {
	f := &fake{t: t, clock: time.Unix(1_800_000_000, 0), solved: map[string]bool{}, cleans: map[string]bool{}, fail: map[string]bool{}}
	f.g = New(func() time.Time { return f.clock })
	f.g.Sync(SyncRequest{BankHash: "h", Bank: &bank, Saved: []byte(`{}`),
		Env: Env{ControlPlane: "cp", Workers: []string{"w1", "w2"}}})
	return f
}

// step advances the clock and runs one sync, answering what the previous one asked for.
func (f *fake) step(d time.Duration) SyncResponse {
	f.clock = f.clock.Add(d)
	resp := f.g.Sync(SyncRequest{BankHash: "h", Env: Env{ControlPlane: "cp", Workers: []string{"w1", "w2"}}})
	var results []Result
	for _, a := range resp.Actions {
		switch a.Op {
		case "diagnose":
			results = append(results, Result{ID: a.ID, Op: a.Op, Done: true, OK: true})
		case "setup":
			f.setups++
			if f.fail[a.Task] {
				results = append(results, Result{ID: a.ID, Op: a.Op, Done: true, Error: "boom"})
				continue
			}
			results = append(results, Result{ID: a.ID, Op: a.Op, Done: true, OK: true, Instance: a.ID,
				View: &TaskView{Title: "T " + a.Task, Brief: "fix " + a.Task, Hints: []string{"h1"}, Solution: "sol", Explain: "why"}})
		case "check":
			results = append(results, Result{ID: a.ID, Op: a.Op, Done: true, OK: true, Instance: a.Instance,
				Solved: f.solved[a.Instance], Checks: []CheckResult{{Name: "c", OK: f.solved[a.Instance]}}})
		case "cleanup":
			f.cleans[a.Instance] = true
			results = append(results, Result{ID: a.ID, Op: a.Op, Done: true, OK: true, Instance: a.Instance})
		}
	}
	f.g.Sync(SyncRequest{BankHash: "h", Env: Env{ControlPlane: "cp", Workers: []string{"w1", "w2"}}, Results: results})
	return resp
}

func (f *fake) open() []TicketView {
	var out []TicketView
	for _, tk := range f.g.View(0).Tickets {
		if tk.State == TicketOpen {
			out = append(out, tk)
		}
	}
	return out
}

func (f *fake) start(mode, task string) {
	f.t.Helper()
	if err := f.g.CheckSystem(); err != nil {
		f.t.Fatal(err)
	}
	f.step(time.Second)
	if p := f.g.View(0).Phase; p != PhaseReady {
		f.t.Fatalf("after the systems check the phase is %s", p)
	}
	if err := f.g.Begin(mode, task, 7); err != nil {
		f.t.Fatal(err)
	}
}

func testBank() Bank {
	return Bank{Bank: "b", Title: "B", Shift: 600, Lives: 3,
		Categories: []Category{{ID: "net", Title: "Net"}, {ID: "work", Title: "Work"}},
		Stages: []Stage{
			{Open: 1, SpawnEvery: 30, TimeScale: 1, Difficulty: []int{1}},
			{Open: 2, SpawnEvery: 30, TimeScale: 1, Difficulty: []int{1, 2}},
		},
		Ranks: []Rank{{Min: 0, Title: "Intern"}, {Min: 100, Title: "Pro"}},
		Tasks: []PublicTask{
			{ID: "a", Title: "A", Category: "net", Difficulty: 1, Points: 100, TimeLimit: 60, Hints: []int{25}},
			{ID: "b", Title: "B", Category: "work", Difficulty: 1, Points: 100, TimeLimit: 60, Locks: []string{"x"}},
			{ID: "c", Title: "C", Category: "work", Difficulty: 2, Points: 200, TimeLimit: 60, Locks: []string{"x"}},
			{ID: "boss", Title: "Boss", Category: "net", Difficulty: 3, Points: 300, TimeLimit: 120, Boss: true},
			{ID: "nope", Title: "Nope", Category: "net", Difficulty: 1, Points: 100, TimeLimit: 60, Unsupported: "needs calico"},
		}}
}

func TestClearingATicketScoresAndCleansUp(t *testing.T) {
	f := newFake(t, testBank())
	f.start(ModeArcade, "")
	f.step(5 * time.Second) // first spawn at 4s: the setup goes out
	f.step(time.Second)     // the setup's result comes back
	open := f.open()
	if len(open) != 1 {
		t.Fatalf("%d tickets open after the first spawn, want 1", len(open))
	}
	tk := open[0]
	if tk.TaskID == "nope" || tk.TaskID == "boss" {
		t.Fatalf("drew %s, which stage 1 must not draw", tk.TaskID)
	}
	if strings.Contains(tk.Brief, "sol") {
		t.Fatal("the solution reached the page before the debrief")
	}
	f.solved[tk.setupID] = true
	f.step(11 * time.Second) // the periodic check goes out
	f.step(time.Second)      // and comes back solved
	v := f.g.View(0)
	if v.Score <= 0 {
		t.Fatalf("score %d after a clear", v.Score)
	}
	f.step(time.Second)
	if !f.cleans[tk.setupID] {
		t.Error("a cleared ticket was not cleaned up")
	}
	var cleared bool
	for _, e := range v.Events {
		cleared = cleared || e.Kind == "clear"
	}
	if !cleared {
		t.Error("no clear event for the page to animate")
	}
}

func TestThreeTimeoutsIsGameOverWithADebrief(t *testing.T) {
	f := newFake(t, testBank())
	f.start(ModeArcade, "")
	for i := 0; i < 200 && f.g.View(0).Phase == PhasePlaying; i++ {
		f.step(5 * time.Second)
	}
	v := f.g.View(0)
	if v.Lives != 0 {
		t.Fatalf("lives %d after the tickets ran out", v.Lives)
	}
	for i := 0; i < 5 && v.Phase == PhaseEnding; i++ {
		f.step(time.Second)
		v = f.g.View(0)
	}
	if v.Phase != PhaseOver {
		t.Fatalf("phase %s, want the debrief", v.Phase)
	}
	if v.Debrief == nil || v.Debrief.Outcome != "gameover" || len(v.Debrief.Tickets) < 3 {
		t.Fatalf("debrief %+v", v.Debrief)
	}
	for _, dt := range v.Debrief.Tickets {
		if dt.Outcome == TicketExpired && dt.Solution == "" {
			t.Error("an escalated ticket's debrief has no solution")
		}
	}
	for id := range f.g.actions {
		t.Errorf("work left over after the debrief: %s", id)
	}
}

func TestGivingUpCostsNoLife(t *testing.T) {
	f := newFake(t, testBank())
	f.start(ModeArcade, "")
	f.step(5 * time.Second)
	f.step(time.Second)
	tk := f.open()[0]
	if err := f.g.GiveUp(tk.ID); err != nil {
		t.Fatal(err)
	}
	if v := f.g.View(0); v.Lives != 3 {
		t.Errorf("lives %d after giving up, want 3", v.Lives)
	}
}

func TestLockedTasksNeverOverlap(t *testing.T) {
	b := testBank()
	b.Stages = []Stage{{Open: 3, SpawnEvery: 1, TimeScale: 1, Difficulty: []int{1, 2}}}
	f := newFake(t, b)
	f.start(ModeArcade, "")
	for i := 0; i < 40; i++ {
		f.step(2 * time.Second)
		holders := 0
		for _, tk := range f.g.tickets {
			if (tk.State == TicketOpen || tk.State == TicketIncoming || !tk.cleaned) && (tk.TaskID == "b" || tk.TaskID == "c") {
				holders++
			}
		}
		if holders > 1 {
			t.Fatal("two tasks holding lock x were in play at once")
		}
	}
}

func TestSurvivingTheShiftIsVictory(t *testing.T) {
	f := newFake(t, testBank())
	f.start(ModeArcade, "")
	for i := 0; i < 400 && f.g.View(0).Phase == PhasePlaying; i++ {
		f.step(2 * time.Second)
		for _, tk := range f.g.tickets {
			f.solved[tk.setupID] = true
		}
	}
	for i := 0; i < 10 && f.g.View(0).Phase == PhaseEnding; i++ {
		f.step(time.Second)
	}
	v := f.g.View(0)
	if v.Debrief == nil || v.Debrief.Outcome != "victory" {
		t.Fatalf("outcome %+v", v.Debrief)
	}
	var bossSeen, flawless bool
	for _, dt := range v.Debrief.Tickets {
		bossSeen = bossSeen || dt.Boss
	}
	for _, m := range v.Medals {
		flawless = flawless || m.ID == "flawless"
	}
	if !bossSeen {
		t.Error("the last stage brought no boss ticket")
	}
	if !flawless {
		t.Error("a shift with no lives lost earned no Flawless medal")
	}
	if !v.EnterInitial {
		t.Error("a first score should make the high-score table")
	}
	if err := f.g.Initials("ace!"); err != nil {
		t.Fatal(err)
	}
	if hs := f.g.View(0).HighScores; len(hs) != 1 || hs[0].Initials != "ACE" {
		t.Errorf("high scores %+v", hs)
	}
}

func TestPracticeIsUntimedAndOneTask(t *testing.T) {
	f := newFake(t, testBank())
	f.start(ModePractice, "a")
	f.step(time.Second)
	f.step(30 * time.Minute)
	open := f.open()
	if len(open) != 1 || open[0].TaskID != "a" {
		t.Fatalf("practice tickets %+v", open)
	}
	if _, err := f.g.Hint(open[0].ID); err != nil {
		t.Fatal(err)
	}
	if v := f.g.View(0); len(v.Tickets[0].Hints) != 1 || v.Score != 0 {
		t.Errorf("hint not shown, or practice changed the score: %+v", v.Tickets[0])
	}
	f.solved[open[0].setupID] = true
	f.step(11 * time.Second)
	f.step(time.Second)
	f.step(time.Second)
	if p := f.g.View(0).Phase; p != PhaseOver {
		t.Errorf("phase %s after the practice ticket cleared", p)
	}
	if err := f.g.Begin(ModePractice, "nope", 1); err == nil {
		t.Error("an unsupported task could be practiced")
	}
}

func TestAFailedSetupIsSkippedNotCounted(t *testing.T) {
	f := newFake(t, testBank())
	f.fail["a"], f.fail["b"] = true, true
	f.start(ModeArcade, "")
	f.step(5 * time.Second)
	f.step(time.Second)
	v := f.g.View(0)
	if v.Lives != 3 {
		t.Errorf("a failed setup cost a life")
	}
	if !f.cleanAllSkipped() {
		t.Error("a skipped ticket stays active, so DBCanvas would keep it")
	}
}

func (f *fake) cleanAllSkipped() bool {
	resp := f.g.Sync(SyncRequest{BankHash: "h"})
	for _, tk := range f.g.tickets {
		if tk.State == TicketSkipped {
			for _, id := range resp.Active {
				if id == tk.setupID {
					return false
				}
			}
		}
	}
	return true
}

func TestWaitingForDBCanvas(t *testing.T) {
	g := New(nil)
	if err := g.CheckSystem(); err == nil {
		t.Error("a systems check started before DBCanvas connected")
	}
	if v := g.View(0); v.Connected || v.Waiting == "" {
		t.Errorf("view %+v", v)
	}
}

func TestAnEmptyQueueRefillsWithinThirtySecondsWithAHeadStart(t *testing.T) {
	b := testBank()
	b.Stages[0].SpawnEvery = 600 // the stage's own gap would leave the queue empty for minutes
	f := newFake(t, b)
	f.start(ModeArcade, "")
	f.step(5 * time.Second)
	f.step(time.Second)
	tk := f.open()[0]
	f.solved[tk.setupID] = true
	f.step(11 * time.Second) // cleared after about 12s of a 60s limit: a fast clear
	f.step(time.Second)
	if len(f.open()) != 0 {
		t.Fatal("the ticket did not clear")
	}
	var next *TicketView
	for i := 0; i < 31 && next == nil; i++ {
		f.step(time.Second)
		for _, o := range f.open() {
			if o.ID != tk.ID {
				next = &o
			}
		}
	}
	if next == nil {
		t.Fatal("no new ticket within 30 seconds of the queue emptying")
	}
	if next.Extra <= 0 || next.Limit != 60+next.Extra {
		t.Errorf("after a fast clear the next ticket has limit %v, extra %v; want a head start on 60s", next.Limit, next.Extra)
	}
	if next.Extra > 30 {
		t.Errorf("head start %v is more than half the ticket's limit", next.Extra)
	}
}

func TestASlowClearGivesNoHeadStart(t *testing.T) {
	b := testBank()
	b.Stages[0].SpawnEvery = 600
	f := newFake(t, b)
	f.start(ModeArcade, "")
	f.step(5 * time.Second)
	f.step(time.Second)
	tk := f.open()[0]
	f.step(40 * time.Second) // well past half of the 60s limit
	f.solved[tk.setupID] = true
	f.step(11 * time.Second)
	f.step(time.Second)
	for i := 0; i < 32; i++ {
		f.step(time.Second)
	}
	open := f.open()
	if len(open) != 1 || open[0].ID == tk.ID {
		t.Fatalf("want one new ticket within 30 seconds, have %d", len(open))
	}
	if open[0].Extra != 0 || open[0].Limit != 60 {
		t.Errorf("a slow clear gave a head start: limit %v, extra %v", open[0].Limit, open[0].Extra)
	}
}

// DBCanvas skips an action ID it finished recently, so a restarted game must not reuse
// the last run's IDs.
func TestARestartedGameUsesNewActionIDs(t *testing.T) {
	ids := map[string]bool{}
	for i := 0; i < 5; i++ {
		f := newFake(t, testBank())
		if err := f.g.CheckSystem(); err != nil {
			t.Fatal(err)
		}
		for id := range f.g.actions {
			if ids[id] {
				t.Fatalf("a new game reused action ID %s", id)
			}
			ids[id] = true
		}
	}
}
