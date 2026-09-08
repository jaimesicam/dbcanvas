package store

import (
	"context"
	"strconv"
	"strings"
)

// WsrepStatus is a snapshot of a PXC node's own Galera state — plain
// `SHOW STATUS LIKE 'wsrep_%'`, the same thing every existing lab check in
// this repo already does inline; no dedicated helper existed anywhere in
// the codebase before this. Zero-valued (Ready=false, everything else 0) on
// a non-PXC target, since the query itself just returns no rows there —
// callers gate display on the target family, not on this struct.
type WsrepStatus struct {
	ClusterSize       int
	Ready             bool
	Connected         bool
	LocalState        int
	LocalStateComment string
	CertDepsDistance  float64
	FlowControlPaused float64 // fraction of time [0,1] flow control paused this node
	FlowControlSent   int64
	FlowControlRecv   int64
	LocalCertFailures int64
	LocalBFAborts     int64
	ReceiveQueueLen   int64
	SendQueueLen      int64
	ReplLatencyAvgMs  float64
}

func (s *Store) WsrepStatus(ctx context.Context) (WsrepStatus, error) {
	rows, err := s.DB.QueryContext(ctx, "SHOW STATUS LIKE 'wsrep_%'")
	if err != nil {
		return WsrepStatus{}, err
	}
	defer rows.Close()

	raw := map[string]string{}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			raw[k] = v
		}
	}
	if err := rows.Err(); err != nil {
		return WsrepStatus{}, err
	}

	var st WsrepStatus
	st.ClusterSize = atoiOr(raw["wsrep_cluster_size"], 0)
	st.Ready = raw["wsrep_ready"] == "ON"
	st.Connected = raw["wsrep_connected"] == "ON"
	st.LocalState = atoiOr(raw["wsrep_local_state"], 0)
	st.LocalStateComment = raw["wsrep_local_state_comment"]
	st.CertDepsDistance = atofOr(raw["wsrep_cert_deps_distance"], 0)
	st.FlowControlPaused = atofOr(raw["wsrep_flow_control_paused"], 0)
	st.FlowControlSent = int64(atoiOr(raw["wsrep_flow_control_sent"], 0))
	st.FlowControlRecv = int64(atoiOr(raw["wsrep_flow_control_recv"], 0))
	st.LocalCertFailures = int64(atoiOr(raw["wsrep_local_cert_failures"], 0))
	st.LocalBFAborts = int64(atoiOr(raw["wsrep_local_bf_aborts"], 0))
	st.ReceiveQueueLen = int64(atoiOr(raw["wsrep_local_recv_queue"], 0))
	st.SendQueueLen = int64(atoiOr(raw["wsrep_local_send_queue"], 0))
	// wsrep_evs_repl_latency: "min/avg/max/stddev/samplesize" — only the avg
	// (2nd field) is surfaced; the rest is diagnostic detail this panel
	// doesn't need.
	if parts := strings.SplitN(raw["wsrep_evs_repl_latency"], "/", 5); len(parts) == 5 {
		st.ReplLatencyAvgMs = atofOr(parts[1], 0) * 1000
	}
	return st, nil
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func atofOr(s string, def float64) float64 {
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}
