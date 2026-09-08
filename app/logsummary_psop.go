package main

// logsummary_psop.go — the Percona Operator for MySQL (Percona Server), the last of the six.
//
// The cheapest of them all to add, and for two reasons that are the payoff of everything
// before it:
//
//   - The operator writes the **same zap format** the PXC, PSMDB and Percona PostgreSQL
//     operators write, so `lsFoldOperator` reads it unchanged. Only the controller group
//     (`ps.percona.com`) and the catalogue are new.
//   - `kubectl logs <pod> -c mysql` on a member returns **the mysqld error log itself** —
//     not the entrypoint's `bash -x` trace the way the PXC operator's pods do — and its
//     records are **Group Replication's**, which logsummary_grouprepl.go already reads in
//     full. The members therefore need no new code at all.
//
// Where it sits between the other two MySQL operators is worth knowing. Killing the primary
// of each produced, in the operators' own logs:
//
//	PXC     nothing whatsoever
//	PS      `Assigning primary label to pod psc-mysql-0` — it names the new primary
//	        and nothing else
//
// So this operator records the one fact that is genuinely hard to reconstruct afterwards
// (which member the writes moved to, and when) and nothing about the failure that caused
// the move. The members' Group Replication records carry that half.

import "strings"

const lsFlavourPSOperator = "psoperator"

// lsSniffPSOperator recognises it. The group is checked before the shape for the same
// reason every other Percona operator's is: they all write identical lines.
func lsSniffPSOperator(data string) string {
	head := data
	if len(head) > 256<<10 {
		head = head[:256<<10]
	}
	if strings.Contains(head, "ps.percona.com") || strings.Contains(head, "PerconaServerMySQL") ||
		strings.Contains(head, "percona-server-mysql-operator") {
		return lsFlavourPSOperator
	}
	return ""
}

func lsPSOpNodeName(recs []lsRecord) string {
	for _, r := range recs {
		for _, ln := range r.Body {
			if v, ok := strings.CutPrefix(ln, "PerconaServerMySQL: "); ok {
				if n := lsOpObjName(v); n != "" {
					return "operator/" + n
				}
			}
		}
	}
	return ""
}

var lsPSOpRules = []lsRule{
	{
		substr: []string{"Assigning primary label to pod"},
		class:  lsClassState, sev: lsSevWarn,
		label: "Primary moved",
		means: "The operator has decided which member is the primary and labelled it, which is what moves the write Service. It is the single most useful record this operator writes: Group Replication elects the primary itself and the members' logs say who WAS elected, but this is the moment the traffic followed.",
		enrich: func(r lsRecord, e *lsEvent) {
			if _, pod, ok := strings.Cut(r.Text, "Assigning primary label to pod "); ok {
				e.Peer = strings.TrimSpace(pod)
				e.Label = "Primary moved to " + e.Peer
			}
		},
	},
	{
		substr: []string{"Cluster state changed"},
		class:  lsClassState, sev: lsSevInfo,
		label: "Cluster state changed",
		means: "The operator's own opinion of the cluster, from its custom resource.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			prev, cur := f.last("previous"), f.last("current")
			if cur != "" {
				e.Label = "Cluster state: " + prev + " → " + cur
				if cur == "error" {
					e.Sev = lsSevBad
				}
			}
		},
	},
	{
		substr: []string{"Not all MySQL pods are ready", "getting ready pod: no pod is ready"},
		class:  lsClassState, sev: lsSevWarn, overLevel: true,
		label: "No member is ready",
		means: "The operator cannot find a member it considers ready, so it can reconcile nothing that needs a database connection — users, the group's membership, the backup configuration. Ordinary for a minute while a cluster starts; a run of these afterwards is a cluster the operator has lost contact with.",
	},
	{
		substr: []string{"groupReplicationStatus"},
		class:  lsClassMember, sev: lsSevInfo,
		label: "Group Replication status checked",
		means: "The operator asking the group who is in it. The answer it acts on is in the members' own logs.",
	},
	{
		substr: []string{"removing existing telemetry job", "adding new job"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "Scheduled job registered",
		means: "The operator's cron entries for this cluster — backups, the version check, telemetry.",
	},
	{
		substr: []string{"Reconciler error"},
		class:  lsClassReconcile, sev: lsSevWarn, overLevel: true,
		label: "Reconcile failed",
		means: "controller-runtime's retry record with an exponential backoff behind it, so one fault produces many.",
		enrich: func(r lsRecord, e *lsEvent) {
			if msg := lsOpErrText(lsOpFieldsOf(r)); msg != "" {
				e.Label = "Reconcile failed: " + lsOpTrunc(msg, 90)
			}
		},
	},
	{
		substr: []string{"starting manager", "Build info", "Starting EventSource", "Starting Controller",
			"Starting workers", "starting server", "Starting metrics server", "Serving metrics server"},
		class: lsClassStartup, sev: lsSevInfo,
		label: "Operator start-up",
		means: "controller-runtime wiring itself up.",
	},
}

func lsClassifyPSOperator(r lsRecord) (lsEvent, bool) {
	return lsClassifyOpRecord(r, lsPSOpRules, nil)
}

func lsResolvePSOperator(events []lsEvent) {
	for i := range events {
		if events[i].Label == "Operator start-up" && events[i].State == "" {
			events[i].State = lsStateStarting
		}
	}
}
