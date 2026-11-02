package main

import (
	"strings"
	"testing"
)

func TestLCFileTools(t *testing.T) {
	if got := lcFileTools(designNode{}); len(got) != 0 {
		t.Errorf("nothing chosen, nothing to install: %v", got)
	}
	got := lcFileTools(designNode{LCInnodump: true, LCBinlogsum: true, LCGcacheInspector: true, LCMydumper: true})
	if len(got) != 4 || got[0].ID != "innodump" || got[1].ID != "binlogsum" || got[2].ID != "gcache-inspector" || got[3].ID != "mydumper" {
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

// mydumper is installed from the package its release built for this OS release and architecture,
// checked against the asset's GitHub digest before the package manager sees it.
func TestLCFileToolsPkgScript(t *testing.T) {
	if !lcToolMydumper.Pkg || lcToolInnodump.Pkg {
		t.Fatal("mydumper is the package-installed tool; the file readers are tarballs")
	}
	if got := lcFileToolScript("debian", lcToolMydumper); got != lcFileToolsDebian+lcFileToolsPkgBody {
		t.Error("mydumper on Debian must run the Debian preamble and the package body")
	}
	if got := lcFileToolScript("oraclelinux", lcToolInnodump); got != lcFileToolsRHEL+lcFileToolsBody {
		t.Error("innodump must keep the tarball body")
	}
	body := lcFileToolsPkgBody
	for _, want := range []string{
		`. /etc/os-release`,
		`"${TOOL}_${ver}.${VERSION_CODENAME}_${ARCH}.deb"`,
		`"${TOOL}-${ver}.el${VERSION_ID%%.*}.${rarch}.rpm"`,
		`[ "$ARCH" = arm64 ] && rarch=aarch64`,
		`has no package for $PRETTY_NAME on $ARCH`,
		`sha256sum -c -`,
		`apt-get install -y -qq "./$asset"`,
		`dnf -y -q install "./$asset"`,
		`yum -y -q localinstall "./$asset"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the package install script is missing %q", want)
		}
	}
	if strings.Index(body, "sha256sum -c") > strings.Index(body, "apt-get install -y -qq \"./") {
		t.Error("the package must be verified before it is installed")
	}
}
