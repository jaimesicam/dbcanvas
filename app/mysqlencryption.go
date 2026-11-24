package main

import (
	"context"
	"strings"
)

// mysqlencryption.go — what a keyring-encrypted MySQL-family server encrypts besides its tables:
// the binary logs, the InnoDB redo and undo logs, and the temporary tablespaces. Each is one
// server option, and each needs the keyring (a keyring file, keyringfile.go, or OpenBao,
// dbvault.go) for the key it encrypts with, so they are offered only alongside one, and only
// on a release that has the option (mysqlFeatures).
//
// The options are written into the node's config with the keyring's own, before mysqld first
// starts, and checked once it runs.

// mysqlEncryption is the design's choice; it rides on designNode and designFrame.
type mysqlEncryption struct {
	EncryptBinlog   bool `json:"encryptBinlog,omitempty"`
	EncryptRedoUndo bool `json:"encryptRedoUndo,omitempty"`
	EncryptTemp     bool `json:"encryptTemp,omitempty"`
}

// mysqlEncryptionVar is one option, with the feature that says which releases have it.
type mysqlEncryptionVar struct {
	Variable, Feature string
	on                func(mysqlEncryption) bool
}

var mysqlEncryptionVars = []mysqlEncryptionVar{
	{"binlog_encryption", featBinlogEncryption, func(e mysqlEncryption) bool { return e.EncryptBinlog }},
	{"innodb_redo_log_encrypt", featRedoLogEncrypt, func(e mysqlEncryption) bool { return e.EncryptRedoUndo }},
	{"innodb_undo_log_encrypt", featUndoLogEncrypt, func(e mysqlEncryption) bool { return e.EncryptRedoUndo }},
	{"innodb_temp_tablespace_encrypt", featTempTablespaceEncrypt, func(e mysqlEncryption) bool { return e.EncryptTemp }},
}

// any reports whether anything besides tables is to be encrypted.
func (e mysqlEncryption) any() bool { return e.EncryptBinlog || e.EncryptRedoUndo || e.EncryptTemp }

// variables are the server variables the choice turns on.
func (e mysqlEncryption) variables() []string {
	var out []string
	for _, v := range mysqlEncryptionVars {
		if v.on(e) {
			out = append(out, v.Variable)
		}
	}
	return out
}

// mysqlEncryptionOptions renders the [mysqld] lines.
func mysqlEncryptionOptions(e mysqlEncryption) string {
	var b strings.Builder
	for _, v := range e.variables() {
		b.WriteString(v + "=ON\n")
	}
	return b.String()
}

// mysqlEncryptionIssues validates the choice on a node or frame: it needs a keyring, a release
// with each option, and — for the binary logs — no XtraBackup point-in-time restore, which
// replays downloaded binary logs with mysqlbinlog, and mysqlbinlog cannot read an encrypted
// binary log from a file.
func mysqlEncryptionIssues(who string, e mysqlEncryption, keyring, community bool, major, version string, xbPITR bool) []issue {
	if !e.any() {
		return nil
	}
	if !keyring {
		return []issue{{Level: "error", Message: who + " encrypts its logs or temporary tablespaces without a keyring — turn on a keyring file or OpenBao encryption, which holds the keys they are encrypted with"}}
	}
	var out []issue
	seen := map[string]bool{}
	for _, v := range mysqlEncryptionVars {
		if !v.on(e) || seen[v.Feature] {
			continue
		}
		seen[v.Feature] = true
		f, _ := mysqlFeatureOf(v.Feature)
		switch {
		case community && f.PerconaOnly:
			out = append(out, issue{Level: "error", Message: who + ": " + f.Label + " is a Percona Server option — MySQL Community does not have it"})
		case !mysqlHas(v.Feature, major, version):
			out = append(out, issue{Level: "error", Message: who + ": " + f.Label + " needs " + f.Since + " or later, and this server is " + orDefault(version, major)})
		}
	}
	if e.EncryptBinlog && xbPITR {
		out = append(out, issue{Level: "error", Message: who + ": binary log encryption and XtraBackup point-in-time restore exclude each other — a restore replays the shipped binary logs with mysqlbinlog, which cannot read an encrypted binary log file. Turn off one of them"})
	}
	return out
}

// mysqlEncryptionVerifyScript checks every variable the design turned on reads ON (Percona
// Server 8.0 reports innodb_redo_log_encrypt=ON as MASTER_KEY). Env: VARS
// (space-separated), ROOT_PW.
const mysqlEncryptionVerifyScript = `set -e
export MYSQL_PWD="$ROOT_PW"
for v in $VARS; do
  got=$(mysql -u root -N -B -e "SELECT @@GLOBAL.$v" 2>/dev/null | tail -1)
  case "$got" in
    1|ON|MASTER_KEY|KEYRING_KEY) echo "$v=$got" ;;
    *) echo "$v is '${got:-unknown}', not ON"; exit 1 ;;
  esac
done`

// verifyMySQLEncryption runs mysqlEncryptionVerifyScript when the frame asks for anything.
func (a *App) verifyMySQLEncryption(ctx context.Context, frame designFrame, containerID, rootPW string, pr *pxcProg) error {
	vars := frame.mysqlEncryption.variables()
	if len(vars) == 0 || mysqlKeyringKind(frame) == "" {
		return nil
	}
	return a.runStep(ctx, containerID, mysqlEncryptionVerifyScript,
		[]string{"VARS=" + strings.Join(vars, " "), "ROOT_PW=" + rootPW}, pr.logln)
}
