package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Shared helpers for the MySQL-family lab curricula (labs_mysqlrepl.go,
// labs_pxc.go, labs_haproxy_pxc.go) — mirrors how patroniFrameFromStack/
// runningPatroniMembers in labs.go, and valkeyFrameFromStack/
// mongoLabFrameFromStack in their own family files, each provide the same
// small set of "find the lab's cluster" primitives every check in that family
// builds on. Airline Sim's own tables live inside the very same MySQL/PXC
// database every check already reaches via Exec, so no separate connection
// path to Airline Sim itself is needed — and its container is distroless (no
// shell, no mysql client), so it's never Exec'd into directly, same as
// hotelsim/trafficsim never are either.

// mysqlLabExec runs a single SQL statement inside a MySQL-family container as
// the unix root user and returns its trimmed stdout. Every MySQL-family node
// (standalone ps, mysql replication, pxc) already gets a passwordless
// /root/.my.cnf written at provisioning time (pxcRootMyCnf, see app/pxc.go /
// app/mysql.go), so no credentials need to be threaded through here — the
// same shell-out-to-a-CLI pattern every existing lab check uses (patronictl,
// valkey-cli, psql).
//
// -N (skip column names) is used for every ordinary query so a plain `SELECT
// COUNT(*) ...` or `SHOW STATUS LIKE '...'` returns just the bare value(s), no
// header row to strip. It is deliberately OMITTED for a `\G` (vertical
// format) query: -N also strips \G's own "Field_name: value" labels, which
// verticalField depends on entirely — confirmed live, the very first time
// this ran against a real replica, `SHOW REPLICA STATUS\G` with -N came back
// as bare unlabeled values with no way to tell which line was which field.
func (a *App) mysqlLabExec(ctx context.Context, containerID, query string) (string, error) {
	args := []string{"mysql"}
	if !strings.HasSuffix(strings.TrimSpace(query), `\G`) {
		args = append(args, "-N")
	}
	args = append(args, "-e", query)
	res, err := a.engCtx(ctx).Exec(ctx, containerID, args, nil)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("mysql exited %d: %s", res.Code, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// verticalField extracts one "<Field>: <value>" line's value from a mysql
// client's `\G` (vertical) output — the format `SHOW REPLICA STATUS\G` and
// `SHOW SLAVE STATUS\G` produce, one field per line instead of one wide row.
func verticalField(output, field string) string {
	prefix := field + ": "
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

// statusValue extracts the value column from a `mysql -N -e "SHOW STATUS LIKE
// '...'"` or `SHOW VARIABLES LIKE '...'` single-row result (tab-separated
// Variable_name<TAB>Value with -N's headers suppressed).
func statusValue(output string) string {
	parts := strings.Split(output, "\t")
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[len(parts)-1])
}

// sqlQuote single-quotes and escapes a value built from a prior query's own
// result (never raw learner input) for interpolation into a follow-up query —
// mysqlLabExec has no parameterized-query path, so every check that needs to
// feed one query's result into another goes through this.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func pxcFrameOf(doc designDoc) (designFrame, bool) {
	for _, f := range doc.Frames {
		if f.Type == "pxc" {
			return f, true
		}
	}
	return designFrame{}, false
}

func mysqlReplFrameOf(doc designDoc) (designFrame, bool) {
	for _, f := range doc.Frames {
		if f.Type == "mysql" {
			return f, true
		}
	}
	return designFrame{}, false
}

// pxcFrameFromStack parses a lab stack's design and returns its (single) PXC
// cluster frame — every PXC/HAProxy+PXC check needs this first.
func pxcFrameFromStack(st Stack) (designDoc, designFrame, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return doc, designFrame{}, false
	}
	frame, ok := pxcFrameOf(doc)
	return doc, frame, ok
}

// mysqlReplFrameFromStack is pxcFrameFromStack's sibling for the MySQL
// replication curriculum.
func mysqlReplFrameFromStack(st Stack) (designDoc, designFrame, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return doc, designFrame{}, false
	}
	frame, ok := mysqlReplFrameOf(doc)
	return doc, frame, ok
}

// haproxyNodeFromStack finds the lab's one HAProxy node.
func haproxyNodeFromStack(st Stack) (designDoc, designNode, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return doc, designNode{}, false
	}
	for _, n := range doc.Nodes {
		if n.Type == "haproxy" {
			return doc, n, true
		}
	}
	return doc, designNode{}, false
}

// runningPXCMembers returns the running PXC deployments for a lab stack, in
// doc.Nodes order (the order every design template lists them in — lab
// instructions refer to "node 1"/"node 4" etc. by that same order).
func (a *App) runningPXCMembers(st Stack, doc designDoc) ([]Deployment, error) {
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return nil, err
	}
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	var running []Deployment
	for _, n := range doc.Nodes {
		if n.Type != "pxc" {
			continue
		}
		if d, ok := byNode[n.ID]; ok && d.State == DeployRunning && d.ContainerID != "" {
			running = append(running, d)
		}
	}
	return running, nil
}

// runningMySQLReplMembers is runningPXCMembers' sibling for MySQL replication.
func (a *App) runningMySQLReplMembers(st Stack, doc designDoc) ([]Deployment, error) {
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return nil, err
	}
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	var running []Deployment
	for _, n := range doc.Nodes {
		if n.Type != "mysql" {
			continue
		}
		if d, ok := byNode[n.ID]; ok && d.State == DeployRunning && d.ContainerID != "" {
			running = append(running, d)
		}
	}
	return running, nil
}

// mysqlReplMembers splits a lab's running MySQL replication members into
// (primary, secondaries) by their designNode.Role, mirroring how
// provisionMySQLFrame itself identifies the primary. ok is false unless every
// frame member is currently running.
func (a *App) mysqlReplMembers(st Stack, doc designDoc, frame designFrame) (primary Deployment, secondaries []Deployment, ok bool) {
	running, err := a.runningMySQLReplMembers(st, doc)
	if err != nil {
		return Deployment{}, nil, false
	}
	byNode := map[string]Deployment{}
	for _, d := range running {
		byNode[d.NodeID] = d
	}
	havePrimary := false
	total := 0
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "mysql" {
			continue
		}
		total++
		d, isRunning := byNode[n.ID]
		if !isRunning {
			return Deployment{}, nil, false
		}
		if n.Role == "primary" && !havePrimary {
			primary, havePrimary = d, true
		} else {
			secondaries = append(secondaries, d)
		}
	}
	return primary, secondaries, havePrimary && len(secondaries) == total-1
}

// mysqlLabExecRemote is mysqlLabExec's sibling for querying a DIFFERENT host
// (e.g. through HAProxy's read port) from inside a container that already has
// a mysql client. root only exists as 'root'@'localhost' on every MySQL-family
// node (so .my.cnf's passwordless root only ever works against the local
// socket) — a remote connection needs the network-reachable admin@'%'
// superuser instead, hence sec is required here but not in mysqlLabExec.
func (a *App) mysqlLabExecRemote(ctx context.Context, containerID string, sec pxcSecrets, host string, port int, query string) (string, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{
		"mysql", "-h", host, "-P", strconv.Itoa(port), "-u" + sec.AdminUser, "-p" + sec.AdminPassword, "-N", "-e", query,
	}, nil)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("mysql exited %d: %s", res.Code, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// pxcHottestInventoryRow finds a real, currently-booked flight_inventory row
// to target for a certification-conflict probe — the row with the most
// booked_seats right now, on the theory that Airline Sim's own Zipf-weighted
// popularity means this is a genuinely busy row, though (confirmed live) not
// necessarily one Airline Sim's own agents will collide with again inside a
// short probe window — see pxcConflictProbe's own doc comment for why the
// probe drives the conflict itself rather than waiting on that.
func (a *App) pxcHottestInventoryRow(ctx context.Context, containerID string) (string, error) {
	row, err := a.mysqlLabExec(ctx, containerID, "SELECT id FROM airlinesim.flight_inventory ORDER BY booked_seats DESC LIMIT 1")
	if err != nil || row == "" {
		return "", fmt.Errorf("airlinesim.flight_inventory isn't seeded yet")
	}
	return row, nil
}

// pxcConflictProbe fires `rounds` pairs of concurrent conflicting UPDATEs at
// the exact same flight_inventory row — one from each of the two given exec
// functions — and counts how many come back as a Galera certification
// failure / InnoDB deadlock (MySQL error 1213). This is a direct,
// deterministic proof of whether two concurrent writers actually produce a
// certification conflict, rather than depending on incidental collision with
// Airline Sim's own narrow, composite-keyed traffic — confirmed live, a real
// multi-node conflict storm run for two minutes produced 27 genuine 1213s
// between the storm's own writers while Airline Sim's txnRetries counter
// stayed at 0 the entire time, because its own agents never happened to touch
// that exact route+class+date row in the window. The lab's written
// instructions still have the learner cause this by hand for the learning
// experience; Check Work verifies the underlying mechanism directly instead
// of trusting (or waiting on) what their terminal happened to see.
func pxcConflictProbe(ctx context.Context, rowID string, rounds int, execA, execB func(ctx context.Context, query string) (string, error)) int {
	query := "UPDATE airlinesim.flight_inventory SET booked_seats=booked_seats+1 WHERE id=" + sqlQuote(rowID)
	conflicts := 0
	var mu sync.Mutex
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		probe := func(exec func(ctx context.Context, query string) (string, error)) {
			defer wg.Done()
			if _, err := exec(ctx, query); err != nil && strings.Contains(err.Error(), "1213") {
				mu.Lock()
				conflicts++
				mu.Unlock()
			}
		}
		go probe(execA)
		go probe(execB)
		wg.Wait()
	}
	return conflicts
}

// haproxyActiveWriter execs into a lab's HAProxy node and parses its stats CSV
// export (the same stats page haproxyCfg/haproxyPXCCfg's `stats uri /` already
// serves, at `<uri>;csv`) to find which PXC node is currently receiving write
// traffic: the lowest-priority (bck=0 first, then backup order) server in the
// "_write" backend whose status is UP — mirroring exactly how haproxyPXCCfg
// itself routes (one active node, the rest kept as `backup`, promoted only if
// the active one fails).
func (a *App) haproxyActiveWriter(ctx context.Context, containerID string) (string, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"curl", "-fsS", "http://127.0.0.1:7000/;csv"}, nil)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("curl exited %d: %s", res.Code, strings.TrimSpace(res.Stderr))
	}
	rows, err := csv.NewReader(strings.NewReader(res.Stdout)).ReadAll()
	if err != nil || len(rows) < 2 {
		return "", fmt.Errorf("empty or malformed haproxy stats")
	}
	header := rows[0]
	header[0] = strings.TrimPrefix(header[0], "# ")
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	pxIdx, svIdx, statusIdx, bckIdx := col["pxname"], col["svname"], col["status"], col["bck"]

	type srv struct{ name, status, bck string }
	var servers []srv
	for _, row := range rows[1:] {
		if len(row) <= pxIdx || len(row) <= svIdx || len(row) <= statusIdx || len(row) <= bckIdx {
			continue
		}
		pxname, svname := row[pxIdx], row[svIdx]
		if !strings.HasSuffix(pxname, "_write") || svname == "BACKEND" || svname == "FRONTEND" {
			continue
		}
		servers = append(servers, srv{name: svname, status: row[statusIdx], bck: row[bckIdx]})
	}
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].bck < servers[j].bck })
	for _, s := range servers {
		if s.status == "UP" {
			return s.name, nil
		}
	}
	return "", fmt.Errorf("no server in the write backend is currently UP")
}

// airlineSimMetric reads one numeric field out of Airline Sim's own
// metrics/id:"current" JSON payload, executed as a plain SQL query against
// whichever MySQL-family container is passed in — Airline Sim's tables live in
// that same database, so this is just another mysqlLabExec call, not a
// separate connection path.
func (a *App) airlineSimMetric(ctx context.Context, containerID, field string) (float64, error) {
	out, err := a.mysqlLabExec(ctx, containerID, "SELECT payload FROM airlinesim.metrics WHERE id='current'")
	if err != nil || out == "" {
		return 0, fmt.Errorf("airlinesim metrics not available yet — is it deployed and running?")
	}
	var payload map[string]any
	if jerr := json.Unmarshal([]byte(out), &payload); jerr != nil {
		return 0, fmt.Errorf("could not parse airlinesim metrics payload: %w", jerr)
	}
	v, ok := payload[field].(float64)
	if !ok {
		return 0, fmt.Errorf("airlinesim metrics payload has no numeric field %q yet", field)
	}
	return v, nil
}

// captureLabInitialHAProxyWriter polls the lab's HAProxy node until it reports
// an active writer, then records its short hostname as the baseline the
// HAProxy automatic-failover lab compares against — reuses the same
// SetLabRunLeader column the Patroni curriculum uses (same semantic: "which
// node was primary/active when the lab started").
func (a *App) captureLabInitialHAProxyWriter(runID, stackID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		st, err := a.store.GetStack(stackID)
		if err != nil {
			return
		}
		_, hp, ok := haproxyNodeFromStack(st)
		if !ok {
			return // no HAProxy in this lab's stack at all
		}
		dep, err := a.store.GetDeployment(stackID, hp.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		writer, err := a.haproxyActiveWriter(ctx, dep.ContainerID)
		if err != nil {
			continue
		}
		a.store.SetLabRunLeader(runID, writer)
		return
	}
}
