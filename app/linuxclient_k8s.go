package main

import (
	"context"
	"fmt"
	"strings"
)

// linuxclient_k8s.go — kubectl and Helm on a Linux Client node.
//
// A Linux Client is the jump box: a bare host on the stack network with the Intranet's DNS and CA
// already trusted. Next to a K3D frame it is the obvious place to drive the cluster from — which
// is where kubectl and Helm belong, and why they are a design-time choice rather than something to
// install by hand every time a stack is rebuilt.
//
// ------------------------------------------------------------------- which kubectl
//
// The version follows the stack's own Kubernetes cluster. kubectl is supported one minor version
// either side of the API server it talks to, and a K3D frame pins its k3s tag — so "latest kubectl"
// is the one choice that can be wrong here, and it gets more wrong the longer a stack lives. When
// the design has a K3D frame, its k3s tag decides (`v1.36.4-k3s1` → `v1.36.4`); with several, the
// first one wins and the log says so; with none, the k3s catalog's latest stands in, because a
// client node with no cluster on the canvas yet is a stack being built up.
//
// Helm has no such constraint — it speaks the API server's REST, not a pinned version — so it takes
// the version its own installer picks, and what landed is recorded rather than assumed.
//
// ------------------------------------------------------------------- what is NOT done here
//
// No kubeconfig is written. The clusters are provisioned concurrently with this node, so anything
// this step copied would be a race — and the frame's own panel already hands out both an admin
// kubeconfig and per-user RBAC ones (k3d_rbac.go), which is where the choice of *identity* belongs.
// The log points at it.

// lcKubectlURL is where the official binaries live. Split out because the checksum sits beside the
// binary under the same prefix, and the two must not drift.
const lcKubectlURL = "https://dl.k8s.io/release/%s/bin/linux/%s/kubectl"

// lcHelmInstaller is Helm's own install script. Preferred over pinning a tarball URL: it resolves
// the current release, matches the architecture, and verifies the checksum — the three things a
// hand-rolled download gets wrong.
const lcHelmInstaller = "https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3"

// lcK8sToolsRHEL / lcK8sToolsDebian install the download prerequisites. Everything after that is
// the same on both, so the shared body is appended to whichever package manager runs.
const lcK8sToolsRHEL = `set -e
dnf -y -q install curl tar ca-certificates bash-completion >/dev/null
`

const lcK8sToolsDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq curl tar ca-certificates bash-completion >/dev/null
`

// lcK8sToolsBody downloads what was asked for. It is one script for both tools so a node with both
// selected takes one round trip through the retrying runStep, and so the proxy is exported once.
//
// The kubectl checksum is verified, and a mismatch fails the step: an image that is not what
// upstream published is not something to shrug at, and a half-downloaded binary that answers
// `kubectl version` with a shell error is worse than no kubectl at all.
const lcK8sToolsBody = `
if [ -n "$PROXY" ]; then
  export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"
  export no_proxy="localhost,127.0.0.1,.$DOMAIN" NO_PROXY="$no_proxy"
fi
if [ -n "$KUBECTL_VERSION" ]; then
  cd /tmp
  curl -fsSL -o kubectl "$KUBECTL_URL"
  curl -fsSL -o kubectl.sha256 "$KUBECTL_URL.sha256"
  echo "$(cat kubectl.sha256)  kubectl" | sha256sum -c - >/dev/null
  install -m 0755 kubectl /usr/local/bin/kubectl
  rm -f kubectl kubectl.sha256
  # Completion and the alias everyone types anyway. Sourced from a login shell, so the node's
  # terminal has them without anything to run first.
  /usr/local/bin/kubectl completion bash > /etc/bash_completion.d/kubectl 2>/dev/null || true
  printf 'alias k=kubectl\ncomplete -o default -F __start_kubectl k 2>/dev/null || true\n' > /etc/profile.d/kubectl.sh
fi
if [ -n "$HELM" ]; then
  curl -fsSL "$HELM_INSTALLER" -o /tmp/get-helm-3
  chmod +x /tmp/get-helm-3
  /tmp/get-helm-3 --no-sudo >/dev/null
  rm -f /tmp/get-helm-3
  /usr/local/bin/helm completion bash > /etc/bash_completion.d/helm 2>/dev/null || true
fi
`

// lcKubectlVersion is the kubectl release to install, and why that one.
//
// A K3D frame's k3s tag is `v1.36.4-k3s1`; kubectl's releases are `v1.36.4`, so the suffix goes.
// The frames are read in design order and the first one with a resolvable tag decides — a stack
// with two clusters on different minors cannot have one kubectl that is exactly right for both,
// and picking the first is at least predictable and stated in the log.
func lcKubectlVersion(doc designDoc) (version, why string) {
	cat := loadK3SCatalog()
	for _, f := range doc.Frames {
		if f.Type != "k3d" {
			continue
		}
		if tag, ok := cat.resolveK3SVersion(f.K3DK3SVersion); ok {
			return lcKubectlFromK3S(tag), "matching " + f.Label + " (k3s " + tag + ")"
		}
	}
	if cat.Latest != "" {
		return lcKubectlFromK3S(cat.Latest), "no Kubernetes cluster on the canvas yet — the catalog's newest k3s (" + cat.Latest + ")"
	}
	return "", ""
}

// lcKubectlFromK3S turns a k3s image tag into the Kubernetes release it is built from:
// `v1.36.4-k3s1` and `v1.36.4+k3s1` both become `v1.36.4`.
func lcKubectlFromK3S(tag string) string {
	tag = strings.TrimSpace(tag)
	for _, sep := range []string{"-k3s", "+k3s"} {
		if i := strings.Index(tag, sep); i > 0 {
			tag = tag[:i]
			break
		}
	}
	if tag != "" && !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return tag
}

// linuxClientInstallK8sTools installs what the node asked for and records what landed.
//
// It never fails the deploy, for the same reason the gdb step does not: a Linux Client with no
// kubectl on it is still a Linux Client, and the node's terminal is right there to finish the job.
// What must not happen is a node that *reports* kubectl and does not have it, so the recorded
// version is read back from the binary rather than assumed from what was requested.
func (a *App) linuxClientInstallK8sTools(ctx context.Context, id string, n designNode, doc designDoc, cfg *linuxClientConfig, pr *pxcProg) {
	version, why := lcKubectlVersion(doc)
	if n.LCKubectl && version == "" {
		pr.logln("kubectl skipped: no Kubernetes version could be resolved from the design or the catalog")
	}
	want := []string{}
	if n.LCKubectl && version != "" {
		want = append(want, "kubectl "+version+" ("+why+")")
	}
	if n.LCHelm {
		want = append(want, "Helm (latest)")
	}
	if len(want) == 0 {
		return
	}
	pr.phase("Installing "+strings.Join(want, " and "), 85)
	pr.logln("installing " + strings.Join(want, ", "))

	arch := archOr(n.Arch)
	script := lcK8sToolsRHEL
	if isDebianOS(n.OS) {
		script = lcK8sToolsDebian
	}
	env := []string{
		"KUBECTL_VERSION=" + map[bool]string{true: version, false: ""}[n.LCKubectl],
		"KUBECTL_URL=" + fmt.Sprintf(lcKubectlURL, version, arch),
		"HELM=" + map[bool]string{true: "1", false: ""}[n.LCHelm],
		"HELM_INSTALLER=" + lcHelmInstaller,
		"DOMAIN=" + envOr("DOMAIN", "example.net"),
		"PROXY=" + map[bool]string{true: "http://intranet." + envOr("DOMAIN", "example.net") + ":3128", false: ""}[n.UseProxy],
	}
	if err := a.runStep(ctx, id, script+lcK8sToolsBody, env, pr.logln); err != nil {
		pr.logln("the Kubernetes client tools did not install: " + lastLines(err.Error(), 200) +
			" — the node is up regardless; install them from its terminal")
	}

	// Read back what is actually on the node. "The step exited 0" and "kubectl runs" are
	// different statements, and the panel is about to make the second one.
	if n.LCKubectl {
		cfg.KubectlVersion = a.lcToolVersion(ctx, id, "/usr/local/bin/kubectl version --client -o yaml | awk '/gitVersion/{print $2; exit}'")
	}
	if n.LCHelm {
		cfg.HelmVersion = a.lcToolVersion(ctx, id, "/usr/local/bin/helm version --short 2>/dev/null")
	}
	switch {
	case n.LCKubectl && cfg.KubectlVersion == "":
		pr.logln("kubectl did not install")
	case cfg.KubectlVersion != "":
		pr.logln("kubectl " + cfg.KubectlVersion + " on PATH (alias k, bash completion)")
	}
	switch {
	case n.LCHelm && cfg.HelmVersion == "":
		pr.logln("helm did not install")
	case cfg.HelmVersion != "":
		pr.logln("helm " + cfg.HelmVersion + " on PATH")
	}
	// Neither tool has a cluster to talk to until it is given a kubeconfig, and this node cannot
	// safely fetch one (see the file comment). Say where it is instead of leaving a kubectl that
	// answers "connection refused" as the first experience of the node.
	if cfg.KubectlVersion != "" || cfg.HelmVersion != "" {
		if k3dFrames := k3dFrameLabels(doc); len(k3dFrames) > 0 {
			pr.logln("no kubeconfig yet: copy one from " + strings.Join(k3dFrames, " / ") +
				" — the frame's server node has Kubeconfig (admin) and Users (per-role) in its panel")
		}
	}
}

// k3dFrameLabels names the Kubernetes clusters on the canvas, for the hint above.
func k3dFrameLabels(doc designDoc) []string {
	var out []string
	for _, f := range doc.Frames {
		if f.Type == "k3d" {
			out = append(out, f.Label)
		}
	}
	return out
}

// lcToolVersion runs a one-liner on the node and returns its trimmed first line, or "" if it did
// not work — which is the answer that matters: the tool is not there.
func (a *App) lcToolVersion(ctx context.Context, id, script string) string {
	res, err := a.engCtx(ctx).Exec(ctx, id, []string{"bash", "-lc", script}, nil)
	if err != nil || res.Code != 0 {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(res.Stdout), "\n", 2)[0])
}
