// Package pagerpanic is the task engine behind the Pager Panic game: it loads task banks
// (YAML), checks them, turns a task into a playable instance with its own random values,
// and judges a check's output. It runs nothing itself. DBCanvas runs a task's scripts on
// the nodes of the linked cluster and hands the output back here to be judged, and the
// game only ever sees what Public and Instance.View give it.
package pagerpanic

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Bank is one file of tasks for one kind of target.
type Bank struct {
	Bank     string `json:"bank"`
	Title    string `json:"title"`
	Target   string `json:"target"` // the frame type it plays against: "kubeadm"
	MinNodes int    `json:"min_nodes"`
	// Shift is how long a game lasts; every ticket's own time limit is shorter.
	Shift Duration `json:"shift"`
	Lives int      `json:"lives"`
	// Images are pulled onto every node during the systems check, so a ticket's timer
	// never runs out on an image download.
	Images     []string   `json:"images"`
	Categories []Category `json:"categories"`
	Stages     []Stage    `json:"stages"`
	Ranks      []Rank     `json:"ranks"`
	Tasks      []Task     `json:"tasks"`

	// File is where the bank was read from.
	File string `json:"-"`
}

type Category struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Medal is the name of the medal for clearing MedalAt tickets of this category.
	Medal   string `json:"medal"`
	MedalAt int    `json:"medal_at"`
}

// Stage is one part of a shift. Stages split the shift evenly.
type Stage struct {
	// Open is how many tickets may be open at once.
	Open       int      `json:"open"`
	SpawnEvery Duration `json:"spawn_every"`
	// TimeScale multiplies every ticket's time limit (0.8 leaves 80% of it).
	TimeScale float64 `json:"time_scale"`
	// Difficulty lists the task difficulties this stage draws from.
	Difficulty []int `json:"difficulty"`
}

type Rank struct {
	Min   int    `json:"min"`
	Title string `json:"title"`
}

// Task is one ticket a game can draw.
type Task struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind"` // "incident" (something broke) or "request" (build something)
	Category   string   `json:"category"`
	Difficulty int      `json:"difficulty"` // 1–3
	Points     int      `json:"points"`     // 0 means difficulty × 100
	TimeLimit  Duration `json:"time_limit"`
	// Boss marks the ticket that closes the last stage.
	Boss bool `json:"boss"`
	// Requires names what the cluster must have: "calico", "flannel", "workers>=N", "k8s>=1.N".
	Requires []string `json:"requires"`
	// Locks name things the task changes; two tasks with a lock in common never run at once.
	Locks []string `json:"locks"`
	// Blocks names capabilities the task takes away while it is open ("scheduling");
	// Needs names the ones it depends on. A task that needs what an open one blocks waits.
	Blocks []string `json:"blocks"`
	Needs  []string `json:"needs"`
	// Vars are this task's random values. A value can use {{rand N}}, {{pick a b c}},
	// {{int lo hi}} and other vars; {{worker}} and {{control_plane}} are always there.
	Vars     map[string]string `json:"vars"`
	Setup    []Step            `json:"setup"`
	Brief    string            `json:"brief"`
	Hints    []Hint            `json:"hints"`
	Check    []Check           `json:"check"`
	Solution string            `json:"solution"`
	Explain  string            `json:"explain"`
	// Solve is a script that fixes the task. Players never see it; the self-test runs it to
	// prove that the checks pass once the task is done.
	Solve   []Step `json:"solve"`
	Cleanup []Step `json:"cleanup"`
}

// Step is one script on one or more nodes.
type Step struct {
	// On (node: in YAML) is "control-plane", "worker" (the task's chosen worker), "worker-N", "workers"
	// or "all-nodes". Empty is the control plane.
	On      string   `json:"node"`
	Run     string   `json:"run"`
	Timeout Duration `json:"timeout"`
	// Record runs each command on the control plane after the step and keeps its output
	// as a var, for checks that compare against how things were (a UID, a count).
	Record map[string]string `json:"record"`
}

type Hint struct {
	Cost int    `json:"cost"`
	Text string `json:"text"`
}

// Check is one condition a solved task meets.
type Check struct {
	Name    string   `json:"name"`
	On      string   `json:"node"`
	Run     string   `json:"run"`
	Expect  Expect   `json:"expect"`
	Timeout Duration `json:"timeout"`
	// Guard marks a check that catches a shortcut (the Deployment was recreated, the
	// namespace was deleted). It is shown to the player only when it fails.
	Guard bool `json:"guard"`
}

// Expect is what a check's output must be. Output is trimmed before it is compared. With
// nothing set, the command must exit 0.
type Expect struct {
	Equals   *string  `json:"equals,omitempty"`
	Contains *string  `json:"contains,omitempty"`
	Regex    *string  `json:"regex,omitempty"`
	Exit     *int     `json:"exit,omitempty"`
	NonEmpty bool     `json:"nonempty,omitempty"`
	Empty    bool     `json:"empty,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
}

// Duration reads "90s", "6m" or "1h" from YAML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n float64
		if json.Unmarshal(b, &n) == nil {
			d.Duration = time.Duration(n * float64(time.Second))
			return nil
		}
		return fmt.Errorf("a duration is written like 90s, 6m or 1h")
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("%q is not a duration (write it like 90s, 6m or 1h)", s)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

// Parse reads one bank and checks it.
func Parse(data []byte, file string) (*Bank, error) {
	var b Bank
	if err := yaml.UnmarshalStrict(data, &b); err != nil {
		return nil, fmt.Errorf("%s: %v", filepath.Base(file), err)
	}
	b.File = file
	b.defaults()
	if errs := b.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", filepath.Base(file), strings.Join(errs, "; "))
	}
	return &b, nil
}

// LoadDir reads every *.yaml bank in a directory. A bank that does not parse is
// reported, and the others still load.
func LoadDir(dir string) (map[string]*Bank, []error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	more, _ := filepath.Glob(filepath.Join(dir, "*.yml"))
	files = append(files, more...)
	sort.Strings(files)
	out := map[string]*Bank{}
	var errs []error
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		b, err := Parse(data, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if prev, dup := out[b.Bank]; dup {
			errs = append(errs, fmt.Errorf("%s: bank %q is also defined in %s", filepath.Base(f), b.Bank, filepath.Base(prev.File)))
			continue
		}
		out[b.Bank] = b
	}
	return out, errs
}

func (b *Bank) defaults() {
	if b.Lives == 0 {
		b.Lives = 3
	}
	if b.Shift.Duration == 0 {
		b.Shift.Duration = time.Hour
	}
	for i := range b.Stages {
		if b.Stages[i].TimeScale == 0 {
			b.Stages[i].TimeScale = 1
		}
		if b.Stages[i].Open == 0 {
			b.Stages[i].Open = 1
		}
	}
	for i := range b.Tasks {
		t := &b.Tasks[i]
		if t.Points == 0 {
			t.Points = t.Difficulty * 100
		}
	}
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Validate reports every problem in the bank, not only the first.
func (b *Bank) Validate() []string {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if !idRe.MatchString(b.Bank) {
		bad("bank: %q must be lowercase letters, digits and dashes", b.Bank)
	}
	if b.Title == "" {
		bad("title is missing")
	}
	if b.Target != "kubeadm" {
		bad("target: %q is not one Pager Panic can play against (kubeadm)", b.Target)
	}
	if b.MinNodes < 1 {
		bad("min_nodes must be at least 1")
	}
	if b.Lives < 1 {
		bad("lives must be at least 1")
	}
	if len(b.Stages) == 0 {
		bad("stages: a bank needs at least one stage")
	}
	for i, s := range b.Stages {
		if s.SpawnEvery.Duration <= 0 {
			bad("stage %d: spawn_every is missing", i+1)
		}
		if s.TimeScale <= 0 || s.TimeScale > 2 {
			bad("stage %d: time_scale %.2f is out of range (0–2]", i+1, s.TimeScale)
		}
		if len(s.Difficulty) == 0 {
			bad("stage %d: difficulty lists no levels", i+1)
		}
	}
	cats := map[string]bool{}
	for _, c := range b.Categories {
		if !idRe.MatchString(c.ID) {
			bad("category %q: the id must be lowercase letters, digits and dashes", c.ID)
		}
		cats[c.ID] = true
	}
	if len(cats) == 0 {
		bad("categories: a bank needs at least one")
	}
	seen := map[string]bool{}
	for i, t := range b.Tasks {
		where := fmt.Sprintf("task %d (%s)", i+1, t.ID)
		if !idRe.MatchString(t.ID) {
			bad("%s: id must be lowercase letters, digits and dashes", where)
		}
		if seen[t.ID] {
			bad("%s: the id is used twice", where)
		}
		seen[t.ID] = true
		if t.Title == "" || t.Brief == "" {
			bad("%s: title and brief are required", where)
		}
		if t.Kind != "incident" && t.Kind != "request" {
			bad("%s: kind is %q; it is incident or request", where, t.Kind)
		}
		if !cats[t.Category] {
			bad("%s: category %q is not in the bank's categories", where, t.Category)
		}
		if t.Difficulty < 1 || t.Difficulty > 3 {
			bad("%s: difficulty is 1, 2 or 3", where)
		}
		if t.TimeLimit.Duration < 30*time.Second || t.TimeLimit.Duration >= b.Shift.Duration {
			bad("%s: time_limit %s must be at least 30s and shorter than the shift (%s)", where, t.TimeLimit.Duration, b.Shift.Duration)
		}
		if len(t.Check) == 0 {
			bad("%s: a task needs at least one check", where)
		}
		if t.Solution == "" {
			bad("%s: solution is missing (it is what the debrief shows)", where)
		}
		if len(t.Cleanup) == 0 {
			bad("%s: cleanup is missing — a task must put the cluster back", where)
		}
		for _, r := range t.Requires {
			if _, err := parseRequire(r); err != nil {
				bad("%s: requires %q: %v", where, r, err)
			}
		}
		for j, c := range t.Check {
			if c.Name == "" || c.Run == "" {
				bad("%s: check %d needs a name and a run", where, j+1)
			}
			if c.Expect.Regex != nil {
				if _, err := regexp.Compile(*c.Expect.Regex); err != nil {
					bad("%s: check %q: regex: %v", where, c.Name, err)
				}
			}
		}
		steps := map[string][]Step{"setup": t.Setup, "solve": t.Solve, "cleanup": t.Cleanup}
		for name, ss := range steps {
			for j, s := range ss {
				if s.Run == "" {
					bad("%s: %s step %d has no run", where, name, j+1)
				}
				if !validOn(s.On) {
					bad("%s: %s step %d: node %q is not control-plane, worker, worker-N, workers or all-nodes", where, name, j+1, s.On)
				}
				if name != "setup" && len(s.Record) > 0 {
					bad("%s: record is only allowed in setup", where)
				}
			}
		}
		for _, c := range t.Check {
			if !validOn(c.On) {
				bad("%s: check %q: node %q is not control-plane, worker, worker-N, workers or all-nodes", where, c.Name, c.On)
			}
		}
		// Every template must resolve: a typo in a var name is found here, not mid-game.
		inst, err := t.Instantiate(1, Env{ControlPlane: "cp", Workers: []string{"w1", "w2"}})
		if err != nil {
			bad("%s: %v", where, err)
			continue
		}
		for k := range allRecords(t) {
			inst.Vars[k] = "recorded"
		}
		for _, s := range t.textFields() {
			if _, err := render(s, inst.Vars); err != nil {
				bad("%s: %v", where, err)
			}
		}
	}
	if len(b.Tasks) == 0 {
		bad("tasks: a bank needs at least one task")
	}
	return errs
}

func validOn(on string) bool {
	switch on {
	case "", "control-plane", "worker", "workers", "all-nodes":
		return true
	}
	if n, ok := strings.CutPrefix(on, "worker-"); ok {
		i, err := strconv.Atoi(n)
		return err == nil && i >= 1
	}
	return false
}

func allRecords(t Task) map[string]bool {
	out := map[string]bool{}
	for _, s := range t.Setup {
		for k := range s.Record {
			out[k] = true
		}
	}
	return out
}

// textFields are every string a task renders with its vars.
func (t Task) textFields() []string {
	out := []string{t.Title, t.Brief, t.Solution, t.Explain}
	for _, h := range t.Hints {
		out = append(out, h.Text)
	}
	for _, c := range t.Check {
		out = append(out, c.Name, c.Run)
		for _, p := range []*string{c.Expect.Equals, c.Expect.Contains, c.Expect.Regex} {
			if p != nil {
				out = append(out, *p)
			}
		}
	}
	for _, ss := range [][]Step{t.Setup, t.Solve, t.Cleanup} {
		for _, s := range ss {
			out = append(out, s.Run)
			for _, r := range s.Record {
				out = append(out, r)
			}
		}
	}
	return out
}

// Task finds a task by id.
func (b *Bank) Task(id string) (Task, bool) {
	for _, t := range b.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// RankFor is the title a score earns.
func (b *Bank) RankFor(score int) string {
	best := ""
	bestMin := -1
	for _, r := range b.Ranks {
		if score >= r.Min && r.Min > bestMin {
			best, bestMin = r.Title, r.Min
		}
	}
	return best
}
