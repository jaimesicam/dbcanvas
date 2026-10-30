#!/usr/bin/env bash
#
# Build the demo application images — the first-party simulators a stack can run
# alongside its databases:
#
#   dbcanvas-supportsim:latest     MongoDB Vector Search Support Desk
#   dbcanvas-stocksim:latest       Stock Market Sim (CRUD + reports)
#   dbcanvas-ledgersim:latest      Ledger Sim (JDBC + HikariCP)
#   dbcanvas-perfsim:latest        Performance Sim (stressors and their impact)
#
# Each is a single-binary service with its frontend embedded — no systemd, no OS
# matrix, one tag apiece — which is why they are not in versions.yaml and not built
# per OS. Ledger Sim is the one that is not a Go binary: it is a shaded jar on an
# Eclipse Temurin JRE, because JDBC and HikariCP are the whole point of it. Its
# build is therefore slower and pulls from Maven Central rather than the Go proxy. A node of
# one of these types refuses to deploy without its image (see app/stocksim.go and
# friends), so they are built by `make images` along with everything else a node needs.
#
# Usage: apps.sh [name ...]        (default: all of them)
#
# Called at the end of images/build.sh, and on its own by `make stocksim-image` and
# the other per-app targets. The tags below must match the *Image constants in the app.
set -uo pipefail

IMAGES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$IMAGES_DIR/.." && pwd)"

# shellcheck source=platform.sh
. "$IMAGES_DIR/platform.sh"
PLATFORM="$(resolve_platform "$ROOT")" || exit 1

# name → build context, in the order they are built.
APPS=(supportsim stocksim ledgersim perfsim)

declare -a BUILT=() FAILED=()

build_app() {
  local name="$1"
  local ctx="$ROOT/$name"
  local tag="dbcanvas-${name}:$(image_release "$ROOT")"

  echo "=================================================================="
  echo "==> building ${tag}  (platform=${PLATFORM})"
  echo "=================================================================="
  if [ ! -d "$ctx" ]; then
    echo "    FAIL  ${tag}  — no such directory: ${ctx}"
    FAILED+=("${tag} (missing ${name}/)")
    return
  fi
  # The image conveys the simulator as a binary, so it carries the GPL text and the
  # licences of the Go modules linked into it (GPLv3 s.4/6; see THIRD_PARTY_NOTICES.md).
  # Each simulator builds from its own directory, which cannot reach the repository root,
  # and plain `docker build` has no named build contexts without buildx — so the two
  # files are staged into the context for the build and removed after it.
  local stage="$ctx/.dbcanvas-build"
  rm -rf "$stage" && mkdir -p "$stage"
  cp "$ROOT/LICENSE" "$ROOT/app/scripts/go-third-party-licenses.sh" "$stage/"
  # --platform, unlike the plain `docker build` these used to get: an installation
  # targets one platform, and a simulator built natively on a host whose stacks are
  # emulated would be the one container in the stack of a different architecture.
  local ok=0
  docker build --platform "$PLATFORM" -t "$tag" "$ctx" && ok=1
  rm -rf "$stage"
  if [ "$ok" = 1 ]; then
    BUILT+=("$tag")
  else
    echo "    FAIL  ${tag}"
    FAILED+=("$tag")
  fi
}

wanted=("$@")
[ "${#wanted[@]}" -eq 0 ] && wanted=("${APPS[@]}")

for w in "${wanted[@]}"; do
  case " ${APPS[*]} " in
    *" $w "*) build_app "$w" ;;
    *)
      echo "usage: $(basename "$0") [${APPS[*]}]" >&2
      exit 2
      ;;
  esac
done

echo ""
echo "=================================================================="
echo "Built ${#BUILT[@]} application image(s)"
for b in "${BUILT[@]}"; do echo "  - ${b}"; done
if [ "${#FAILED[@]}" -gt 0 ]; then
  echo "Failed ${#FAILED[@]}:"
  for f in "${FAILED[@]}"; do echo "  - ${f}"; done
  exit 1
fi
echo "=================================================================="
