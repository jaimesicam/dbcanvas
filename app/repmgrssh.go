package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// repmgrssh.go — passwordless SSH between the members of a repmgr cluster, which is what
// `repmgr standby switchover` needs and nothing else in DBCanvas does.
//
// Streaming replication, cloning and repmgrd's automatic failover all work over the PostgreSQL
// protocol, so a repmgr cluster deployed without any of this is healthy and fails over on its
// own. Switchover is the one operation that is not a database operation: it has to stop
// PostgreSQL on the *other* machine, and there is no SQL for that. So repmgr shells out, and a
// cluster without SSH stops at
//
//	NOTICE: checking switchover on node "repmgr03" (ID: 3) in --dry-run mode
//	WARNING: unable to connect to remote host "repmgr01.example.net" via SSH
//	ERROR: unable to connect via SSH to host "repmgr01.example.net", user ""
//
// which is a dead end in a lab whose whole point is rehearsing that operation.
//
// ---------------------------------------------------------------- what repmgr actually runs
//
// The check behind that error is exact, and worth pinning down rather than guessing at, because
// every requirement below falls out of it (repmgr-client.c, test_ssh_connection):
//
//	ssh -o Batchmode=yes <ssh_options> <host> /bin/true
//
// `Batchmode=yes` means **no prompts of any kind**: not for a password, not to accept a host
// key. So the connection has to be key-based and the host key already trusted, or it fails
// without ever asking. The empty `user ""` in the error is not a missing setting — it is
// repmgr's `--remote-user` command-line option, unset, which makes ssh use the OS user repmgr
// is running as. That is `postgres`, because repmgr.conf is postgres-owned and repmgr must run
// as the data directory's owner. So `postgres` is the account that needs the key, the login
// shell and the sudo rights — not root.
//
// ---------------------------------------------------------------- and what switchover does next
//
// Passing the SSH check only gets repmgr to the part where it stops the old primary. On these
// images PostgreSQL is a systemd unit, so letting repmgr fall back to its built-in `pg_ctl stop`
// would have systemd restart what it just stopped. repmgr covers this with the
// `service_*_command` settings, which it runs verbatim — locally and, during switchover, over
// the SSH connection above. They are set in repmgrConf, and they are why postgres also gets a
// scoped sudoers entry: a `systemctl stop` from a non-root user is otherwise a password prompt,
// and Batchmode has already ruled those out.
//
// ---------------------------------------------------------------- one key per cluster
//
// The keypair is generated once per deploy and pushed to every member, with the public half in
// every member's own authorized_keys — including its own. That last part is not redundant:
// switchover runs in whichever direction the operator chooses, so every ordered pair of members
// has to work, and a cluster of three that can only SSH one way fails the half of the exercise
// somebody is most likely to try second.
//
// The private key is deliberately not persisted anywhere outside the nodes. It authorises
// nothing but postgres-to-postgres inside one disposable lab cluster, a redeploy generates a
// fresh one for every member at once, and keeping it in the stack's secrets would make it
// readable from the API for no gain.

// repmgrSSHKeyFile is the postgres user's private key, in the default filename ssh looks for —
// so the ssh command repmgr builds needs no `-i`, which matters because repmgr's ssh_options
// are shared with commands DBCanvas does not construct.
const repmgrSSHKeyFile = "id_ed25519"

// repmgrSSHOptions is what goes into repmgr.conf's `ssh_options`. repmgr's own default is
// `-q -o ConnectTimeout=10`; the addition is host-key handling.
//
// `accept-new` trusts an unknown host on first contact and records it, but still refuses a host
// whose key has *changed* — so it keeps the protection that matters (a member replaced under
// you) and drops the one that cannot work under Batchmode (an interactive yes/no on a cluster
// that has never been connected to). Pre-seeding known_hosts with ssh-keyscan would be the
// alternative; it needs every sshd up before any key is written, and buys nothing here because
// the host keys are generated on nodes this deploy just created.
const repmgrSSHOptions = "-q -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"

// repmgrSSHKeypair generates the cluster's ed25519 keypair, returned as the OpenSSH private key
// file and the single-line authorized_keys entry.
//
// ed25519 rather than RSA: the key is generated on every deploy and a 4096-bit RSA keygen is
// seconds of CPU per cluster for no benefit, since both ends are ours and both are modern
// OpenSSH.
func repmgrSSHKeypair(cluster string) (priv, pub string, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	comment := "repmgr@" + cluster
	blk, err := ssh.MarshalPrivateKey(privKey, comment)
	if err != nil {
		return "", "", fmt.Errorf("marshal private key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", "", fmt.Errorf("marshal public key: %w", err)
	}
	// MarshalAuthorizedKey ends with a newline; the comment is appended so `ssh-add -l` and a
	// glance at authorized_keys both say which cluster the key belongs to.
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment + "\n"
	return string(pem.EncodeToMemory(blk)), line, nil
}

// repmgrAllowUserLogins removes the one obstacle that a working sshd, a valid key and correct
// permissions do not get past.
//
// The systemd base images trim `multi-user.target.wants` down to almost nothing, and one of the
// units that goes with it is **systemd-user-sessions.service** — whose entire job is deleting
// `/run/nologin` once boot has finished. It never runs, the file written at boot stays, and PAM
// refuses every non-root login for the life of the container:
//
//	"System is booting up. Unprivileged users are not permitted to log in yet.
//	 Please come back later. For technical details, see pam_nologin(8)."
//
// which ssh reports as `Connection closed` and repmgr reports as `unable to connect via SSH`.
// Nothing about the key, the sshd config or the file modes is wrong, so it is a long way to
// look for a file that says exactly what is happening in a log nobody has opened.
//
// The unit is `static` — no [Install] section — so `systemctl enable` cannot help; it is
// normally pulled in by multi-user.target, which is the link the image removed. `add-wants`
// restores exactly that link, under /etc, so it survives a container restart (when /run is a
// fresh tmpfs and the file comes back). Starting it as well makes this deploy work now rather
// than after a restart. Removing the file by hand is the fallback, for an image whose unit is
// genuinely absent.
const repmgrAllowUserLogins = `
if systemctl cat systemd-user-sessions.service >/dev/null 2>&1; then
  systemctl add-wants multi-user.target systemd-user-sessions.service >/dev/null 2>&1 || true
  systemctl start systemd-user-sessions.service >/dev/null 2>&1 || rm -f /run/nologin
else
  rm -f /run/nologin
fi
[ -e /run/nologin ] && { echo "/run/nologin still present — non-root SSH logins will be refused"; exit 1; }
`

// repmgrSSHInstallRHEL installs and starts the SSH server. The EL images already carry
// openssh-server and openssh-clients (the Debian ones carry neither), but `sudo` is absent from
// both, and installing what is already present costs nothing and keeps the two scripts honest
// about what they depend on.
//
// `ssh-keygen -A` is what actually makes sshd startable: the image has the package but no host
// keys, because those are generated by the postinstall on a real install and the image is built
// from a package cache.
const repmgrSSHInstallRHEL = `set -e
dnf -y -q install openssh-server openssh-clients sudo >/dev/null
ssh-keygen -A >/dev/null 2>&1 || true
systemctl enable --now sshd >/dev/null 2>&1
systemctl is-active --quiet sshd || { echo "sshd failed to start:"; journalctl -u sshd --no-pager 2>/dev/null | tail -10; exit 1; }
` + repmgrAllowUserLogins + `
echo "sshd running, user logins permitted"`

// repmgrSSHInstallDebian is the same for the Debian family, where the unit is `ssh` rather than
// `sshd` and the client package is openssh-client (singular).
const repmgrSSHInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq openssh-server openssh-client sudo >/dev/null 2>&1 || {
  apt-get update -qq >/dev/null; apt-get install -y -qq openssh-server openssh-client sudo >/dev/null; }
systemctl enable --now ssh >/dev/null 2>&1
systemctl is-active --quiet ssh || { echo "ssh failed to start:"; journalctl -u ssh --no-pager 2>/dev/null | tail -10; exit 1; }
` + repmgrAllowUserLogins + `
echo "sshd running, user logins permitted"`

// repmgrSSHKeyScript installs the cluster keypair for the postgres user.
//
// The login shell is set explicitly. A postgres account with /sbin/nologin accepts the key and
// then closes the connection, which `ssh … /bin/true` reports as a plain failure — the same
// symptom as a missing key, from a completely different cause.
const repmgrSSHKeyScript = `set -e
usermod -s /bin/bash postgres 2>/dev/null || true
install -d -o postgres -g postgres -m 0700 "$HOME/.ssh"
printf '%s' "$PRIV" > "$HOME/.ssh/` + repmgrSSHKeyFile + `"
printf '%s' "$PUB"  > "$HOME/.ssh/` + repmgrSSHKeyFile + `.pub"
# Rewritten rather than appended: a redeploy issues a new cluster key, and appending would
# leave every previous deploy's key authorised on a node that no longer shares its private half.
printf '%s' "$PUB" > "$HOME/.ssh/authorized_keys"
chown -R postgres:postgres "$HOME/.ssh"
chmod 0600 "$HOME/.ssh/` + repmgrSSHKeyFile + `" "$HOME/.ssh/authorized_keys"
chmod 0644 "$HOME/.ssh/` + repmgrSSHKeyFile + `.pub"
echo "postgres SSH key installed"`

// repmgrSudoersScript grants postgres exactly the systemctl calls repmgr's service_*_command
// settings make, and nothing else.
//
// Scoped to the units rather than granting `systemctl` wholesale: this is a lab, but a sudoers
// line is the kind of thing that gets copied out of one, and "postgres may restart the database"
// is a defensible rule where "postgres may control systemd" is not. Both repmgrd unit spellings
// are listed because the packaged name differs by platform (repmgr-<major> on EL, repmgrd on
// Debian) and repmgrdStartScript picks whichever exists at run time.
//
// `visudo -cf` before installing: a malformed file in /etc/sudoers.d breaks sudo for every user
// on the node, including the root shell somebody would use to fix it.
const repmgrSudoersScript = `set -e
F=/etc/sudoers.d/repmgr
cat > "$F.tmp" <<EOF
Defaults:postgres !requiretty
postgres ALL=(root) NOPASSWD: /usr/bin/systemctl start $UNIT, /usr/bin/systemctl stop $UNIT, /usr/bin/systemctl restart $UNIT, /usr/bin/systemctl reload $UNIT, /usr/bin/systemctl start $RUNIT, /usr/bin/systemctl stop $RUNIT, /usr/bin/systemctl restart $RUNIT, /usr/bin/systemctl start repmgrd, /usr/bin/systemctl stop repmgrd, /usr/bin/systemctl restart repmgrd
EOF
chmod 0440 "$F.tmp"
visudo -cf "$F.tmp" >/dev/null || { echo "generated sudoers file is invalid:"; cat "$F.tmp"; rm -f "$F.tmp"; exit 1; }
mv "$F.tmp" "$F"
echo "sudoers: postgres may systemctl $UNIT / $RUNIT"`

// repmgrSSHVerifyScript proves the thing the deploy is actually for, from this node to one
// peer: the exact command repmgr runs, as the user repmgr runs it as.
const repmgrSSHVerifyScript = `set -e
runuser -u postgres -- ssh -o Batchmode=yes ` + repmgrSSHOptions + ` "$PEER" /bin/true
echo "ssh postgres@$PEER ok"`

// repmgrWireSSH gives every member of a repmgr cluster passwordless SSH to every other, and the
// sudo rights repmgr's service_*_command settings need.
//
// Never fatal. A cluster without this is a working cluster that cannot do switchover — the
// replication, the failover and the backups are all untouched — so a failure here is logged
// against the node it happened on and the deploy carries on. The panel's repmgr tab says what
// was set up, so the gap is visible where somebody would go looking for the command.
func (a *App) repmgrWireSSH(ctx context.Context, st Stack, frame designFrame, members []designNode, fqdns map[string]string, major string) {
	if len(members) < 2 {
		return // one node has nobody to switch over with
	}
	priv, pub, err := repmgrSSHKeypair(frame.Label)
	if err != nil {
		a.pxcNewProg(st.ID, members[0].ID).logln("SSH between members skipped: " + err.Error())
		return
	}
	install := repmgrSSHInstallRHEL
	if isDebianOS(frame.OS) {
		install = repmgrSSHInstallDebian
	}
	home := pgHome(frame.OS)
	unit := pgServiceName(frame.OS, major)
	runit := "repmgr-" + ppgMajorOf(major)

	ok := make(map[string]bool, len(members))
	for _, n := range members {
		pr := a.pxcNewProg(st.ID, n.ID)
		pr.phase("Wiring SSH between members", 70)
		dep, derr := a.store.GetDeployment(st.ID, n.ID)
		if derr != nil || dep.ContainerID == "" {
			pr.logln("SSH setup skipped: the node has no container")
			continue
		}
		if err := a.runStep(ctx, dep.ContainerID, install, nil, pr.logln); err != nil {
			pr.logln("SSH server not started, so `repmgr standby switchover` will not work from or to this node: " + err.Error())
			continue
		}
		if err := a.runStep(ctx, dep.ContainerID, repmgrSSHKeyScript,
			[]string{"HOME=" + home, "PRIV=" + priv, "PUB=" + pub}, pr.logln); err != nil {
			pr.logln("SSH key not installed, so switchover will not work from or to this node: " + err.Error())
			continue
		}
		if err := a.runStep(ctx, dep.ContainerID, repmgrSudoersScript,
			[]string{"UNIT=" + unit, "RUNIT=" + runit}, pr.logln); err != nil {
			// Worth its own line: SSH works, so the check passes and the switchover then dies
			// later, stopping PostgreSQL — which is a much more confusing place to fail.
			pr.logln("sudo rights for postgres not granted, so switchover will reach the point of stopping PostgreSQL and fail there: " + err.Error())
		}
		ok[n.ID] = true
	}

	// Verify in the direction that matters most — every member to the initial primary, which is
	// the peer a first switchover names. A cluster that logs "ok" here has had the real command
	// run against it, not merely the files written.
	primary := members[0]
	for _, n := range members[1:] {
		if !ok[n.ID] || !ok[primary.ID] {
			continue
		}
		dep, derr := a.store.GetDeployment(st.ID, n.ID)
		if derr != nil || dep.ContainerID == "" {
			continue
		}
		pr := a.pxcNewProg(st.ID, n.ID)
		if err := a.runStep(ctx, dep.ContainerID, repmgrSSHVerifyScript,
			[]string{"PEER=" + fqdns[primary.ID]}, pr.logln); err != nil {
			pr.logln("SSH to " + fqdns[primary.ID] + " did not work, so switchover from this node will not: " + err.Error())
		}
	}
}
