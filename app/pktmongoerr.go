package main

// pktmongoerr.go — MongoDB's error codes, and what each one means for whoever is
// looking at the capture.
//
// The MySQL and PostgreSQL halves of this (pktErrCatalog, pgErrCatalog) exist because a
// number or a SQLSTATE on its own tells a reader nothing. MongoDB is a step better and a
// step worse at once: better, because every error carries a `codeName` string, so the
// name needs no lookup at all; worse, because the *consequence* is even less obvious.
// "NotWritablePrimary" does not say "your replica set is holding an election and every
// write in the application is failing for the next two seconds", and that is the part
// worth writing down.
//
// So the catalogue here is thinner on names (the server supplies them) and thicker on
// consequences. It also covers the codes that a capture is the ONLY place to see
// properly: MongoDB reports a failed command in the reply body as `ok: 0`, not by any
// transport signal, and reports a partly-failed write in a `writeErrors` array inside an
// otherwise successful reply. Neither is visible to a tool watching connections.

import (
	"fmt"
	"sort"
	"strings"
)

// mongoErrEntry is one error code: the server's own name for it, what it means
// operationally, and whether it is worth a one-click filter.
type mongoErrEntry struct {
	name   string
	issue  string
	severe bool
}

// mongoErrCatalog covers the codes a capture of a real cluster actually contains. The
// full list is several hundred entries in the server's own error_codes.yml; what is here
// is the network, election, sharding, resource and write-concern families, plus the
// handful of application errors worth naming.
var mongoErrCatalog = map[int]mongoErrEntry{
	// ---- the network and the host.
	6:    {"HostUnreachable", "The member could not be reached at all — this is one node's view of another, so a capture on this node shows the attempt and nothing coming back", true},
	7:    {"HostNotFound", "The member's hostname did not resolve. In a replica set the names come from the set's own configuration, so this is a DNS or a reconfiguration problem, not a client one", true},
	89:   {"NetworkTimeout", "A network operation timed out — the peer was reachable but did not answer in time; on a heartbeat this is what starts an election", true},
	9001: {"SocketException", "The socket failed mid-operation. What the capture shows is the reset or the missing response that caused it", true},
	202:  {"NetworkInterfaceExceededTimeLimit", "An internal request between members exceeded its deadline", true},

	// ---- authentication and authorisation.
	18: {"AuthenticationFailed", "Authentication failed — a wrong password, a user in the wrong database, or a member whose keyfile does not match the rest of the set (which fails as __system)", true},
	13: {"Unauthorized", "The authenticated user lacks the privilege for this command. After a role change this is what an application failing everywhere looks like", true},
	11: {"UserNotFound", "The user does not exist in the database it authenticated against — usually admin versus the application database", true},

	// ---- the primary, elections, and read preference. The replica-set family.
	10107: {"NotWritablePrimary", "This member is not the primary, so the write was refused. Either an election is happening, or the driver is holding a stale view of the topology and sending writes to a member that has stepped down", true},
	11602: {"InterruptedDueToReplStateChange", "The operation was killed by a replica-set state change — this member stepped down (or up) while the command was running. Retryable writes handle this; a non-retryable write is lost", true},
	13435: {"NotPrimaryNoSecondaryOk", "A read reached a secondary without a read preference that allows it. The driver's read preference is primary and the connection is not to the primary", true},
	13436: {"NotPrimaryOrSecondary", "The member is neither primary nor secondary — it is starting up, recovering, or rolling back, and cannot serve anything", true},
	189:   {"PrimarySteppedDown", "The primary stepped down while this command was in flight; the write may or may not have been applied and will not be acknowledged", true},
	91:    {"ShutdownInProgress", "The member is shutting down. Everything after this on this connection is a consequence, not a separate fault", true},
	133:   {"FailedToSatisfyReadPreference", "No member matching the read preference could be found — with readPreference secondary and no healthy secondary, or with a tag set nothing matches", true},
	262:   {"ExceededTimeLimit", "The operation exceeded maxTimeMS. This is a client-imposed deadline, so it says the client gave up rather than that the server failed", true},
	50:    {"MaxTimeMSExpired", "The server killed the operation because maxTimeMS expired. On a getMore against a tailable cursor this is normal; anywhere else it is a slow operation the client refused to wait for", true},

	// ---- write concern and durability.
	64:  {"WriteConcernFailed", "The write was applied on this member but not acknowledged by enough members. It is not durable yet and can still be rolled back if this primary steps down", true},
	79:  {"UnknownReplWriteConcern", "The write concern names a mode this set's configuration does not define (a tag set that no longer exists)", true},
	100: {"UnsatisfiableWriteConcern", "The write concern can never be satisfied by this set — w greater than the number of data-bearing members", true},

	// ---- contention.
	112: {"WriteConflict", "Two transactions wrote the same document and this one lost. The application is expected to retry it; a burst of these means the workload is contending on a hot document", true},
	24:  {"LockTimeout", "The operation waited for a lock and gave up", true},
	46:  {"LockBusy", "The lock is held elsewhere", true},
	251: {"NoSuchTransaction", "The transaction this command names does not exist — it was already committed, aborted, or timed out (transactionLifetimeLimitSeconds, 60 s by default)", true},
	225: {"TransactionTooOld", "A newer transaction on the same session has superseded this one", true},
	263: {"OperationNotSupportedInTransaction", "This command cannot run inside a multi-document transaction", false},

	// ---- resources.
	292:   {"QueryExceededMemoryLimitNoDiskUseAllowed", "A sort or aggregation needed more memory than allowed and disk use was not permitted — allowDiskUse, or an index that makes the sort unnecessary", true},
	16819: {"Location16819", "A sort exceeded its memory limit", true},
	17144: {"Location17144", "A pipeline stage exceeded its memory limit", true},

	// ---- sharding.
	13388: {"StaleConfig", "The shard refused the command because the router's routing table is out of date — the chunk has moved. mongos refreshes and retries, so a few of these are normal after a migration; a stream of them means the balancer is moving data or a router is not refreshing", true},
	63:    {"StaleShardVersion", "The shard version the router sent does not match the shard's own — the same situation as StaleConfig, reported by an older server", true},
	118:   {"CannotSplit", "The chunk cannot be split, usually because the shard key has too little cardinality in that range", true},
	82:    {"NoProgressMade", "A chunk migration made no progress and was abandoned", true},
	96:    {"OperationFailed", "The operation failed for a reason the server described in the message rather than in the code", true},

	// ---- namespaces and schema.
	// Driver and monitoring probes: every driver asks for optional commands and
	// parameters that a given deployment may not have, and the failures are how it
	// discovers what is there. Named, never flagged — 21 of these turned up in one
	// two-minute capture of an idle-ish replica set (atlasVersion, getParameter).
	59: {"CommandNotFound", "", false},
	72: {"InvalidOptions", "", false},
	// Named but never flagged: a read of a missing collection returns an empty result, and
	// a command that needs the namespace getting this back is the application's business.
	26:    {"NamespaceNotFound", "", false},
	48:    {"NamespaceExists", "The collection already exists", false},
	27:    {"IndexNotFound", "The index named does not exist", false},
	85:    {"IndexOptionsConflict", "An index with the same name exists with different options", false},
	86:    {"IndexKeySpecsConflict", "An index with the same key pattern exists with a different name", false},
	11000: {"DuplicateKey", "A unique index rejected the document. This arrives inside writeErrors in an otherwise successful reply, which is why a tool watching only command status never sees it", false},
	2:     {"BadValue", "The command's arguments were rejected by the server", false},
	9:     {"FailedToParse", "The command document could not be parsed — usually a driver or a hand-written command, not a server problem", false},
	14:    {"TypeMismatch", "A field had the wrong BSON type", false},
	40:    {"ConflictingUpdateOperators", "The update document uses operators that cannot be combined", false},
	31254: {"Location31254", "An aggregation stage was used where it is not allowed", false},
	4:     {"NoSuchKey", "", false},
	43:    {"CursorNotFound", "The cursor is gone — it was killed, it timed out (10 minutes idle by default), or the server it lived on restarted. A long pause between getMore calls is the usual cause", true},
	237:   {"CursorKilled", "The cursor was killed while it was being read", true},

	// ---- the server itself.
	211:   {"KeyNotFound", "The cluster time key could not be found — usually a member that has not finished starting up", true},
	14031: {"OutOfDiskSpace", "The member is out of disk", true},
	28663: {"Location28663", "", false},
	1:     {"InternalError", "An internal server error — the message and the server's own log are where this has to be followed up", true},
	255:   {"UnrecoverableRollbackError", "A rollback failed unrecoverably; the member needs a resync", true},
}

// mongoCodeName is the server's own name for a code, for the rare replies that carry a
// code without a codeName (older servers, and some write errors).
func mongoCodeName(code int) string {
	if e, ok := mongoErrCatalog[code]; ok {
		return e.name
	}
	if code == 0 {
		return ""
	}
	return fmt.Sprintf("code %d", code)
}

// mongoErrIssue is the line the Issues list gets, or "" for an error that is an ordinary
// application matter and needs no commentary.
//
// Three codes are read together with the message text, because the code alone is
// ambiguous about which of two very different things happened.
func mongoErrIssue(code int, name, msg string, mc *pktMongoConn) string {
	e, known := mongoErrCatalog[code]
	base := ""
	if known {
		base = e.issue
	}
	low := strings.ToLower(msg)

	switch code {
	case 50, 262: // MaxTimeMSExpired / ExceededTimeLimit
		if strings.Contains(low, "tailable") || (mc != nil && mc.kind == mongoKindOplog) {
			// A tailing cursor is *supposed* to time out and be reissued.
			return ""
		}
	case 10107: // NotWritablePrimary
		if strings.Contains(low, "not primary") && mc != nil && mc.kind == mongoKindRouted {
			return "NotWritablePrimary (10107) on a routed command — the mongos sent a write to a shard member that is no longer that shard's primary; it refreshes and retries, and a burst of these means the shard just failed over"
		}
	case 13388, 63: // StaleConfig
		if mc != nil && mc.kind == mongoKindRouted {
			return fmt.Sprintf("%s (%d) — %s. This one arrived on a mongos→shard connection, which is where it is supposed to appear", name, code, base)
		}
	case 18: // AuthenticationFailed
		if strings.Contains(low, "__system") || (mc != nil && mc.kind == mongoKindInternal) {
			return "AuthenticationFailed (18) as __system — the members' keyFile or x.509 identities do not match, so the set cannot form. Nothing an application does will fix this"
		}
	case 11000: // DuplicateKey
		if mc != nil && mc.kind == mongoKindConfig {
			return "DuplicateKey (11000) in the config database — two routers or shards tried to write the same metadata"
		}
	}
	if base != "" {
		// The catalogue's text is the explanation; the label in front of it is what the
		// summary's issue filter shows, and it has to be short and stable. Without this
		// prefix a chip read "A unique index rejected the document. This arrives inside
		// writeErrors in an otherwise…" — the whole sentence, because pktIssueKind cuts
		// at " — " and there was none.
		return fmt.Sprintf("%s (%d) — %s", name, code, base)
	}
	// A code that is in the catalogue with no text is deliberately silent; the fallback
	// below must not talk over that decision.
	if known {
		return ""
	}
	// An unrecognised code still gets a line when its name says something operational,
	// because MongoDB adds codes faster than any catalogue keeps up. "NotFound" is
	// absent from the list on purpose: CommandNotFound and NamespaceNotFound are both
	// ordinary discovery, and neither is worth a finding.
	for _, hint := range []string{"Timeout", "Unreachable", "Shutdown", "StepDown",
		"NotPrimary", "Interrupted", "Exceeded", "Stale", "Network", "Socket", "WriteConcern"} {
		if strings.Contains(name, hint) {
			return fmt.Sprintf("%s (%d) — %s", name, code, pktEllipsis(pktPrintable(msg), 110))
		}
	}
	return ""
}

// mongoSevereCodes lists the codes worth offering as a one-click filter.
func mongoSevereCodes() []string {
	var out []string
	for code, e := range mongoErrCatalog {
		if e.severe {
			out = append(out, fmt.Sprintf("%d %s", code, e.name))
		}
	}
	sort.Strings(out)
	return out
}
