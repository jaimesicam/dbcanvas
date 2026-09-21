package main

import (
	"strings"
	"testing"
)

// engineInstallScripts is every script that installs an engine's packages. Keyed by what it
// installs, so a failure names the engine rather than a variable.
var engineInstallScripts = map[string]string{
	"Percona Server (RHEL)":     mysqlInstallRHEL,
	"Percona Server (Debian)":   mysqlInstallDebian,
	"PXC (RHEL)":                pxcInstallRHEL,
	"PXC (Debian)":              pxcInstallDebian,
	"XtraBackup (RHEL)":         pxcInstallXtrabackupRHEL,
	"XtraBackup (Debian)":       pxcInstallXtrabackupDebian,
	"MySQL Community (RHEL)":    mysqlceInstallRHEL,
	"MySQL Community (Debian)":  mysqlceInstallDebian,
	"MariaDB (RHEL)":            mariadbInstallRHEL,
	"MariaDB (Debian)":          mariadbInstallDebian,
	"PSMDB (RHEL)":              mongoInstallRHEL,
	"PSMDB (Debian)":            mongoInstallDebian,
	"PostgreSQL (RHEL)":         patroniInstallRHEL,
	"PostgreSQL (Debian)":       patroniInstallDebian,
	"repmgr (RHEL)":             repmgrInstallRHEL,
	"repmgr (Debian)":           repmgrInstallDebian,
	"InnoDB Cluster (RHEL)":     innodbInstallRHEL,
	"InnoDB Cluster (Debian)":   innodbInstallDebian,
	"Valkey (RHEL)":             valkeyInstallRHEL,
	"Valkey (Debian)":           valkeyInstallDebian,
	"ProxySQL (RHEL)":           proxysqlInstallRHEL,
	"ProxySQL (Debian)":         proxysqlInstallDebian,
	"Orchestrator (RHEL)":       orchestratorInstallRHEL,
	"Orchestrator (Debian)":     orchestratorInstallDebian,
	"barman-cloud (RHEL)":       barmanInstallRHEL,
	"barman-cloud (Debian)":     barmanInstallDebian,
	"pg_oidc_validator (RHEL)":  pgOIDCScript,
	"Core Dump Analyzer (RHEL)": gdbInstallRHEL,
	"Core Dump Analyzer (Deb)":  gdbInstallDebian,
}

// A pinned minor has to reach the whole package set, dependencies included. Naming two packages
// at 16.10 and leaving the resolver to satisfy their base leaves an install that mixes releases
// — and on Percona PostgreSQL, one that fails outright on files owned by both 16.10 and 16.15.
func TestPinInstallPinsDependenciesToo(t *testing.T) {
	if !strings.Contains(pinInstallRHEL, "--requires --resolve --recursive") {
		t.Error("the RHEL helper no longer resolves what the pinned packages depend on")
	}
	if !strings.Contains(pinInstallDebian, "apt-cache depends --recurse") {
		t.Error("the Debian helper no longer resolves what the pinned packages depend on")
	}
	// The extra queries cost nothing when no version was chosen, which is the common case.
	for name, script := range map[string]string{"RHEL": pinInstallRHEL, "Debian": pinInstallDebian} {
		if !strings.Contains(script, "if [ ${#pinned[@]} -gt 0 ]; then") {
			t.Errorf("%s: the dependency pass must be skipped when nothing was pinned", name)
		}
	}
}

// One pin_install call is one transaction. Installing a package at a time is one resolution per
// package, so the second install is free to upgrade what the first one pinned.
func TestEveryEngineInstallsItsPackagesInOneTransaction(t *testing.T) {
	for name, script := range engineInstallScripts {
		if strings.Contains(script, "do pin_install") {
			t.Errorf("%s installs one package at a time; pass the whole set to one pin_install", name)
		}
		if !strings.Contains(script, "pin_install") {
			t.Errorf("%s does not go through pin_install, so a chosen minor cannot reach it", name)
		}
	}
}

// Every engine script must carry the helper it calls: a `pin_install` that was never defined is a
// script that fails at run time on a node, which is a slow way to find a missing prefix.
func TestEveryEngineScriptDefinesTheHelperItCalls(t *testing.T) {
	for name, script := range engineInstallScripts {
		if !strings.Contains(script, "pin_install() {") {
			t.Errorf("%s calls pin_install without prefixing pinInstallRHEL/pinInstallDebian", name)
		}
	}
}

// The packages installed beside an engine are the other way a version drifts: they are not the
// engine, but they depend on it, and dnf will happily upgrade the engine to satisfy them.
func TestPackagesInstalledBesideAnEngineArePinnedToIt(t *testing.T) {
	for name, script := range map[string]string{
		"XtraBackup (RHEL)":        pxcInstallXtrabackupRHEL,
		"XtraBackup (Debian)":      pxcInstallXtrabackupDebian,
		"barman-cloud (RHEL)":      barmanInstallRHEL,
		"barman-cloud (Debian)":    barmanInstallDebian,
		"pg_oidc_validator (RHEL)": pgOIDCScript,
	} {
		if strings.Contains(script, "dnf -y -q install percona") || strings.Contains(script, `dnf -y -q install "$PKG"`) {
			t.Errorf("%s still installs an engine-adjacent package with a bare dnf", name)
		}
		if strings.Contains(script, "apt-get install -y -qq barman") || strings.Contains(script, `apt-get install -y -qq "$PKG"`) {
			t.Errorf("%s still installs an engine-adjacent package with a bare apt-get", name)
		}
	}
}

// Oracle Linux 10 renamed its MySQL and MariaDB packages, and Percona's el10 build of Percona
// Server 8.0 still carries unversioned Obsoletes on the old spellings (mariadb-server,
// mariadb-backup, …). On EL10 those names are provided by mariadb11.8-*, so dnf5 pulls that whole
// stack into the transaction and the install dies on a file conflict:
//
//	file /var/lib/mysql conflicts between attempted installs of
//	percona-server-server-8.0.45-36.1.el10.x86_64 and mariadb11.8-server-3:11.8.8-1.el10_2.x86_64
//
// Reproduced against the real repositories in the real image, and gone with the excludes.
func TestDistroExcludesAreEL10Only(t *testing.T) {
	const want = "mariadb11.8*,mysql8.4*"
	if got := mysqlDistroExcludes("oraclelinux", "10"); got != want {
		t.Errorf("Oracle Linux 10 = %q, want %q", got, want)
	}
	// Excluding mariadb11.8* alone only moves the conflict to mysql8.4*, so both have to go.
	for _, pkg := range []string{"mariadb11.8*", "mysql8.4*"} {
		if !strings.Contains(mysqlDistroExcludes("oraclelinux", "10"), pkg) {
			t.Errorf("EL10 excludes must cover %s", pkg)
		}
	}
	// Nowhere else. EL8/EL9 never had the renamed packages, and Debian is a different resolver
	// with a different problem — an exclude there would be a guess.
	for _, tc := range []struct{ os, ver string }{
		{"oraclelinux", "9"}, {"oraclelinux", "8"},
		{"debian", "12"}, {"ubuntu", "24.04"}, {"debian", "10"},
	} {
		if got := mysqlDistroExcludes(tc.os, tc.ver); got != "" {
			t.Errorf("%s %s = %q, want empty", tc.os, tc.ver, got)
		}
	}
}

// The excludes are one caller's, deliberately, and must not drift into pin_install's defaults:
// the MariaDB node kind installs mariadb11.8-* on purpose, and a global exclude would break the
// one thing on EL10 that wants those packages.
func TestDistroExcludesStayOutOfTheSharedHelper(t *testing.T) {
	for _, name := range []string{"MariaDB (RHEL)", "MariaDB (Debian)"} {
		if strings.Contains(engineInstallScripts[name], "mariadb11.8*") {
			t.Errorf("%s must not exclude the packages it exists to install", name)
		}
	}
	// pin_install reads EXCL from the environment rather than hard-coding a list, so a script
	// that does not set it is unaffected.
	if !strings.Contains(pinInstallRHEL, `${EXCL:-}`) {
		t.Error("the RHEL helper no longer takes its exclude list from EXCL")
	}
	if !strings.Contains(pinInstallRHEL, `"${excl[@]}" install`) {
		t.Error("the RHEL helper builds an exclude list but never passes it to dnf")
	}
	// And the exclusion is empty unless a caller sets EXCL: an unset variable must not become
	// `--exclude=`, which dnf rejects.
	if !strings.Contains(pinInstallRHEL, `[ -n "${EXCL:-}" ] && excl=(--exclude="$EXCL")`) {
		t.Error("EXCL must be applied only when it is non-empty")
	}
}
