#!/usr/bin/env bash
#
# Probe the systemd base images built by `make images` for the Percona Server
# versions installable on each (OS × platform), and record them in
# versions.yaml at the repo root.
#
# For every image listed in versions.yaml we spin up a throwaway container and,
# using the percona-release manager that is already baked into the image, ask
# the package manager which percona-server-server builds are available:
#
#   RHEL family (Oracle Linux):
#       percona-release setup ps80     # Percona Server 8.0
#       dnf search percona-server-server --showduplicates
#       percona-release setup ps84lts  # Percona Server 8.4 LTS
#       dnf search percona-server-server --showduplicates
#
#   Debian family (Ubuntu): same products, queried with apt-cache madison.
#
# Results are written back under each image entry as a `percona_server:` map
# keyed by major series ("8.0", "8.4"). A series that the OS has no packages for
# is recorded as an empty list. Re-run: make versions
#
# Querying a non-native platform requires the local Docker to be able to run it
# (e.g. via binfmt/qemu); images that cannot be started are recorded with empty
# version lists and skipped, never aborting the run.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/versions.yaml"

if [ ! -f "$OUT" ]; then
  echo "ERROR: $OUT not found — run 'make images' first." >&2
  exit 1
fi

ts() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

# Pull the header values we want to preserve across the rewrite.
IMAGE_PREFIX="$(grep -E '^image_prefix:' "$OUT" | head -1 | sed -E 's/^image_prefix:[[:space:]]*//')"
GENERATED_AT="$(grep -E '^generated_at:' "$OUT" | head -1 | sed -E 's/^generated_at:[[:space:]]*//')"
[ -n "$IMAGE_PREFIX" ] || IMAGE_PREFIX="dbcanvas-systemd"
[ -n "$GENERATED_AT" ] || GENERATED_AT="$(ts)"

# ---- parse existing image entries: os \t version \t platform \t arch \t tag \t base \t built_at ----
parse_entries() {
  awk '
    function val(s){ sub(/^[^:]*:[[:space:]]*/,"",s); gsub(/"/,"",s); return s }
    function emit(){ if(seen) print os"\t"version"\t"platform"\t"arch"\t"tag"\t"base"\t"built }
    /^  - os:/      { emit(); seen=1; os=val($0); next }
    /^    version:/ { version=val($0); next }
    /^    platform:/{ platform=val($0); next }
    /^    arch:/    { arch=val($0); next }
    /^    tag:/     { tag=val($0); next }
    /^    base:/    { base=val($0); next }
    /^    built_at:/{ built=val($0); next }
    END           { emit() }
  ' "$OUT"
}

# ---- in-container probe scripts, one per OS family ----
# Each prints version lines (newest first) fenced by @@PS80@@ / @@PS84@@ /
# @@PXC80@@ / @@PXC84@@ / @@PROXYSQL2@@ / @@PROXYSQL3@@ / @@END@@ markers —
# Percona Server and Percona XtraDB Cluster (each 8.0 and 8.4) plus ProxySQL
# (major series 2 and 3, from the proxysql2 / proxysql3 packages).

rhel_probe() {
  cat <<'EOS'
set +e
# On EL8 the distro ships a default `mysql` dnf module that masks Percona's
# packages; disabling it makes the versions visible. Harmless no-op on EL9/EL10.
dnf -y -q module disable mysql >/dev/null 2>&1
# elsearch <pkg>: exact package versions, normalised (e.g. 8.0.30-22.1).
elsearch() {
  dnf -q search "$1" --showduplicates 2>/dev/null | grep -iE "^$1-[0-9]" \
    | sed -E "s/ .*//; s/^$1-//; s/\.el[0-9]+\.(x86_64|aarch64|noarch)$//"
}
percona-release setup ps80     >/dev/null 2>&1
echo '@@PS80@@';  elsearch percona-server-server   | grep -E '^8\.0\.' | sort -rV -u
percona-release setup ps84lts  >/dev/null 2>&1
echo '@@PS84@@';  elsearch percona-server-server   | grep -E '^8\.4\.' | sort -rV -u
percona-release setup pxc80    >/dev/null 2>&1
echo '@@PXC80@@'; elsearch percona-xtradb-cluster  | grep -E '^8\.0\.' | sort -rV -u
percona-release setup pxc84lts >/dev/null 2>&1
echo '@@PXC84@@'; elsearch percona-xtradb-cluster  | grep -E '^8\.4\.' | sort -rV -u
# ProxySQL: a single 'proxysql' repo carries both the proxysql2 and proxysql3
# packages; enumerate each separately (proxysql2-2.x.y, proxysql3-3.x.y).
percona-release setup proxysql >/dev/null 2>&1
echo '@@PROXYSQL2@@'; elsearch proxysql2 | grep -E '^2\.' | sort -rV -u
echo '@@PROXYSQL3@@'; elsearch proxysql3 | grep -E '^3\.' | sort -rV -u
echo '@@END@@'
EOS
}

debian_probe() {
  cat <<'EOS'
set +e
# madison <pkg>: exact package versions, with any "N:" epoch prefix and the
# distro codename suffix stripped (PXC carries an epoch, e.g. 1:8.0.45-36-1.noble).
madison() {
  apt-cache madison "$1" 2>/dev/null \
    | awk -F'|' -v p="$1" '{
        gsub(/^[ \t]+|[ \t]+$/,"",$1); gsub(/^[ \t]+|[ \t]+$/,"",$2); gsub(/^[ \t]+|[ \t]+$/,"",$3);
        if ($1==p && $3 ~ /Packages/) print $2
      }' \
    | sed -E 's/^[0-9]+://; s/\.(noble|jammy|focal|bookworm|bullseye|trixie)$//'
}
percona-release setup ps80     >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PS80@@';  madison percona-server-server   | grep -E '^8\.0\.' | sort -rV -u
percona-release setup ps84lts  >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PS84@@';  madison percona-server-server   | grep -E '^8\.4\.' | sort -rV -u
percona-release setup pxc80    >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PXC80@@'; madison percona-xtradb-cluster  | grep -E '^8\.0\.' | sort -rV -u
percona-release setup pxc84lts >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PXC84@@'; madison percona-xtradb-cluster  | grep -E '^8\.4\.' | sort -rV -u
percona-release setup proxysql >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PROXYSQL2@@'; madison proxysql2 | grep -E '^2\.' | sort -rV -u
echo '@@PROXYSQL3@@'; madison proxysql3 | grep -E '^3\.' | sort -rV -u
echo '@@END@@'
EOS
}

# Extract the lines for one marker section from captured probe output.
section() { awk -v s="$1" '$0=="@@"s"@@"{f=1;next} /^@@/{f=0} f' ; }

# PDPS (Percona Distribution for MySQL using Percona Server) repositories are
# enumerated from the percona-release manager itself (`percona-release | grep pdps`).
# Each repo name (e.g. pdps-80-lts, pdps-84-lts, pdps-8x-innovation) is what you
# pass to `percona-release enable <repo>`; the repo determines the Percona Server
# major/minor series installed. Cross-OS, so discover once from any built image.
pdps_discover() {
  docker run --rm "$1" bash -lc 'percona-release 2>&1 | grep -oiE "pdps[a-z0-9._-]*" | sort -u' 2>/dev/null
}

# PMM3 (Percona Monitoring and Management) ships as the percona/pmm-server Docker
# image rather than an OS package, so its installable minor versions come from
# the image registry, not from inside a container. Query Docker Hub for the
# repository's tags and keep the full three-part PMM 3.x.y releases. Prints one
# version per line (ascending); empty output means discovery failed/offline.
PMM_REPO="percona/pmm-server"
pmm_discover() {
  command -v curl >/dev/null 2>&1 || { echo "WARN: curl not found; skipping PMM discovery" >&2; return 0; }
  local url="https://hub.docker.com/v2/repositories/${PMM_REPO}/tags?page_size=100&ordering=last_updated"
  local page=1
  : >/tmp/pmm_tags.$$
  while [ -n "$url" ] && [ "$page" -le 10 ]; do
    local body
    body="$(curl -fsSL "$url" 2>/dev/null)" || break
    # Pull tag names and the URL of the next page out of the JSON without a JSON
    # parser: names appear as "name":"<tag>", the next page as "next":"<url>".
    printf '%s' "$body" | grep -oE '"name": *"[^"]+"' | sed -E 's/.*: *"([^"]+)"/\1/' >>/tmp/pmm_tags.$$
    url="$(printf '%s' "$body" | grep -oE '"next": *"[^"]+"' | head -1 | sed -E 's/.*: *"([^"]+)"/\1/')"
    [ "$url" = "null" ] && url=""
    page=$((page + 1))
  done
  # Newest first (recent → oldest) so the version picker lists latest at the top.
  grep -E '^3\.[0-9]+\.[0-9]+$' /tmp/pmm_tags.$$ 2>/dev/null | sort -rV -u
  rm -f /tmp/pmm_tags.$$
}

# ---- write the YAML, enriching each entry with its Percona Server versions ----
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

{
  echo "# Generated by \`make images\` and enriched by \`make versions\`. Do not edit by"
  echo "# hand — regenerate instead. Each image lists the Percona Server and Percona"
  echo "# XtraDB Cluster versions installable on it (per OS, per platform), keyed by"
  echo "# major series, newest first; the trailing 'pmm' section lists the PMM3 server"
  echo "# image versions selectable for a PMM node."
  echo "# Re-run image discovery: make images   Re-run version discovery: make versions"
  echo "generated_at: ${GENERATED_AT}"
  echo "versions_generated_at: $(ts)"
  echo "image_prefix: ${IMAGE_PREFIX}"
  echo "images:"
} >"$TMP"

count=0
first_tag=""
while IFS=$'\t' read -r os version platform arch tag base built; do
  [ -n "$tag" ] || continue
  count=$((count + 1))
  [ -n "$first_tag" ] || first_tag="$tag"

  case "$os" in
    oraclelinux|rhel|centos|rocky|almalinux) probe="$(rhel_probe)" ;;
    ubuntu|debian)                           probe="$(debian_probe)" ;;
    *) echo "WARN: unknown OS family '${os}' for ${tag}; skipping probe" >&2; probe="" ;;
  esac

  echo "==> probing ${tag} (${platform}) for installable versions" >&2

  ps80="" ; ps84="" ; pxc80="" ; pxc84="" ; psql2="" ; psql3=""
  if [ -n "$probe" ]; then
    if out="$(docker run --rm "$tag" bash -lc "$probe" 2>/dev/null)"; then
      ps80="$(printf '%s\n' "$out" | section PS80)"
      ps84="$(printf '%s\n' "$out" | section PS84)"
      pxc80="$(printf '%s\n' "$out" | section PXC80)"
      pxc84="$(printf '%s\n' "$out" | section PXC84)"
      psql2="$(printf '%s\n' "$out" | section PROXYSQL2)"
      psql3="$(printf '%s\n' "$out" | section PROXYSQL3)"
    else
      echo "    FAIL  could not run ${tag} (recording empty version lists)" >&2
    fi
  fi

  n80=$(printf '%s' "$ps80" | grep -c . || true)
  n84=$(printf '%s' "$ps84" | grep -c . || true)
  px0=$(printf '%s' "$pxc80" | grep -c . || true)
  px4=$(printf '%s' "$pxc84" | grep -c . || true)
  pq2=$(printf '%s' "$psql2" | grep -c . || true)
  pq3=$(printf '%s' "$psql3" | grep -c . || true)
  echo "    ps: ${n80}+${n84}  pxc: ${px0}+${px4}  proxysql: ${pq2}+${pq3}" >&2

  # emit_series <indent-key> <key1> <list1> <key2> <list2>: emit a major-series
  # map under `key:` with two series (e.g. "8.0"/"8.4" or "2"/"3").
  emit_series() {
    local key="$1" k1="$2" v1="$3" k2="$4" v2="$5"
    echo "    ${key}:"
    if [ -n "$v1" ]; then
      echo "      \"${k1}\":"
      while IFS= read -r v; do [ -n "$v" ] && echo "        - ${v}"; done <<<"$v1"
    else
      echo "      \"${k1}\": []"
    fi
    if [ -n "$v2" ]; then
      echo "      \"${k2}\":"
      while IFS= read -r v; do [ -n "$v" ] && echo "        - ${v}"; done <<<"$v2"
    else
      echo "      \"${k2}\": []"
    fi
  }

  {
    echo "  - os: ${os}"
    echo "    version: \"${version}\""
    echo "    platform: ${platform}"
    echo "    arch: ${arch}"
    echo "    tag: ${tag}"
    echo "    base: ${base}"
    echo "    built_at: ${built}"
    emit_series percona_server         "8.0" "$ps80"  "8.4" "$ps84"
    emit_series percona_xtradb_cluster "8.0" "$pxc80" "8.4" "$pxc84"
    emit_series proxysql               "2"   "$psql2" "3"   "$psql3"
  } >>"$TMP"
done < <(parse_entries)

if [ "$count" -eq 0 ]; then
  echo "ERROR: no image entries found in ${OUT}; run 'make images' first." >&2
  exit 1
fi

# ---- PMM3 minor versions (from the percona/pmm-server registry) ----
echo "==> discovering PMM3 minor versions from ${PMM_REPO}" >&2
pmm_versions="$(pmm_discover)"
pmm_n=$(printf '%s' "$pmm_versions" | grep -c . || true)
# List is newest-first, so the latest is the first line.
pmm_latest="$(printf '%s\n' "$pmm_versions" | head -1)"
echo "    pmm3: ${pmm_n} version(s)${pmm_latest:+, latest ${pmm_latest}}" >&2
{
  echo "# PMM3 (Percona Monitoring and Management) server image versions, discovered"
  echo "# from the ${PMM_REPO} registry. 'default_tag' is the rolling latest-3.x tag"
  echo "# used when no specific minor version is selected. Re-run: make versions"
  echo "pmm:"
  echo "  repository: ${PMM_REPO}"
  echo "  default_tag: \"3\""
  if [ -n "$pmm_latest" ]; then
    echo "  latest: \"${pmm_latest}\""
  else
    echo "  latest: \"\""
  fi
  if [ -n "$pmm_versions" ]; then
    echo "  versions:"
    while IFS= read -r v; do [ -n "$v" ] && echo "    - \"${v}\""; done <<<"$pmm_versions"
  else
    echo "  versions: []"
  fi
} >>"$TMP"

# ---- PDPS repositories (from percona-release, for InnoDB/Group Replication) ----
echo "==> discovering PDPS repositories from percona-release (${first_tag})" >&2
pdps_repos="$(pdps_discover "$first_tag")"
pdps_n=$(printf '%s' "$pdps_repos" | grep -c . || true)
echo "    pdps: ${pdps_n} repo(s)" >&2
{
  echo "# PDPS (Percona Distribution for MySQL / Percona Server) repositories available"
  echo "# via percona-release — pass a name to 'percona-release enable <repo>'. The repo"
  echo "# determines the Percona Server major/minor series. Re-run: make versions"
  if [ -n "$pdps_repos" ]; then
    echo "pdps:"
    while IFS= read -r r; do [ -n "$r" ] && echo "  - \"${r}\""; done <<<"$pdps_repos"
  else
    echo "pdps: []"
  fi
} >>"$TMP"

mv "$TMP" "$OUT"
trap - EXIT

echo "" >&2
echo "==================================================================" >&2
echo "Probed ${count} image(s) + ${pmm_n} PMM3 version(s) → ${OUT}" >&2
echo "==================================================================" >&2
