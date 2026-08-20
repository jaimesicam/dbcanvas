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

# Only probe/record the single platform selected by DOCKER_PLATFORM (see
# platform.sh). Image entries on the other platform are dropped from
# versions.yaml — `make images` is what puts them back — so the host never
# probes (or advertises) an architecture it does not target.
# shellcheck source=platform.sh
. "$(dirname "${BASH_SOURCE[0]}")/platform.sh"
PLATFORM="$(resolve_platform "$ROOT")" || exit 1
echo "==> selected platform: ${PLATFORM}" >&2

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

# ---- ONLY: probe a subset of the upstreams ----
# ONLY is a comma-separated list of probe groups: "percona" (everything served by
# percona-release) and "upstream" (MariaDB from mariadb.org, MySQL Community from
# repo.mysql.com). Unset means both.
#
# This exists because the groups fail independently: internet connection issues can
# make one upstream unreachable, and re-probing it would otherwise block recording
# the other. A group that is not probed is not erased — carry_section copies that
# product's existing map out of the current versions.yaml, so a partial run edits
# only what it actually measured.
#
#   ONLY=upstream make versions    # refresh MariaDB + MySQL CE, keep Percona as-is
ONLY="${ONLY:-}"
want_probe() {
  [ -z "$ONLY" ] && return 0
  case ",${ONLY}," in *",$1,"*) return 0 ;; esac
  return 1
}
if [ -n "$ONLY" ]; then
  for g in ${ONLY//,/ }; do
    case "$g" in
      percona|upstream) ;;
      *) echo "ERROR: unknown ONLY group '${g}' (want: percona, upstream)" >&2; exit 1 ;;
    esac
  done
  echo "==> ONLY=${ONLY} — other product groups keep their recorded versions" >&2
fi

# carry_section <tag> <product-key>: reprint one product's existing block for one
# image, verbatim, from the versions.yaml being replaced. Returns 1 when the image
# or the product is not in the old file (a newly built image, or a product recorded
# for the first time), which tells the caller to emit an empty map instead.
carry_section() {
  awk -v tag="$1" -v key="$2" '
    $0 == "    tag: " tag { inimg = 1; next }
    inimg && /^  - os:/   { exit }
    inimg && $0 == "    " key ":" { found = 1; print; next }
    inimg && found && /^    [a-z_]+:/ { exit }
    inimg && found { print }
    END { exit !found }
  ' "$OUT"
}

# ---- in-container probe scripts, one per OS family ----
# Each prints version lines (newest first) fenced by @@PS80@@ / @@PS84@@ /
# @@PS57@@ / @@PXC80@@ / @@PXC84@@ / @@PROXYSQL2@@ / @@PROXYSQL3@@ /
# @@VALKEY91@@ / @@END@@ markers — Percona Server (8.0, 8.4 and the legacy 5.7
# series), Percona XtraDB Cluster (8.0 and 8.4), ProxySQL (major series 2 and
# 3, from the proxysql2 / proxysql3 packages), and Valkey (9.1, from the
# percona-valkey-bundle meta-package).

rhel_probe() {
  cat <<'EOS'
set +e
# On EL8 the distro ships default `mysql` and `mariadb` dnf modules that mask the
# third-party packages of the same name; disabling them makes the upstream versions
# visible. Harmless no-ops on EL9/EL10.
dnf -y -q module disable mysql mariadb >/dev/null 2>&1
# elsearch <pkg>: exact package versions, normalised (e.g. 8.0.30-22.1).
elsearch() {
  dnf -q search "$1" --showduplicates 2>/dev/null | grep -iE "^$1-[0-9]" \
    | sed -E "s/ .*//; s/^$1-//; s/\.el[0-9]+\.(x86_64|aarch64|noarch)$//"
}
EOS
  want_probe percona && cat <<'EOS'
percona-release setup ps80     >/dev/null 2>&1
echo '@@PS80@@';  elsearch percona-server-server   | grep -E '^8\.0\.' | sort -rV -u
percona-release setup ps84lts  >/dev/null 2>&1
echo '@@PS84@@';  elsearch percona-server-server   | grep -E '^8\.4\.' | sort -rV -u
# Legacy Percona Server 5.7 (EOL) — on EL the package keeps its own suffixed name
# (Percona-Server-server-57), unlike the unsuffixed 8.0/8.4 server package.
percona-release setup ps57     >/dev/null 2>&1
echo '@@PS57@@';  elsearch Percona-Server-server-57 | grep -E '^5\.7\.' | sort -rV -u
# Percona Server 9.7 LTS is written by hand, NOT through percona-release: version
# 1.0-33 (the newest published) lists ps97lts among its products and then requests
# repo.percona.com/ps-97lts/, which 404s, while the dashed spelling disables every
# Percona repo and enables nothing while exiting 0. Verified against a live EL9 node.
cat >/etc/yum.repos.d/dbc-ps-97.repo <<EOF
[dbc-ps-97-lts]
name=Percona Server 9.7 LTS
baseurl=https://repo.percona.com/ps-97-lts/yum/release/\$releasever/RPMS/\$basearch/
gpgkey=https://repo.percona.com/yum/PERCONA-PACKAGING-KEY
gpgcheck=1
enabled=1
skip_if_unavailable=1
EOF
echo '@@PS97@@';  elsearch percona-server-server   | grep -E '^9\.7\.' | sort -rV -u
percona-release setup pxc80    >/dev/null 2>&1
echo '@@PXC80@@'; elsearch percona-xtradb-cluster  | grep -E '^8\.0\.' | sort -rV -u
percona-release setup pxc84lts >/dev/null 2>&1
echo '@@PXC84@@'; elsearch percona-xtradb-cluster  | grep -E '^8\.4\.' | sort -rV -u
# ProxySQL: a single 'proxysql' repo carries both the proxysql2 and proxysql3
# packages; enumerate each separately (proxysql2-2.x.y, proxysql3-3.x.y).
percona-release setup proxysql >/dev/null 2>&1
echo '@@PROXYSQL2@@'; elsearch proxysql2 | grep -E '^2\.' | sort -rV -u
echo '@@PROXYSQL3@@'; elsearch proxysql3 | grep -E '^3\.' | sort -rV -u
# Percona Server for MongoDB: each psmdb-NN repo carries one major series
# (6.0/7.0/8.0); the percona-server-mongodb meta package is the versioned one.
percona-release setup psmdb-60 >/dev/null 2>&1
echo '@@PSMDB60@@'; elsearch percona-server-mongodb | grep -E '^6\.0\.' | sort -rV -u
percona-release setup psmdb-70 >/dev/null 2>&1
echo '@@PSMDB70@@'; elsearch percona-server-mongodb | grep -E '^7\.0\.' | sort -rV -u
percona-release setup psmdb-80 >/dev/null 2>&1
echo '@@PSMDB80@@'; elsearch percona-server-mongodb | grep -E '^8\.0\.' | sort -rV -u
# Percona Distribution for PostgreSQL: each ppg-NN repo carries one major series
# (13..18); on EL the versioned meta package is percona-postgresqlNN (no hyphen;
# the server is percona-postgresqlNN-server).
# The PG packages carry an epoch (e.g. percona-postgresql16-1:16.14-2.el9), so
# strip the leading "N:" that elsearch leaves in place before filtering on the
# major series.
percona-release setup ppg-13 >/dev/null 2>&1
echo '@@PPG13@@'; elsearch percona-postgresql13 | sed -E 's/^[0-9]+://' | grep -E '^13\.' | sort -rV -u
percona-release setup ppg-14 >/dev/null 2>&1
echo '@@PPG14@@'; elsearch percona-postgresql14 | sed -E 's/^[0-9]+://' | grep -E '^14\.' | sort -rV -u
percona-release setup ppg-15 >/dev/null 2>&1
echo '@@PPG15@@'; elsearch percona-postgresql15 | sed -E 's/^[0-9]+://' | grep -E '^15\.' | sort -rV -u
percona-release setup ppg-16 >/dev/null 2>&1
echo '@@PPG16@@'; elsearch percona-postgresql16 | sed -E 's/^[0-9]+://' | grep -E '^16\.' | sort -rV -u
percona-release setup ppg-17 >/dev/null 2>&1
echo '@@PPG17@@'; elsearch percona-postgresql17 | sed -E 's/^[0-9]+://' | grep -E '^17\.' | sort -rV -u
percona-release setup ppg-18 >/dev/null 2>&1
echo '@@PPG18@@'; elsearch percona-postgresql18 | sed -E 's/^[0-9]+://' | grep -E '^18\.' | sort -rV -u
# Valkey: percona-valkey-bundle is the meta-package (pulls in the real server
# plus the bloom/json/ldap/search modules); its own version tracks the set.
percona-release enable valkey-91 >/dev/null 2>&1 || percona-release setup -y valkey-91 >/dev/null 2>&1
echo '@@VALKEY91@@'; elsearch percona-valkey-bundle | grep -E '^9\.1\.' | sort -rV -u
# Percona Orchestrator: bundled only in PDPS ("Percona Distribution for MySQL"),
# NOT in PDPXC ("...- PXC") — confirmed live: pdpxc-84-lts carries no
# percona-orchestrator package at all, even though it sounds like the more
# on-the-nose choice for a PXC-adjacent tool. Not versioned per MySQL major
# series, so no grep filter beyond a leading digit. The package carries an
# epoch (e.g. percona-orchestrator-2:3.2.6-22.el9), stripped the same way the
# PG packages are.
percona-release setup pdps-84-lts >/dev/null 2>&1
echo '@@ORCH@@'; elsearch percona-orchestrator | sed -E 's/^[0-9]+://' | sort -rV -u
EOS
  want_probe upstream && cat <<'EOS'
# ---- Non-Percona upstreams: MariaDB and MySQL Community ----
# Neither is managed by percona-release, so their repos are written by hand. Both
# are PER MAJOR (like the ppg-NN repos): one baseurl per series, so every series
# gets its own repo file. They are all left enabled and the series is selected by
# grepping the version — filtering by repo would need dnf config-manager, which
# buys nothing here because the version prefix already identifies the series.
#
# skip_if_unavailable=1 matters: not every series exists for every EL release
# (MySQL 8.0 has no el10 repo, MariaDB has no el10 build before 11.4). Without it
# one 404 aborts the whole dnf transaction and every later probe returns empty.
for V in 10.6 10.11 11.4 11.8; do
  cat >"/etc/yum.repos.d/dbc-mariadb-$V.repo" <<EOF
[dbc-mariadb-$V]
name=MariaDB $V
baseurl=https://mirror.mariadb.org/yum/$V/rhel/\$releasever/\$basearch
gpgkey=https://mirror.mariadb.org/yum/RPM-GPG-KEY-MariaDB
gpgcheck=1
skip_if_unavailable=1
module_hotfixes=1
EOF
done
# The EL packages are capitalised (MariaDB-server) — the distro's own lowercase
# mariadb-server is a different, older build from AppStream and is NOT what these
# repos install. On EL8 the distro also ships a `mariadb` dnf module that would
# mask the upstream packages, hence the module disable above.
echo '@@MARIADB106@@';  elsearch MariaDB-server | grep -E '^10\.6\.'  | sort -rV -u
echo '@@MARIADB1011@@'; elsearch MariaDB-server | grep -E '^10\.11\.' | sort -rV -u
echo '@@MARIADB114@@';  elsearch MariaDB-server | grep -E '^11\.4\.'  | sort -rV -u
echo '@@MARIADB118@@';  elsearch MariaDB-server | grep -E '^11\.8\.'  | sort -rV -u
# MySQL Community. Note the repo path is mysql-8.4-community on yum but the apt
# component is mysql-8.4-lts — the two are not spelled the same (see debian_probe).
# The signing key MUST be RPM-GPG-KEY-mysql-2025: the widely-cited -2023 file is the
# same key ID (B7B3B788A8D3785C) but its published copy expired 2025-10-22, so
# installs fail the signature check even though metadata downloads fine.
for V in 8.0 8.4 9.7; do
  cat >"/etc/yum.repos.d/dbc-mysqlce-$V.repo" <<EOF
[dbc-mysqlce-$V]
name=MySQL $V Community
baseurl=https://repo.mysql.com/yum/mysql-$V-community/el/\$releasever/\$basearch/
gpgkey=https://repo.mysql.com/RPM-GPG-KEY-mysql-2025
gpgcheck=1
skip_if_unavailable=1
module_hotfixes=1
EOF
done
echo '@@MYSQLCE80@@'; elsearch mysql-community-server | grep -E '^8\.0\.' | sort -rV -u
echo '@@MYSQLCE84@@'; elsearch mysql-community-server | grep -E '^8\.4\.' | sort -rV -u
echo '@@MYSQLCE97@@'; elsearch mysql-community-server | grep -E '^9\.7\.' | sort -rV -u
EOS
  echo "echo '@@END@@'"
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
EOS
  want_probe percona && cat <<'EOS'
percona-release setup ps80     >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PS80@@';  madison percona-server-server   | grep -E '^8\.0\.' | sort -rV -u
percona-release setup ps84lts  >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PS84@@';  madison percona-server-server   | grep -E '^8\.4\.' | sort -rV -u
# Legacy Percona Server 5.7 (EOL) — on Debian the package is percona-server-server-5.7.
percona-release setup ps57     >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PS57@@';  madison percona-server-server-5.7 | grep -E '^5\.7\.' | sort -rV -u
# Percona Server 9.7 LTS by hand — percona-release cannot enable it (see rhel_probe).
apt-get install -y -qq curl gnupg ca-certificates >/dev/null 2>&1
install -d /etc/apt/keyrings
curl -fsSL https://repo.percona.com/yum/PERCONA-PACKAGING-KEY 2>/dev/null | gpg --batch --yes --dearmor -o /etc/apt/keyrings/dbc-percona.gpg 2>/dev/null
echo "deb [signed-by=/etc/apt/keyrings/dbc-percona.gpg] https://repo.percona.com/ps-97-lts/apt $(. /etc/os-release; echo "$VERSION_CODENAME") main" \
  >/etc/apt/sources.list.d/dbc-ps-97.list
apt-get update >/dev/null 2>&1
echo '@@PS97@@';  madison percona-server-server   | grep -E '^9\.7\.' | sort -rV -u
percona-release setup pxc80    >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PXC80@@'; madison percona-xtradb-cluster  | grep -E '^8\.0\.' | sort -rV -u
percona-release setup pxc84lts >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PXC84@@'; madison percona-xtradb-cluster  | grep -E '^8\.4\.' | sort -rV -u
percona-release setup proxysql >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PROXYSQL2@@'; madison proxysql2 | grep -E '^2\.' | sort -rV -u
echo '@@PROXYSQL3@@'; madison proxysql3 | grep -E '^3\.' | sort -rV -u
percona-release setup psmdb-60 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PSMDB60@@'; madison percona-server-mongodb | grep -E '^6\.0\.' | sort -rV -u
percona-release setup psmdb-70 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PSMDB70@@'; madison percona-server-mongodb | grep -E '^7\.0\.' | sort -rV -u
percona-release setup psmdb-80 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PSMDB80@@'; madison percona-server-mongodb | grep -E '^8\.0\.' | sort -rV -u
percona-release setup ppg-13 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PPG13@@'; madison percona-postgresql-13 | grep -E '^13\.' | sort -rV -u
percona-release setup ppg-14 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PPG14@@'; madison percona-postgresql-14 | grep -E '^14\.' | sort -rV -u
percona-release setup ppg-15 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PPG15@@'; madison percona-postgresql-15 | grep -E '^15\.' | sort -rV -u
percona-release setup ppg-16 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PPG16@@'; madison percona-postgresql-16 | grep -E '^16\.' | sort -rV -u
percona-release setup ppg-17 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PPG17@@'; madison percona-postgresql-17 | grep -E '^17\.' | sort -rV -u
percona-release setup ppg-18 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@PPG18@@'; madison percona-postgresql-18 | grep -E '^18\.' | sort -rV -u
percona-release enable valkey-91 >/dev/null 2>&1 || percona-release setup -y valkey-91 >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@VALKEY91@@'; madison percona-valkey-bundle | grep -E '^9\.1\.' | sort -rV -u
# Percona Orchestrator: see the matching comment in rhel_probe — PDPS only, not
# versioned per MySQL major series. madison() already strips the epoch/codename.
percona-release setup pdps-84-lts >/dev/null 2>&1; apt-get update >/dev/null 2>&1
echo '@@ORCH@@'; madison percona-orchestrator | sort -rV -u
EOS
  want_probe upstream && cat <<'EOS'
# ---- Non-Percona upstreams: MariaDB and MySQL Community ----
# See the matching block in rhel_probe. Three Debian-specific differences:
#   * MariaDB's apt repo is per major AND per codename, and older series have no
#     build for newer codenames (10.6 has no noble). A missing series makes
#     `apt-get update` print an error for that one list file and carry on, so the
#     other series still resolve — the missing one just yields an empty list.
#   * MySQL's apt components are spelled differently from its yum repo paths:
#     mysql-8.0 but mysql-8.4-lts (yum uses mysql-8.4-community).
#   * Both upstreams split their apt trees by distribution, not just by codename
#     (.../repo/11.4/ubuntu vs .../repo/11.4/debian), so the path comes from
#     os-release's ID — Ubuntu codenames are not served under the Debian tree and
#     vice versa. Percona's own repos need no such split: percona-release writes
#     them itself.
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq curl gnupg ca-certificates >/dev/null 2>&1
install -d /etc/apt/keyrings
CODE="$(. /etc/os-release; echo "$VERSION_CODENAME")"
DISTRO="$(. /etc/os-release; echo "$ID")"
curl -fsSL https://mariadb.org/mariadb_release_signing_key.pgp -o /etc/apt/keyrings/dbc-mariadb.pgp 2>/dev/null
# RPM-GPG-KEY-mysql-2025, not -2023: same key, but the -2023 copy is expired.
curl -fsSL https://repo.mysql.com/RPM-GPG-KEY-mysql-2025 2>/dev/null | gpg --batch --yes --dearmor -o /etc/apt/keyrings/dbc-mysql.gpg 2>/dev/null
for V in 10.6 10.11 11.4 11.8; do
  echo "deb [signed-by=/etc/apt/keyrings/dbc-mariadb.pgp] https://mirror.mariadb.org/repo/$V/$DISTRO $CODE main" \
    >"/etc/apt/sources.list.d/dbc-mariadb-$V.list"
done
echo "deb [signed-by=/etc/apt/keyrings/dbc-mysql.gpg] https://repo.mysql.com/apt/$DISTRO $CODE mysql-8.0" \
  >/etc/apt/sources.list.d/dbc-mysqlce-8.0.list
echo "deb [signed-by=/etc/apt/keyrings/dbc-mysql.gpg] https://repo.mysql.com/apt/$DISTRO $CODE mysql-8.4-lts" \
  >/etc/apt/sources.list.d/dbc-mysqlce-8.4.list
echo "deb [signed-by=/etc/apt/keyrings/dbc-mysql.gpg] https://repo.mysql.com/apt/$DISTRO $CODE mysql-9.7-lts" \
  >/etc/apt/sources.list.d/dbc-mysqlce-9.7.list
apt-get update >/dev/null 2>&1
# Restrict to the upstream builds (+maria~): Ubuntu's own archive also carries a
# mariadb-server, and offering its version here would advertise a build that the
# configured mariadb.org repo cannot install.
echo '@@MARIADB106@@';  madison mariadb-server | grep -E '^10\.6\..*\+maria'  | sort -rV -u
echo '@@MARIADB1011@@'; madison mariadb-server | grep -E '^10\.11\..*\+maria' | sort -rV -u
echo '@@MARIADB114@@';  madison mariadb-server | grep -E '^11\.4\..*\+maria'  | sort -rV -u
echo '@@MARIADB118@@';  madison mariadb-server | grep -E '^11\.8\..*\+maria'  | sort -rV -u
echo '@@MYSQLCE80@@'; madison mysql-community-server | grep -E '^8\.0\.' | sort -rV -u
echo '@@MYSQLCE84@@'; madison mysql-community-server | grep -E '^8\.4\.' | sort -rV -u
echo '@@MYSQLCE97@@'; madison mysql-community-server | grep -E '^9\.7\.' | sort -rV -u
EOS
  echo "echo '@@END@@'"
}

# Extract the lines for one marker section from captured probe output.
section() { awk -v s="$1" '$0=="@@"s"@@"{f=1;next} /^@@/{f=0} f' ; }

# PDPS (Percona Distribution for MySQL using Percona Server) repositories are
# enumerated from the percona-release manager itself. Each repo name (e.g.
# pdps-8.0, pdps-84-lts, pdps-9.7.1) is what you pass to `percona-release enable
# <repo>`; the repo determines the Percona Server major/minor series installed.
# Cross-OS, so discover once from any built image.
#
# Read ONLY the "Available repositories:" section. percona-release prints the same
# set twice, under two headings and in two spellings: "Available setup products"
# is undashed (pdps9.7.1, pdps97lts) and is what `setup` takes, "Available
# repositories" is dashed (pdps-9.7.1, pdps-97-lts) and is what `enable` takes.
# Scraping the whole output mixed both into the picker, and a frame that saved a
# product name failed at deploy with "ERROR: Unknown repository: pdps9.7.1" —
# which is how the 9.7 InnoDB Cluster frame broke (IMPLEMENTATION.md #277).
pdps_discover() {
  docker run --rm "$1" bash -lc 'percona-release 2>&1 |
    sed -n "/^Available repositories:/,/^Available components:/p" |
    grep -oiE "pdps[a-z0-9._-]*" | sort -u' 2>/dev/null
}

# PMM3 (Percona Monitoring and Management) ships as the percona/pmm-server Docker
# image rather than an OS package, so its installable minor versions come from
# the image registry, not from inside a container. Query Docker Hub for the
# repository's tags and keep the full three-part PMM 3.x.y releases. Prints one
# version per line (ascending); empty output means discovery failed/offline.
PMM_REPO="percona/pmm-server"

# ---- Spock (source-built PostgreSQL + Spock extension) availability ----
# Unlike the package-installed engines, a Spock member compiles PostgreSQL from
# source: the postgresql.org release tag for the chosen minor with the pinned
# Spock patch set applied. So its availability is NOT the Percona package catalog
# — it is (a) the PG majors the pinned Spock ref carries patches for, and (b) the
# postgresql.org release tags (minors) that exist for each. This is OS-independent
# and computed once; it is recorded only against Oracle Linux images because
# `spockPrepareNode` compiles on the RHEL toolchain only. Prints TAB-separated
# "major<TAB>minor,minor,…" lines, newest minor first. Keep SPOCK_REF in sync
# with app/spock.go's spockRef() default. Empty output (offline) → empty section.
SPOCK_REF="${SPOCK_REF:-v5.0.10}"
PG_SRC_REPO="${PG_SRC_REPO:-https://github.com/postgres/postgres}"
SPOCK_SRC_REPO="${SPOCK_SRC_REPO:-https://github.com/pgEdge/spock}"
spock_discover() {
  command -v git >/dev/null 2>&1 || { echo "WARN: git not found; skipping Spock discovery" >&2; return 0; }
  local tmp majors m mins
  tmp="$(mktemp -d)"
  if ! git clone --quiet --depth 1 --branch "$SPOCK_REF" --filter=blob:none --sparse \
        "$SPOCK_SRC_REPO" "$tmp/spock" >/dev/null 2>&1; then
    echo "WARN: could not clone Spock ${SPOCK_REF}; skipping Spock discovery" >&2
    rm -rf "$tmp"; return 0
  fi
  git -C "$tmp/spock" sparse-checkout set patches >/dev/null 2>&1
  # Numeric patch dirs are PG majors (skip non-numeric like "attic").
  majors="$(ls "$tmp/spock/patches" 2>/dev/null | grep -E '^[0-9]+$' | sort -n)"
  rm -rf "$tmp"
  for m in $majors; do
    # postgresql.org release tags REL_<major>_<minor>; keep numeric minors only
    # (drop BETA/RC), newest first, as "<major>.<minor>". A major with no stable
    # release yet (e.g. an in-development series) yields nothing and is omitted.
    mins="$(git ls-remote --tags --refs "$PG_SRC_REPO" "REL_${m}_*" 2>/dev/null \
      | sed -E "s#.*/REL_${m}_##" | grep -E '^[0-9]+$' | sort -rn | sed "s/^/${m}./" | paste -sd, -)"
    [ -n "$mins" ] && printf '%s\t%s\n' "$m" "$mins"
  done
}

# hub_tags <repo> <tag-regex> — list a Docker Hub repository's tags matching the
# regex, newest version first. Anonymous API, no JSON parser: tag names appear as
# "name":"<tag>" and the next page as "next":"<url>". Empty output means discovery
# failed (offline) — every caller treats that as "record nothing", never an error.
hub_tags() {
  local repo="$1" want="$2"
  command -v curl >/dev/null 2>&1 || { echo "WARN: curl not found; skipping ${repo} discovery" >&2; return 0; }
  local url="https://hub.docker.com/v2/repositories/${repo}/tags?page_size=100&ordering=last_updated"
  local page=1 tmp
  tmp="$(mktemp)"
  while [ -n "$url" ] && [ "$page" -le 10 ]; do
    local body
    body="$(curl -fsSL "$url" 2>/dev/null)" || break
    printf '%s' "$body" | grep -oE '"name": *"[^"]+"' | sed -E 's/.*: *"([^"]+)"/\1/' >>"$tmp"
    url="$(printf '%s' "$body" | grep -oE '"next": *"[^"]+"' | head -1 | sed -E 's/.*: *"([^"]+)"/\1/')"
    [ "$url" = "null" ] && url=""
    page=$((page + 1))
  done
  # Newest first so a version picker lists the latest at the top.
  grep -E "$want" "$tmp" 2>/dev/null | sort -rV -u
  rm -f "$tmp"
}

pmm_discover() { hub_tags "$PMM_REPO" '^3\.[0-9]+\.[0-9]+$'; }

# ---- Percona Kubernetes operators ----
# The operators ship as Docker images (and as a git tag carrying deploy/bundle.yaml +
# deploy/cr.yaml, which is what a K3D node actually installs). Their versions are
# therefore image tags, independent of OS and arch — recorded as a top-level section,
# like pmm. The repositories also carry auxiliary tags (…-backup, …-logcollector,
# …-haproxy); the three-part anchor keeps only the operator releases themselves.
OPERATOR_PRODUCTS="pxc ps psmdb pg"
operator_repo() {
  case "$1" in
    pxc)   echo "percona/percona-xtradb-cluster-operator" ;;
    ps)    echo "percona/percona-server-mysql-operator" ;;
    psmdb) echo "percona/percona-server-mongodb-operator" ;;
    pg)    echo "percona/percona-postgresql-operator" ;;
  esac
}
operator_discover() { hub_tags "$(operator_repo "$1")" '^[0-9]+\.[0-9]+\.[0-9]+$'; }

# ---- Helm charts (CloudNativePG and the pieces it can pull in) ----
# A K3D cluster installs these through k3s' bundled helm-controller (a HelmChart object),
# so what matters is the *chart* version, which lives in the repo's index.yaml rather than
# in any image registry. A chart version is not the operator version it ships: the
# CloudNativePG chart's 0.29.0 carries operator 1.30.x. Hence a section of its own.
CHART_PRODUCTS="cloudnative-pg kube-prometheus-stack cert-manager"
chart_repo_url() {
  case "$1" in
    cloudnative-pg)        echo "https://cloudnative-pg.github.io/charts" ;;
    kube-prometheus-stack) echo "https://prometheus-community.github.io/helm-charts" ;;
    cert-manager)          echo "https://charts.jetstack.io" ;;
  esac
}

# chart_versions <repo-url> <chart> — the chart's versions from index.yaml, newest first.
# index.yaml lists every chart the repo hosts under `entries:`, each a list of releases, so
# the scan has to stay inside the requested chart's block: a 2-space key at the same level
# starts the next chart.
#
# The version is kept verbatim, because it goes straight back into a HelmChart's `version:`
# and repos differ: cloudnative-pg publishes "0.29.0", cert-manager publishes "v1.21.1". The
# regex therefore allows an optional leading v, and rejects prereleases (cert-manager ships
# -alpha/-beta lines a lab has no business installing).
#
# CHART_VERSION_LIMIT caps how many are recorded. Without it kube-prometheus-stack alone
# contributes ~1200 entries, which bloats versions.yaml and makes the picker unusable.
CHART_VERSION_LIMIT="${CHART_VERSION_LIMIT:-40}"
chart_versions() {
  local url="$1" chart="$2"
  command -v curl >/dev/null 2>&1 || { echo "WARN: curl not found; skipping ${chart} chart discovery" >&2; return 0; }
  curl -fsSL --max-time 60 "${url}/index.yaml" 2>/dev/null | awk -v want="$chart" '
    /^  [a-zA-Z0-9_.-]+:[[:space:]]*$/ {
      key = $0; sub(/^  /, "", key); sub(/:[[:space:]]*$/, "", key)
      inchart = (key == want); next
    }
    # Exactly four spaces: that is a release-level key inside `- ` list items under the
    # chart. Any deeper `version:` belongs to a dependency (kube-prometheus-stack lists
    # plenty, including a bare 0.0.0 that would otherwise pass for a chart release).
    inchart && /^    version:[[:space:]]/ {
      v = $0; sub(/^    version:[[:space:]]*/, "", v); gsub(/["\047]/, "", v); print v
    }
  ' | grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+$' | sort -rV -u | head -n "$CHART_VERSION_LIMIT"
}

# ---- images a chart-installed operator is pointed at ----
# CloudNativePG selects its PostgreSQL with spec.imageName, and those images are on ghcr.io,
# not Docker Hub — so hub_tags cannot reach them. Only the bare major tags are recorded
# (13, 14, …): that is what imageName wants, and the full matrix is ~10k tags of
# minor/patch/distro variants.
CNPG_PG_IMAGE="ghcr.io/cloudnative-pg/postgresql"

# ghcr_tags <repo-path> <tag-regex> — list a ghcr.io repository's tags. ghcr needs an
# anonymous pull token first, and paginates through a Link header rather than a body field
# (and the first page is not ordered, so following it is not optional).
ghcr_tags() {
  local repo="$1" want="$2"
  command -v curl >/dev/null 2>&1 || { echo "WARN: curl not found; skipping ${repo} discovery" >&2; return 0; }
  local token
  token="$(curl -fsSL --max-time 30 "https://ghcr.io/token?scope=repository:${repo}:pull&service=ghcr.io" 2>/dev/null \
    | grep -oE '"token":"[^"]+"' | sed 's/.*:"//;s/"//')"
  [ -n "$token" ] || { echo "WARN: no ghcr.io token for ${repo}; skipping" >&2; return 0; }
  local path="/v2/${repo}/tags/list?n=1000" page=1 tmp hdr
  tmp="$(mktemp)"; hdr="$(mktemp)"
  while [ -n "$path" ] && [ "$page" -le 15 ]; do
    curl -fsSL --max-time 60 -D "$hdr" -H "Authorization: Bearer ${token}" "https://ghcr.io${path}" 2>/dev/null \
      | tr ',' '\n' | tr -d '"[]{}' | sed 's/.*: *//' >>"$tmp" || break
    path="$(grep -i '^link:' "$hdr" | sed -E 's@.*<([^>]+)>.*@\1@' | head -1)"
    page=$((page + 1))
  done
  grep -E "$want" "$tmp" 2>/dev/null | sort -rV -u
  rm -f "$tmp" "$hdr"
}
cnpg_pg_discover() { ghcr_tags "${CNPG_PG_IMAGE#ghcr.io/}" '^[0-9]+$'; }

# ---- Crunchy Postgres for Kubernetes (PGO) ----
# PGO is not on a Helm HTTP repository at all: Crunchy publishes the installer chart as an OCI
# artifact in their own registry, so there is no index.yaml to read and chart_versions cannot
# reach it. The registry's tags/list is the chart's version list, and it is the right list to
# offer — the GitHub tags are NOT. v5.8.9 and v6.0.3 are tagged on GitHub but their images
# return 404 ("no longer available from the Crunchy Data Developer Program"), while every tag
# published here pulls anonymously. Chart version == appVersion == the operator image tag.
CRUNCHY_REGISTRY="registry.developers.crunchydata.com"
CRUNCHY_AUTH="https://registry-auth.developers.crunchydata.com/auth?service=docker-registry"
PGO_CHART_REPO="crunchydata/pgo"

# crunchy_token <repository> — an anonymous pull token for the Crunchy developer registry.
crunchy_token() {
  curl -fsSL --max-time 30 "${CRUNCHY_AUTH}&scope=repository:${1}:pull" 2>/dev/null \
    | grep -oE '"token":"[^"]+"' | sed 's/.*:"//;s/"//'
}

pgo_chart_discover() {
  command -v curl >/dev/null 2>&1 || { echo "WARN: curl not found; skipping PGO discovery" >&2; return 0; }
  local token; token="$(crunchy_token "$PGO_CHART_REPO")"
  [ -n "$token" ] || { echo "WARN: no ${CRUNCHY_REGISTRY} token; skipping PGO discovery" >&2; return 0; }
  curl -fsSL --max-time 60 -H "Authorization: Bearer ${token}" \
    "https://${CRUNCHY_REGISTRY}/v2/${PGO_CHART_REPO}/tags/list" 2>/dev/null \
    | tr ',' '\n' | tr -d '"[]{}' | sed 's/.*: *//' \
    | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -rV -u | head -n "$CHART_VERSION_LIMIT"
}

# The PostgreSQL majors a PGO release can run are the relatedImages keys in the chart's own
# values.yaml (postgres_15 … postgres_18), which is also exactly what spec.postgresVersion
# accepts. Read them out of the newest chart rather than hardcoding a list that ages.
crunchy_pg_discover() {
  local ver="$1" token digest
  [ -n "$ver" ] || return 0
  command -v curl >/dev/null 2>&1 || return 0
  token="$(crunchy_token "$PGO_CHART_REPO")"
  [ -n "$token" ] || return 0
  digest="$(curl -fsSL --max-time 30 -H "Authorization: Bearer ${token}" \
    -H "Accept: application/vnd.oci.image.manifest.v1+json" \
    "https://${CRUNCHY_REGISTRY}/v2/${PGO_CHART_REPO}/manifests/${ver}" 2>/dev/null \
    | tr ',' '\n' | grep -oE '"digest":"sha256:[a-f0-9]+"' | tail -1 | sed 's/.*:"//;s/"//')"
  [ -n "$digest" ] || return 0
  curl -fsSL --max-time 60 -H "Authorization: Bearer ${token}" \
    "https://${CRUNCHY_REGISTRY}/v2/${PGO_CHART_REPO}/blobs/${digest}" 2>/dev/null \
    | tar xzO --wildcards '*/values.yaml' 2>/dev/null \
    | grep -oE '^  postgres_[0-9]+:' | grep -oE '[0-9]+' | sort -rn -u
}

# ---- k3s (the Kubernetes a K3D cluster runs) ----
# k3d creates k3s containers from rancher/k3s:<tag>. The tag is what fixes the cluster's
# Kubernetes version, so the K3D frame lets you pick it — and pinning matters: k3d's own
# default trails the releases, and an API server too old for an operator's CRDs makes the
# operator uninstallable (the ps-operator's clusterset CRD needs a 1.32+ CEL library).
# Only stable release tags (vX.Y.Z-k3sN); rc/beta and the -rc suffixes are dropped.
K3S_REPO="rancher/k3s"
k3s_discover() { hub_tags "$K3S_REPO" '^v[0-9]+\.[0-9]+\.[0-9]+-k3s[0-9]+$'; }

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

echo "==> discovering Spock (source-built PostgreSQL) majors/minors from ${SPOCK_REF}" >&2
SPOCK_MAP="$(spock_discover)"
spock_n=$(printf '%s' "$SPOCK_MAP" | grep -c . || true)
echo "    spock: ${spock_n} PG major series (Oracle Linux only)" >&2

count=0
skipped=0
first_tag=""
while IFS=$'\t' read -r os version platform arch tag base built; do
  [ -n "$tag" ] || continue
  # Not the platform this install targets: drop the entry entirely (do not probe
  # it, do not re-emit it into versions.yaml).
  if [ "$platform" != "$PLATFORM" ]; then
    echo "==> skipping ${tag} (${platform}) — DOCKER_PLATFORM is ${PLATFORM}" >&2
    skipped=$((skipped + 1))
    continue
  fi
  count=$((count + 1))
  [ -n "$first_tag" ] || first_tag="$tag"

  case "$os" in
    oraclelinux|rhel|centos|rocky|almalinux) probe="$(rhel_probe)" ;;
    ubuntu|debian)                           probe="$(debian_probe)" ;;
    *) echo "WARN: unknown OS family '${os}' for ${tag}; skipping probe" >&2; probe="" ;;
  esac

  echo "==> probing ${tag} (${platform}) for installable versions" >&2

  ps80="" ; ps84="" ; ps97="" ; ps57="" ; pxc80="" ; pxc84="" ; psql2="" ; psql3=""
  mdb60="" ; mdb70="" ; mdb80=""
  pg13="" ; pg14="" ; pg15="" ; pg16="" ; pg17="" ; pg18=""
  vk91=""
  orch=""
  md106="" ; md1011="" ; md114="" ; md118=""
  myc80="" ; myc84="" ; myc97=""
  if [ -n "$probe" ]; then
    if out="$(docker run --rm "$tag" bash -lc "$probe" 2>/dev/null)"; then
      ps80="$(printf '%s\n' "$out" | section PS80)"
      ps84="$(printf '%s\n' "$out" | section PS84)"
      ps57="$(printf '%s\n' "$out" | section PS57)"
      ps97="$(printf '%s\n' "$out" | section PS97)"
      pxc80="$(printf '%s\n' "$out" | section PXC80)"
      pxc84="$(printf '%s\n' "$out" | section PXC84)"
      psql2="$(printf '%s\n' "$out" | section PROXYSQL2)"
      psql3="$(printf '%s\n' "$out" | section PROXYSQL3)"
      mdb60="$(printf '%s\n' "$out" | section PSMDB60)"
      mdb70="$(printf '%s\n' "$out" | section PSMDB70)"
      mdb80="$(printf '%s\n' "$out" | section PSMDB80)"
      pg13="$(printf '%s\n' "$out" | section PPG13)"
      pg14="$(printf '%s\n' "$out" | section PPG14)"
      pg15="$(printf '%s\n' "$out" | section PPG15)"
      pg16="$(printf '%s\n' "$out" | section PPG16)"
      pg17="$(printf '%s\n' "$out" | section PPG17)"
      pg18="$(printf '%s\n' "$out" | section PPG18)"
      vk91="$(printf '%s\n' "$out" | section VALKEY91)"
      orch="$(printf '%s\n' "$out" | section ORCH)"
      md106="$(printf '%s\n' "$out" | section MARIADB106)"
      md1011="$(printf '%s\n' "$out" | section MARIADB1011)"
      md114="$(printf '%s\n' "$out" | section MARIADB114)"
      md118="$(printf '%s\n' "$out" | section MARIADB118)"
      myc80="$(printf '%s\n' "$out" | section MYSQLCE80)"
      myc84="$(printf '%s\n' "$out" | section MYSQLCE84)"
      myc97="$(printf '%s\n' "$out" | section MYSQLCE97)"
    else
      echo "    FAIL  could not run ${tag} (recording empty version lists)" >&2
    fi
  fi

  n80=$(printf '%s' "$ps80" | grep -c . || true)
  n84=$(printf '%s' "$ps84" | grep -c . || true)
  n57=$(printf '%s' "$ps57" | grep -c . || true)
  px0=$(printf '%s' "$pxc80" | grep -c . || true)
  px4=$(printf '%s' "$pxc84" | grep -c . || true)
  pq2=$(printf '%s' "$psql2" | grep -c . || true)
  pq3=$(printf '%s' "$psql3" | grep -c . || true)
  m6=$(printf '%s' "$mdb60" | grep -c . || true)
  m7=$(printf '%s' "$mdb70" | grep -c . || true)
  m8=$(printf '%s' "$mdb80" | grep -c . || true)
  g13=$(printf '%s' "$pg13" | grep -c . || true)
  g14=$(printf '%s' "$pg14" | grep -c . || true)
  g15=$(printf '%s' "$pg15" | grep -c . || true)
  g16=$(printf '%s' "$pg16" | grep -c . || true)
  g17=$(printf '%s' "$pg17" | grep -c . || true)
  g18=$(printf '%s' "$pg18" | grep -c . || true)
  vk9=$(printf '%s' "$vk91" | grep -c . || true)
  orc=$(printf '%s' "$orch" | grep -c . || true)
  d106=$(printf '%s' "$md106" | grep -c . || true)
  d1011=$(printf '%s' "$md1011" | grep -c . || true)
  d114=$(printf '%s' "$md114" | grep -c . || true)
  d118=$(printf '%s' "$md118" | grep -c . || true)
  c80=$(printf '%s' "$myc80" | grep -c . || true)
  c84=$(printf '%s' "$myc84" | grep -c . || true)
  echo "    ps: ${n80}+${n84}+${n57}  pxc: ${px0}+${px4}  proxysql: ${pq2}+${pq3}  psmdb: ${m6}+${m7}+${m8}  ppg: ${g13}+${g14}+${g15}+${g16}+${g17}+${g18}  valkey: ${vk9}  orchestrator: ${orc}" >&2
  echo "    mariadb: ${d106}+${d1011}+${d114}+${d118}  mysql_community: ${c80}+${c84}" >&2

  # emit_series <indent-key> <key1> <list1> [<key2> <list2> ...]: emit a major-series
  # map under `key:` with one or more series (e.g. "8.0"/"8.4", "2"/"3", or the three
  # MongoDB series "6.0"/"7.0"/"8.0").
  emit_series() {
    local key="$1"; shift
    echo "    ${key}:"
    while [ "$#" -ge 2 ]; do
      local k="$1" v="$2"; shift 2
      if [ -n "$v" ]; then
        echo "      \"${k}\":"
        while IFS= read -r vv; do [ -n "$vv" ] && echo "        - ${vv}"; done <<<"$v"
      else
        echo "      \"${k}\": []"
      fi
    done
  }

  # emit_group <group> <key> <series...>: emit a product's map when its probe group
  # ran, otherwise carry the recorded map forward unchanged (see carry_section).
  emit_group() {
    local group="$1" key="$2"; shift 2
    if want_probe "$group"; then
      emit_series "$key" "$@"
    elif ! carry_section "$tag" "$key"; then
      emit_series "$key" "$@"   # nothing recorded yet — emit the empty maps
    fi
  }

  # emit_spock: the source-built Spock catalog (from SPOCK_MAP), recorded only on
  # Oracle Linux images (Spock compiles on the RHEL toolchain only). Non-OEL images
  # get an empty map so the picker offers Spock exclusively on Oracle Linux.
  emit_spock() {
    echo "    spock:"
    case "$os" in
      oraclelinux|rhel|centos|rocky|almalinux)
        while IFS=$'\t' read -r maj mins; do
          [ -n "$maj" ] || continue
          if [ -n "$mins" ]; then
            echo "      \"${maj}\":"
            local IFS=','; local v
            for v in $mins; do echo "        - ${v}"; done
          else
            echo "      \"${maj}\": []"
          fi
        done <<<"$SPOCK_MAP"
        ;;
    esac
  }

  {
    echo "  - os: ${os}"
    echo "    version: \"${version}\""
    echo "    platform: ${platform}"
    echo "    arch: ${arch}"
    echo "    tag: ${tag}"
    echo "    base: ${base}"
    echo "    built_at: ${built}"
    emit_group percona  percona_server         "8.0" "$ps80"  "8.4" "$ps84"  "9.7" "$ps97"  "5.7" "$ps57"
    emit_group percona  percona_xtradb_cluster "8.0" "$pxc80" "8.4" "$pxc84"
    emit_group percona  proxysql               "2"   "$psql2" "3"   "$psql3"
    emit_group percona  percona_server_mongodb "6.0" "$mdb60" "7.0" "$mdb70" "8.0" "$mdb80"
    emit_group percona  percona_postgresql     "13" "$pg13" "14" "$pg14" "15" "$pg15" "16" "$pg16" "17" "$pg17" "18" "$pg18"
    emit_group percona  percona_valkey         "9.1" "$vk91"
    emit_group percona  percona_orchestrator   "3"   "$orch"
    emit_group upstream mariadb                "10.6" "$md106" "10.11" "$md1011" "11.4" "$md114" "11.8" "$md118"
    emit_group upstream mysql_community        "8.0" "$myc80" "8.4" "$myc84" "9.7" "$myc97"
    emit_spock
  } >>"$TMP"
done < <(parse_entries)

if [ "$count" -eq 0 ]; then
  if [ "$skipped" -gt 0 ]; then
    echo "ERROR: no ${PLATFORM} image entries in ${OUT} (skipped ${skipped} on another platform)." >&2
    echo "       Run 'make images' to build them for ${PLATFORM}, or change DOCKER_PLATFORM in .env." >&2
  else
    echo "ERROR: no image entries found in ${OUT}; run 'make images' first." >&2
  fi
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
# The only top-level discovery that talks to the Percona repository, so it honours ONLY
# the same way the per-image product maps do: skipped, the recorded list is reused.
if want_probe percona; then
  echo "==> discovering PDPS repositories from percona-release (${first_tag})" >&2
  pdps_repos="$(pdps_discover "$first_tag")"
else
  pdps_repos="$(awk '/^pdps:/{f=1;next} f && /^  - /{gsub(/^  - "|"$/,""); print; next} f{exit}' "$OUT")"
  echo "==> keeping recorded PDPS repositories (ONLY=${ONLY})" >&2
fi
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

# ---- Percona Kubernetes operator versions (from the operator image registries) ----
echo "==> discovering Percona operator versions from Docker Hub" >&2
{
  echo "# Percona Kubernetes operator versions, discovered from the operator image"
  echo "# registries. A K3D cluster node installs an operator from its matching git tag"
  echo "# (deploy/bundle.yaml + deploy/cr.yaml), so these tags are what the K3D frame's"
  echo "# operator picker offers. OS/arch independent. Re-run: make versions"
  echo "operators:"
} >>"$TMP"
op_total=0
for op in $OPERATOR_PRODUCTS; do
  op_repo="$(operator_repo "$op")"
  op_versions="$(operator_discover "$op")"
  op_n=$(printf '%s' "$op_versions" | grep -c . || true)
  op_latest="$(printf '%s\n' "$op_versions" | head -1)"
  op_total=$((op_total + op_n))
  echo "    ${op}: ${op_n} version(s)${op_latest:+, latest ${op_latest}}" >&2
  {
    echo "  ${op}:"
    echo "    repository: ${op_repo}"
    echo "    latest: \"${op_latest}\""
    if [ -n "$op_versions" ]; then
      echo "    versions:"
      while IFS= read -r v; do [ -n "$v" ] && echo "      - \"${v}\""; done <<<"$op_versions"
    else
      echo "    versions: []"
    fi
  } >>"$TMP"
done

# ---- Helm chart versions (from each chart repo's index.yaml) ----
echo "==> discovering Helm chart versions from the chart repositories" >&2
{
  echo "# Helm chart versions, discovered from each chart repository's index.yaml. A K3D"
  echo "# cluster installs these through k3s' bundled helm-controller, so a HelmChart object"
  echo "# pins the *chart* version — which is not the version of the operator it ships (the"
  echo "# cloudnative-pg chart 0.29.0 carries operator 1.30.x). Re-run: make versions"
  echo "charts:"
} >>"$TMP"
chart_total=0
for ch in $CHART_PRODUCTS; do
  ch_url="$(chart_repo_url "$ch")"
  ch_versions="$(chart_versions "$ch_url" "$ch")"
  ch_n=$(printf '%s' "$ch_versions" | grep -c . || true)
  ch_latest="$(printf '%s\n' "$ch_versions" | head -1)"
  chart_total=$((chart_total + ch_n))
  echo "    ${ch}: ${ch_n} version(s)${ch_latest:+, latest ${ch_latest}}" >&2
  {
    echo "  ${ch}:"
    echo "    repository: ${ch_url}"
    echo "    latest: \"${ch_latest}\""
    if [ -n "$ch_versions" ]; then
      echo "    versions:"
      while IFS= read -r v; do [ -n "$v" ] && echo "      - \"${v}\""; done <<<"$ch_versions"
    else
      echo "    versions: []"
    fi
  } >>"$TMP"
done

# PGO's chart is an OCI artifact in Crunchy's own registry, so it is discovered differently
# (pgo_chart_discover) but belongs in the same section: it is still a chart version a HelmChart
# object pins. `repository:` carries the oci:// reference the HelmChart uses as its `chart:`,
# rather than a repo URL — there is no separate chart name to add to it.
echo "==> discovering PGO chart versions from ${CRUNCHY_REGISTRY}" >&2
pgo_versions="$(pgo_chart_discover)"
pgo_n=$(printf '%s' "$pgo_versions" | grep -c . || true)
pgo_latest="$(printf '%s\n' "$pgo_versions" | head -1)"
chart_total=$((chart_total + pgo_n))
echo "    pgo: ${pgo_n} version(s)${pgo_latest:+, latest ${pgo_latest}}" >&2
{
  echo "  pgo:"
  echo "    repository: oci://${CRUNCHY_REGISTRY}/${PGO_CHART_REPO}"
  echo "    latest: \"${pgo_latest}\""
  if [ -n "$pgo_versions" ]; then
    echo "    versions:"
    while IFS= read -r v; do [ -n "$v" ] && echo "      - \"${v}\""; done <<<"$pgo_versions"
  else
    echo "    versions: []"
  fi
} >>"$TMP"

# ---- images a chart-installed operator is pointed at (ghcr.io, not Docker Hub) ----
echo "==> discovering CloudNativePG PostgreSQL image majors from ghcr.io" >&2
cnpg_pg_versions="$(cnpg_pg_discover)"
cnpg_pg_n=$(printf '%s' "$cnpg_pg_versions" | grep -c . || true)
cnpg_pg_latest="$(printf '%s\n' "$cnpg_pg_versions" | head -1)"
echo "    cnpg-postgresql: ${cnpg_pg_n} major(s)${cnpg_pg_latest:+, latest ${cnpg_pg_latest}}" >&2
# The PostgreSQL majors PGO can run, read from the newest chart's own values.yaml. Its
# registry publishes no usable tags/list for the crunchy-postgres image itself, so the chart
# is the only place the supported set is stated — and it is the set spec.postgresVersion takes.
crunchy_pg_versions="$(crunchy_pg_discover "$pgo_latest")"
crunchy_pg_n=$(printf '%s' "$crunchy_pg_versions" | grep -c . || true)
crunchy_pg_latest="$(printf '%s\n' "$crunchy_pg_versions" | head -1)"
echo "    crunchy-postgres: ${crunchy_pg_n} major(s)${crunchy_pg_latest:+, latest ${crunchy_pg_latest}}" >&2
{
  echo "# Container images a chart-installed operator is pointed at, as opposed to the chart"
  echo "# itself. A CloudNativePG Cluster picks its PostgreSQL with spec.imageName; only the"
  echo "# bare major tags are listed, which is what imageName takes (the registry also carries"
  echo "# ~10k minor/patch/distro variants). Re-run: make versions"
  echo "chart_images:"
  echo "  cnpg-postgresql:"
  echo "    repository: ${CNPG_PG_IMAGE}"
  echo "    latest: \"${cnpg_pg_latest}\""
  if [ -n "$cnpg_pg_versions" ]; then
    echo "    versions:"
    while IFS= read -r v; do [ -n "$v" ] && echo "      - \"${v}\""; done <<<"$cnpg_pg_versions"
  else
    echo "    versions: []"
  fi
  echo "  crunchy-postgres:"
  echo "    repository: ${CRUNCHY_REGISTRY}/crunchydata/crunchy-postgres"
  echo "    latest: \"${crunchy_pg_latest}\""
  if [ -n "$crunchy_pg_versions" ]; then
    echo "    versions:"
    while IFS= read -r v; do [ -n "$v" ] && echo "      - \"${v}\""; done <<<"$crunchy_pg_versions"
  else
    echo "    versions: []"
  fi
} >>"$TMP"

# ---- k3s versions (the Kubernetes a K3D cluster runs) ----
echo "==> discovering k3s versions from Docker Hub" >&2
k3s_versions="$(k3s_discover)"
k3s_n=$(printf '%s' "$k3s_versions" | grep -c . || true)
k3s_latest="$(printf '%s\n' "$k3s_versions" | head -1)"
echo "    k3s: ${k3s_n} version(s)${k3s_latest:+, latest ${k3s_latest}}" >&2
{
  echo "# k3s image tags — the Kubernetes version a K3D cluster frame runs. The frame's"
  echo "# picker offers these; \"latest\" means the first entry. Re-run: make versions"
  echo "k3s:"
  echo "  repository: ${K3S_REPO}"
  echo "  latest: \"${k3s_latest}\""
  if [ -n "$k3s_versions" ]; then
    echo "  versions:"
    while IFS= read -r v; do [ -n "$v" ] && echo "    - \"${v}\""; done <<<"$k3s_versions"
  else
    echo "  versions: []"
  fi
} >>"$TMP"

mv "$TMP" "$OUT"
trap - EXIT

echo "" >&2
echo "==================================================================" >&2
echo "Probed ${count} ${PLATFORM} image(s) + ${pmm_n} PMM3 version(s) + ${op_total} operator version(s) + ${chart_total} chart version(s) + ${cnpg_pg_n} PostgreSQL major(s) + ${k3s_n} k3s version(s) → ${OUT}" >&2
if [ "$skipped" -gt 0 ]; then
  echo "Skipped ${skipped} image(s) not on ${PLATFORM}" >&2
fi
echo "==================================================================" >&2
