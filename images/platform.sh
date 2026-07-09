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
