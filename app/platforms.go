package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// platforms.go — which architecture each node runs on, and what may not.
//
// A node runs on its own `arch` (amd64 or arm64) when it has one, else on the default platform
// (DOCKER_PLATFORM, which the dbcanvas service sets to the Docker host's own architecture).
// Three things are not free to choose:
//
//   - Kubernetes nodes (K3D and Kubeadm clusters) run on K8S_PLATFORM, the host's own
//     architecture: under emulation containerd cannot create a pod sandbox.
//   - Some node types have images for amd64 alone (amd64Only).
//   - A deployed node keeps the architecture it was built with.
//
// A node of the other architecture than the host's runs under emulation, which works and is
// slower; validation says so.

// amd64Only are the node types whose images exist for amd64 alone.
var amd64Only = map[string]string{
	"pmm2":          "PMM 2",
	"mclusteradmin": "MClusterAdmin",
}

// kubernetesFrames are the cluster frames whose nodes run Kubernetes.
var kubernetesFrames = map[string]string{"k3d": "K3D", "kubeadm": "Kubeadm"}

// archIssues checks every node's and cluster's architecture; host is the Docker host's (amd64,
// arm64), or "" when unknown, which leaves out the emulation notes.
func archIssues(doc designDoc, host string) []issue {
	var out []issue
	bad := func(what, arch string) bool {
		if arch != "" && arch != "amd64" && arch != "arm64" {
			out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: architecture %q is not amd64 or arm64", what, arch)})
			return true
		}
		return false
	}
	emulated := func(what, arch string) {
		if host != "" && archOr(arch) != host {
			out = append(out, issue{Level: "info", Message: fmt.Sprintf("%s runs as %s under emulation on this %s Docker host: it works, several times more slowly — expect a longer deploy, and DBCanvas waits %d times longer for its server to come back after a restore", what, archOr(arch), host, emulationWaitScale)})
		}
	}
	frames := map[string]designFrame{}
	for _, f := range doc.Frames {
		frames[f.ID] = f
		what := firstNonEmpty(f.Label, f.ID)
		if bad(what, f.Arch) {
			continue
		}
		if kind, ok := kubernetesFrames[f.Type]; ok {
			if f.Arch != "" && f.Arch != k8sArch() {
				out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: %s nodes run on this Docker host's own architecture (%s), not %s — Kubernetes cannot create pods under emulation; leave the architecture on Automatic", what, kind, k8sArch(), f.Arch)})
			}
			continue
		}
		emulated(what, f.Arch)
	}
	for _, n := range doc.Nodes {
		what := firstNonEmpty(n.Label, n.ID)
		if bad(what, n.Arch) {
			continue
		}
		if f, ok := frames[n.FrameID]; ok {
			// A member runs on its cluster's architecture: its image is the cluster's. One that
			// names another (a stack file can) is told so, rather than ignored without a word.
			if n.Arch != "" && n.Arch != archOr(f.Arch) {
				out = append(out, issue{Level: "warning", Message: fmt.Sprintf("%s: a cluster member runs on its cluster's architecture (%s); its arch %s is ignored. Set arch on the cluster", what, archOr(f.Arch), n.Arch)})
			}
			continue
		}
		if n.FrameID != "" {
			continue
		}
		if product, ok := amd64Only[n.Type]; ok {
			if n.Arch == "arm64" {
				out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: %s runs on amd64 only", what, product)})
			}
			continue
		}
		if _, ok := kubernetesFrames[n.Type]; ok || n.Type == "pmm" {
			continue // PMM depends on its release (pmmArchIssues)
		}
		emulated(what, n.Arch)
	}
	return out
}

// platformFollower reports whether a stack-wide platform (a stack file's `platform:`, `dbcanvas
// up --platform`) moves a node or cluster of this type: the ones that install a database from
// the version catalogue. Support nodes — the Intranet, the VNC desktop, PMM, Keycloak and the
// other pulled or pre-baked images — stay on the host's own architecture, where they are fast,
// and Kubernetes nodes cannot move. A node's own arch is not touched either way.
func platformFollower(typ string) bool {
	if _, ok := amd64Only[typ]; ok {
		return false
	}
	if _, ok := kubernetesFrames[typ]; ok {
		return false
	}
	if typ == "vnc" {
		return false
	}
	if k, ok := composeKindByType(typ); ok && k.ImageOnly {
		return false
	}
	return true
}

// composeKindByType is the compose kind that builds a design type, if any.
func composeKindByType(typ string) (composeKind, bool) {
	for _, k := range composeKinds {
		if k.Type == typ {
			return k, true
		}
	}
	return composeKind{}, false
}

// normalizePlatform turns "linux/amd64", "amd64" (and arm64 likewise) into amd64 or arm64.
func normalizePlatform(p string) (string, error) {
	switch p = strings.TrimPrefix(strings.TrimSpace(p), "linux/"); p {
	case "amd64", "arm64":
		return p, nil
	}
	return "", fmt.Errorf("platform %q: use linux/amd64 or linux/arm64", p)
}

// stampPlatform gives every follower node and cluster without an arch of its own the stack's
// platform, keeping every other key of the design as it is. Members follow their cluster.
func stampPlatform(design json.RawMessage, arch string, skip map[string]bool) (json.RawMessage, error) {
	var d map[string]any
	if err := json.Unmarshal(design, &d); err != nil {
		return nil, err
	}
	stamp := func(key string, member bool) {
		items, _ := d[key].([]any)
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if member {
				if f, _ := m["frameId"].(string); f != "" {
					continue
				}
			}
			typ, _ := m["type"].(string)
			id, _ := m["id"].(string)
			if a, _ := m["arch"].(string); a == "" && platformFollower(typ) && !skip[id] {
				m["arch"] = arch
			}
		}
	}
	stamp("frames", false)
	stamp("nodes", true)
	return json.Marshal(d)
}

// effectiveArch is what a node or cluster of this type runs as, given its own arch and, for a
// cluster member, its cluster's.
func effectiveArch(typ, arch, frameArch string) string {
	if _, ok := kubernetesFrames[typ]; ok {
		return k8sArch()
	}
	if _, ok := amd64Only[typ]; ok {
		return "amd64"
	}
	if frameArch != "" {
		return frameArch
	}
	return archOr(arch)
}

// emulationWaitScale is how many times longer DBCanvas waits for a server running under
// emulation (an amd64 node on an arm64 host — Percona Server 5.7, which is built for amd64 only,
// always is one there) where a script waits a fixed time for it, such as the start after an
// XtraBackup restore. Emulated, mysqld starts and recovers several times more slowly, and a
// wait sized for a native server fails a restore that was only slow.
const emulationWaitScale = 3

// waitScale is emulationWaitScale for a node of this architecture that runs under emulation on
// this Docker host, and 1 otherwise.
func (a *App) waitScale(ctx context.Context, arch string) int {
	if _, emu := a.emulatedAmd64(ctx, arch); emu {
		return emulationWaitScale
	}
	return 1
}

// debianAmd64Only are the node types whose Debian and Ubuntu packages Percona publishes for amd64
// alone; on Oracle Linux they exist for both architectures.
var debianAmd64Only = map[string]string{"orchestrator": "Percona Orchestrator"}

// packageArchIssues refuses a node of debianAmd64Only on Debian or Ubuntu as arm64, which would
// otherwise fail at its package install.
func packageArchIssues(doc designDoc) []issue {
	var out []issue
	for _, n := range doc.Nodes {
		product, ok := debianAmd64Only[n.Type]
		if !ok || !isDebianOS(n.OS) || archOr(n.Arch) != "arm64" {
			continue
		}
		out = append(out, issue{Level: "error", Message: fmt.Sprintf(
			"%s: Percona publishes %s for Debian and Ubuntu on amd64 only — choose Oracle Linux, or amd64 (emulated on this host)",
			firstNonEmpty(n.Label, n.ID), product)})
	}
	return out
}
