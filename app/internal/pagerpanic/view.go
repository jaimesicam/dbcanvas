package pagerpanic

import (
	"fmt"
	"strings"
	"time"
)

// Prelude is prepended to every script DBCanvas runs for a task. PP_INSTANCE names the
// instance, which keeps one ticket's backups apart from another's.
//
//	pp_backup FILE      keep a copy of FILE before changing it
//	pp_restore          put back every file this instance backed up, newest first
//	pp_wait SECS CMD…   run CMD until it succeeds, for at most SECS seconds
const Prelude = `export KUBECONFIG="${KUBECONFIG:-/etc/kubernetes/admin.conf}"
PP_DIR="/var/lib/pagerpanic/${PP_INSTANCE:-none}"
pp_backup() {
  local f="$1" b
  mkdir -p "$PP_DIR"
  b="$PP_DIR/$(echo "$f" | sed 's#/#__#g')"
  if [ -e "$f" ] && [ ! -e "$b" ]; then cp -a "$f" "$b" && echo "$f" >> "$PP_DIR/files"; fi
}
pp_restore() {
  [ -f "$PP_DIR/files" ] || return 0
  tac "$PP_DIR/files" | while read -r f; do
    cp -a "$PP_DIR/$(echo "$f" | sed 's#/#__#g')" "$f"
  done
  rm -rf "$PP_DIR"
}
pp_wait() {
  local secs="$1"; shift
  local end=$(( $(date +%s) + secs ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$end" ] && return 1
    sleep 2
  done
}
`

// Script is a step's script as DBCanvas runs it: the prelude, then the rendered body.
func Script(body string, setE bool) string {
	head := ""
	if setE {
		head = "set -eo pipefail\n"
	}
	return Prelude + head + body
}

// PublicTask is what the game may know about a task before it is drawn.
type PublicTask struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind"`
	Category   string   `json:"category"`
	Difficulty int      `json:"difficulty"`
	Points     int      `json:"points"`
	TimeLimit  float64  `json:"timeLimit"` // seconds
	Boss       bool     `json:"boss,omitempty"`
	Locks      []string `json:"locks,omitempty"`
	Blocks     []string `json:"blocks,omitempty"`
	Needs      []string `json:"needs,omitempty"`
	Hints      []int    `json:"hints,omitempty"` // the cost of each hint
	// Unsupported says why the linked cluster cannot run it; "" when it can.
	Unsupported string `json:"unsupported,omitempty"`
}

type PublicStage struct {
	Open       int     `json:"open"`
	SpawnEvery float64 `json:"spawnEvery"` // seconds
	TimeScale  float64 `json:"timeScale"`
	Difficulty []int   `json:"difficulty"`
}

// PublicBank is a bank without its scripts, solutions or hint texts.
type PublicBank struct {
	Bank       string        `json:"bank"`
	Title      string        `json:"title"`
	Shift      float64       `json:"shift"` // seconds
	Lives      int           `json:"lives"`
	Categories []Category    `json:"categories"`
	Stages     []PublicStage `json:"stages"`
	Ranks      []Rank        `json:"ranks"`
	Tasks      []PublicTask  `json:"tasks"`
}

// Public is the bank as the game sees it, with each task marked for whether env can run it.
func (b *Bank) Public(env Env) PublicBank {
	pb := PublicBank{Bank: b.Bank, Title: b.Title, Shift: b.Shift.Seconds(), Lives: b.Lives,
		Categories: b.Categories, Ranks: b.Ranks}
	for _, s := range b.Stages {
		pb.Stages = append(pb.Stages, PublicStage{Open: s.Open, SpawnEvery: s.SpawnEvery.Seconds(),
			TimeScale: s.TimeScale, Difficulty: s.Difficulty})
	}
	for _, t := range b.Tasks {
		pt := PublicTask{ID: t.ID, Title: t.Title, Kind: t.Kind, Category: t.Category,
			Difficulty: t.Difficulty, Points: t.Points, TimeLimit: t.TimeLimit.Seconds(), Boss: t.Boss,
			Locks: t.Locks, Blocks: t.Blocks, Needs: t.Needs}
		for _, h := range t.Hints {
			pt.Hints = append(pt.Hints, h.Cost)
		}
		if env.ControlPlane != "" {
			if ok, why := t.Supports(env); !ok {
				pt.Unsupported = why
			}
		}
		pb.Tasks = append(pb.Tasks, pt)
	}
	return pb
}

// TaskView is a drawn task, rendered with its vars. Hints, Solution and Explain are
// handed to the game, which keeps them on its server and shows a hint when it is
// bought and the solution in the debrief.
type TaskView struct {
	Title    string   `json:"title"`
	Brief    string   `json:"brief"`
	Hints    []string `json:"hints"`
	Solution string   `json:"solution"`
	Explain  string   `json:"explain"`
}

// View renders what the player reads about an instance.
func (t Task) View(inst Instance) (TaskView, error) {
	var v TaskView
	var err error
	r := func(s string) string {
		out, e := inst.Render(s)
		if e != nil && err == nil {
			err = e
		}
		return strings.TrimSpace(out)
	}
	v.Title, v.Brief, v.Solution, v.Explain = r(t.Title), r(t.Brief), r(t.Solution), r(t.Explain)
	for _, h := range t.Hints {
		v.Hints = append(v.Hints, r(h.Text))
	}
	return v, err
}

// CheckResult is one check's outcome, as the debrief shows it.
type CheckResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Guard bool   `json:"guard,omitempty"`
	Got   string `json:"got"`
	Want  string `json:"want"`
}

// Summarize trims a command's output to what fits on a ticket.
func Summarize(out string) string {
	out = strings.TrimSpace(out)
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		lines := strings.Count(out, "\n") + 1
		out = out[:i] + fmt.Sprintf(" … (%d lines)", lines)
	}
	if len(out) > 160 {
		out = out[:157] + "…"
	}
	if out == "" {
		return "(no output)"
	}
	return out
}

// DefaultStepTimeout and DefaultCheckTimeout apply when a step or check sets none.
const (
	DefaultStepTimeout  = 3 * time.Minute
	DefaultCheckTimeout = 30 * time.Second
)
