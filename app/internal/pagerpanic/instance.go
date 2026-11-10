package pagerpanic

import (
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Env is what DBCanvas knows about the cluster a game plays against.
type Env struct {
	ControlPlane string   `json:"controlPlane"` // host name of the control plane
	Workers      []string `json:"workers"`      // host names of the workers, in order
	CNI          string   `json:"cni"`          // "calico" or "flannel"
	Version      string   `json:"version"`      // vX.Y.Z
}

// Instance is one drawing of a task: its vars chosen, ready to run.
type Instance struct {
	ID     string            `json:"id"`
	TaskID string            `json:"task"`
	Seed   int64             `json:"seed"`
	Worker string            `json:"worker"`
	Vars   map[string]string `json:"vars"`
}

var tmplRe = regexp.MustCompile(`\{\{\s*([a-z_][a-z0-9_]*)((?:\s+[^\s}]+)*)\s*\}\}`)

// Instantiate chooses a task's random values. The same seed gives the same values, which
// is what lets a game be replayed.
func (t Task) Instantiate(seed int64, env Env) (Instance, error) {
	rng := rand.New(rand.NewSource(seed))
	inst := Instance{TaskID: t.ID, Seed: seed, Vars: map[string]string{}}
	inst.Vars["control_plane"] = env.ControlPlane
	if len(env.Workers) > 0 {
		inst.Worker = env.Workers[rng.Intn(len(env.Workers))]
		inst.Vars["worker"] = inst.Worker
		for i, w := range env.Workers {
			inst.Vars[fmt.Sprintf("worker_%d", i+1)] = w
		}
		// other_worker is a worker that is not {{worker}}, for tasks that move pods.
		inst.Vars["other_worker"] = inst.Worker
		for _, w := range env.Workers {
			if w != inst.Worker {
				inst.Vars["other_worker"] = w
				break
			}
		}
	}
	// Vars may use one another, in any order; resolve them in rounds, sorted so the
	// random draws happen in the same order every time.
	names := make([]string, 0, len(t.Vars))
	for k := range t.Vars {
		names = append(names, k)
	}
	sort.Strings(names)
	pending := map[string]string{}
	for _, k := range names {
		pending[k] = t.Vars[k]
	}
	for len(pending) > 0 {
		progress := false
		for _, k := range names {
			v, ok := pending[k]
			if !ok || !refsResolved(v, inst.Vars) {
				continue
			}
			out, err := expand(v, inst.Vars, rng)
			if err != nil {
				return inst, fmt.Errorf("var %s: %v", k, err)
			}
			inst.Vars[k] = out
			delete(pending, k)
			progress = true
		}
		if !progress {
			var left []string
			for k := range pending {
				left = append(left, k)
			}
			sort.Strings(left)
			return inst, fmt.Errorf("vars %s refer to each other or to a var that does not exist", strings.Join(left, ", "))
		}
	}
	inst.ID = fmt.Sprintf("%s-%x", t.ID, uint32(seed))
	return inst, nil
}

func refsResolved(v string, vars map[string]string) bool {
	for _, m := range tmplRe.FindAllStringSubmatch(v, -1) {
		if isGenerator(m[1]) {
			continue
		}
		if _, ok := vars[m[1]]; !ok {
			return false
		}
	}
	return true
}

func isGenerator(name string) bool { return name == "rand" || name == "pick" || name == "int" }

const randAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

func expand(s string, vars map[string]string, rng *rand.Rand) (string, error) {
	var err error
	out := tmplRe.ReplaceAllStringFunc(s, func(m string) string {
		p := tmplRe.FindStringSubmatch(m)
		name, args := p[1], strings.Fields(p[2])
		switch name {
		case "rand":
			n := 4
			if len(args) > 0 {
				if v, e := strconv.Atoi(args[0]); e == nil && v > 0 && v <= 32 {
					n = v
				}
			}
			b := make([]byte, n)
			for i := range b {
				b[i] = randAlphabet[rng.Intn(len(randAlphabet))]
			}
			return string(b)
		case "pick":
			if len(args) == 0 {
				err = fmt.Errorf("{{pick}} needs at least one choice")
				return m
			}
			return args[rng.Intn(len(args))]
		case "int":
			if len(args) != 2 {
				err = fmt.Errorf("{{int lo hi}} needs two numbers")
				return m
			}
			lo, e1 := strconv.Atoi(args[0])
			hi, e2 := strconv.Atoi(args[1])
			if e1 != nil || e2 != nil || hi < lo {
				err = fmt.Errorf("{{int %s %s}}: two numbers, low first", args[0], args[1])
				return m
			}
			return strconv.Itoa(lo + rng.Intn(hi-lo+1))
		}
		v, ok := vars[name]
		if !ok {
			err = fmt.Errorf("{{%s}} is not a var of this task", name)
			return m
		}
		return v
	})
	return out, err
}

// render fills a string from the vars. Generators are not allowed here: a value that
// changed every time it was rendered would make a check disagree with its setup.
func render(s string, vars map[string]string) (string, error) {
	var err error
	out := tmplRe.ReplaceAllStringFunc(s, func(m string) string {
		p := tmplRe.FindStringSubmatch(m)
		if isGenerator(p[1]) {
			err = fmt.Errorf("{{%s}} can only be used in vars", p[1])
			return m
		}
		v, ok := vars[p[1]]
		if !ok {
			err = fmt.Errorf("{{%s}} is not a var of this task", p[1])
			return m
		}
		return v
	})
	return out, err
}

// Render fills a string with the instance's vars.
func (inst Instance) Render(s string) (string, error) { return render(s, inst.Vars) }

// Hosts resolves a step's or check's "on" to host names.
func (inst Instance) Hosts(on string, env Env) ([]string, error) {
	switch on {
	case "", "control-plane":
		return []string{env.ControlPlane}, nil
	case "worker":
		if inst.Worker == "" {
			return nil, fmt.Errorf("the cluster has no worker")
		}
		return []string{inst.Worker}, nil
	case "workers":
		return env.Workers, nil
	case "all-nodes":
		return append([]string{env.ControlPlane}, env.Workers...), nil
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(on, "worker-"))
	if n < 1 || n > len(env.Workers) {
		return nil, fmt.Errorf("%s: the cluster has %d worker(s)", on, len(env.Workers))
	}
	return []string{env.Workers[n-1]}, nil
}

// StepTimeout is a step's own timeout, or the default.
func StepTimeout(d Duration, def time.Duration) time.Duration {
	if d.Duration > 0 {
		return d.Duration
	}
	return def
}

// ---------------------------------------------------------------- requirements

type require struct {
	kind string // "cni", "workers", "k8s"
	cni  string
	n    int // workers: at least n; k8s: at least minor n
}

func parseRequire(r string) (require, error) {
	switch r {
	case "calico", "flannel":
		return require{kind: "cni", cni: r}, nil
	}
	if v, ok := strings.CutPrefix(r, "workers>="); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return require{}, fmt.Errorf("write it as workers>=2")
		}
		return require{kind: "workers", n: n}, nil
	}
	if v, ok := strings.CutPrefix(r, "k8s>=1."); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return require{}, fmt.Errorf("write it as k8s>=1.30")
		}
		return require{kind: "k8s", n: n}, nil
	}
	return require{}, fmt.Errorf("unknown requirement (calico, flannel, workers>=N, k8s>=1.N)")
}

// Supports says whether the cluster can run the task, and if not, why.
func (t Task) Supports(env Env) (bool, string) {
	for _, r := range t.Requires {
		q, err := parseRequire(r)
		if err != nil {
			return false, err.Error()
		}
		switch q.kind {
		case "cni":
			if env.CNI != q.cni {
				return false, "needs the " + q.cni + " pod network"
			}
		case "workers":
			if len(env.Workers) < q.n {
				return false, fmt.Sprintf("needs %d workers", q.n)
			}
		case "k8s":
			minor := 0
			if p := strings.Split(strings.TrimPrefix(env.Version, "v"), "."); len(p) >= 2 {
				minor, _ = strconv.Atoi(p[1])
			}
			if minor < q.n {
				return false, fmt.Sprintf("needs Kubernetes 1.%d or newer", q.n)
			}
		}
	}
	return true, ""
}

// ---------------------------------------------------------------- judging a check

// Judge says whether a check's output meets what it expects, and describes what was
// wanted for the debrief. vars fill the expectation (a recorded UID, say).
func (c Check) Judge(out string, exit int, inst Instance) (ok bool, want string, err error) {
	got := strings.TrimSpace(out)
	e := c.Expect
	r := func(p *string) (string, error) { return inst.Render(*p) }
	switch {
	case e.Equals != nil:
		w, err := r(e.Equals)
		return err == nil && got == strings.TrimSpace(w), fmt.Sprintf("%q", w), err
	case e.Contains != nil:
		w, err := r(e.Contains)
		return err == nil && strings.Contains(got, w), fmt.Sprintf("contains %q", w), err
	case e.Regex != nil:
		w, err := r(e.Regex)
		if err != nil {
			return false, "", err
		}
		re, err := regexp.Compile(w)
		if err != nil {
			return false, "", err
		}
		return re.MatchString(got), "matches /" + w + "/", nil
	case e.Exit != nil:
		return exit == *e.Exit, fmt.Sprintf("exit %d", *e.Exit), nil
	case e.NonEmpty:
		return got != "", "any output", nil
	case e.Empty:
		return got == "", "no output", nil
	case e.Min != nil || e.Max != nil:
		v, perr := strconv.ParseFloat(got, 64)
		var parts []string
		if e.Min != nil {
			parts = append(parts, fmt.Sprintf("≥ %g", *e.Min))
		}
		if e.Max != nil {
			parts = append(parts, fmt.Sprintf("≤ %g", *e.Max))
		}
		want = strings.Join(parts, " and ")
		if perr != nil {
			return false, want, nil
		}
		ok = (e.Min == nil || v >= *e.Min) && (e.Max == nil || v <= *e.Max)
		return ok, want, nil
	}
	return exit == 0, "exit 0", nil
}
