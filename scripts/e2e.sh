#!/usr/bin/env bash
# e2e.sh — live end-to-end tests: deploy real stacks with a dbcanvas binary, run checks on them,
# and clean up. `make e2e MATRIX=keyring` builds dist/dbcanvas and runs scripts/e2e/keyring.sh.
#
# Everything runs in an instance of its own: a throwaway HOME (so ~/.dbcanvas is a fresh one),
# DBCANVAS_INSTANCE=dbcanvas-e2e (its own container and network names), on the Docker the shell
# already uses. The DBCanvas you use day to day is not touched, restarted or rebuilt. Every stack
# a matrix creates is removed at the end, pass or fail, unless E2E_KEEP=1.
#
# A matrix is a bash file that calls the functions below:
#
#   e2e_up STACK --node SPEC ...        deploy and wait (dbcanvas up … --wait)
#   e2e_refused STACK MESSAGE --node …  the design must be refused, with MESSAGE in the output
#   e2e_sql STACK NODE "SQL"            run SQL as root on a MySQL-family node; prints the rows
#   e2e_exec STACK NODE "SHELL"         run a shell command on a node
#   e2e_api METHOD PATH [JSON]          call the API with the run's token; prints the body
#   e2e_expect "WHAT" ACTUAL WANTED     record a check: ACTUAL must equal WANTED
#   e2e_contains "WHAT" TEXT NEEDLE     record a check: TEXT must contain NEEDLE
#   e2e_stack_id STACK                  the stack's numeric id, for API paths
#
# It ends with a summary and exits non-zero if any check failed.
#
# Env: MATRIX (name, or path to a matrix file), DBCANVAS (binary, default dist/dbcanvas),
# E2E_KEEP=1 (leave the stacks and the instance for a look), E2E_TIMEOUT (per deploy, 50m).
set -uo pipefail

# Bash 4 or later: macOS's /bin/bash is 3.2, which mangles quotes nested inside "$(…)" and
# brace-expands a JSON body there. Re-run under a newer bash on PATH (Homebrew's) if there is one.
if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  for b in $(type -ap bash) /opt/homebrew/bin/bash /usr/local/bin/bash; do
    if [ -x "$b" ] && [ "$("$b" -c 'echo "${BASH_VERSINFO[0]}"')" -ge 4 ]; then exec "$b" "$0" "$@"; fi
  done
  echo "e2e.sh needs bash 4 or later (this is $BASH_VERSION); on macOS: brew install bash"; exit 2
fi

ROOT=$(cd "$(dirname "$0")/.." && pwd)
MATRIX=${MATRIX:-}
[ -n "$MATRIX" ] || { echo "MATRIX is not set; one of: $(cd "$ROOT/scripts/e2e" && ls ./*.sh | sed 's|^\./||; s|\.sh$||' | tr '\n' ' ')"; exit 2; }
case "$MATRIX" in
  */*) MFILE=$MATRIX ;;
  *) MFILE=$ROOT/scripts/e2e/$MATRIX.sh ;;
esac
[ -f "$MFILE" ] || { echo "no matrix $MFILE"; exit 2; }
BIN=${DBCANVAS:-$ROOT/dist/dbcanvas}
[ -x "$BIN" ] || { echo "no dbcanvas binary at $BIN — make dbcanvas first"; exit 2; }
E2E_TIMEOUT=${E2E_TIMEOUT:-50m}

# The Docker the shell uses, for the instance's config.
SOCK=""
if [ -n "${DOCKER_HOST:-}" ]; then
  SOCK=${DOCKER_HOST#unix://}
else
  SOCK=$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null | sed 's|^unix://||')
fi
[ -S "$SOCK" ] || SOCK=/var/run/docker.sock

E2E_HOME=$(mktemp -d "${TMPDIR:-/tmp}/dbcanvas-e2e.XXXXXX")
export HOME=$E2E_HOME DBCANVAS_INSTANCE=dbcanvas-e2e
mkdir -p "$HOME/.dbcanvas"
printf 'instance: dbcanvas-e2e\ndockerSocket: %s\n' "$SOCK" > "$HOME/.dbcanvas/config.yaml"

PASS=0
FAIL=0
FAILED=()
STACKS=()

log() { printf '\033[1m[e2e]\033[0m %s\n' "$*"; }

cleanup() {
  if [ "${E2E_KEEP:-0}" = 1 ]; then
    log "E2E_KEEP=1: left ${STACKS[*]:-no stacks} running; HOME=$E2E_HOME DBCANVAS_INSTANCE=dbcanvas-e2e $BIN …"
    return
  fi
  for s in ${STACKS[@]+"${STACKS[@]}"}; do "$BIN" rm "$s" >/dev/null 2>&1 || true; done
  "$BIN" stop >/dev/null 2>&1 || true
  rm -rf "$E2E_HOME"
}
trap cleanup EXIT

e2e_up() {
  local name=$1; shift
  STACKS+=("$name")
  log "deploying $name"
  if ! "$BIN" up "$name" "$@" --ttl 8h --wait --timeout "$E2E_TIMEOUT" > "$E2E_HOME/$name.up.log" 2>&1; then
    tail -15 "$E2E_HOME/$name.up.log"
    e2e_expect "deploy $name" failed ok
    return 1
  fi
  e2e_expect "deploy $name" ok ok
}

e2e_refused() {
  local name=$1 msg=$2; shift 2
  STACKS+=("$name")
  local out
  out=$("$BIN" up "$name" "$@" --ttl 2h --wait --timeout 5m 2>&1)
  e2e_contains "design $name is refused" "$out" "$msg"
  "$BIN" rm "$name" >/dev/null 2>&1 || true
}

e2e_sql() { "$BIN" exec "$1" "$2" -- mysql --defaults-extra-file=/root/.my.cnf -N -B -e "$3" 2>&1; }
e2e_exec() { "$BIN" exec "$1" "$2" -- bash -c "$3" 2>&1; }

e2e_stack_id() { "$BIN" ls 2>/dev/null | awk -v n="$1" '$2 == n { print $1 }'; }

TOKEN=""
BASE=""
e2e_api() {
  if [ -z "$TOKEN" ]; then
    TOKEN=$("$BIN" token create e2e 2>/dev/null | grep -o 'dbc_[A-Za-z0-9_-]*' | head -1)
  fi
  BASE=$("$BIN" status 2>/dev/null | sed -n 's/.* at \(http[^ ]*\)\/$/\1/p' | head -1)
  local m=$1 p=$2 body=${3:-}
  if [ -n "$body" ]; then
    curl -s -m 3600 -X "$m" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --data "$body" "$BASE$p"
  else
    curl -s -m 3600 -X "$m" -H "Authorization: Bearer $TOKEN" "$BASE$p"
  fi
}

e2e_expect() {
  if [ "$2" = "$3" ]; then
    PASS=$((PASS + 1)); printf '  \033[32mok\033[0m    %s\n' "$1"
  else
    FAIL=$((FAIL + 1)); FAILED+=("$1"); printf '  \033[31mFAIL\033[0m  %s: got %q, want %q\n' "$1" "$2" "$3"
  fi
}

e2e_contains() {
  if [[ "$2" == *"$3"* ]]; then
    PASS=$((PASS + 1)); printf '  \033[32mok\033[0m    %s\n' "$1"
  else
    FAIL=$((FAIL + 1)); FAILED+=("$1"); printf '  \033[31mFAIL\033[0m  %s: %q not in output:\n%s\n' "$1" "$3" "$(printf '%s' "$2" | tail -5)"
  fi
}

log "matrix $(basename "$MFILE" .sh) with $BIN on $SOCK (HOME=$E2E_HOME)"
# shellcheck source=/dev/null
. "$MFILE"

echo
log "$PASS passed, $FAIL failed"
for f in ${FAILED[@]+"${FAILED[@]}"}; do echo "  - $f"; done
[ "$FAIL" -eq 0 ]
