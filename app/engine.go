package main

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// engine.go — the provisioning-engine seam.
//
// DBCanvas provisions stack nodes as Docker containers (*Docker) and, for nodes set to
// run as virtual machines, as Lima VMs (*Lima, lima.go). Both satisfy the Engine
// interface below, and a stack with VM nodes uses an engineRouter (engine_router.go)
// that sends each node to its own engine, so provisioning and node-management code can
// talk to "the engine this stack runs on" without caring which it is.
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
	ImageRemove(ctx context.Context, ref string)

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
	ContainerPorts(ctx context.Context, id string) (map[int]int, error)
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
	// PutArchiveStream is PutArchive without holding the archive in memory —
	// for node file drops, which are bounded by the configured max upload size
	// (gibibytes) rather than by anything that would fit in RAM.
	PutArchiveStream(ctx context.Context, id, dir string, r io.Reader) error
	// GetArchiveStream reads path out of a node as a tar stream. The caller
	// closes it. This is the download/copy-between-nodes direction, and it is
	// deliberately not exec-based: the archive endpoint needs no shell, no tar
	// and no coreutils in the image (see nodefs.go).
	GetArchiveStream(ctx context.Context, id, path string) (io.ReadCloser, error)

	// Interactive console (web terminal).
	HijackExec(ctx context.Context, containerID string, cmd, env []string, user string) (*ExecConn, error)
	ResizeExec(ctx context.Context, execID string, w, h int) error
}

// Compile-time proof Docker implements Engine.
var _ Engine = (*Docker)(nil)

// eng returns the provisioning engine for a stack: Docker for a stack whose nodes all run
// in containers, an engineRouter when some run as VMs.
func (a *App) eng(st Stack) Engine {
	if a.lima == nil {
		return a.docker
	}
	if vm := stackVMNames(st); len(vm) > 0 {
		return &engineRouter{app: a, docker: a.docker, lima: a.lima, vm: vm}
	}
	return a.docker
}

// anyEngine is the engine for an operation that names a container or VM by id without a
// stack at hand: it tells the two apart by the id.
func (a *App) anyEngine() Engine {
	if a.lima == nil {
		return a.docker
	}
	return &engineRouter{app: a, docker: a.docker, lima: a.lima}
}

// intranetEngine is the engine the Intranet always runs on. The Intranet stays on
// Docker in a stack with VM nodes too (its bind config forwards to Docker's embedded
// resolver, 127.0.0.11, which only exists in a container), so every operation on the
// Intranet container uses Docker regardless of the calling node's engine. VM nodes
// reach the Intranet's DNS and CA over the stack network the Intranet carries to them.
func (a *App) intranetEngine() Engine { return a.docker }

// readIntranetFile reads a file (e.g. the CA cert/key used to sign a node's TLS
// certificate) from the Intranet container on the Intranet's engine, so a VM node
// provisioning on Lima still reads it from the Docker Intranet.
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

// nodeEngine picks the engine for a node or frame of the given type in a stack. The
// stack's engine sends each node to its own runtime by its container name.
func (a *App) nodeEngine(st Stack, typ string) Engine { return a.eng(st) }

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
	return a.eng(st)
}

// stackEngines returns the engines a stack may have provisioned nodes on; teardown sweeps
// each. With Lima installed that is the router, which also finds the VM of a node whose
// runtime changed since it was deployed.
func (a *App) stackEngines(st Stack) []Engine { return []Engine{a.anyEngine()} }

// engCtx returns the engine carried by ctx, or, when none is set (management and
// request contexts that never entered a deploy scope), the one that tells containers
// and VMs apart by id.
func (a *App) engCtx(ctx context.Context) Engine {
	if e, ok := ctx.Value(engineKey{}).(Engine); ok && e != nil {
		return e
	}
	return a.anyEngine()
}
