#!/usr/bin/env bash
#
# Build the pre-baked service images — the two node types whose software never varies:
#
#   dbcanvas-intranet:oraclelinux-9-<arch>   (images/intranet.Dockerfile)
#   dbcanvas-vnc:ubuntu-24.04-<arch>         (images/vnc.Dockerfile)
#
# Each is the matching systemd base from `make images` with that node's packages already
# installed, so deploying the node is configuration only: the Intranet's nine remaining
# steps take about four seconds and the VNC node's three about two, where installing the
# packages at deploy time took 56 s and 120 s. Nothing stack-specific is baked in — the
# CA, LDAP credentials, mail domain, DNS zones, desktop user and VNC password are all
# still written at deploy (see app/intranet.go and app/vnc.go).
#
# Usage: service.sh [intranet|vnc|all]     (default: all)
#
# Called at the end of `make images`, and on its own by `make intranet-image` /
# `make vnc-image`. The base pins below must match intranetImage() in app/intranet.go
# and vncImage() in app/vnc.go — those are what a deployed node asks Docker for.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGES_DIR="$ROOT/images"

# Which base each service image is built from. The Intranet is Oracle Linux 9 whatever
# the canvas says (its bind/squid/roundcube config is written for it), and the VNC
# desktop is Ubuntu 24.04 for the same reason: one image, not one per version the OS
# picker happens to offer.
INTRANET_BASE_OS="oraclelinux"; INTRANET_BASE_VER="9"
VNC_BASE_OS="ubuntu";          VNC_BASE_VER="24.04"

WANT="${1:-all}"
case "$WANT" in
  intranet|vnc|all) ;;
  *) echo "usage: $(basename "$0") [intranet|vnc|all]" >&2; exit 2 ;;
esac

# shellcheck source=platform.sh
. "$IMAGES_DIR/platform.sh"
PLATFORM="$(resolve_platform "$ROOT")" || exit 1
ARCH="${PLATFORM#linux/}"

declare -a BUILT=() SKIPPED=() FAILED=()

# build_service <name> <dockerfile> <base_os> <base_version> <tag>
build_service() {
  local name="$1" dockerfile="$2" base_os="$3" base_ver="$4" tag="$5"
  local base="dbcanvas-systemd:${base_os}-${base_ver}-${ARCH}"

  echo "=================================================================="
  echo "==> building ${tag}  (base=${base}, platform=${PLATFORM})"
  echo "=================================================================="
  # The base is what `make images` builds; without it there is nothing to bake onto.
  if ! docker image inspect "$base" >/dev/null 2>&1; then
    echo "    SKIP  ${tag}  — base image ${base} not found (run 'make images' first)"
    SKIPPED+=("${tag} (no ${base})")
    return
  fi
  if docker build \
      --platform "$PLATFORM" \
      --build-arg "BASE_IMAGE=${base}" \
      -f "$IMAGES_DIR/$dockerfile" \
      -t "$tag" \
      "$IMAGES_DIR"; then
    echo "    OK    ${tag}"
    BUILT+=("$tag")
  else
    echo "    FAIL  ${tag}"
    FAILED+=("$tag")
  fi
}

if [ "$WANT" = "intranet" ] || [ "$WANT" = "all" ]; then
  build_service intranet intranet.Dockerfile "$INTRANET_BASE_OS" "$INTRANET_BASE_VER" \
    "dbcanvas-intranet:${INTRANET_BASE_OS}-${INTRANET_BASE_VER}-${ARCH}"
fi
if [ "$WANT" = "vnc" ] || [ "$WANT" = "all" ]; then
  build_service vnc vnc.Dockerfile "$VNC_BASE_OS" "$VNC_BASE_VER" \
    "dbcanvas-vnc:${VNC_BASE_OS}-${VNC_BASE_VER}-${ARCH}"
fi

echo ""
echo "=================================================================="
echo "Service images: ${#BUILT[@]} built, ${#SKIPPED[@]} skipped, ${#FAILED[@]} failed"
for b in "${BUILT[@]}";   do echo "  OK    $b"; done
for s in "${SKIPPED[@]}"; do echo "  SKIP  $s"; done
for f in "${FAILED[@]}";  do echo "  FAIL  $f"; done
echo "=================================================================="

# A failed service image is a real failure: the node that needs it cannot deploy.
[ "${#FAILED[@]}" -eq 0 ]
