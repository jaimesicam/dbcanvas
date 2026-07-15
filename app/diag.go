package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// On-node diagnostic captures. Two kinds, gated by engine:
//
//	pg_gather (PostgreSQL: pg / patroni / repmgr / spock) — clone pg_gather, run its
//	  gather.sql against the chosen database, build the schema + report, producing
//	  GatherReport.html on the node for download.
//	pt-stalk  (MySQL family: pxc / mysql / ps / innodb) — pt-summary + pt-mysql-summary +
//	  pt-stalk (has a ~90s cooldown of sampling), tarred up on the node for download.
//
// Captures run asynchronously (pt-stalk is slow); the frontend polls the status endpoint
// and, once done, hits the download endpoint. Result files live at fixed paths so a
// download works even after an app restart (the status handler also probes the file).

const (
	pgGatherFile   = "/tmp/pg_gather/GatherReport.html"
	ptStalkFile    = "/tmp/ptstalk.tar.gz"
	captureRunning = "running"
	captureDone    = "done"
	captureError   = "error"
	captureIdle    = "idle"
)

// captureState is the status of one node's capture of a given kind.
type captureState struct {
	Status   string `json:"status"` // idle | running | done | error
	Message  string `json:"message,omitempty"`
	Database string `json:"database,omitempty"` // pg_gather target db
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`
	Name     string `json:"name,omitempty"` // download filename
}

func captureKey(stackID int64, nodeID, kind string) string {
	return fmt.Sprintf("%d/%s/%s", stackID, nodeID, kind)
}

// loadRunningDBNode resolves a running node of the wanted engine ("postgres"|"mysql"),
// returning its deployment and design type. Errors are written to w.
func (a *App) loadRunningDBNode(w http.ResponseWriter, r *http.Request, wantEngine string) (Deployment, string, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Deployment{}, "", false
	}
	nid := r.PathValue("nid")
	typ := nodeTypeIn(st, nid)
	if engineForType(typ) != wantEngine {
		writeErr(w, http.StatusBadRequest, "this capture is not available for this node type")
		return Deployment{}, "", false
	}
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" {
		writeErr(w, http.StatusNotFound, "node is not deployed")
		return Deployment{}, "", false
	}
	if dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return Deployment{}, "", false
	}
	dep.StackID = st.ID
	a.stampEngine(r, st, nid)
	dep = a.reconcileContainerID(r.Context(), st.ID, nid, dep)
	return dep, typ, true
}

// fileExists reports whether a non-empty file exists in the container, on the engine
// carried by ctx (a VM for a hybrid stack's DB node, else Docker).
func (a *App) fileExists(ctx context.Context, containerID, path string) bool {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", "test -s " + path}, nil)
	return err == nil && res.Code == 0
}

// captureStatusFor returns the in-memory capture state, or — if none — a "done"/"idle"
// state derived from whether the result file is present on the node (so a prior capture
// survives an app restart).
func (a *App) captureStatusFor(ctx context.Context, stackID int64, nodeID, kind, containerID, file, name string) captureState {
	if v, ok := a.captures.Load(captureKey(stackID, nodeID, kind)); ok {
		return *(v.(*captureState))
	}
	if a.fileExists(ctx, containerID, file) {
		return captureState{Status: captureDone, Name: name}
	}
	return captureState{Status: captureIdle}
}

// startCapture launches a capture in the background, tracking its status in a.captures.
// eng is the node's engine (from the request context) — the goroutine outlives the
// request, so it carries the engine explicitly onto a background context.
func (a *App) startCapture(eng Engine, stackID int64, nodeID, kind, containerID, script string, env []string, database, name string) {
	key := captureKey(stackID, nodeID, kind)
	if v, ok := a.captures.Load(key); ok && v.(*captureState).Status == captureRunning {
		return // already running
	}
	a.captures.Store(key, &captureState{Status: captureRunning, Database: database, Name: name, Started: time.Now().UTC().Format(time.RFC3339)})
	go func() {
		out, err := a.execScript(withEngine(context.Background(), eng), containerID, script, env)
		st := &captureState{Database: database, Name: name, Finished: time.Now().UTC().Format(time.RFC3339)}
		if v, ok := a.captures.Load(key); ok {
			st.Started = v.(*captureState).Started
		}
		if err != nil {
			st.Status = captureError
			st.Message = lastLines(err.Error(), 300)
		} else {
			st.Status = captureDone
			st.Message = strings.TrimSpace(out)
		}
		a.captures.Store(key, st)
	}()
}

// serveContainerFile reads a file out of a container and serves it as a download, on
// the engine carried by ctx.
func (a *App) serveContainerFile(ctx context.Context, w http.ResponseWriter, containerID, path, name, contentType string) {
	if !a.fileExists(ctx, containerID, path) {
		writeErr(w, http.StatusNotFound, "no capture available — run it first")
		return
	}
	data, err := a.readContainerFile(ctx, containerID, path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read capture: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// hostnameOf reads a deployment's config hostname (best-effort).
func hostnameOf(dep Deployment) string {
	var c struct {
		Hostname string `json:"hostname"`
	}
	_ = json.Unmarshal(dep.Config, &c)
	if c.Hostname == "" {
		return "node"
	}
	return c.Hostname
}

// ------------------------------------------------------------------ pg_gather

func (a *App) handlePGGatherStatus(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, "postgres")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.captureStatusFor(r.Context(), dep.StackID, r.PathValue("nid"), "pggather", dep.ContainerID, pgGatherFile, "GatherReport.html"))
}

func (a *App) handlePGGatherStart(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, "postgres")
	if !ok {
		return
	}
	var b struct {
		Database string `json:"database"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	b.Database = strings.TrimSpace(b.Database)
	if !validName(b.Database) {
		writeErr(w, http.StatusBadRequest, "select a database to gather from")
		return
	}
	a.startCapture(a.engCtx(r.Context()), dep.StackID, r.PathValue("nid"), "pggather", dep.ContainerID, pgGatherScript, []string{"DB=" + b.Database}, b.Database, "GatherReport.html")
	writeJSON(w, http.StatusAccepted, map[string]any{"status": captureRunning, "database": b.Database})
}

func (a *App) handlePGGatherDownload(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, "postgres")
	if !ok {
		return
	}
	a.serveContainerFile(r.Context(), w, dep.ContainerID, pgGatherFile, "GatherReport.html", "text/html; charset=utf-8")
}

// ------------------------------------------------------------------ pt-stalk

func (a *App) handlePTStalkStatus(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, "mysql")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.captureStatusFor(r.Context(), dep.StackID, r.PathValue("nid"), "ptstalk", dep.ContainerID, ptStalkFile, ptStalkName(dep)))
}

func (a *App) handlePTStalkStart(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, "mysql")
	if !ok {
		return
	}
	a.startCapture(a.engCtx(r.Context()), dep.StackID, r.PathValue("nid"), "ptstalk", dep.ContainerID, ptStalkScript, nil, "", ptStalkName(dep))
	writeJSON(w, http.StatusAccepted, map[string]any{"status": captureRunning})
}

func (a *App) handlePTStalkDownload(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, "mysql")
	if !ok {
		return
	}
	a.serveContainerFile(r.Context(), w, dep.ContainerID, ptStalkFile, ptStalkName(dep), "application/gzip")
}

func ptStalkName(dep Deployment) string { return "ptstalk-" + hostnameOf(dep) + ".tar.gz" }

// ------------------------------------------------------------------ scripts

// pgGatherScript clones jobinau/pg_gather and runs its three-step capture: gather.sql
// against the chosen database ($DB), then load the schema + gathered data and build the
// HTML report into GatherReport.html. psql runs as the postgres OS user (peer auth); its
// path is resolved for PGDG / Debian / source (Spock) layouts. git is a fallback-install
// (it is pre-installed in the base images).
const pgGatherScript = `set -e
command -v git >/dev/null 2>&1 || { (command -v dnf >/dev/null 2>&1 && dnf -y -q install git >/dev/null 2>&1) || (apt-get update -qq >/dev/null && apt-get install -y -qq git >/dev/null) || { echo "git is not available"; exit 1; }; }
PSQL=$(command -v psql 2>/dev/null || true)
[ -n "$PSQL" ] || PSQL=$(ls /usr/pgsql-*/bin/psql /usr/lib/postgresql/*/bin/psql /usr/local/bin/psql 2>/dev/null | head -1)
[ -n "$PSQL" ] || { echo "psql not found on this node"; exit 1; }
rm -rf /tmp/pg_gather
git clone --depth 1 https://github.com/jobinau/pg_gather.git /tmp/pg_gather >/tmp/pgg-clone.log 2>&1 || { echo "git clone pg_gather failed:"; tail -6 /tmp/pgg-clone.log; exit 1; }
cd /tmp/pg_gather
chmod -R a+rX /tmp/pg_gather
runuser -u postgres -- "$PSQL" -U postgres -d "$DB" -X -f gather.sql > /tmp/pg_gather/out.tsv 2>/tmp/pgg1.log || { echo "gather.sql (db $DB) failed:"; tail -8 /tmp/pgg1.log; exit 1; }
runuser -u postgres -- "$PSQL" -U postgres -f gather_schema.sql -f /tmp/pg_gather/out.tsv >/tmp/pgg2.log 2>&1 || { echo "gather_schema failed:"; tail -8 /tmp/pgg2.log; exit 1; }
runuser -u postgres -- "$PSQL" -U postgres -X -f gather_report.sql > /tmp/pg_gather/GatherReport.html 2>/tmp/pgg3.log || { echo "gather_report failed:"; tail -8 /tmp/pgg3.log; exit 1; }
test -s /tmp/pg_gather/GatherReport.html || { echo "GatherReport.html was not produced"; exit 1; }
echo "GatherReport.html generated for database $DB ($(wc -c < /tmp/pg_gather/GatherReport.html) bytes)"`

// ptStalkScript captures pt-summary + pt-mysql-summary + pt-stalk (a ~90s sampling
// window) into a per-host directory, tarred to a fixed path for download. The pt-* tools
// are pre-installed (percona-toolkit) and authenticate to the local MySQL via /root/.my.cnf.
const ptStalkScript = `set -e
PTDEST=/tmp/ptstalk-$(uname -n)
rm -rf "$PTDEST" /tmp/ptstalk.tar.gz
mkdir -p "$PTDEST/samples"
pt-summary > "$PTDEST/pt-summary.out" 2>>"$PTDEST/capture-errors.log" || true
pt-mysql-summary --save-samples="$PTDEST/samples" > "$PTDEST/pt-mysql-summary.out" 2>>"$PTDEST/capture-errors.log" || true
pt-stalk --no-stalk --iterations=2 --sleep=30 --dest="$PTDEST" >>"$PTDEST/capture-errors.log" 2>&1 || true
tar czf /tmp/ptstalk.tar.gz -C /tmp "$(basename "$PTDEST")"
test -s /tmp/ptstalk.tar.gz || { echo "pt-stalk archive was not produced"; exit 1; }
echo "pt-stalk archive ready ($(du -h /tmp/ptstalk.tar.gz | cut -f1))"`
