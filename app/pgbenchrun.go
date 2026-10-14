package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// pgbenchrun.go — running pgbench against the data pgbenchdata.go loaded, from the node's panel.
//
// The sample data is pgbench's own tables, so the natural thing to do with it is pgbench: a few
// clients for a few seconds, read/write (the TPC-B-like default) or read-only (-S, selects only),
// and the numbers that come back — TPS, average latency, failed transactions. It runs on the node
// itself (on Patroni, the leader), as the postgres user over the local socket, so it measures the
// server rather than the network, and it is bounded — up to 64 clients for up to 5 minutes —
// because it is a request that waits for its answer.

type pgBenchRequest struct {
	Clients  int  `json:"clients"`
	Seconds  int  `json:"seconds"`
	ReadOnly bool `json:"readOnly"`
}

// pgBenchResult is what pgbench printed, parsed.
type pgBenchResult struct {
	TPS          float64 `json:"tps"`
	LatencyMs    float64 `json:"latencyMs"`
	Transactions int64   `json:"transactions"`
	Failed       int64   `json:"failed"`
	Clients      int     `json:"clients"`
	Seconds      int     `json:"seconds"`
	ReadOnly     bool    `json:"readOnly"`
	Output       string  `json:"output"`
}

func (q pgBenchRequest) check() error {
	if q.Clients < 1 || q.Clients > 64 {
		return fmt.Errorf("clients must be 1–64")
	}
	if q.Seconds < 5 || q.Seconds > 300 {
		return fmt.Errorf("seconds must be 5–300")
	}
	return nil
}

var (
	pgbTPS     = regexp.MustCompile(`tps = ([0-9.]+)`)
	pgbLatency = regexp.MustCompile(`latency average = ([0-9.]+) ms`)
	pgbTxns    = regexp.MustCompile(`number of transactions actually processed: ([0-9]+)`)
	pgbFailed  = regexp.MustCompile(`number of failed transactions: ([0-9]+)`)
)

// parsePGBench reads pgbench's summary.
func parsePGBench(out string) (pgBenchResult, error) {
	var r pgBenchResult
	m := pgbTPS.FindStringSubmatch(out)
	if m == nil {
		return r, fmt.Errorf("pgbench printed no tps line")
	}
	r.TPS, _ = strconv.ParseFloat(m[1], 64)
	if m := pgbLatency.FindStringSubmatch(out); m != nil {
		r.LatencyMs, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := pgbTxns.FindStringSubmatch(out); m != nil {
		r.Transactions, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if m := pgbFailed.FindStringSubmatch(out); m != nil {
		r.Failed, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return r, nil
}

// runPGBench runs it in a container and writes the response.
func (a *App) runPGBench(w http.ResponseWriter, r *http.Request, containerID, bindir string) {
	var q pgBenchRequest
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := q.check(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mode := ""
	if q.ReadOnly {
		mode = "-S"
	}
	ctx := r.Context()
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pgBenchRunScript}, []string{
		"BINDIR=" + bindir, "CLIENTS=" + strconv.Itoa(q.Clients), "SECONDS=" + strconv.Itoa(q.Seconds), "MODE=" + mode,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pgbench: "+err.Error())
		return
	}
	out := strings.TrimSpace(res.Stdout + res.Stderr)
	if res.Code != 0 {
		writeErr(w, http.StatusConflict, lastLines(out, 400))
		return
	}
	pr, err := parsePGBench(out)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error()+": "+lastLines(out, 300))
		return
	}
	pr.Clients, pr.Seconds, pr.ReadOnly, pr.Output = q.Clients, q.Seconds, q.ReadOnly, lastLines(out, 1500)
	writeJSON(w, http.StatusOK, pr)
}

// handlePGBenchRun is pgbench on a standalone node.
func (a *App) handlePGBenchRun(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	var doc designDoc
	_ = json.Unmarshal(st.Design, &doc)
	for _, n := range doc.Nodes {
		if n.ID != nid || n.Type != "pg" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, nid)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			writeErr(w, http.StatusConflict, "node is not running")
			return
		}
		a.stampEngine(r, st, nid)
		a.runPGBench(w, r, dep.ContainerID, pgBinDir(n.OS, ppgMajorOf(n.PGMajor)))
		return
	}
	writeErr(w, http.StatusNotFound, "PostgreSQL node not found")
}

// handlePatroniPGBenchRun is pgbench on a Patroni cluster's leader.
func (a *App) handlePatroniPGBenchRun(w http.ResponseWriter, r *http.Request) {
	t, ok := a.patroniFrameMembers(w, r)
	if !ok {
		return
	}
	a.runPGBench(w, r, t.leader.container, pgBinDir(t.frame.OS, ppgMajorOf(t.frame.PGMajor)))
}

// pgBenchRunScript runs pgbench on the local socket; no pgbench tables is said in words.
// Env: BINDIR, CLIENTS, SECONDS, MODE ("" or -S).
const pgBenchRunScript = `PSQL="$BINDIR/psql"
has=$(runuser -u postgres -- "$PSQL" -h /var/run/postgresql -d pgbench -qAtX -c "SELECT to_regclass('pgbench_accounts') IS NOT NULL" 2>/dev/null)
[ "$has" = "t" ] || { echo "there is no pgbench data on this server — set a pgbench scale on the node's form (it is loaded at deploy), or run pgbench -i -s 10 pgbench"; exit 1; }
runuser -u postgres -- "$BINDIR/pgbench" -h /var/run/postgresql -n $MODE -c "$CLIENTS" -j "$(( CLIENTS < 4 ? CLIENTS : 4 ))" -T "$SECONDS" pgbench 2>&1`
