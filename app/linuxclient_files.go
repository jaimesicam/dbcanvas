package main

import (
	"context"
)

// linuxclient_files.go — innodump, binlogsum, gcache-inspector and mydumper on a Linux Client
// node. The MySQL-family servers install the same tools with the same scripts; see dbfiletools.go.
//
// All three read a MySQL server's files offline, with no server running: innodump writes out a
// table's definition and rows from its .ibd / .MYD / MyRocks files (or a whole datadir), binlogsum
// summarizes a binary log from mysqlbinlog's decoded output, and gcache-inspector summarizes and
// decodes the write-sets in a Galera node's galera.cache. The Linux Client is where a copy of
// those files gets looked at, so they are a design-time choice like the other tools.
//
//	innodump            https://github.com/PrzemekMalkowski/innodump
//	binlogsum           https://github.com/PrzemekMalkowski/binlogsum
//	gcache-inspector    https://github.com/PrzemekMalkowski/gcache-inspector
//
// Each publishes statically linked Go binaries for linux amd64 and arm64 on its GitHub release
// page, as tarballs beside a checksums.txt, so every Linux Client release — CentOS 7 included —
// runs them. The asset names differ: innodump and binlogsum carry the version
// (innodump_0.8.2_linux_amd64.tar.gz), gcache-inspector does not
// (gcache-inspector_linux_amd64.tar.gz). Rather than hard-code either,
// the script resolves the latest tag, then takes the one checksums.txt line for this tool and
// architecture — the asset to download is the one the checksum is for, by construction.
//
// mydumper (with myloader) is the odd one out: it links the MySQL client library, so its releases
// are .rpm and .deb packages built per OS release (el7..el10, Ubuntu and Debian codenames) with no
// checksums.txt beside them. Its script picks the package for the node's /etc/os-release and
// architecture and checks it against the SHA-256 digest GitHub publishes for each release asset,
// then lets the package manager install it, into /usr/bin. Not every release has an arm64 build
// (Ubuntu 22.04 and CentOS 7 have none); the step then fails with a message that says so.
//
//	mydumper            https://github.com/mydumper/mydumper
//
// Like the other tools, a failed install never fails the deploy, and what is recorded is read back
// off the binary afterwards.

// lcFileTool is one installable offline reader.
type lcFileTool struct {
	ID    string // the binary, and the id the panel keys on
	Label string
	Repo  string // GitHub releases page
	Pkg   bool   // released as per-OS .rpm/.deb packages rather than a static tarball
}

var (
	lcToolInnodump        = lcFileTool{"innodump", "innodump", "https://github.com/PrzemekMalkowski/innodump/releases", false}
	lcToolBinlogsum       = lcFileTool{"binlogsum", "binlogsum", "https://github.com/PrzemekMalkowski/binlogsum/releases", false}
	lcToolGcacheInspector = lcFileTool{"gcache-inspector", "gcache-inspector", "https://github.com/PrzemekMalkowski/gcache-inspector/releases", false}
	lcToolMydumper        = lcFileTool{"mydumper", "mydumper", "https://github.com/mydumper/mydumper/releases", true}
)

// lcFileTools is what a node's choices install, in the order they run.
func lcFileTools(n designNode) []lcFileTool {
	var out []lcFileTool
	if n.LCInnodump {
		out = append(out, lcToolInnodump)
	}
	if n.LCBinlogsum {
		out = append(out, lcToolBinlogsum)
	}
	if n.LCGcacheInspector {
		out = append(out, lcToolGcacheInspector)
	}
	if n.LCMydumper {
		out = append(out, lcToolMydumper)
	}
	return out
}

// lcFileToolsRHEL / lcFileToolsEL7 / lcFileToolsDebian install the download prerequisites; the
// body is the same on every release. lcFileToolsPrelude picks one for an OS. Each runs the
// package manager only when one of the download tools is missing: a database server that installs
// from a Repository node, or has no package egress, usually has all three already.
const lcFileToolsHave = `command -v curl >/dev/null && command -v tar >/dev/null && command -v gzip >/dev/null`

const lcFileToolsRHEL = `set -e
` + lcFileToolsHave + ` || dnf -y -q install curl tar gzip ca-certificates >/dev/null
`

const lcFileToolsEL7 = `set -e
` + lcFileToolsHave + ` || yum -y -q install curl tar gzip ca-certificates >/dev/null
`

const lcFileToolsDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
` + lcFileToolsHave + ` || { apt-get update -qq >/dev/null; apt-get install -y -qq curl tar gzip ca-certificates >/dev/null; }
`

func lcFileToolsPrelude(os string) string {
	switch {
	case isDebianOS(os):
		return lcFileToolsDebian
	case isEL7OS(os):
		return lcFileToolsEL7
	}
	return lcFileToolsRHEL
}

// lcFileToolEnv is the install script's environment for one tool.
func lcFileToolEnv(t lcFileTool, arch string, useProxy bool) []string {
	domain := envOr("DOMAIN", "example.net")
	proxy := ""
	if useProxy {
		proxy = "http://intranet." + domain + ":3128"
	}
	return []string{"TOOL=" + t.ID, "REPO=" + t.Repo, "ARCH=" + archOr(arch), "PROXY=" + proxy, "DOMAIN=" + domain}
}

// lcFileToolScript is the whole install script for one tool on an OS.
func lcFileToolScript(os string, t lcFileTool) string {
	if t.Pkg {
		return lcFileToolsPrelude(os) + lcFileToolsPkgBody
	}
	return lcFileToolsPrelude(os) + lcFileToolsBody
}

// lcFileToolVersion is what the tool on the node says its version is, or "" when it is not
// there. The file readers answer --version with "<name> 0.8.2"; mydumper with
// "mydumper v1.0.11-1, built against MySQL 8.4.11 with SSL support".
func (a *App) lcFileToolVersion(ctx context.Context, id string, t lcFileTool) string {
	if t.Pkg {
		return a.lcToolVersion(ctx, id, "command -v "+t.ID+" >/dev/null && "+t.ID+
			" --version 2>&1 | awk '{v=$2; sub(/^v/, \"\", v); sub(/,$/, \"\", v); print v; exit}'")
	}
	bin := "/usr/local/bin/" + t.ID
	return a.lcToolVersion(ctx, id, "test -x "+bin+" && "+bin+" --version 2>&1 | awk '{print $NF; exit}'")
}

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

// lcFileToolsPkgBody installs $TOOL from the package its latest release in $REPO built for this
// node's OS release and architecture. The package is checked against the asset's digest from the
// GitHub API, and a mismatch fails the step. The API allows 60 unauthenticated calls an hour per
// address; when it does not answer, the package is installed over HTTPS without the digest check
// and the log says so.
const lcFileToolsPkgBody = `
if [ -n "$PROXY" ]; then
  export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"
  export no_proxy="localhost,127.0.0.1,.$DOMAIN" NO_PROXY="$no_proxy"
fi
. /etc/os-release
work=$(mktemp -d)
chmod 0755 "$work"
trap 'rm -rf "$work"' EXIT
cd "$work"
tag=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$REPO/latest")
tag=${tag##*/}
case "$tag" in v[0-9]*) ;; *) echo "$TOOL: could not resolve the latest release ($tag)" >&2; exit 1 ;; esac
ver=${tag#v}
case " $ID $ID_LIKE " in
  *" debian "*|*" ubuntu "*) asset="${TOOL}_${ver}.${VERSION_CODENAME}_${ARCH}.deb" ;;
  *) rarch=x86_64; [ "$ARCH" = arm64 ] && rarch=aarch64
     asset="${TOOL}-${ver}.el${VERSION_ID%%.*}.${rarch}.rpm" ;;
esac
api="https://api.github.com/repos/${REPO#https://github.com/}"
if curl -fsSL -o release.json "$api/tags/$tag"; then
  # One asset object at a time: its "name", then its "digest", before the next asset's "name".
  sum=$(tr -d ' \n' < release.json | tr ',{}' '\n\n\n' |
    awk -v a="\"name\":\"$asset\"" '$0 == a {f = 1; next} f && /^"name":/ {exit} f && /^"digest":"sha256:/ {print; exit}')
  grep -q "\"name\": *\"$asset\"" release.json ||
    { echo "$TOOL: $tag has no package for $PRETTY_NAME on $ARCH ($asset)" >&2; exit 1; }
  sum=${sum#*sha256:}; sum=${sum%\"}
else
  sum=""
  echo "$TOOL: the GitHub API did not answer; installing $asset without its digest check"
fi
curl -fsSL -o "$asset" "$REPO/download/$tag/$asset"
if [ -n "$sum" ]; then echo "$sum  $asset" | sha256sum -c - >/dev/null; fi
case "$asset" in
  *.deb) apt-get update -qq >/dev/null || true
         apt-get install -y -qq "./$asset" >/dev/null ;;
  *) if command -v dnf >/dev/null; then dnf -y -q install "./$asset" >/dev/null
     else yum -y -q localinstall "./$asset" >/dev/null; fi ;;
esac
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

	for _, t := range tools {
		pr.logln("installing " + t.ID + " (latest) from " + t.Repo)
		if err := a.runStep(ctx, id, lcFileToolScript(n.OS, t), lcFileToolEnv(t, n.Arch, n.UseProxy), pr.logln); err != nil {
			pr.logln(t.ID + " did not install: " + lastLines(err.Error(), 200) +
				" — the node is up regardless; install it from its terminal")
		}
	}

	// Read back what is on the node.
	cfg.FileTools = nil
	for _, t := range tools {
		v := a.lcFileToolVersion(ctx, id, t)
		if v == "" {
			pr.logln(t.ID + " did not install")
			continue
		}
		cfg.FileTools = append(cfg.FileTools, lcDBClient{Tool: t.ID, Label: t.Label, Version: v})
		pr.logln(t.ID + " " + v + " on PATH")
	}
}
