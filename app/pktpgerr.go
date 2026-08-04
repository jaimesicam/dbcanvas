package main

// pktpgerr.go — PostgreSQL's SQLSTATE codes, and what each one means for whoever is
// looking at the capture.
//
// This is the PostgreSQL half of what pktErrCatalog does for MySQL, and it is built
// the same way and for the same reason: a code on its own ("42P01") tells a reader
// who already knows PostgreSQL nothing they did not know, and tells everyone else
// nothing at all. The consequence is the useful part — which side is at fault,
// whether the connection survives, and what to look at next.
//
// Two things make SQLSTATE more informative than MySQL's numbers, and both are used
// here. The first two characters are a class, so an unknown code still places itself
// ("53" is "insufficient resources" whatever the third character says). And the
// severity travels separately from the code: the SAME state can arrive as an ERROR
// that leaves the session usable or as a FATAL that ends it, which is why
// pgErrorResponse reports severity as well as state.
//
// The list is deliberately not exhaustive — PostgreSQL defines several hundred
// states, most of them ordinary SQL mistakes that need no commentary. What is here
// is every state a capture of the wire is the right tool for: connection and
// authentication failures, resource exhaustion, shutdown and recovery, locking and
// serialisation, and the handful of client mistakes worth naming because they look
// like server faults.

import (
	"fmt"
	"sort"
	"strings"
)

// pgErrEntry is one SQLSTATE: its condition name (PostgreSQL's own, from
// errcodes.txt) and what it means operationally. issue is empty for the states that
// are ordinary application errors — they are reported, but they are not findings.
type pgErrEntry struct {
	name  string
	issue string
	// severe marks the states worth offering as a one-click filter, on the same
	// principle as pktErrIsSevere: something is wrong with the server, the cluster
	// or the connection, not with the SQL somebody wrote.
	severe bool
}

var pgErrCatalog = map[string]pgErrEntry{
	// ---- Class 08: connection exceptions. The connection itself failed.
	"08000": {"connection_exception", "Connection exception — the connection failed at the protocol level rather than in a statement", true},
	"08003": {"connection_does_not_exist", "The client used a connection the server had already closed", true},
	"08006": {"connection_failure", "Connection failure — the server ended the connection; the client sees a lost connection, and the reason is on the server side of this capture", true},
	"08001": {"sqlclient_unable_to_establish_sqlconnection", "The client could not establish a connection at all", true},
	"08004": {"sqlserver_rejected_establishment_of_sqlconnection", "The server refused the connection — pg_hba.conf did not match, or the database/role is not allowed to connect from here", true},
	"08007": {"transaction_resolution_unknown", "A transaction's outcome is unknown — the connection broke between COMMIT and its acknowledgement, so whether it committed cannot be told from the client", true},
	"08P01": {"protocol_violation", "Protocol violation — the server could not make sense of the bytes the client sent; a broken driver, a proxy corrupting the stream, or something that is not PostgreSQL talking to the port", true},

	// ---- Class 28: authorization.
	"28000": {"invalid_authorization_specification", "Authorisation failed — no pg_hba.conf line matched this user, database and address, or the role may not log in", true},
	"28P01": {"invalid_password", "Password authentication failed — a wrong password, or a role whose password was never set for the method pg_hba.conf demands", true},

	// ---- Class 3D/3F: the target does not exist.
	"3D000": {"invalid_catalog_name", "The database named at connect time does not exist — a typo, a dropped database, or a connection string pointing at the wrong server", true},
	"3F000": {"invalid_schema_name", "", false},

	// ---- Class 53: insufficient resources. The server is out of something.
	"53000": {"insufficient_resources", "The server is out of a resource it needs", true},
	"53100": {"disk_full", "Disk full — PostgreSQL cannot write; on the WAL volume this stops every write in the cluster and is often an unconsumed replication slot", true},
	"53200": {"out_of_memory", "Out of memory — the backend could not allocate; work_mem × concurrency, or a query planning to sort far more than expected", true},
	"53300": {"too_many_connections", "Too many connections — max_connections is full, so new clients are refused while existing ones are unaffected; a connection pool, not a bigger max_connections, is the fix", true},
	"53400": {"configuration_limit_exceeded", "A configuration limit was hit (prepared transactions, replication slots, locks per transaction)", true},

	// ---- Class 55: object not in the required state.
	"55000": {"object_not_in_prerequisite_state", "The object is not in a state that allows this — a slot in use, a table being altered, a subscription already enabled", true},
	"55006": {"object_in_use", "The object is in use elsewhere — the classic case is DROP DATABASE while a session is still connected to it", true},
	"55P02": {"cant_change_runtime_param", "", false},
	"55P03": {"lock_not_available", "Lock not available — a NOWAIT or lock_timeout gave up rather than waiting; something else holds the lock, and on a busy table that something is often an idle-in-transaction session", true},

	// ---- Class 57: operator intervention. The server is going away or said no.
	"57000": {"operator_intervention", "The operator intervened — the server or the backend was told to stop", true},
	"57014": {"query_canceled", "Query cancelled — statement_timeout expired, a client sent a CancelRequest, or on a standby the query conflicted with recovery", true},
	"57P01": {"admin_shutdown", "Administrator shutdown — the server, or this backend, was terminated deliberately (pg_terminate_backend, a restart, a Patroni failover); every connection sees its own version of this", true},
	"57P02": {"crash_shutdown", "Crash shutdown — another backend crashed, so the server is dropping every connection and going through recovery; the crash itself is in the server log, not in this capture", true},
	"57P03": {"cannot_connect_now", "The server cannot accept connections yet — it is still starting up, is in recovery, or is a standby that has not reached a consistent state; a client retry loop sees this until it clears", true},
	"57P04": {"database_dropped", "The database this session was using has been dropped", true},
	"57P05": {"idle_session_timeout", "The session was closed by idle_session_timeout — the pool is holding connections open longer than the server will", true},

	// ---- Class 58: external to PostgreSQL.
	"58000": {"system_error", "A system-level error outside PostgreSQL itself", true},
	"58030": {"io_error", "I/O error — the storage under PostgreSQL failed a read or a write; this is a hardware or filesystem fault, not a query problem", true},
	"58P01": {"undefined_file", "A file PostgreSQL expected is missing — a dropped relation still referenced, or a data directory modified underneath the server", true},

	// ---- Class 40: transaction rollback. Concurrency, not error.
	"40000": {"transaction_rollback", "The transaction was rolled back by the server", false},
	"40001": {"serialization_failure", "Serialisation failure — two transactions conflicted under REPEATABLE READ or SERIALIZABLE, and this one lost; the application is expected to retry it. On a standby the same code means a query conflicted with WAL replay", true},
	"40003": {"statement_completion_unknown", "", false},
	"40P01": {"deadlock_detected", "Deadlock detected — two transactions each held what the other needed and the server broke the cycle by killing this one; deadlock_timeout (1 s by default) is how long it waited before looking", true},

	// ---- Class 25: invalid transaction state.
	"25001": {"active_sql_transaction", "", false},
	"25006": {"read_only_sql_transaction", "A write was attempted on a read-only connection — almost always traffic reaching a standby: a load balancer routing writes to a replica, a stale DNS record after a failover, or default_transaction_read_only left on", true},
	"25P01": {"no_active_sql_transaction", "", false},
	"25P02": {"in_failed_sql_transaction", "The transaction has already failed, so every statement until ROLLBACK is rejected — the application is not checking errors before continuing to send", true},
	"25P03": {"idle_in_transaction_session_timeout", "The session was killed for sitting idle inside a transaction — it was holding locks and blocking VACUUM, and the server gave up on it", true},

	// ---- Class 42 / 23 / 22: the SQL or the data. Reported, not flagged.
	"42601": {"syntax_error", "", false},
	"42501": {"insufficient_privilege", "Permission denied — the role lacks the privilege; after a restore or a role change this is what an application failing everywhere looks like", true},
	"42P01": {"undefined_table", "", false},
	"42703": {"undefined_column", "", false},
	"42883": {"undefined_function", "", false},
	"42P07": {"duplicate_table", "", false},
	"42P05": {"duplicate_prepared_statement", "A prepared statement name was reused without being closed — a driver-level statement-cache bug, and it fails every execution that follows", true},
	"42P02": {"undefined_parameter", "", false},
	"42804": {"datatype_mismatch", "", false},
	"42P18": {"indeterminate_datatype", "", false},
	"23502": {"not_null_violation", "", false},
	"23503": {"foreign_key_violation", "", false},
	"23505": {"unique_violation", "", false},
	"23514": {"check_violation", "", false},
	"23P01": {"exclusion_violation", "", false},
	"22001": {"string_data_right_truncation", "A value was too long for its column — the analogue of a client sending more than the column can hold, and a common surprise after a schema change", false},
	"22003": {"numeric_value_out_of_range", "", false},
	"22012": {"division_by_zero", "", false},
	"22P02": {"invalid_text_representation", "", false},
	"22023": {"invalid_parameter_value", "", false},
	"22P05": {"untranslatable_character", "Character encoding mismatch — the bytes cannot be represented in the client_encoding this connection asked for", true},
	"22021": {"character_not_in_repertoire", "Invalid byte sequence for the connection's encoding — the client is sending one encoding and declaring another", true},

	// ---- Class 0A / F0 / XX: unsupported, config, internal.
	"0A000": {"feature_not_supported", "", false},
	"F0000": {"config_file_error", "PostgreSQL could not read its own configuration", true},
	"XX000": {"internal_error", "Internal error — PostgreSQL hit a condition it does not have a code for; these belong in a bug report with the server log around them", true},
	"XX001": {"data_corrupted", "Data corruption reported by the server — stop writing and look at the storage; this does not fix itself", true},
	"XX002": {"index_corrupted", "Index corruption reported by the server — a REINDEX may clear it, but find out why first", true},

	// ---- Class P0: PL/pgSQL, and the two that surface in application traffic.
	"P0001": {"raise_exception", "", false},
	"P0002": {"no_data_found", "", false},
	"P0004": {"assert_failure", "", false},
}

// pgClassNames covers the SQLSTATE classes, so an unrecognised code still places
// itself. The class is the first two characters, and PostgreSQL assigns them by
// meaning rather than by number.
var pgClassNames = map[string]string{
	"00": "success", "01": "warning", "02": "no data",
	"03": "SQL statement not yet complete", "08": "connection exception",
	"09": "triggered action exception", "0A": "feature not supported",
	"0B": "invalid transaction initiation", "0F": "locator exception",
	"0L": "invalid grantor", "0P": "invalid role specification",
	"0Z": "diagnostics exception", "20": "case not found",
	"21": "cardinality violation", "22": "data exception",
	"23": "integrity constraint violation", "24": "invalid cursor state",
	"25": "invalid transaction state", "26": "invalid SQL statement name",
	"27": "triggered data change violation", "28": "invalid authorization specification",
	"2B": "dependent privilege descriptors still exist", "2D": "invalid transaction termination",
	"2F": "SQL routine exception", "34": "invalid cursor name",
	"38": "external routine exception", "39": "external routine invocation exception",
	"3B": "savepoint exception", "3D": "invalid catalog name",
	"3F": "invalid schema name", "40": "transaction rollback",
	"42": "syntax error or access rule violation", "44": "WITH CHECK OPTION violation",
	"53": "insufficient resources", "54": "program limit exceeded",
	"55": "object not in prerequisite state", "57": "operator intervention",
	"58": "system error", "72": "snapshot failure",
	"F0": "configuration file error", "HV": "foreign data wrapper error",
	"P0": "PL/pgSQL error", "XX": "internal error",
}

// pgStateName is the condition name for a SQLSTATE, or the class's name for one that
// is not in the catalogue. Empty only for a state that is not a state at all.
func pgStateName(state string) string {
	if e, ok := pgErrCatalog[state]; ok {
		return e.name
	}
	if len(state) >= 2 {
		if cls, ok := pgClassNames[strings.ToUpper(state[:2])]; ok {
			return cls
		}
	}
	return ""
}

// pgErrIssue is the line the Issues list gets for an ErrorResponse, or "" when the
// error is an ordinary application error that needs no commentary.
//
// Two states are read together with the message text because the code alone is
// ambiguous: 57014 is a statement timeout, a user cancellation or a recovery
// conflict, and 40001 on a standby is a recovery conflict rather than a
// serialisation failure. Those are worth distinguishing — they point at completely
// different things to fix.
func pgErrIssue(state, severity, msg string, pc *pktPGConn) string {
	e, known := pgErrCatalog[state]
	base := ""
	if known {
		base = e.issue
	}
	low := strings.ToLower(msg)

	switch state {
	case "57014":
		switch {
		case strings.Contains(low, "statement timeout"):
			return "Statement cancelled by statement_timeout — the server enforced a limit the application may not know about"
		case strings.Contains(low, "conflict with recovery"):
			return "Query cancelled by conflict with recovery — this is a standby, and WAL replay needed rows the query was still reading; max_standby_streaming_delay or hot_standby_feedback decides who wins"
		case strings.Contains(low, "user request"):
			return "Query cancelled at the client's request — a CancelRequest arrived on a second connection, which is how a client-side timeout or an interrupted session looks on the wire"
		case strings.Contains(low, "lock timeout"):
			return "Statement cancelled by lock_timeout — it waited for a lock and gave up"
		}
	case "40001":
		if strings.Contains(low, "conflict with recovery") {
			return "Serialisation failure caused by recovery conflict on a standby — WAL replay removed rows the query still needed"
		}
	case "53300":
		// The message carries the limit, which is the number worth reading.
		if strings.Contains(low, "max_connections") || strings.Contains(low, "connection slots") {
			return base + ". Reserved superuser slots are what let an administrator still get in when this happens"
		}
	case "0A000":
		if strings.Contains(low, "unsupported frontend protocol") {
			return "The client asked for a protocol version this server does not speak — everything from a port scanner to a client built against a much newer libpq; the connection is closed immediately"
		}
	case "08P01":
		if pc != nil && pc.replication != "" {
			return "Protocol violation on a replication connection — the standby and primary disagree about the stream; a version mismatch or a corrupted slot"
		}
	case "28000":
		if strings.Contains(low, "no pg_hba.conf entry") {
			return "No pg_hba.conf entry matched — the user, database, source address and requested auth method combination is not allowed; this is a configuration answer, not a password answer"
		}
		if strings.Contains(low, "not permitted to log in") || strings.Contains(low, "nologin") {
			return "The role is not permitted to log in (NOLOGIN) — it exists for grants, not for sessions"
		}
	case "3D000":
		if pc != nil && pc.replication == "logical" {
			return "The database named on a logical replication connection does not exist on this server"
		}
	}
	if base != "" {
		return base
	}
	// An unknown state still deserves its class, but only when the class says
	// something operational. An unrecognised syntax error is not a finding.
	if len(state) >= 2 {
		switch strings.ToUpper(state[:2]) {
		case "08", "53", "57", "58", "XX", "F0":
			if cls := pgClassNames[strings.ToUpper(state[:2])]; cls != "" {
				return fmt.Sprintf("%s (%s) — %s", state, cls, pktEllipsis(pktPrintable(msg), 100))
			}
		}
	}
	return ""
}

// pgNoticeIssue picks out the notices that explain everything after them. A notice
// is normally noise — "relation already exists, skipping" — but the shutdown and
// recovery messages arrive as notices and are the reason the next hundred frames
// look the way they do.
func pgNoticeIssue(severity, msg string) string {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "terminating connection due to administrator command"):
		return "The server is terminating this connection on an administrator's instruction — a restart, a pg_terminate_backend, or a Patroni-driven role change"
	case strings.Contains(low, "shutting down"), strings.Contains(low, "shutdown"):
		return "The server is shutting down — every connection is about to end, and connection failures after this point are consequences rather than separate faults"
	case strings.Contains(low, "conflict with recovery"):
		return "Recovery conflict on a standby — WAL replay is contending with queries that are still reading"
	case strings.Contains(low, "checkpoints are occurring too frequently"):
		return "Checkpoints are occurring too frequently — max_wal_size is too small for the write rate, and the resulting I/O shows up as latency in this capture"
	case strings.EqualFold(severity, "FATAL"), strings.EqualFold(severity, "PANIC"):
		return strings.ToUpper(severity) + " notice: " + pktEllipsis(pktPrintable(msg), 120)
	}
	return ""
}

// pgSevereStates lists the states worth a one-click filter, newest-first order being
// meaningless here — the UI sorts them.
func pgSevereStates() []string {
	var out []string
	for state, e := range pgErrCatalog {
		if e.severe {
			out = append(out, fmt.Sprintf("%s %s", state, e.name))
		}
	}
	sort.Strings(out)
	return out
}
