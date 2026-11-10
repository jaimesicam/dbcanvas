#!/usr/bin/env bash
# scripts/release.sh — the release bookkeeping that is not building.
#
#   release.sh notes   <version>   print that release's notes (CHANGELOG.md "## <version>" section)
#   release.sh tag     <version>   create the annotated tag v<version> on HEAD
#   release.sh publish <version>   push the tag; the release workflow builds, signs and publishes
#
# `tag` refuses a dirty tree, an existing tag, and a VERSION file that disagrees, so a tag
# always names the commit the release was built from. `publish` is the only one that
# reaches the network: it pushes the tag to origin. That starts .github/workflows/release.yml,
# which builds dbcanvas-cli for every platform, signs the files with Sigstore and creates the
# GitHub release from these notes with them attached.
set -euo pipefail
cd "$(dirname "$0")/.."

cmd="${1:-}"; ver="${2:-}"
[ -n "$cmd" ] && [ -n "$ver" ] || { echo "usage: $0 notes|tag|publish <version>" >&2; exit 2; }
ver="${ver#v}"

notes() {
  awk -v v="## $ver" '$0==v {f=1; next} f && /^## / {exit} f' CHANGELOG.md
}

case "$cmd" in
  notes)
    out="$(notes)"
    [ -n "$out" ] || { echo "CHANGELOG.md has no \"## $ver\" section" >&2; exit 1; }
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
    [ -n "$(notes)" ] || { echo "CHANGELOG.md has no notes for $ver" >&2; exit 1; }
    git push origin "v$ver"
    echo "pushed v$ver: the release workflow now builds, signs and publishes it."
    if command -v gh >/dev/null; then
      echo "follow it with: gh run watch \$(gh run list --workflow release.yml --limit 1 --json databaseId -q '.[0].databaseId')"
    fi
    ;;
  *) echo "usage: $0 notes|tag|publish <version>" >&2; exit 2 ;;
esac
