#!/usr/bin/env bash
#
# Build the demo application images — the first-party simulators a stack can run
# alongside its databases:
#
#   dbcanvas-trafficsim:latest     Valkey Traffic Lab
#   dbcanvas-hotelsim:latest       MongoDB Hotel Reservation Lab
#   dbcanvas-airlinesim:latest     MySQL Airline Reservation Lab
#   dbcanvas-carsim:latest         PostgreSQL Car Rental Lab
#   dbcanvas-marketchaos:latest    "Unoptimized MySQL Challenge" stock exchange
#   dbcanvas-stocksim:latest       Stock Market Sim (CRUD + reports)
#
# Each is a Go binary with its frontend embedded — no systemd, no OS matrix, one tag
# apiece — which is why they are not in versions.yaml and not built per OS. A node of
# one of these types refuses to deploy without its image (see app/trafficsim.go and
# friends), so they are built by `make images` along with everything else a node needs.
#
# Usage: apps.sh [name ...]        (default: all of them)
#
# Called at the end of images/build.sh, and on its own by `make trafficsim-image` and
# the other per-app targets. The tags below must match the *Image constants in the app.
set -uo pipefail

IMAGES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$IMAGES_DIR/.." && pwd)"

# shellcheck source=platform.sh
. "$IMAGES_DIR/platform.sh"
PLATFORM="$(resolve_platform "$ROOT")" || exit 1

# name → build context, in the order they are built.
APPS=(trafficsim hotelsim airlinesim carsim marketchaos stocksim)

declare -a BUILT=() FAILED=()

build_app() {
  local name="$1"
  local ctx="$ROOT/$name"
  local tag="dbcanvas-${name}:latest"

  echo "=================================================================="
  echo "==> building ${tag}  (platform=${PLATFORM})"
  echo "=================================================================="
  if [ ! -d "$ctx" ]; then
    echo "    FAIL  ${tag}  — no such directory: ${ctx}"
    FAILED+=("${tag} (missing ${name}/)")
    return
  fi
  # --platform, unlike the plain `docker build` these used to get: an installation
  # targets one platform, and a simulator built natively on a host whose stacks are
  # emulated would be the one container in the stack of a different architecture.
  if docker build --platform "$PLATFORM" -t "$tag" "$ctx"; then
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
