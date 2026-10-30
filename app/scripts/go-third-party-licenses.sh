#!/bin/sh
# Prints the licence of every Go module compiled into the given packages, and of the
# Go standard library, which is linked into every binary. The files that travel with
# a licence come too: NOTICE (Apache-2.0 s.4(d) obliges us to pass it on),
# THIRD-PARTY-NOTICES for code a module vendors (mongo-driver), and the Go project's
# PATENTS grant. app/Dockerfile runs it in the server and CLI build
# stages and ships the output beside the binaries (/usr/share/doc/dbcanvas/).
#
#   usage: go-third-party-licenses.sh TITLE PACKAGE...   (from the module's root)
#
# A module with no licence file fails the build on purpose: a new dependency must
# not ship without somebody having looked at its licence.
set -eu
title=$1; shift
rule='=============================================================================='

printf '%s\n\n%s\n%s\n%s\n' "$title" "$rule" "Go standard library and runtime ($(go env GOVERSION))  (BSD-3-Clause)" "$rule"
cat "$(go env GOROOT)/LICENSE"
[ ! -f "$(go env GOROOT)/PATENTS" ] || { echo; cat "$(go env GOROOT)/PATENTS"; }

go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' "$@" |
  sort -u |
  while read -r path version dir; do
    files=$(ls "$dir" | grep -iE '^(licen[cs]e|copying|notice|third[-_]party|patents)' || true)
    if ! echo "$files" | grep -qiE '^(licen[cs]e|copying)'; then echo "error: $path $version has no licence file in $dir" >&2; exit 1; fi
    printf '\n%s\n%s\n%s\n' "$rule" "$path $version" "$rule"
    for f in $files; do cat "$dir/$f"; echo; done
  done
