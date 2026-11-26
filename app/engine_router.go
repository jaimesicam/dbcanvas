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

// SELinux modes of a VM node.
const (
	selinuxEnforcing  = "enforcing"
	selinuxPermissive = "permissive"
)

// vmSELinuxPermissive are the node types whose VMs run SELinux permissive unless set otherwise:
// All-in-One, whose many instances use ports and folders no policy knows, and Valkey, which ships
// no SELinux policy of its own — the redis policy it runs under keeps a cluster node from opening
// its nodes.conf.
var vmSELinuxPermissive = map[string]bool{"aio": true, "valkey": true, "valkeycluster": true}

// vmSELinuxDefault is the SELinux mode a VM node of this type runs in unless set otherwise.
func vmSELinuxDefault(typ string) string {
	if vmSELinuxPermissive[typ] {
		return selinuxPermissive
	}
	return selinuxEnforcing
}

// nodeSELinux is the SELinux mode a VM node runs in: its own, else its cluster's, else its type's
// default.
func nodeSELinux(n designNode, frames map[string]designFrame) string {
	if n.SELinux != "" {
		return n.SELinux
	}
	if f, ok := frames[n.FrameID]; ok && f.SELinux != "" {
		return f.SELinux
	}
	if f, ok := frames[n.FrameID]; ok {
		return vmSELinuxDefault(f.Type)
	}
	return vmSELinuxDefault(n.Type)
}

// stackVMSELinux is the SELinux mode of each of a stack's VM nodes, by container name.
func stackVMSELinux(st Stack) map[string]string {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return nil
	}
	frames := map[string]designFrame{}
	for _, f := range doc.Frames {
		frames[f.ID] = f
	}
	out := map[string]string{}
	for _, n := range doc.Nodes {
		if nodeRuntime(n, frames) == runtimeVM {
			out[containerName(st.ID, n.ID)] = nodeSELinux(n, frames)
		}
	}
	return out
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
// cluster with Docker members, and across a reboot. A type is added here once it passes; until
// then a node of it cannot run as a VM. docs/VM_NODES.md lists them.
var vmNodeTypes = map[string]bool{
	"pxc":     true, // Percona XtraDB Cluster: the cluster and its members
	"ps":      true, // Percona Server for MySQL, standalone
	"mysql":   true, // Percona Server asynchronous replication: the cluster and its members
	"pg":      true, // Percona Distribution for PostgreSQL, standalone
	"patroni": true, // Patroni with etcd: the cluster and its members
	"psm":     true, // Percona Server for MongoDB, standalone
	"psmrs":   true, // a PSMDB replica set: the cluster and its members
	"psmdb":   true, // a sharded PSMDB cluster: the cluster and its members

	"proxysql":      true, // ProxySQL, standalone or a ProxySQL cluster and its members
	"orchestrator":  true, // Percona Orchestrator
	"haproxy":       true, // HAProxy
	"pgbouncer":     true, // PgBouncer
	"repmgr":        true, // repmgr: the cluster and its members
	"valkey":        true, // Valkey, standalone (SELinux permissive by default)
	"valkeycluster": true, // a Valkey cluster or Valkey with Sentinel, and its members (likewise)

	"mariadb":       true, // MariaDB, standalone
	"mariadbgalera": true, // MariaDB Galera Cluster: the cluster and its members
	"mariadbrepl":   true, // MariaDB asynchronous replication: the cluster and its members
	"mysqlce":       true, // MySQL Community Server, standalone
	"mysqlcerepl":   true, // MySQL Community asynchronous replication: the cluster and its members
	"mysqlceinnodb": true, // MySQL Community InnoDB Cluster: the cluster and its members
	"innodb":        true, // Percona InnoDB Cluster: the cluster and its members
	"spock":         true, // PostgreSQL with Spock: the cluster and its members
	"pgstream":      true, // PostgreSQL streaming replication: the cluster and its members
	"openbao":       true, // OpenBao
	"sambaad":       true, // Samba AD DC
	"kubeadm":       true, // a Kubeadm cluster and its nodes
	"aio":           true, // All in One (SELinux permissive by default)
	"linuxclient":   true, // the Linux client
}

// vmCapableType reports whether a node or cluster of this type can run as a VM at all: the ones
// installed from packages on the dbcanvas-systemd OS images. Images that arrive ready-made (the
// Intranet, the desktop, PMM, Keycloak, the sims) and K3D clusters cannot.
func vmCapableType(typ string) bool {
	if typ == "kubeadm" {
		return true // installed from packages at deploy, on a VM prepared as its image is
	}
	if _, k8s := kubernetesFrames[typ]; k8s || typ == "vnc" {
		return false
	}
	if k, ok := composeKindByType(typ); ok {
		return !k.ImageOnly
	}
	// Package-built types that compose does not build.
	return vmPackageTypes[typ]
}

// vmPackageTypes are the package-built node types outside compose's kinds that could run as a VM.
var vmPackageTypes = map[string]bool{"aio": true, "linuxclient": true}

// vmTypeEnabled reports whether a node or cluster of this type may run as a VM now.
func vmTypeEnabled(typ string) bool {
	return vmCapableType(typ) && (vmNodeTypes[typ] || vmPreviewTypes()[typ])
}

// vmEnabledTypes is the node and cluster types that may run as a VM now, sorted.
func vmEnabledTypes() []string {
	var out []string
	seen := map[string]bool{}
	for _, set := range []map[string]bool{vmNodeTypes, vmPreviewTypes()} {
		for t, on := range set {
			if on && !seen[t] && vmCapableType(t) {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}

// vmEnabled reports whether any node type may run as a VM; until one may, the runtime setting
// stays out of the editor.
func vmEnabled() bool { return len(vmEnabledTypes()) > 0 }

// runtimeIssues checks every node's and cluster's runtime. limaWhy is why VMs cannot run on this
// machine, or "" when they can.
func runtimeIssues(doc designDoc, limaWhy string) []issue {
	var out []issue
	frames := map[string]designFrame{}
	checkSE := func(what, mode string) {
		if mode != "" && mode != selinuxEnforcing && mode != selinuxPermissive {
			out = append(out, issue{Level: "error", Message: what + `: selinux "` + mode + `" is not enforcing or permissive`})
		}
	}
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
		checkSE(firstNonEmpty(f.Label, f.ID), f.SELinux)
	}
	for _, n := range doc.Nodes {
		checkSE(firstNonEmpty(n.Label, n.ID), n.SELinux)
		rt := n.Runtime
		if rt == "" {
			continue // a member without its own runtime follows its cluster, checked above
		}
		check(firstNonEmpty(n.Label, n.ID), n.Type, rt)
	}
	for _, n := range doc.Nodes {
		if nodeRuntime(n, frames) == runtimeVM && n.Type == "linuxclient" && n.GDBEnabled {
			// The analysis reads the core and the crashed host's libraries through read-only bind
			// mounts from the Docker host (gdbcore.go), which a VM has no counterpart for.
			out = append(out, issue{Level: "error", Message: firstNonEmpty(n.Label, n.ID) + ": a Linux Client for core-dump analysis runs in a container: it reads the core through bind mounts from the Docker host, which a VM does not have. The server that crashed can run as a VM"})
		}
	}
	for _, n := range doc.Nodes {
		if nodeRuntime(n, frames) == runtimeVM && (n.DeviceReadMBps > 0 || n.DeviceWriteMBps > 0) {
			out = append(out, issue{Level: "warning", Message: firstNonEmpty(n.Label, n.ID) + ": disk read/write limits apply to containers; a VM node runs without them"})
		}
	}
	if limaWhy == "" {
		// A VM that runs on QEMU needs it installed; said once per architecture, at validation,
		// rather than as limactl's own error halfway through a deploy.
		said := map[string]bool{}
		for _, n := range doc.Nodes {
			if nodeRuntime(n, frames) != runtimeVM {
				continue
			}
			arch := effectiveArch(n.Type, n.Arch, frames[n.FrameID].Arch)
			if said[arch] {
				continue
			}
			said[arch] = true
			if why := limaQEMUMissing(arch); why != "" {
				out = append(out, issue{Level: "error", Message: firstNonEmpty(n.Label, n.ID) + ": " + why})
			}
		}
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
	app     *App // joins a new VM to its stack's network and keeps its link (vmnet.go)
	docker  *Docker
	lima    *Lima
	vm      map[string]bool
	selinux map[string]string // each VM node's SELinux mode, by container name
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
	if mode := r.selinux[spec.Name]; mode != "" {
		spec.SELinux = mode
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

// ContainerStart starts a VM and waits for its link to the stack network. The link is started
// first, alongside the boot: Lima reports a VM started only once cloud-init has finished, which
// waits for the node's services, which wait for the link (limaBootOrder) — the link connects as
// soon as the VM's sshd answers, early in the boot.
func (r *engineRouter) ContainerStart(ctx context.Context, id string) error {
	if !isLimaID(id) {
		return r.docker.ContainerStart(ctx, id)
	}
	var link *vmLink
	if r.app != nil {
		link = r.app.ensureVMLink(id)
	}
	if err := r.lima.ContainerStart(ctx, id); err != nil {
		return err
	}
	if link == nil {
		return nil
	}
	return link.wait(ctx, 2*time.Minute)
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
	out, err := r.docker.ListPublishedPorts(ctx)
	if err != nil {
		return nil, err
	}
	vms, _ := r.lima.ListPublishedPorts(ctx)
	for p, name := range vms {
		out[p] = name
	}
	return out, nil
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
