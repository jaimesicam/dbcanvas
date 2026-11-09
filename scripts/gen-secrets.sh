#!/usr/bin/env bash
# scripts/gen-secrets.sh <env file> — replace every *_PASSWORD that still has .env.example's
# value with a random one. `make env` runs it on the .env it has just created, so a new
# installation never starts with passwords anybody can read in the repository. An existing .env
# is left alone (`dbcanvas-cli doctor` lists any password still at its example value).
#
# Two values have rules of their own: VNC uses at most 8 characters, and the Samba AD password
# must mix upper and lower case and digits.
set -euo pipefail
f="${1:?usage: gen-secrets.sh <env file>}"
ex="$(dirname "$0")/../.env.example"

# tr is cut off by head on purpose; that SIGPIPE is not a failure.
rnd() ( set +o pipefail; LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c "$1" )

tmp="$(mktemp)"
changed=0
while IFS= read -r line || [ -n "$line" ]; do
  if [[ "$line" =~ ^([A-Z_]*PASSWORD)=(.*)$ ]]; then
    key="${BASH_REMATCH[1]}"; val="${BASH_REMATCH[2]}"
    if grep -qxF "$key=$val" "$ex"; then
      case "$key" in
        VNC_PASSWORD)   val="$(rnd 8)" ;;
        SAMBA_PASSWORD) val="Sb$(rnd 16)7" ;;
        *)              val="$(rnd 24)" ;;
      esac
      line="$key=$val"
      changed=$((changed + 1))
    fi
  fi
  printf '%s\n' "$line" >>"$tmp"
done <"$f"
cat "$tmp" >"$f"
rm -f "$tmp"
echo "Generated $changed random password(s) in $f (see .env.example for what each is for)"
