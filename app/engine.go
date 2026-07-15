package main

import (
	"context"
	"encoding/json"
	"time"
)

// engine.go — the provisioning-engine seam.
//
// DBCanvas provisions stack nodes through one of two backends: Docker containers
// (the default, *Docker) or VirtualBox VMs driven by Vagrant (*Vagrant). Both
// satisfy the Engine interface below, so provisioning and node-management code can
// talk to "the engine this stack was deployed with" without caring which it is.
//
// The method set is exactly what the rest of the app calls on the Docker client
// today (see `grep -oh '\.docker\.[A-Z]\w*'`). *Docker already implements every
// method, so wrapping it in Engine is behaviour-preserving. Shared value types
// (ContainerSpec, ExecResult, ContainerInfo, ContainerStat, PortMap, ExecConn)
// live in docker.go and are used verbatim by both backends.
type Engine interface {
	// Reachability / host facts.
	Ping(ctx context.Context) error
	HostArch(ctx context.Context) string
	HostResources(ctx context.Context) (ncpu int, memBytes int64)

	// Images / boxes.
	ImageExists(ctx context.Context, ref string) (bool, error)
	EnsureImage(ctx context.Context, repo, tag, platform string) error

	// Networks.
	NetworkEnsure(ctx context.Context, name string) error
	NetworkRemove(ctx context.Context, name string)
	NetworkConnect(ctx context.Context, network, container string) error
	NetworkDisconnect(ctx context.Context, network, container string)
	NetworkSubnet(ctx context.Context, name string) (string, error)

	// Volumes.
	VolumeCreate(ctx context.Context, name string) error
	VolumeRemove(ctx context.Context, name string)

	// Container/VM lifecycle.
	ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string) error
	ContainerRestart(ctx context.Context, id string) error
	ContainerRemove(ctx context.Context, id string)
	ContainerUpdate(ctx context.Context, id string, nanoCPUs, memBytes int64) error

	// Lookup / introspection.
	ContainerByName(ctx context.Context, name string) (string, bool, error)
	ContainersByNamePrefix(ctx context.Context, prefix string) ([]string, error)
	ContainerIP(ctx context.Context, id, network string) (string, error)
	ContainerPort(ctx context.Context, id, portProto string) (string, error)
	ListPublishedPorts(ctx context.Context) (map[int]string, error)
	ListManaged(ctx context.Context) ([]ContainerInfo, error)
	ContainerStats(ctx context.Context, id string) (ContainerStat, error)
	WaitSystemd(ctx context.Context, id string, timeout time.Duration) error

	// Exec / file transfer.
	Exec(ctx context.Context, id string, cmd []string, env []string) (ExecResult, error)
	ExecAs(ctx context.Context, id, user string, cmd []string, env []string) (ExecResult, error)
	ExecInput(ctx context.Context, id, user string, cmd, env []string, stdin []byte) (ExecResult, error)
	CopyFile(ctx context.Context, id, dir, name string, mode int64, content []byte) error
	PutArchive(ctx context.Context, id, dir string, tarball []byte) error

	// Interactive console (web terminal).
	HijackExec(ctx context.Context, containerID string, cmd, env []string, user string) (*ExecConn, error)
	ResizeExec(ctx context.Context, execID string, w, h int) error
}

// Compile-time proof both backends implement Engine.
var (
	_ Engine = (*Docker)(nil)
	_ Engine = (*Vagrant)(nil)
)

// eng returns the provisioning engine for a stack: the Vagrant backend when the
// stack was deployed with it (and the host actually has it), else Docker. Empty
// Backend (stacks predating the toggle, or never deployed) means Docker.
func (a *App) eng(st Stack) Engine {
	if st.Backend == BackendVagrant && a.vagrant != nil {
		return a.vagrant
	}
	return a.docker
}

// intranetEngine is the engine the Intranet always runs on. The Intranet stays on
// Docker even in a vagrant stack (its bind config forwards to Docker's embedded
// resolver, 127.0.0.11, which only exists in a container), so every operation on the
// Intranet container uses Docker regardless of the calling node's engine. VM nodes
// reach the Docker Intranet's DNS/CA over VirtualBox NAT.
func (a *App) intranetEngine() Engine { return a.docker }

// readIntranetFile reads a file (e.g. the CA cert/key used to sign a node's TLS
// certificate) from the Intranet container on the Intranet's engine, so a VM node
// provisioning on Vagrant still reads it from the Docker Intranet.
func (a *App) readIntranetFile(ctx context.Context, intranetID, path string) ([]byte, error) {
	return a.readContainerFile(withEngine(ctx, a.intranetEngine()), intranetID, path)
}

// engineKey types the context value that carries the engine for an operation.
type engineKey struct{}

// withEngine returns a context carrying e, so deeply-nested provisioning helpers can
// recover "the engine this stack deploys with" without threading it through every
// signature. deployScope injects it for the whole deploy; see engCtx.
func withEngine(ctx context.Context, e Engine) context.Context {
	return context.WithValue(ctx, engineKey{}, e)
}

// userBackend is the deploying user's chosen provisioning backend, normalized
// (defaults to docker). Used to stamp a stack on its first deploy.
func (a *App) userBackend(u User) string {
	js, _ := a.store.UserSettings(u.ID)
	s := defaultSettings()
	if js != "" {
		json.Unmarshal([]byte(js), &s)
	}
	return s.normalize().DeploymentBackend
}

// vagrantVMNode / vagrantVMFrame are the node/frame types that run as VirtualBox
// VMs in hybrid mode: the OS/DB nodes and their cluster frames. Everything not
// listed stays on Docker even in a vagrant stack, so those well-tested paths are
// unchanged — image-only infra (PMM, Keycloak, OpenBao, SeaweedFS, VNC, Watchtower,
// Samba AD), K3D (k3s-in-Docker), and crucially the **Intranet**: its bind config
// forwards to Docker's embedded resolver (127.0.0.11), which only exists in a
// container. VM nodes reach the Docker Intranet's DNS/CA over VirtualBox NAT.
var vagrantVMNode = map[string]bool{
	"ps": true, "pg": true, "psm": true,
	"valkey": true, "proxysql": true, "haproxy": true,
}
var vagrantVMFrame = map[string]bool{
	"pxc": true, "mysql": true, "innodb": true, "psmdb": true, "psmrs": true,
	"patroni": true, "repmgr": true, "spock": true, "valkeycluster": true, "proxysql": true,
}

// nodeEngine picks the engine for a node/frame of the given type. In a vagrant
// (hybrid) stack, VM-capable types run on Vagrant and everything else on Docker; a
// docker stack (or a host with no vagrant) always uses Docker.
func (a *App) nodeEngine(st Stack, typ string) Engine {
	if st.Backend == BackendVagrant && a.vagrant != nil && (vagrantVMNode[typ] || vagrantVMFrame[typ]) {
		return a.vagrant
	}
	return a.docker
}

// depEngine resolves the engine a deployed node runs on, from the stack backend and
// the node's type in the design. Used by teardown / management / the web terminal,
// which hold a node id but not its type.
func (a *App) depEngine(st Stack, nodeID string) Engine {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) == nil {
		for _, n := range doc.Nodes {
			if n.ID == nodeID {
				return a.nodeEngine(st, n.Type)
			}
		}
	}
	return a.docker
}

// stackEngines returns the distinct engines a stack may have provisioned nodes on:
// always Docker, plus Vagrant for a vagrant/hybrid stack. Teardown sweeps both.
func (a *App) stackEngines(st Stack) []Engine {
	if st.Backend == BackendVagrant && a.vagrant != nil {
		return []Engine{a.docker, a.vagrant}
	}
	return []Engine{a.docker}
}

// engCtx returns the engine carried by ctx, or the Docker engine when none is set
// (management/request contexts that never entered a deploy scope, and every code
// path from before the vagrant backend existed — so behaviour is unchanged there).
func (a *App) engCtx(ctx context.Context) Engine {
	if e, ok := ctx.Value(engineKey{}).(Engine); ok && e != nil {
		return e
	}
	return a.docker
}
