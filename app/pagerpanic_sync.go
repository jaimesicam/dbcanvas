package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbcanvas/internal/pagerpanic"
)

// The game and DBCanvas talk in one direction: every pagerPanicSyncEvery, DBCanvas posts
// what it knows (the cluster, the bank, finished work) to the game's /api/dbcanvas/sync,
// and the game answers with what it wants done next. Work runs in the background, so a
// two-minute setup never holds up the poll; its progress and result go out on later polls
// until the game acknowledges them.
const (
	pagerPanicSyncEvery = 2 * time.Second
	// pagerPanicQuiet is how long a game may go unanswered, with tickets still on the
	// cluster, before DBCanvas cleans them up itself.
	pagerPanicQuiet = 90 * time.Second
)

// ppAction is one piece of work the game asks for.
type ppAction struct {
	ID       string `json:"id"`
	Op       string `json:"op"` // diagnose | setup | check | cleanup
	Task     string `json:"task,omitempty"`
	Seed     int64  `json:"seed,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// ppResult is the state of one action, sent until the game acknowledges it.
type ppResult struct {
	ID       string                   `json:"id"`
	Op       string                   `json:"op"`
	Done     bool                     `json:"done"`
	OK       bool                     `json:"ok"`
	Error    string                   `json:"error,omitempty"`
	Steps    []ppDiagStep             `json:"steps,omitempty"`
	Env      *pagerpanic.Env          `json:"env,omitempty"`
	Instance string                   `json:"instance,omitempty"`
	Worker   string                   `json:"worker,omitempty"`
	View     *pagerpanic.TaskView     `json:"view,omitempty"`
	Checks   []pagerpanic.CheckResult `json:"checks,omitempty"`
	Solved   bool                     `json:"solved,omitempty"`
	Monitor  *pagerpanic.MonitorView  `json:"monitor,omitempty"`
}

type ppDiagStep struct {
	Name   string `json:"name"`
	State  string `json:"state"` // pending | running | ok | warn | fail
	Detail string `json:"detail,omitempty"`
}

// ppRunner is DBCanvas's side of one game: work in flight and results not yet acknowledged.
type ppRunner struct {
	mu       sync.Mutex
	inflight map[string]bool
	results  map[string]ppResult
	finished map[string]time.Time // acknowledged, kept a while so a resent action is not rerun
	lastOK   time.Time
	// cfgMu serialises changes to the deployment's record (Active, Save).
	cfgMu sync.Mutex
}

var ppRunners = struct {
	sync.Mutex
	m map[string]*ppRunner
}{m: map[string]*ppRunner{}}

func ppRunnerFor(stackID int64, nodeID string) *ppRunner {
	key := fmt.Sprintf("%d/%s", stackID, nodeID)
	ppRunners.Lock()
	defer ppRunners.Unlock()
	r, ok := ppRunners.m[key]
	if !ok {
		r = &ppRunner{inflight: map[string]bool{}, results: map[string]ppResult{}, finished: map[string]time.Time{}}
		ppRunners.m[key] = r
	}
	return r
}

func (r *ppRunner) put(res ppResult) {
	r.mu.Lock()
	r.results[res.ID] = res
	if res.Done {
		delete(r.inflight, res.ID)
	}
	r.mu.Unlock()
}

func (a *App) startPagerPanicSync() {
	go func() {
		for {
			a.pagerPanicPass(context.Background())
			time.Sleep(pagerPanicSyncEvery)
		}
	}()
}

func (a *App) pagerPanicPass(ctx context.Context) {
	stacks, err := a.store.ListStacks(0, true)
	if err != nil {
		return
	}
	for _, sum := range stacks {
		if sum.Status != StackDeployed {
			continue
		}
		st, err := a.store.GetStack(sum.ID)
		if err != nil {
			continue
		}
		doc := buildDoc(st)
		for _, n := range doc.Nodes {
			if n.Type != "pagerpanic" {
				continue
			}
			dep, err := a.store.GetDeployment(st.ID, n.ID)
			if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
				continue
			}
			c, cancel := context.WithTimeout(ctx, 20*time.Second)
			a.syncPagerPanic(c, st, doc, n, dep)
			cancel()
		}
	}
}

// ppSyncRequest is what DBCanvas posts to the game.
type ppSyncRequest struct {
	Stack      ppStackRef             `json:"stack"`
	Nodes      []pagerPanicNode       `json:"nodes"`
	Env        pagerpanic.Env         `json:"env"`
	BankHash   string                 `json:"bankHash"`
	Bank       *pagerpanic.PublicBank `json:"bank,omitempty"`
	BankErrors []string               `json:"bankErrors,omitempty"`
	ClusterErr string                 `json:"clusterError,omitempty"`
	Results    []ppResult             `json:"results"`
	Saved      json.RawMessage        `json:"saved,omitempty"`
}

type ppStackRef struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Frame string `json:"frame"`
}

// ppSyncResponse is what the game answers.
type ppSyncResponse struct {
	Actions   []ppAction      `json:"actions"`
	Ack       []string        `json:"ack"`
	NeedBank  bool            `json:"needBank"`
	NeedSaved bool            `json:"needSaved"`
	Save      json.RawMessage `json:"save,omitempty"`
	// Active is every ticket the game still owns. One DBCanvas set up that is not in it
	// (the game restarted, say) is cleaned up.
	Active []string `json:"active"`
}

func (a *App) syncPagerPanic(ctx context.Context, st Stack, doc designDoc, n designNode, dep Deployment) {
	var cfg pagerPanicConfig
	if json.Unmarshal(dep.Config, &cfg) != nil {
		return
	}
	run := ppRunnerFor(st.ID, n.ID)
	f, linked := pagerPanicLink(doc, n.ID)
	banks, bankErrs := pagerPanicBanks()
	bank := pagerPanicBankFor(banks, "kubeadm")

	req := ppSyncRequest{Stack: ppStackRef{ID: st.ID, Name: st.Name, Frame: f.Label}}
	for _, e := range bankErrs {
		req.BankErrors = append(req.BankErrors, e.Error())
	}
	var nodes []pagerPanicNode
	var env pagerpanic.Env
	if linked {
		var err error
		nodes, env, err = a.pagerPanicCluster(st, doc, f)
		if err != nil {
			req.ClusterErr = err.Error()
		}
	} else {
		req.ClusterErr = "the game is not linked to a Kubeadm Cluster"
	}
	// A self-test changes the cluster on its own; a shift played at the same time would
	// collide with it. The self-test refuses to start while the game has tickets out.
	selfTest := ppSelfTestRunning(st.ID, n.ID)
	if selfTest && req.ClusterErr == "" {
		req.ClusterErr = "a self-test is running on the cluster; play when it finishes"
	}
	req.Nodes, req.Env = nodes, env
	var pub pagerpanic.PublicBank
	if bank != nil {
		pub = bank.Public(env)
		raw, _ := json.Marshal(pub)
		sum := sha256.Sum256(raw)
		req.BankHash = hex.EncodeToString(sum[:8])
	}
	run.mu.Lock()
	for _, r := range run.results {
		req.Results = append(req.Results, r)
	}
	run.mu.Unlock()

	var sec pagerPanicSecrets
	json.Unmarshal(dep.Secrets, &sec)
	resp, err := a.pagerPanicCall(ctx, st, dep.ContainerID, sec.Token, req)
	if err == nil && (resp.NeedBank || resp.NeedSaved) {
		// The game asked for the bank or its record; send them once, then carry on.
		if resp.NeedBank && bank != nil {
			req.Bank = &pub
		}
		if resp.NeedSaved {
			req.Saved = cfg.Save
			if len(req.Saved) == 0 {
				req.Saved = json.RawMessage("{}")
			}
		}
		resp, err = a.pagerPanicCall(ctx, st, dep.ContainerID, sec.Token, req)
	}
	if err != nil {
		run.mu.Lock()
		quiet := !run.lastOK.IsZero() && time.Since(run.lastOK) > pagerPanicQuiet
		run.mu.Unlock()
		if quiet && len(cfg.Active) > 0 && linked {
			log.Printf("stack %d: Pager Panic %s has not answered for %s; cleaning up its %d ticket(s)", st.ID, n.Label, pagerPanicQuiet, len(cfg.Active))
			a.pagerPanicCleanupAll(st, n.ID, nodes, env, "")
		}
		return
	}
	run.mu.Lock()
	run.lastOK = time.Now()
	for _, id := range resp.Ack {
		if res, ok := run.results[id]; ok && res.Done {
			delete(run.results, id)
			run.finished[id] = time.Now()
		}
	}
	for id, at := range run.finished {
		if time.Since(at) > 15*time.Minute {
			delete(run.finished, id)
		}
	}
	var todo []ppAction
	for _, act := range resp.Actions {
		if run.inflight[act.ID] {
			continue
		}
		if _, done := run.finished[act.ID]; done {
			continue
		}
		if _, have := run.results[act.ID]; have {
			continue
		}
		run.inflight[act.ID] = true
		todo = append(todo, act)
	}
	run.mu.Unlock()

	if len(resp.Save) > 0 && string(resp.Save) != string(cfg.Save) {
		a.pagerPanicUpdateConfig(st.ID, n.ID, func(c *pagerPanicConfig) { c.Save = resp.Save })
	}
	// Tickets the game no longer knows about are put back.
	if linked && len(cfg.Active) > 0 {
		owned := map[string]bool{}
		for _, id := range resp.Active {
			owned[id] = true
		}
		for _, act := range cfg.Active {
			// A running self-test's tasks are on the cluster too, and are never the game's.
			if selfTest && strings.HasPrefix(act.Instance.ID, ppSelfTestPrefix) {
				continue
			}
			if !owned[act.Instance.ID] && !run.isInflightFor(act.Instance.ID) {
				a.pagerPanicCleanupAll(st, n.ID, nodes, env, act.Instance.ID)
			}
		}
	}
	if bank == nil || !linked || req.ClusterErr != "" {
		for _, act := range todo {
			run.put(ppResult{ID: act.ID, Op: act.Op, Done: true, Error: firstNonEmpty(req.ClusterErr, "no task bank for a Kubeadm Cluster")})
		}
		return
	}
	for _, act := range todo {
		act := act
		go a.pagerPanicDo(st, n.ID, bank, nodes, env, act, run)
	}
}

func (r *ppRunner) isInflightFor(instanceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight[instanceID]
}

func (a *App) pagerPanicCall(ctx context.Context, st Stack, containerID, token string, req ppSyncRequest) (ppSyncResponse, error) {
	var out ppSyncResponse
	eng := a.dialEngine(st.ID, containerID)
	netName := networkName(st.ID)
	if err := a.joinStackForDial(ctx, eng, netName); err != nil {
		return out, err
	}
	ip, err := eng.ContainerIP(ctx, containerID, netName)
	if err != nil || ip == "" {
		return out, fmt.Errorf("the game has no address on the stack network")
	}
	body, _ := json.Marshal(req)
	url := "http://" + net.JoinHostPort(ip, strconv.Itoa(pagerPanicPort)) + "/api/dbcanvas/sync"
	hreq, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("X-Pager-Panic-Token", token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(hreq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("sync answered %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

// pagerPanicUpdateConfig changes the node's deployment record under the runner's lock.
func (a *App) pagerPanicUpdateConfig(stackID int64, nodeID string, change func(*pagerPanicConfig)) {
	run := ppRunnerFor(stackID, nodeID)
	run.cfgMu.Lock()
	defer run.cfgMu.Unlock()
	dep, err := a.store.GetDeployment(stackID, nodeID)
	if err != nil {
		return
	}
	var cfg pagerPanicConfig
	if json.Unmarshal(dep.Config, &cfg) != nil {
		return
	}
	change(&cfg)
	dep.Config = mustJSON(cfg)
	a.store.UpsertDeployment(dep)
}

func (a *App) pagerPanicActive(stackID int64, nodeID, instanceID string) (pagerPanicActive, bool) {
	dep, err := a.store.GetDeployment(stackID, nodeID)
	if err != nil {
		return pagerPanicActive{}, false
	}
	var cfg pagerPanicConfig
	json.Unmarshal(dep.Config, &cfg)
	for _, act := range cfg.Active {
		if act.Instance.ID == instanceID {
			return act, true
		}
	}
	return pagerPanicActive{}, false
}

// ---------------------------------------------------------------- running a script

// ppExec runs a script as root on one node, bounded by timeout.
func (a *App) ppExec(ctx context.Context, node pagerPanicNode, script string, env []string, timeout time.Duration) (stdout, stderr string, code int, err error) {
	secs := int(timeout.Seconds())
	if secs < 5 {
		secs = 5
	}
	cctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	res, err := a.engCtx(cctx).Exec(cctx, node.id, []string{"timeout", "-k", "5", strconv.Itoa(secs), "bash", "-c", script}, env)
	if err != nil {
		return "", "", -1, err
	}
	if res.Code == 124 {
		return res.Stdout, res.Stderr, res.Code, fmt.Errorf("timed out after %ds", secs)
	}
	return res.Stdout, res.Stderr, res.Code, nil
}

func ppNodeByHost(nodes []pagerPanicNode, host string) (pagerPanicNode, bool) {
	for _, n := range nodes {
		if n.Host == host {
			return n, true
		}
	}
	return pagerPanicNode{}, false
}

// ppRunStep runs one step of a task on every node it names.
func (a *App) ppRunStep(ctx context.Context, nodes []pagerPanicNode, env pagerpanic.Env, inst pagerpanic.Instance, step pagerpanic.Step, setE bool) error {
	body, err := inst.Render(step.Run)
	if err != nil {
		return err
	}
	hosts, err := inst.Hosts(step.On, env)
	if err != nil {
		return err
	}
	script := pagerpanic.Script(body, setE)
	timeout := pagerpanic.StepTimeout(step.Timeout, pagerpanic.DefaultStepTimeout)
	for _, h := range hosts {
		node, ok := ppNodeByHost(nodes, h)
		if !ok {
			return fmt.Errorf("no node %s", h)
		}
		out, errOut, code, err := a.ppExec(ctx, node, script, []string{"PP_INSTANCE=" + inst.ID}, timeout)
		if err != nil {
			return fmt.Errorf("%s: %v: %s", h, err, pagerpanic.Summarize(errOut+out))
		}
		if code != 0 {
			return fmt.Errorf("%s: exit %d: %s", h, code, pagerpanic.Summarize(firstNonEmpty(strings.TrimSpace(errOut), out)))
		}
	}
	return nil
}

// ---------------------------------------------------------------- actions

func (a *App) pagerPanicDo(st Stack, nodeID string, bank *pagerpanic.Bank, nodes []pagerPanicNode, env pagerpanic.Env, act ppAction, run *ppRunner) {
	ctx := context.Background()
	defer func() {
		if p := recover(); p != nil {
			run.put(ppResult{ID: act.ID, Op: act.Op, Done: true, Error: fmt.Sprintf("internal error: %v", p)})
		}
	}()
	switch act.Op {
	case "diagnose":
		a.pagerPanicDiagnose(ctx, st, nodeID, bank, nodes, env, act, run)
	case "setup":
		res := a.pagerPanicSetup(ctx, st, nodeID, bank, nodes, env, act)
		run.put(res)
	case "check":
		run.put(a.pagerPanicCheck(ctx, st, nodeID, bank, nodes, env, act))
	case "cleanup":
		err := a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, act.Instance)
		res := ppResult{ID: act.ID, Op: act.Op, Done: true, OK: err == nil, Instance: act.Instance}
		if err != nil {
			res.Error = err.Error()
		}
		run.put(res)
	default:
		run.put(ppResult{ID: act.ID, Op: act.Op, Done: true, Error: "unknown action " + act.Op})
	}
}

func (a *App) pagerPanicSetup(ctx context.Context, st Stack, nodeID string, bank *pagerpanic.Bank, nodes []pagerPanicNode, env pagerpanic.Env, act ppAction) ppResult {
	res := ppResult{ID: act.ID, Op: "setup", Done: true, Instance: act.ID}
	task, ok := bank.Task(act.Task)
	if !ok {
		res.Error = "no task " + act.Task
		return res
	}
	if ok, why := task.Supports(env); !ok {
		res.Error = why
		return res
	}
	inst, err := task.Instantiate(act.Seed, env)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// The game names the instance, so it can tell DBCanvas which tickets it still owns
	// before this answer reaches it.
	inst.ID = act.ID
	// Recorded before anything runs, so even a setup that fails halfway is cleaned up.
	a.pagerPanicUpdateConfig(st.ID, nodeID, func(c *pagerPanicConfig) {
		c.Active = append(c.Active, pagerPanicActive{Bank: bank.Bank, Instance: inst})
	})
	for i, step := range task.Setup {
		if err := a.ppRunStep(ctx, nodes, env, inst, step, true); err != nil {
			res.Error = fmt.Sprintf("setup step %d: %v", i+1, err)
			a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, inst.ID)
			return res
		}
		for k, cmd := range step.Record {
			body, err := inst.Render(cmd)
			if err != nil {
				res.Error = err.Error()
				a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, inst.ID)
				return res
			}
			cp, _ := ppNodeByHost(nodes, env.ControlPlane)
			out, _, code, err := a.ppExec(ctx, cp, pagerpanic.Script(body, false), []string{"PP_INSTANCE=" + inst.ID}, pagerpanic.DefaultCheckTimeout)
			if err != nil || code != 0 {
				res.Error = fmt.Sprintf("record %s: %v (exit %d)", k, err, code)
				a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, inst.ID)
				return res
			}
			inst.Vars[k] = strings.TrimSpace(out)
		}
	}
	a.pagerPanicUpdateConfig(st.ID, nodeID, func(c *pagerPanicConfig) {
		for i := range c.Active {
			if c.Active[i].Instance.ID == inst.ID {
				c.Active[i].Instance = inst
			}
		}
	})
	view, err := task.View(inst)
	if err != nil {
		res.Error = err.Error()
		a.pagerPanicCleanup(ctx, st, nodeID, nodes, env, inst.ID)
		return res
	}
	res.OK, res.View, res.Worker = true, &view, inst.Worker
	res.Monitor = a.ppFetchMonitor(ctx, nodes, env, inst, task)
	return res
}

func (a *App) pagerPanicCheck(ctx context.Context, st Stack, nodeID string, bank *pagerpanic.Bank, nodes []pagerPanicNode, env pagerpanic.Env, act ppAction) ppResult {
	res := ppResult{ID: act.ID, Op: "check", Done: true, Instance: act.Instance}
	active, ok := a.pagerPanicActive(st.ID, nodeID, act.Instance)
	if !ok {
		res.Error = "the ticket is not on the cluster"
		return res
	}
	b := bank
	if active.Bank != bank.Bank {
		banks, _ := pagerPanicBanks()
		if b = banks[active.Bank]; b == nil {
			res.Error = "its bank is gone"
			return res
		}
	}
	task, ok := b.Task(active.Instance.TaskID)
	if !ok {
		res.Error = "its task is gone from the bank"
		return res
	}
	res.Checks, res.Solved = a.ppRunChecks(ctx, nodes, env, active.Instance, task)
	res.Monitor = a.ppFetchMonitor(ctx, nodes, env, active.Instance, task)
	res.OK = true
	return res
}

// ppFetchMonitor fetches the page a task's monitor watches; nil when the task has none.
func (a *App) ppFetchMonitor(ctx context.Context, nodes []pagerPanicNode, env pagerpanic.Env, inst pagerpanic.Instance, task pagerpanic.Task) *pagerpanic.MonitorView {
	plan, ok, err := task.MonitorPlan(inst)
	if !ok {
		return nil
	}
	v := pagerpanic.MonitorView{Title: plan.Title, URL: plan.URL}
	if err != nil {
		v.Error, v.Detail = "The page could not be fetched", err.Error()
		return &v
	}
	hosts, err := inst.Hosts(plan.On, env)
	if err != nil || len(hosts) == 0 {
		v.Error, v.Detail = "The page could not be fetched", fmt.Sprint(err)
		return &v
	}
	node, ok := ppNodeByHost(nodes, hosts[0])
	if !ok {
		v.Error, v.Detail = "The page could not be fetched", "no node "+hosts[0]
		return &v
	}
	out, errOut, _, err := a.ppExec(ctx, node, pagerpanic.Script(pagerpanic.MonitorScript, false),
		append([]string{"PP_INSTANCE=" + inst.ID}, plan.Env...), 20*time.Second)
	if err != nil {
		v.Error, v.Detail = "The page could not be fetched", pagerpanic.Summarize(firstNonEmpty(errOut, err.Error()))
		return &v
	}
	v = pagerpanic.ParseMonitor(plan, out)
	return &v
}

// ppRunChecks runs a task's checks and says whether all of them pass.
func (a *App) ppRunChecks(ctx context.Context, nodes []pagerPanicNode, env pagerpanic.Env, inst pagerpanic.Instance, task pagerpanic.Task) ([]pagerpanic.CheckResult, bool) {
	var out []pagerpanic.CheckResult
	solved := true
	for _, c := range task.Check {
		name, _ := inst.Render(c.Name)
		cr := pagerpanic.CheckResult{Name: name, Guard: c.Guard}
		body, err := inst.Render(c.Run)
		hosts, herr := inst.Hosts(c.On, env)
		if err != nil || herr != nil {
			cr.Got = firstNonEmpty(fmt.Sprint(err), fmt.Sprint(herr))
			out = append(out, cr)
			solved = false
			continue
		}
		cr.OK = true
		for _, h := range hosts {
			node, ok := ppNodeByHost(nodes, h)
			if !ok {
				cr.OK, cr.Got = false, "no node "+h
				break
			}
			stdout, stderr, code, err := a.ppExec(ctx, node, pagerpanic.Script(body, false), []string{"PP_INSTANCE=" + inst.ID},
				pagerpanic.StepTimeout(c.Timeout, pagerpanic.DefaultCheckTimeout))
			if err != nil {
				cr.OK, cr.Got = false, err.Error()
				break
			}
			ok, want, jerr := c.Judge(stdout, code, inst)
			cr.Want = want
			cr.Got = pagerpanic.Summarize(stdout)
			if strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) != "" {
				cr.Got = pagerpanic.Summarize(stderr)
			}
			if jerr != nil {
				ok, cr.Got = false, jerr.Error()
			}
			if !ok {
				cr.OK = false
				if len(hosts) > 1 {
					cr.Got = h + ": " + cr.Got
				}
				break
			}
		}
		if !cr.OK {
			solved = false
		}
		out = append(out, cr)
	}
	return out, solved
}

// pagerPanicCleanup runs an instance's cleanup and forgets it. Cleanup steps run even when
// an earlier one fails: putting back as much as possible beats stopping halfway.
func (a *App) pagerPanicCleanup(ctx context.Context, st Stack, nodeID string, nodes []pagerPanicNode, env pagerpanic.Env, instanceID string) error {
	active, ok := a.pagerPanicActive(st.ID, nodeID, instanceID)
	if !ok {
		return nil
	}
	banks, _ := pagerPanicBanks()
	var errs []string
	if b := banks[active.Bank]; b != nil {
		if task, ok := b.Task(active.Instance.TaskID); ok {
			for i, step := range task.Cleanup {
				if err := a.ppRunStep(ctx, nodes, env, active.Instance, step, false); err != nil {
					errs = append(errs, fmt.Sprintf("cleanup step %d: %v", i+1, err))
				}
			}
		}
	}
	a.pagerPanicUpdateConfig(st.ID, nodeID, func(c *pagerPanicConfig) {
		kept := c.Active[:0]
		for _, act := range c.Active {
			if act.Instance.ID != instanceID {
				kept = append(kept, act)
			}
		}
		c.Active = kept
	})
	if len(errs) > 0 {
		log.Printf("stack %d: Pager Panic %s: cleaning up %s: %s", st.ID, nodeID, instanceID, strings.Join(errs, "; "))
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// pagerPanicCleanupAll cleans up one ticket, or every ticket when instanceID is "".
func (a *App) pagerPanicCleanupAll(st Stack, nodeID string, nodes []pagerPanicNode, env pagerpanic.Env, instanceID string) {
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil {
		return
	}
	var cfg pagerPanicConfig
	json.Unmarshal(dep.Config, &cfg)
	run := ppRunnerFor(st.ID, nodeID)
	for _, act := range cfg.Active {
		if instanceID != "" && act.Instance.ID != instanceID {
			continue
		}
		run.mu.Lock()
		busy := run.inflight["cleanup:"+act.Instance.ID]
		run.inflight["cleanup:"+act.Instance.ID] = true
		run.mu.Unlock()
		if busy {
			continue
		}
		go func(id string) {
			a.pagerPanicCleanup(context.Background(), st, nodeID, nodes, env, id)
			run.mu.Lock()
			delete(run.inflight, "cleanup:"+id)
			run.mu.Unlock()
		}(act.Instance.ID)
	}
}

// clearPagerPanic puts the cluster back when a Pager Panic node is removed with tickets
// still on it. Best-effort, and skipped when the cluster is going away too.
func (a *App) clearPagerPanic(ctx context.Context, st Stack, d Deployment) {
	var cfg pagerPanicConfig
	if json.Unmarshal(d.Config, &cfg) != nil || len(cfg.Active) == 0 || cfg.Frame == "" {
		return
	}
	doc := buildDoc(st)
	f := frameByID(doc, cfg.Frame)
	if f.ID == "" {
		return
	}
	nodes, env, err := a.pagerPanicCluster(st, doc, f)
	if err != nil {
		return
	}
	c, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	for _, act := range cfg.Active {
		a.pagerPanicCleanup(c, st, d.NodeID, nodes, env, act.Instance.ID)
	}
}
