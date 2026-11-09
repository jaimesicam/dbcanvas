package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// haproxysource.go — where an HAProxy node's haproxy package comes from.
//
// Three sources, chosen on the node (HAProxySource):
//
//	""        the OS's own repository (AppStream on EL, the Ubuntu archive). The version is
//	          whatever that OS release ships; there is nothing to choose.
//	"percona" percona-haproxy from Percona's ppg-NN repositories — the HAProxy that ships with
//	          Percona Distribution for PostgreSQL, on the 2.8 LTS line. Every ppg-NN repository
//	          carries it, each with its own range of builds (2.5 and 2.6 only in ppg-13 to
//	          ppg-15; ppg-18 starts at 2.8.16), so the install tries the backend's own series first and then the others,
//	          newest first, until one has the version asked for.
//	"pgdg"    haproxy from PGDG's pgdg-rhelN-extras repository: current HAProxy releases, 2.7
//	          through 3.x. EL only — apt.postgresql.org publishes no HAProxy.
//
// All three install the same /usr/sbin/haproxy, haproxy.service and /etc/haproxy/haproxy.cfg
// (percona-haproxy declares Provides: haproxy), so nothing after the install branches on the
// source. The versions offered are the percona_haproxy and pgdg_haproxy sections of
// versions.yaml (images/versions.sh), keyed by HAProxy branch ("2.8", "3.4").

const (
	haproxySrcOS      = ""
	haproxySrcPercona = "percona"
	haproxySrcPGDG    = "pgdg"
)

// haproxyPPGSeries are the ppg-NN repositories percona-haproxy is looked for in, newest first.
var haproxyPPGSeries = []string{"18", "17", "16", "15", "14", "13"}

// haproxyRepoMarker is where the install leaves the name of the repository it used, for the
// deployment's config.
const haproxyRepoMarker = "/etc/haproxy/.dbcanvas-repo"

// haproxySourceLabel names a source for messages and the panel.
func haproxySourceLabel(src string) string {
	switch strings.TrimSpace(src) {
	case haproxySrcPercona:
		return "Percona (percona-haproxy)"
	case haproxySrcPGDG:
		return "PGDG (pgdg-rhel-extras)"
	}
	return "the OS repository"
}

// haproxyCatalogSection is the versions.yaml section a source's versions are recorded in; ""
// for the OS package, which has none.
func haproxyCatalogSection(src string) string {
	switch strings.TrimSpace(src) {
	case haproxySrcPercona:
		return "percona_haproxy"
	case haproxySrcPGDG:
		return "pgdg_haproxy"
	}
	return ""
}

// loadHAProxyCatalog is a source's versions per image, keyed by HAProxy branch. nil for the OS
// package.
func loadHAProxyCatalog(src string) []PXCImage {
	sec := haproxyCatalogSection(src)
	if sec == "" {
		return nil
	}
	return loadImageCatalog(sec)
}

// haproxyCatalogVersions is every version a source offers on one image, newest first, and
// whether the catalogue has an entry for that image at all.
func haproxyCatalogVersions(src, os, osVer, arch string) ([]string, bool) {
	for _, img := range loadHAProxyCatalog(src) {
		if img.OS == os && img.OSVersion == osVer && (arch == "" || img.Arch == archOr(arch)) {
			var out []string
			for _, vs := range img.Versions {
				out = append(out, vs...)
			}
			sort.Slice(out, func(i, j int) bool { return compareVersions(out[i], out[j]) > 0 })
			return out, true
		}
	}
	return nil, false
}

// haproxyPPGOrder is the order the Percona install tries the ppg-NN repositories in: the
// backend's own series first (a PostgreSQL backend's major), then the rest newest first.
func haproxyPPGOrder(backendMajor string) []string {
	// Not ppgMajorOf: its "" → 16 default would put ppg-16 first for a MySQL backend.
	first, _, _ := strings.Cut(strings.TrimSpace(backendMajor), ".")
	out := []string{}
	for _, s := range haproxyPPGSeries {
		if s == first {
			out = append(out, s)
		}
	}
	for _, s := range haproxyPPGSeries {
		if s != first {
			out = append(out, s)
		}
	}
	return out
}

// haproxyBackendPGMajor is the PostgreSQL major of the cluster an HAProxy fronts, or "" for a
// MySQL-family backend.
func haproxyBackendPGMajor(kind string, frame designFrame) string {
	switch kind {
	case "patroni", "repmgr", "pgstream", "spock":
		return frame.PGMajor
	}
	return ""
}

// haproxyInstallStep is the install script and environment for a node's source.
func haproxyInstallStep(n designNode, backendMajor string) (string, []string) {
	ver := strings.TrimSpace(n.HAProxyVersion)
	debian := isDebianOS(n.OS)
	switch strings.TrimSpace(n.HAProxySource) {
	case haproxySrcPercona:
		env := []string{"VER=" + ver, "SERIES=" + strings.Join(haproxyPPGOrder(backendMajor), " ")}
		if debian {
			return haproxyInstallPerconaDebian, env
		}
		return haproxyInstallPerconaRHEL, env
	case haproxySrcPGDG:
		return haproxyInstallPGDGRHEL, []string{"VER=" + ver}
	}
	if debian {
		return haproxyInstallDebian, nil
	}
	return haproxyInstallRHEL, nil
}

// haproxyInstalledRepo reads back which repository the install used ("ppg-17",
// "pgdg-rhel9-extras"); "" for the OS package.
func (a *App) haproxyInstalledRepo(ctx context.Context, id string) string {
	res, err := a.engCtx(ctx).Exec(ctx, id, []string{"bash", "-c", "cat " + haproxyRepoMarker + " 2>/dev/null"}, nil)
	if err != nil || res.Code != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// haproxySourceIssues checks a node's source and version against the OS and the catalogue.
func haproxySourceIssues(n designNode) []issue {
	var out []issue
	bad := func(format string, args ...any) {
		out = append(out, issue{Level: "error", Message: "HAProxy node " + n.Label + ": " + fmt.Sprintf(format, args...)})
	}
	src, ver := strings.TrimSpace(n.HAProxySource), strings.TrimSpace(n.HAProxyVersion)
	switch src {
	case haproxySrcOS, haproxySrcPercona, haproxySrcPGDG:
	default:
		bad("unknown package source %q — use \"\" (the OS repository), \"percona\" or \"pgdg\"", src)
		return out
	}
	if src == haproxySrcPGDG && isDebianOS(n.OS) {
		bad("PGDG publishes HAProxy for EL only (the pgdg-rhelN-extras repository); apt.postgresql.org has none. Use Oracle Linux, or the Percona or OS package")
		return out
	}
	if ver == "" {
		return out
	}
	if src == haproxySrcOS {
		bad("the OS package cannot be pinned to a version — it installs what %s %s ships. Choose the Percona or PGDG source to pick one", n.OS, n.OSVersion)
		return out
	}
	vs, ok := haproxyCatalogVersions(src, n.OS, n.OSVersion, n.Arch)
	if !ok || len(vs) == 0 {
		// Nothing recorded for this image (`make versions` predates the source): the install
		// finds out, rather than validation refusing a version it cannot check.
		return out
	}
	for _, v := range vs {
		if v == ver {
			return out
		}
	}
	bad("%s has no HAProxy %s on %s %s — it has %s to %s", haproxySourceLabel(src), ver, n.OS, n.OSVersion, vs[len(vs)-1], vs[0])
	return out
}

// haproxyInstallPerconaRHEL/Debian install percona-haproxy from the first ppg-NN repository in
// SERIES that carries VER ("" → the newest build of the first one). The repository used is
// left in haproxyRepoMarker.
const haproxyInstallPerconaRHEL = `set -e
for S in $SERIES; do
  percona-release setup -y "ppg-$S" >/dev/null 2>&1 || continue
  spec=percona-haproxy
  if [ -n "$VER" ]; then
    dnf -q --showduplicates list "percona-haproxy-$VER" >/dev/null 2>&1 || continue
    spec="percona-haproxy-$VER"
  else
    dnf -q list percona-haproxy >/dev/null 2>&1 || continue
  fi
  dnf -y -q install "$spec" >/dev/null
  echo "ppg-$S" > ` + haproxyRepoMarker + `
  exit 0
done
echo "percona-haproxy ${VER:-(any version)} is in none of ppg-${SERIES// /, ppg-}" >&2
exit 1`

const haproxyInstallPerconaDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
for S in $SERIES; do
  percona-release setup -y "ppg-$S" >/dev/null 2>&1 || continue
  apt-get update -qq >/dev/null 2>&1 || true
  # madison prints 2:2.8.18-1.noble; VER is 2.8.18.
  exact=$(apt-cache madison percona-haproxy 2>/dev/null | awk -F'|' -v v="$VER" '{gsub(/ /,"",$2); s=$2; sub(/^[0-9]+:/,"",s)} v=="" || index(s, v "-")==1 {print $2; exit}')
  [ -n "$exact" ] || continue
  apt-get install -y -qq "percona-haproxy=$exact" >/dev/null
  echo "ppg-$S" > ` + haproxyRepoMarker + `
  exit 0
done
echo "percona-haproxy ${VER:-(any version)} is in none of ppg-${SERIES// /, ppg-}" >&2
exit 1`

// haproxyInstallPGDGRHEL installs haproxy from PGDG's extras repository: the repository RPM
// and the Oracle Linux releasever fix as repmgrInstallRHEL has them, then the extras repository
// turned on for this one install. The package is named haproxy, the same as the OS's, so the
// install checks where it came from rather than trusting the version to decide.
const haproxyInstallPGDGRHEL = `set -e
elver=$(rpm -E %rhel)
arch=$(uname -m)
rpm -q pgdg-redhat-repo >/dev/null 2>&1 || dnf -y -q install "https://download.postgresql.org/pub/repos/yum/reporpms/EL-${elver}-${arch}/pgdg-redhat-repo-latest.noarch.rpm" >/dev/null
if /usr/libexec/platform-python -c 'import sys, dnf.rpm; sys.exit(0 if dnf.rpm.detect_releasevers("/")[2] is None else 1)' 2>/dev/null \
   && [ -f /etc/yum.repos.d/pgdg-redhat-all.repo ]; then
  sed -i 's/\$releasever_major\.\$releasever_minor/$releasever_major/g' /etc/yum.repos.d/pgdg-redhat-all.repo
fi
repo="pgdg-rhel${elver}-extras"
spec=haproxy
[ -n "$VER" ] && spec="haproxy-$VER"
dnf -y -q --enablerepo="$repo" install "$spec" >/dev/null
from=$(dnf -q info --installed haproxy 2>/dev/null | awk -F': ' '/^From repo/{print $2; exit}')
if [ "$from" != "$repo" ]; then
  echo "haproxy was installed from ${from:-an unknown repository}, not $repo" >&2
  exit 1
fi
echo "$repo" > ` + haproxyRepoMarker

// composeHAProxySource reads compose's source= ("os" is the OS repository, as "" is).
func composeHAProxySource(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "os" {
		return haproxySrcOS
	}
	return s
}

// resolveHAProxyVersion is compose's version= for an HAProxy entry: a build ("2.8.16") or a
// branch ("3.2", the newest build in it) from the entry's source. Returns the branch and the
// build; a node records only the build, so a branch resolves to its newest one.
func resolveHAProxyVersion(source, os, osVer, arch, want string) (string, string, error) {
	src := composeHAProxySource(source)
	switch src {
	case haproxySrcOS, haproxySrcPercona, haproxySrcPGDG:
	default:
		return "", "", fmt.Errorf("source=%s: want os, percona or pgdg", source)
	}
	if src == haproxySrcPGDG && isDebianOS(os) {
		return "", "", fmt.Errorf("source=pgdg is Oracle Linux only — apt.postgresql.org publishes no HAProxy; use source=percona or the OS package")
	}
	want = strings.TrimSpace(want)
	if want == "" {
		return "", "", nil
	}
	if src == haproxySrcOS {
		return "", "", fmt.Errorf("version= needs source=percona or source=pgdg — the OS package is whatever %s %s ships", os, osVer)
	}
	major, minor, err := resolveCatalogVersion(loadHAProxyCatalog(src), os, osVer, archOr(arch), want)
	if err != nil {
		return "", "", err
	}
	if minor == "" {
		for _, img := range loadHAProxyCatalog(src) {
			if img.OS == os && img.OSVersion == osVer && img.Arch == archOr(arch) && len(img.Versions[major]) > 0 {
				minor = img.Versions[major][0]
			}
		}
	}
	return major, minor, nil
}
