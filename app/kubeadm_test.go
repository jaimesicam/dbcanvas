package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// K8S_PLATFORM sets the platform of every Kubernetes node; K3D_PLATFORM, its earlier name, is still
// read when it is unset — an .env written before the rename must not quietly go back to amd64.
func TestK8sPlatformFallsBackToK3DPlatform(t *testing.T) {
	cases := []struct{ k8s, k3d, want, arch string }{
		{"", "", "linux/amd64", "amd64"},
		{"", "linux/arm64", "linux/arm64", "arm64"},
		{"linux/arm64", "", "linux/arm64", "arm64"},
		{"linux/amd64", "linux/arm64", "linux/amd64", "amd64"}, // the new name wins
		{" linux/arm64 ", "", "linux/arm64", "arm64"},
	}
	for _, c := range cases {
		t.Setenv("K8S_PLATFORM", c.k8s)
		t.Setenv("K3D_PLATFORM", c.k3d)
		if got := k8sPlatform(); got != c.want {
			t.Errorf("K8S_PLATFORM=%q K3D_PLATFORM=%q: k8sPlatform() = %q, want %q", c.k8s, c.k3d, got, c.want)
		}
		if got := k8sArch(); got != c.arch {
			t.Errorf("K8S_PLATFORM=%q K3D_PLATFORM=%q: k8sArch() = %q, want %q", c.k8s, c.k3d, got, c.arch)
		}
	}
}

// The shell builds the image for the same platform the app asks for, by the same rule.
func TestResolveK8sPlatformAgreesWithTheApp(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	for _, c := range []struct{ env, dotenv, want string }{
		{"", "", "linux/amd64"},
		{"", "K3D_PLATFORM=linux/arm64\n", "linux/arm64"},
		{"", "K3D_PLATFORM=linux/arm64\nK8S_PLATFORM=linux/amd64\n", "linux/amd64"},
		{"K8S_PLATFORM=linux/arm64", "K8S_PLATFORM=linux/amd64\n", "linux/arm64"},
	} {
		if err := os.WriteFile(dir+"/.env", []byte(c.dotenv), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", `. ../images/platform.sh; resolve_k8s_platform "$ROOT"`)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ROOT=" + dir}
		if c.env != "" {
			cmd.Env = append(cmd.Env, c.env)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("env %q, .env %q: resolve_k8s_platform = %q, want %q", c.env, c.dotenv, got, c.want)
		}
	}
}

// The tag the node asks for is the one images/service.sh builds, for K8S_PLATFORM — not
// DOCKER_PLATFORM, which is how an amd64 installation on Apple Silicon gets arm64 nodes.
func TestKubeadmImageFollowsK8sPlatform(t *testing.T) {
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
	t.Setenv("K8S_PLATFORM", "linux/arm64")
	if got, want := kubeadmImage(), "dbcanvas-kubeadm:ubuntu-24.04-arm64-"+imageRelease(); got != want {
		t.Errorf("kubeadmImage() = %q, want %q", got, want)
	}
	e, ok := extraImageByID("kubeadm")
	if !ok || e.Platform != "linux/arm64" || e.Tag != kubeadmImage() {
		t.Errorf("the in-app build does not build the node's image for K8S_PLATFORM: %+v", e)
	}
	sh, err := os.ReadFile("../images/service.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sh), `KUBEADM_TAG="dbcanvas-kubeadm:ubuntu-24.04-${K8S_PLATFORM_RESOLVED#linux/}-$(image_release "$ROOT")"`) {
		t.Error("images/service.sh does not tag the kubeadm image the way kubeadmImage() names it")
	}
}

// What the node needs from its image, each found by running kubeadm in a container without it.
func TestKubeadmDockerfile(t *testing.T) {
	b, err := os.ReadFile("../images/kubeadm.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	df := string(b)
	if regexp.MustCompile(`(?m)^VOLUME .*/sys/fs/cgroup`).MatchString(df) {
		t.Error("VOLUME /sys/fs/cgroup hides the private cgroup namespace's mount; kubeadm then reads cgroup v1")
	}
	for _, want := range []string{
		`VOLUME ["/var/lib/containerd"]`,   // not overlayfs on overlayfs
		`mount --make-rshared /`,           // Calico's bpffs mount
		`/sys/fs/cgroup/init/cgroup.procs`, // execs out of the root, so controllers delegate
		`/sys/fs/cgroup/cgroup.subtree_control`,
		`/boot/config-$(uname -r)`, // kubeadm's SystemVerification
		`SystemdCgroup = true`,     // the kubelet's cgroup driver
		`ENTRYPOINT ["/usr/local/sbin/dbcanvas-node-init"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("images/kubeadm.Dockerfile lacks %s", want)
		}
	}
}

func TestKubeadmInitConfig(t *testing.T) {
	conf := kubeadmInitConfig("v1.37.2", "kube01", "kube01.example.net", "172.20.0.5")
	docs := strings.Split(conf, "\n---\n")
	if len(docs) != 4 {
		t.Fatalf("want InitConfiguration, ClusterConfiguration, KubeletConfiguration and KubeProxyConfiguration; got %d documents", len(docs))
	}
	kinds := map[string]map[string]any{}
	for _, d := range docs {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatalf("not YAML: %v\n%s", err, d)
		}
		kinds[m["kind"].(string)] = m
	}
	cl := kinds["ClusterConfiguration"]
	if cl["kubernetesVersion"] != "v1.37.2" || cl["controlPlaneEndpoint"] != "kube01.example.net:6443" {
		t.Errorf("ClusterConfiguration: %v", cl)
	}
	net := cl["networking"].(map[string]any)
	if net["podSubnet"] != kubeadmPodCIDR || net["serviceSubnet"] != kubeadmServiceCIDR {
		t.Errorf("networking: %v", net)
	}
	init := kinds["InitConfiguration"]
	if init["localAPIEndpoint"].(map[string]any)["advertiseAddress"] != "172.20.0.5" ||
		init["nodeRegistration"].(map[string]any)["name"] != "kube01" {
		t.Errorf("InitConfiguration: %v", init)
	}
	// The three settings a container node needs.
	kl := kinds["KubeletConfiguration"]
	if kl["failSwapOn"] != false || kl["resolvConf"] != kubeadmResolvConf || kl["cgroupDriver"] != "systemd" {
		t.Errorf("KubeletConfiguration: %v", kl)
	}
	kp := kinds["KubeProxyConfiguration"]
	if kp["conntrack"].(map[string]any)["maxPerCore"] != float64(0) {
		t.Errorf("KubeProxyConfiguration: %v", kp)
	}
}

func TestKubeadmMinor(t *testing.T) {
	repo, pkg, minor, ok := kubeadmMinor("v1.37.2")
	if !ok || repo != "v1.37" || pkg != "1.37.2" || minor != 37 {
		t.Errorf("kubeadmMinor(v1.37.2) = %q %q %d %v", repo, pkg, minor, ok)
	}
	for _, bad := range []string{"1.37.2", "v1.37", "v1.37.2-k3s1", ""} {
		if _, _, _, ok := kubeadmMinor(bad); ok {
			t.Errorf("kubeadmMinor(%q) accepted", bad)
		}
	}
}

func TestKubeadmCIDROverlap(t *testing.T) {
	for subnet, want := range map[string]string{
		"172.20.0.0/16":  "",
		"192.168.0.0/20": "",
		"10.244.3.0/24":  kubeadmPodCIDR,
		"10.0.0.0/8":     kubeadmPodCIDR,
		"10.100.0.0/16":  kubeadmServiceCIDR,
		"not a subnet":   "",
	} {
		if got := kubeadmCIDROverlap(subnet); got != want {
			t.Errorf("kubeadmCIDROverlap(%q) = %q, want %q", subnet, got, want)
		}
	}
}

// The catalog the frame's picker offers: vX.Y.Z only, newest first, latest being the first.
func TestKubernetesCatalog(t *testing.T) {
	t.Setenv("VERSIONS_FILE", "../versions.yaml")
	cat := loadKubernetesCatalog()
	if len(cat.Versions) == 0 || cat.Latest != cat.Versions[0] {
		t.Fatalf("catalog: latest %q, %d versions", cat.Latest, len(cat.Versions))
	}
	for _, v := range cat.Versions {
		if _, _, minor, ok := kubeadmMinor(v); !ok || minor < kubeadmMinMinor {
			t.Errorf("catalog version %q is not one the frame can install", v)
		}
	}
	if v, ok := cat.resolveK3SVersion(""); !ok || v != cat.Latest {
		t.Errorf("\"\" resolves to %q, want %q", v, cat.Latest)
	}
	// No versions.yaml at all: the fallback still deploys.
	t.Setenv("VERSIONS_FILE", t.TempDir()+"/none.yaml")
	if fb := loadKubernetesCatalog(); fb.Latest == "" || len(fb.Versions) == 0 {
		t.Errorf("fallback catalog is empty: %+v", fb)
	}
}

func TestKubeadmShapeIssues(t *testing.T) {
	t.Setenv("VERSIONS_FILE", "../versions.yaml")
	frame := func(stage, cni, ver string) designFrame {
		return designFrame{ID: "f", Type: "kubeadm", Label: "kube", KubeadmStage: stage, KubeadmCNI: cni, KubeadmVersion: ver}
	}
	members := func(n int) []designNode {
		var out []designNode
		for i := 0; i < n; i++ {
			out = append(out, designNode{ID: string(rune('a' + i)), Type: "kubeadm", FrameID: "f", Label: "kube0" + string(rune('1'+i))})
		}
		return out
	}
	msgs := func(is []issue) string {
		var s []string
		for _, i := range is {
			s = append(s, i.Level+": "+i.Message)
		}
		return strings.Join(s, "\n")
	}
	if is := kubeadmShapeIssues(frame("", "", ""), designDoc{Nodes: members(2)}); len(is) != 0 {
		t.Errorf("a default frame has issues:\n%s", msgs(is))
	}
	for _, c := range []struct {
		f    designFrame
		n    int
		want string
	}{
		{frame("", "", ""), 5, "it takes 1 to 4"},
		{frame("", "", ""), 0, "it takes 1 to 4"},
		{frame("half", "", ""), 2, "stage \"half\""},
		{frame("", "cilium", ""), 2, "pod network \"cilium\""},
		{frame("", "", "v9.99.0"), 2, "not in the catalog"},
	} {
		if got := msgs(kubeadmShapeIssues(c.f, designDoc{Nodes: members(c.n)})); !strings.Contains(got, c.want) {
			t.Errorf("%+v with %d nodes: want %q in\n%s", c.f, c.n, c.want, got)
		}
	}
	// The control plane is the first member by label, and kubeadm refuses it one CPU.
	nodes := members(2)
	nodes[0].CPUs = 1
	if got := msgs(kubeadmShapeIssues(frame("", "", ""), designDoc{Nodes: nodes})); !strings.Contains(got, "kube01 is the control plane and has 1 CPU") {
		t.Errorf("one CPU on the control plane: %s", got)
	}
	if m := kubeadmMembers(frame("", "", ""), designDoc{Nodes: []designNode{nodes[1], nodes[0]}}); m[0].Label != "kube01" {
		t.Errorf("the control plane is %s, want the first by label", m[0].Label)
	}
}

func TestKubeadmEmulationIssue(t *testing.T) {
	t.Setenv("K8S_PLATFORM", "linux/amd64")
	if kubeadmEmulationIssue("x86_64") != "" {
		t.Error("amd64 nodes on an x86_64 host are native")
	}
	if msg := kubeadmEmulationIssue("aarch64"); !strings.Contains(msg, "K8S_PLATFORM=linux/arm64") {
		t.Errorf("amd64 nodes on an aarch64 host: %q", msg)
	}
	t.Setenv("K8S_PLATFORM", "linux/arm64")
	if kubeadmEmulationIssue("arm64") != "" || kubeadmEmulationIssue("aarch64") != "" {
		t.Error("arm64 nodes on an arm64 host are native")
	}
}

// A join command or a public key is the last line of what a step printed — not its last character.
func TestKubeadmTail(t *testing.T) {
	out := "W1102 a warning\nkubeadm join kube01.example.net:6443 --token abc.def --discovery-token-ca-cert-hash sha256:0a7 \n"
	if got := kubeadmTail(out, 1); got != "kubeadm join kube01.example.net:6443 --token abc.def --discovery-token-ca-cert-hash sha256:0a7" {
		t.Errorf("kubeadmTail(out, 1) = %q", got)
	}
	if got := kubeadmTail("one\ntwo\nthree", 2); got != "two\nthree" {
		t.Errorf("kubeadmTail(…, 2) = %q", got)
	}
	if got := kubeadmTail("only", 5); got != "only" {
		t.Errorf("kubeadmTail(only, 5) = %q", got)
	}
}

// A Linux Client reaches a Kubeadm cluster at its controlPlaneEndpoint, the FQDN its API
// certificate names; a K3D cluster still at its serverlb.
func TestLinuxClientServerForEachFrameKind(t *testing.T) {
	cfg, _ := json.Marshal(kubeadmConfig{Role: "control-plane", ControlPlane: "kube01.example.net"})
	if got := lcKubeServer(Stack{ID: 3}, designFrame{Type: "kubeadm", Label: "kubeadm-01"}, Deployment{Config: cfg}); got != "https://kube01.example.net:6443" {
		t.Errorf("kubeadm server = %q", got)
	}
	if got := lcKubeServer(Stack{ID: 3}, designFrame{Type: "kubeadm"}, Deployment{}); got != "" {
		t.Errorf("a kubeadm frame with no running control plane has no server, got %q", got)
	}
	if got := lcKubeServer(Stack{ID: 3}, designFrame{Type: "k3d", Label: "k3d-00"}, Deployment{}); !strings.Contains(got, "serverlb:6443") {
		t.Errorf("k3d server = %q", got)
	}
}

// The kubectl a Linux Client installs matches a Kubeadm frame's release, as it does a K3D frame's.
func TestLinuxClientKubectlMatchesKubeadm(t *testing.T) {
	t.Setenv("VERSIONS_FILE", "../versions.yaml")
	v, why := lcKubectlVersion(designDoc{Frames: []designFrame{{Type: "kubeadm", Label: "kube", KubeadmVersion: "v1.36.6"}}})
	if v != "v1.36.6" || !strings.Contains(why, "kube") {
		t.Errorf("lcKubectlVersion = %q (%s)", v, why)
	}
	if labels := k3dFrameLabels(designDoc{Frames: []designFrame{{Type: "kubeadm", Label: "kube"}, {Type: "pxc", Label: "p"}}}); len(labels) != 1 || labels[0] != "kube" {
		t.Errorf("k3dFrameLabels = %v", labels)
	}
}

// kubeadm names every cluster's entries kubernetes / kubernetes-admin; two of them merged into one
// client must still come out as two contexts, each with its own server.
func TestTwoKubeadmKubeconfigsMergeApart(t *testing.T) {
	admin := func(server string) string {
		return `apiVersion: v1
kind: Config
clusters:
- cluster: {certificate-authority-data: Q0E=, server: '` + server + `'}
  name: kubernetes
contexts:
- context: {cluster: kubernetes, user: kubernetes-admin}
  name: kubernetes-admin@kubernetes
current-context: kubernetes-admin@kubernetes
users:
- name: kubernetes-admin
  user: {client-certificate-data: Q0VSVA==, client-key-data: S0VZ}
`
	}
	var ours []kubeconfig
	for _, c := range []struct{ name, server string }{{"kubeadm-01", "https://kube01.example.net:6443"}, {"kubeadm-02", "https://kube11.example.net:6443"}} {
		kc, err := parseKubeconfig(admin(c.server))
		if err == nil {
			kc, err = renameKubeconfig(kc, c.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		ours = append(ours, kc)
	}
	m := mergeKubeconfigs(kubeconfig{}, ours, "kubeadm-02")
	if len(m.Contexts) != 2 || len(m.Clusters) != 2 || len(m.Users) != 2 || m.CurrentContext != "kubeadm-02" {
		t.Fatalf("merged: %+v", m)
	}
	if m.Clusters[1].Cluster["server"] != "https://kube11.example.net:6443" {
		t.Errorf("second cluster's server = %v", m.Clusters[1].Cluster["server"])
	}
}

// One KUBECONFIG serves both node kinds: kubectl skips the file that does not exist on each.
func TestK8sKubeconfigNamesBothAdminConfigs(t *testing.T) {
	if k8sKubeconfig != "/etc/rancher/k3s/k3s.yaml:/etc/kubernetes/admin.conf" {
		t.Errorf("k8sKubeconfig = %q", k8sKubeconfig)
	}
}
