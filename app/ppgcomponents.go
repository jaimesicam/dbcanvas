package main

import (
	"fmt"
	"strings"
)

// ppgcomponents.go — which Percona Distribution for PostgreSQL components exist for which
// PostgreSQL major on which OS, and what each is called there.
//
// The rules used to be scattered: pgTDEMajors in pgtde.go, a hard-coded "18" for pg_oidc_validator in
// pgoidc.go, and copies of both in the designer (PG_TDE_MAJORS, pgOidcMajorOk). None of them knew
// about the OS, and the package names were spelled by hand per call site — which is how the
// Debian name for pg_tde came to be percona-pg-tde-18 when the repository calls it
// percona-pg-tde18, failing every pg_tde deploy on Ubuntu 18 (and 17.7+).
//
// So this table is the one place, and it is served to the designer with the PPG catalogue
// (handlePPGCatalog) so the form greys out exactly what validation would reject.
//
// The majors were read off repo.percona.com/ppg-<major> for every OS release the PostgreSQL
// forms offer (2026-10-14).

// ppgComponent is one optional component of a Percona PostgreSQL node.
type ppgComponent struct {
	ID    string `json:"id"`    // the name the design and the API use
	Label string `json:"label"` // what the form calls it
	// Majors it is packaged for. Every OS release the PostgreSQL forms offer (Oracle Linux 9/10,
	// Ubuntu 22.04/24.04) has the same set for every component below — the repository was
	// checked for that — so this is per major only.
	Majors []string `json:"majors"`
	// MinMinor is the first minor of a major the component works with, where that is not the
	// whole series ("" or absent = every minor). Not visible in package metadata — the packages
	// do not declare which server they need — so these come from installing each minor and
	// loading the extension (see the pg_tde note).
	MinMinor map[string]string `json:"minMinor,omitempty"`
	// RHEL and Debian are the package name per family, %s being the major. "" means the
	// component has no package of its own there (it is in the server package).
	RHEL   string `json:"rhel"`
	Debian string `json:"debian"`
}

var ppgComponents = []ppgComponent{
	{
		ID: "pg_tde", Label: "pg_tde (transparent data encryption)",
		Majors: []string{"17", "18"},
		// 17.0 has no pg_tde that loads: not in its server package, and every separate build
		// fails on it with "undefined symbol: percona_api_version" (installed and tried, OL9,
		// 2026-10-14). 17.1 was never published, so 17.2 is the first that works.
		MinMinor: map[string]string{"17": "17.2"},
		// Up to 17.6 pg_tde is inside percona-postgresql17-server; from 17.7 it is a package of
		// its own, and which build of it loads depends on the server build — pgTDEInstall*
		// find the one that does on the node itself.
		RHEL: "percona-pg_tde%s", Debian: "percona-pg-tde%s",
	},
	{
		ID: "pg_oidc_validator", Label: "pg_oidc_validator (OAuth/OIDC login)",
		// PostgreSQL 18 is the first with OAuth validator modules; there is nothing to build
		// it against before that.
		Majors: []string{"18"},
		RHEL:   "percona-pg_oidc_validator%s", Debian: "percona-pg-oidc-validator%s",
	},
	{
		ID: "pgaudit", Label: "pgAudit",
		Majors: []string{"13", "14", "15", "16", "17", "18"},
		RHEL:   "percona-pgaudit%s", Debian: "percona-postgresql-%s-pgaudit",
	},
	{
		ID: "set_user", Label: "set_user",
		Majors: []string{"13", "14", "15", "16", "17", "18"},
		RHEL:   "percona-pgaudit%s_set_user", Debian: "percona-pgaudit%s-set-user",
	},
	{
		ID: "pg_repack", Label: "pg_repack",
		Majors: []string{"13", "14", "15", "16", "17", "18"},
		RHEL:   "percona-pg_repack%s", Debian: "percona-postgresql-%s-repack",
	},
	{
		ID: "wal2json", Label: "wal2json",
		Majors: []string{"13", "14", "15", "16", "17", "18"},
		RHEL:   "percona-wal2json%s", Debian: "percona-postgresql-%s-wal2json",
	},
	{
		ID: "pg_cron", Label: "pg_cron",
		// ppg-13 has none, on any OS.
		Majors: []string{"14", "15", "16", "17", "18"},
		RHEL:   "percona-pg_cron_%s", Debian: "percona-postgresql-%s-cron",
	},
}

// ppgComponentByID returns the component called id.
func ppgComponentByID(id string) (ppgComponent, bool) {
	for _, c := range ppgComponents {
		if c.ID == id {
			return c, true
		}
	}
	return ppgComponent{}, false
}

// Package is the component's package for a major on an OS ("" if it has none of its own).
func (c ppgComponent) Package(nodeOS, major string) string {
	pat := c.RHEL
	if isDebianOS(nodeOS) {
		pat = c.Debian
	}
	if pat == "" {
		return ""
	}
	return fmt.Sprintf(pat, ppgMajorOf(major))
}

// Available reports whether the component can be installed on this OS, major and minor, and if
// not, why — in words that name the thing to change. minor is the node's pinned minor ("" =
// newest, which is always at least MinMinor).
func (c ppgComponent) Available(nodeOS, major, minor string) (bool, string) {
	m := ppgMajorOf(major)
	if !containsStr(c.Majors, m) {
		return false, fmt.Sprintf("%s is packaged for PostgreSQL %s only, not %s", c.Label, joinOr(c.Majors), m)
	}
	if min := c.MinMinor[m]; min != "" && minor != "" && compareVersions(ppgMinorOf(minor), min) < 0 {
		return false, fmt.Sprintf("%s needs PostgreSQL %s or newer, and this node is pinned to %s", c.Label, min, ppgMinorOf(minor))
	}
	return true, ""
}

// ppgMinorOf strips the package release from a catalogue minor: "17.5-3" → "17.5".
func ppgMinorOf(v string) string {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i]
	}
	return v
}
