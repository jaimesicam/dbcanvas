package pagerpanic

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The banks in the repository must load cleanly: a bank error found here is one a player
// never meets mid-game.
func TestShippedBanksLoad(t *testing.T) {
	banks, errs := LoadDir("../../../pagerpanic/banks")
	for _, err := range errs {
		t.Error(err)
	}
	b, ok := banks["kubernetes"]
	if !ok {
		t.Fatal("no kubernetes bank")
	}
	if b.MinNodes < 3 {
		t.Errorf("the Kubernetes bank needs at least 3 nodes, says %d", b.MinNodes)
	}
	bosses := 0
	diff := map[int]int{}
	for _, task := range b.Tasks {
		if task.Boss {
			bosses++
		}
		diff[task.Difficulty]++
		if len(task.Solve) == 0 {
			t.Errorf("%s has no solve script, so the self-test cannot prove it", task.ID)
		}
	}
	if bosses == 0 {
		t.Error("the last stage needs a boss ticket")
	}
	for _, st := range b.Stages {
		for _, d := range st.Difficulty {
			if diff[d] == 0 {
				t.Errorf("a stage draws difficulty %d and no task has it", d)
			}
		}
	}
}

func TestInstantiateIsRepeatable(t *testing.T) {
	task := Task{ID: "x", Vars: map[string]string{
		"ns":   "arcade-{{rand 6}}",
		"port": "{{int 30000 30010}}",
		"full": "{{ns}}:{{port}}",
		"pick": "{{pick a b c}}",
	}}
	env := Env{ControlPlane: "cp", Workers: []string{"w1", "w2"}}
	a, err := task.Instantiate(42, env)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := task.Instantiate(42, env)
	for k, v := range a.Vars {
		if b.Vars[k] != v {
			t.Errorf("%s differs between two draws with one seed: %q, %q", k, v, b.Vars[k])
		}
	}
	if a.Vars["full"] != a.Vars["ns"]+":"+a.Vars["port"] {
		t.Errorf("full = %q; vars that use vars are not resolved", a.Vars["full"])
	}
	if !strings.HasPrefix(a.Vars["ns"], "arcade-") || len(a.Vars["ns"]) != len("arcade-")+6 {
		t.Errorf("ns = %q", a.Vars["ns"])
	}
	if a.Worker != "w1" && a.Worker != "w2" {
		t.Errorf("worker = %q", a.Worker)
	}
	if a.Vars["other_worker"] == a.Worker {
		t.Errorf("other_worker is the same as worker")
	}
}

func TestInstantiateRefusesCycles(t *testing.T) {
	task := Task{ID: "x", Vars: map[string]string{"a": "{{b}}", "b": "{{a}}"}}
	if _, err := task.Instantiate(1, Env{ControlPlane: "cp"}); err == nil {
		t.Error("vars that refer to each other were accepted")
	}
	task = Task{ID: "x", Vars: map[string]string{"a": "{{nope}}"}}
	if _, err := task.Instantiate(1, Env{ControlPlane: "cp"}); err == nil {
		t.Error("a var naming a var that does not exist was accepted")
	}
}

func TestRenderRefusesGeneratorsOutsideVars(t *testing.T) {
	if _, err := render("x {{rand 4}}", map[string]string{}); err == nil {
		t.Error("{{rand}} was rendered outside vars, so a check would disagree with its setup")
	}
}

func TestJudge(t *testing.T) {
	inst := Instance{Vars: map[string]string{"uid": "abc"}}
	s := func(v string) *string { return &v }
	f := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }
	cases := []struct {
		e    Expect
		out  string
		exit int
		ok   bool
	}{
		{Expect{Equals: s("3")}, " 3\n", 0, true},
		{Expect{Equals: s("{{uid}}")}, "abc", 0, true},
		{Expect{Equals: s("{{uid}}")}, "abd", 0, false},
		{Expect{Contains: s("Ready")}, "Ready,SchedulingDisabled", 0, true},
		{Expect{Regex: s("^[1-9]")}, "2", 0, true},
		{Expect{Regex: s("^[1-9]")}, "0", 0, false},
		{Expect{Exit: i(0)}, "", 1, false},
		{Expect{NonEmpty: true}, "  ", 0, false},
		{Expect{Empty: true}, "\n", 0, true},
		{Expect{Min: f(3)}, "3", 0, true},
		{Expect{Min: f(3)}, "2", 0, false},
		{Expect{Min: f(1), Max: f(2)}, "x", 0, false},
		{Expect{}, "", 0, true},
		{Expect{}, "", 2, false},
	}
	for _, c := range cases {
		ok, _, err := Check{Expect: c.e}.Judge(c.out, c.exit, inst)
		if err != nil {
			t.Errorf("%+v: %v", c.e, err)
		}
		if ok != c.ok {
			t.Errorf("%+v on %q (exit %d) = %v, want %v", c.e, c.out, c.exit, ok, c.ok)
		}
	}
}

func TestSupports(t *testing.T) {
	env := Env{ControlPlane: "cp", Workers: []string{"w1", "w2"}, CNI: "flannel", Version: "v1.37.2"}
	cases := map[string]bool{"calico": false, "flannel": true, "workers>=2": true, "workers>=3": false, "k8s>=1.30": true, "k8s>=1.40": false}
	for r, want := range cases {
		if ok, why := (Task{Requires: []string{r}}).Supports(env); ok != want {
			t.Errorf("%s: %v (%s), want %v", r, ok, why, want)
		}
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
bank: Bad Name
title: x
target: k3d
min_nodes: 0
stages: []
categories: [{id: a, title: A}]
tasks:
  - id: t
    title: t
    brief: b
    kind: outage
    category: zz
    difficulty: 5
    time_limit: 10s
    check: [{name: c, run: "echo {{missing}}"}]
`), "bad.yaml")
	if err == nil {
		t.Fatal("a broken bank parsed")
	}
	for _, want := range []string{"bank:", "target:", "min_nodes", "stages", "kind is", "category", "difficulty", "time_limit", "solution", "cleanup", "{{missing}}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

func TestPublicHidesScriptsAndSolutions(t *testing.T) {
	banks, _ := LoadDir("../../../pagerpanic/banks")
	b := banks["kubernetes"]
	if b == nil {
		t.Skip("no bank")
	}
	pb := b.Public(Env{ControlPlane: "cp", Workers: []string{"w1", "w2"}, CNI: "flannel"})
	raw, _ := json.Marshal(pb)
	for _, task := range b.Tasks {
		for _, frag := range []string{task.Solution, task.Explain, task.Brief, task.Setup[0].Run} {
			line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(frag), "\n", 2)[0])
			if len(line) > 12 && strings.Contains(string(raw), line) {
				t.Errorf("%s: the public bank carries %q", task.ID, line)
			}
		}
		for _, h := range task.Hints {
			if strings.Contains(string(raw), h.Text) {
				t.Errorf("%s: the public bank carries a hint's text", task.ID)
			}
		}
	}
	unsupported := 0
	for _, t := range pb.Tasks {
		if t.Unsupported != "" {
			unsupported++
		}
	}
	if unsupported == 0 {
		t.Error("on Flannel, the Calico-only tasks should be marked unsupported")
	}
}

func TestMonitorPlanAndParse(t *testing.T) {
	task := Task{ID: "m", Vars: map[string]string{"ns": "shop-1"},
		Monitor: &Monitor{Title: "Shop", Service: "web", Namespace: "{{ns}}"}}
	inst, err := task.Instantiate(1, Env{ControlPlane: "cp", Workers: []string{"w1"}})
	if err != nil {
		t.Fatal(err)
	}
	p, ok, err := task.MonitorPlan(inst)
	if !ok || err != nil {
		t.Fatalf("plan: %v %v", ok, err)
	}
	if p.URL != "http://web.shop-1.svc/" {
		t.Errorf("address bar %q", p.URL)
	}
	v := ParseMonitor(p, "PPMON 0 200 text/html\n<h1>hi</h1>")
	if !v.OK || v.Status != 200 || v.Body != "<h1>hi</h1>" || v.Type != "text/html" {
		t.Errorf("a page that answered: %+v", v)
	}
	v = ParseMonitor(p, "PPMON 0 503 text/html\n<h1>503</h1>")
	if v.OK || v.Status != 503 {
		t.Errorf("a 503 counts as working: %+v", v)
	}
	v = ParseMonitor(p, "PPMON 7 000\ncurl: (7) Failed to connect")
	if v.OK || !strings.Contains(v.Error, "ERR_CONNECTION_REFUSED") || !strings.Contains(v.Detail, "Failed to connect") {
		t.Errorf("connection refused: %+v", v)
	}
	v = ParseMonitor(p, "PPMON nosvc 0 -\nError from server (NotFound): services \"web\" not found")
	if v.OK || v.Error == "" {
		t.Errorf("a missing Service: %+v", v)
	}
	if _, ok, _ := (Task{}).MonitorPlan(inst); ok {
		t.Error("a task with no monitor has a plan")
	}
}

func TestMonitorValidation(t *testing.T) {
	b := Bank{Bank: "b", Title: "B", Target: "kubeadm", MinNodes: 1, Lives: 1,
		Stages:     []Stage{{Open: 1, SpawnEvery: Duration{time.Minute}, TimeScale: 1, Difficulty: []int{1}}},
		Categories: []Category{{ID: "c", Title: "C"}}}
	base := Task{ID: "t", Title: "T", Brief: "b", Kind: "incident", Category: "c", Difficulty: 1,
		TimeLimit: Duration{time.Minute}, Check: []Check{{Name: "c", Run: "true"}}, Solution: "s",
		Cleanup: []Step{{Run: "true"}}}
	for _, tc := range []struct {
		m    Monitor
		want string
	}{
		{Monitor{Title: "A", Service: "s", URL: "http://x/"}, "a service or a url"},
		{Monitor{Title: "A"}, "a service or a url"},
		{Monitor{Service: "s"}, "needs a title"},
		{Monitor{Title: "A", URL: "http://{{nope}}/"}, "nope"},
		{Monitor{Title: "A", URL: "http://x/", On: "workers"}, "one node"},
	} {
		task := base
		m := tc.m
		task.Monitor = &m
		b.Tasks = []Task{task}
		if errs := strings.Join(b.Validate(), "; "); !strings.Contains(errs, tc.want) {
			t.Errorf("monitor %+v: errors %q, want %q", tc.m, errs, tc.want)
		}
	}
}
