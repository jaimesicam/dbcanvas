package main

import "fmt"

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
	"pmm":           "PMM",
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
			out = append(out, issue{Level: "info", Message: fmt.Sprintf("%s runs as %s under emulation on this %s Docker host: it works, more slowly", what, archOr(arch), host)})
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
		if bad(what, n.Arch) || n.FrameID != "" {
			continue // a member runs on its cluster's architecture
		}
		if product, ok := amd64Only[n.Type]; ok {
			if n.Arch == "arm64" {
				out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: %s runs on amd64 only", what, product)})
			}
			continue
		}
		if _, ok := kubernetesFrames[n.Type]; ok {
			continue
		}
		emulated(what, n.Arch)
	}
	return out
}
