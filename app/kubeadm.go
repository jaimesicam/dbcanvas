package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// kubeadm.go — the Kubeadm Cluster frame: Kubernetes built the way kubeadm builds it.
//
// A K3D frame asks k3d for a cluster and gets k3s, which hides how a cluster is put together. This
// frame is the opposite: each member is an Ubuntu 24.04 node DBCanvas creates itself, with systemd,
// containerd, and kubeadm, kubelet and kubectl from pkgs.k8s.io at the release the frame names. The
// first member (by label) is the control plane and the console — `kubeadm init` ran there,
// /etc/kubernetes/admin.conf is root's kubeconfig, etcdctl matches the cluster's etcd, and it can
// ssh to every worker by name. The rest are workers.
//
// How far the deploy goes is the frame's stage (designFrame.KubeadmStage):
//
//	ready        init, every worker joined, the pod network installed, every node Ready
//	initialized  init and the joins, no pod network: the nodes are NotReady and CoreDNS Pending
//	prepared     packages, containerd, the config file and the SSH keys — nothing run
//
// A node is a container, not a machine, and four things differ. Each was found by running kubeadm
// in one, and each is handled so that kubeadm's own commands — init, join, upgrade — work exactly
// as documented:
//
//   - Native architecture (K8S_PLATFORM). Under emulation containerd cannot create a pod sandbox:
//     "seccomp is not supported".
//   - A private cgroup namespace (ContainerSpec.CgroupnsPrivate), so the kubelet's kubepods.slice
//     is inside the container's cgroup rather than at the Docker host's root.
//   - The image's entrypoint (images/kubeadm.Dockerfile): shared mount propagation, which Calico
//     needs, and a kernel config where kubeadm's preflight looks for one.
//   - The cluster configuration (kubeadmInitConfig): swap left on, the kubelet's resolv.conf, and
//     kube-proxy leaving the host-wide conntrack table alone.
//
// The configuration is written to /root/kubeadm-config.yaml on the control plane at every stage,
// so a cluster built by hand from "prepared" gets the same three settings: `kubeadm init --config
// /root/kubeadm-config.yaml`.

const (
	kubeadmOS      = "ubuntu"
	kubeadmOSVer   = "24.04"
	kubeadmAPIPort = 6443
	// The pod and Service ranges. Docker's default address pools are 172.17–31.x and 192.168.x,
	// so these do not meet a stack network; a daemon configured with 10.x pools is caught at
	// deploy (kubeadmCIDROverlap) rather than left to route pods into the stack.
	kubeadmPodCIDR     = "10.244.0.0/16"
	kubeadmServiceCIDR = "10.96.0.0/12"
	kubeadmConfigFile  = "/root/kubeadm-config.yaml"
	// kubeadmAdminConf is the cluster-admin kubeconfig kubeadm init writes on the control plane.
	kubeadmAdminConf = "/etc/kubernetes/admin.conf"
	// kubeadmResolvConf is the kubelet's resolv.conf: the Intranet. Docker's embedded resolver
	// (127.0.0.11), which a node's own resolv.conf names after every restart, answers only inside
	// the node's network namespace — CoreDNS would forward to itself, and exits on the loop.
	kubeadmResolvConf = "/etc/kubernetes/resolv.conf"
	// The pod networks a ready cluster can get, pinned so a deploy is the same next week.
	kubeadmCalicoVersion  = "v3.33.0"
	kubeadmFlannelVersion = "v0.28.10"
	// kubeadmMinMinor is the oldest minor whose kubeadm reads kubeadm.k8s.io/v1beta4, the API
	// the configuration is written in.
	kubeadmMinMinor = 31
	kubeadmMaxNodes = 4
	// How long the pod network has to bring every node Ready.
	kubeadmReadyTimeout = 10 * time.Minute
)

// kubeadmImage is the node image (images/kubeadm.Dockerfile), built for K8S_PLATFORM by
// `make kubeadm-image`. Must match KUBEADM_TAG in images/service.sh.
func kubeadmImage() string {
	return "dbcanvas-kubeadm:" + kubeadmOS + "-" + kubeadmOSVer + "-" + k8sArch() + "-" + imageRelease()
}

// kubeadmStage is the frame's stage with the default spelled out.
func kubeadmStage(f designFrame) string {
	if f.KubeadmStage == "" {
		return "ready"
	}
	return f.KubeadmStage
}

// kubeadmCNI is the frame's pod network with the default spelled out.
func kubeadmCNI(f designFrame) string {
	if f.KubeadmCNI == "" {
		return "calico"
	}
	return f.KubeadmCNI
}

var (
	kubeadmStages = []string{"ready", "initialized", "prepared"}
	kubeadmCNIs   = []string{"calico", "flannel"}
)

func kubeadmOneOf(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// kubeadmConfig is the non-secret profile stored on every member.
type kubeadmConfig struct {
	Image    string `json:"image"`
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	IP       string `json:"ip,omitempty"`
	// Role is "control-plane" or "worker".
	Role         string `json:"role"`
	Cluster      string `json:"cluster"`
	Version      string `json:"version"` // vX.Y.Z
	Stage        string `json:"stage"`
	CNI          string `json:"cni,omitempty"` // the pod network, at the ready stage only
	ControlPlane string `json:"controlPlane"`  // the control plane's FQDN
	PodCIDR      string `json:"podCidr"`
	ServiceCIDR  string `json:"serviceCidr"`
	ConfigFile   string `json:"configFile,omitempty"` // on the control plane
}

// kubeadmMembers are a frame's members in label order: the first is the control plane.
func kubeadmMembers(f designFrame, doc designDoc) []designNode {
	var out []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == f.ID && n.Type == "kubeadm" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// kubeadmMinor splits vX.Y.Z into its apt repository ("v1.37") and its package version ("1.37.2").
var kubeadmVersionRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

func kubeadmMinor(v string) (repo, pkg string, minor int, ok bool) {
	m := kubeadmVersionRe.FindStringSubmatch(v)
	if m == nil {
		return "", "", 0, false
	}
	minor, _ = strconv.Atoi(m[2])
	return "v" + m[1] + "." + m[2], m[1] + "." + m[2] + "." + m[3], minor, true
}

// kubeadmResolveVersion is the release a frame installs: the one it names when the catalog has it,
// or the catalog's newest for "".
func kubeadmResolveVersion(f designFrame) (string, bool) {
	return loadKubernetesCatalog().resolveK3SVersion(f.KubeadmVersion)
}

// kubeadmFrameIssues validates one Kubeadm frame: its shape and options (kubeadmShapeIssues), and
// what the Docker host has to offer it — the node image, and a native architecture.
func (a *App) kubeadmFrameIssues(ctx context.Context, f designFrame, doc designDoc) []issue {
	out := kubeadmShapeIssues(f, doc)
	name := "Kubeadm cluster " + f.Label
	if ok, _ := a.engCtx(ctx).ImageExists(ctx, kubeadmImage()); !ok {
		out = append(out, missingImageIssue("kubeadm"))
	}
	// A node under emulation cannot run a pod: say so before the deploy, with the fix.
	if host := a.engCtx(ctx).HostArch(ctx); host != "" {
		if msg := kubeadmEmulationIssue(host); msg != "" {
			out = append(out, issue{Level: "error", Message: name + ": " + msg})
		}
	}
	return out
}

// kubeadmEmulationIssue says why nodes for K8S_PLATFORM cannot run on a Docker host of
// architecture host (as /info reports it: x86_64, aarch64, arm64), or "" when they can.
func kubeadmEmulationIssue(host string) string {
	native := "amd64"
	if strings.Contains(host, "arm") || strings.Contains(host, "aarch64") {
		native = "arm64"
	}
	if native == k8sArch() {
		return ""
	}
	return fmt.Sprintf("Docker runs on %s, and K8S_PLATFORM is linux/%s — under emulation containerd cannot create a pod (\"seccomp is not supported\"). Set K8S_PLATFORM=linux/%s in .env and restart DBCanvas", native, k8sArch(), native)
}

// kubeadmShapeIssues are the checks that need nothing but the design.
func kubeadmShapeIssues(f designFrame, doc designDoc) []issue {
	var out []issue
	name := "Kubeadm cluster " + f.Label
	members := kubeadmMembers(f, doc)
	if len(members) < 1 || len(members) > kubeadmMaxNodes {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s has %d nodes; it takes 1 to %d (a control plane and up to %d workers)", name, len(members), kubeadmMaxNodes, kubeadmMaxNodes-1)})
	}
	if !kubeadmOneOf(kubeadmStages, kubeadmStage(f)) {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: stage %q is not one of %s", name, f.KubeadmStage, strings.Join(kubeadmStages, ", "))})
	}
	if !kubeadmOneOf(kubeadmCNIs, kubeadmCNI(f)) {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: pod network %q is not one of %s", name, f.KubeadmCNI, strings.Join(kubeadmCNIs, ", "))})
	}
	if v, ok := kubeadmResolveVersion(f); !ok {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: Kubernetes %s is not in the catalog (GET /api/catalog/kubernetes)", name, f.KubeadmVersion)})
	} else if _, _, minor, ok := kubeadmMinor(v); !ok || minor < kubeadmMinMinor {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: Kubernetes %s is older than 1.%d, the first kubeadm that reads the configuration DBCanvas writes", name, v, kubeadmMinMinor)})
	}
	for _, n := range members {
		if n.MemoryGB > 0 && n.MemoryGB < 2 {
			out = append(out, issue{Level: "warning", Message: fmt.Sprintf("%s: %s has %d GB of memory; kubeadm wants 2 GB on every node, and the control plane alone uses about 1.5", name, n.Label, n.MemoryGB)})
		}
		if n.CPUs == 1 && n.Label == members[0].Label {
			out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: %s is the control plane and has 1 CPU; kubeadm refuses a control plane with fewer than 2", name, n.Label)})
		}
	}
	return out
}

// k8sFrameAndServer is k3dFrameAndServer for the features that work on any Kubernetes frame
// (Kubernetes States, a Linux Client's kubeconfig): a K3D frame's running server, or a Kubeadm
// frame's running control plane once it has been initialized — a "prepared" cluster has no API
// server until someone runs kubeadm init, and is not one to watch until then.
func (a *App) k8sFrameAndServer(st Stack, doc designDoc, fid string) (designFrame, Deployment, error) {
	for _, f := range doc.Frames {
		if f.ID != fid || f.Type != "kubeadm" {
			continue
		}
		cp := kubeadmFindControlPlane(a, st, f, doc)
		if cp == nil {
			return f, Deployment{}, fmt.Errorf("no running control plane found for this cluster")
		}
		dep, err := a.store.GetDeployment(st.ID, cp.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			return f, Deployment{}, fmt.Errorf("the control plane %s is not running", cp.Label)
		}
		return f, dep, nil
	}
	return a.k3dFrameAndServer(st, doc, fid)
}

// kubeadmInitialized says whether a control plane has run kubeadm init.
func (a *App) kubeadmInitialized(ctx context.Context, cid string) bool {
	res, err := a.engCtx(ctx).Exec(ctx, cid, []string{"test", "-f", kubeadmAdminConf}, nil)
	return err == nil && res.Code == 0
}

// kubeadmCIDROverlap names the cluster range the stack network overlaps, or "" when neither does.
func kubeadmCIDROverlap(subnet string) string {
	_, sn, err := net.ParseCIDR(subnet)
	if err != nil {
		return ""
	}
	for _, c := range []string{kubeadmPodCIDR, kubeadmServiceCIDR} {
		_, cn, _ := net.ParseCIDR(c)
		if sn.Contains(cn.IP) || cn.Contains(sn.IP) {
			return c
		}
	}
	return ""
}

// kubeadmInitConfig is /root/kubeadm-config.yaml: what `kubeadm init --config` builds the control
// plane from. The comment at its top is for whoever opens it at the console.
func kubeadmInitConfig(version, host, fqdn, ip string) string {
	return fmt.Sprintf(`# Written by DBCanvas: build the control plane with
#
#   kubeadm init --config %[5]s
#
# Edit it first to change how. The workers then join with the command init prints (or
# "kubeadm token create --print-join-command"), and need no file of their own: the kubelet
# settings below reach them through the cluster's kubelet-config ConfigMap.
#
# Three settings are here because the nodes are containers rather than machines:
#   failSwapOn: false      a container cannot turn the host's swap off.
#   resolvConf             Docker's resolver (127.0.0.11) answers only inside the node's own
#                          network namespace; pods would send CoreDNS back to itself. The
#                          file names the stack's DNS server instead.
#   conntrack.maxPerCore   nf_conntrack_max is a host-wide setting a container cannot change;
#                          0 leaves it alone instead of kube-proxy exiting over it.
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: %[3]s
  bindPort: %[6]d
nodeRegistration:
  name: %[2]s
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: %[1]s
controlPlaneEndpoint: %[4]s:%[6]d
apiServer:
  certSANs:
    - %[4]s
    - %[2]s
networking:
  podSubnet: %[7]s
  serviceSubnet: %[8]s
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
failSwapOn: false
resolvConf: %[9]s
---
apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
conntrack:
  maxPerCore: 0
`, version, host, ip, fqdn, kubeadmConfigFile, kubeadmAPIPort, kubeadmPodCIDR, kubeadmServiceCIDR, kubeadmResolvConf)
}

// kubeadmPrepScript installs one node's Kubernetes packages at the frame's release and points the
// kubelet at the stack. Safe to run again: apt and the files it writes are idempotent.
//
// Env: REPO (v1.37), VER (1.37.2), NODE_IP, DNS_IP, DOMAIN.
const kubeadmPrepScript = `set -e
export DEBIAN_FRONTEND=noninteractive
mkdir -p /etc/apt/keyrings /etc/kubernetes /etc/bash_completion.d
curl -fsSL --retry 3 "https://pkgs.k8s.io/core:/stable:/$REPO/deb/Release.key" \
  | gpg --dearmor --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/$REPO/deb/ /" \
  > /etc/apt/sources.list.d/kubernetes.list
apt-get update -qq
# The package revision (the -1.1 of 1.37.2-1.1) differs between releases, so ask apt for it.
PKG="$(apt-cache madison kubeadm | awk -v v="$VER" '$3 ~ "^"v"-" {print $3; exit}')"
if [ -z "$PKG" ]; then echo "kubeadm $VER is not in the $REPO repository" >&2; exit 1; fi
apt-get install -y -qq --allow-downgrades --allow-change-held-packages \
  kubelet="$PKG" kubeadm="$PKG" kubectl="$PKG" cri-tools >/dev/null
apt-mark hold kubelet kubeadm kubectl >/dev/null
printf 'search %s\nnameserver %s\n' "$DOMAIN" "$DNS_IP" > ` + kubeadmResolvConf + `
printf 'KUBELET_EXTRA_ARGS=--node-ip=%s\n' "$NODE_IP" > /etc/default/kubelet
# The pause image this kubeadm expects, so its preflight does not warn about containerd's.
PAUSE="$(kubeadm config images list --kubernetes-version "v$VER" 2>/dev/null | grep '/pause:' | head -1)"
if [ -n "$PAUSE" ] && ! grep -q "sandbox = '$PAUSE'" /etc/containerd/config.toml; then
  sed -i -E "s#^([[:space:]]*sandbox[[:space:]]*=[[:space:]]*).*#\1'$PAUSE'#" /etc/containerd/config.toml
  systemctl restart containerd
fi
sysctl --system >/dev/null 2>&1 || true
kubectl completion bash > /etc/bash_completion.d/kubectl
kubeadm completion bash > /etc/bash_completion.d/kubeadm
crictl completion bash > /etc/bash_completion.d/crictl 2>/dev/null || true
if ! grep -q 'dbcanvas-kubeadm' /root/.bashrc; then
  cat >> /root/.bashrc <<'RC'
# dbcanvas-kubeadm
[ -f /usr/share/bash-completion/bash_completion ] && . /usr/share/bash-completion/bash_completion
alias k=kubectl
complete -o default -F __start_kubectl k
RC
fi
systemctl enable kubelet >/dev/null 2>&1
systemctl is-active -q containerd
echo "kubeadm $(kubeadm version -o short), containerd $(containerd --version | awk '{print $3}')"
`

// kubeadmEtcdctlScript puts etcdctl and etcdutl, at the version of the etcd the cluster runs, on
// the control plane — a snapshot is taken from the node, and a restore has to be. Env: VER.
const kubeadmEtcdctlScript = `set -e
command -v etcdctl >/dev/null && exit 0
E="$(kubeadm config images list --kubernetes-version "v$VER" | sed -n 's#.*/etcd:\([0-9.]*\)-.*#\1#p' | head -1)"
[ -n "$E" ] || { echo "no etcd image for v$VER" >&2; exit 1; }
A="$(dpkg --print-architecture)"
D="$(mktemp -d)"
curl -fsSL --retry 3 "https://github.com/etcd-io/etcd/releases/download/v$E/etcd-v$E-linux-$A.tar.gz" | tar xz -C "$D"
install -m 0755 "$D/etcd-v$E-linux-$A/etcdctl" "$D/etcd-v$E-linux-$A/etcdutl" /usr/local/bin/
rm -rf "$D"
echo "etcdctl $E"
`

// kubeadmSSHKeyScript makes the control plane's key, once, and prints its public half.
const kubeadmSSHKeyScript = `set -e
mkdir -p /root/.ssh && chmod 700 /root/.ssh
[ -f /root/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -C "root@$(hostname)" -f /root/.ssh/id_ed25519
systemctl enable --now ssh >/dev/null 2>&1 || true
cat /root/.ssh/id_ed25519.pub
`

// kubeadmTrustScript adds the control plane's key to a worker's authorized_keys. Env: KEY.
const kubeadmTrustScript = `set -e
mkdir -p /root/.ssh && chmod 700 /root/.ssh
touch /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys
grep -qxF "$KEY" /root/.ssh/authorized_keys || echo "$KEY" >> /root/.ssh/authorized_keys
systemctl enable --now ssh >/dev/null 2>&1
`

// kubeadmKnownHostsScript records the workers' host keys on the control plane, by short name and
// FQDN, so the first ssh asks nothing. Env: HOSTS (space-separated).
const kubeadmKnownHostsScript = `set -e
for h in $HOSTS; do
  for i in 1 2 3 4 5; do ssh-keyscan -T 5 "$h" 2>/dev/null | grep -q . && break; sleep 2; done
done
ssh-keyscan -T 5 $HOSTS 2>/dev/null | sort -u > /root/.ssh/known_hosts.dbcanvas
cat /root/.ssh/known_hosts /root/.ssh/known_hosts.dbcanvas 2>/dev/null | sort -u > /root/.ssh/known_hosts.new || true
mv /root/.ssh/known_hosts.new /root/.ssh/known_hosts && rm -f /root/.ssh/known_hosts.dbcanvas
chmod 600 /root/.ssh/known_hosts
`

// kubeadmInitScript builds the control plane once and gives root the admin kubeconfig.
const kubeadmInitScript = `set -e
if [ ! -f /etc/kubernetes/admin.conf ]; then
  kubeadm init --config ` + kubeadmConfigFile + `
fi
mkdir -p /root/.kube
cp -f /etc/kubernetes/admin.conf /root/.kube/config
chmod 600 /root/.kube/config
`

// kubeadmJoinScript joins a worker once. Env: JOIN, the command `kubeadm token create
// --print-join-command` printed on the control plane.
const kubeadmJoinScript = `set -e
[ -f /etc/kubernetes/kubelet.conf ] && exit 0
$JOIN
`

// kubeadmCNIScript installs the pod network from the control plane. Env: CNI, VER (the CNI's
// release), POD_CIDR.
const kubeadmCNIScript = `set -e
export KUBECONFIG=/etc/kubernetes/admin.conf
case "$CNI" in
calico)
  B="https://raw.githubusercontent.com/projectcalico/calico/$VER/manifests"
  # The CRDs are too large for client-side apply's last-applied annotation.
  curl -fsSL --retry 3 "$B/operator-crds.yaml" | kubectl apply --server-side --force-conflicts -f - >/dev/null
  curl -fsSL --retry 3 "$B/tigera-operator.yaml" | kubectl apply --server-side --force-conflicts -f - >/dev/null
  kubectl wait --for=condition=Established crd/installations.operator.tigera.io --timeout=120s >/dev/null
  curl -fsSL --retry 3 "$B/custom-resources.yaml" | sed "s#192.168.0.0/16#$POD_CIDR#" | kubectl apply -f - >/dev/null
  ;;
flannel)
  curl -fsSL --retry 3 "https://github.com/flannel-io/flannel/releases/download/$VER/kube-flannel.yml" \
    | sed "s#10.244.0.0/16#$POD_CIDR#" | kubectl apply -f - >/dev/null
  ;;
*) echo "unknown pod network $CNI" >&2; exit 1 ;;
esac
echo "$CNI $VER applied"
`

// kubeadmWaitReadyScript waits for every node and for CoreDNS. Env: NODES (the expected count),
// TIMEOUT (seconds).
const kubeadmWaitReadyScript = `set -e
export KUBECONFIG=/etc/kubernetes/admin.conf
end=$(( $(date +%s) + TIMEOUT ))
until [ "$(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready ')" -ge "$NODES" ]; do
  if [ "$(date +%s)" -ge "$end" ]; then kubectl get nodes >&2; exit 1; fi
  sleep 5
done
kubectl -n kube-system rollout status deploy/coredns --timeout=300s >/dev/null
kubectl get nodes --no-headers | awk '{print $1" "$2" "$5}'
`

// kubeadmTail is the last n lines of a command's output. (lastLines, elsewhere, keeps the last n
// characters — right for an error message, wrong for picking a line out of output.)
func kubeadmTail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// kubeadmExecOnce runs a step that must not be retried blindly (kubeadm init and join leave state
// behind on failure), logging its output. The error carries the end of what it printed.
func (a *App) kubeadmExecOnce(ctx context.Context, cid, script string, env []string, pr *pxcProg) (string, error) {
	res, err := a.engCtx(ctx).Exec(ctx, cid, []string{"bash", "-c", script}, env)
	out := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	if err != nil {
		return out, err
	}
	if res.Code != 0 {
		return out, fmt.Errorf("exit %d: %s", res.Code, kubeadmTail(out, 12))
	}
	return out, nil
}

// kubeadmBuildNode creates one member's container and installs its packages. It returns the
// container id and the node's address on the stack network.
func (a *App) kubeadmBuildNode(ctx context.Context, st Stack, n designNode, host, intranetIP, domain, version string, pr *pxcProg) (string, string, error) {
	repo, pkg, _, _ := kubeadmMinor(version)
	pr.phase("Creating container", 15)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
		a.engCtx(ctx).ContainerRemove(ctx, cid)
	}
	spec := ContainerSpec{
		Name: name, Image: kubeadmImage(), Hostname: host, Privileged: true, CgroupnsPrivate: true,
		Network: networkName(st.ID), Aliases: []string{host},
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
		Binds: []string{"/lib/modules:/lib/modules:ro"},
	}
	applyVMSize(&spec, n.limits())
	cid, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
	if err != nil {
		return "", "", pr.fail("create container: %v", err)
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, cid); err != nil {
		return "", "", pr.fail("start container: %v", err)
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: cid, State: DeployProvisioning, Config: a.depConfig(st.ID, n.ID)})
	a.pointResolverAtIntranet(ctx, cid, intranetIP, domain)
	ip, err := a.engCtx(ctx).ContainerIP(ctx, cid, networkName(st.ID))
	if err != nil || ip == "" {
		return "", "", pr.fail("node address on the stack network: %v", err)
	}
	pr.phase("Waiting for systemd", 20)
	if err := a.runStep(ctx, cid, `for i in $(seq 1 30); do systemctl is-active -q containerd && exit 0; sleep 1; done; exit 1`, nil, pr.logln); err != nil {
		return "", "", pr.fail("containerd did not start: %v", err)
	}
	pr.phase("Installing Kubernetes "+version, 30)
	env := []string{"REPO=" + repo, "VER=" + pkg, "NODE_IP=" + ip, "DNS_IP=" + intranetIP, "DOMAIN=" + domain}
	res, err := a.engCtx(ctx).Exec(ctx, cid, []string{"bash", "-c", kubeadmPrepScript}, env)
	if err != nil || res.Code != 0 {
		// apt reaches the network; a second and third try is what runStep is for.
		if err := a.runStep(ctx, cid, kubeadmPrepScript, env, pr.logln); err != nil {
			return "", "", pr.fail("install Kubernetes %s: %v", version, err)
		}
	} else if s := strings.TrimSpace(res.Stdout); s != "" {
		pr.logln(kubeadmTail(s, 2))
	}
	return cid, ip, nil
}

// kubeadmRecord stores a member's profile.
func (a *App) kubeadmRecord(st Stack, n designNode, cid, state string, cfg kubeadmConfig) {
	b, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: cid, State: state, Config: b})
}

// provisionKubeadmFrame builds a whole Kubeadm cluster: every member's node in parallel, then the
// control plane, the joins and the pod network, as far as the frame's stage says.
func (a *App) provisionKubeadmFrame(st Stack, frame designFrame, doc designDoc) {
	members := kubeadmMembers(frame, doc)
	if len(members) == 0 {
		return
	}
	a.kubeadmProvision(st, frame, doc, members, nil)
}

// joinKubeadmFrame builds members added to a running cluster: each is a worker, prepared like the
// others and — when the cluster was initialized — joined to it.
func (a *App) joinKubeadmFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	a.kubeadmProvision(st, frame, doc, fresh, kubeadmFindControlPlane(a, st, frame, doc))
}

// kubeadmFindControlPlane is the running control plane of a frame, from its members' profiles.
func kubeadmFindControlPlane(a *App, st Stack, frame designFrame, doc designDoc) *designNode {
	for _, n := range kubeadmMembers(frame, doc) {
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" {
			continue
		}
		var cfg kubeadmConfig
		if json.Unmarshal(dep.Config, &cfg) == nil && cfg.Role == "control-plane" {
			n := n
			return &n
		}
	}
	return nil
}

// kubeadmProvision builds the nodes in build. cp is the running control plane when they join an
// existing cluster, or nil when build is the whole cluster (its first member is then the control
// plane).
func (a *App) kubeadmProvision(st Stack, frame designFrame, doc designDoc, build []designNode, cp *designNode) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	stage, cni := kubeadmStage(frame), kubeadmCNI(frame)
	version, _ := kubeadmResolveVersion(frame)
	joining := cp != nil
	if !joining {
		c := build[0]
		cp = &c
	}
	cpHost := hosts[cp.ID]
	cpFQDN := fqdnOf(cpHost, domain)

	cfgOf := func(n designNode) kubeadmConfig {
		c := kubeadmConfig{
			Image: kubeadmImage(), Hostname: hosts[n.ID], FQDN: fqdnOf(hosts[n.ID], domain),
			Role: "worker", Cluster: frame.Label, Version: version, Stage: stage,
			ControlPlane: cpFQDN, PodCIDR: kubeadmPodCIDR, ServiceCIDR: kubeadmServiceCIDR,
		}
		if stage == "ready" {
			c.CNI = cni
		}
		if n.ID == cp.ID {
			c.Role, c.ConfigFile = "control-plane", kubeadmConfigFile
		}
		return c
	}
	cfgs := map[string]kubeadmConfig{}
	for _, n := range build {
		cfgs[n.ID] = cfgOf(n)
		a.kubeadmRecord(st, n, "", DeployPending, cfgs[n.ID])
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range build {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		failAll := func(format string, args ...any) {
			for _, n := range build {
				progs[n.ID].fail(format, args...)
			}
		}
		_, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			failAll("%v", err)
			return
		}
		if sub, err := a.engCtx(ctx).NetworkSubnet(ctx, networkName(st.ID)); err == nil {
			if c := kubeadmCIDROverlap(sub); c != "" {
				failAll("the stack network %s overlaps the cluster's range %s — Docker's address pools put this stack in 10.x; change default-address-pools in the daemon's configuration", sub, c)
				return
			}
		}

		// Phase 1, in parallel: every node built and its packages installed.
		cids, ips := map[string]string{}, map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		ok := true
		for _, n := range build {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				cid, ip, err := a.kubeadmBuildNode(ctx, st, n, hosts[n.ID], intranetIP, domain, version, progs[n.ID])
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					ok = false
					return
				}
				cids[n.ID], ips[n.ID] = cid, ip
				c := cfgs[n.ID]
				c.IP = ip
				cfgs[n.ID] = c
				a.kubeadmRecord(st, n, cid, DeployProvisioning, c)
			}(n)
		}
		wg.Wait()
		if !ok {
			for _, n := range build {
				if cids[n.ID] != "" {
					progs[n.ID].fail("another node of %s failed to build", frame.Label)
				}
			}
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// The control plane: its container (built now, or already running when joining).
		cpCID := cids[cp.ID]
		cpProg := progs[cp.ID]
		if joining {
			dep, err := a.store.GetDeployment(st.ID, cp.ID)
			if err != nil || dep.ContainerID == "" {
				failAll("the control plane %s is not running", cp.Label)
				return
			}
			cpCID = dep.ContainerID
			// The control plane's steps on the joiners' behalf land in the first joiner's log.
			cpProg = a.sideProg(st.ID, cp.ID, cp.Label, progs[build[0].ID])
		}
		var workers []designNode
		for _, n := range build {
			if n.ID != cp.ID {
				workers = append(workers, n)
			}
		}

		// Phase 2: the control plane's tools, its configuration file and its SSH key.
		if !joining {
			cpProg.phase("Writing "+kubeadmConfigFile, 50)
			conf := kubeadmInitConfig(version, cpHost, cpFQDN, ips[cp.ID])
			if err := a.engCtx(ctx).CopyFile(ctx, cpCID, "/root", "kubeadm-config.yaml", 0o644, []byte(conf)); err != nil {
				failAll("write %s on %s: %v", kubeadmConfigFile, cp.Label, err)
				return
			}
			_, pkg, _, _ := kubeadmMinor(version)
			if out, err := a.kubeadmExecOnce(ctx, cpCID, kubeadmEtcdctlScript, []string{"VER=" + pkg}, cpProg); err != nil {
				cpProg.logln("etcdctl not installed (snapshots work through the etcd pod): " + err.Error())
			} else if out != "" {
				cpProg.logln(out)
			}
		}
		key, err := a.kubeadmExecOnce(ctx, cpCID, kubeadmSSHKeyScript, nil, cpProg)
		if err != nil {
			failAll("SSH key on %s: %v", cp.Label, err)
			return
		}
		key = strings.TrimSpace(kubeadmTail(key, 1))
		var sshHosts []string
		for _, n := range workers {
			if err := a.runStep(ctx, cids[n.ID], kubeadmTrustScript, []string{"KEY=" + key}, progs[n.ID].logln); err != nil {
				failAll("%s could not trust %s's SSH key: %v", n.Label, cp.Label, err)
				return
			}
			sshHosts = append(sshHosts, hosts[n.ID], fqdnOf(hosts[n.ID], domain))
		}
		if len(sshHosts) > 0 {
			if _, err := a.kubeadmExecOnce(ctx, cpCID, kubeadmKnownHostsScript, []string{"HOSTS=" + strings.Join(sshHosts, " ")}, cpProg); err != nil {
				cpProg.logln("known_hosts not written; the first ssh to a worker will ask: " + err.Error())
			}
		}

		finish := func(msg string) {
			for _, n := range build {
				a.kubeadmRecord(st, n, cids[n.ID], DeployRunning, cfgs[n.ID])
				pr := progs[n.ID]
				pr.phase("Running", 100)
				pr.p.Message = msg
				pr.save()
			}
			a.reconcileStackDNS(ctx, st.ID)
			log.Printf("stack %d kubeadm %s: %s", st.ID, frame.Label, msg)
		}
		if stage == "prepared" {
			finish("prepared — run kubeadm init on " + cp.Label)
			return
		}

		// Phase 3: kubeadm init (a fresh cluster), then every worker joins.
		if !joining {
			cpProg.phase("kubeadm init", 60)
			out, err := a.kubeadmExecOnce(ctx, cpCID, kubeadmInitScript, nil, cpProg)
			if err != nil {
				cpProg.logln(kubeadmTail(out, 30))
				failAll("kubeadm init on %s: %v", cp.Label, err)
				return
			}
			cpProg.logln("control plane initialized")
		} else if _, err := a.kubeadmExecOnce(ctx, cpCID, `test -f /etc/kubernetes/admin.conf`, nil, cpProg); err != nil {
			// Built at "prepared" and never initialized: the new node is prepared like the others.
			finish("prepared — " + cp.Label + " has not run kubeadm init")
			return
		}
		if len(workers) > 0 {
			join, err := a.kubeadmExecOnce(ctx, cpCID, `kubeadm token create --print-join-command`, nil, cpProg)
			if err != nil {
				failAll("join command from %s: %v", cp.Label, err)
				return
			}
			join = strings.TrimSpace(kubeadmTail(join, 1))
			var jwg sync.WaitGroup
			jok := true
			for _, n := range workers {
				jwg.Add(1)
				go func(n designNode) {
					defer jwg.Done()
					pr := progs[n.ID]
					pr.phase("kubeadm join", 70)
					if out, err := a.kubeadmExecOnce(ctx, cids[n.ID], kubeadmJoinScript, []string{"JOIN=" + join}, pr); err != nil {
						pr.logln(kubeadmTail(out, 20))
						pr.fail("kubeadm join: %v", err)
						mu.Lock()
						jok = false
						mu.Unlock()
						return
					}
					pr.logln("joined " + cpFQDN)
				}(n)
			}
			jwg.Wait()
			if !jok {
				// Every node this deploy started has to end in a state: the ones that joined, and
				// the control plane, fail with the cluster rather than sit at "provisioning".
				for _, n := range build {
					if p := progs[n.ID]; p.p.Phase != "failed" {
						p.fail("a worker of %s failed to join; see its log", frame.Label)
					}
				}
				return
			}
		}
		if stage == "initialized" {
			finish("initialized — no pod network yet, so the nodes are NotReady")
			return
		}

		// Phase 4: the pod network (a fresh cluster), and every node Ready.
		total := len(build)
		if joining {
			total = len(kubeadmMembers(frame, doc))
		} else {
			cniVer := kubeadmCalicoVersion
			if cni == "flannel" {
				cniVer = kubeadmFlannelVersion
			}
			cpProg.phase("Installing "+cni+" "+cniVer, 80)
			if out, err := a.kubeadmExecOnce(ctx, cpCID, kubeadmCNIScript, []string{"CNI=" + cni, "VER=" + cniVer, "POD_CIDR=" + kubeadmPodCIDR}, cpProg); err != nil {
				cpProg.logln(kubeadmTail(out, 20))
				failAll("install %s: %v", cni, err)
				return
			}
			if len(workers) == 0 {
				// One node is the whole cluster: let it run workloads.
				a.kubeadmExecOnce(ctx, cpCID, `KUBECONFIG=/etc/kubernetes/admin.conf kubectl taint nodes --all node-role.kubernetes.io/control-plane- >/dev/null 2>&1 || true`, nil, cpProg)
				cpProg.logln("single-node cluster: the control-plane taint is removed so pods can run on it")
			}
		}
		for _, n := range build {
			progs[n.ID].phase("Waiting for every node to be Ready", 90)
		}
		out, err := a.kubeadmExecOnce(ctx, cpCID, kubeadmWaitReadyScript, []string{"NODES=" + strconv.Itoa(total), "TIMEOUT=" + strconv.Itoa(int(kubeadmReadyTimeout.Seconds()))}, cpProg)
		if err != nil {
			failAll("nodes not Ready within %s: %v", kubeadmReadyTimeout, err)
			return
		}
		cpProg.logln(out)
		finish(fmt.Sprintf("ready — Kubernetes %s, %s", version, cni))
	}()
}
