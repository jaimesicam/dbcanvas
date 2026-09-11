package main

// install_pin.go — shared shell helpers that install packages while respecting a selected minor
// version. Each engine's install script sources one of these (defining a `pin_install` function)
// and calls `pin_install <packages…>` with `VER=<catalog minor>` in the environment. When VER is
// set, every listed package that actually has that version is pinned to it, **and so is every
// dependency that publishes the same version**; packages that don't publish that version (e.g. a
// separately-versioned client like mongosh, or a base-OS library) are left to the resolver.
// VER="" ⇒ all latest, and not a single extra query.
//
// PINNING THE NAMED PACKAGES IS NOT ENOUGH, which is what this file exists to say. An engine is
// usually a split package set — a server, a client, a base, a libs — and naming two of them at
// 16.10 leaves the resolver free to satisfy the rest with the newest build it can see. On Percona
// PostgreSQL that is not a subtle drift, it is a failed install:
//
//	file /usr/pgsql-16/share/man/man1/pg_waldump.1 conflicts between attempted installs of
//	percona-postgresql16-server-1:16.10-1.el9.x86_64 and percona-postgresql16-1:16.15-1.el9.x86_64
//
// — the server pinned to the minor that was asked for, its own base package resolved to the
// newest one in the repo, and the two carrying the same files. Where the packages do not conflict
// the same gap is quieter and worse: an install that looks fine and is a mix of two releases.
//
// So the pinned specs are resolved first, and any dependency that the repository also builds at
// VER is pinned alongside them. "Also builds at VER" is the whole test, and it is deliberately not
// a name-prefix rule: a sibling is whatever ships the same version, which is exactly the set that
// has to move together, and nothing else is touched.
//
// PASS THE WHOLE SET IN ONE CALL. Several engines used to loop, installing a package at a time;
// that is one resolution per package, so nothing stops the second install from upgrading what the
// first one just pinned — and with sibling pinning it is also N times the queries. One call is one
// transaction, which is the unit dnf and apt both resolve consistently.
//
// A PACKAGE WITH NO BUILD AT VER SAYS SO. The fallback to latest is deliberate — mongosh and
// XtraBackup carry their own version series and will never match an engine's minor — but it is
// also what happens when a repository drops the release that was asked for (MariaDB's mirrors
// keep two), and that must not pass in silence. The note lands in the node's deploy log, where
// somebody comparing "what I chose" against "what is running" will find it.
//
// `pin_present` is the companion for packages that exist in some builds of a repository and not
// others. Naming a package a repo does not carry fails the whole install — which is the behaviour
// we want for a package that is simply misspelled, and the reason a conditional sibling cannot
// just be added to the list. Filtering it first keeps both: the required names stay strict, and an
// optional one is pinned wherever it exists. See psServerPackagesOptional for the one that made
// this necessary.

// The catalog minor strings come from the per-image version catalog, so they match the target
// repo's format (RPM `16.4-1`, DEB `16.4-1.…`); the RHEL matcher globs `-<VER>*`, the Debian
// matcher resolves the exact `apt-cache madison` version containing VER.
const pinInstallRHEL = `pin_install() {
  local specs=() pinned=() p d deps avail
  for p in "$@"; do
    if [ -n "$VER" ] && [ -n "$(dnf -q repoquery "${p}-${VER}*" 2>/dev/null)" ]; then
      specs+=("${p}-${VER}*")
      pinned+=("${p}-${VER}*")
    else
      [ -n "$VER" ] && echo "note: $p has no $VER build in this repository — installing the newest it has"
      specs+=("$p")
    fi
  done
  # Dependencies resolve to the newest build unless they are pinned too — see install_pin.go.
  # Two queries, both skipped entirely when nothing was pinned: what the pinned packages pull in,
  # and which of those the repositories also carry at this version.
  if [ ${#pinned[@]} -gt 0 ]; then
    deps=$(dnf -q repoquery --requires --resolve --recursive --qf '%{name}' "${pinned[@]}" 2>/dev/null | sort -u)
    if [ -n "$deps" ]; then
      avail=" $(dnf -q repoquery --qf '%{name}' $(for d in $deps; do printf '%s-%s* ' "$d" "$VER"; done) 2>/dev/null | sort -u | tr '\n' ' ') "
      for d in $deps; do
        case "$avail" in *" $d "*) ;; *) continue ;; esac
        case " ${specs[*]} " in *" ${d}-${VER}"*) continue ;; esac
        specs+=("${d}-${VER}*")
      done
    fi
  fi
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
  local specs=() pinned=() p d exact deps
  for p in "$@"; do
    exact=""
    if [ -n "$VER" ]; then
      exact=$(apt-cache madison "$p" 2>/dev/null | awk -F'|' -v v="$VER" 'index($2,v){gsub(/ /,"",$2);print $2;exit}')
    fi
    if [ -n "$exact" ]; then
      specs+=("${p}=${exact}"); pinned+=("$p")
    else
      [ -n "$VER" ] && echo "note: $p has no $VER build in this repository — installing the newest it has"
      specs+=("$p")
    fi
  done
  # Same reasoning as the RHEL half: a pinned server with an unpinned base is two releases in one
  # install. apt-cache reads the local lists, so the recursive walk costs nothing on the network.
  if [ ${#pinned[@]} -gt 0 ]; then
    deps=$(apt-cache depends --recurse --no-recommends --no-suggests --no-conflicts \
             --no-breaks --no-replaces --no-enhances "${pinned[@]}" 2>/dev/null |
           grep -v '^ ' | grep -v '^<' | sort -u)
    for d in $deps; do
      case " ${specs[*]} " in *" ${d}="*) continue ;; esac
      exact=$(apt-cache madison "$d" 2>/dev/null | awk -F'|' -v v="$VER" 'index($2,v){gsub(/ /,"",$2);print $2;exit}')
      [ -n "$exact" ] && specs+=("${d}=${exact}")
    done
  fi
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
