package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// errorlog.go — the last lines of a database node's own error log, for the Live details tab: the
// first place a DBA looks when a member is down or its replication stopped, without opening a
// console to find out where this engine on this OS keeps it.
//
// Where the log is is asked of the server where it can say (MySQL's @@log_error, PostgreSQL's
// pg_current_logfile()), read from the config where it cannot (MongoDB's systemLog.path, Valkey's
// logfile), and when a server is down — exactly when the log matters most — or logs to stderr,
// the systemd journal of its unit is read instead.

const errorLogMax = 1000

// errorLogScript tails $FILE when it is a readable file, else the journal of the first of $UNITS
// that systemd knows.
const errorLogScript = `if [ -n "$FILE" ] && [ -r "$FILE" ]; then
  echo "# $FILE"
  tail -n "$N" "$FILE"
  exit 0
fi
for u in $UNITS; do
  if systemctl cat "$u" >/dev/null 2>&1; then
    echo "# journalctl -u $u"
    journalctl -u "$u" -n "$N" --no-pager 2>&1
    exit 0
  fi
done
echo "# no log found${FILE:+ at $FILE}"`

func (a *App) handleNodeErrorLog(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n <= 0 {
		n = 200
	}
	if n > errorLogMax {
		n = errorLogMax
	}
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return
	}
	typ := nodeTypeIn(st, nid)
	ctx, cancel := context.WithTimeout(withEngine(r.Context(), a.depEngine(st, nid)), 20*time.Second)
	defer cancel()
	file, units := a.errorLogWhere(ctx, st, nid, typ, dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"sh", "-c", errorLogScript},
		[]string{"FILE=" + file, "UNITS=" + strings.Join(units, " "), "N=" + strconv.Itoa(n)})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := res.Stdout
	source := ""
	if first, rest, ok := strings.Cut(out, "\n"); ok && strings.HasPrefix(first, "# ") {
		source, out = strings.TrimPrefix(first, "# "), rest
	} else if strings.HasPrefix(out, "# ") {
		source, out = strings.TrimSpace(strings.TrimPrefix(out, "# ")), ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": source, "lines": strings.Split(strings.TrimRight(out, "\n"), "\n")})
}

// errorLogWhere is the node's error log file ("" when the server does not say or is down) and the
// systemd units to read the journal of instead, by engine.
func (a *App) errorLogWhere(ctx context.Context, st Stack, nid, typ string, dep Deployment) (string, []string) {
	if typ == "valkey" || typ == "valkeycluster" {
		var sec valkeySecrets
		json.Unmarshal(dep.Secrets, &sec)
		res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "--no-auth-warning", "CONFIG", "GET", "logfile"}, []string{"REDISCLI_AUTH=" + sec.Password})
		file := ""
		if err == nil {
			if l := strings.Split(strings.TrimSpace(res.Stdout), "\n"); len(l) == 2 {
				file = strings.TrimSpace(l[1])
			}
		}
		return file, []string{"valkey", "valkey-server"}
	}
	c, ok := a.dbConnFor(st, nid)
	if !ok {
		return "", nil
	}
	switch c.Engine {
	case "mysql":
		var p struct{ F, D string }
		pc, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if a.queryJSON(pc, c, "", "SELECT JSON_OBJECT('F', @@log_error, 'D', @@datadir)", &p) == nil && p.F != "" && p.F != "stderr" {
			f := p.F
			if !strings.HasPrefix(f, "/") {
				f = path.Join(p.D, f)
			}
			return f, []string{"mysqld", "mysql", "mariadb"}
		}
		return "", []string{"mysqld", "mysql", "mariadb"}
	case "postgres":
		var p struct{ F, D string }
		pc, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		units := []string{"patroni", "postgresql", "postgresql-18", "postgresql-17", "postgresql-16", "postgresql-15", "postgresql-14", "postgresql-13"}
		if a.queryJSON(pc, c, "postgres", "SELECT json_build_object('F', pg_current_logfile(), 'D', current_setting('data_directory'))", &p) == nil && p.F != "" {
			f := p.F
			if !strings.HasPrefix(f, "/") {
				f = path.Join(p.D, f)
			}
			return f, units
		}
		return "", units
	case "mongodb":
		res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"sh", "-c", `awk '/^systemLog:/{s=1;next} s&&/^[^ ]/{s=0} s&&/path:/{print $2; exit}' /etc/mongod.conf`}, nil)
		file := ""
		if err == nil {
			file = strings.Trim(strings.TrimSpace(res.Stdout), `"'`)
		}
		return file, []string{"mongod", "mongos"}
	}
	return "", nil
}
