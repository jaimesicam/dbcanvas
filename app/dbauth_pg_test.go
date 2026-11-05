package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The keytab goes into the postgres user's home on each OS; /var/lib/pgsql does not exist on
// Ubuntu.
func TestPGKeytabPath(t *testing.T) {
	if got := pgKeytabPath("ubuntu"); got != "/var/lib/postgresql/dbcanvas.keytab" {
		t.Errorf("ubuntu: %s", got)
	}
	if got := pgKeytabPath("oraclelinux"); got != "/var/lib/pgsql/dbcanvas.keytab" {
		t.Errorf("oraclelinux: %s", got)
	}
}

// pgDirAuthScript edits the pg_hba.conf in CONFDIR — on Ubuntu /etc/postgresql/<major>/main,
// which its old /var/lib-only search never found — and refuses rather than guessing when there
// is none.
func TestPGDirAuthScriptFindsTheConfigDirectory(t *testing.T) {
	head := strings.SplitN(pgDirAuthScript, "\nsed -i", 2)[0] + "\necho \"HBA=$HBA CONF=$CONF\""
	dir := filepath.Join(t.TempDir(), "etc", "postgresql", "17", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "pg_hba.conf"), []byte("local all all peer\n"), 0o644)

	cmd := exec.Command("/bin/sh", "-c", head)
	cmd.Env = append(os.Environ(), "CONFDIR="+dir)
	out, err := cmd.CombinedOutput()
	want := "HBA=" + dir + "/pg_hba.conf CONF=" + dir + "/postgresql.conf"
	if err != nil || strings.TrimSpace(string(out)) != want {
		t.Errorf("with CONFDIR: %v %q, want %q", err, out, want)
	}

	cmd = exec.Command("/bin/sh", "-c", head)
	cmd.Env = append(os.Environ(), "CONFDIR="+t.TempDir()) // no pg_hba.conf there, none in /etc here
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "pg_hba.conf not found") {
		t.Errorf("without a pg_hba.conf: %v %q", err, out)
	}
}
