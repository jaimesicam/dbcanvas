package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// pgrestore.go — restore and point-in-time recovery for the standalone PostgreSQL node, from the
// pgBackRest repository it already backs up to.
//
// Backups existed without restores: the Backup tab could take one, and getting data back meant
// knowing pgBackRest. Restore is the half people actually come to a lab for — "can I get back to
// 14:32, before the DROP?" — so this is three choices, each one pgBackRest command:
//
//   - latest: replay every archived WAL segment, the state just before the restore.
//   - time:   stop replay at a timestamp (PITR) and promote there.
//   - backup: one backup set exactly as it was taken (--type=immediate), nothing replayed after.
//
// All three are --delta restores — only files that differ from the backup are copied, as
// Patroni's own pgbackrest command does — of a stopped server, which is then started and waited
// on until it has left recovery and accepts writes.
//
// Every restore starts a new timeline — recovery that ends in a promotion always does, "latest"
// included. pgBackRest keeps archiving on it, and the response says which timeline the node is
// on now and suggests a new full backup: the old backups still restore, but a fresh one is the
// clean starting point for the next point-in-time restore.

// pgRestoreNode loads a deployed, running standalone pg node with pgBackRest on, writing the HTTP
// error itself when anything is missing.
func (a *App) pgRestoreNode(w http.ResponseWriter, r *http.Request) (Stack, designNode, Deployment, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, designNode{}, Deployment{}, false
	}
	nid := r.PathValue("nid")
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return Stack{}, designNode{}, Deployment{}, false
	}
	for _, n := range doc.Nodes {
		if n.ID != nid || n.Type != "pg" {
			continue
		}
		if !n.UsePgBackRest {
			writeErr(w, http.StatusBadRequest, "pgBackRest is not enabled for this node")
			return Stack{}, designNode{}, Deployment{}, false
		}
		dep, err := a.store.GetDeployment(st.ID, nid)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			writeErr(w, http.StatusConflict, "node is not running")
			return Stack{}, designNode{}, Deployment{}, false
		}
		return st, n, dep, true
	}
	writeErr(w, http.StatusNotFound, "PostgreSQL node not found")
	return Stack{}, designNode{}, Deployment{}, false
}

// pgBackupSet is one backup in the repository, as the Backup tab lists it.
type pgBackupSet struct {
	Label string `json:"label"`
	Type  string `json:"type"` // full | diff | incr
	Start string `json:"start"`
	Stop  string `json:"stop"`
	Size  int64  `json:"size"`     // database size at backup time
	Repo  int64  `json:"repoSize"` // what it takes in the repository
	WAL   string `json:"walStart"`
}

// pgBackRestInfoJSON is the subset of `pgbackrest info --output=json` the tab needs.
type pgBackRestInfoJSON []struct {
	Archive []struct {
		Min string `json:"min"`
		Max string `json:"max"`
	} `json:"archive"`
	Backup []struct {
		Label     string `json:"label"`
		Type      string `json:"type"`
		Timestamp struct {
			Start int64 `json:"start"`
			Stop  int64 `json:"stop"`
		} `json:"timestamp"`
		Info struct {
			Size       int64 `json:"size"`
			Repository struct {
				Delta int64 `json:"delta"`
			} `json:"repository"`
		} `json:"info"`
		Archive struct {
			Start string `json:"start"`
		} `json:"archive"`
	} `json:"backup"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

// handlePGBackups lists the node's backup sets and the window a point-in-time restore can reach.
func (a *App) handlePGBackups(w http.ResponseWriter, r *http.Request) {
	_, n, dep, ok := a.pgRestoreNode(w, r)
	if !ok {
		return
	}
	out, err := a.pgBackupList(r.Context(), dep.ContainerID, patroniStanza(n.Label))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// pgBackupList runs `pgbackrest info` in a container and shapes it for the Backup tab: the sets,
// and the earliest point a time restore can reach. Shared by the standalone node and Patroni.
func (a *App) pgBackupList(ctx context.Context, containerID, stanza string) (map[string]any, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c",
		`runuser -u postgres -- pgbackrest --stanza="$STANZA" info --output=json`}, []string{"STANZA=" + stanza})
	if err != nil {
		return nil, fmt.Errorf("pgbackrest info failed: %w", err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("pgbackrest info failed: %s", lastLines(res.Stderr+res.Stdout, 200))
	}
	var info pgBackRestInfoJSON
	if err := json.Unmarshal([]byte(res.Stdout), &info); err != nil || len(info) == 0 {
		return nil, fmt.Errorf("pgbackrest info: unreadable output")
	}
	st := info[0]
	sets := []pgBackupSet{}
	for _, b := range st.Backup {
		sets = append(sets, pgBackupSet{
			Label: b.Label, Type: b.Type,
			Start: time.Unix(b.Timestamp.Start, 0).UTC().Format(time.RFC3339),
			Stop:  time.Unix(b.Timestamp.Stop, 0).UTC().Format(time.RFC3339),
			Size:  b.Info.Size, Repo: b.Info.Repository.Delta, WAL: b.Archive.Start,
		})
	}
	out := map[string]any{"stanza": stanza, "backups": sets, "status": st.Status.Message}
	// The earliest point a time restore can reach is the end of the oldest backup: before that
	// there is no consistent copy to replay from.
	if len(sets) > 0 {
		out["earliest"] = sets[0].Stop
	}
	if len(st.Archive) > 0 {
		out["walMax"] = st.Archive[len(st.Archive)-1].Max
	}
	return out, nil
}

// pgRestoreRequest is POST …/pg/restore.
type pgRestoreRequest struct {
	Mode string `json:"mode"` // latest | time | backup
	Time string `json:"time"` // RFC 3339, for mode=time
	Set  string `json:"set"`  // backup label, for mode=backup
}

// pgRestoreArgs turns a request into pgBackRest's restore options, or says what is wrong with it.
func pgRestoreArgs(req pgRestoreRequest) ([]string, error) {
	switch req.Mode {
	case "", "latest":
		return nil, nil
	case "time":
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Time))
		if err != nil {
			return nil, fmt.Errorf("time must be RFC 3339, e.g. 2026-10-14T09:30:00Z")
		}
		if t.After(time.Now().Add(time.Minute)) {
			return nil, fmt.Errorf("%s is in the future — there is no WAL to replay to it", t.UTC().Format(time.RFC3339))
		}
		// pgBackRest hands the target to PostgreSQL's recovery_target_time, which wants
		// "YYYY-MM-DD HH:MM:SS+00", and picks the newest backup that ends before it.
		return []string{"--type=time", "--target=" + t.UTC().Format("2006-01-02 15:04:05") + "+00", "--target-action=promote"}, nil
	case "backup":
		set := strings.TrimSpace(req.Set)
		if set == "" || strings.ContainsAny(set, " \t\n'\"$`;") {
			return nil, fmt.Errorf("set must be a backup label from the list")
		}
		return []string{"--set=" + set, "--type=immediate", "--target-action=promote"}, nil
	}
	return nil, fmt.Errorf("mode must be latest, time or backup")
}

// handlePGRestore stops PostgreSQL, restores it from pgBackRest and starts it again.
func (a *App) handlePGRestore(w http.ResponseWriter, r *http.Request) {
	st, n, dep, ok := a.pgRestoreNode(w, r)
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
	a.stampEngine(r, st, n.ID)
	major := ppgMajorOf(n.PGMajor)
	ctx := r.Context()
	env := []string{"STANZA=" + patroniStanza(n.Label), "SERVICE=" + pgServiceName(n.OS, major),
		"PSQL=" + pgBinDir(n.OS, major) + "/psql", "ARGS=" + strings.Join(args, "\n")}
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"bash", "-c", pgRestoreScript}, env)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "restore failed: "+err.Error())
		return
	}
	if res.Code != 0 {
		writeErr(w, http.StatusInternalServerError, "restore failed: "+lastLines(res.Stderr+res.Stdout, 400))
		return
	}
	what := map[string]string{"": "the latest point", "latest": "the latest point", "time": req.Time, "backup": "backup " + req.Set}[req.Mode]
	a.notifyStack(st.ID, "restore.done", "success", "Restore completed", n.Label+": restored to "+what+".", n.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "restoredTo": what, "log": lastLines(res.Stdout, 2000),
		"timeline": pgRestoredTimeline(res.Stdout),
	})
}

// pgRestoreScript: stop, --delta restore, start, wait out recovery. ARGS is newline-separated so
// a target with a space in it ("2026-10-14 09:30:00+00") stays one argument.
// Env: STANZA, SERVICE, PSQL, ARGS.
const pgRestoreScript = `set -e
mapfile -t extra < <(printf '%s' "$ARGS" | sed '/^$/d')
systemctl stop "$SERVICE"
echo "stopped $SERVICE"
runuser -u postgres -- pgbackrest --stanza="$STANZA" --delta --log-level-console=info "${extra[@]}" restore 2>&1 |
  grep -E "INFO: (restore command begin|restore backup set|restore size|write updated|restore command end)|WARN|ERROR" || true
test "${PIPESTATUS[0]}" -eq 0
systemctl start "$SERVICE"
for i in $(seq 1 120); do
  r=$(runuser -u postgres -- "$PSQL" -qAtX -c 'SELECT pg_is_in_recovery()' 2>/dev/null || true)
  [ "$r" = "f" ] && { echo "recovery finished — accepting writes"; runuser -u postgres -- "$PSQL" -qAtX -c "SELECT 'timeline ' || timeline_id FROM pg_control_checkpoint()"; exit 0; }
  sleep 1
done
echo "PostgreSQL did not leave recovery within 120s:"; journalctl -u "$SERVICE" --no-pager 2>/dev/null | tail -15
exit 1`

// pgRestoredTimeline reads the "timeline N" line pgRestoreScript ends with.
func pgRestoredTimeline(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "timeline "); ok {
			return v
		}
	}
	return ""
}
