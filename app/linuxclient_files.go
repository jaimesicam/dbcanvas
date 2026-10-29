package main

import (
	"context"
)

// linuxclient_files.go — innodump and gcache-inspector on a Linux Client node.
//
// Both read a MySQL server's files offline, with no server running: innodump writes out a table's
// definition and rows from its .ibd / .MYD / MyRocks files (or a whole datadir), gcache-inspector
// summarizes and decodes the write-sets in a Galera node's galera.cache. The Linux Client is where
// a copy of those files gets looked at, so they are a design-time choice like the other tools.
//
//	innodump            https://github.com/PrzemekMalkowski/innodump
//	gcache-inspector    https://github.com/PrzemekMalkowski/gcache-inspector
//
// Both publish statically linked Go binaries for linux amd64 and arm64 on their GitHub release
// pages, as tarballs beside a checksums.txt, so every Linux Client release — CentOS 7 included —
// runs them. The asset names differ: innodump carries the version (innodump_0.8.2_linux_amd64.tar.gz),
// gcache-inspector does not (gcache-inspector_linux_amd64.tar.gz). Rather than hard-code either,
// the script resolves the latest tag, then takes the one checksums.txt line for this tool and
// architecture — the asset to download is the one the checksum is for, by construction.
//
// Like the other tools, a failed install never fails the deploy, and what is recorded is read back
// off the binary afterwards.

// lcFileTool is one installable offline reader.
type lcFileTool struct {
	ID    string // the binary, and the id the panel keys on
	Label string
	Repo  string // GitHub releases page
}

var (
	lcToolInnodump        = lcFileTool{"innodump", "innodump", "https://github.com/PrzemekMalkowski/innodump/releases"}
	lcToolGcacheInspector = lcFileTool{"gcache-inspector", "gcache-inspector", "https://github.com/PrzemekMalkowski/gcache-inspector/releases"}
)

// lcFileTools is what a node's choices install, in the order they run.
func lcFileTools(n designNode) []lcFileTool {
	var out []lcFileTool
	if n.LCInnodump {
		out = append(out, lcToolInnodump)
	}
	if n.LCGcacheInspector {
		out = append(out, lcToolGcacheInspector)
	}
	return out
}

// lcFileToolsRHEL / lcFileToolsEL7 / lcFileToolsDebian install the download prerequisites; the
// body is the same on every release.
const lcFileToolsRHEL = `set -e
dnf -y -q install curl tar gzip ca-certificates >/dev/null
`

const lcFileToolsEL7 = `set -e
yum -y -q install curl tar gzip ca-certificates >/dev/null
`

const lcFileToolsDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq curl tar gzip ca-certificates >/dev/null
`

// lcFileToolsBody installs $TOOL from $REPO. The checksum is verified and a mismatch fails the
// step, as for cmctl and cfssl.
const lcFileToolsBody = `
if [ -n "$PROXY" ]; then
  export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"
  export no_proxy="localhost,127.0.0.1,.$DOMAIN" NO_PROXY="$no_proxy"
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
tag=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$REPO/latest")
tag=${tag##*/}
case "$tag" in v[0-9]*) ;; *) echo "$TOOL: could not resolve the latest release ($tag)" >&2; exit 1 ;; esac
curl -fsSL -o checksums.txt "$REPO/download/$tag/checksums.txt"
# One line, for this tool and architecture, with or without the version in the asset name.
grep -E "  ${TOOL}_([0-9][^_]*_)?linux_${ARCH}\.tar\.gz\$" checksums.txt | head -1 > "$TOOL.sha256"
asset=$(awk '{print $2}' "$TOOL.sha256")
[ -n "$asset" ] || { echo "$TOOL: checksums.txt for $tag has no linux_$ARCH tarball" >&2; exit 1; }
curl -fsSL -o "$asset" "$REPO/download/$tag/$asset"
sha256sum -c "$TOOL.sha256" >/dev/null
tar -xzf "$asset" "$TOOL"
install -m 0755 "$TOOL" "/usr/local/bin/$TOOL"
echo "$TOOL $tag installed"
`

// linuxClientInstallFileTools installs what the node asked for and records what landed. It never
// fails the deploy — see the file comment.
func (a *App) linuxClientInstallFileTools(ctx context.Context, id string, n designNode, cfg *linuxClientConfig, pr *pxcProg) {
	tools := lcFileTools(n)
	if len(tools) == 0 {
		return
	}
	var names []string
	for _, t := range tools {
		names = append(names, t.ID)
	}
	pr.phase("Installing "+lcJoinAnd(names), 87)

	script := lcFileToolsRHEL
	switch {
	case isDebianOS(n.OS):
		script = lcFileToolsDebian
	case isEL7OS(n.OS):
		script = lcFileToolsEL7
	}
	domain := envOr("DOMAIN", "example.net")
	proxy := ""
	if n.UseProxy {
		proxy = "http://intranet." + domain + ":3128"
	}
	for _, t := range tools {
		pr.logln("installing " + t.ID + " (latest) from " + t.Repo)
		env := []string{
			"TOOL=" + t.ID, "REPO=" + t.Repo, "ARCH=" + archOr(n.Arch),
			"PROXY=" + proxy, "DOMAIN=" + domain,
		}
		if err := a.runStep(ctx, id, script+lcFileToolsBody, env, pr.logln); err != nil {
			pr.logln(t.ID + " did not install: " + lastLines(err.Error(), 200) +
				" — the node is up regardless; install it from its terminal")
		}
	}

	// Read back what is on the node: both answer --version with "<name> 0.8.2".
	cfg.FileTools = nil
	for _, t := range tools {
		bin := "/usr/local/bin/" + t.ID
		v := a.lcToolVersion(ctx, id, "test -x "+bin+" && "+bin+" --version 2>&1 | awk '{print $NF; exit}'")
		if v == "" {
			pr.logln(t.ID + " did not install")
			continue
		}
		cfg.FileTools = append(cfg.FileTools, lcDBClient{Tool: t.ID, Label: t.Label, Version: v})
		pr.logln(t.ID + " " + v + " on PATH")
	}
}
