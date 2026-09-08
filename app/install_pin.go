package main

// install_pin.go — shared shell helpers that install packages while respecting a selected minor
// version. Each engine's install script sources one of these (defining a `pin_install` function)
// and calls `pin_install <packages…>` with `VER=<catalog minor>` in the environment. When VER is
// set, every listed package that actually has that version is pinned to it (dependencies follow,
// so shared sub-packages get the matching version); packages that don't publish that version
// (e.g. a separately-versioned client like mongosh) fall back to latest. VER="" ⇒ all latest.
//
// The catalog minor strings come from the per-image version catalog, so they match the target
// repo's format (RPM `16.4-1`, DEB `16.4-1.…`); the RHEL matcher globs `-<VER>*`, the Debian
// matcher resolves the exact `apt-cache madison` version containing VER.
//
// `pin_present` is the companion for packages that exist in some builds of a repository and not
// others. Naming a package a repo does not carry fails the whole install — which is the behaviour
// we want for a package that is simply misspelled, and the reason a conditional sibling cannot
// just be added to the list. Filtering it first keeps both: the required names stay strict, and an
// optional one is pinned wherever it exists. See psServerPackagesOptional for the one that made
// this necessary.

const pinInstallRHEL = `pin_install() {
  local specs=() p
  for p in "$@"; do
    if [ -n "$VER" ] && [ -n "$(dnf -q repoquery "${p}-${VER}*" 2>/dev/null)" ]; then
      specs+=("${p}-${VER}*")
    else
      specs+=("$p")
    fi
  done
  dnf -y -q install "${specs[@]}"
}
pin_present() {
  local p
  for p in "$@"; do
    [ -n "$(dnf -q repoquery "$p" 2>/dev/null)" ] && printf '%s ' "$p"
  done
  return 0
}
`

const pinInstallDebian = `pin_install() {
  local specs=() p exact
  for p in "$@"; do
    exact=""
    if [ -n "$VER" ]; then
      exact=$(apt-cache madison "$p" 2>/dev/null | awk -F'|' -v v="$VER" 'index($2,v){gsub(/ /,"",$2);print $2;exit}')
    fi
    if [ -n "$exact" ]; then specs+=("${p}=${exact}"); else specs+=("$p"); fi
  done
  apt-get install -y -qq "${specs[@]}"
}
pin_present() {
  local p
  for p in "$@"; do
    [ -n "$(apt-cache policy "$p" 2>/dev/null)" ] && printf '%s ' "$p"
  done
  return 0
}
`
