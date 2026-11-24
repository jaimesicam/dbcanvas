package main

import "encoding/json"

// xbkeyring.go — XtraBackup backups of a keyring-encrypted server (a keyring file,
// keyringfile.go, or OpenBao, dbvault.go).
//
// An encrypted tablespace's key is stored in its header under the server's master key, and the
// master key is in that server's keyring. Every member of a cluster has a keyring of its own, so
// a backup of one member cannot simply be restored on another: its tablespace keys are under a
// master key the other member has never seen. Copying keyrings around would work for a keyring
// file and not at all for OpenBao, where every member has its own mount.
//
// XtraBackup's transition key solves it without moving a keyring. The backup is taken with
// --transition-key: xtrabackup reads each tablespace key with the server's master key and writes
// it into the backup under the transition key instead. --prepare decrypts with the same key, and
// --move-back --generate-new-master-key re-encrypts the tablespace keys under a brand-new master
// key that it writes into the restoring member's own keyring. The keyring never leaves its node,
// and the bucket holds nothing that opens the backup.
//
// The transition key is random for every full backup; an incremental reuses its base's, so a
// whole chain prepares with one key. DBCanvas keeps it, not the bucket: in every member's
// deployment secrets (sealed with the rest of them), under xbTransitionKeys, so any member
// still knows it when the one that took the backup has been rebuilt. Losing the stack loses the
// keys, and with them the backups, which live in the stack's own SeaweedFS anyway.

// xbTransitionKeysField is the deployment-secrets key the transition keys are kept under.
const xbTransitionKeysField = "xbTransitionKeys"

// keyring is the target's keyring: "file", "vault" or "".
func (t xbTarget) keyring() string { return mysqlKeyringKind(t.Frame) }

// xbKeyringMode tells the swap script where the restoring member's keyring is: the keyring_file
// plugin is named by its data file, a component by its config in plugin_dir, and the
// keyring_vault plugin needs nothing — xtrabackup reads keyring_vault_config from my.cnf.
func xbKeyringMode(kind, major string) string {
	shape := "plugin"
	if mysqlKeyringComponent(major) {
		shape = "component"
	}
	return kind + "-" + shape
}

// xbTransitionKey is the transition key a backup was taken with, from whichever member has it
// on record; "" when none does.
func (a *App) xbTransitionKey(t xbTarget, name string) string {
	for _, m := range t.Members {
		dep, err := a.store.GetDeployment(t.St.ID, m.Node.ID)
		if err != nil {
			continue
		}
		var sec struct {
			Keys map[string]string `json:"xbTransitionKeys"`
		}
		json.Unmarshal(dep.Secrets, &sec)
		if k := sec.Keys[name]; k != "" {
			return k
		}
	}
	return ""
}

// xbRecordTransitionKey keeps a backup's transition key on every member.
func (a *App) xbRecordTransitionKey(t xbTarget, name, key string) {
	for _, m := range t.Members {
		dep, err := a.store.GetDeployment(t.St.ID, m.Node.ID)
		if err != nil {
			continue
		}
		var sec struct {
			Keys map[string]string `json:"xbTransitionKeys"`
		}
		json.Unmarshal(dep.Secrets, &sec)
		if sec.Keys == nil {
			sec.Keys = map[string]string{}
		}
		sec.Keys[name] = key
		a.persistSecretKey(t.St, m.Node.ID, xbTransitionKeysField, sec.Keys)
	}
}

// xbForgetTransitionKeys drops the keys of deleted backups.
func (a *App) xbForgetTransitionKeys(t xbTarget, names map[string]bool) {
	for _, m := range t.Members {
		dep, err := a.store.GetDeployment(t.St.ID, m.Node.ID)
		if err != nil {
			continue
		}
		var sec struct {
			Keys map[string]string `json:"xbTransitionKeys"`
		}
		json.Unmarshal(dep.Secrets, &sec)
		changed := false
		for n := range names {
			if _, ok := sec.Keys[n]; ok {
				delete(sec.Keys, n)
				changed = true
			}
		}
		if changed {
			a.persistSecretKey(t.St, m.Node.ID, xbTransitionKeysField, sec.Keys)
		}
	}
}
