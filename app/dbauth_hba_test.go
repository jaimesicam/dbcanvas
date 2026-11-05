package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pgHBAEditPart is the part of pgDirAuthScript that inserts the rules into pg_hba.conf.
func pgHBAEditPart(t *testing.T) string {
	t.Helper()
	i := strings.Index(pgDirAuthScript, "ADD=$(mktemp)")
	j := strings.Index(pgDirAuthScript, "su - postgres")
	if i < 0 || j < i {
		t.Fatal("the pg_hba edit is not where this test expects it")
	}
	return pgDirAuthScript[i:j]
}

// The rules go in front of the first catch-all scram rule (pg_hba is first-match-wins), or at
// the top when there is none, byte for byte — a bind password with quotes, a backslash, `$` and
// `&` included. It runs with the host's awk; the node images' gawk and mawk run it at deploy.
func TestPGHBAEditPlacesTheRules(t *testing.T) {
	ldap := `host all all 0.0.0.0/0 ldap ldapserver=intranet.example.net ldapbindpasswd="p\a'$x&y" # dbcanvas-dirauth`
	su := "host all postgres 0.0.0.0/0 scram-sha-256 # dbcanvas-dirauth"
	for _, c := range []struct{ name, in, want string }{
		{"before the catch-all",
			"local all all peer\nhost all all 0.0.0.0/0 scram-sha-256\nhost all all ::/0 scram-sha-256\n",
			"local all all peer\n" + su + "\n" + ldap + "\nhost all all 0.0.0.0/0 scram-sha-256\nhost all all ::/0 scram-sha-256\n"},
		{"no catch-all: at the top",
			"local all all peer\nhostssl all all 0.0.0.0/0 cert\n",
			su + "\n" + ldap + "\nlocal all all peer\nhostssl all all 0.0.0.0/0 cert\n"},
	} {
		hba := filepath.Join(t.TempDir(), "pg_hba.conf")
		os.WriteFile(hba, []byte(c.in), 0o640)
		cmd := exec.Command("/bin/sh", "-c", "set -e\n"+pgHBAEditPart(t))
		cmd.Env = append(os.Environ(), "HBA="+hba, "SU="+su, "GSS=", "LDAPLINE="+ldap)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", c.name, err, out)
		}
		got, _ := os.ReadFile(hba)
		if string(got) != c.want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", c.name, got, c.want)
		}
		if fi, _ := os.Stat(hba); fi.Mode().Perm() != 0o640 {
			t.Errorf("%s: mode changed to %v", c.name, fi.Mode().Perm())
		}
	}
}
