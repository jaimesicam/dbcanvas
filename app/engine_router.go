package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// engine_router.go — one stack, two engines.
//
// A stack whose nodes all run on Docker uses the Docker engine directly. A stack with nodes that
// run as virtual machines uses an engineRouter instead: it sends the creation of a container to
// Lima when the container is one of the stack's VM nodes, and every other call to the engine that
// owns the id or name it is given. Lima's ids start with "dbc-", which a Docker id (hexadecimal)
// never does, so an id alone tells the engines apart. Networks, volumes and images are Docker's:
// a VM joins its stack's network through the Intranet.
//
// The provisioners need no change for this: they already create every node's container through
// the engine their context carries, under the name containerName(stack, node).

// Node runtimes: where a node runs. "" is Docker.
const (
	runtimeDocker = "docker"
	runtimeVM     = "vm"
)

// nodeRuntime is where a node runs: its own runtime, else its cluster's, else Docker.
func nodeRuntime(n designNode, frames map[string]designFrame) string {
	if n.Runtime != "" {
		return n.Runtime
	}
	if f, ok := frames[n.FrameID]; ok && f.Runtime != "" {
		return f.Runtime
	}
	return runtimeDocker
}

// vmNodeIDs is the ids of a design's nodes that run as virtual machines.
func vmNodeIDs(doc designDoc) []string {
	frames := map[string]designFrame{}
	for _, f := range doc.Frames {
		frames[f.ID] = f
	}
	var ids []string
	for _, n := range doc.Nodes {
		if nodeRuntime(n, frames) == runtimeVM {
			ids = append(ids, n.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// stackVMNames is the container names of a stack's VM nodes.
func stackVMNames(st Stack) map[string]bool {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return nil
	}
	ids := vmNodeIDs(doc)
	if len(ids) == 0 {
		return nil
	}
	names := make(map[string]bool, len(ids))
	for _, id := range ids {
		names[containerName(st.ID, id)] = true
	}
	return names
}

// vmPreviewTypes is DBCANVAS_VM_PREVIEW: node types allowed to run as a VM before their own
// support is switched on (vmNodeTypes), for working on that support.
func vmPreviewTypes() map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Split(os.Getenv("DBCANVAS_VM_PREVIEW"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			out[t] = true
		}
	}
	return out
}

// vmNodeTypes are the node and cluster types whose provisioning is verified on a virtual
// machine: on Oracle Linux with SELinux enforcing and on Ubuntu with AppArmor, alone and in a
// cluster with Docker members. A type is added here once it passes; until then a node of it
// cannot run as a VM.
var vmNodeTypes = map[string]bool{}

// vmCapableType reports whether a node or cluster of this type can run as a VM at all: the ones
// installed from packages on the dbcanvas-systemd OS images. Images that arrive ready-made (the
// Intranet, the desktop, PMM, Keycloak, the sims) and K3D clusters cannot.
func vmCapableType(typ string) bool {
	if _, k8s := kubernetesFrames[typ]; k8s {
		return false
	}
	return typ != "vnc" && platformFollower(typ)
}

// vmTypeEnabled reports whether a node or cluster of this type may run as a VM now.
func vmTypeEnabled(typ string) bool {
	return vmCapableType(typ) && (vmNodeTypes[typ] || vmPreviewTypes()[typ])
}

// vmEnabled reports whether any node type may run as a VM; until one may, the runtime setting
// stays out of the editor.
func vmEnabled() bool {
	for t := range vmNodeTypes {
		if vmCapableType(t) {
			return true
		}
	}
	for t := range vmPreviewTypes() {
		if vmCapableType(t) {
			return true
		}
	}
	return false
}

// runtimeIssues checks every node's and cluster's runtime. limaWhy is why VMs cannot run on this
// machine, or "" when they can.
func runtimeIssues(doc designDoc, limaWhy string) []issue {
	var out []issue
	frames := map[string]designFrame{}
	check := func(what, typ, rt string) {
		switch rt {
		case "", runtimeDocker:
			return
		case runtimeVM:
		default:
			out = append(out, issue{Level: "error", Message: what + `: runtime "` + rt + `" is not docker or vm`})
			return
		}
		switch {
		case !vmCapableType(typ):
			out = append(out, issue{Level: "error", Message: what + ": this node type cannot run as a VM, only in a container"})
		case !vmTypeEnabled(typ):
			out = append(out, issue{Level: "error", Message: what + ": running this node type as a VM is not available yet"})
		case limaWhy != "":
			out = append(out, issue{Level: "error", Message: what + ": cannot run as a VM on this machine: " + limaWhy})
		}
	}
	for _, f := range doc.Frames {
		frames[f.ID] = f
		check(firstNonEmpty(f.Label, f.ID), f.Type, f.Runtime)
	}
	for _, n := range doc.Nodes {
		rt := n.Runtime
		if rt == "" {
			continue // a member without its own runtime follows its cluster, checked above
		}
		check(firstNonEmpty(n.Label, n.ID), n.Type, rt)
	}
	if len(vmNodeIDs(doc)) > 0 {
		hasIntranet := false
		for _, n := range doc.Nodes {
			hasIntranet = hasIntranet || n.Type == "intranet"
		}
		if !hasIntranet {
			out = append(out, issue{Level: "error", Message: "nodes that run as VMs reach the stack network through the Intranet: add an Intranet node"})
		}
	}
	return out
}

// vmUnavailable says why nodes cannot run as VMs on this machine, or "" when they can.
func (a *App) vmUnavailable() string {
	if a.lima != nil {
		return ""
	}
	return firstNonEmpty(a.limaWhy, "limactl is not installed (https://lima-vm.io)")
}

// engineRouter is the Engine of a stack with VM nodes. vm holds the container names that run as
// VMs; nil sends every creation to Docker (the router then only tells ids apart).
type engineRouter struct {
	app    *App // joins a new VM to its stack's network and keeps its link (vmnet.go)
	docker *Docker
	lima   *Lima
	vm     map[string]bool
}

func isLimaID(id string) bool { return strings.HasPrefix(id, limaPrefix) }

func (r *engineRouter) byID(id string) Engine {
	if isLimaID(id) {
		return r.lima
	}
	return r.docker
}

func (r *engineRouter) byName(name string) Engine {
	if r.vm[name] {
		return r.lima
	}
	return r.docker
}

// dockerFor is the Docker engine when id is a container, nil when it is a VM: for the features
// only containers have.
func (r *engineRouter) dockerFor(id string) *Docker {
	if isLimaID(id) {
		return nil
	}
	return r.docker
}

func (r *engineRouter) Ping(ctx context.Context) error      { return r.docker.Ping(ctx) }
func (r *engineRouter) HostArch(ctx context.Context) string { return r.docker.HostArch(ctx) }
func (r *engineRouter) HostResources(ctx context.Context) (int, int64) {
	return r.docker.HostResources(ctx)
}
func (r *engineRouter) ImageExists(ctx context.Context, ref string) (bool, error) {
	return r.docker.ImageExists(ctx, ref)
}
func (r *engineRouter) EnsureImage(ctx context.Context, repo, tag, platform string) error {
	return r.docker.EnsureImage(ctx, repo, tag, platform)
}
func (r *engineRouter) ImageRemove(ctx context.Context, ref string) { r.docker.ImageRemove(ctx, ref) }

func (r *engineRouter) NetworkEnsure(ctx context.Context, name string) error {
	return r.docker.NetworkEnsure(ctx, name)
}
func (r *engineRouter) NetworkRemove(ctx context.Context, name string) {
	r.docker.NetworkRemove(ctx, name)
}
func (r *engineRouter) NetworkConnect(ctx context.Context, network, container string) error {
	return r.byID(container).NetworkConnect(ctx, network, container)
}
func (r *engineRouter) NetworkDisconnect(ctx context.Context, network, container string) {
	r.byID(container).NetworkDisconnect(ctx, network, container)
}
func (r *engineRouter) NetworkSubnet(ctx context.Context, name string) (string, error) {
	return r.docker.NetworkSubnet(ctx, name)
}
func (r *engineRouter) VolumeCreate(ctx context.Context, name string) error {
	return r.docker.VolumeCreate(ctx, name)
}
func (r *engineRouter) VolumeRemove(ctx context.Context, name string) {
	r.docker.VolumeRemove(ctx, name)
}

// ContainerCreate makes a VM node's VM and puts it on its stack's network before returning, so
// the node's provisioning finds it where it would find a container.
func (r *engineRouter) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	if !r.vm[spec.Name] {
		return r.docker.ContainerCreate(ctx, spec)
	}
	id, err := r.lima.ContainerCreate(ctx, spec)
	if err != nil {
		return "", err
	}
	if r.app != nil {
		if err := r.app.joinVM(ctx, id, spec); err != nil {
			r.app.stopVMLink(id)
			r.lima.ContainerRemove(ctx, id)
			return "", err
		}
	}
	return id, nil
}

// ContainerStart starts a VM and waits for its link to the stack network.
func (r *engineRouter) ContainerStart(ctx context.Context, id string) error {
	if !isLimaID(id) {
		return r.docker.ContainerStart(ctx, id)
	}
	if err := r.lima.ContainerStart(ctx, id); err != nil {
		return err
	}
	if r.app == nil {
		return nil
	}
	return r.app.ensureVMLink(id).wait(ctx, 2*time.Minute)
}

func (r *engineRouter) ContainerStop(ctx context.Context, id string) error {
	if isLimaID(id) && r.app != nil {
		r.app.stopVMLink(id)
	}
	return r.byID(id).ContainerStop(ctx, id)
}

func (r *engineRouter) ContainerRestart(ctx context.Context, id string) error {
	if !isLimaID(id) {
		return r.docker.ContainerRestart(ctx, id)
	}
	if err := r.ContainerStop(ctx, id); err != nil {
		return err
	}
	return r.ContainerStart(ctx, id)
}

func (r *engineRouter) ContainerRemove(ctx context.Context, id string) {
	if isLimaID(id) && r.app != nil {
		r.app.stopVMLink(id)
	}
	r.byID(id).ContainerRemove(ctx, id)
}
func (r *engineRouter) ContainerUpdate(ctx context.Context, id string, nanoCPUs, memBytes int64) error {
	return r.byID(id).ContainerUpdate(ctx, id, nanoCPUs, memBytes)
}

// ContainerByName looks on the engine the name belongs to, then on the other: a node whose
// runtime changed since its last deploy still has its old container or VM to find and remove.
func (r *engineRouter) ContainerByName(ctx context.Context, name string) (string, bool, error) {
	first, second := Engine(r.docker), Engine(r.lima)
	if r.vm[name] {
		first, second = second, first
	}
	if id, ok, err := first.ContainerByName(ctx, name); ok || err != nil {
		return id, ok, err
	}
	return second.ContainerByName(ctx, name)
}

func (r *engineRouter) ContainersByNamePrefix(ctx context.Context, prefix string) ([]string, error) {
	ids, err := r.docker.ContainersByNamePrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}
	vms, _ := r.lima.ContainersByNamePrefix(ctx, prefix)
	return append(ids, vms...), nil
}

func (r *engineRouter) ContainerIP(ctx context.Context, id, network string) (string, error) {
	return r.byID(id).ContainerIP(ctx, id, network)
}
func (r *engineRouter) ContainerPort(ctx context.Context, id, portProto string) (string, error) {
	return r.byID(id).ContainerPort(ctx, id, portProto)
}
func (r *engineRouter) ContainerPorts(ctx context.Context, id string) (map[int]int, error) {
	return r.byID(id).ContainerPorts(ctx, id)
}
func (r *engineRouter) ListPublishedPorts(ctx context.Context) (map[int]string, error) {
	return r.docker.ListPublishedPorts(ctx)
}
func (r *engineRouter) ListManaged(ctx context.Context) ([]ContainerInfo, error) {
	list, err := r.docker.ListManaged(ctx)
	if err != nil {
		return nil, err
	}
	vms, _ := r.lima.ListManaged(ctx)
	return append(list, vms...), nil
}
func (r *engineRouter) ContainerStats(ctx context.Context, id string) (ContainerStat, error) {
	return r.byID(id).ContainerStats(ctx, id)
}
func (r *engineRouter) WaitSystemd(ctx context.Context, id string, timeout time.Duration) error {
	return r.byID(id).WaitSystemd(ctx, id, timeout)
}

func (r *engineRouter) Exec(ctx context.Context, id string, cmd []string, env []string) (ExecResult, error) {
	return r.byID(id).Exec(ctx, id, cmd, env)
}
func (r *engineRouter) ExecAs(ctx context.Context, id, user string, cmd []string, env []string) (ExecResult, error) {
	return r.byID(id).ExecAs(ctx, id, user, cmd, env)
}
func (r *engineRouter) ExecInput(ctx context.Context, id, user string, cmd, env []string, stdin []byte) (ExecResult, error) {
	return r.byID(id).ExecInput(ctx, id, user, cmd, env, stdin)
}
func (r *engineRouter) CopyFile(ctx context.Context, id, dir, name string, mode int64, content []byte) error {
	return r.byID(id).CopyFile(ctx, id, dir, name, mode, content)
}
func (r *engineRouter) PutArchive(ctx context.Context, id, dir string, tarball []byte) error {
	return r.byID(id).PutArchive(ctx, id, dir, tarball)
}
func (r *engineRouter) PutArchiveStream(ctx context.Context, id, dir string, rd io.Reader) error {
	return r.byID(id).PutArchiveStream(ctx, id, dir, rd)
}
func (r *engineRouter) GetArchiveStream(ctx context.Context, id, path string) (io.ReadCloser, error) {
	return r.byID(id).GetArchiveStream(ctx, id, path)
}
func (r *engineRouter) HijackExec(ctx context.Context, id string, cmd, env []string, user string) (*ExecConn, error) {
	return r.byID(id).HijackExec(ctx, id, cmd, env, user)
}
func (r *engineRouter) ResizeExec(ctx context.Context, execID string, w, h int) error {
	if strings.HasPrefix(execID, "lima-term-") {
		return r.lima.ResizeExec(ctx, execID, w, h)
	}
	return r.docker.ResizeExec(ctx, execID, w, h)
}

var _ Engine = (*engineRouter)(nil)

// asDocker is the Docker engine behind e for the container id, or nil when e is not Docker or
// id is a VM: for the features only containers have (published ports, cgroups, the Docker API
// tunnel).
func asDocker(e Engine, id string) *Docker {
	switch v := e.(type) {
	case *Docker:
		return v
	case *engineRouter:
		return v.dockerFor(id)
	}
	return nil
}
