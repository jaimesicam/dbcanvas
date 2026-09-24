package main

import (
	_ "embed"
	"strings"
)

// linuxclient_el7.go — CentOS 7 as a Linux Client release, offered only with EOL=on (eol.go).
//
// Every other Linux Client release is a dbcanvas-systemd:* image that `make images` built. This one
// is not built at all: it is the stock centos:7 image, pulled like PMM or Keycloak, and made usable
// when the node is deployed. Two things stand between that image and a usable node.
//
// **Its repositories are gone.** CentOS 7 reached end of life on 2024-06-30 and mirror.centos.org
// no longer serves it, so the image's own CentOS-Base.repo — mirrorlist.centos.org — fails every
// `yum` with "Could not resolve host". The release lives on, frozen, at vault.centos.org, and
// el7CentOSBaseRepo points base/updates/extras there. The SCL "rh" repository gets the same
// treatment, because it is where the only Python 3 new enough for the current drivers comes from
// (see samplecode_env.go).
//
// **Its systemd cannot run.** CentOS 7 ships systemd 219, which predates cgroup v2, and every
// current Docker host — Docker Desktop, a recent Ubuntu or Fedora, Colima — runs cgroup v2. Started
// as PID 1 there it neither boots nor fails: it sits at "Failed to get D-Bus connection" forever,
// which is the worst of the three outcomes. So this node does not run systemd at all. Its PID 1 is
// a shell that reaps children and exits on SIGTERM, and everything DBCanvas does on a Linux Client
// — the terminal, Sample Client Code, the file manager — is an exec, which needs no init system. A
// service needs one, and there are none to run on a jump box.

// el7CentOSBaseRepo is CentOS 7's base repository file, pointed at vault.centos.org. It is written
// over /etc/yum.repos.d/CentOS-Base.repo before the first yum runs.
//
//go:embed el7/CentOS-Base.repo
var el7CentOSBaseRepo string

// el7OS is the OS id, and the only CentOS release DBCanvas offers is 7 — so "centos" means EL7
// wherever the code only has the family to go on (a deployment config records the OS, not always
// its version).
const (
	el7OS        = "centos"
	el7OSVersion = "7"
	// el7ImageRepo/el7ImageTag is the image, pulled rather than built. It is multi-arch (amd64 and
	// arm64), so it is pulled for the platform this installation targets.
	el7ImageRepo = "centos"
	el7ImageTag  = "7"
)

// isEL7OS reports whether an OS id is CentOS 7. See el7OS.
func isEL7OS(os string) bool { return os == el7OS }

// el7Image is the image reference a CentOS 7 Linux Client runs.
func el7Image() string { return el7ImageRepo + ":" + el7ImageTag }

// el7Cmd is PID 1 for a CentOS 7 node, in place of systemd. bash rather than `sleep infinity`
// because PID 1 has to reap: every exec DBCanvas runs leaves children behind, and a sleep would let
// them pile up as zombies. The trap makes a stop immediate rather than a ten-second SIGKILL.
var el7Cmd = []string{"/bin/bash", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"}

// el7EOLImage is the entry the Linux Client's OS picker gets for CentOS 7 when EOL is on. It rides
// beside the images.yaml catalogue rather than inside it (handleImagesCatalog): that catalogue is
// also what HAProxy and All in One pick an OS from, and neither installs anything on CentOS 7.
func el7EOLImage() PXCImage {
	return PXCImage{OS: el7OS, OSVersion: el7OSVersion, Arch: platformArch(), Platform: pullPlatform()}
}

// el7BootstrapScript makes a fresh centos:7 container into a node yum works on: the vault
// repositories, IPv4-only resolution and the Intranet proxy (both in yum.conf — there is no
// /etc/dnf here, which is where ensureDNFIPv4 and pkgProxyRHEL write), the SCL "rh" repository, and
// percona-release, which every Percona package on this node is installed through.
//
// The repo file is written for x86_64. On aarch64 CentOS 7 lives under /altarch/ on the vault and
// signs its packages with a second key, so the same file is rewritten for it rather than kept as a
// second copy that could drift.
//
// Env: $REPO_FILE (the CentOS-Base.repo content), $PROXY (empty = direct).
const el7BootstrapScript = `set -e
arch=$(uname -m)
if [ "$arch" = aarch64 ]; then
  printf '%s\n' "$REPO_FILE" | sed -e 's#vault.centos.org/7.9.2009/#vault.centos.org/altarch/7.9.2009/#' \
    -e 's#/x86_64/#/aarch64/#' \
    -e 's#RPM-GPG-KEY-CentOS-7$#RPM-GPG-KEY-CentOS-7 file:///etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-7-aarch64#' \
    > /etc/yum.repos.d/CentOS-Base.repo
else
  printf '%s\n' "$REPO_FILE" > /etc/yum.repos.d/CentOS-Base.repo
fi
grep -q '^ip_resolve=' /etc/yum.conf || echo 'ip_resolve=4' >> /etc/yum.conf
if [ -n "$PROXY" ]; then
  grep -q '^proxy=' /etc/yum.conf || echo "proxy=$PROXY" >> /etc/yum.conf
fi
# fastestmirror probes mirrors that no longer exist; with every repo on one baseurl it only
# adds a timeout to each yum.
sed -i 's/^enabled=1/enabled=0/' /etc/yum/pluginconf.d/fastestmirror.conf 2>/dev/null || true
echo "CentOS repositories pointed at vault.centos.org ($arch)"
yum -y -q install which centos-release-scl-rh >/dev/null
# centos-release-scl-rh installs the SCLo key and a repo file aimed at mirrorlist.centos.org,
# which is as dead as the base one was. Same repository, on the vault.
if [ "$arch" = aarch64 ]; then scl_base="https://vault.centos.org/altarch/7.9.2009/sclo/aarch64/rh/"; else scl_base="https://vault.centos.org/7.9.2009/sclo/x86_64/rh/"; fi
cat > /etc/yum.repos.d/CentOS-SCLo-scl-rh.repo <<EOF
[centos-sclo-rh]
name=CentOS-7 - SCLo rh
baseurl=$scl_base
gpgcheck=1
enabled=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-SIG-SCLo
EOF
echo "SCL rh repository pointed at the vault"
rpm -q percona-release >/dev/null 2>&1 || yum -y -q install https://repo.percona.com/yum/percona-release-latest.noarch.rpm >/dev/null
echo "$(rpm -q percona-release) installed"`

// el7BootstrapEnv is the environment el7BootstrapScript runs with.
func el7BootstrapEnv(proxy string) []string {
	return []string{"REPO_FILE=" + strings.TrimRight(el7CentOSBaseRepo, "\n"), "PROXY=" + proxy}
}
