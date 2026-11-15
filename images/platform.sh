#!/usr/bin/env bash
#
# Shared platform resolution for `make images` and `make versions`.
#
# DOCKER_PLATFORM (from the environment, else .env) selects the single Docker
# platform this installation targets. It must be exactly one of:
#
#   linux/amd64
#   linux/arm64
#
# `make images` builds only that platform, and `make versions` only probes (and
# records) image entries on it — so the host never spends time emulating the
# other architecture. Unset/empty defaults to linux/amd64, matching the
# docker-compose.yml fallback for the app image.

# The platform used when DOCKER_PLATFORM is unset or empty.
DOCKER_PLATFORM_DEFAULT="linux/amd64"

# resolve_platform prints the selected platform. Anything other than the two
# supported values — including a comma-separated list — is a hard error: silently
# ignoring it would build the wrong matrix.
# $1: repo root (to locate .env)
resolve_platform() {
  local root="$1" raw=""

  # Environment wins over .env (matches how docker compose resolves variables).
  if [ -n "${DOCKER_PLATFORM:-}" ]; then
    raw="$DOCKER_PLATFORM"
  elif [ -f "$root/.env" ]; then
    # Last assignment wins; strip inline comments, quotes and whitespace.
    raw="$(grep -E '^[[:space:]]*DOCKER_PLATFORM[[:space:]]*=' "$root/.env" \
            | tail -1 | cut -d= -f2- \
            | sed -E 's/[[:space:]]*#.*$//; s/^[[:space:]]*//; s/[[:space:]]*$//; s/^"(.*)"$/\1/; s/^'\''(.*)'\''$/\1/')"
  fi
  # Trim surrounding whitespace (an exported value may carry it too).
  raw="$(printf '%s' "$raw" | sed -E 's/^[[:space:]]*//; s/[[:space:]]*$//')"
  [ -n "$raw" ] || raw="$DOCKER_PLATFORM_DEFAULT"

  case "$raw" in
    linux/amd64|linux/arm64)
      printf '%s\n' "$raw"
      ;;
    *)
      echo "ERROR: DOCKER_PLATFORM must be exactly one of linux/amd64 or linux/arm64 (got '${raw}')" >&2
      return 1
      ;;
  esac
}

# home_platform prints the platform this installation itself runs: DOCKER_PLATFORM from .env
# only, ignoring the environment. They differ when a release is built for both architectures
# from one host — `DOCKER_PLATFORM=linux/amd64 make images` on an arm64 installation — and that
# build must not take over what the installation runs: the plain demo-application tags (see
# images/apps.sh) stay on the home platform's images.
# $1: repo root (to locate .env)
home_platform() {
  (unset DOCKER_PLATFORM; resolve_platform "$1")
}

# other_platform_entries prints the base-image entries of an images.yaml that are NOT for the
# given platform, exactly as they are. Whatever writes images.yaml — images/build.sh (`make
# images`) and scripts/registry.sh (`make pull-images`) — replaces only its own platform's
# entries and keeps these, so building or pulling one architecture never forgets the other's.
# Prints nothing when the file does not exist.
# $1: images.yaml path, $2: platform whose entries are left out
other_platform_entries() {
  [ -f "$1" ] || return 0
  awk -v keep_not="$2" '
    function flush() { if (blk != "" && plat != keep_not) printf "%s", blk; blk = ""; plat = "" }
    /^  - os:/ { flush(); inblk = 1 }
    /^[^ ]/ { flush(); inblk = 0; next }
    inblk {
      blk = blk $0 "\n"
      if ($1 == "platform:") plat = $2
    }
    END { flush() }
  ' "$1"
}

# versions_file prints the version catalog for a platform: versions.yaml for linux/amd64,
# versions-arm64.yaml for linux/arm64. One file per platform, because what a repository offers
# differs by architecture and a probe on one must never overwrite the other's catalog. Must
# match versionsFilePath() in app/versions.go.
# $1: repo root, $2: platform
versions_file() {
  case "$2" in
    linux/arm64) printf '%s\n' "$1/versions-arm64.yaml" ;;
    *) printf '%s\n' "$1/versions.yaml" ;;
  esac
}

# resolve_k8s_platform prints the platform Kubernetes nodes target: K8S_PLATFORM, else its
# earlier name K3D_PLATFORM, else linux/amd64 — from the environment first, then .env, the
# same way resolve_platform reads DOCKER_PLATFORM. It is separate from DOCKER_PLATFORM because
# a Kubernetes node has to run natively (under emulation its containerd cannot create a pod
# sandbox). Must match k8sPlatform() in app/docker.go.
# $1: repo root (to locate .env)
resolve_k8s_platform() {
  local root="$1" raw="" var
  for var in K8S_PLATFORM K3D_PLATFORM; do
    if [ -n "${!var:-}" ]; then
      raw="${!var}"
    elif [ -f "$root/.env" ]; then
      raw="$(grep -E "^[[:space:]]*${var}[[:space:]]*=" "$root/.env" \
              | tail -1 | cut -d= -f2- \
              | sed -E 's/[[:space:]]*#.*$//; s/^[[:space:]]*//; s/[[:space:]]*$//; s/^"(.*)"$/\1/; s/^'\''(.*)'\''$/\1/')"
    fi
    raw="$(printf '%s' "$raw" | sed -E 's/^[[:space:]]*//; s/[[:space:]]*$//')"
    [ -n "$raw" ] && break
  done
  [ -n "$raw" ] || raw="$DOCKER_PLATFORM_DEFAULT"

  case "$raw" in
    linux/amd64|linux/arm64)
      printf '%s\n' "$raw"
      ;;
    *)
      echo "ERROR: K8S_PLATFORM must be exactly one of linux/amd64 or linux/arm64 (got '${raw}')" >&2
      return 1
      ;;
  esac
}

# run_k8s_platform prints the platform this run builds, publishes or pulls the Kubeadm node image
# for. A DOCKER_PLATFORM given on the command line (or exported) selects the architecture of the
# whole run, the Kubeadm image included, unless K8S_PLATFORM (or K3D_PLATFORM) is given there
# too: that is what makes a release one run per architecture —
#
#   DOCKER_PLATFORM=linux/amd64 make images publish-images
#   DOCKER_PLATFORM=linux/arm64 make images publish-images
#
# — with one of every image in each and no extra build. Without either on the command line it is
# resolve_k8s_platform, .env's K8S_PLATFORM: the image this installation's own nodes run.
# $1: repo root (to locate .env)
run_k8s_platform() {
  if [ -z "${K8S_PLATFORM:-}" ] && [ -z "${K3D_PLATFORM:-}" ] && [ -n "${DOCKER_PLATFORM:-}" ]; then
    resolve_platform "$1"
  else
    resolve_k8s_platform "$1"
  fi
}

# image_release prints the release every image DBCanvas builds is tagged with: "v" + the repo's
# VERSION file (v0.0.14). It is what lets several installations share one Docker daemon: one
# checkout's `make images` cannot replace the images another, on a different release, deploys
# from — and installations on the same release share them instead of each building their own.
# Must match imageRelease() in app/instance.go, which reads the same VERSION (stamped into the app).
# $1: repo root
image_release() {
  local v
  v="$(tr -d '[:space:]' <"$1/VERSION" 2>/dev/null)"
  printf 'v%s\n' "${v:-dev}"
}
