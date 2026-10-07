#!/usr/bin/env bash
#
# adopt.sh — tag the images this host already has with the current release, instead of
# rebuilding them (`make adopt-images`).
#
# Every image DBCanvas builds is tagged with the release it was built for (image_release in
# platform.sh), so installations on one Docker daemon cannot replace each other's. After an
# upgrade the installation looks for …-amd64-v0.0.15 and finds only the previous release's
# …-v0.0.14 — or, from before releases were in tags, plain dbcanvas-systemd:oraclelinux-9-amd64
# and dbcanvas-trafficsim:latest — and rebuilding the lot takes a long time. When the image
# definitions did not change between the two releases, this gives each image's newest earlier
# build a second tag for the current release. Nothing is removed or rebuilt, and an image that
# already has the current release's tag is left alone.

set -uo pipefail
IMAGES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$IMAGES_DIR/.." && pwd)"
# shellcheck source=platform.sh
. "$IMAGES_DIR/platform.sh"
RELEASE="$(image_release "$ROOT")"
echo "==> adopting earlier builds for ${RELEASE}"

# Every candidate as "<current-release name>\t<release it has, or 0>\t<ref>", newest release first,
# so the first candidate seen for a name is the one adopted.
candidates() {
  docker images --format '{{.Repository}}:{{.Tag}}' | sort -u | while read -r ref; do
    repo="${ref%%:*}"; tag="${ref#*:}"
    rel=0
    case "$tag" in
      "<none>") continue ;;
      v[0-9]*) rel="${tag#v}"; tag=latest ;;
      *-v[0-9]*) rel="${tag##*-v}"; tag="${tag%-v*}" ;;
    esac
    case "$repo" in
      dbcanvas-systemd|dbcanvas-intranet|dbcanvas-vnc|dbcanvas-k8scollector) new="${repo}:${tag}-${RELEASE}" ;;
      dbcanvas-*sim|dbcanvas-marketchaos) [ "$tag" = latest ] || continue; new="${repo}:${RELEASE}" ;;
      *) continue ;;
    esac
    [ "$ref" = "$new" ] && continue
    printf '%s\t%s\t%s\n' "$new" "$rel" "$ref"
  done | sort -t$'\t' -k1,1 -k2,2Vr
}

n=0
seen=""
while IFS=$'\t' read -r new rel ref; do
  case " $seen " in *" $new "*) continue ;; esac
  seen="$seen $new"
  if docker image inspect "$new" >/dev/null 2>&1; then
    echo "    have  ${new}"
    continue
  fi
  docker tag "$ref" "$new" && echo "    tag   ${ref} → ${new}" && n=$((n + 1))
done < <(candidates)

# images.yaml lists what `make images` built, for `make versions` to probe: point it at the new tags.
if [ -f "$ROOT/images.yaml" ]; then
  sed -i.bak -E "s/^(    tag: dbcanvas-systemd:[a-z0-9.]+-[0-9.]+-(amd64|arm64))(-v[0-9][0-9a-z.]*)?$/\\1-${RELEASE}/" "$ROOT/images.yaml" && rm -f "$ROOT/images.yaml.bak"
fi
echo "==> ${n} image(s) tagged"
