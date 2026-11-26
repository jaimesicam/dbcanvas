package main

import (
	"strings"
	"testing"
)

func TestHostArchName(t *testing.T) {
	for in, want := range map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64", "": "", "riscv64": ""} {
		if got := hostArchName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestArchIssues(t *testing.T) {
	t.Setenv("DOCKER_PLATFORM", "linux/arm64")
	t.Setenv("K8S_PLATFORM", "linux/arm64")
	doc := designDoc{
		Nodes: []designNode{
			{ID: "a", Label: "ps-auto", Type: "ps"},
			{ID: "b", Label: "ps-amd", Type: "ps", Arch: "amd64"},
			{ID: "c", Label: "pmm-arm", Type: "pmm2", Arch: "arm64"},
			{ID: "d", Label: "pmm-auto", Type: "pmm2"},
			{ID: "e", Label: "odd", Type: "ps", Arch: "x86"},
			{ID: "f", Label: "member", Type: "pxc", Arch: "amd64", FrameID: "px"},
		},
		Frames: []designFrame{
			{ID: "k", Label: "k3d-amd", Type: "k3d", Arch: "amd64"},
			{ID: "k2", Label: "k3d-auto", Type: "k3d"},
			{ID: "px", Label: "pxc-amd", Type: "pxc", Arch: "amd64"},
		},
	}
	var errs, infos []string
	for _, is := range archIssues(doc, "arm64") {
		if is.Level == "error" {
			errs = append(errs, is.Message)
		} else {
			infos = append(infos, is.Message)
		}
	}
	all := strings.Join(errs, "\n")
	for _, want := range []string{"k3d-amd: K3D nodes run on this Docker host's own architecture (arm64)", "pmm-arm: PMM 2 runs on amd64 only", `odd: architecture "x86"`} {
		if !strings.Contains(all, want) {
			t.Errorf("missing error %q in:\n%s", want, all)
		}
	}
	if len(errs) != 3 {
		t.Errorf("want 3 errors, got:\n%s", all)
	}
	notes := strings.Join(infos, "\n")
	if !strings.Contains(notes, "ps-amd runs as amd64 under emulation") || !strings.Contains(notes, "pxc-amd runs as amd64") {
		t.Errorf("emulation notes:\n%s", notes)
	}
	if strings.Contains(notes, "ps-auto") || strings.Contains(notes, "member") || strings.Contains(notes, "pmm-auto") {
		t.Errorf("a note for a node that is native, a member or fixed:\n%s", notes)
	}
	// Unknown host: no notes, the errors still stand.
	for _, is := range archIssues(doc, "") {
		if is.Level == "info" {
			t.Errorf("a note with no host architecture: %s", is.Message)
		}
	}
}

// Compose writes an architecture into the design only when the spec asks for one.
func TestComposeLeavesArchOnAutomatic(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, _ := composeBuild(t, composeSpec{Name: "lab", Nodes: []composeNodeSpec{{Kind: "ps", OS: "el9"}, {Kind: "ps", Name: "pinned", OS: "el9", Arch: "amd64"}}})
	for _, n := range doc.Nodes {
		switch {
		case n.Label == "pinned" && n.Arch != "amd64":
			t.Errorf("an asked-for arch was not kept: %+v", n)
		case n.Label != "pinned" && n.Arch != "":
			t.Errorf("%s was pinned to %s without being asked", n.Label, n.Arch)
		}
	}
}

func TestPackageArchIssues(t *testing.T) {
	doc := designDoc{Nodes: []designNode{
		{ID: "a", Label: "orch-ubuntu-arm", Type: "orchestrator", OS: "ubuntu", Arch: "arm64"},
		{ID: "b", Label: "orch-ubuntu-amd", Type: "orchestrator", OS: "ubuntu", Arch: "amd64"},
		{ID: "c", Label: "orch-ol-arm", Type: "orchestrator", OS: "oraclelinux", Arch: "arm64"},
		{ID: "d", Label: "ps-ubuntu-arm", Type: "ps", OS: "ubuntu", Arch: "arm64"},
	}}
	got := packageArchIssues(doc)
	if len(got) != 1 || !strings.Contains(got[0].Message, "orch-ubuntu-arm: Percona publishes Percona Orchestrator for Debian and Ubuntu on amd64 only") {
		t.Fatalf("issues %+v", got)
	}
}

// TestMemberArchIgnoredSaid: a member that names an architecture other than its cluster's is
// warned about; one that repeats its cluster's, or names none, is not.
func TestMemberArchIgnoredSaid(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "f", Label: "pxc-cluster", Type: "pxc", Arch: "arm64"}},
		Nodes: []designNode{
			{ID: "a", Label: "pxc-1", Type: "pxc", FrameID: "f", Arch: "amd64"},
			{ID: "b", Label: "pxc-2", Type: "pxc", FrameID: "f", Arch: "arm64"},
			{ID: "c", Label: "pxc-3", Type: "pxc", FrameID: "f"},
		},
	}
	var got []string
	for _, i := range archIssues(doc, "arm64") {
		got = append(got, i.Level+": "+i.Message)
	}
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "warning: pxc-1: a cluster member runs on its cluster's architecture (arm64); its arch amd64 is ignored") {
		t.Errorf("a member's other architecture was not warned about:\n%s", all)
	}
	if strings.Contains(all, "pxc-2") || strings.Contains(all, "pxc-3") {
		t.Errorf("a member following its cluster was flagged:\n%s", all)
	}
}
