package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"

	"dbcanvas/internal/seal"
)

// encryption.go — which columns are sealed, and the store's half of encryption at
// rest: the key check on startup, sealing what an older version left in plaintext,
// and key rotation. The format and the key file are internal/seal.
//
// Sealed are the columns that hold a credential, or something a guest typed:
//
//	users.password_hash        the bcrypt hash, so a copy of the database cannot be
//	                           cracked offline, nor a hash of one's own written in
//	stacks.design_json         the canvas, which carries every password typed into it
//	deployments.secrets_json   what provisioning generated: root, admin, VNC passwords
//	share_guests.name, email   what a guest typed at the join page
//	share_messages.author, body  the session transcript
//
// Not sealed, and why: stack_templates.design_json is sanitised of secrets on the way
// in (sanitizeTemplateDesign); deployments.config_json is by convention the
// non-secret profile; login sessions, API tokens, share links and guest cookies are
// stored as SHA-256 hashes, which need no key to look up.

// settingKeyID is the app_settings row naming the key the database is sealed with.
const settingKeyID = "encryption_key_id"

// settingSessionsHashed marks that sessions.token holds hashes. A token and its hash
// are both 64 hex characters, so the column cannot say which it holds by itself.
const settingSessionsHashed = "sessions_hashed"

// sealedColumn names a sealed column and the SQL expression that rebuilds its AAD,
// so the startup migration and key rotation can walk every value.
type sealedColumn struct {
	Table, Column, AAD string
}

var sealedColumns = []sealedColumn{
	{"users", "password_hash", `'users.password_hash:' || id`},
	{"stacks", "design_json", `'stacks.design_json:' || id`},
	{"deployments", "secrets_json", `'deployments.secrets_json:' || stack_id || ':' || node_id`},
	{"share_guests", "name", `'share_guests.name:' || session_id`},
	{"share_guests", "email", `'share_guests.email:' || session_id`},
	{"share_messages", "author", `'share_messages.author:' || session_id`},
	{"share_messages", "body", `'share_messages.body:' || session_id`},
}

func aadID(table, col string, id int64) string {
	return table + "." + col + ":" + strconv.FormatInt(id, 10)
}

func aadDeployment(stackID int64, nodeID string) string {
	return "deployments.secrets_json:" + strconv.FormatInt(stackID, 10) + ":" + nodeID
}

// sealVal and openVal are the store's shorthands. The empty string stays empty
// either way, and a failed open names the value it was.
func (s *Store) sealVal(aad, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	return s.seal.Seal(aad, v)
}

func (s *Store) openVal(aad, v string) (string, error) {
	pt, err := s.seal.Open(aad, v)
	if err != nil {
		return "", fmt.Errorf("%s: %w", aad, err)
	}
	return pt, nil
}

// setupEncryption loads (or creates) the key for the database at dbPath, refuses a
// key that is not the one the database was sealed with — starting anyway would leave
// every sealed value unreadable — and seals what is still plaintext.
func (s *Store) setupEncryption(dbPath string) error {
	kp := seal.KeyPath(dbPath)
	sl, created, err := seal.LoadOrCreate(kp)
	if err != nil {
		return err
	}
	want, err := s.AppSetting(settingKeyID)
	if err != nil {
		return err
	}
	switch {
	case want == "":
		if err := s.SetAppSetting(settingKeyID, sl.KeyID()); err != nil {
			return err
		}
		if created {
			log.Printf("encryption: generated a new key at %s (key id %s) — back it up; without it the database cannot be read", kp, sl.KeyID())
		}
	case want != sl.KeyID():
		if created {
			os.Remove(kp) // don't leave a wrong key where the right one belongs
			return fmt.Errorf("encryption key %s is missing, but this database was encrypted with key id %s — restore that key file (or set %s to it)", kp, want, seal.KeyPathEnv)
		}
		return fmt.Errorf("encryption key %s has id %s, but this database was encrypted with key id %s — restore the original key file", kp, sl.KeyID(), want)
	}
	s.seal = sl
	return s.migratePlaintext()
}

// migratePlaintext seals every value an older version stored in plaintext and hashes
// the login sessions it stored as-is, in one transaction. Idempotent: a sealed value
// carries the "v1:" prefix, which no plaintext these columns held ever had, and the
// sessions are hashed once, marked in app_settings.
func (s *Store) migratePlaintext() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n := 0
	for _, c := range sealedColumns {
		items, err := sealedRows(tx, c, `AND `+c.Column+` NOT LIKE 'v1:%'`)
		if err != nil {
			return err
		}
		for _, it := range items {
			v, err := s.seal.Seal(it.aad, it.val)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE `+c.Table+` SET `+c.Column+` = ? WHERE rowid = ?`, v, it.rowid); err != nil {
				return err
			}
			n++
		}
	}
	var hashed string
	err = tx.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, settingSessionsHashed).Scan(&hashed)
	if errors.Is(err, sql.ErrNoRows) {
		rows, err := tx.Query(`SELECT token FROM sessions`)
		if err != nil {
			return err
		}
		var toks []string
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			toks = append(toks, t)
		}
		rows.Close()
		for _, t := range toks {
			if _, err := tx.Exec(`UPDATE sessions SET token = ? WHERE token = ?`, seal.HashSecret(t), t); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO app_settings (key, value) VALUES (?, '1')`, settingSessionsHashed); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if n > 0 {
		log.Printf("encryption: sealed %d values left in plaintext by an earlier version", n)
	}
	return nil
}

type sealedRow struct {
	rowid    int64
	aad, val string
}

// sealedRows reads one sealed column's non-empty values, with the AAD each belongs to.
func sealedRows(tx *sql.Tx, c sealedColumn, extra string) ([]sealedRow, error) {
	rows, err := tx.Query(`SELECT rowid, ` + c.AAD + `, ` + c.Column + ` FROM ` + c.Table +
		` WHERE ` + c.Column + ` IS NOT NULL AND ` + c.Column + ` != '' ` + extra)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sealedRow
	for rows.Next() {
		var it sealedRow
		if err := rows.Scan(&it.rowid, &it.aad, &it.val); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Rotate re-seals every sealed value under next, in one transaction, and records
// next's key id. On error nothing changed.
func (s *Store) Rotate(next *seal.Sealer) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count := 0
	for _, c := range sealedColumns {
		items, err := sealedRows(tx, c, "")
		if err != nil {
			return 0, err
		}
		for _, it := range items {
			pt, err := s.seal.Open(it.aad, it.val)
			if err != nil {
				return 0, fmt.Errorf("%s.%s row %d: %w", c.Table, c.Column, it.rowid, err)
			}
			v, err := next.Seal(it.aad, pt)
			if err != nil {
				return 0, err
			}
			if _, err := tx.Exec(`UPDATE `+c.Table+` SET `+c.Column+` = ? WHERE rowid = ?`, v, it.rowid); err != nil {
				return 0, err
			}
			count++
		}
	}
	if _, err := tx.Exec(`INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, settingKeyID, next.KeyID()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.seal = next
	return count, nil
}

// rotateKey re-encrypts the database with a fresh key: `dbcanvas
// -rotate-encryption-key`, run while the server is stopped (make rotate-key). The new
// key is written beside the old one first; the database is re-sealed in one
// transaction; then the files are swapped, keeping the old key as <path>.old.
func rotateKey(dbPath string) error {
	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	kp := seal.KeyPath(dbPath)
	k, err := seal.GenerateKey()
	if err != nil {
		return err
	}
	next, err := seal.New(k)
	if err != nil {
		return err
	}
	if err := seal.WriteKeyFile(kp+".new", k); err != nil {
		return err
	}
	old := store.seal.KeyID()
	n, err := store.Rotate(next)
	if err != nil {
		os.Remove(kp + ".new")
		return fmt.Errorf("rotation failed, nothing changed: %w", err)
	}
	if err := os.Rename(kp, kp+".old"); err != nil {
		return fmt.Errorf("database re-sealed with key %s, but the old key file could not be moved aside — move %s.new to %s: %w", next.KeyID(), kp, kp, err)
	}
	if err := os.Rename(kp+".new", kp); err != nil {
		return fmt.Errorf("the new key is at %s.new — move it to %s: %w", kp, kp, err)
	}
	log.Printf("encryption: re-sealed %d values; key id %s → %s. The old key is kept at %s.old — delete it once the new one is backed up.",
		n, old, next.KeyID(), kp)
	return nil
}
