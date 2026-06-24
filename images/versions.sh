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
# Each prints version lines fenced by @@PS80@@ / @@PS84@@ / @@END@@ markers.

rhel_probe() {
  cat <<'EOS'
set +e
# On EL8 the distro ships a default `mysql` dnf module that masks Percona's
# percona-server-server package; disabling it makes the versions visible. This
# is a harmless no-op on releases without that module (EL9/EL10).
dnf -y -q module disable mysql >/dev/null 2>&1
strip() { sed -E 's/ .*//; s/^percona-server-server-//; s/\.el[0-9]+\.(x86_64|aarch64|noarch)$//'; }
search() { dnf -q search percona-server-server --showduplicates 2>/dev/null | grep -iE '^percona-server-server-[0-9]' | strip; }
percona-release setup ps80     >/dev/null 2>&1
echo '@@PS80@@'; search | grep -E '^8\.0\.' | sort -V -u
percona-release setup ps84lts  >/dev/null 2>&1
echo '@@PS84@@'; search | grep -E '^8\.4\.' | sort -V -u
echo '@@END@@'
EOS
}

debian_probe() {
  cat <<'EOS'
set +e
madison() {
  apt-cache madison percona-server-server 2>/dev/null \
    | awk -F'|' '{
        gsub(/^[ \t]+|[ \t]+$/,"",$1); gsub(/^[ \t]+|[ \t]+$/,"",$2); gsub(/^[ \t]+|[ \t]+$/,"",$3);
        if ($1=="percona-server-server" && $3 ~ /Packages/) print $2
      }' \
    | sed -E 's/\.(noble|jammy|focal|bookworm|bullseye|trixie)$//'
}
percona-release setup ps80     >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PS80@@'; madison | grep -E '^8\.0\.' | sort -V -u
percona-release setup ps84lts  >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PS84@@'; madison | grep -E '^8\.4\.' | sort -V -u
echo '@@END@@'
EOS
}

# Extract the lines for one marker section from captured probe output.
section() { awk -v s="$1" '$0=="@@"s"@@"{f=1;next} /^@@/{f=0} f' ; }

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
  echo "# Generated by \`make images\` and enriched by \`make versions\`. Do not edit"
  echo "# by hand — regenerate instead. Each image lists the Percona Server versions"
  echo "# installable on it (per OS, per platform), keyed by major series; the trailing"
  echo "# 'pmm' section lists the PMM3 server image versions selectable for a PMM node."
  echo "# Re-run image discovery: make images   Re-run version discovery: make versions"
  echo "generated_at: ${GENERATED_AT}"
  echo "versions_generated_at: $(ts)"
  echo "image_prefix: ${IMAGE_PREFIX}"
  echo "images:"
} >"$TMP"

count=0
while IFS=$'\t' read -r os version platform arch tag base built; do
  [ -n "$tag" ] || continue
  count=$((count + 1))

  case "$os" in
    oraclelinux|rhel|centos|rocky|almalinux) probe="$(rhel_probe)" ;;
    ubuntu|debian)                           probe="$(debian_probe)" ;;
    *) echo "WARN: unknown OS family '${os}' for ${tag}; skipping probe" >&2; probe="" ;;
  esac

  echo "==> probing ${tag} (${platform}) for Percona Server versions" >&2

  ps80="" ; ps84=""
  if [ -n "$probe" ]; then
    if out="$(docker run --rm "$tag" bash -lc "$probe" 2>/dev/null)"; then
      ps80="$(printf '%s\n' "$out" | section PS80)"
      ps84="$(printf '%s\n' "$out" | section PS84)"
    else
      echo "    FAIL  could not run ${tag} (recording empty version lists)" >&2
    fi
  fi

  n80=$(printf '%s' "$ps80" | grep -c . || true)
  n84=$(printf '%s' "$ps84" | grep -c . || true)
  echo "    ps80: ${n80} version(s), ps84: ${n84} version(s)" >&2

  {
    echo "  - os: ${os}"
    echo "    version: \"${version}\""
    echo "    platform: ${platform}"
    echo "    arch: ${arch}"
    echo "    tag: ${tag}"
    echo "    base: ${base}"
    echo "    built_at: ${built}"
    echo "    percona_server:"
    if [ -n "$ps80" ]; then
      echo "      \"8.0\":"
      while IFS= read -r v; do [ -n "$v" ] && echo "        - ${v}"; done <<<"$ps80"
    else
      echo "      \"8.0\": []"
    fi
    if [ -n "$ps84" ]; then
      echo "      \"8.4\":"
      while IFS= read -r v; do [ -n "$v" ] && echo "        - ${v}"; done <<<"$ps84"
    else
      echo "      \"8.4\": []"
    fi
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

mv "$TMP" "$OUT"
trap - EXIT

echo "" >&2
echo "==================================================================" >&2
echo "Probed ${count} image(s) + ${pmm_n} PMM3 version(s) → ${OUT}" >&2
echo "==================================================================" >&2
