#!/usr/bin/env bash
#
# adopt.sh — tag the images this host already has with the current release, instead of
# rebuilding them (`make adopt-images`).
#
# Every image DBCanvas builds is tagged with the release it was built for (image_release in
# platform.sh), so installations on one Docker daemon cannot replace each other's. Images built
# before that carry no release: dbcanvas-systemd:oraclelinux-9-amd64, dbcanvas-trafficsim:latest.
# An upgraded installation looks for …-amd64-v0.0.14 and finds nothing, and rebuilding the lot
# takes a long time. When you know the images you have were built from this checkout — the usual
# case on the first upgrade — this gives each a second tag for the current release. Nothing is
# removed or rebuilt, and an image that already has the release tag is left alone.

set -uo pipefail
IMAGES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$IMAGES_DIR/.." && pwd)"
# shellcheck source=platform.sh
. "$IMAGES_DIR/platform.sh"
RELEASE="$(image_release "$ROOT")"
echo "==> tagging untagged images for ${RELEASE}"

n=0
while read -r ref; do
  repo="${ref%%:*}"; tag="${ref#*:}"
  case "$tag" in *-v[0-9]*|v[0-9]*|"<none>") continue ;; esac
  case "$repo" in
    dbcanvas-systemd|dbcanvas-intranet|dbcanvas-vnc|dbcanvas-k8scollector) new="${repo}:${tag}-${RELEASE}" ;;
    dbcanvas-*sim|dbcanvas-marketchaos) [ "$tag" = latest ] || continue; new="${repo}:${RELEASE}" ;;
    *) continue ;;
  esac
  if docker image inspect "$new" >/dev/null 2>&1; then
    echo "    have  ${new}"
    continue
  fi
  docker tag "$ref" "$new" && echo "    tag   ${ref} → ${new}" && n=$((n + 1))
done < <(docker images --format '{{.Repository}}:{{.Tag}}' | sort -u)

# images.yaml lists what `make images` built, for `make versions` to probe: point it at the new tags.
if [ -f "$ROOT/images.yaml" ]; then
  sed -i.bak -E "s/^(    tag: dbcanvas-systemd:[a-z0-9.]+-[0-9.]+-(amd64|arm64))$/\\1-${RELEASE}/" "$ROOT/images.yaml" && rm -f "$ROOT/images.yaml.bak"
fi
echo "==> ${n} image(s) tagged"
