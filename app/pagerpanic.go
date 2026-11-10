package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"dbcanvas/internal/pagerpanic"
)

// Pager Panic (Type=="pagerpanic"): an on-call game played against a cluster on the
// canvas. Tickets pop up, a timer runs on each, and the player fixes real faults on the
// real nodes from a terminal. The game runs in DBCanvas's dbcanvas-pagerpanic image
// (`make pagerpanic-image`) and holds no credentials: it asks DBCanvas to set up, check
// and clean up a ticket, and DBCanvas runs that ticket's scripts from a task bank it
// loaded itself, as root, on the nodes of the one frame the game is linked to.
//
// The direction is the one Performance Sim uses (perfsimcond.go): DBCanvas polls the
// game over the stack network, so the game never needs a way into DBCanvas.
const pagerPanicPort = 8097

var pagerPanicImage = "dbcanvas-pagerpanic:" + imageRelease()

// pagerPanicMinNodes is the smallest cluster a bank may ask for. The Kubernetes bank
// needs a control plane and two workers, so one worker can be taken down while the
// other keeps running.
const pagerPanicMinNodes = 3

// pagerPanicConfig is the node's deployment record.
type pagerPanicConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	HTTPPort   int    `json:"httpPort"`
	Frame      string `json:"frame"`
	FrameLabel string `json:"frameLabel"`
	Bank       string `json:"bank,omitempty"`
	// Active are the tickets set up on the cluster and not yet cleaned up. Kept here, not
	// only in memory, so a restart of DBCanvas can still put the cluster back.
	Active []pagerPanicActive `json:"active,omitempty"`
	// Save is the game's own record (high scores, medals, past shifts), which the game
	// container cannot keep across a restart.
	Save json.RawMessage `json:"save,omitempty"`
}

// pagerPanicSecrets holds the token the game accepts syncs with, so a browser on the
// game's page cannot post results of its own.
type pagerPanicSecrets struct {
	Token string `json:"token"`
}

type pagerPanicActive struct {
	Bank     string              `json:"bank"`
	Instance pagerpanic.Instance `json:"instance"`
}

// pagerPanicLink finds the frame a Pager Panic node is linked to.
func pagerPanicLink(doc designDoc, nodeID string) (designFrame, bool) {
	for _, e := range doc.Edges {
		other := ""
		switch nodeID {
		case e.From.Node:
			other = e.To.Node
		case e.To.Node:
			other = e.From.Node
		default:
			continue
		}
		for _, f := range doc.Frames {
			if f.ID == other && f.Type == "kubeadm" {
				return f, true
			}
		}
	}
	return designFrame{}, false
}

func pagerPanicIssues(doc designDoc, n designNode) []issue {
	f, ok := pagerPanicLink(doc, n.ID)
	if !ok {
		return []issue{{Level: "error", Message: "Pager Panic node " + n.Label +
			" needs an association line to a Kubeadm Cluster, the cluster the game plays against."}}
	}
	var out []issue
	members := kubeadmMembers(f, doc)
	if len(members) < pagerPanicMinNodes {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf(
			"Pager Panic needs a Kubeadm Cluster of %d or more nodes; %s has %d.", pagerPanicMinNodes, f.Label, len(members))})
	}
	if f.KubeadmStage != "" {
		out = append(out, issue{Level: "error", Message: "Pager Panic plays against a Kubeadm Cluster at the Ready stage; " +
			f.Label + " is deployed only as far as " + f.KubeadmStage + "."})
	}
	banks, errs := pagerPanicBanks()
	if pagerPanicBankFor(banks, "kubeadm") == nil {
		msg := "Pager Panic found no task bank for a Kubeadm Cluster"
		if len(errs) > 0 {
			msg += ": " + errs[0].Error()
		}
		out = append(out, issue{Level: "error", Message: msg})
	} else {
		for _, err := range errs {
			out = append(out, issue{Level: "warning", Message: "Pager Panic task bank: " + err.Error()})
		}
	}
	out = append(out, issue{Level: "info", Message: "Pager Panic changes the cluster on purpose (it stops services and edits manifests on its nodes); give it a Kubeadm Cluster of its own."})
	return out
}

// ---------------------------------------------------------------- banks

// pagerPanicBankDirs are where banks are read from, first match wins: the directory the
// installation mounts (so a bank can be edited without a rebuild), the copy in the image,
// and the repository's own when running from a checkout.
func pagerPanicBankDirs() []string {
	dirs := []string{}
	if d := os.Getenv("PAGERPANIC_BANKS"); d != "" {
		dirs = append(dirs, d)
	}
	return append(dirs, "/etc/dbcanvas/pagerpanic", "/usr/share/dbcanvas/pagerpanic", "../pagerpanic/banks", "pagerpanic/banks")
}

var pagerPanicBankCache = struct {
	sync.Mutex
	dir   string
	stamp string
	banks map[string]*pagerpanic.Bank
	errs  []error
}{}

// pagerPanicBanks loads the banks, re-reading them only when a file changed, so an edit
// is picked up by the next game without anything being restarted.
func pagerPanicBanks() (map[string]*pagerpanic.Bank, []error) {
	for _, dir := range pagerPanicBankDirs() {
		files, _ := filepath.Glob(filepath.Join(dir, "*.y*ml"))
		if len(files) == 0 {
			continue
		}
		sort.Strings(files)
		stamp := ""
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil {
				stamp += f + strconv.FormatInt(fi.ModTime().UnixNano(), 10) + strconv.FormatInt(fi.Size(), 10)
			}
		}
		c := &pagerPanicBankCache
		c.Lock()
		defer c.Unlock()
		if c.dir != dir || c.stamp != stamp {
			c.banks, c.errs = pagerpanic.LoadDir(dir)
			c.dir, c.stamp = dir, stamp
		}
		return c.banks, c.errs
	}
	return nil, []error{fmt.Errorf("no task bank directory found (%v)", pagerPanicBankDirs())}
}

// pagerPanicBankFor picks the bank for a target: "kubernetes" when there is one, else the
// first by name.
func pagerPanicBankFor(banks map[string]*pagerpanic.Bank, target string) *pagerpanic.Bank {
	if b, ok := banks["kubernetes"]; ok && b.Target == target {
		return b
	}
	names := make([]string, 0, len(banks))
	for k := range banks {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if banks[k].Target == target {
			return banks[k]
		}
	}
	return nil
}

// ---------------------------------------------------------------- the cluster

// pagerPanicNode is one node of the linked cluster, as the scripts and the game see it.
type pagerPanicNode struct {
	Host      string `json:"host"`
	Role      string `json:"role"` // "control-plane" or "worker"
	NodeID    string `json:"nodeId"`
	Container string `json:"container"`
	id        string // the container id
}

// pagerPanicCluster resolves the frame's nodes and what the engine needs to know about it.
func (a *App) pagerPanicCluster(st Stack, doc designDoc, f designFrame) ([]pagerPanicNode, pagerpanic.Env, error) {
	hosts := stackHostnames(doc)
	var nodes []pagerPanicNode
	env := pagerpanic.Env{CNI: "calico", Version: f.KubeadmVersion}
	if f.KubeadmCNI != "" {
		env.CNI = f.KubeadmCNI
	}
	for i, m := range kubeadmMembers(f, doc) {
		dep, err := a.store.GetDeployment(st.ID, m.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			return nil, env, fmt.Errorf("%s is not running", m.Label)
		}
		var cfg kubeadmConfig
		json.Unmarshal(dep.Config, &cfg)
		host := cfg.Hostname
		if host == "" {
			host = hosts[m.ID]
		}
		if cfg.Version != "" {
			env.Version = cfg.Version
		}
		if cfg.CNI != "" {
			env.CNI = cfg.CNI
		}
		role := "worker"
		if i == 0 {
			role = "control-plane"
			env.ControlPlane = host
		} else {
			env.Workers = append(env.Workers, host)
		}
		nodes = append(nodes, pagerPanicNode{Host: host, Role: role, NodeID: m.ID,
			Container: containerName(st.ID, m.ID), id: dep.ContainerID})
	}
	if env.ControlPlane == "" {
		return nil, env, fmt.Errorf("%s has no nodes", f.Label)
	}
	return nodes, env, nil
}

// ---------------------------------------------------------------- provisioning

func (a *App) provisionPagerPanic(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	cfg := pagerPanicConfig{Image: pagerPanicImage, Hostname: host, FQDN: fqdnOf(host, domain)}
	sec := pagerPanicSecrets{Token: randHex(16)}
	secJSON := mustJSON(sec)
	// A redeploy keeps the port, the tickets still on the cluster and the game's record.
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old pagerPanicConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			cfg.HTTPPort, cfg.Active, cfg.Save = old.HTTPPort, old.Active, old.Save
		}
	}
	f, ok := pagerPanicLink(doc, n.ID)
	if !ok {
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError, Config: mustJSON(cfg), Secrets: secJSON})
		return
	}
	cfg.Frame, cfg.FrameLabel = f.ID, f.Label
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg), Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
		if ok, _ := a.engCtx(ctx).ImageExists(ctx, pagerPanicImage); !ok {
			pr.fail("image %s not found — run `make pagerpanic-image` first", pagerPanicImage)
			return
		}
		banks, errs := pagerPanicBanks()
		for _, err := range errs {
			pr.logln("task bank: " + err.Error())
		}
		bank := pagerPanicBankFor(banks, "kubeadm")
		if bank == nil {
			pr.fail("no task bank for a Kubeadm Cluster")
			return
		}
		cfg.Bank = bank.Bank
		pr.phase("Waiting for Intranet to be ready", 10)
		_, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			pr.fail("%v", err)
			return
		}
		pr.phase("Waiting for the Kubeadm Cluster", 25)
		if err := a.waitPagerPanicCluster(ctx, st, doc, f, deployTimeout()); err != nil {
			pr.fail("%v", err)
			return
		}
		pr.phase("Creating container", 55)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: pagerPanicImage, Hostname: host,
			Env:     []string{"PORT=" + strconv.Itoa(pagerPanicPort), "PAGERPANIC_TOKEN=" + sec.Token},
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: pagerPanicPort, HostPort: cfg.HTTPPort, Auto: true}},
			DNS:        []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", pagerPanicPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: mustJSON(cfg), Secrets: secJSON})
		pr.phase("Waiting for the game", 80)
		if err := a.waitPagerPanicHealthy(ctx, id, 90*time.Second); err != nil {
			pr.fail("the game did not start: %v", err)
			return
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg), Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.logln(fmt.Sprintf("game on http://localhost:%d, playing %q against %s", cfg.HTTPPort, bank.Title, f.Label))
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// waitPagerPanicCluster waits for every member of the frame to be running.
func (a *App) waitPagerPanicCluster(ctx context.Context, st Stack, doc designDoc, f designFrame, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, _, err := a.pagerPanicCluster(st, doc, f); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("the Kubeadm Cluster %s is not running: %v", f.Label, err)
		}
		for _, m := range kubeadmMembers(f, doc) {
			if dep, err := a.store.GetDeployment(st.ID, m.ID); err == nil && dep.State == DeployError {
				return fmt.Errorf("%s failed to deploy", m.Label)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// waitPagerPanicHealthy waits for the game to answer. The image is distroless, so the
// check is the binary's own -healthcheck flag.
func (a *App) waitPagerPanicHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/pagerpanic", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("no answer within %s", timeout)
}
