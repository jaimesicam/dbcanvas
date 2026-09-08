package main

import (
	"strconv"
	"strings"
)

// version.go — what build of DBCanvas this is.
//
// The value comes from the repo's VERSION file, stamped in at link time:
//
//	go build -ldflags "-X main.appVersion=$(cat VERSION)"
//
// which is what `make build` and the Dockerfile do. A plain `go build ./...` in
// this directory leaves it at "dev", and that has to keep working — the Go test
// suite and everyone's local build depend on it.
//
// The numbering is deliberately small and flat: it started at 0.0.1 and each
// update adds 0.0.1, so the third release is 0.0.3 and the tenth is 0.0.10. There
// is no semantic-versioning promise here — nothing consumes DBCanvas as a library,
// so a major/minor/patch split would be describing a compatibility contract that
// does not exist. Two rules keep it working:
//
//   - Bump VERSION in the same change that adds the What's New note for it. A
//     release with no note shows an empty dialog to nobody, and a note with no
//     release is invisible; whatsnew_test.go catches the second, not the first.
//   - Never renumber a release somebody has already seen. Read state is stored per
//     account as the version string it acknowledged, so moving a number backwards
//     makes an installation look rolled back — handled (hasUnseenIn treats a `seen`
//     ahead of `current` as showing nothing), but it means those readers silently
//     stop getting notes until the number passes them again.
//
// "dev" sorts *newer* than every real version (see compareVersions). That is
// deliberate, and its effect is narrow: a dev build never *suppresses* the What's
// New dialog on the "you have already acknowledged this build" test, so a note
// written for an untagged release is visible to the person writing it. It cannot
// conjure notes that do not exist — a dev build with nothing new written still
// shows nothing, which is what an empty changelog should do.
var appVersion = "dev"

// devVersion is the value appVersion holds in an unstamped build.
const devVersion = "dev"

// compareVersions orders two dotted numeric versions: -1 if a is older than b, +1
// if newer, 0 if they are the same release.
//
// It exists because string comparison gets this wrong in the one case that will
// certainly happen: "0.10.0" < "0.2.0" lexically, and that would silently stop
// showing release notes at the tenth minor version. Segments are compared as
// integers, a missing segment counts as zero ("1.2" == "1.2.0"), and any
// non-numeric suffix on a segment is ignored ("1.2.0-rc1" == "1.2.0") — a
// pre-release should not re-pop a dialog the final release already dismissed.
func compareVersions(a, b string) int {
	if a == b {
		return 0
	}
	// dev is newer than everything real, and two devs are equal (handled above).
	if a == devVersion {
		return 1
	}
	if b == devVersion {
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		if d := segment(as, i) - segment(bs, i); d != 0 {
			if d < 0 {
				return -1
			}
			return 1
		}
	}
	return 0
}

// segment reads the i'th dotted segment as an integer, treating a missing segment
// as zero and stopping at the first non-digit so "0-rc1" reads as 0.
func segment(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	s := parts[i]
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}
