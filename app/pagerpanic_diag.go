package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"dbcanvas/internal/pagerpanic"
)

// ---------------------------------------------------------------- the systems check

// pagerPanicDiagnose is the check before a game starts. Each step's state goes to the
// game as it changes, so the player watches the lines turn green one by one.
func (a *App) pagerPanicDiagnose(ctx context.Context, st Stack, nodeID string, bank *pagerpanic.Bank, nodes []pagerPanicNode, env pagerpanic.Env, act ppAction, run *ppRunner) {
	names := []string{"Cluster nodes", "Nodes Ready", "System pods", "Leftovers from earlier shifts",
		"Images on every node", "Scheduling and DNS on every worker", "Tools on the control plane"}
	steps := make([]ppDiagStep, len(names))
	for i, n := range names {
		steps[i] = ppDiagStep{Name: n, State: "pending"}
	}
	res := ppResult{ID: act.ID, Op: "diagnose"}
	push := func() {
		res.Steps = append([]ppDiagStep(nil), steps...)
		run.put(res)
	}
	set := func(i int, state, detail string) {
		steps[i].State, steps[i].Detail = state, detail
		push()
	}
	failed := false
	finish := func() {
		res.Done, res.OK = true, !failed
		e := env
		res.Env = &e
		push()
	}
	cp, _ := ppNodeByHost(nodes, env.ControlPlane)
	sh := func(n pagerPanicNode, script string, timeout time.Duration) (string, int, error) {
		out, errOut, code, err := a.ppExec(ctx, n, pagerpanic.Script(script, false), nil, timeout)
		if code != 0 && strings.TrimSpace(out) == "" {
			out = errOut
		}
		return strings.TrimSpace(out), code, err
	}

	set(0, "running", "")
	if len(nodes) < bank.MinNodes {
		failed = true
		set(0, "fail", fmt.Sprintf("%d nodes; this bank needs %d or more", len(nodes), bank.MinNodes))
		finish()
		return
	}
	set(0, "ok", fmt.Sprintf("%d nodes: %s (control plane), %s", len(nodes), env.ControlPlane, strings.Join(env.Workers, ", ")))

	set(1, "running", "")
	out, code, err := sh(cp, `kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'`, 30*time.Second)
	if err != nil || code != 0 {
		failed = true
		set(1, "fail", "kubectl get nodes: "+pagerpanic.Summarize(firstNonEmpty(out, fmt.Sprint(err))))
		finish()
		return
	}
	var notReady []string
	ready := 0
	for _, line := range strings.Split(out, "\n") {
		if name, status, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			if status == "True" {
				ready++
			} else {
				notReady = append(notReady, name)
			}
		}
	}
	if len(notReady) > 0 || ready < len(nodes) {
		failed = true
		set(1, "fail", fmt.Sprintf("%d of %d Ready; not Ready: %s", ready, len(nodes), strings.Join(notReady, ", ")))
		finish()
		return
	}
	set(1, "ok", fmt.Sprintf("%d of %d Ready", ready, len(nodes)))

	set(2, "running", "")
	out, _, _ = sh(cp, `kubectl get pods -A --no-headers 2>/dev/null | awk '$4!="Running" && $4!="Completed" {print $1"/"$2" "$4}' | grep -v '^arcade-' || true`, 30*time.Second)
	if out != "" {
		failed = true
		set(2, "fail", "not running: "+pagerpanic.Summarize(out))
		finish()
		return
	}
	set(2, "ok", "every system pod is running ("+env.CNI+" pod network)")

	set(3, "running", "")
	detail := "none"
	dep, _ := a.store.GetDeployment(st.ID, nodeID)
	var cfg pagerPanicConfig
	json.Unmarshal(dep.Config, &cfg)
	if len(cfg.Active) == 0 {
		// Nothing is in play, so any arcade- namespace is from a shift that ended badly.
		out, _, _ = sh(cp, `ns=$(kubectl get ns -o name | sed 's#namespace/##' | grep '^arcade-' || true)
[ -n "$ns" ] && kubectl delete ns $ns --wait=false >/dev/null 2>&1; echo $ns | wc -w`, 60*time.Second)
		if out != "0" && out != "" {
			detail = out + " old namespace(s) removed"
		}
	}
	foreign, _, _ := sh(cp, `kubectl get ns -o name | sed 's#namespace/##' | grep -v -E '^(default|kube-|calico|tigera|kube-flannel|arcade-|local-path)' || true`, 30*time.Second)
	if foreign != "" {
		set(3, "warn", "the cluster also runs namespaces the game does not own ("+strings.ReplaceAll(foreign, "\n", ", ")+"); tickets may change what runs there")
	} else {
		set(3, "ok", detail)
	}

	set(4, "running", "pulling "+strings.Join(bank.Images, ", "))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var pullErrs []string
	for _, n := range nodes {
		wg.Add(1)
		go func(n pagerPanicNode) {
			defer wg.Done()
			script := ""
			for _, img := range bank.Images {
				ref := img
				if !strings.Contains(strings.SplitN(img, "/", 2)[0], ".") {
					ref = "docker.io/library/" + img
					if strings.Contains(img, "/") {
						ref = "docker.io/" + img
					}
				}
				script += fmt.Sprintf("crictl inspecti %q >/dev/null 2>&1 || crictl pull %q >/dev/null || exit 1\n", ref, ref)
			}
			out, code, err := sh(n, script, 6*time.Minute)
			if err != nil || code != 0 {
				mu.Lock()
				pullErrs = append(pullErrs, n.Host+": "+pagerpanic.Summarize(firstNonEmpty(out, fmt.Sprint(err))))
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	if len(pullErrs) > 0 {
		failed = true
		sort.Strings(pullErrs)
		set(4, "fail", strings.Join(pullErrs, "; "))
		finish()
		return
	}
	set(4, "ok", fmt.Sprintf("%d image(s) on %d nodes", len(bank.Images), len(nodes)))

	set(5, "running", "")
	probe := `set -e
kubectl create ns arcade-probe --dry-run=client -o yaml | kubectl apply -f - >/dev/null
for w in ` + strings.Join(env.Workers, " ") + `; do
  kubectl -n arcade-probe delete pod "probe-$w" --ignore-not-found --wait=true >/dev/null
  kubectl -n arcade-probe run "probe-$w" --image=busybox:1.36 --restart=Never \
    --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"'"$w"'"}}}' \
    -- nslookup kubernetes.default.svc.cluster.local >/dev/null
done
fail=""
for w in ` + strings.Join(env.Workers, " ") + `; do
  ok=""
  for i in $(seq 1 45); do
    p=$(kubectl -n arcade-probe get pod "probe-$w" -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$p" = Succeeded ] && { ok=1; break; }
    [ "$p" = Failed ] && break
    sleep 2
  done
  if [ -z "$ok" ]; then
    n=$(kubectl -n arcade-probe get pod "probe-$w" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
    if [ -z "$n" ]; then fail="$fail $w(not scheduled)"; else fail="$fail $w(DNS lookup failed)"; fi
  fi
done
kubectl delete ns arcade-probe --wait=false >/dev/null 2>&1 || true
echo "$fail"`
	out, code, err = sh(cp, probe, 4*time.Minute)
	if err != nil || code != 0 || out != "" {
		failed = true
		set(5, "fail", pagerpanic.Summarize(firstNonEmpty(out, fmt.Sprint(err))))
		finish()
		return
	}
	set(5, "ok", "a pod ran on each worker and resolved kubernetes.default")

	set(6, "running", "")
	out, _, _ = sh(cp, `m=""; for t in kubectl etcdctl curl crictl; do command -v $t >/dev/null || m="$m $t"; done; echo $m`, 20*time.Second)
	if out != "" {
		failed = true
		set(6, "fail", "missing:"+out)
		finish()
		return
	}
	set(6, "ok", "kubectl, etcdctl, curl, crictl")
	finish()
}

// ---------------------------------------------------------------- the self-test

// The self-test plays every task of the bank for real: setup, the checks must fail, the
// task's solve script, the checks must pass, cleanup, and the cluster must be healthy
// again. It is how a task proves it can be won before a player meets it.

type ppSelfTestTask struct {
	Task    string                   `json:"task"`
	State   string                   `json:"state"` // pending | running | passed | failed | skipped
	Stage   string                   `json:"stage,omitempty"`
	Error   string                   `json:"error,omitempty"`
	Before  []pagerpanic.CheckResult `json:"before,omitempty"`
	After   []pagerpanic.CheckResult `json:"after,omitempty"`
	// MonitorBefore and MonitorAfter are what the task's monitor showed (no page body).
	MonitorBefore string `json:"monitorBefore,omitempty"`
	MonitorAfter  string `json:"monitorAfter,omitempty"`
	Seconds float64                  `json:"seconds"`
}

type ppSelfTest struct {
	Running  bool             `json:"running"`
	Started  time.Time        `json:"started"`
	Finished time.Time        `json:"finished,omitempty"`
	Bank     string           `json:"bank"`
	Tasks    []ppSelfTestTask `json:"tasks"`
}

// ppSelfTestPrefix starts the instance id of every task a self-test sets up.
const ppSelfTestPrefix = "selftest-"

// ppSelfTestRunning reports whether a self-test is running on the node. While one is,
// the sync leaves its tasks alone; once none is (DBCanvas restarted mid-test, say),
// any left on the cluster are orphans and are cleaned up like any other.
func ppSelfTestRunning(stackID int64, nodeID string) bool {
	ppSelfTests.Lock()
	defer ppSelfTests.Unlock()
	t := ppSelfTests.m[fmt.Sprintf("%d/%s", stackID, nodeID)]
	return t != nil && t.Running
}

var ppSelfTests = struct {
	sync.Mutex
	m map[string]*ppSelfTest
}{m: map[string]*ppSelfTest{}}

func (a *App) pagerPanicNodeFor(w http.ResponseWriter, r *http.Request) (Stack, designDoc, designNode, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return st, designDoc{}, designNode{}, false
	}
	doc := buildDoc(st)
	nid := r.PathValue("nid")
	for _, n := range doc.Nodes {
		if n.ID == nid && n.Type == "pagerpanic" {
			return st, doc, n, true
		}
	}
	writeErr(w, http.StatusNotFound, "no Pager Panic node "+nid+" in this stack")
	return st, doc, designNode{}, false
}

// handlePagerPanicSelfTest starts a self-test: every task, or the ones named.
func (a *App) handlePagerPanicSelfTest(w http.ResponseWriter, r *http.Request) {
	st, doc, n, ok := a.pagerPanicNodeFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Tasks []string `json:"tasks"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	dep, err := a.store.GetDeployment(st.ID, n.ID)
	if err != nil || dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "the Pager Panic node is not running")
		return
	}
	var cfg pagerPanicConfig
	json.Unmarshal(dep.Config, &cfg)
	if len(cfg.Active) > 0 {
		writeErr(w, http.StatusConflict, "a game has tickets on the cluster; finish or quit it first")
		return
	}
	f, linked := pagerPanicLink(doc, n.ID)
	if !linked {
		writeErr(w, http.StatusConflict, "the node is not linked to a Kubeadm Cluster")
		return
	}
	nodes, env, err := a.pagerPanicCluster(st, doc, f)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	banks, _ := pagerPanicBanks()
	bank := pagerPanicBankFor(banks, "kubeadm")
	if bank == nil {
		writeErr(w, http.StatusConflict, "no task bank for a Kubeadm Cluster")
		return
	}
	want := map[string]bool{}
	for _, t := range body.Tasks {
		if _, ok := bank.Task(t); !ok {
			writeErr(w, http.StatusBadRequest, "no task "+t+" in bank "+bank.Bank)
			return
		}
		want[t] = true
	}
	key := fmt.Sprintf("%d/%s", st.ID, n.ID)
	ppSelfTests.Lock()
	if cur := ppSelfTests.m[key]; cur != nil && cur.Running {
		ppSelfTests.Unlock()
		writeErr(w, http.StatusConflict, "a self-test is already running")
		return
	}
	test := &ppSelfTest{Running: true, Started: time.Now(), Bank: bank.Bank}
	for _, t := range bank.Tasks {
		if len(want) == 0 || want[t.ID] {
			test.Tasks = append(test.Tasks, ppSelfTestTask{Task: t.ID, State: "pending"})
		}
	}
	ppSelfTests.m[key] = test
	ppSelfTests.Unlock()
	go a.pagerPanicRunSelfTest(st, n.ID, bank, nodes, env, test)
	writeJSON(w, http.StatusAccepted, test)
}

func (a *App) handlePagerPanicSelfTestGet(w http.ResponseWriter, r *http.Request) {
	st, _, n, ok := a.pagerPanicNodeFor(w, r)
	if !ok {
		return
	}
	ppSelfTests.Lock()
	defer ppSelfTests.Unlock()
	test := ppSelfTests.m[fmt.Sprintf("%d/%s", st.ID, n.ID)]
	if test == nil {
		writeErr(w, http.StatusNotFound, "no self-test has run on this node")
		return
	}
	writeJSON(w, http.StatusOK, test)
}

func (a *App) pagerPanicRunSelfTest(st Stack, nodeID string, bank *pagerpanic.Bank, nodes []pagerPanicNode, env pagerpanic.Env, test *ppSelfTest) {
	ctx := context.Background()
	update := func(i int, change func(*ppSelfTestTask)) {
		ppSelfTests.Lock()
		change(&test.Tasks[i])
		ppSelfTests.Unlock()
	}
	cp, _ := ppNodeByHost(nodes, env.ControlPlane)
	healthy := func() string {
		out, _, code, err := a.ppExec(ctx, cp, pagerpanic.Script(`for i in $(seq 1 60); do
  nr=$(kubectl get nodes --no-headers | awk '$2!="Ready"' | wc -l)
  bad=$(kubectl get pods -n kube-system --no-headers | awk '$3!="Running" && $3!="Completed"' | wc -l)
  [ "$nr" = 0 ] && [ "$bad" = 0 ] && exit 0
  sleep 3
done
kubectl get nodes --no-headers | awk '$2!="Ready"{print "node "$1" "$2}'
kubectl get pods -n kube-system --no-headers | awk '$3!="Running" && $3!="Completed"{print $1" "$3}'
exit 1`, false), nil, 4*time.Minute)
		if err != nil || code != 0 {
			return pagerpanic.Summarize(firstNonEmpty(out, fmt.Sprint(err)))
		}
		return ""
	}
	for i := range test.Tasks {
		start := time.Now()
		id := test.Tasks[i].Task
		task, _ := bank.Task(id)
		if ok, why := task.Supports(env); !ok {
			update(i, func(t *ppSelfTestTask) { t.State, t.Error = "skipped", why })
			continue
		}
		fail := func(stage, msg string) {
			update(i, func(t *ppSelfTestTask) {
				t.State, t.Stage, t.Error, t.Seconds = "failed", stage, msg, time.Since(start).Seconds()
			})
		}
		update(i, func(t *ppSelfTestTask) { t.State, t.Stage = "running", "setup" })
		actID := fmt.Sprintf("%s%s-%d", ppSelfTestPrefix, id, time.Now().UnixNano())
		res := a.pagerPanicSetup(ctx, st, nodeID, bank, nodes, env, ppAction{ID: actID, Op: "setup", Task: id, Seed: time.Now().UnixNano()})
		if !res.OK {
			fail("setup", res.Error)
			if h := healthy(); h != "" {
				fail("setup", res.Error+" — and the cluster did not recover: "+h)
				break
			}
			continue
		}
		active, _ := a.pagerPanicActive(st.ID, nodeID, actID)
		update(i, func(t *ppSelfTestTask) { t.Stage = "checks before the fix" })
		before, solved := a.ppRunChecks(ctx, nodes, env, active.Instance, task)
		monBefore := a.ppFetchMonitor(ctx, nodes, env, active.Instance, task)
		update(i, func(t *ppSelfTestTask) { t.Before, t.MonitorBefore = before, ppMonitorLine(monBefore) })
		if monBefore != nil && monBefore.OK {
			a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, actID)
			fail("checks before the fix", "the monitored page already works before anything was fixed")
			continue
		}
		if solved {
			a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, actID)
			fail("checks before the fix", "every check already passes before anything was fixed")
			continue
		}
		update(i, func(t *ppSelfTestTask) { t.Stage = "solve" })
		var solveErr error
		for j, step := range task.Solve {
			if err := a.ppRunStep(ctx, nodes, env, active.Instance, step, true); err != nil {
				solveErr = fmt.Errorf("solve step %d: %v", j+1, err)
				break
			}
		}
		if solveErr != nil {
			a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, actID)
			fail("solve", solveErr.Error())
			if h := healthy(); h != "" {
				fail("solve", solveErr.Error()+" — and the cluster did not recover: "+h)
				break
			}
			continue
		}
		update(i, func(t *ppSelfTestTask) { t.Stage = "checks after the fix" })
		var after []pagerpanic.CheckResult
		var monAfter *pagerpanic.MonitorView
		for try := 0; try < 6; try++ {
			after, solved = a.ppRunChecks(ctx, nodes, env, active.Instance, task)
			monAfter = a.ppFetchMonitor(ctx, nodes, env, active.Instance, task)
			if solved && (monAfter == nil || monAfter.OK) {
				break
			}
			time.Sleep(10 * time.Second)
		}
		update(i, func(t *ppSelfTestTask) { t.After, t.MonitorAfter = after, ppMonitorLine(monAfter) })
		monitorBroken := monAfter != nil && !monAfter.OK
		update(i, func(t *ppSelfTestTask) { t.Stage = "cleanup" })
		cerr := a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, actID)
		h := healthy()
		switch {
		case !solved:
			fail("checks after the fix", "the checks still fail after the solve script")
		case monitorBroken:
			fail("checks after the fix", "the checks pass but the monitored page still fails: "+ppMonitorLine(monAfter))
		case cerr != nil:
			fail("cleanup", cerr.Error())
		case h != "":
			fail("cleanup", "the cluster is not healthy after cleanup: "+h)
		default:
			update(i, func(t *ppSelfTestTask) { t.State, t.Stage, t.Seconds = "passed", "", time.Since(start).Seconds() })
		}
		if h != "" {
			break
		}
	}
	ppSelfTests.Lock()
	test.Running, test.Finished = false, time.Now()
	for i := range test.Tasks {
		if test.Tasks[i].State == "pending" {
			test.Tasks[i].State, test.Tasks[i].Error = "skipped", "not reached: an earlier task left the cluster unhealthy"
		}
	}
	ppSelfTests.Unlock()
}

// ppMonitorLine is a monitor's outcome on one line, for the self-test report.
func ppMonitorLine(v *pagerpanic.MonitorView) string {
	if v == nil {
		return ""
	}
	if v.Error != "" {
		return v.URL + ": " + v.Error + firstNonEmpty(" — "+pagerpanic.Summarize(v.Detail), "")
	}
	return fmt.Sprintf("%s: HTTP %d", v.URL, v.Status)
}
