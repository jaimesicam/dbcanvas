//go:build containertest

package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// keyring_container_test.go — the keyring-file stage and verify scripts, and the encryption
// options, run against real Percona Server binaries in a throwaway container: the 8.4 series
// with component_keyring_file, the 8.0 series with the keyring_file plugin. The unit tests check
// what the scripts say; this checks a server started the way DBCanvas starts one actually loads
// the keyring, writes a master key to it and turns the options on. (It is how a keyring file
// created root-owned by `mysqld --initialize` would have been caught before a deploy.)
//
// Needs Docker and the network: `make test-container`, or go test -tags containertest -run
// TestKeyringFileInContainer. CI runs it in its own job.

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func dockerExec(t *testing.T, id, script string, env ...string) string {
	t.Helper()
	args := []string{"exec"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, id, "bash", "-c", script)
	return dockerOut(t, args...)
}

func TestKeyringFileInContainer(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	enc := mysqlEncryption{EncryptBinlog: true, EncryptRedoUndo: true, EncryptTemp: true}
	for _, c := range []struct{ image, major string }{
		{"percona/percona-server:8.4", "8.4"},
		{"percona/percona-server:8.0", "8.0"},
	} {
		t.Run(c.major, func(t *testing.T) {
			dockerOut(t, "pull", "-q", c.image)
			raw, err := exec.Command("docker", "run", "-d", "--rm", "--user", "root", "--entrypoint", "sleep", c.image, "infinity").Output()
			if err != nil {
				t.Fatalf("docker run %s: %v", c.image, err)
			}
			id := strings.TrimSpace(string(raw))
			t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })

			mode, keyring := "plugin", mysqlKeyringFileData
			if mysqlKeyringComponent(c.major) {
				mode, keyring = "component", mysqlKeyringComponentData
				t.Log(dockerExec(t, id, mysqlKeyringFileStageComponentScript, "CONF="+mysqlKeyringFileComponentConf()))
			} else {
				t.Log(dockerExec(t, id, mysqlKeyringFileStagePluginScript))
			}

			// The server's config, as DBCanvas writes the keyring and encryption lines.
			cnf := "[mysqld]\nuser=mysql\ndatadir=/var/lib/mysql\nsocket=/var/lib/mysql/mysql.sock\n" +
				"log-error=/var/log/mysqld-test.log\npid-file=/var/lib/mysql/mysqld.pid\nlog-bin=binlog\nserver-id=1\n" +
				mysqlKeyringFileOptions(c.major, "") + mysqlEncryptionOptions(enc) +
				"[client]\nsocket=/var/lib/mysql/mysql.sock\n"
			dockerExec(t, id, `printf '%s' "$CNF" > /etc/my.cnf && rm -rf /var/lib/mysql/* && touch /var/log/mysqld-test.log && chown mysql:mysql /var/log/mysqld-test.log /var/lib/mysql`, "CNF="+cnf)
			// --initialize runs as root here, as it does on a node.
			dockerExec(t, id, `mysqld --initialize-insecure || { tail -20 /var/log/mysqld-test.log; exit 1; }`)
			dockerExec(t, id, `mysqld --daemonize >/dev/null || { tail -20 /var/log/mysqld-test.log; exit 1; }
for i in $(seq 1 60); do mysqladmin -u root ping >/dev/null 2>&1 && exit 0; sleep 1; done
tail -20 /var/log/mysqld-test.log; exit 1`)

			out := dockerExec(t, id, mysqlKeyringFileVerifyScript,
				"MODE="+mode, "KEYRING="+keyring, "ROOT_PW=", "LOGERR=/var/log/mysqld-test.log", "SMOKE=1")
			if !strings.Contains(out, "the master key is in "+keyring) {
				t.Errorf("verify did not confirm the master key:\n%s", out)
			}
			out = dockerExec(t, id, mysqlEncryptionVerifyScript,
				"VARS="+strings.Join(enc.variables(), " "), "ROOT_PW=")
			for _, v := range enc.variables() {
				if !strings.Contains(out, v+"=") {
					t.Errorf("%s was not confirmed:\n%s", v, out)
				}
			}
			// A restart reads the keyring back: the encrypted table must still open.
			dockerExec(t, id, `mysql -u root -e "CREATE DATABASE k; CREATE TABLE k.t (id INT PRIMARY KEY) ENCRYPTION='Y'; INSERT INTO k.t VALUES (1)"
mysqladmin -u root shutdown && sleep 2
mysqld --daemonize >/dev/null
for i in $(seq 1 60); do mysqladmin -u root ping >/dev/null 2>&1 && break; sleep 1; done
[ "$(mysql -u root -N -e 'SELECT COUNT(*) FROM k.t')" = 1 ] || { tail -20 /var/log/mysqld-test.log; exit 1; }`)
		})
	}
}
