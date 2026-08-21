package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// k3ddebug.go — run a K3D cluster's operator under Delve, with the debugger's port published to
// the host so an IDE can attach to it.
//
// This turns the operator from a black box into something you can step through: put a breakpoint
// in Reconcile, annotate the custom resource, and watch the reconcile loop run against a real
// cluster. Three things have to happen for that, and all three are deployment-time decisions —
// which is why this is a frame option and not something to bolt on afterwards.
//
//  1. A debug build. The release image's binary is compiled with the optimiser on and its symbol
//     table stripped (-w -s); Delve can attach to it but not usefully step through it — inlined
//     frames vanish and locals read "optimized out". So the tag's source — the very tarball the
//     frame already downloads for bundle.yaml and cr.yaml — is compiled again with
//     `-gcflags=all=-N -l`, next to a dlv built by the same toolchain.
//
//  2. Getting the two binaries into the pod. Deliberately NOT by building an image: DBCanvas has
//     no image build (docker.go pulls, it does not build), and a locally built image then has to
//     be imported into every k3s node and kept in step with the cluster's platform. Instead both
//     static binaries are dropped into /opt/dbcanvas-debug on each k3s node *container* and
//     mounted into the operator pod as a hostPath — kubelet runs inside that container, so its
//     idea of "the host" is the node container's own filesystem.
//
//     Keeping the **stock image** on the pod is not just tidiness. The operator resolves the image
//     for the init containers it injects into every database pod from its own pod's image
//     (k8s.GetInitImage, which falls back to the operator container's image). Run it from a
//     locally built image and every database pod tries to pull that name and fails — which is why
//     the hand-rolled recipe for this needs `spec.initContainer.image` pinned in cr.yaml. Leaving
//     the image alone means the CR needs no change at all, and the *only* difference between a
//     debug deploy and a normal one is the container's command.
//
//  3. Reaching Delve from the IDE. `kubectl port-forward` is the usual answer, but it is a
//     foreground command the user has to keep alive. DBCanvas is already the thing that creates
//     the cluster, so the port is published properly instead: a NodePort Service in front of the
//     operator pod, and `--port <bind>:<host>:30400@server:0` on `k3d cluster create` — which k3d
//     accepts only at create time (a running cluster's published ports cannot be changed except by
//     `k3d cluster edit --port-add`, which only targets the load balancer). The IDE then attaches
//     to 127.0.0.1:40000 on the host with nothing left running in a terminal. From inside the
//     stack the same listener is <server FQDN>:30400.
//
// Delve is left with `--continue`, so the operator runs normally until a breakpoint is set — a
// deploy with the debugger on still produces a working cluster if nobody ever attaches.

const (
	// Where the debug binaries live on each k3s node container, and where the operator pod sees
	// them. The node path's basename is the tar entry GetArchiveStream produces, so the two
	// constants below are not independent — see k3dDebugBuild.
	k3dDebugNodeDir  = "/opt/dbcanvas-debug"
	k3dDebugPodMount = "/dbcanvas-debug"
	// Delve's listener inside the pod, the NodePort that fronts it, and the host port published
	// to the machine running the IDE. The NodePort must sit in Kubernetes' service-node-port
	// range (30000–32767), which is why it is not simply 40000 as well.
	k3dDebugPort     = 40000
	k3dDebugNodePort = 30400
	k3dDebugHostDflt = 40000
	// Resource limits for the debug container. bundle.yaml ships 200m CPU / 500Mi, which is
	// generous for the operator and much too tight for Delve on top of it: it reads the whole
	// (unstripped, un-inlined) binary's DWARF into memory, and a 200m ceiling makes every step
	// crawl. Requests are left alone, so scheduling is unchanged.
	k3dDebugCPULimit = "2"
	k3dDebugMemLimit = "2Gi"
	// The watchdog sidecar that heals the debugger's state when a session ends — see
	// k3dDebugWatchdogScript. Its poll interval is the worst case the operator can stay frozen
	// after an IDE disconnects.
	k3dDebugWatchdog     = "dbcanvas-debug-watchdog"
	k3dDebugWatchdogTick = 10
	// The Go module and build caches, kept in named volumes so the second debug deploy does not
	// download the operator's (very large) dependency tree again.
	k3dDebugModCache   = "dbcanvas-go-mod"
	k3dDebugBuildCache = "dbcanvas-go-build"
)

// k3dDebuggableOperator lists the operators whose debug build is wired up. All four Percona
// operators are a single Go module with the manager under cmd/manager, so extending this is a map
// entry and a build-directory line — but each one has to be tried on a live cluster before it is
// offered, so only the one that has been is listed.
var k3dDebuggableOperator = map[string]bool{"pxc": true}

// k3dDebugOn reports whether this frame asked for its operator to run under Delve. A frame that
// asks for it against an operator it is not wired up for is ignored here and flagged by
// k3dFrameIssues, rather than failing the deploy over a debugger.
func k3dDebugOn(f designFrame) bool {
	return f.K3DDebug && k3dDebuggableOperator[strings.TrimSpace(f.K3DOperator)]
}

// k3dDebugHostPort is the host port Delve is published on. Fixed rather than auto-assigned on
// purpose: it goes into the IDE's launch.json, where a port that changes on every redeploy is
// worse than one that occasionally collides (and a collision is caught before the cluster is
// created — see provisionK3DFrame).
func k3dDebugHostPort(f designFrame) int {
	if p := f.K3DDebugPort; p > 0 && p < 65536 {
		return p
	}
	return k3dDebugHostDflt
}

// k3dDebugCreateArgs are the `k3d cluster create` flags that publish the NodePort to the host.
// Bound to CONTAINER_BIND_IP like every other port DBCanvas publishes (default 127.0.0.1, i.e.
// loopback only — a debugger port is a remote-code-execution endpoint by design, and has no
// business on the LAN).
func k3dDebugCreateArgs(f designFrame) []string {
	return []string{"--port", fmt.Sprintf("%s:%d:%d/tcp@server:0",
		envOr("CONTAINER_BIND_IP", "127.0.0.1"), k3dDebugHostPort(f), k3dDebugNodePort)}
}

// k3dDebugBuildDir is where the operator source is compiled inside the builder container. It
// matches the path the operator's own build/Dockerfile uses, so the paths recorded in the binary's
// DWARF are the ones upstream's builds produce — and the launch.json this generates maps them back
// to a local clone with one substitutePath entry.
func k3dDebugBuildDir(op string) string {
	repo, ok := k3dOperatorRepos[op]
	if !ok {
		return ""
	}
	return "/go/src/github.com/percona/" + repo
}

// ---------------------------------------------------------------- install

// k3dInstallDebugger builds the debug binaries, drops them on every node, and patches the operator
// Deployment to run under Delve.
//
// Called after bundle.yaml (the Deployment has to exist to be patched) and before cr.yaml, so a
// breakpoint set while the deploy is still running catches the cluster's very first reconcile.
//
// It never fails the deploy. An operator that could not be put under a debugger is still a working
// operator, and a failed rollout is undone rather than left half-applied; what went wrong is
// written to the node's log and recorded in the config the panel renders.
func (a *App) k3dInstallDebugger(ctx context.Context, st Stack, frame designFrame, deployment string,
	tarball []byte, serverID string, cfg *k3dConfig, pr *pxcProg) {
	cfg.DebugPort = k3dDebugHostPort(frame)
	cfg.DebugNodePort = k3dDebugNodePort
	cfg.DebugBuildDir = k3dDebugBuildDir(cfg.Operator)
	cfg.DebugGOARCH = k3dDebugGOARCH()
	if err := a.k3dDebugInstall(ctx, st, deployment, tarball, serverID, cfg, pr); err != nil {
		cfg.DebugStatus = "not attached: " + err.Error()
		pr.logln("the operator is NOT running under Delve: " + err.Error())
		return
	}
	cfg.DebugStatus = "listening"
}

func (a *App) k3dDebugInstall(ctx context.Context, st Stack, deployment string, tarball []byte,
	serverID string, cfg *k3dConfig, pr *pxcProg) error {
	pr.phase("Building the debug operator", 77)
	bits, err := a.k3dDebugBuild(ctx, st, tarball, cfg, pr)
	if err != nil {
		return err
	}
	defer func() { bits.Close(); os.Remove(bits.Name()) }()

	pr.phase("Installing the debugger on the nodes", 79)
	if err := a.k3dDebugDrop(ctx, bits, cfg, pr); err != nil {
		return err
	}

	pr.phase("Restarting the operator under Delve", 80)
	return a.k3dDebugPatch(ctx, serverID, deployment, cfg, pr)
}

// ---------------------------------------------------------------- the build

// k3dDebugBuild compiles the operator and dlv in a throwaway golang container and returns the tar
// stream of both binaries, spooled to a temp file — they are ~200 MB together, which is not
// something to hold in memory, and it has to be replayed once per node.
//
// The builder runs on the *daemon's own* architecture and cross-compiles to the cluster's
// (GOARCH from k3dPlatform), rather than running an emulated builder: a K3D frame targets
// linux/amd64 by default even on an arm64 host, and a Go cross-compile is free while qemu is not.
// Both binaries are CGO_ENABLED=0, so they run in the operator's UBI-minimal image with nothing
// else copied in.
func (a *App) k3dDebugBuild(ctx context.Context, st Stack, tarball []byte, cfg *k3dConfig, pr *pxcProg) (*os.File, error) {
	repo, ok := k3dOperatorRepos[cfg.Operator]
	if !ok {
		return nil, fmt.Errorf("no source repository for operator %q", cfg.Operator)
	}
	srcDir := fmt.Sprintf("%s-%s", repo, cfg.OperatorVer) // the tarball's own top directory
	goTag, err := k3dDebugGoVersion(tarball, srcDir)
	if err != nil {
		return nil, err
	}
	arch := k3dDebugGOARCH()
	pr.logln("debug build: go " + goTag + " → linux/" + arch + " (optimiser off: -gcflags=all=-N -l)")

	eng := a.engCtx(ctx)
	if err := eng.EnsureImage(ctx, "golang", goTag, ""); err != nil {
		return nil, fmt.Errorf("pull golang:%s: %w", goTag, err)
	}
	// Best-effort: a cache volume that cannot be created just means a slower build.
	eng.VolumeCreate(ctx, k3dDebugModCache)
	eng.VolumeCreate(ctx, k3dDebugBuildCache)

	// The builder idles and is driven by Exec, like the io.max helper in blkio.go: a container
	// whose *command* is the build would be restarted by the default restart policy the moment
	// it finished, and its exit code is easier to read from an exec than from a wait.
	cid, err := eng.ContainerCreate(ctx, ContainerSpec{
		Name:      fmt.Sprintf("dbcanvas-%d-dlvbuild-%d", st.ID, time.Now().UnixNano()),
		Image:     "golang:" + goTag,
		Cmd:       []string{"sleep", "3600"},
		NoRestart: true,
		Binds: []string{
			k3dDebugModCache + ":/go/pkg/mod",
			k3dDebugBuildCache + ":/root/.cache/go-build",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create the build container: %w", err)
	}
	defer eng.ContainerRemove(ctx, cid)
	if err := eng.ContainerStart(ctx, cid); err != nil {
		return nil, fmt.Errorf("start the build container: %w", err)
	}

	buildDir := k3dDebugBuildDir(cfg.Operator)
	parent := buildDir[:strings.LastIndex(buildDir, "/")]
	if res, err := eng.Exec(ctx, cid, []string{"mkdir", "-p", parent}, nil); err != nil {
		return nil, err
	} else if res.Code != 0 {
		return nil, fmt.Errorf("mkdir %s: %s", parent, strings.TrimSpace(res.Stderr))
	}
	if err := eng.PutArchive(ctx, cid, parent, tarball); err != nil {
		return nil, fmt.Errorf("copy the operator source into the build container: %w", err)
	}

	started := time.Now()
	res, err := eng.Exec(ctx, cid, []string{"sh", "-c", k3dDebugBuildScript}, []string{
		"SRC=" + parent + "/" + srcDir,
		"DST=" + buildDir,
		"OUT=" + k3dDebugPodMount,
		"BIN=" + repo,
		"DLV=" + envOr("DLV_VERSION", "latest"),
		"GOOS=linux", "GOARCH=" + arch, "CGO_ENABLED=0",
	})
	if err != nil {
		return nil, fmt.Errorf("run the debug build: %w", err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("the debug build failed: %s", k3dDebugTail(res.Stderr+res.Stdout))
	}
	pr.logln("debug build finished in " + time.Since(started).Round(time.Second).String() +
		" — " + strings.TrimSpace(res.Stdout))

	// Read the output directory back out as a tar. Its single top-level entry is the basename of
	// OUT, which is also the basename of k3dDebugNodeDir — that is what lets the same archive be
	// unpacked straight into /opt on each node.
	rc, err := eng.GetArchiveStream(ctx, cid, k3dDebugPodMount)
	if err != nil {
		return nil, fmt.Errorf("read the debug binaries out of the build container: %w", err)
	}
	defer rc.Close()
	f, err := os.CreateTemp("", "dbcanvas-dlv-*.tar")
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, rc)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("spool the debug binaries: %w", err)
	}
	pr.logln(fmt.Sprintf("debug binaries: %s and dlv (%d MiB)", repo, n>>20))
	return f, nil
}

// k3dDebugBuildScript builds both binaries into $OUT.
//
// dlv is built rather than `go install`ed because GOBIN's interaction with a cross-compile is not
// worth relying on: `go build -o` puts the binary exactly where it is told, whatever GOARCH says.
// For the same reason the version reported at the end comes from the module graph and not from
// running the binary — on an arm64 daemon building for an amd64 cluster it would not run here.
// It is pinned to the same toolchain as the operator, which is what keeps Delve's Go-version check
// happy — and --check-go-version=false at run time covers the case where it still is not.
const k3dDebugBuildScript = `set -e
mkdir -p "$OUT"
mv "$SRC" "$DST"
cd "$DST"
go build -gcflags="all=-N -l" -o "$OUT/$BIN" ./cmd/manager
mkdir -p /tmp/dlvbuild
cd /tmp/dlvbuild
go mod init dbcanvas-dlv >/dev/null
go get github.com/go-delve/delve/cmd/dlv@$DLV
go build -o "$OUT/dlv" github.com/go-delve/delve/cmd/dlv
echo "dlv $(go list -m -f '{{.Version}}' github.com/go-delve/delve)"
`

// k3dDebugGoVersion is the golang image tag to build with: the major.minor of the module's own go
// directive, so the operator is compiled by the toolchain it was written for.
//
// The go.mod has to be addressed by its full path in the tarball. Matching on the suffix alone
// (what tarFile does) finds .github/linters/go.mod first — it sorts ahead of the root one, and it
// is a linter stub whose go directive means nothing.
func k3dDebugGoVersion(tarball []byte, srcDir string) (string, error) {
	mod, err := tarFile(tarball, srcDir+"/go.mod")
	if err != nil {
		return "", fmt.Errorf("read go.mod from the operator source: %w", err)
	}
	for _, ln := range strings.Split(string(mod), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "go ") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(ln, "go "))
		parts := strings.Split(v, ".")
		if len(parts) < 2 {
			continue
		}
		return parts[0] + "." + parts[1], nil
	}
	return "", fmt.Errorf("no go directive in %s/go.mod", srcDir)
}

// k3dDebugGOARCH is the cluster's architecture, as Go spells it. k3dPlatform is "linux/amd64" or
// "linux/arm64" — the k3s nodes' platform, which is not necessarily the daemon's.
func k3dDebugGOARCH() string {
	p := k3dPlatform()
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// k3dDebugTail keeps the last few lines of a failed build's output — a Go build error's useful
// part is at the end, and the whole log does not belong in a progress line.
func k3dDebugTail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, "; ")
}

// ---------------------------------------------------------------- the drop

// k3dDebugDrop unpacks the binaries into /opt on every k3s node container.
//
// Every node, not just the one the operator happens to be scheduled on: a hostPath the node does
// not have leaves the pod stuck in ContainerCreating, and 200 MB per node is cheap next to pinning
// the operator to the server with a nodeSelector it would not otherwise have.
func (a *App) k3dDebugDrop(ctx context.Context, bits *os.File, cfg *k3dConfig, pr *pxcProg) error {
	eng := a.engCtx(ctx)
	parent := k3dDebugNodeDir[:strings.LastIndex(k3dDebugNodeDir, "/")]
	for i := 0; i < cfg.Nodes; i++ {
		name := k3dNodeContainer(cfg.Cluster, i)
		cid, ok, err := eng.ContainerByName(ctx, name)
		if err != nil || !ok {
			return fmt.Errorf("node container %s not found", name)
		}
		// The k3s image is busybox — no bash, but mkdir is there (k3dFetchOperator relies on the
		// same thing), and /opt does not exist in it.
		if _, err := eng.Exec(ctx, cid, []string{"mkdir", "-p", parent}, nil); err != nil {
			return fmt.Errorf("mkdir %s on %s: %w", parent, name, err)
		}
		if _, err := bits.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := eng.PutArchiveStream(ctx, cid, parent, bits); err != nil {
			return fmt.Errorf("copy the debug binaries to %s: %w", name, err)
		}
	}
	pr.logln(fmt.Sprintf("debug binaries in %s on all %d node(s)", k3dDebugNodeDir, cfg.Nodes))
	return nil
}

// ---------------------------------------------------------------- the patch

// k3dDebugPatch swaps the operator's command for Delve and publishes its port.
//
// The patch is strategic-merge, so every list it touches merges by the key Kubernetes defines for
// it (containers by name, env by name, ports by containerPort, volumes by name, volumeMounts by
// mountPath) — nothing the bundle set is replaced wholesale.
//
// What each piece is for:
//
//   - livenessProbe: null. The probe hits the operator's own /metrics. Halt at a breakpoint and
//     it stops answering, and kubelet kills the pod out from under the debugger within 30s.
//   - PXCO_LEADER_ELECTION_ENABLED=false. The manager renews its lease every 10s and *exits* when
//     it cannot; a paused process cannot, so the second breakpoint you sit on would end the
//     session. There is exactly one operator here, so the lease buys nothing.
//   - SYS_PTRACE. Delve ptraces the process it exec'd. That is a child of the same uid, which
//     Yama normally permits, but the capability costs nothing and removes the question.
//   - --only-same-user=false. Delve otherwise refuses any connection it cannot prove came from the
//     user that started it, which it can only do for loopback peers. That is why the usual recipe
//     works through `kubectl port-forward` (the connection arrives from inside the pod's own
//     netns) and would silently refuse the NodePort, where kube-proxy hands it the node's address.
//   - --continue. The operator runs immediately instead of waiting for a client, so the deploy
//     finishes and the cluster comes up whether or not anyone ever attaches.
func (a *App) k3dDebugPatch(ctx context.Context, serverID, deployment string, cfg *k3dConfig, pr *pxcProg) error {
	ns := cfg.Namespace
	// The watchdog sidecar runs the *operator's own* image — it needs a shell and the dlv on the
	// hostPath, nothing more — so nothing extra is ever pulled. Read it off the Deployment rather
	// than reconstructing it: bundle.yaml is the authority on which image this release uses.
	image, err := a.kubectl(ctx, serverID, "-n", ns, "get", "deployment", deployment,
		"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="`+deployment+`")].image}`)
	if err != nil || strings.TrimSpace(image) == "" {
		return fmt.Errorf("read the operator image off its deployment: %w", err)
	}
	patch := k3dDebugPatchJSON(deployment, strings.TrimSpace(image))
	if _, err := a.kubectl(ctx, serverID, "-n", ns, "patch", "deployment/"+deployment,
		"--type=strategic", "-p", patch); err != nil {
		return fmt.Errorf("patch the operator deployment: %w", err)
	}
	if _, err := a.kubectl(ctx, serverID, "-n", ns, "rollout", "status",
		"deployment/"+deployment, "--timeout=300s"); err != nil {
		// A half-applied debugger leaves no operator at all, which is worse than no debugger.
		// Undo puts the shipped pod spec back and waits for it, so the cluster still gets built.
		pr.logln("the debug rollout did not come up — reverting the operator to its shipped spec")
		if _, uerr := a.kubectl(ctx, serverID, "-n", ns, "rollout", "undo", "deployment/"+deployment); uerr == nil {
			a.kubectl(ctx, serverID, "-n", ns, "rollout", "status", "deployment/"+deployment, "--timeout=300s")
		}
		return fmt.Errorf("the operator did not restart under Delve: %w", err)
	}
	if err := a.kubectlApply(ctx, serverID, ns, k3dDebugService(deployment)); err != nil {
		return fmt.Errorf("publish the Delve port: %w", err)
	}

	// Delve prints its listener as its very first line. Reading it back is the one cheap proof
	// that the debugger is actually up rather than merely rolled out.
	//
	// Two details, both found the hard way. `logs deployment/...` picks an arbitrary pod out of
	// the label selector, and the one being replaced is still Running (Terminating is not a
	// phase) for seconds after the rollout completes — so the newest pod is named explicitly.
	// And the line is at the *head* of the log, under everything the operator has said since,
	// which is why this reads the first few KiB rather than a --tail.
	if pod, err := a.kubectl(ctx, serverID, "-n", ns, "get", "pods",
		"-l", "app.kubernetes.io/name="+deployment,
		"--sort-by=.metadata.creationTimestamp",
		"-o", "jsonpath={.items[-1:].metadata.name}"); err == nil && strings.TrimSpace(pod) != "" {
		out, _ := a.kubectl(ctx, serverID, "-n", ns, "logs", strings.TrimSpace(pod), "--limit-bytes=4096")
		for _, ln := range strings.Split(out, "\n") {
			if strings.Contains(ln, "API server listening at") {
				pr.logln("delve: " + strings.TrimSpace(ln))
				break
			}
		}
	}
	pr.logln("the operator runs under Delve — attach an IDE to " +
		envOr("CONTAINER_BIND_IP", "127.0.0.1") + ":" + strconv.Itoa(cfg.DebugPort) +
		" (from inside the stack: any node's FQDN on :" + strconv.Itoa(k3dDebugNodePort) + ")")
	return nil
}

// k3dDebugWatchdogScript is the sidecar that makes the debugger safe to walk away from.
//
// The problem it solves is not hypothetical — it is what makes a hand-rolled Delve setup so
// unpleasant, and Delve documents it as a known gap (service/dap/server.go, onDisconnectRequest:
// "The target is left in whatever state it is already in ... TODO(polina): should we always issue
// a continue here"). Two things survive an IDE disconnecting:
//
//  1. **The breakpoints.** They stay armed on the server. The next reconcile hits one with nobody
//     attached, and the operator freezes — no probe fires, nothing is logged, the cluster simply
//     stops being reconciled. Reproduced live: a normal VS Code session, ended normally, and the
//     operator was halted 6 seconds later.
//  2. **A halted process.** Disconnect while paused at a breakpoint and it never resumes.
//
// And on the *next* attach the stale breakpoint makes Delve answer setBreakpoints with
// "Breakpoint exists at ...", which the IDE renders as an unverified (hollow) breakpoint — the
// debugger looks broken when it is the leftovers that are in the way.
//
// The fix needs two facts Delve does not report, and the kernel reports both:
//
//   - **Is anyone attached?** The sidecar shares the pod's network namespace, so an ESTABLISHED
//     (state 01) socket on Delve's port in /proc/net/tcp6 is an attached client. A client that has
//     gone leaves the socket in CLOSE_WAIT (08), which correctly does not count.
//   - **Is the operator actually halted?** Delve stops its debuggee with ptrace, so the process
//     sits in state `t` (tracing stop) in /proc/<pid>/stat — verified live on a frozen operator,
//     where dlv itself was `S` and the operator it had exec'd was `t`. Seeing it needs
//     shareProcessNamespace on the pod, which is why the patch sets it.
//
// Acting on the *condition* rather than on session bookkeeping is what makes this reliable: an
// earlier version watched for a client appearing and then leaving, and missed short sessions
// entirely (it polls every 10s; a session can start and end between two ticks, and the operator
// then stays frozen forever — reproduced). Halted-and-unattached is the thing that is actually
// wrong, and it is true whether or not the watchdog saw the session that caused it. In the
// ordinary case — a cluster nobody is debugging — both checks are a few /proc reads and no
// connection at all.
//
// Clearing rather than keeping the breakpoints is deliberate: an IDE re-sends its breakpoints on
// every attach, so nothing is lost, and the next session starts against a clean server — which is
// also what stops the second attach from showing an unverified breakpoint.
const k3dDebugWatchdogScript = `BIN=%s
attached() {
  awk -v p=":%s$" '$2 ~ p && $4 == "01" { f=1 } END { exit(f?0:1) }' /proc/net/tcp6 /proc/net/tcp 2>/dev/null
}
halted() {
  for d in /proc/[0-9]*; do
    [ -r "$d/cmdline" ] || continue
    case "$(tr '\0' ' ' < "$d/cmdline")" in
      "$BIN "*) ;;
      *) continue ;;
    esac
    st=$(awk '{ s = $0; sub(/^.*\) /, "", s); split(s, a, " "); print a[1] }' "$d/stat" 2>/dev/null)
    [ "$st" = "t" ] && return 0
  done
  return 1
}
echo "dbcanvas debug watchdog: nothing to do unless the operator is halted with no debugger on :%d"
while :; do
  sleep %d
  attached && continue
  halted || continue
  printf 'clearall\nexit -c\nn\n' | %s/dlv connect 127.0.0.1:%d >/dev/null 2>&1
  echo "the operator was halted at a breakpoint with no debugger attached: cleared the leftover breakpoints and resumed it"
done
`

// k3dDebugWatchdogSh renders the watchdog for this operator. /proc/net/tcp holds ports in
// upper-case hex, which is what the socket scan matches on.
func k3dDebugWatchdogSh(deployment string) string {
	return fmt.Sprintf(k3dDebugWatchdogScript,
		k3dDebugPodMount+"/"+deployment, fmt.Sprintf("%04X", k3dDebugPort),
		k3dDebugPort, k3dDebugWatchdogTick, k3dDebugPodMount, k3dDebugPort)
}

// k3dDebugPatchJSON is the strategic-merge patch itself, kept separate from the kubectl call so
// its shape can be asserted without a cluster.
func k3dDebugPatchJSON(deployment, image string) string {
	return fmt.Sprintf(`{"spec":{"template":{"spec":{
"shareProcessNamespace":true,
"volumes":[{"name":"dbcanvas-debug","hostPath":{"path":%q,"type":"Directory"}}],
"containers":[{"name":%q,
"command":["%s/dlv"],
"args":["exec","%s/%s","--headless","--listen=:%d","--api-version=2","--accept-multiclient","--only-same-user=false","--check-go-version=false","--continue"],
"volumeMounts":[{"name":"dbcanvas-debug","mountPath":%q,"readOnly":true}],
"ports":[{"name":"delve","containerPort":%d,"protocol":"TCP"}],
"env":[{"name":"PXCO_LEADER_ELECTION_ENABLED","value":"false"}],
"livenessProbe":null,
"resources":{"limits":{"cpu":%q,"memory":%q}},
"securityContext":{"capabilities":{"add":["SYS_PTRACE"]}}},
{"name":%q,
"image":%q,
"command":["sh","-c",%s],
"volumeMounts":[{"name":"dbcanvas-debug","mountPath":%q,"readOnly":true}],
"resources":{"requests":{"cpu":"10m","memory":"16Mi"},"limits":{"cpu":"100m","memory":"64Mi"}}}]}}}}`,
		k3dDebugNodeDir, deployment,
		k3dDebugPodMount, k3dDebugPodMount, deployment, k3dDebugPort,
		k3dDebugPodMount, k3dDebugPort, k3dDebugCPULimit, k3dDebugMemLimit,
		k3dDebugWatchdog, image, strconv.Quote(k3dDebugWatchdogSh(deployment)), k3dDebugPodMount)
}

// k3dDebugService fronts Delve with a NodePort, which is the half of the published port that lives
// inside Kubernetes: k3d maps the host port onto the server node's :30400, and this is what
// answers there. It selects the operator pod by the three labels bundle.yaml gives it.
func k3dDebugService(deployment string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s-delve
  labels:
    app.kubernetes.io/name: %s
    dbcanvas.io/purpose: debugger
spec:
  type: NodePort
  selector:
    app.kubernetes.io/component: operator
    app.kubernetes.io/instance: %s
    app.kubernetes.io/name: %s
  ports:
    - name: delve
      protocol: TCP
      port: %d
      targetPort: %d
      nodePort: %d
`, deployment, deployment, deployment, deployment, k3dDebugPort, k3dDebugPort, k3dDebugNodePort))
}
