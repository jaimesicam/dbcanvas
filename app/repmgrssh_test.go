package main

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The keypair has to be usable by OpenSSH at both ends — a private key file ssh will load and
// an authorized_keys line sshd will match — so it is parsed back with the same library rather
// than pattern-matched.
func TestRepmgrSSHKeypairRoundTrips(t *testing.T) {
	priv, pub, err := repmgrSSHKeypair("repmgr-cluster-01")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil {
		t.Fatalf("the private key is not one OpenSSH could load: %v", err)
	}
	parsed, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		t.Fatalf("the public half is not a valid authorized_keys line: %v", err)
	}
	// The two halves must belong together, or every node authorises a key nobody holds.
	if string(parsed.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Error("the authorized_keys line is not the public half of the private key")
	}
	if parsed.Type() != ssh.KeyAlgoED25519 {
		t.Errorf("want an ed25519 key, got %s", parsed.Type())
	}
	// The comment names the cluster, which is the only thing that identifies the key on a node.
	if comment != "repmgr@repmgr-cluster-01" {
		t.Errorf("comment = %q", comment)
	}
	// authorized_keys is line-oriented: a stray newline in the middle would split one key into
	// two invalid ones.
	if n := strings.Count(strings.TrimSuffix(pub, "\n"), "\n"); n != 0 {
		t.Errorf("the authorized_keys entry must be a single line, got %d breaks", n)
	}

	// Every cluster gets its own key — two deploys sharing one would let any lab node into any
	// other.
	priv2, _, err := repmgrSSHKeypair("repmgr-cluster-01")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if priv == priv2 {
		t.Error("two calls produced the same private key")
	}
}

// repmgr runs `ssh -o Batchmode=yes <ssh_options> <host> /bin/true`, so ssh_options is the only
// place host-key policy can be set — and under Batchmode an unknown host is a failure, not a
// prompt. accept-new is the setting that trusts a first contact while still refusing a host
// whose key has changed.
func TestRepmgrSSHOptionsHandleAnUnknownHost(t *testing.T) {
	if !strings.Contains(repmgrSSHOptions, "StrictHostKeyChecking=accept-new") {
		t.Errorf("ssh_options must accept a first-contact host key: %q", repmgrSSHOptions)
	}
	// `no` would also connect, and would keep connecting to a host whose key changed underneath
	// it — which is the one thing host-key checking is for.
	if strings.Contains(repmgrSSHOptions, "StrictHostKeyChecking=no") {
		t.Errorf("ssh_options must not disable host-key checking outright: %q", repmgrSSHOptions)
	}
	if !strings.Contains(repmgrSSHOptions, "ConnectTimeout=") {
		t.Errorf("ssh_options must bound the connect, or an unreachable peer hangs the deploy: %q", repmgrSSHOptions)
	}
}

// The base images trim multi-user.target.wants, which drops systemd-user-sessions.service —
// whose only job is removing /run/nologin. Without it PAM refuses every non-root login for the
// life of the container, and ssh reports it as a closed connection. The unit is `static`, so
// `systemctl enable` cannot restore it; add-wants is what re-creates the link, under /etc, so it
// survives the restart that re-creates /run as a fresh tmpfs.
func TestRepmgrSSHInstallClearsNologin(t *testing.T) {
	for name, script := range map[string]string{
		"RHEL":   repmgrSSHInstallRHEL,
		"Debian": repmgrSSHInstallDebian,
	} {
		if !strings.Contains(script, "add-wants multi-user.target systemd-user-sessions.service") {
			t.Errorf("%s: the nologin fix must be boot-persistent, not just started once", name)
		}
		if !strings.Contains(script, "rm -f /run/nologin") {
			t.Errorf("%s: no fallback for an image without the unit", name)
		}
		// Silence here is the failure mode that cost the most time, so the script asserts.
		if !strings.Contains(script, "[ -e /run/nologin ]") {
			t.Errorf("%s: must fail loudly if /run/nologin survives", name)
		}
		if !strings.Contains(script, "sudo") {
			t.Errorf("%s: sudo is needed for the service_*_command settings and is in neither image", name)
		}
	}
	// The unit names differ, and starting the wrong one leaves sshd down.
	if !strings.Contains(repmgrSSHInstallRHEL, "--now sshd") {
		t.Error("EL's unit is sshd")
	}
	if !strings.Contains(repmgrSSHInstallDebian, "--now ssh") {
		t.Error("Debian's unit is ssh")
	}
}

// Switchover stops PostgreSQL on the other node. Left to repmgr's built-in pg_ctl that is undone
// by systemd restarting the service, so the service_* commands must be set — and they must go
// through sudo, because repmgr runs as postgres and Batchmode has ruled out a password prompt.
func TestRepmgrConfCarriesSwitchoverSettings(t *testing.T) {
	frame := designFrame{OS: "oraclelinux", OSVersion: "9", PGMajor: "17"}
	conf := repmgrConf(3, "repmgr-3", "repmgr-3.example.net", frame, pgSecrets{ReplUser: "repmgr", ReplPassword: "pw"})

	for _, want := range []string{
		"ssh_options='" + repmgrSSHOptions + "'",
		"service_start_command='sudo /usr/bin/systemctl start postgresql-17'",
		"service_stop_command='sudo /usr/bin/systemctl stop postgresql-17'",
		"service_restart_command='sudo /usr/bin/systemctl restart postgresql-17'",
		"service_reload_command='sudo /usr/bin/systemctl reload postgresql-17'",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("repmgr.conf omits %q\n%s", want, conf)
		}
	}
	// The unit follows the OS, so a Debian cluster must not be told to stop an EL unit name.
	deb := repmgrConf(1, "repmgr-1", "repmgr-1.example.net",
		designFrame{OS: "debian", OSVersion: "12", PGMajor: "17"}, pgSecrets{})
	if strings.Contains(deb, "postgresql-17'") && !strings.Contains(deb, pgServiceName("debian", "17")) {
		t.Errorf("the Debian unit name is wrong:\n%s", deb)
	}
}

// The sudoers entry is scoped to the units repmgr names, not to systemctl wholesale, and is
// validated before it is installed — a malformed file in /etc/sudoers.d breaks sudo for every
// user on the node including the root shell somebody would use to fix it.
func TestRepmgrSudoersIsScopedAndChecked(t *testing.T) {
	if !strings.Contains(repmgrSudoersScript, "visudo -cf") {
		t.Error("the sudoers file must be syntax-checked before it is installed")
	}
	if !strings.Contains(repmgrSudoersScript, "$UNIT") || !strings.Contains(repmgrSudoersScript, "$RUNIT") {
		t.Error("the sudoers entry must name the units rather than granting systemctl wholesale")
	}
	if strings.Contains(repmgrSudoersScript, "NOPASSWD: ALL") {
		t.Error("postgres must not get blanket sudo")
	}
	// Installed atomically: a half-written file in sudoers.d is the same outage as a malformed one.
	if !strings.Contains(repmgrSudoersScript, `mv "$F.tmp" "$F"`) {
		t.Error("the sudoers file must be moved into place, not written in place")
	}
}

// The panel builds its commands from these, and a switchover names a peer — so the peer list has
// to exclude the node looking at it and keep the node_id the cluster was registered with.
func TestRepmgrPeersOf(t *testing.T) {
	members := []designNode{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	hosts := map[string]string{"a": "repmgr-1", "b": "repmgr-2", "c": "repmgr-3"}

	peers := repmgrPeersOf(members, hosts, "example.net", 2) // as seen by repmgr-3
	if len(peers) != 2 {
		t.Fatalf("want 2 peers, got %d", len(peers))
	}
	for _, p := range peers {
		if p.NodeName == "repmgr-3" {
			t.Error("a node must not list itself as a peer")
		}
	}
	// node_id is the 1-based member index, which is what repmgr.conf was written with.
	if peers[0].NodeID != 1 || peers[0].NodeName != "repmgr-1" || peers[0].FQDN != "repmgr-1.example.net" {
		t.Errorf("first peer = %+v", peers[0])
	}
	if peers[0].Role != "primary" || peers[1].Role != "standby" {
		t.Errorf("member 0 is the initial primary: %+v", peers)
	}
	// Seen from the primary, the peers are the two standbys.
	if got := repmgrPeersOf(members, hosts, "example.net", 0); len(got) != 2 || got[0].NodeID != 2 {
		t.Errorf("peers of member 0 = %+v", got)
	}
}
