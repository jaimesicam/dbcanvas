#!/usr/bin/env bash
# scripts/exposure-check.sh — refuse to start DBCanvas reachable beyond this machine unless that was
# asked for twice: APP_HOST in .env, and APP_HOST_OVERRIDE=on beside it. The app makes the same
# check at startup (app/exposure.go); this one runs first, so `make compose` and `make up` stop
# with the reason instead of starting a container that exits and restarts.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
envval() { grep -E "^$1=" "$ROOT/.env" 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '"'"'"' ' || true; }

host="${APP_HOST:-$(envval APP_HOST)}"; host="${host:-127.0.0.1}"
override="${APP_HOST_OVERRIDE:-$(envval APP_HOST_OVERRIDE)}"
case "$(printf '%s' "${host#[}" | sed 's/]$//')" in
  127.*|localhost|::1) exit 0 ;;
esac
case "$(printf '%s' "${override:-off}" | tr 'A-Z' 'a-z')" in
  on|true|yes|1) echo "  APP_HOST=$host with APP_HOST_OVERRIDE=on: DBCanvas is reachable beyond this machine (docs/SECURITY.md)."; exit 0 ;;
esac
cat >&2 <<MSG

  Refusing to start: APP_HOST is $host, which makes DBCanvas reachable beyond this machine,
  and APP_HOST_OVERRIDE is off. Anyone who can reach the sign-in page and has an account can
  deploy privileged containers on this Docker host.

  Set APP_HOST=127.0.0.1 in .env (colleagues can use an SSH tunnel), or set
  APP_HOST_OVERRIDE=on in .env to expose it deliberately. See docs/SECURITY.md.

MSG
exit 1
