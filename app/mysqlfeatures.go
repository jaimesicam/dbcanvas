package main

import "net/http"

// mysqlfeatures.go — the one table of MySQL-family server features that depend on the release:
// which option or capability exists from which version (and, for the keyring plugins, until
// which series). Validation, the deploy scripts and the designer all read it, the designer
// through GET /api/catalog/mysql-features (lib/mysqlFeatures.js), so the form, the validator
// and the my.cnf DBCanvas writes cannot disagree about a version.
//
// Versions are compared numerically on their dotted part: "8.0.45-36.1" (Percona Server),
// "8.0.36-28.1" (PXC) and "8.0.40" (MySQL Community) are 8.0.45, 8.0.36 and 8.0.40. A node that
// is not pinned to a minor gets the newest of its series, so an unpinned node counts as having
// everything its series ever gained.

// mysqlFeature is one entry of the table.
type mysqlFeature struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Since is the first release that has it ("" — every release DBCanvas offers).
	Since string `json:"since,omitempty"`
	// Until is the first series that no longer has it ("" — still there).
	Until string `json:"until,omitempty"`
	// PerconaOnly is a Percona Server / PXC option that Oracle's MySQL does not have at all.
	PerconaOnly bool `json:"perconaOnly,omitempty"`
}

// The keys the code asks about.
const (
	featDefaultTableEncryption = "default_table_encryption"
	featBinlogEncryption       = "binlog_encryption"
	featRedoLogEncrypt         = "innodb_redo_log_encrypt"
	featUndoLogEncrypt         = "innodb_undo_log_encrypt"
	featTempTablespaceEncrypt  = "innodb_temp_tablespace_encrypt"
	featClone                  = "clone"
	featKeyringPlugins         = "keyring_plugins"
	featKeyringComponents      = "keyring_components"
)

var mysqlFeatures = []mysqlFeature{
	{Key: featDefaultTableEncryption, Label: "default_table_encryption", Since: "8.0.16"},
	{Key: featBinlogEncryption, Label: "binary log encryption (binlog_encryption)", Since: "8.0.14"},
	{Key: featRedoLogEncrypt, Label: "redo log encryption (innodb_redo_log_encrypt)", Since: "8.0.1"},
	{Key: featUndoLogEncrypt, Label: "undo log encryption (innodb_undo_log_encrypt)", Since: "8.0.1"},
	{Key: featTempTablespaceEncrypt, Label: "temporary tablespace encryption (innodb_temp_tablespace_encrypt)", Since: "8.0.16", PerconaOnly: true},
	{Key: featClone, Label: "the CLONE plugin", Since: "8.0.17"},
	// The keyring plugins (keyring_file, keyring_vault) were removed in 8.4.
	{Key: featKeyringPlugins, Label: "keyring plugins (keyring_file, keyring_vault)", Until: "8.4"},
	// The keyring components exist from 8.0.24, but DBCanvas keys a series with one keyring
	// shape: the plugin up to 8.0, the component from 8.4, where it is the only choice.
	{Key: featKeyringComponents, Label: "keyring components (component_keyring_file, component_keyring_vault)", Since: "8.4.0"},
}

// mysqlFeatureOf looks a key up; an unknown key is a programming error, and reads as absent.
func mysqlFeatureOf(key string) (mysqlFeature, bool) {
	for _, f := range mysqlFeatures {
		if f.Key == key {
			return f, true
		}
	}
	return mysqlFeature{}, false
}

// mysqlEffectiveVersion is the release a node runs: its pinned version, or the newest of its
// series when it is not pinned. The series is normalized the way psMajorOf does for Percona
// Server; a MySQL Community or PXC major passes through the same way.
func mysqlEffectiveVersion(major, version string) string {
	if version != "" {
		return version
	}
	return psMajorOf(major) + ".9999"
}

// mysqlHas reports whether a server of this series (and pinned minor, if any) has the feature.
func mysqlHas(key, major, version string) bool {
	f, ok := mysqlFeatureOf(key)
	if !ok {
		return false
	}
	v := mysqlEffectiveVersion(major, version)
	if f.Since != "" && compareVersions(v, f.Since) < 0 {
		return false
	}
	if f.Until != "" && compareVersions(v, f.Until) >= 0 {
		return false
	}
	return true
}

// mysqlHasOn is mysqlHas for a server of a known distribution: community is Oracle's MySQL,
// which has none of the PerconaOnly features.
func mysqlHasOn(key string, community bool, major, version string) bool {
	f, ok := mysqlFeatureOf(key)
	if !ok || (community && f.PerconaOnly) {
		return false
	}
	return mysqlHas(key, major, version)
}

// handleMySQLFeatures is GET /api/catalog/mysql-features: the table, for the designer.
func (a *App) handleMySQLFeatures(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"features": mysqlFeatures})
}

// mysqlKeyringComponent reports whether a series is keyed with a keyring component
// (component_keyring_file, component_keyring_vault) rather than a keyring plugin.
func mysqlKeyringComponent(major string) bool {
	return mysqlHas(featKeyringComponents, major, "")
}
