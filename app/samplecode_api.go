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
)

// samplecode_api.go — the HTTP surface and the job that does the work on the node.
//
// Everything here is gated on loadRunningNode, the same check the web terminal and the file
// manager use, and for the same reason: this endpoint installs packages and runs a program as root
// inside the Linux Client. Anyone who can reach it can already open a root shell on that
// container, which is what makes the gate sufficient — and what makes it necessary.
//
// Two shapes, as elsewhere in the app: plain JSON for the things a page loads once (the catalogue,
// the endpoint list, the generated project), and a polled job for the part that takes minutes.
// A job is polled rather than streamed because the unit of progress here is a *step* — "checking
// python3", "installing mysql-connector-python", "running crud.py" — and a step is one exec that
// either worked or did not. There is nothing in between to stream.

// scMaxJobDuration bounds one job. Installing a JDK and letting Maven populate an empty ~/.m2 over
// a slow link is genuinely minutes; a program that hangs on an unreachable endpoint should not be
// allowed to sit there forever.
const scMaxJobDuration = 20 * time.Minute

// ------------------------------------------------------------------------------- jobs

type scLogLine struct {
	Time string `json:"time"`
	// Kind decides how the line is rendered: step (a heading), cmd (what is about to run),
	// out / err (what it said), ok / skip / fail (the verdict), info (everything else).
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// scJobState is the part of a job that is reported. Split from scJob so a snapshot can be
// returned by value without copying the mutex that guards it — which `go vet` is right to
// object to, and which would be a real bug the first time two readers raced.
type scJobState struct {
	ID      string `json:"id"`
	StackID int64  `json:"stackId"`
	NodeID  string `json:"nodeId"`
	Sample  string `json:"sample"`
	Action  string `json:"action"` // save | prepare | run | reset
	Status  string `json:"status"` // running | done | error | canceled
	Message string `json:"message,omitempty"`
	Dir     string `json:"dir,omitempty"`
	// Exit is the sample program's own exit code, and only meaningful once a run has
	// finished. A non-zero exit is not a DBCanvas failure — it is the program's answer.
	Exit  int         `json:"exit"`
	Ran   bool        `json:"ran"` // the program was actually executed
	Start time.Time   `json:"start"`
	End   time.Time   `json:"end,omitempty"`
	Log   []scLogLine `json:"log"`
}

type scJob struct {
	scJobState
	ownerID int64
	cancel  context.CancelFunc
	mu      sync.Mutex
}

func (j *scJob) add(kind, text string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		j.Log = append(j.Log, scLogLine{Time: time.Now().UTC().Format(time.RFC3339), Kind: kind, Text: ln})
	}
}

// setResult records the sample program's own outcome, under the same lock the log uses.
func (j *scJob) setResult(exit int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Ran, j.Exit = true, exit
}

func (j *scJob) finish(status, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status == "running" {
		j.Status, j.Message, j.End = status, msg, time.Now().UTC()
	}
}

// snapshot copies the job for a response, under the lock the log is appended with.
func (j *scJob) snapshot() scJobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.scJobState
	out.Log = append([]scLogLine(nil), j.Log...)
	return out
}

var scJobs = struct {
	sync.Mutex
	m map[string]*scJob
}{m: map[string]*scJob{}}

func scRegisterJob(j *scJob) {
	scJobs.Lock()
	defer scJobs.Unlock()
	scJobs.m[j.ID] = j
	// Jobs are in-memory and small, but a long-lived installation should not accumulate
	// them forever: drop anything finished more than an hour ago whenever a new one starts.
	cutoff := time.Now().Add(-time.Hour)
	for id, old := range scJobs.m {
		if old.Status != "running" && !old.End.IsZero() && old.End.Before(cutoff) {
			delete(scJobs.m, id)
		}
	}
}

func scGetJob(id string) *scJob {
	scJobs.Lock()
	defer scJobs.Unlock()
	return scJobs.m[id]
}

// ------------------------------------------------------------------------------- requests

// scRequest is what the page asks for: which sample, against which endpoint, with which TLS
// posture and optionally which client certificate.
type scRequest struct {
	Sample     string `json:"sample"`               // database/language/client/scenario
	Target     string `json:"target"`               // an endpoint id from the targets list
	TLS        string `json:"tls,omitempty"`        // off | require | verify; "" keeps the derived default
	ClientCert string `json:"clientCert,omitempty"` // an Intranet-issued certificate username
	Action     string `json:"action,omitempty"`     // save | prepare | run | reset (runs only)
}

// scGenerated is a rendered sample: the project, the environment plan, and the sentence that says
// what it demonstrates.
type scGenerated struct {
	Sample   string      `json:"sample"`
	Title    string      `json:"title"`
	Explain  string      `json:"explain"`
	Target   scTargetDTO `json:"target"`
	Dir      string      `json:"dir"`
	Files    []scFile    `json:"files"`
	Plan     scPlan      `json:"plan"`
	RunCmd   string      `json:"runCmd"`
	Deps     []scDep     `json:"deps"`
	Requires []string    `json:"requires"`
}

// scBuild resolves a request against a stack and a Linux Client into everything the rest of the
// feature needs. One function so the generate endpoint and the run endpoint cannot disagree about
// what "this sample against that endpoint" means.
func (a *App) scBuild(ctx context.Context, st Stack, dep Deployment, req scRequest) (scClient, scGen, scPlan, error) {
	c, scenario, err := scResolveSample(req.Sample)
	if err != nil {
		return scClient{}, scGen{}, scPlan{}, err
	}
	var cfg linuxClientConfig
	json.Unmarshal(dep.Config, &cfg)
	nodeOS := cfg.OS
	// The release, not just the family. It is read from the design rather than the deployment
	// config because that is where it has always been recorded, so a node deployed before this
	// mattered still answers — and an empty answer only costs the version-specific cases.
	osInfo := scOS{ID: nodeOS, Version: scNodeOSVersion(st, dep.NodeID)}
	// Before the target is even looked up: a client that cannot run on this release is the
	// answer whatever it would have connected to, and it is the answer that should be read.
	if why := scUnsupported(c, osInfo); why != "" {
		return scClient{}, scGen{}, scPlan{}, fmt.Errorf("%s cannot run on this Linux Client: %s", c.Label, why)
	}

	target, err := a.scFindTarget(st, req.Target, nodeOS)
	if err != nil {
		return scClient{}, scGen{}, scPlan{}, err
	}
	if target.Engine != c.Database {
		return scClient{}, scGen{}, scPlan{}, fmt.Errorf(
			"%s speaks %s, and %s is a %s endpoint — pick a client for this database",
			c.Label, c.Database, target.Label, target.Engine)
	}
	target = scApplyTLSChoice(target, req.TLS, nodeOS)
	if req.ClientCert != "" && target.TLS.Mode == scTLSOff {
		return scClient{}, scGen{}, scPlan{}, fmt.Errorf(
			"a client certificate is only presented on a TLS connection — set TLS to Require or Verify, or clear the certificate")
	}

	g := scNewGen(req.Sample, c, scenario, target, nodeOS, req.ClientCert)
	return c, g, scBuildPlan(c, g, osInfo, cfg.UseProxy), nil
}

// scExplain is the short paragraph under the picker: what this example does, and what DBCanvas
// will install to make it run. Two sentences, because a third would not be read.
func scExplain(c scClient, g scGen) string {
	tls := map[string]string{
		scTLSVerify:  " over TLS, verifying the server's certificate against the stack CA",
		scTLSRequire: " over an encrypted but unverified TLS connection",
		scTLSOff:     " over a plaintext connection",
	}[g.Target.TLS.Mode]
	mtls := ""
	if g.MTLS() {
		mtls = ", presenting a client certificate of its own"
	}
	what := "connect to " + g.Target.Label
	if g.Ops.Any() {
		var ops []string
		if g.Ops.Create {
			ops = append(ops, "Create")
		}
		if g.Ops.Read {
			ops = append(ops, "Read")
		}
		if g.Ops.Update {
			ops = append(ops, "Update")
		}
		if g.Ops.Delete {
			ops = append(ops, "Delete")
		}
		if len(ops) > 0 {
			what += " and perform " + strings.Join(ops, ", ")
		}
	}
	req := scRequirementLabels(c)
	install := ""
	if len(req) > 0 {
		install = " DBCanvas installs " + strings.Join(req, ", ") + " on this Linux Client first, and skips whatever is already there."
	}
	return fmt.Sprintf("This example uses %s %s to %s%s%s.%s",
		scLanguageLabel(c.Language), c.Label, what, tls, mtls, install)
}

func scTitle(c scClient, g scGen) string {
	return scLanguageLabel(c.Language) + " — " + c.Label + " · " + g.Scenario.Label
}

// ------------------------------------------------------------------------------- handlers

// handleSampleCodeCatalog is the registry: every database, every client, every scenario, with the
// dependencies and licences each choice implies. Static, so it needs no stack.
func (a *App) handleSampleCodeCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, scCatalog())
}

// scClientNode is one deployed Linux Client the page can generate onto.
type scClientNode struct {
	StackID   int64  `json:"stackId"`
	StackName string `json:"stackName"`
	NodeID    string `json:"nodeId"`
	Label     string `json:"label"`
	FQDN      string `json:"fqdn"`
	OS        string `json:"os"`
	OSVersion string `json:"osVersion"`
	// Targets is how many database endpoints this node's own stack has. A Linux Client in a
	// stack with no running database is still listed — with zero — because "there is nothing
	// to connect to yet" is a more useful answer than an empty picker.
	Targets int `json:"targets"`
}

// handleSampleCodeNodes lists every running Linux Client across the caller's stacks. The page is a
// tool of its own rather than a tab on one node, so — like the Core Dump Analyzer and the Packet
// Inspector — it finds its own hosts.
func (a *App) handleSampleCodeNodes(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	out := []scClientNode{}
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	for _, s := range stacks {
		st, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		doc := buildDoc(st)
		var targets int
		counted := false
		for _, n := range doc.Nodes {
			if n.Type != "linuxclient" {
				continue
			}
			dep, err := a.store.GetDeployment(st.ID, n.ID)
			if err != nil || dep.State != DeployRunning {
				continue
			}
			var cfg linuxClientConfig
			json.Unmarshal(dep.Config, &cfg)
			if !counted {
				targets, counted = len(a.scStackTargets(st, cfg.OS)), true
			}
			out = append(out, scClientNode{
				StackID: st.ID, StackName: st.Name, NodeID: n.ID, Label: n.Label,
				FQDN: cfg.FQDN, OS: cfg.OS, OSVersion: n.OSVersion, Targets: targets,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StackName != out[j].StackName {
			return out[i].StackName < out[j].StackName
		}
		return out[i].Label < out[j].Label
	})
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// scNodeContext resolves the Linux Client a request names, refusing any other node type. The
// refusal is specific because the mistake is easy to make from the API: every node has a terminal,
// but only a Linux Client is the disposable host this feature is allowed to install packages on.
func (a *App) scNodeContext(w http.ResponseWriter, r *http.Request) (Stack, Deployment, bool) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return Stack{}, Deployment{}, false
	}
	st, err := a.store.GetStack(dep.StackID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "stack not found")
		return Stack{}, Deployment{}, false
	}
	for _, n := range buildDoc(st).Nodes {
		if n.ID != dep.NodeID {
			continue
		}
		if n.Type != "linuxclient" {
			writeErr(w, http.StatusBadRequest,
				"Sample Client Code needs a Linux Client node to run on — "+n.Label+" is a "+n.Type+" node")
			return Stack{}, Deployment{}, false
		}
		return st, dep, true
	}
	writeErr(w, http.StatusNotFound, "node not found in this stack")
	return Stack{}, Deployment{}, false
}

// handleSampleCodeTargets lists the database endpoints in this Linux Client's own stack, plus the
// client certificates its Intranet has issued.
func (a *App) handleSampleCodeTargets(w http.ResponseWriter, r *http.Request) {
	st, dep, ok := a.scNodeContext(w, r)
	if !ok {
		return
	}
	var cfg linuxClientConfig
	json.Unmarshal(dep.Config, &cfg)
	targets := a.scStackTargets(st, cfg.OS)
	out := make([]scTargetDTO, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.dto())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"targets":     out,
		"clientCerts": a.scClientCertNames(r.Context(), st),
		"os":          cfg.OS,
		"caPath":      scCAPath(cfg.OS),
		// The catalogue is global; what this node's release cannot run is not. Keyed
		// database/language/client and valued with the reason, so the picker can grey an entry
		// out and say why rather than offer it and fail.
		"unsupported": scUnsupportedOn(scOS{ID: cfg.OS, Version: scNodeOSVersion(st, dep.NodeID)}),
	})
}

// handleSampleCodeGenerate renders a sample without touching the node. A POST because the request
// is a four-part selection with a TLS choice in it, and ReadOnly because nothing changes.
func (a *App) handleSampleCodeGenerate(w http.ResponseWriter, r *http.Request) {
	st, dep, ok := a.scNodeContext(w, r)
	if !ok {
		return
	}
	var req scRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	c, g, plan, err := a.scBuild(r.Context(), st, dep, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, scGenerated{
		Sample: req.Sample, Title: scTitle(c, g), Explain: scExplain(c, g),
		Target: g.Target.dto(), Dir: plan.Dir, Files: plan.Files, Plan: plan,
		RunCmd: plan.Run.Show, Deps: c.Deps, Requires: scRequirementLabels(c),
	})
}

// handleSampleCodeRun starts one job: save the project, prepare the environment, run the sample,
// or reset the project directory.
func (a *App) handleSampleCodeRun(w http.ResponseWriter, r *http.Request) {
	st, dep, ok := a.scNodeContext(w, r)
	if !ok {
		return
	}
	u, _ := a.currentUser(r)
	var req scRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Action {
	case "save", "prepare", "run", "reset":
	default:
		writeErr(w, http.StatusBadRequest, "action must be save, prepare, run or reset")
		return
	}
	// Reset is resolved from the sample id alone. Everything else needs the endpoint, and a
	// reset that refused to run because the database it was generated against has since been
	// deleted would be refusing at exactly the moment someone wants to tidy up.
	c, g, plan, err := a.scBuild(r.Context(), st, dep, req)
	if err != nil {
		if req.Action != "reset" {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, _, _, _, ok := scParseSampleID(req.Sample); !ok {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		c, g, plan = scClient{}, scGen{}, scPlan{Dir: scProjectDir(req.Sample)}
	}

	job := &scJob{
		scJobState: scJobState{
			ID: qrNewID(), StackID: st.ID, NodeID: dep.NodeID, Sample: req.Sample,
			Action: req.Action, Status: "running", Dir: plan.Dir, Start: time.Now().UTC(),
		},
		ownerID: u.ID,
	}
	// The job outlives the request, so it runs on a background context carrying the node's
	// engine — a Vagrant node is not reachable on the Docker engine (see dialEngine).
	ctx, cancel := context.WithTimeout(withEngine(context.Background(), a.depEngine(st, dep.NodeID)), scMaxJobDuration)
	job.cancel = cancel
	scRegisterJob(job)
	go func() {
		defer cancel()
		a.scRunJob(ctx, job, st, dep, c, g, plan, req)
	}()
	writeJSON(w, http.StatusOK, map[string]string{"jobId": job.ID})
}

// handleSampleCodeJob returns a live snapshot of one job.
func (a *App) handleSampleCodeJob(w http.ResponseWriter, r *http.Request) {
	job, ok := a.scOwnedJob(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

// handleSampleCodeStop cancels a running job. The step in flight is not killed inside the node —
// an exec has no cancel — but nothing after it runs, and the job stops being reported as running.
func (a *App) handleSampleCodeStop(w http.ResponseWriter, r *http.Request) {
	job, ok := a.scOwnedJob(w, r)
	if !ok {
		return
	}
	job.finish("canceled", "stopped")
	if job.cancel != nil {
		job.cancel()
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

func (a *App) scOwnedJob(w http.ResponseWriter, r *http.Request) (*scJob, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	job := scGetJob(r.PathValue("jid"))
	if job == nil {
		writeErr(w, http.StatusNotFound, "job not found — it may have finished long enough ago to be forgotten")
		return nil, false
	}
	if job.ownerID != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your job")
		return nil, false
	}
	return job, true
}

// ------------------------------------------------------------------------------- the runner

// scRunJob is the whole of "prepare and run", in the order the log reports it.
//
// Nothing is hidden: every check says what it found, every install says what it is about to run
// before it runs it, and the output of each command follows it. That is not verbosity for its own
// sake — a Linux Client is where someone is learning what a driver needs, and "DBCanvas installed
// something" is a worse answer than the command line they could have typed themselves.
func (a *App) scRunJob(ctx context.Context, job *scJob, st Stack, dep Deployment, c scClient, g scGen, plan scPlan, req scRequest) {
	if req.Action == "reset" {
		job.add("step", "Reset")
		job.add("cmd", "rm -rf "+plan.Dir)
		if res, err := a.scExec(ctx, dep.ContainerID, scStep{Cmd: scResetScript(plan.Dir)}); err != nil || res.Code != 0 {
			job.add("fail", scStepError(res, err))
			job.finish("error", "the project directory could not be removed")
			return
		}
		job.add("ok", "removed "+plan.Dir)
		job.add("info", "the shared caches (the virtualenv, the Go module cache, ~/.m2) were left alone")
		job.finish("done", "reset")
		return
	}

	// 1. System packages — the runtime and any native client.
	if req.Action != "save" && len(plan.System) > 0 {
		job.add("step", "Preparing environment")
		for _, s := range plan.System {
			if !a.scStepSatisfied(ctx, job, dep.ContainerID, s) {
				if !a.scRunStep(ctx, job, dep.ContainerID, s) {
					job.finish("error", s.Label+" could not be installed")
					return
				}
			}
		}
	}

	// 2. The project itself, written by DBCanvas rather than by a shell command — it is a file
	//    copy, and routing it through a heredoc would only make the log harder to read.
	job.add("step", "Writing the project")
	if err := a.scWriteProject(ctx, st, dep, job, g, plan, req.ClientCert); err != nil {
		job.add("fail", err.Error())
		job.finish("error", "the project could not be written to the node")
		return
	}

	// 3. The ecosystem's own dependency resolver, then anything that has to exist beside the
	//    source (the JVM's keystores).
	if req.Action != "save" {
		for _, s := range append(append([]scStep{}, plan.Deps...), plan.Prepare...) {
			if a.scStepSatisfied(ctx, job, dep.ContainerID, s) {
				continue
			}
			if !a.scRunStep(ctx, job, dep.ContainerID, s) {
				job.finish("error", s.Label+" failed")
				return
			}
		}
		job.add("ok", "Environment ready.")
	}

	if req.Action != "run" {
		job.finish("done", map[string]string{
			"save":    "saved to " + plan.Dir,
			"prepare": "environment ready",
		}[req.Action])
		return
	}

	// 4. The program.
	job.add("step", "Running "+c.Label)
	job.add("cmd", "cd "+plan.Dir+" && "+plan.Run.Show)
	res, err := a.scExec(ctx, dep.ContainerID, plan.Run)
	job.setResult(res.Code)
	if res.Stdout != "" {
		job.add("out", res.Stdout)
	}
	if res.Stderr != "" {
		job.add("err", res.Stderr)
	}
	if err != nil {
		job.add("fail", "could not run the sample: "+err.Error())
		job.finish("error", "the sample could not be started")
		return
	}
	if res.Code != 0 {
		// A non-zero exit is the program's answer, not a DBCanvas failure — and the
		// distinction matters, because the interesting case (a write against a read-only
		// endpoint, a TLS mode the server will not accept) lands exactly here.
		job.add("fail", fmt.Sprintf("the sample exited %d — its output above is the reason", res.Code))
		job.finish("error", fmt.Sprintf("exit %d", res.Code))
		return
	}
	job.add("ok", "Finished — exit 0.")
	job.finish("done", "ran successfully")
}

// scStepSatisfied runs a step's check and reports what it found, in the "Checking x... installed"
// form the environment section is built around.
func (a *App) scStepSatisfied(ctx context.Context, job *scJob, containerID string, s scStep) bool {
	if s.Check == "" {
		return false
	}
	res, err := a.scExec(ctx, containerID, scStep{Dir: s.Dir, Cmd: s.Check, Env: s.Env})
	if err == nil && res.Code == 0 {
		job.add("ok", s.Label+": installed")
		return true
	}
	job.add("info", s.Label+": missing")
	return false
}

// scRunStep runs one step and logs the command, its output and its verdict.
func (a *App) scRunStep(ctx context.Context, job *scJob, containerID string, s scStep) bool {
	if s.Show != "" {
		job.add("cmd", s.Show)
	}
	res, err := a.scExec(ctx, containerID, s)
	if out := strings.TrimSpace(res.Stdout); out != "" {
		job.add("out", lastLines(out, 4000))
	}
	if errOut := strings.TrimSpace(res.Stderr); errOut != "" {
		job.add("err", lastLines(errOut, 4000))
	}
	if err != nil || res.Code != 0 {
		job.add("fail", s.Label+" failed: "+scStepError(res, err))
		return false
	}
	job.add("ok", s.Label+": ready")
	return true
}

// scStepError is the sentence a failed step leaves behind: the command's own words where it had
// any, and the transport error where it did not.
func scStepError(res ExecResult, err error) string {
	if err != nil {
		return err.Error()
	}
	for _, s := range []string{res.Stderr, res.Stdout} {
		if t := strings.TrimSpace(s); t != "" {
			return lastLines(t, 400)
		}
	}
	return fmt.Sprintf("exit %d with no output", res.Code)
}

// scExec runs one step inside the node. A login shell, because the packages installed a moment ago
// put their binaries on the PATH a login shell builds (kubectl's profile.d drop-in is the same
// mechanism), and because a sample's run command is written as a person would type it.
func (a *App) scExec(ctx context.Context, containerID string, s scStep) (ExecResult, error) {
	script := s.Cmd
	if s.Dir != "" {
		script = "cd " + s.Dir + " || exit 1\n" + script
	}
	return a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-lc", script}, s.Env)
}

// scWriteProject puts the generated files on the node, plus the client certificate material when
// the sample was asked for mutual TLS.
//
// The certificate comes from the Intranet's own store and goes straight to the Linux Client: the
// private key never passes through the browser, which is the one piece of this feature's data that
// should not. Three forms are written because the drivers disagree about which they want — a PEM
// certificate and key separately, and the two concatenated for the MongoDB drivers.
func (a *App) scWriteProject(ctx context.Context, st Stack, dep Deployment, job *scJob, g scGen, plan scPlan, clientCertUser string) error {
	dirs := map[string]bool{}
	for _, f := range plan.Files {
		if i := strings.LastIndex(f.Name, "/"); i > 0 {
			dirs[plan.Dir+"/"+f.Name[:i]] = true
		}
	}
	mkdir := "set -e\nmkdir -p " + plan.Dir
	for d := range dirs {
		mkdir += " " + d
	}
	if res, err := a.scExec(ctx, dep.ContainerID, scStep{Cmd: mkdir}); err != nil || res.Code != 0 {
		return fmt.Errorf("create %s: %s", plan.Dir, scStepError(res, err))
	}

	for _, f := range plan.Files {
		dir, name := plan.Dir, f.Name
		if i := strings.LastIndex(f.Name, "/"); i > 0 {
			dir, name = plan.Dir+"/"+f.Name[:i], f.Name[i+1:]
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		if err := a.engCtx(ctx).CopyFile(ctx, dep.ContainerID, dir, name, mode, []byte(f.Body)); err != nil {
			return fmt.Errorf("write %s: %v", f.Name, err)
		}
		job.add("ok", "wrote "+plan.Dir+"/"+f.Name)
	}

	if clientCertUser == "" {
		return nil
	}
	cert, key, err := a.scReadClientCert(ctx, st, clientCertUser)
	if err != nil {
		return err
	}
	files := []struct {
		name string
		mode int64
		body []byte
	}{
		{"client-cert.pem", 0o644, cert},
		{"client-key.pem", 0o600, key},
		// The MongoDB drivers take one file holding both. Written here rather than by a
		// shell step so the key never lands in a log line.
		{"client.pem", 0o600, append(append([]byte{}, cert...), key...)},
	}
	for _, f := range files {
		if err := a.engCtx(ctx).CopyFile(ctx, dep.ContainerID, plan.Dir, f.name, f.mode, f.body); err != nil {
			return fmt.Errorf("write %s: %v", f.name, err)
		}
		job.add("ok", "wrote "+plan.Dir+"/"+f.name+" (client certificate for "+clientCertUser+")")
	}
	return nil
}

// scNodeOSVersion is the release of the Linux Client this sample runs on, from the design.
func scNodeOSVersion(st Stack, nodeID string) string {
	for _, n := range buildDoc(st).Nodes {
		if n.ID == nodeID {
			return n.OSVersion
		}
	}
	return ""
}
