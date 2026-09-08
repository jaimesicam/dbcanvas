package main

import (
	"os"
	"strings"
	"testing"
)

// Percona Server 9.7 and MySQL Community 9.7 are the current LTS releases of each. The
// facts below were established against live nodes (stack "verify-97": percona-server
// 9.7.1-1 primary and replica, mysql-community-server 9.7.2) rather than from release
// notes, because two of them contradict what the documentation implies.

// 9.7 must be a series the frame accepts, not silently downgraded to the default.
func TestPS97IsARealSeries(t *testing.T) {
	if got := psMajorOf("9.7"); got != "9.7" {
		t.Fatalf("psMajorOf(9.7) = %q — a picker offering 9.7 would deploy something else", got)
	}
	if got := mysqlceMajorOf("9.7"); got != "9.7" {
		t.Fatalf("mysqlceMajorOf(9.7) = %q", got)
	}
	// The unsuffixed server package is right for 9.7 as it is for 8.0/8.4 — only the
	// legacy 5.7 series carries a version in its package name.
	for _, os := range []string{"oraclelinux", "ubuntu"} {
		if got := psServerPackages(os, "9.7"); len(got) == 0 || got[0] != "percona-server-server" {
			t.Errorf("psServerPackages(%s, 9.7)[0] = %q", os, got)
		}
	}
}

// Every behavioural fork that 8.4 introduced applies to 9.x too. Verified live: the
// plugin directory of a 9.7.1 server carries semisync_source.so and semisync_replica.so,
// and INSTALL PLUGIN rpl_semi_sync_source succeeded there.
func TestPS97UsesTheModernVocabulary(t *testing.T) {
	if !mysqlModernMajor("9.7") || !mysqlModernMajor("8.4") {
		t.Fatal("9.7 and 8.4 share the source/replica vocabulary")
	}
	if mysqlModernMajor("8.0") || mysqlModernMajor("5.7") {
		t.Error("8.0 and 5.7 predate it")
	}
	if p, so, v := semisyncSource("9.7"); p != "rpl_semi_sync_source" || so != "semisync_source.so" || v != "rpl_semi_sync_source_enabled" {
		t.Errorf("9.7 semi-sync source names are 8.4's, got %q/%q/%q", p, so, v)
	}
	if p, so, v := semisyncReplica("9.7"); p != "rpl_semi_sync_replica" || so != "semisync_replica.so" || v != "rpl_semi_sync_replica_enabled" {
		t.Errorf("9.7 semi-sync replica names are 8.4's, got %q/%q/%q", p, so, v)
	}
	if got := mysqlResetCmd("9.7"); got != "RESET BINARY LOGS AND GTIDS" {
		t.Errorf("RESET MASTER is gone in 9.7: %q", got)
	}
}

// percona-release cannot enable the 9.7 repository. Version 1.0-33 — the newest
// published — lists ps97lts among its products and then requests
// repo.percona.com/ps-97lts/ (404); spelled ps-97-lts it disables every Percona repo,
// enables nothing, and exits 0. Both were run against a live EL9 node. So the install
// script writes the repository by hand, and the empty product string is the signal.
func TestPS97WritesItsRepositoryByHand(t *testing.T) {
	if got := psClientProduct("9.7"); got != "" {
		t.Fatalf("psClientProduct(9.7) = %q — a percona-release product here would silently install nothing", got)
	}
	if got := pxbProduct("9.7"); got != "" {
		t.Fatalf("pxbProduct(9.7) = %q — pxb-97-lts has the same problem", got)
	}
	if got := psRepoName("9.7"); got != "ps-97-lts" {
		t.Errorf("psRepoName(9.7) = %q, want the real directory on repo.percona.com", got)
	}
	if got := pxbRepoName("9.7"); got != "pxb-97-lts" {
		t.Errorf("pxbRepoName(9.7) = %q", got)
	}
	if got := pxbPackage("9.7"); got != "percona-xtrabackup-97" {
		t.Errorf("pxbPackage(9.7) = %q — the installed package on the live node was percona-xtrabackup-97-9.7.1", got)
	}
	// The older series must keep going through percona-release, which works for them.
	if psClientProduct("8.4") != "ps84lts" || psClientProduct("8.0") != "ps80" {
		t.Error("the hand-written path must not swallow the series percona-release handles")
	}
	// And both install scripts have to carry the fork.
	for _, s := range []string{mysqlInstallRHEL, mysqlInstallDebian} {
		if !strings.Contains(s, `if [ -z "$PRODUCT" ]`) {
			t.Error("an install script has no hand-written repository path")
		}
	}
}

// Every caller of the shared install scripts has to pass REPO, or the hand-written
// path writes a repository URL with a hole in it and installs nothing.
func TestEveryInstallCallerPassesTheRepo(t *testing.T) {
	for _, f := range []string{"mysql.go", "aio_mysql.go", "pxc.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Skipf("%s not readable", f)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `"PRODUCT=" + ps`) && !strings.Contains(line, `"PRODUCT=" + pxb`) {
				continue
			}
			if !strings.Contains(line, `"REPO=" + `) {
				t.Errorf("%s passes PRODUCT without REPO:\n  %s", f, strings.TrimSpace(line))
			}
		}
	}
}

// The MySQL Community repository is spelled three different ways for the same series,
// and the apt component marks the LTS rather than the version.
func TestMySQLCE97RepoNames(t *testing.T) {
	if got := mysqlceToolsRepo("9.7"); got != "mysql-tools-9.7-community" {
		t.Errorf("mysqlceToolsRepo(9.7) = %q", got)
	}
	if got := mysqlceToolsRepo("8.0"); got != "mysql-tools-community" {
		t.Errorf("8.0's tools stay in the unversioned repo, got %q", got)
	}
	if !strings.Contains(mysqlceInstallDebian, "8.4|9.7) COMP=mysql-$MAJOR-lts") {
		t.Error("the apt component for 9.7 is mysql-9.7-lts, not mysql-9.7")
	}
}

// The 9.x client rejects the \G terminator in -e batch mode ("ERROR at line 1: Unknown
// command '\G'"), which made a healthy 9.7 replica fail its own status poll ten times
// and report a password warning as the reason. --vertical is identical and ancient.
func TestStatusPollsDoNotUseBackslashG(t *testing.T) {
	for _, f := range []string{"mysql.go", "replication.go", "aio_mysql.go", "innodb.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Skipf("%s not readable", f)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `\G"`) {
				continue
			}
			// SLAVE-era scripts only ever run against 5.7, whose client is fine with it,
			// and the comment explaining this rule naturally quotes the thing it bans.
			if strings.Contains(strings.ToUpper(line), "SLAVE") || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			t.Errorf("%s still terminates a status query with \\G, which 9.x rejects:\n  %s", f, strings.TrimSpace(line))
		}
	}
}

// A picker is only honest if the catalogue behind it lists versions that install. These
// came from probing the real repositories inside each image.
func TestVersionsCatalogueCarries97(t *testing.T) {
	raw, err := os.ReadFile("../versions.yaml")
	if err != nil {
		t.Skip("versions.yaml not readable")
	}
	txt := string(raw)
	for _, want := range []string{"9.7.1-1.1", "9.7.2-1"} {
		if !strings.Contains(txt, want) {
			t.Errorf("versions.yaml has no %s — run `make versions`", want)
		}
	}
}

// Every keyring import must be non-interactive. A provisioning step is retried, and on
// the second attempt the target file exists — at which point gpg asks whether to
// overwrite and dies with "cannot open '/dev/tty'". That is exactly how the first
// Ubuntu 9.7 deploy failed, ten attempts in a row.
func TestKeyringImportsAreNonInteractive(t *testing.T) {
	for _, f := range []string{"mysql.go", "mysqlce.go", "pxc.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Skipf("%s not readable", f)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "gpg ") || !strings.Contains(line, "--dearmor") {
				continue
			}
			if !strings.Contains(line, "--batch") || !strings.Contains(line, "--yes") {
				t.Errorf("%s imports a key without --batch --yes:\n  %s", f, strings.TrimSpace(line))
			}
		}
	}
}

// ------------------------------------------------------- InnoDB Cluster on 9.7

// percona-release prints the same repository under two spellings — "pdps9.7.1" in
// "Available setup products" and "pdps-9.7.1" in "Available repositories" — and each
// verb rejects the other's. The repository picker's catalogue was scraped from the
// whole output, so it offered both, and an InnoDB Cluster frame that saved the
// product name failed at install with "ERROR: Unknown repository: pdps9.7.1".
// Only names `percona-release enable` accepts belong in the catalogue.
func TestPDPSCatalogueOffersOnlyEnableableRepos(t *testing.T) {
	raw, err := os.ReadFile("../versions.yaml")
	if err != nil {
		t.Skip("versions.yaml not readable")
	}
	inPDPS, seen97 := false, false
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, " ") {
			inPDPS = strings.TrimSpace(line) == "pdps:"
			continue
		}
		if !inPDPS || !strings.HasPrefix(strings.TrimSpace(line), "- ") {
			continue
		}
		repo := strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-")), `"`)
		if !strings.Contains(strings.TrimPrefix(repo, "pdps"), "-") {
			t.Errorf("versions.yaml offers %q — a percona-release setup product, which `enable` rejects", repo)
		}
		if repo == "pdps-9.7.1" || repo == "pdps-97-lts" {
			seen97 = true
		}
	}
	if !seen97 {
		t.Error("versions.yaml has no 9.7 PDPS repository — run `make versions`")
	}
}

// The series a PDPS repo installs is the leading digits after the prefix, not any
// digits anywhere in the name. Reading it as a substring search made pdps-8.0.19 —
// and every other 8.0 minor ending in 9 — claim to be a modern series, so the node
// was sent RESET BINARY LOGS AND GTIDS, which 8.0 does not have.
func TestPSMajorOfRepoReadsTheSeriesNotAnyDigit(t *testing.T) {
	for repo, want := range map[string]string{
		"pdps-8.0":           "8.0",
		"pdps-8.0.19":        "8.0",
		"pdps-8.0.29":        "8.0",
		"pdps-8.0.46":        "8.0",
		"pdps-8.1.0":         "8.0",
		"pdps-8x-innovation": "8.0",
		"pdps-8.4.11":        "8.4",
		"pdps-84-lts":        "8.4",
		"pdps-9.7.1":         "9.7",
		"pdps-97-lts":        "9.7",
		"pdps9.7.1":          "9.7", // saved before the catalogue was fixed
		"pdps84lts":          "8.4",
	} {
		if got := psMajorOfRepo(repo); got != want {
			t.Errorf("psMajorOfRepo(%q) = %q, want %q", repo, got, want)
		}
	}
	// The 9.7 repo must reach the modern vocabulary, or the base setup runs
	// RESET MASTER against a server that removed it.
	if !mysqlModernMajor(psMajorOfRepo("pdps-9.7.1")) {
		t.Error("a 9.7 PDPS repo does not select the modern MySQL vocabulary")
	}
}

// The InnoDB install script must resolve the stored repository name rather than pass
// it straight to `enable`, and must not use -y (which `enable` has no flag for — it
// consumed the repository argument, so the first of the two attempts always failed).
func TestInnoDBInstallResolvesTheRepoName(t *testing.T) {
	for _, script := range []string{innodbInstallRHEL, innodbInstallDebian} {
		if !strings.Contains(script, `pdps_enable "$PDPS_REPO"`) {
			t.Error("InnoDB install does not resolve $PDPS_REPO through pdps_enable")
		}
		if strings.Contains(script, "percona-release enable -y") {
			t.Error("InnoDB install passes -y to `percona-release enable`, which has no such flag")
		}
	}
	// Resolution reads percona-release's repositories section, not its whole output.
	if !strings.Contains(pdpsEnable, "/^Available repositories:/,/^Available components:/p") {
		t.Error("pdps_enable does not restrict itself to the repositories section")
	}
	// A repository that cannot be enabled must say so; it used to fail silently and
	// surface as an empty "install packages:" error.
	if !strings.Contains(pdpsEnable, "failed:") {
		t.Error("pdps_enable swallows the percona-release error")
	}
}
