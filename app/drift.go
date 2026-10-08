package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// drift.go — "Compare configuration": every member of a cluster asked for its running settings,
// and the ones that differ laid side by side. Members of one cluster are meant to be configured
// alike; one that is not (a buffer pool set by hand on one member, a variable persisted on a
// replica and never on the primary, a parameter changed and not rolled out) is how a switchover
// or a failover turns into a different database than the one that was tested.
//
// What is meant to differ is not reported as drift: identity (server_id, UUIDs, hostnames —
// a member's own host name in a value is replaced by <host> before comparing) and the transient.
// What differs by role (read_only on a replica, semi-sync's source/replica switches) is shown,
// marked as expected for the role.

// driftIgnore are settings that are each member's own by design.
var driftIgnore = regexp.MustCompile(`(?i)^(server_id|server_uuid|server_uid|server_id_bits|hostname|report_host|report_port|gtid_executed|gtid_purged|gtid_owned|` +
	`gtid_slave_pos|gtid_binlog_pos|gtid_binlog_state|gtid_current_pos|auto_increment_offset|wsrep_cluster_address|wsrep_start_position|` +
	`group_replication_group_seeds|` +
	`wsrep_node_name|wsrep_node_address|wsrep_node_incoming_address|wsrep_sst_receive_address|wsrep_ist_receive_address|` +
	`group_replication_local_address|group_replication_member_weight|pseudo_thread_id|timestamp|rand_seed[12]|warning_count|error_count|` +
	`innodb_buffer_pool_load_at_startup|last_insert_id|identity|insert_id|server_audit_.*_file|` +
	// PostgreSQL
	`cluster_name|primary_conninfo|primary_slot_name|transaction_read_only|in_hot_standby|data_directory|config_file|hba_file|ident_file|` +
	// Valkey
	`replicaof|slaveof|cluster-announce-.*|bind|unixsocket|pidfile|dir|logfile|dbfilename|appendfilename)$`)

// driftByRole are settings a member's role decides.
var driftByRole = regexp.MustCompile(`(?i)^(read_only|super_read_only|rpl_semi_sync_.*_enabled|offline_mode|event_scheduler|default_transaction_read_only|` +
	`group_replication_single_primary_mode|replica-read-only|slave-read-only|hot_standby)$`)

type driftRow struct {
	Name     string            `json:"name"`
	Values   map[string]string `json:"values"` // node id -> value ("" when the member does not have it)
	Expected bool              `json:"expected,omitempty"`
}

type driftMember struct {
	NodeID string `json:"nodeId"`
	Label  string `json:"label"`
	Err    string `json:"error,omitempty"`
	Count  int    `json:"count"`
}

// handleConfigDrift compares the running configuration of every member of {nid}'s cluster.
func (a *App) handleConfigDrift(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	doc := buildDoc(st)
	var frameID, typ string
	for _, n := range doc.Nodes {
		if n.ID == nid {
			frameID, typ = n.FrameID, n.Type
		}
	}
	if frameID == "" {
		writeErr(w, http.StatusConflict, "this node is not a member of a cluster")
		return
	}
	hosts := stackHostnames(doc)
	domain := envOr("DOMAIN", "example.net")
	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frameID && n.Type == typ && n.Role != "mongos" {
			members = append(members, n)
		}
	}
	if len(members) < 2 {
		writeErr(w, http.StatusConflict, "the cluster has one member — there is nothing to compare it with")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	settings := make([]map[string]string, len(members))
	out := make([]driftMember, len(members))
	var wg sync.WaitGroup
	for i, n := range members {
		wg.Add(1)
		go func(i int, n designNode) {
			defer wg.Done()
			out[i] = driftMember{NodeID: n.ID, Label: n.Label}
			c := withEngine(ctx, a.depEngine(st, n.ID))
			m, err := a.memberSettings(c, st, n)
			if err != nil {
				out[i].Err = clipLine(err.Error(), 200)
				return
			}
			// A member's own name inside a value (a log file, an address) is not drift.
			h := hosts[n.ID]
			fq := fqdnOf(h, domain)
			for k, v := range m {
				if h != "" {
					v = strings.ReplaceAll(v, fq, "<host>")
					v = strings.ReplaceAll(v, h, "<host>")
				}
				m[k] = v
			}
			settings[i] = m
			out[i].Count = len(m)
		}(i, n)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"members": out, "diffs": compareSettings(members, settings)})
}

// compareSettings lists every setting that is not the same on all the members that answered.
func compareSettings(members []designNode, settings []map[string]string) []driftRow {
	names := map[string]bool{}
	for _, m := range settings {
		for k := range m {
			names[k] = true
		}
	}
	rows := []driftRow{}
	for name := range names {
		if driftIgnore.MatchString(name) {
			continue
		}
		vals := map[string]string{}
		distinct := map[string]bool{}
		answered := 0
		for i, m := range settings {
			if m == nil {
				continue
			}
			answered++
			v, ok := m[name]
			if !ok {
				v = "(not set)"
			}
			vals[members[i].ID] = v
			distinct[v] = true
		}
		if answered < 2 || len(distinct) < 2 {
			continue
		}
		rows = append(rows, driftRow{Name: name, Values: vals, Expected: driftByRole.MatchString(name)})
	}
	rows = collapsePlugins(rows)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Expected != rows[j].Expected {
			return !rows[i].Expected
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

// collapsePlugins folds the settings of a plugin loaded on some members and not others — every one
// of its variables "(not set)" on the same members — into one row, "clone_* (17 settings)", which
// is the one difference there is.
func collapsePlugins(rows []driftRow) []driftRow {
	type group struct {
		idx  []int
		vals map[string]string
	}
	groups := map[string]*group{}
	for i, r := range rows {
		missing := []string{}
		for id, v := range r.Values {
			if v == "(not set)" {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		prefix, _, ok := strings.Cut(r.Name, "_")
		if !ok {
			continue
		}
		key := prefix + "\x00" + strings.Join(missing, ",")
		g := groups[key]
		if g == nil {
			g = &group{vals: map[string]string{}}
			for id, v := range r.Values {
				if v == "(not set)" {
					g.vals[id] = v
				} else {
					g.vals[id] = "set"
				}
			}
			groups[key] = g
		}
		g.idx = append(g.idx, i)
	}
	drop := map[int]bool{}
	var extra []driftRow
	for key, g := range groups {
		if len(g.idx) < 3 {
			continue
		}
		prefix, _, _ := strings.Cut(key, "\x00")
		for _, i := range g.idx {
			drop[i] = true
		}
		extra = append(extra, driftRow{Name: fmt.Sprintf("%s_* (%d settings — a plugin or component loaded on some members only)", prefix, len(g.idx)), Values: g.vals})
	}
	out := []driftRow{}
	for i, r := range rows {
		if !drop[i] {
			out = append(out, r)
		}
	}
	return append(out, extra...)
}

// memberSettings is one member's running configuration, by engine.
func (a *App) memberSettings(ctx context.Context, st Stack, n designNode) (map[string]string, error) {
	if n.Type == "valkey" || n.Type == "valkeycluster" {
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" {
			return nil, fmt.Errorf("not deployed")
		}
		var sec valkeySecrets
		json.Unmarshal(dep.Secrets, &sec)
		res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "--no-auth-warning", "CONFIG", "GET", "*"}, []string{"REDISCLI_AUTH=" + sec.Password})
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n")
		m := map[string]string{}
		for i := 0; i+1 < len(lines); i += 2 {
			m[strings.TrimSpace(lines[i])] = strings.TrimSpace(lines[i+1])
		}
		return m, nil
	}
	c, ok := a.dbConnFor(st, n.ID)
	if !ok {
		return nil, fmt.Errorf("not running")
	}
	m := map[string]string{}
	switch c.Engine {
	case "mysql":
		if err := a.queryJSON(ctx, c, "", "SELECT JSON_OBJECTAGG(VARIABLE_NAME, VARIABLE_VALUE) FROM performance_schema.global_variables", &m); err != nil || len(m) == 0 {
			// MariaDB with performance_schema off
			m = map[string]string{}
			if err := a.queryJSON(ctx, c, "", "SELECT JSON_OBJECTAGG(LOWER(VARIABLE_NAME), VARIABLE_VALUE) FROM information_schema.GLOBAL_VARIABLES", &m); err != nil {
				return nil, err
			}
		}
	case "postgres":
		if err := a.queryJSON(ctx, c, "postgres", "SELECT json_object_agg(name, setting) FROM pg_settings", &m); err != nil {
			return nil, err
		}
	case "mongodb":
		client, closer, err := a.mongoClientFor(ctx, c)
		if err != nil {
			return nil, err
		}
		defer closer()
		var raw bson.M
		if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "getParameter", Value: "*"}}).Decode(&raw); err != nil {
			return nil, err
		}
		for k, v := range raw {
			if k == "ok" || strings.HasPrefix(k, "$") || k == "operationTime" {
				continue
			}
			b, _ := json.Marshal(v)
			m[k] = strings.Trim(string(b), `"`)
		}
	default:
		return nil, fmt.Errorf("no configuration to compare for this engine")
	}
	return m, nil
}
