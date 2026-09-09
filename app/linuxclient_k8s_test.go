package main

import (
	"strings"
	"testing"
)

// The kubectl version is the reason this is a design-time option at all: kubectl is supported one
// minor version either side of the API server it talks to, and the cluster it will talk to is on
// the same canvas with its k3s tag pinned. Installing "the latest kubectl" is the one choice that
// can be wrong, and it gets more wrong the longer a stack lives.
func TestLCKubectlVersionFollowsTheCluster(t *testing.T) {
	frame := func(label, k3s string) designFrame {
		return designFrame{ID: label, Type: "k3d", Label: label, K3DK3SVersion: k3s}
	}
	cat := loadK3SCatalog()
	if cat.Latest == "" {
		t.Skip("no k3s catalog to resolve against")
	}

	// A pinned frame decides, and its label is in the reason — the log has to say why this
	// version and not another.
	doc := designDoc{Frames: []designFrame{frame("k3d-00", cat.Latest)}}
	v, why := lcKubectlVersion(doc)
	if v != lcKubectlFromK3S(cat.Latest) {
		t.Errorf("version = %q, want the cluster's k3s release %q", v, lcKubectlFromK3S(cat.Latest))
	}
	if !strings.Contains(why, "k3d-00") {
		t.Errorf("the reason must name the cluster: %q", why)
	}
	// An unpinned frame ("latest") resolves through the catalog rather than being skipped.
	if v, _ := lcKubectlVersion(designDoc{Frames: []designFrame{frame("k3d-01", "")}}); v == "" {
		t.Error("a frame that has not pinned its k3s still decides the kubectl version")
	}
	// No Kubernetes on the canvas: the catalog's newest, and a reason that says so — this is a
	// stack being built up, not an error.
	v, why = lcKubectlVersion(designDoc{})
	if v == "" {
		t.Error("a client with no cluster still gets a kubectl")
	}
	if !strings.Contains(why, "no Kubernetes cluster") {
		t.Errorf("the reason must explain the fallback: %q", why)
	}
	// A non-Kubernetes frame is not a cluster.
	if _, why := lcKubectlVersion(designDoc{Frames: []designFrame{{Type: "pxc", Label: "pxc-cluster-00"}}}); !strings.Contains(why, "no Kubernetes cluster") {
		t.Errorf("a PXC frame is not a Kubernetes cluster: %q", why)
	}
}

// k3s tags are the Kubernetes release plus a build suffix, in two spellings: the image tag uses a
// dash (v1.36.4-k3s1) and the node reports a plus (v1.36.4+k3s1). Neither is a kubectl release,
// and downloading `.../v1.36.4-k3s1/bin/linux/amd64/kubectl` is a 404 that would arrive as "the
// tools did not install".
func TestLCKubectlFromK3S(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"v1.36.4-k3s1", "v1.36.4"},
		{"v1.36.4+k3s1", "v1.36.4"},
		{"v1.30.11-k3s2", "v1.30.11"},
		{"1.36.4-k3s1", "v1.36.4"}, // the leading v is not optional in the download URL
		{"v1.36.4", "v1.36.4"},     // already a release
		{"  v1.36.4-k3s1  ", "v1.36.4"},
		{"", ""},
	} {
		if got := lcKubectlFromK3S(tc.in); got != tc.want {
			t.Errorf("lcKubectlFromK3S(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The install script is one shell body for both tools, driven entirely by environment variables —
// which is what lets a node pick one, the other, or both without three scripts to keep in step.
// The two things it must never do are download over an unverified path and leave a partial
// binary on PATH.
func TestLCK8sToolsScriptShape(t *testing.T) {
	body := lcK8sToolsBody
	for _, want := range []string{
		`if [ -n "$KUBECTL_VERSION" ]`, // kubectl is optional
		`if [ -n "$HELM" ]`,            // and so is helm
		`sha256sum -c`,                 // the download is verified
		`install -m 0755`,              // and only then does it reach PATH
		`if [ -n "$PROXY" ]`,           // the Intranet proxy is honoured
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the install script is missing %q", want)
		}
	}
	// The checksum has to be fetched from beside the binary, or it is verifying nothing.
	if !strings.Contains(body, `"$KUBECTL_URL.sha256"`) {
		t.Error("the checksum must come from the same release path as the binary")
	}
	// Both package-manager preambles install what the body needs.
	for name, pre := range map[string]string{"RHEL": lcK8sToolsRHEL, "Debian": lcK8sToolsDebian} {
		for _, tool := range []string{"curl", "tar", "ca-certificates"} {
			if !strings.Contains(pre, tool) {
				t.Errorf("the %s preamble does not install %s", name, tool)
			}
		}
	}
}

// The hint at the end of the install names the clusters a kubeconfig can come from. Nothing else
// on the node points at them, and a kubectl whose first answer is "connection refused" is a poor
// introduction to a jump box.
func TestK3DFrameLabels(t *testing.T) {
	doc := designDoc{Frames: []designFrame{
		{Type: "k3d", Label: "k3d-00"},
		{Type: "pxc", Label: "pxc-cluster-00"},
		{Type: "k3d", Label: "k3d-01"},
	}}
	got := k3dFrameLabels(doc)
	if len(got) != 2 || got[0] != "k3d-00" || got[1] != "k3d-01" {
		t.Errorf("k3dFrameLabels = %v, want the two Kubernetes frames in canvas order", got)
	}
	if len(k3dFrameLabels(designDoc{})) != 0 {
		t.Error("a stack with no Kubernetes clusters names none")
	}
}
