package main

import "testing"

// TestCompareVersions covers the case that motivates the function existing at all:
// "0.10.0" sorts BEFORE "0.2.0" as a string, which would have silently stopped the
// release notes at the tenth minor version.
func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.2.0", 0},
		{"0.2.0", "0.1.0", 1},
		{"0.1.0", "0.2.0", -1},
		{"0.10.0", "0.2.0", 1}, // the whole point
		{"0.2.0", "0.10.0", -1},
		{"1.0.0", "0.99.99", 1},
		{"1.2", "1.2.0", 0}, // a missing segment is zero
		{"1.2.0", "1.2", 0},
		{"1.2.1", "1.2", 1},
		{"2", "1.9.9", 1},
		{"1.2.0-rc1", "1.2.0", 0}, // a pre-release must not re-open a dismissed dialog
		{"1.2.0", "1.2.0-rc1", 0},
		{"dev", "0.2.0", 1}, // dev is newer than everything real
		{"dev", "99.99.99", 1},
		{"0.2.0", "dev", -1},
		{"dev", "dev", 0},
		{"", "0.0.1", -1},
		{"garbage", "0.0.0", 0}, // unparseable reads as zero rather than panicking
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestCompareVersionsIsAntisymmetric: swapping the arguments must negate the result,
// or an ordering built on it is incoherent.
func TestCompareVersionsIsAntisymmetric(t *testing.T) {
	vs := []string{"0.1.0", "0.2.0", "0.10.0", "1.0.0", "1.2.3", "dev", "2"}
	for _, a := range vs {
		for _, b := range vs {
			if compareVersions(a, b) != -compareVersions(b, a) {
				t.Errorf("compareVersions is not antisymmetric for %q vs %q", a, b)
			}
		}
	}
}

func TestAppVersionDefaultsToDev(t *testing.T) {
	// An unstamped `go build ./...` must keep working, and that is what the test
	// binary is. If this ever fails, somebody stamped the tests.
	if appVersion != devVersion {
		t.Logf("appVersion is %q (stamped build)", appVersion)
	}
}
