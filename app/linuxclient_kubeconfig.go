package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// linuxclient_kubeconfig.go — every Kubernetes cluster in the stack, in one kubeconfig on a Linux
// Client, with the current context chosen from the node's panel.
//
// This is done after the deploy, on request, and not by the provisioner: the K3D frames come up
// concurrently with the client node, so copying a kubeconfig at deploy time would be a race (see
// linuxclient_k8s.go). Once the clusters are running it is not, and doing it by hand meant pasting
// one admin kubeconfig per cluster and renaming three entries in each so they could coexist.
//
// That renaming is the whole point of merging rather than copying. k3s names its cluster, user and
// context all "default", so two clusters' configs collide on every name. Each cluster's entries are
// renamed after its frame (the label a person sees on the canvas), which makes them unique within
// the stack and makes `kubectl config use-context k3d-01` say which cluster it means.
//
// The file is merged, not replaced. Entries this writes are the ones named after a K3D frame; any
// other context already in ~/.kube/config — a role-scoped one from the frame's Users tab, a cluster
// somewhere else — is kept. A file that cannot be parsed is kept as config.bak.<time> rather than
// overwritten, since it is somebody's.
//
// The `server:` of every entry is the cluster's serverlb on the stack network (k3dFetchKubeconfig),
// which the client reaches by container name through the Intranet's resolver.

const lcKubeDir = "/root/.kube"
const lcKubeConfig = lcKubeDir + "/config"

// kubeconfig is the part of the format this reads and writes. Cluster and user bodies are kept as
// opaque maps: they carry certificates and fields this has no business interpreting.
type kubeconfig struct {
	APIVersion     string         `json:"apiVersion"`
	Kind           string         `json:"kind"`
	Clusters       []kcCluster    `json:"clusters"`
	Contexts       []kcContext    `json:"contexts"`
	Users          []kcUser       `json:"users"`
	CurrentContext string         `json:"current-context"`
	Preferences    map[string]any `json:"preferences"`
}
type kcCluster struct {
	Name    string         `json:"name"`
	Cluster map[string]any `json:"cluster"`
}
type kcContext struct {
	Name    string        `json:"name"`
	Context kcContextBody `json:"context"`
}
type kcContextBody struct {
	Cluster   string `json:"cluster"`
	User      string `json:"user"`
	Namespace string `json:"namespace,omitempty"`
}
type kcUser struct {
	Name string         `json:"name"`
	User map[string]any `json:"user"`
}

func parseKubeconfig(raw string) (kubeconfig, error) {
	var kc kubeconfig
	if err := yaml.Unmarshal([]byte(raw), &kc); err != nil {
		return kubeconfig{}, err
	}
	return kc, nil
}

func (kc kubeconfig) render() ([]byte, error) {
	if kc.APIVersion == "" {
		kc.APIVersion = "v1"
	}
	if kc.Kind == "" {
		kc.Kind = "Config"
	}
	if kc.Preferences == nil {
		kc.Preferences = map[string]any{}
	}
	return yaml.Marshal(kc)
}

func (kc kubeconfig) hasContext(name string) bool {
	for _, c := range kc.Contexts {
		if c.Name == name {
			return true
		}
	}
	return false
}

// renameKubeconfig gives a single-cluster kubeconfig's cluster, user and context one name. It
// takes the entry its own current-context points at, falling back to the first of each.
func renameKubeconfig(kc kubeconfig, name string) (kubeconfig, error) {
	if len(kc.Clusters) == 0 || len(kc.Users) == 0 {
		return kubeconfig{}, fmt.Errorf("kubeconfig has no cluster or no user")
	}
	ctx := kcContext{Context: kcContextBody{Cluster: kc.Clusters[0].Name, User: kc.Users[0].Name}}
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext || ctx.Name == "" {
			ctx = c
		}
	}
	cl, us := kc.Clusters[0], kc.Users[0]
	for _, c := range kc.Clusters {
		if c.Name == ctx.Context.Cluster {
			cl = c
		}
	}
	for _, u := range kc.Users {
		if u.Name == ctx.Context.User {
			us = u
		}
	}
	return kubeconfig{
		Clusters: []kcCluster{{Name: name, Cluster: cl.Cluster}},
		Users:    []kcUser{{Name: name, User: us.User}},
		Contexts: []kcContext{{Name: name, Context: kcContextBody{Cluster: name, User: name, Namespace: ctx.Context.Namespace}}},
	}, nil
}

// mergeKubeconfigs lays `ours` (one entry set per cluster, already renamed) over `existing`:
// an entry of ours replaces one of the same name, everything else in existing is kept, and the
// current context is `want` when given, else whatever existing had if it survived, else the first
// of ours.
func mergeKubeconfigs(existing kubeconfig, ours []kubeconfig, want string) kubeconfig {
	mine := map[string]bool{}
	for _, o := range ours {
		for _, c := range o.Contexts {
			mine[c.Name] = true
		}
	}
	out := kubeconfig{APIVersion: existing.APIVersion, Kind: existing.Kind, Preferences: existing.Preferences}
	for _, c := range existing.Clusters {
		if !mine[c.Name] {
			out.Clusters = append(out.Clusters, c)
		}
	}
	for _, u := range existing.Users {
		if !mine[u.Name] {
			out.Users = append(out.Users, u)
		}
	}
	for _, c := range existing.Contexts {
		if !mine[c.Name] {
			out.Contexts = append(out.Contexts, c)
		}
	}
	for _, o := range ours {
		out.Clusters = append(out.Clusters, o.Clusters...)
		out.Users = append(out.Users, o.Users...)
		out.Contexts = append(out.Contexts, o.Contexts...)
	}
	switch {
	case want != "" && out.hasContext(want):
		out.CurrentContext = want
	case existing.CurrentContext != "" && out.hasContext(existing.CurrentContext):
		out.CurrentContext = existing.CurrentContext
	case len(ours) > 0 && len(ours[0].Contexts) > 0:
		out.CurrentContext = ours[0].Contexts[0].Name
	}
	return out
}

// lcKubeContextName is the name a K3D frame's entries get: its label, as a kube-safe name.
func lcKubeContextName(f designFrame) string { return sanitizeName(f.Label) }

// lcKubeCluster is one K3D frame as the client's panel sees it.
type lcKubeCluster struct {
	FrameID string `json:"frameId"`
	Context string `json:"context"`
	Running bool   `json:"running"`
	Server  string `json:"server,omitempty"` // the serverlb address the kubeconfig points at
}

// lcKubeContext is one context in the node's kubeconfig.
type lcKubeContext struct {
	Name    string `json:"name"`
	Server  string `json:"server,omitempty"`
	Managed bool   `json:"managed"` // named after a K3D frame in this stack
}

// lcKubeState is what the panel shows: the clusters the stack has, and the kubeconfig on the node.
type lcKubeState struct {
	Clusters []lcKubeCluster `json:"clusters"`
	Exists   bool            `json:"exists"`
	Contexts []lcKubeContext `json:"contexts"`
	Current  string          `json:"current"`
	Note     string          `json:"note,omitempty"`
}

// lcKubeFrames lists the stack's K3D frames, each with its running server's deployment if any.
func (a *App) lcKubeFrames(st Stack, doc designDoc) ([]designFrame, map[string]Deployment) {
	var frames []designFrame
	servers := map[string]Deployment{}
	for _, f := range doc.Frames {
		if f.Type != "k3d" {
			continue
		}
		frames = append(frames, f)
		if _, dep, err := a.k3dFrameAndServer(st, doc, f.ID); err == nil {
			servers[f.ID] = dep
		}
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].Label < frames[j].Label })
	return frames, servers
}

// lcReadKubeconfig returns the node's kubeconfig, "" when there is none.
func (a *App) lcReadKubeconfig(ctx context.Context, containerID string) (string, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", `[ -f "$F" ] && cat "$F" || true`}, []string{"F=" + lcKubeConfig})
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("read %s: %s", lcKubeConfig, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// lcWriteKubeconfig writes the node's kubeconfig: root-owned, 0700 directory, 0600 file — kubectl
// warns about a group- or world-readable config, and this one holds cluster-admin credentials.
func (a *App) lcWriteKubeconfig(ctx context.Context, containerID string, body []byte) error {
	if res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", `install -d -m 0700 -o root -g root "$D"`}, []string{"D=" + lcKubeDir}); err != nil {
		return err
	} else if res.Code != 0 {
		return fmt.Errorf("create %s: %s", lcKubeDir, strings.TrimSpace(res.Stderr))
	}
	if err := a.engCtx(ctx).CopyFile(ctx, containerID, lcKubeDir, "config", 0o600, body); err != nil {
		return fmt.Errorf("write %s: %w", lcKubeConfig, err)
	}
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", `chown root:root "$F" && chmod 0600 "$F"`}, []string{"F=" + lcKubeConfig})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("chmod %s: %s", lcKubeConfig, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// lcKubeStateOf describes a kubeconfig against the stack's frames.
func lcKubeStateOf(raw string, frames []designFrame, servers map[string]Deployment, st Stack) lcKubeState {
	state := lcKubeState{Clusters: []lcKubeCluster{}, Contexts: []lcKubeContext{}}
	managed := map[string]bool{}
	for _, f := range frames {
		_, running := servers[f.ID]
		name := lcKubeContextName(f)
		managed[name] = true
		state.Clusters = append(state.Clusters, lcKubeCluster{FrameID: f.ID, Context: name, Running: running,
			Server: k3dServerLBAddr(k3dClusterName(st.ID, f))})
	}
	if strings.TrimSpace(raw) == "" {
		return state
	}
	state.Exists = true
	kc, err := parseKubeconfig(raw)
	if err != nil {
		state.Note = "~/.kube/config is not a kubeconfig this can read; copying will keep it as a backup"
		return state
	}
	servers2 := map[string]string{}
	for _, c := range kc.Clusters {
		if s, ok := c.Cluster["server"].(string); ok {
			servers2[c.Name] = s
		}
	}
	for _, c := range kc.Contexts {
		state.Contexts = append(state.Contexts, lcKubeContext{Name: c.Name, Server: servers2[c.Context.Cluster], Managed: managed[c.Name]})
	}
	state.Current = kc.CurrentContext
	return state
}

// lcKubeRequest resolves what every handler below needs: the owned stack, its design, and the
// running Linux Client node.
func (a *App) lcKubeRequest(w http.ResponseWriter, r *http.Request) (Stack, designDoc, Deployment, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, designDoc{}, Deployment{}, false
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return Stack{}, designDoc{}, Deployment{}, false
	}
	nid := r.PathValue("nid")
	if nodeTypeOf(st, nid) != "linuxclient" {
		writeErr(w, http.StatusBadRequest, "only a Linux Client node takes a kubeconfig")
		return Stack{}, designDoc{}, Deployment{}, false
	}
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return Stack{}, designDoc{}, Deployment{}, false
	}
	a.stampEngine(r, st, nid)
	dep = a.reconcileContainerID(r.Context(), st.ID, nid, dep)
	return st, doc, dep, true
}

// GET — the stack's clusters and the node's kubeconfig as it stands.
func (a *App) handleLinuxClientKubeconfig(w http.ResponseWriter, r *http.Request) {
	st, doc, dep, ok := a.lcKubeRequest(w, r)
	if !ok {
		return
	}
	frames, servers := a.lcKubeFrames(st, doc)
	raw, err := a.lcReadKubeconfig(r.Context(), dep.ContainerID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, lcKubeStateOf(raw, frames, servers, st))
}

// POST — copy every running K3D cluster's admin kubeconfig into the node's, merged.
// Body: {"context": "<name>"} to also make that one current (optional).
func (a *App) handleLinuxClientKubeconfigCopy(w http.ResponseWriter, r *http.Request) {
	st, doc, dep, ok := a.lcKubeRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Context string `json:"context"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	frames, servers := a.lcKubeFrames(st, doc)
	if len(servers) == 0 {
		writeErr(w, http.StatusConflict, "no Kubernetes cluster in this stack is running")
		return
	}

	var ours []kubeconfig
	skipped := []string{}
	for _, f := range frames {
		sdep, running := servers[f.ID]
		if !running {
			skipped = append(skipped, f.Label+" (not running)")
			continue
		}
		// The cluster's server node may live on another engine than the client: run kubectl there.
		kctx := withEngine(r.Context(), a.depEngine(st, sdep.NodeID))
		raw, err := a.k3dFetchKubeconfig(kctx, sdep.ContainerID, k3dClusterName(st.ID, f))
		if err != nil {
			skipped = append(skipped, f.Label+" ("+lastLines(err.Error(), 120)+")")
			continue
		}
		kc, err := parseKubeconfig(raw)
		if err == nil {
			kc, err = renameKubeconfig(kc, lcKubeContextName(f))
		}
		if err != nil {
			skipped = append(skipped, f.Label+" ("+err.Error()+")")
			continue
		}
		ours = append(ours, kc)
	}
	if len(ours) == 0 {
		writeErr(w, http.StatusBadGateway, "could not read a kubeconfig from any cluster: "+strings.Join(skipped, "; "))
		return
	}

	ctx := r.Context()
	existingRaw, err := a.lcReadKubeconfig(ctx, dep.ContainerID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	existing := kubeconfig{}
	backup := ""
	if strings.TrimSpace(existingRaw) != "" {
		if kc, perr := parseKubeconfig(existingRaw); perr == nil {
			existing = kc
		} else {
			// Not ours to throw away: keep it beside the new one.
			backup = fmt.Sprintf("%s.bak.%s", lcKubeConfig, time.Now().UTC().Format("20060102T150405Z"))
			if res, e := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"cp", "-p", lcKubeConfig, backup}, nil); e != nil || res.Code != 0 {
				writeErr(w, http.StatusBadGateway, "could not back up the existing "+lcKubeConfig)
				return
			}
		}
	}
	if body.Context != "" && !existing.hasContext(body.Context) {
		found := false
		for _, o := range ours {
			found = found || o.hasContext(body.Context)
		}
		if !found {
			writeErr(w, http.StatusBadRequest, "no context named "+body.Context)
			return
		}
	}
	merged := mergeKubeconfigs(existing, ours, body.Context)
	out, err := merged.render()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "render kubeconfig: "+err.Error())
		return
	}
	if err := a.lcWriteKubeconfig(ctx, dep.ContainerID, out); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	state := lcKubeStateOf(string(out), frames, servers, st)
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "copied": len(ours), "skipped": skipped, "backup": backup})
}

// PUT — make one of the node's contexts current. Done on the file, not with kubectl, so it works
// on a client that has Helm but no kubectl, and so the panel and the file cannot disagree.
func (a *App) handleLinuxClientKubeContext(w http.ResponseWriter, r *http.Request) {
	st, doc, dep, ok := a.lcKubeRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Context string `json:"context"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Context) == "" {
		writeErr(w, http.StatusBadRequest, "context is required")
		return
	}
	ctx := r.Context()
	raw, err := a.lcReadKubeconfig(ctx, dep.ContainerID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if strings.TrimSpace(raw) == "" {
		writeErr(w, http.StatusConflict, "this node has no "+lcKubeConfig+" yet — copy the clusters' kubeconfigs first")
		return
	}
	kc, err := parseKubeconfig(raw)
	if err != nil {
		writeErr(w, http.StatusConflict, lcKubeConfig+" is not a kubeconfig this can read")
		return
	}
	if !kc.hasContext(body.Context) {
		writeErr(w, http.StatusBadRequest, "no context named "+body.Context)
		return
	}
	kc.CurrentContext = body.Context
	out, err := kc.render()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "render kubeconfig: "+err.Error())
		return
	}
	if err := a.lcWriteKubeconfig(ctx, dep.ContainerID, out); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	frames, servers := a.lcKubeFrames(st, doc)
	writeJSON(w, http.StatusOK, lcKubeStateOf(string(out), frames, servers, st))
}
