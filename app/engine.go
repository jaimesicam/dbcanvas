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

// engByStackID resolves the engine from a stack id, for code paths that only hold
// the id. Falls back to Docker if the stack can't be loaded.
func (a *App) engByStackID(stackID int64) Engine {
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return a.docker
	}
	return a.eng(st)
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

// vagrantUnsupportedNode / vagrantUnsupportedFrame are the node/frame types the
// vagrant backend cannot provision: Docker-image infra nodes with no OS-box
// equivalent, and K3D (which is k3s-inside-Docker). Everything else — the Intranet
// plus the OS/DB nodes and their cluster frames — runs as a VirtualBox VM.
var vagrantUnsupportedNode = map[string]bool{
	"pmm": true, "keycloak": true, "openbao": true,
	"seaweedfs": true, "vnc": true, "watchtower": true, "sambaad": true,
}
var vagrantUnsupportedFrame = map[string]bool{"k3d": true}

// vagrantUnsupportedTypes returns the distinct unsupported node/frame types present
// in a design, so a vagrant deploy can be rejected with a clear message instead of
// silently skipping nodes.
func vagrantUnsupportedTypes(doc designDoc) []string {
	seen := map[string]bool{}
	var out []string
	mark := func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, n := range doc.Nodes {
		if vagrantUnsupportedNode[n.Type] {
			mark(n.Type)
		}
	}
	for _, f := range doc.Frames {
		if vagrantUnsupportedFrame[f.Type] {
			mark(f.Type)
		}
	}
	return out
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
