#!/usr/bin/env bash
# scripts/release.sh — the release bookkeeping that is not building.
#
#   release.sh notes   <version>   print that release's notes (README "### <version>" section)
#   release.sh tag     <version>   create the annotated tag v<version> on HEAD
#   release.sh publish <version>   push the tag and create the GitHub release from the notes
#
# `tag` refuses a dirty tree, an existing tag, and a VERSION file that disagrees, so a tag
# always names the commit the release was built from. `publish` is the only one that
# reaches the network: it pushes the tag to origin and runs `gh release create`.
set -euo pipefail
cd "$(dirname "$0")/.."

cmd="${1:-}"; ver="${2:-}"
[ -n "$cmd" ] && [ -n "$ver" ] || { echo "usage: $0 notes|tag|publish <version>" >&2; exit 2; }
ver="${ver#v}"

notes() {
  awk -v v="### $ver" '$0==v {f=1; next} f && /^### / {exit} f && /^## / {exit} f' README.md
}

case "$cmd" in
  notes)
    out="$(notes)"
    [ -n "$out" ] || { echo "README.md has no \"### $ver\" section under What's new" >&2; exit 1; }
    printf '%s\n' "$out"
    ;;
  tag)
    [ "$(cat VERSION)" = "$ver" ] || { echo "VERSION is $(cat VERSION), not $ver" >&2; exit 1; }
    [ -z "$(git status --porcelain)" ] || { echo "the working tree has uncommitted changes" >&2; exit 1; }
    if git rev-parse -q --verify "refs/tags/v$ver" >/dev/null; then
      echo "tag v$ver already exists (on $(git rev-list -n1 --abbrev-commit "v$ver"))" >&2; exit 1
    fi
    git tag -a "v$ver" -m "DBCanvas $ver"
    echo "tagged v$ver on $(git rev-parse --short HEAD) — 'make github-release' publishes it"
    ;;
  publish)
    git rev-parse -q --verify "refs/tags/v$ver" >/dev/null || { echo "no tag v$ver — run 'make tag' first" >&2; exit 1; }
    command -v gh >/dev/null || { echo "the GitHub CLI (gh) is not installed" >&2; exit 1; }
    f="$(mktemp)"; trap 'rm -f "$f"' EXIT
    notes >"$f"
    [ -s "$f" ] || { echo "README.md has no notes for $ver" >&2; exit 1; }
    git push origin "v$ver"
    gh release create "v$ver" --title "DBCanvas $ver" --notes-file "$f" --verify-tag
    ;;
  *) echo "usage: $0 notes|tag|publish <version>" >&2; exit 2 ;;
esac
