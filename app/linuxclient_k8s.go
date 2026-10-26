package main

import (
	"context"
	"fmt"
	"strings"
)

// linuxclient_k8s.go — kubectl, Helm, cmctl and cfssl on a Linux Client node.
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
// cmctl, cert-manager's CLI (`cmctl check api`, `cmctl status certificate`, `cmctl renew`), is the
// tool for the cert-manager a K3D frame installs (k3dcertmanager.go). Since v2 it is released on its
// own, apart from cert-manager, and works against the cert-manager releases still supported — so,
// like Helm, it takes the current release.
//
// cfssl and cfssljson are one choice: CloudFlare's PKI toolkit, the pair the Percona operator docs
// use to make a CA and server certificates by hand (`cfssl gencert … | cfssljson -bare server`).
// Neither is useful alone, so they come from the same release together. Its assets carry the
// version in their names (cfssl_1.7.1_linux_amd64), so the script resolves the latest tag first.
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

// lcCmctlRelease is cmctl's latest-release download prefix. Both the binary and the release's
// checksums.txt come from it, so the checksum always belongs to the binary beside it.
const lcCmctlRelease = "https://github.com/cert-manager/cmctl/releases/latest/download"

// lcCfsslRepo is cfssl's GitHub releases page. /latest redirects to /tag/vX.Y.Z, which is how the
// script learns the version its asset names need; the binaries and checksums sit under /download.
const lcCfsslRepo = "https://github.com/cloudflare/cfssl/releases"

// lcK8sToolsRHEL / lcK8sToolsDebian install the download prerequisites. Everything after that is
// the same on both, so the shared body is appended to whichever package manager runs.
const lcK8sToolsRHEL = `set -e
dnf -y -q install curl tar ca-certificates bash-completion >/dev/null
`

// lcK8sToolsEL7 is the same prelude for CentOS 7, which has yum and no dnf. kubectl, Helm,
// cmctl and cfssl are static binaries, so nothing past the download tools differs.
const lcK8sToolsEL7 = `set -e
yum -y -q install curl tar ca-certificates bash-completion >/dev/null
`

const lcK8sToolsDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq curl tar ca-certificates bash-completion >/dev/null
`

// lcK8sToolsBody downloads what was asked for. It is one script for every tool so a node with
// several selected takes one round trip through the retrying runStep, and so the proxy is exported
// once.
//
// The kubectl, cmctl and cfssl checksums are verified, and a mismatch fails the step: an image that is not what
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
if [ -n "$CMCTL" ]; then
  cd /tmp
  curl -fsSL -o "cmctl_linux_$ARCH" "$CMCTL_RELEASE/cmctl_linux_$ARCH"
  curl -fsSL -o cmctl-checksums.txt "$CMCTL_RELEASE/checksums.txt"
  # checksums.txt lists every asset of the release; verify the one line for this binary, and
  # fail when there is no such line rather than let "nothing to check" pass as a pass.
  grep " cmctl_linux_$ARCH\$" cmctl-checksums.txt > cmctl.sha256 ||
    { echo "cmctl: checksums.txt has no entry for cmctl_linux_$ARCH" >&2; exit 1; }
  sha256sum -c cmctl.sha256 >/dev/null
  install -m 0755 "cmctl_linux_$ARCH" /usr/local/bin/cmctl
  rm -f "cmctl_linux_$ARCH" cmctl-checksums.txt cmctl.sha256
  /usr/local/bin/cmctl completion bash > /etc/bash_completion.d/cmctl 2>/dev/null || true
fi
if [ -n "$CFSSL" ]; then
  cd /tmp
  tag=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$CFSSL_REPO/latest")
  tag=${tag##*/}
  case "$tag" in v[0-9]*) ;; *) echo "cfssl: could not resolve the latest release ($tag)" >&2; exit 1 ;; esac
  ver=${tag#v}
  curl -fsSL -o cfssl-checksums.txt "$CFSSL_REPO/download/$tag/cfssl_${ver}_checksums.txt"
  for b in cfssl cfssljson; do
    curl -fsSL -o "${b}_${ver}_linux_$ARCH" "$CFSSL_REPO/download/$tag/${b}_${ver}_linux_$ARCH"
    grep " ${b}_${ver}_linux_$ARCH\$" cfssl-checksums.txt > "$b.sha256" ||
      { echo "cfssl: checksums.txt has no entry for ${b}_${ver}_linux_$ARCH" >&2; exit 1; }
    sha256sum -c "$b.sha256" >/dev/null
  done
  # Only once both verify does either reach PATH: they are one tool in two binaries.
  for b in cfssl cfssljson; do
    install -m 0755 "${b}_${ver}_linux_$ARCH" "/usr/local/bin/$b"
    rm -f "${b}_${ver}_linux_$ARCH" "$b.sha256"
  done
  rm -f cfssl-checksums.txt
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
	if n.LCCmctl {
		want = append(want, "cmctl (latest)")
	}
	if n.LCCfssl {
		want = append(want, "cfssl and cfssljson (latest)")
	}
	if len(want) == 0 {
		return
	}
	pr.phase("Installing "+lcJoinAnd(want), 85)
	pr.logln("installing " + strings.Join(want, ", "))

	arch := archOr(n.Arch)
	script := lcK8sToolsRHEL
	if isDebianOS(n.OS) {
		script = lcK8sToolsDebian
	} else if isEL7OS(n.OS) {
		script = lcK8sToolsEL7
	}
	env := []string{
		"KUBECTL_VERSION=" + map[bool]string{true: version, false: ""}[n.LCKubectl],
		"KUBECTL_URL=" + fmt.Sprintf(lcKubectlURL, version, arch),
		"HELM=" + map[bool]string{true: "1", false: ""}[n.LCHelm],
		"HELM_INSTALLER=" + lcHelmInstaller,
		"CMCTL=" + map[bool]string{true: "1", false: ""}[n.LCCmctl],
		"CMCTL_RELEASE=" + lcCmctlRelease,
		"CFSSL=" + map[bool]string{true: "1", false: ""}[n.LCCfssl],
		"CFSSL_REPO=" + lcCfsslRepo,
		"ARCH=" + arch,
		"DOMAIN=" + envOr("DOMAIN", "example.net"),
		"PROXY=" + map[bool]string{true: "http://intranet." + envOr("DOMAIN", "example.net") + ":3128", false: ""}[n.UseProxy],
	}
	if err := a.runStep(ctx, id, script+lcK8sToolsBody, env, pr.logln); err != nil {
		pr.logln("the client tools did not install: " + lastLines(err.Error(), 200) +
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
	if n.LCCmctl {
		// "Client Version: v2.6.1" — the last field is the release.
		cfg.CmctlVersion = a.lcToolVersion(ctx, id, "/usr/local/bin/cmctl version --client --short 2>/dev/null | awk '{print $NF}'")
	}
	if n.LCCfssl {
		// "Version: 1.7.1" from each; both must answer, or the pair is not there.
		if a.lcToolVersion(ctx, id, "/usr/local/bin/cfssljson -version 2>/dev/null") != "" {
			cfg.CfsslVersion = a.lcToolVersion(ctx, id, "/usr/local/bin/cfssl version 2>/dev/null | awk '/^Version:/{print $2}'")
		}
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
	switch {
	case n.LCCmctl && cfg.CmctlVersion == "":
		pr.logln("cmctl did not install")
	case cfg.CmctlVersion != "":
		pr.logln("cmctl " + cfg.CmctlVersion + " on PATH (bash completion)")
	}
	switch {
	case n.LCCfssl && cfg.CfsslVersion == "":
		pr.logln("cfssl and cfssljson did not install")
	case cfg.CfsslVersion != "":
		pr.logln("cfssl and cfssljson " + cfg.CfsslVersion + " on PATH")
	}
	// No tool has a cluster to talk to until it is given a kubeconfig, and this node cannot
	// safely fetch one (see the file comment). Say where it is instead of leaving a kubectl that
	// answers "connection refused" as the first experience of the node.
	if cfg.KubectlVersion != "" || cfg.HelmVersion != "" || cfg.CmctlVersion != "" {
		if k3dFrames := k3dFrameLabels(doc); len(k3dFrames) > 0 {
			pr.logln("no kubeconfig yet: copy one from " + strings.Join(k3dFrames, " / ") +
				" — the frame's server node has Kubeconfig (admin) and Users (per-role) in its panel")
		}
	}
}

// lcJoinAnd joins a list the way the phase line reads it: "a", "a and b", "a, b and c".
func lcJoinAnd(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
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
