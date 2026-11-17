package main

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"os"
	"sort"
)

// nodepasswords.go — the passwords of the accounts DBCanvas creates on nodes: MySQL's root and
// admin, PostgreSQL's, MongoDB's, Valkey's, PMM's, the VNC desktop's, and the rest
// (exampleSecrets, doctor.go). Each is read from its environment variable at deploy.
//
// The app container took them from its .env, and fell back to well-known values such as
// "root_password". The dbcanvas command has no .env: on its first run it generates a random
// password for each, keeps them sealed in the database (node_passwords, encryption.go), and
// sets them for every deploy. They are never generated again — running nodes use them — and a
// setting a later release adds gets its own when it first starts. The environment, and the
// env: section of ~/.dbcanvas/config.yaml, still win: a value set there is used instead, for
// the nodes built after it. `dbcanvas passwords` shows what each is and where it comes from.

// nodePasswordLen is a generated password's length; VNC uses only the first 8 characters, so
// its password is 8 long and is the one that works.
func nodePasswordLen(name string) int {
	if name == "VNC_PASSWORD" {
		return 8
	}
	return 20
}

const pwLower, pwUpper, pwDigits = "abcdefghijkmnopqrstuvwxyz", "ABCDEFGHJKLMNPQRSTUVWXYZ", "23456789"

// newNodePassword is letters and digits only, with at least one of each kind: safe inside
// SQL, shell and config files the provisioning scripts write it into, and complex enough for
// Samba's policy.
func newNodePassword(n int) string {
	pick := func(set string) byte {
		i, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[i.Int64()]
	}
	all := pwLower + pwUpper + pwDigits
	b := []byte{pick(pwLower), pick(pwUpper), pick(pwDigits)}
	for len(b) < n {
		b = append(b, pick(all))
	}
	for i := len(b) - 1; i > 0; i-- { // the three guaranteed kinds anywhere
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		b[i], b[j.Int64()] = b[j.Int64()], b[i]
	}
	return string(b[:n])
}

// NodePassword reads one generated password.
func (s *Store) NodePassword(name string) (string, bool) {
	var v string
	if s.db.QueryRow(`SELECT value FROM node_passwords WHERE name = ?`, name).Scan(&v) != nil {
		return "", false
	}
	pw, err := s.openVal("node_passwords.value:"+name, v)
	return pw, err == nil
}

// SetNodePassword keeps one generated password, sealed.
func (s *Store) SetNodePassword(name, pw string) error {
	sealed, err := s.sealVal("node_passwords.value:"+name, pw)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO node_passwords (name, value) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value`, name, sealed)
	return err
}

// nodePasswordNames is every node password setting, sorted.
func nodePasswordNames() []string {
	out := make([]string, 0, len(exampleSecrets))
	for k := range exampleSecrets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// passwordsSet are the settings the environment or config.yaml set, found before any was
// filled in from the database.
var passwordsSet = map[string]bool{}

// applyNodePasswords sets each node password setting the environment and config.yaml leave
// unset from the generated one, generating it the first time.
func (a *App) applyNodePasswords() error {
	for _, name := range nodePasswordNames() {
		if _, set := os.LookupEnv(name); set {
			passwordsSet[name] = true
			continue
		}
		pw, ok := a.store.NodePassword(name)
		if !ok {
			pw = newNodePassword(nodePasswordLen(name))
			if err := a.store.SetNodePassword(name, pw); err != nil {
				return err
			}
		}
		os.Setenv(name, pw)
	}
	return nil
}

// nodePasswordInfo is one setting as `dbcanvas passwords` shows it.
type nodePasswordInfo struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Source   string `json:"source"` // generated | config (config.yaml or the environment)
	Replaced bool   `json:"replaced,omitempty"`
}

// handleNodePasswords answers `dbcanvas passwords` on the control socket.
func (a *App) handleNodePasswords(w http.ResponseWriter, r *http.Request) {
	if p, ok := principalOf(r); !ok || !p.Socket {
		writeErr(w, http.StatusForbidden, "the node passwords are given on the control socket only")
		return
	}
	out := []nodePasswordInfo{}
	for _, name := range nodePasswordNames() {
		info := nodePasswordInfo{Name: name, Value: os.Getenv(name), Source: "generated"}
		if passwordsSet[name] {
			info.Source = "config"
			// A generated one that nodes may have been built with before the override.
			if gen, ok := a.store.NodePassword(name); ok && gen != info.Value {
				info.Replaced = true
			}
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"passwords": out})
}
