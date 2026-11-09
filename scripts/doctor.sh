#!/usr/bin/env bash
# scripts/doctor.sh — `make doctor`: the host-side checks, which need no running DBCanvas.
# For the installation's own (images, catalogue, passwords, exposure), run `dbcanvas-cli doctor`.
set -uo pipefail
cd "$(dirname "$0")/.."
fail=0
ok()   { printf 'ok    %-20s %s\n' "$1" "$2"; }
warn() { printf 'WARN  %-20s %s\n' "$1" "$2"; }
bad()  { printf 'FAIL  %-20s %s\n' "$1" "$2"; fail=1; }

if docker info >/dev/null 2>&1; then ok docker "$(docker version --format 'server {{.Server.Version}}' 2>/dev/null)"; else bad docker "the daemon does not answer"; fi
if docker compose version >/dev/null 2>&1; then ok compose "$(docker compose version --short 2>/dev/null)"; else bad compose "docker compose is not installed"; fi
if docker buildx version >/dev/null 2>&1; then ok buildx "present"; else warn buildx "missing: image builds use the legacy builder"; fi
root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
if [ -n "$root" ]; then
  free="$(df -Pk "$root" 2>/dev/null | awk 'NR==2 {print int($4/1024/1024)}')"
  if [ -z "$free" ]; then warn disk "cannot read free space for $root (inside the Docker VM?)"
  elif [ "$free" -lt 30 ]; then warn disk "${free} GiB free for Docker — images and stacks need tens of GiB"
  else ok disk "${free} GiB free for Docker"; fi
fi
if [ -f .env ]; then
  n=0
  while IFS= read -r line; do grep -qxF "$line" .env && n=$((n + 1)); done < <(grep -E '^[A-Z_]*PASSWORD=' .env.example)
  if [ "$n" -gt 0 ]; then warn passwords "$n password(s) in .env still at their .env.example value — bash scripts/gen-secrets.sh .env"; else ok passwords "none at their example value"; fi
  host="$(grep -E '^APP_HOST=' .env | cut -d= -f2)"
  case "${host:-127.0.0.1}" in 127.*|localhost|::1) ok exposure "APP_HOST=${host:-127.0.0.1}" ;; *) warn exposure "APP_HOST=$host: reachable beyond this machine (docs/SECURITY.md)" ;; esac
else
  warn env ".env does not exist yet — make env"
fi
missing=0
for t in $(bash scripts/registry.sh list 2>/dev/null | grep -E 'systemd|intranet'); do docker image inspect "$t" >/dev/null 2>&1 || missing=$((missing + 1)); done
if [ "$missing" -gt 0 ]; then warn images "$missing base/Intranet image(s) missing for this release — make images, or make pull-images"; else ok images "base and Intranet images present"; fi
port="$(grep -E '^APP_PORT=' .env 2>/dev/null | cut -d= -f2)"
if curl -fsS -o /dev/null "http://127.0.0.1:${port:-8080}/api/setup/status" 2>/dev/null; then ok app "answering on port ${port:-8080}"; else warn app "not answering on port ${port:-8080} — make compose"; fi
exit $fail
