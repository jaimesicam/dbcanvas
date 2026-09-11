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
