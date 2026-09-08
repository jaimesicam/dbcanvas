#!/usr/bin/env bash
#
# Build the pre-baked service images — the two node types whose software never varies:
#
#   dbcanvas-intranet:oraclelinux-9-<arch>   (images/intranet.Dockerfile)
#   dbcanvas-vnc:ubuntu-24.04-<arch>         (images/vnc.Dockerfile)
#   dbcanvas-k8scollector:debian-12-amd64    (images/k8scollector.Dockerfile)
#   dbcanvas-mclusteradmin:<version>         (images/mclusteradmin.Dockerfile)
#   dbcanvas-bighole:<commit>                (images/bighole.Dockerfile)
#
# Each is the matching systemd base from `make images` with that node's packages already
# installed, so deploying the node is configuration only: the Intranet's nine remaining
# steps take about four seconds and the VNC node's three about two, where installing the
# packages at deploy time took 56 s and 120 s. Nothing stack-specific is baked in — the
# CA, LDAP credentials, mail domain, DNS zones, desktop user and VNC password are all
# still written at deploy (see app/intranet.go and app/vnc.go).
#
# The last two are not node images built from a systemd base. The collector is the
# throwaway container a K3D Diagnostics capture runs pt-k8s-debug-collector in, and
# it is pinned to linux/amd64 because Percona's apt repo publishes percona-toolkit
# for that architecture only (see images/k8scollector.Dockerfile). MClusterAdmin is
# a third-party Go binary built from source at a pinned upstream tag — it IS a node
# image, but there is no OS under it: the panel is one static binary on scratch. Big
# Hole is the same idea one step further out: a third-party browser app, built from
# source at a pinned commit and served by nginx, with no backend at all.
#
# Usage: service.sh [intranet|vnc|k8scollector|mclusteradmin|bighole|all]  (default: all)
#
# `make images` calls this with `intranet` once the bases are built — the Intranet is
# the DNS and the CA a stack is built against, so it ships with the bases rather than
# with the optional images. `make extra-images` calls it with `all`, which re-checks
# the Intranet (a cached no-op when it is already there) and builds the rest; the
# per-image make targets call it with one name. The base pins below must match
# intranetImage() in app/intranet.go and vncImage() in app/vnc.go — those are what a
# deployed node asks Docker for.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGES_DIR="$ROOT/images"

# Which base each service image is built from. The Intranet is Oracle Linux 9 whatever
# the canvas says (its bind/squid/roundcube config is written for it), and the VNC
# desktop is Ubuntu 24.04 for the same reason: one image, not one per version the OS
# picker happens to offer.
INTRANET_BASE_OS="oraclelinux"; INTRANET_BASE_VER="9"
VNC_BASE_OS="ubuntu";          VNC_BASE_VER="24.04"

# The collector image carries its own platform and tag: it is not built per-arch
# like the node images, because the package it exists to deliver is amd64-only.
# Must match k8sCollectorImage() in app/k8sdiag.go.
K8SCOLLECTOR_PLATFORM="linux/amd64"
K8SCOLLECTOR_TAG="dbcanvas-k8scollector:debian-12-amd64"

# MClusterAdmin: the upstream git tag to build, and the image tag that carries its
# version. Both must match mcaImage/mcaVersion in app/mclusteradmin.go — the node
# asks Docker for that exact tag, and reports the version from it (there is no
# shell in the image to ask). Bumping the panel means changing all three.
MCA_VERSION="v0.3.7"
MCA_TAG="dbcanvas-mclusteradmin:0.3.7"

# Big Hole: the upstream commit to build, and the image tag that records it. The
# project has no tags and no version in its package.json, so the revision is the
# version. Both must match bigHoleRef/bigHoleImage in app/bighole.go.
BIGHOLE_REF="896984fe9e9a7f1f59cc4ce29237625f8aaa713f"
BIGHOLE_TAG="dbcanvas-bighole:896984f"

WANT="${1:-all}"
case "$WANT" in
  intranet|vnc|k8scollector|mclusteradmin|bighole|all) ;;
  *) echo "usage: $(basename "$0") [intranet|vnc|k8scollector|mclusteradmin|bighole|all]" >&2; exit 2 ;;
esac

# shellcheck source=platform.sh
. "$IMAGES_DIR/platform.sh"
PLATFORM="$(resolve_platform "$ROOT")" || exit 1
ARCH="${PLATFORM#linux/}"

# ---- BuildKit or not -------------------------------------------------------------
#
# mclusteradmin.Dockerfile and bighole.Dockerfile build their sources on the BUILD
# host's architecture and emit output for the target one — `FROM
# --platform=$BUILDPLATFORM` plus $TARGETARCH — so an arm64 installation does not run
# `npm ci` or the Go compiler under emulation. Those two variables are BuildKit's,
# and a Docker install with no buildx plugin (or DOCKER_BUILDKIT=0) has only the
# legacy builder, which sets neither and dies on the very first instruction:
#
#   failed to parse platform : "" is an invalid OS component of ""
#
# So work out which builder `docker build` will use and, when it is the legacy one,
# pass both by hand. The Dockerfiles declare the two ARGs without defaults, which is
# what makes this work either way: a default would override the value BuildKit sets
# and cross-build the wrong way round.
#
# The value passed is the TARGET platform, not the host's — a legacy build cannot do
# the split. Give it a build stage of one architecture and a target of another and it
# gets as far as the runtime stage before refusing to copy between them:
#
#   invalid from flag value build: image with reference sha256:… was found but does
#   not provide the specified platform (linux/amd64)
#
# So on this builder both stages are the target platform: native and free when that
# is the host's architecture (the usual case), emulated and slow when it is not.
# Installing buildx is what buys back the fast cross-build.

# buildkit_build — true when `docker build` here will be a BuildKit build.
# DOCKER_BUILDKIT settles it when set; otherwise it comes down to whether the CLI has
# the buildx plugin it shells out to.
buildkit_build() {
  case "${DOCKER_BUILDKIT:-}" in
    0|false) return 1 ;;
    1|true)  return 0 ;;
  esac
  docker buildx version >/dev/null 2>&1
}

# native_platform — the daemon's own os/arch. Only used to say whether a legacy build
# is about to be emulated; empty if Docker will not say.
native_platform() {
  local os arch
  os="$(docker version --format '{{.Server.Os}}' 2>/dev/null)"
  arch="$(docker version --format '{{.Server.Arch}}' 2>/dev/null)"
  [ -n "$os" ] && [ -n "$arch" ] && printf '%s/%s\n' "$os" "$arch"
}

# Extra "KEY=VALUE" build args for the two cross-compiling Dockerfiles. Empty under
# BuildKit — passing them there would override what it works out for itself, and the
# legacy builder is the only one that needs telling.
declare -a XPLATFORM_ARGS=()
if ! buildkit_build; then
  XPLATFORM_ARGS=("BUILDPLATFORM=${PLATFORM}" "TARGETARCH=${ARCH}")
  echo "==> legacy Docker builder (no buildx): passing BUILDPLATFORM=${PLATFORM} and"
  echo "    TARGETARCH=${ARCH} by hand, which BuildKit would have supplied."
  NATIVE="$(native_platform)"
  if [ -n "$NATIVE" ] && [ "$NATIVE" != "$PLATFORM" ]; then
    echo "    NOTE: this host is ${NATIVE}, so the ${PLATFORM} build stages run under"
    echo "          emulation and will be slow. Installing the buildx plugin lets them"
    echo "          build natively instead: https://docs.docker.com/go/buildx/"
  fi
fi

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

# build_standalone <dockerfile> <tag> <platform> [build-arg ...] — an image with no
# systemd base to bake onto and, in the collector's case, a platform of its own.
build_standalone() {
  local dockerfile="$1" tag="$2" platform="$3"
  shift 3
  local args=()
  for a in "$@"; do args+=(--build-arg "$a"); done
  echo "=================================================================="
  echo "==> building ${tag}  (standalone, platform=${platform})"
  echo "=================================================================="
  if docker build --platform "$platform" "${args[@]}" -f "$IMAGES_DIR/$dockerfile" -t "$tag" "$IMAGES_DIR"; then
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

if [ "$WANT" = "k8scollector" ] || [ "$WANT" = "all" ]; then
  build_standalone k8scollector.Dockerfile "$K8SCOLLECTOR_TAG" "$K8SCOLLECTOR_PLATFORM"
fi

# Built for the installation's own platform, unlike the collector: the panel is
# ordinary Go with no architecture-bound packages, and it runs beside the stack.
if [ "$WANT" = "mclusteradmin" ] || [ "$WANT" = "all" ]; then
  build_standalone mclusteradmin.Dockerfile "$MCA_TAG" "$PLATFORM" "MCA_VERSION=${MCA_VERSION}" \
    ${XPLATFORM_ARGS[@]+"${XPLATFORM_ARGS[@]}"}
fi

if [ "$WANT" = "bighole" ] || [ "$WANT" = "all" ]; then
  build_standalone bighole.Dockerfile "$BIGHOLE_TAG" "$PLATFORM" "BIGHOLE_REF=${BIGHOLE_REF}" \
    ${XPLATFORM_ARGS[@]+"${XPLATFORM_ARGS[@]}"}
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
