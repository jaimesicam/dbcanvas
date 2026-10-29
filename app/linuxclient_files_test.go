package main

import (
	"strings"
	"testing"
)

func TestLCFileTools(t *testing.T) {
	if got := lcFileTools(designNode{}); len(got) != 0 {
		t.Errorf("nothing chosen, nothing to install: %v", got)
	}
	got := lcFileTools(designNode{LCInnodump: true, LCBinlogsum: true, LCGcacheInspector: true})
	if len(got) != 3 || got[0].ID != "innodump" || got[1].ID != "binlogsum" || got[2].ID != "gcache-inspector" {
		t.Fatalf("all chosen = %v", got)
	}
	for _, tool := range got {
		if !strings.HasPrefix(tool.Repo, "https://github.com/") || !strings.HasSuffix(tool.Repo, "/releases") {
			t.Errorf("%s: %q is not a GitHub releases page", tool.ID, tool.Repo)
		}
	}
}

// The tarball downloaded is the one checksums.txt names for this tool and architecture, it is
// verified before it is unpacked, and only then does it reach PATH.
func TestLCFileToolsScript(t *testing.T) {
	body := lcFileToolsBody
	for _, want := range []string{
		`if [ -n "$PROXY" ]`,
		`"$REPO/latest"`,
		`"$REPO/download/$tag/checksums.txt"`,
		`linux_${ARCH}\.tar\.gz`,
		`has no linux_$ARCH tarball`,
		`sha256sum -c "$TOOL.sha256"`,
		`install -m 0755 "$TOOL" "/usr/local/bin/$TOOL"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the install script is missing %q", want)
		}
	}
	if strings.Index(body, "sha256sum -c") > strings.Index(body, "tar -xzf") {
		t.Error("the tarball must be verified before it is unpacked")
	}
	for name, pre := range map[string]string{"RHEL": lcFileToolsRHEL, "Debian": lcFileToolsDebian, "CentOS 7": lcFileToolsEL7} {
		for _, tool := range []string{"curl", "tar", "gzip", "ca-certificates"} {
			if !strings.Contains(pre, tool) {
				t.Errorf("the %s preamble does not install %s", name, tool)
			}
		}
	}
}
