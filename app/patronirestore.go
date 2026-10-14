package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// patronirestore.go — restore and point-in-time recovery for a Patroni cluster, from the
// pgBackRest repository it backs up to. The same three choices as the standalone node
// (pgrestore.go): everything archived, a moment, or one backup set.
//
// A cluster cannot be restored the way a node is. Restore the leader under a running Patroni
// and Patroni treats the rewound member as one that fell behind — it rejoins it as a replica of
// whoever holds the lock, or rewinds it forward again. And the replicas cannot simply be
// restored too: one built from a backup newer than the target is already past the point the
// cluster went back to, and can never follow the new timeline. So, the procedure that was run
// by hand on a live cluster before this was written:
//
//  1. Stop Patroni on every member, replicas first. PostgreSQL stops with it.
//  2. Remove the cluster from etcd (patronictl remove), so nothing tells the restored leader to
//     rejoin anybody.
//  3. On the leader: pgBackRest --delta restore with the target, then PostgreSQL started by hand
//     — outside Patroni — until recovery finishes and it promotes. Then stopped.
//  4. Start Patroni there. It finds a data directory and an empty cluster in etcd, and takes the
//     lock as leader of a new cluster on the new timeline.
//  5. A full backup, so the repository's newest backup is on that timeline.
//  6. Each replica: data directory emptied, Patroni started. It rebuilds the member with its
//     create_replica_methods — pgBackRest from the backup just taken, then streaming.
//
// Every step is a member's own script, run in order and logged; a failure stops the procedure
// and says which step, on which member — by then the cluster is down, and the log is what
// somebody needs to finish it by hand.

// patroniRestoreTarget is the frame and its running members, leader first.
type patroniRestoreTarget struct {
	st      Stack
	frame   designFrame
	leader  memberRef
	members []memberRef // replicas, in design order
}

type memberRef struct{ label, container string }

func (a *App) patroniRestoreFrame(w http.ResponseWriter, r *http.Request) (patroniRestoreTarget, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return patroniRestoreTarget{}, false
	}
	fid := r.PathValue("fid")
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return patroniRestoreTarget{}, false
	}
	var frame designFrame
	found := false
	for _, f := range doc.Frames {
		if f.ID == fid && f.Type == "patroni" {
			frame, found = f, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "Patroni cluster not found")
		return patroniRestoreTarget{}, false
	}
	if !frame.UsePgBackRest {
		writeErr(w, http.StatusBadRequest, "pgBackRest is not enabled for this cluster")
		return patroniRestoreTarget{}, false
	}
	t := patroniRestoreTarget{st: st, frame: frame}
	leaderCID := a.patroniLeaderContainer(r.Context(), st, frame, doc)
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "patroni" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			writeErr(w, http.StatusConflict, n.Label+" is not running — every member has to be, to restore the cluster")
			return patroniRestoreTarget{}, false
		}
		m := memberRef{n.Label, dep.ContainerID}
		if dep.ContainerID == leaderCID {
			t.leader = m
		} else {
			t.members = append(t.members, m)
		}
	}
	if t.leader.container == "" {
		writeErr(w, http.StatusConflict, "no running leader found for this cluster")
		return patroniRestoreTarget{}, false
	}
	return t, true
}

// handlePatroniBackups lists the cluster's backup sets, read on the leader.
func (a *App) handlePatroniBackups(w http.ResponseWriter, r *http.Request) {
	t, ok := a.patroniRestoreFrame(w, r)
	if !ok {
		return
	}
	out, err := a.pgBackupList(r.Context(), t.leader.container, patroniStanza(t.frame.Label))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePatroniRestore runs the procedure in the file comment.
func (a *App) handlePatroniRestore(w http.ResponseWriter, r *http.Request) {
	t, ok := a.patroniRestoreFrame(w, r)
	if !ok {
		return
	}
	var req pgRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	args, err := pgRestoreArgs(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	f := t.frame
	major := ppgMajorOf(f.PGMajor)
	stanza := patroniStanza(f.Label)
	data := pgDataDir(f.OS, major)
	var log []string
	step := func(m memberRef, what, script string, env []string) error {
		res, err := a.engCtx(ctx).Exec(ctx, m.container, []string{"bash", "-c", script}, env)
		out := ""
		if res.Stdout != "" || res.Stderr != "" {
			out = strings.TrimSpace(res.Stdout + res.Stderr)
		}
		if err == nil && res.Code != 0 {
			err = fmt.Errorf("exit %d", res.Code)
		}
		line := m.label + ": " + what
		if out != "" {
			line += "\n" + out
		}
		log = append(log, line)
		if err != nil {
			return fmt.Errorf("%s on %s failed (%v): %s", what, m.label, err, lastLines(out, 600))
		}
		return nil
	}
	fail := func(err error) {
		a.notifyStack(t.st.ID, "restore.failed", "error", "Restore failed", f.Label+": "+err.Error(), "")
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error() + " — the cluster is stopped part-way; the log says how far it got",
			"log":   strings.Join(log, "\n\n"),
		})
	}

	// 1. Stop every member, replicas first, so none of them is promoted when the leader goes.
	for _, m := range append(append([]memberRef{}, t.members...), t.leader) {
		if err := step(m, "stop Patroni", patroniStopScript, nil); err != nil {
			fail(err)
			return
		}
	}
	// 2. Forget the cluster in etcd.
	if err := step(t.leader, "remove the cluster from etcd", patroniForgetScript,
		[]string{"SCOPE=" + sanitizeName(f.Label), "CONF=/etc/patroni/postgresql.yml"}); err != nil {
		fail(err)
		return
	}
	// 3. Restore and recover the leader outside Patroni.
	if err := step(t.leader, "pgBackRest restore and recovery", patroniRestoreLeaderScript, []string{
		"STANZA=" + stanza, "BINDIR=" + pgBinDir(f.OS, major), "DATA=" + data, "ARGS=" + strings.Join(args, "\n"),
	}); err != nil {
		fail(err)
		return
	}
	// 4. Patroni takes the lock on the restored data.
	if err := step(t.leader, "start Patroni as leader", patroniStartLeaderScript, nil); err != nil {
		fail(err)
		return
	}
	// 5. A backup on the new timeline, for the replicas to be built from.
	if err := step(t.leader, "full backup on the new timeline", patroniRestoreBackupScript, []string{"STANZA=" + stanza}); err != nil {
		fail(err)
		return
	}
	// 6. Rebuild each replica from it.
	for _, m := range t.members {
		if err := step(m, "rebuild as a replica", patroniRebuildReplicaScript, []string{"DATA=" + data}); err != nil {
			fail(err)
			return
		}
	}

	what := map[string]string{"": "the latest point", "latest": "the latest point", "time": req.Time, "backup": "backup " + req.Set}[req.Mode]
	a.notifyStack(t.st.ID, "restore.done", "success", "Restore completed",
		fmt.Sprintf("%s: restored to %s; %s leads, %d replica(s) rebuilt.", f.Label, what, t.leader.label, len(t.members)), "")
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "restoredTo": what, "leader": t.leader.label, "log": strings.Join(log, "\n\n"),
	})
}

// patroniStopScript stops Patroni, which stops PostgreSQL, and waits until it has.
const patroniStopScript = `set -e
systemctl stop patroni
for i in $(seq 1 60); do
  pgrep -u postgres -x postgres >/dev/null || pgrep -u postgres -f 'bin/postgres' >/dev/null || { echo "stopped"; exit 0; }
  sleep 1
done
echo "PostgreSQL is still running after Patroni stopped"; exit 1`

// patroniForgetScript drops the cluster from etcd. patronictl asks twice — the cluster name, then
// a sentence — so both answers go in on stdin. Env: SCOPE, CONF.
const patroniForgetScript = `set -e
printf '%s\nYes I am aware\n' "$SCOPE" | patronictl -c "$CONF" remove "$SCOPE" >/dev/null 2>&1 || true
patronictl -c "$CONF" list 2>/dev/null | grep -q "(uninitialized)" || {
  echo "the cluster is still in etcd:"; patronictl -c "$CONF" list 2>&1 | tail -5; exit 1; }
echo "removed from etcd"`

// patroniRestoreLeaderScript restores the data directory and runs recovery to the target with
// PostgreSQL started by hand, then stops it — Patroni starts it again in the next step.
// Env: STANZA, BINDIR, DATA, ARGS (newline-separated, as pgRestoreScript).
const patroniRestoreLeaderScript = `set -e
mapfile -t extra < <(printf '%s' "$ARGS" | sed '/^$/d')
runuser -u postgres -- pgbackrest --stanza="$STANZA" --delta --log-level-console=info "${extra[@]}" restore 2>&1 |
  grep -E "INFO: (restore backup set|restore size|restore command end)|WARN|ERROR" || true
test "${PIPESTATUS[0]}" -eq 0
runuser -u postgres -- "$BINDIR/pg_ctl" -D "$DATA" -l /tmp/pg-restore.log -w -t 600 start >/dev/null
r=""
for i in $(seq 1 600); do
  r=$(runuser -u postgres -- "$BINDIR/psql" -qAtX -h /var/run/postgresql -c 'SELECT pg_is_in_recovery()' 2>/dev/null || true)
  [ "$r" = "f" ] && break
  sleep 1
done
[ "$r" = "f" ] || { echo "recovery did not finish:"; tail -15 /tmp/pg-restore.log; exit 1; }
runuser -u postgres -- "$BINDIR/psql" -qAtX -h /var/run/postgresql -c "SELECT 'recovered — timeline ' || timeline_id FROM pg_control_checkpoint()"
runuser -u postgres -- "$BINDIR/pg_ctl" -D "$DATA" -m fast -w stop >/dev/null`

// patroniStartLeaderScript starts Patroni and waits for it to hold the leader lock.
const patroniStartLeaderScript = `set -e
systemctl start patroni
for i in $(seq 1 180); do
  curl -fsS -o /dev/null http://127.0.0.1:8008/leader 2>/dev/null && { echo "leader"; exit 0; }
  sleep 1
done
echo "Patroni did not take the leader lock:"; journalctl -u patroni --no-pager 2>/dev/null | tail -15; exit 1`

// patroniRebuildReplicaScript empties the data directory and starts Patroni, which rebuilds the
// member from the new leader's timeline, then waits for it to stream. Env: DATA.
const patroniRebuildReplicaScript = `set -e
find "$DATA" -mindepth 1 -delete
systemctl start patroni
for i in $(seq 1 900); do
  s=$(curl -fsS http://127.0.0.1:8008/patroni 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("state",""), d.get("replication_state",""))' 2>/dev/null || true)
  case "$s" in "running streaming"*) echo "streaming"; exit 0 ;; esac
  sleep 1
done
echo "the replica did not start streaming:"; journalctl -u patroni --no-pager 2>/dev/null | tail -15; exit 1`

// patroniRestoreBackupScript is patroniBackupNowScript reduced, in the restore's log, to the
// backup's label — pgBackRest's console output is fifteen lines of options and archive checks.
// Env: STANZA.
const patroniRestoreBackupScript = `set -o pipefail
runuser -u postgres -- pgbackrest --stanza="$STANZA" --type=full --log-level-console=info backup 2>&1 |
  grep -E "new backup label|ERROR" || true
test "${PIPESTATUS[0]}" -eq 0`
