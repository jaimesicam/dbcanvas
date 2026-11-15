#!/usr/bin/env bash
# scripts/registry.sh — the node images through a container registry, instead of building them.
#
#   registry.sh push   tag this release's images with the registry's name and push them
#   registry.sh pull   pull them, give them the local names DBCanvas deploys, write images.yaml
#
# The images are every one `make images` builds: the systemd bases (for DOCKER_PLATFORM), the
# images baked on them (Intranet, VNC desktop, K3D collector, Kubeadm node) and the demo
# applications (images/apps.sh), each tagged with the release
# (-v<VERSION>) as they are locally. In the registry they are <IMAGE_REGISTRY>/<local name>:
#
#   ghcr.io/jaimesicam/dbcanvas-systemd:oraclelinux-9-amd64-v0.0.28
#
# A run is one architecture of every image. A release is published for both by running
#
#   DOCKER_PLATFORM=linux/amd64 make images publish-images
#   DOCKER_PLATFORM=linux/arm64 make images publish-images
#
# in either order and from one host or two. A DOCKER_PLATFORM given on the command line also
# selects the Kubeadm node image's architecture for that run (images/platform.sh
# run_k8s_platform); without it, the Kubeadm image is .env's K8S_PLATFORM, the one this
# installation's nodes run, which `pull` takes too. The K3D collector is amd64 only, whatever the
# run. Every image but the demo applications names its architecture in its tag; those are pushed
# as …:v0.0.30-amd64 and …:v0.0.30-arm64 and joined into one multi-architecture …:v0.0.30,
# which is what `pull` asks for with this platform.
#
# `pull` is what `make install-pull` runs: a first install that takes minutes instead of the
# hour `make images` does. `push` publishes, so nothing runs it but `make publish-images`.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

REGISTRY="${IMAGE_REGISTRY:-ghcr.io/jaimesicam}"
REGISTRY="${REGISTRY%/}"

# shellcheck source=../images/platform.sh
. "$ROOT/images/platform.sh"
PLATFORM="$(resolve_platform "$ROOT")" || exit 1
ARCH="${PLATFORM#linux/}"
K8S_PLATFORM="$(run_k8s_platform "$ROOT")" || exit 1
REL="$(image_release "$ROOT")"

# The demo applications, as images/apps.sh has them; a test keeps the two equal.
APPS=(supportsim stocksim ledgersim perfsim pagerpanic)

# The base matrix, as images/build.sh has it (base|os|version); a test keeps the two equal.
MATRIX=(
  "oraclelinux:8|oraclelinux|8"
  "oraclelinux:9|oraclelinux|9"
  "oraclelinux:10|oraclelinux|10"
  "ubuntu:22.04|ubuntu|22.04"
  "ubuntu:24.04|ubuntu|24.04"
  "debian:12|debian|12"
  "debian:13|debian|13"
)

# ONLY, a regular expression, keeps the tags it matches (ONLY=intranet, ONLY='oraclelinux-9').
ONLY="${ONLY:-}"

# images prints "<local tag>|<platform>|<kind>" for every image of this release this run builds:
# DOCKER_PLATFORM's, the Kubeadm node image for K8S_PLATFORM (see run_k8s_platform) and the
# amd64-only collector.
images() {
  all_images "${1:-}" | { if [ -n "$ONLY" ]; then grep -E "^[^|]*(${ONLY})" || true; else cat; fi; }
}

all_images() {
  local mode="${1:-}"
  local e base os ver
  for e in "${MATRIX[@]}"; do
    IFS='|' read -r base os ver <<<"$e"
    echo "dbcanvas-systemd:${os}-${ver}-${ARCH}-${REL}|${PLATFORM}|base|${os}|${ver}|${base}"
  done
  # Must match intranetImage, vncImage, k8sCollectorImage and kubeadmImage in app/.
  echo "dbcanvas-intranet:oraclelinux-9-${ARCH}-${REL}|${PLATFORM}|extra"
  echo "dbcanvas-vnc:ubuntu-24.04-${ARCH}-${REL}|${PLATFORM}|extra"
  echo "dbcanvas-k8scollector:debian-12-amd64-${REL}|linux/amd64|extra"
  echo "dbcanvas-kubeadm:ubuntu-24.04-${K8S_PLATFORM#linux/}-${REL}|${K8S_PLATFORM}|extra"
  # Must match the *SimImage / pagerPanicImage tags in app/; built for DOCKER_PLATFORM like the bases.
  # They are pushed under their architecture tag (…:v0.0.30-arm64) and joined into the plain
  # one afterwards (join_apps); pulled by the plain one, which resolves to this platform.
  local a
  for a in "${APPS[@]}"; do
    if [ "$mode" = push ]; then
      echo "dbcanvas-${a}:${REL}-${ARCH}|${PLATFORM}|app|${a}"
    else
      echo "dbcanvas-${a}:${REL}|${PLATFORM}|app|${a}"
    fi
  done
}

# arch_tag_from_plain gives a demo application built before the architecture tags existed its
# …-<arch> tag, when its plain tag is an image of this platform. Returns 1 otherwise.
arch_tag_from_plain() {
  local tag="$1" plain="${1%-"${ARCH}"}" got
  [ "$plain" != "$tag" ] || return 1
  got="$(docker image inspect -f '{{.Architecture}}' "$plain" 2>/dev/null)" || return 1
  [ "$got" = "$ARCH" ] && docker tag "$plain" "$tag"
}

# join_apps points each demo application's plain release tag at one multi-architecture index
# of every architecture the registry holds for it: after the arm64 push it is arm64 alone,
# after the amd64 push as well, both — so the two publishes can come in either order, from one
# host or two, and a pull with --platform gets its own. The registry does the work; nothing is
# downloaded. Needs docker buildx (imagetools).
join_apps() {
  local a arch src plain joined=0
  if ! docker buildx imagetools --help >/dev/null 2>&1; then
    echo "  WARN  docker buildx is not available: the demo applications were pushed under their" >&2
    echo "        architecture tags only, and their plain tags were not updated" >&2
    return 0
  fi
  for a in "${APPS[@]}"; do
    plain="dbcanvas-${a}:${REL}"
    if [ -n "$ONLY" ] && ! printf '%s\n' "${plain}-${ARCH}" | grep -qE "^[^|]*(${ONLY})"; then
      continue
    fi
    src=()
    for arch in amd64 arm64; do
      if docker buildx imagetools inspect "$REGISTRY/${plain}-${arch}" >/dev/null 2>&1; then
        src+=("$REGISTRY/${plain}-${arch}")
      fi
    done
    [ "${#src[@]}" -gt 0 ] || continue
    if docker buildx imagetools create -t "$REGISTRY/${plain}" "${src[@]}" >/dev/null 2>&1; then
      echo "  join  $REGISTRY/${plain}  ← ${src[*]##*-}"; joined=$((joined + 1))
    else
      echo "  FAIL  joining $REGISTRY/${plain}" >&2; return 1
    fi
  done
  echo "==> joined $joined demo application tag(s)"
}

ts() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

push() {
  local line tag plat out pushed=0 skipped=0
  if [ "$K8S_PLATFORM" != "$PLATFORM" ]; then
    echo "  WARN  this run publishes the ${K8S_PLATFORM#linux/} Kubeadm node image with ${ARCH} everything else" >&2
    echo "        (K8S_PLATFORM in .env). For a release, give the architecture on the command line:" >&2
    echo "        DOCKER_PLATFORM=${PLATFORM} make images publish-images" >&2
  fi
  while IFS= read -r line; do
    IFS='|' read -r tag plat kind _ <<<"$line"
    if ! docker image inspect "$tag" >/dev/null 2>&1 && ! { [ "$kind" = app ] && arch_tag_from_plain "$tag"; }; then
      echo "  skip  $tag (not built here)"; skipped=$((skipped + 1)); continue
    fi
    docker tag "$tag" "$REGISTRY/$tag"
    # --platform pushes the image's own manifest. Without it, Docker's containerd image store
    # pushes an OCI index with attestation entries, whose manifest ghcr.io refuses ("denied")
    # after taking the layers. Docker before 28 has no --platform; it pushes the manifest anyway.
    if docker push --help 2>/dev/null | grep -q -- '--platform'; then
      out="$(docker push -q --platform "$plat" "$REGISTRY/$tag" 2>&1)" || { echo "  FAIL  $REGISTRY/$tag: $out" >&2; return 1; }
    else
      out="$(docker push -q "$REGISTRY/$tag" 2>&1)" || { echo "  FAIL  $REGISTRY/$tag: $out" >&2; return 1; }
    fi
    echo "  push  $REGISTRY/$tag"; pushed=$((pushed + 1))
  done < <(images push)
  echo "==> pushed $pushed image(s) to $REGISTRY, skipped $skipped"
  join_apps
}

# pulled_arch_ok checks that an image pulled for a platform is that platform's: the tag names the
# architecture, but only the image itself says what it is. A tag pushed with the wrong one, or a
# single-architecture image a daemon pulls whatever --platform asked for, would otherwise be
# installed and then run emulated (or, for the Kubeadm node, not at all).
# $1: image, $2: platform it was pulled for
pulled_arch_ok() {
  local got
  got="$(docker image inspect -f '{{.Architecture}}' "$1" 2>/dev/null)" || return 1
  [ "$got" = "${2#linux/}" ] && return 0
  echo "  FAIL  $1 is ${got:-of unknown architecture}, not ${2#linux/}" >&2
  return 1
}

pull() {
  local line tag plat kind os ver base src pulled=0 failed=0 nbase=0
  local tmp; tmp="$(mktemp)"
  # The other architecture's entries stay (see other_platform_entries): an installation that
  # also built or pulled the other platform keeps knowing about those images.
  local other; other="$(other_platform_entries "$ROOT/images.yaml" "$PLATFORM")"
  {
    echo "# Generated by scripts/registry.sh pull (make install-pull). The systemd base images this"
    echo "# installation pulled from $REGISTRY, one entry per OS x version x platform (a pull"
    echo "# replaces only DOCKER_PLATFORM's entries)."
    echo "# Re-run: make pull-images (or build them instead: make images)"
    echo "generated_at: $(ts)"
    echo "image_prefix: dbcanvas-systemd"
    echo "images:"
    [ -z "$other" ] || printf '%s\n' "$other"
  } >"$tmp"
  while IFS= read -r line; do
    IFS='|' read -r tag plat kind os ver base <<<"$line"
    # A demo application is pulled by its architecture tag, so the local name is that one
    # image rather than the multi-architecture index: the app creates their containers without
    # a platform, and with Docker's containerd image store an index would let it pick the
    # host's architecture, whose layers were never pulled. A release published before the
    # architecture tags has only the plain one, which is then pulled instead.
    src="$REGISTRY/$tag"
    if [ "$kind" = app ] && docker pull -q --platform "$plat" "$REGISTRY/${tag}-${ARCH}" >/dev/null 2>&1; then
      src="$REGISTRY/${tag}-${ARCH}"
    fi
    if { [ "$src" != "$REGISTRY/$tag" ] || docker pull -q --platform "$plat" "$src" >/dev/null 2>&1; } &&
      pulled_arch_ok "$src" "$plat" && docker tag "$src" "$tag"; then
      echo "  pull  $tag"; pulled=$((pulled + 1))
      # A demo application also gets the architecture tag a local build would give it.
      [ "$kind" = app ] && docker tag "$src" "${tag}-${ARCH}"
      if [ "$kind" = base ]; then
        nbase=$((nbase + 1))
        printf '  - os: %s\n    version: "%s"\n    platform: %s\n    arch: %s\n    tag: %s\n    base: %s\n    built_at: %s\n' \
          "$os" "$ver" "$plat" "${plat#linux/}" "$tag" "$base" "$(ts)" >>"$tmp"
      fi
    else
      echo "  FAIL  $REGISTRY/$tag"; failed=$((failed + 1))
    fi
  done < <(images)
  # Written only when this platform got a base: a pull that got none leaves images.yaml as it
  # was rather than replacing this platform's entries with nothing.
  if [ "$nbase" -gt 0 ]; then
    mv "$tmp" "$ROOT/images.yaml"
    echo "==> wrote images.yaml"
  else
    rm -f "$tmp"
  fi
  echo "==> pulled $pulled image(s) from $REGISTRY, $failed failed"
  [ "$failed" -eq 0 ] || { echo "    Build what failed with 'make images', or its own target (e.g. 'make vnc-image')." >&2; exit 1; }
}

case "${1:-}" in
  push) push ;;
  pull) pull ;;
  list) images "${2:-}" | cut -d'|' -f1 ;;
  *) echo "usage: $0 push|pull|list [push]   (IMAGE_REGISTRY=${REGISTRY})" >&2; exit 2 ;;
esac
