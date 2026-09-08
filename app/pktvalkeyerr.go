package main

// pktvalkeyerr.go — Valkey's error replies, and what each one means for whoever is looking
// at the capture.
//
// Valkey has no error *numbers*. An error reply is a line of text whose first word is,
// by convention, an uppercase code: `-WRONGTYPE Operation against a key…`,
// `-MOVED 3999 127.0.0.1:6381`, `-NOAUTH Authentication required.`. That convention is
// the whole diagnostic surface, and it is machine-readable enough to catalogue — which
// matters because the codes that look alike behave completely differently:
//
//	MOVED   the slot has moved permanently. The client must update its slot map.
//	ASK     the slot is migrating and THIS key is already on the far side. One-shot:
//	        the client must resend with ASKING and must NOT update its map.
//
// A client that treats them the same either caches a redirect it should not (ASK) or
// re-asks forever (MOVED). A capture is where that is visible, because both look like
// ordinary error replies to the application.
//
// The other thing worth writing down is which errors mean *the server is refusing all
// writes* — LOADING, MISCONF, READONLY, OOM, NOREPLICAS. Each of those is an outage with a
// specific cause, and each is a single line in a capture that an application usually
// reports as "Redis is down".

import (
	"fmt"
	"sort"
	"strings"
)

// valkeyErrEntry is one error prefix and what it means operationally.
type valkeyErrEntry struct {
	issue  string
	severe bool
}

var valkeyErrCatalog = map[string]valkeyErrEntry{
	// ---- cluster redirection. Not failures: the protocol working as designed — unless
	// there are thousands of them, which means the client is not learning.
	"MOVED":       {"MOVED — the slot this key belongs to is served by another node. The client is expected to update its slot map and retry there; a steady stream of these means it is not caching the map at all, and every operation is costing two round trips", true},
	"ASK":         {"ASK — the slot is mid-migration and this key is already on the target node. This is one-shot: the client must retry with ASKING and must NOT update its slot map, because the slot itself has not moved yet. Treating ASK like MOVED corrupts a client's routing until the next full refresh", true},
	"TRYAGAIN":    {"TRYAGAIN — a multi-key command spanned keys that are mid-migration, so it cannot be completed atomically right now. The client should retry after a short pause", true},
	"CROSSSLOT":   {"CROSSSLOT — a multi-key command named keys in different hash slots. In cluster mode that is not executable at all; hash tags ({user:1}) are how keys are forced into one slot", true},
	"CLUSTERDOWN": {"CLUSTERDOWN — the cluster is not serving: slots are uncovered or too few primaries are reachable. Every key in an uncovered slot is unavailable, and cluster-require-full-coverage decides whether the rest still serve", true},
	"MASTERDOWN":  {"MASTERDOWN — this replica has lost its primary and replica-serve-stale-data is off, so it refuses reads rather than serving data that may be behind", true},

	// ---- the server is refusing writes. Each of these is an outage with a cause.
	"LOADING":    {"LOADING — the node is reading its dataset from disk (an RDB or AOF) and cannot serve anything yet. After a restart or a FULLRESYNC this is what clients see, and it lasts as long as the dataset takes to load", true},
	"MISCONF":    {"MISCONF — writes are refused because a background save keeps failing (usually a full or read-only disk). Valkey stops accepting writes on purpose so a dataset it cannot persist does not keep growing; the fix is on the filesystem, not in the client", true},
	"READONLY":   {"READONLY — a write reached a replica. Either the client's topology is stale after a failover, or a read-only replica is behind a load balancer that is sending it writes", true},
	"OOM":        {"OOM — used_memory is above maxmemory and the eviction policy cannot free anything (noeviction, or every key is protected). Writes are refused while reads still work, which is why an application sees this as half-broken", true},
	"NOREPLICAS": {"NOREPLICAS — a write was rejected because min-replicas-to-write is not satisfied: not enough replicas are connected and current, so the primary refuses writes it could not replicate", true},
	"NOWRITE":    {"Writes are refused by this node", true},

	// ---- authentication and permissions.
	"NOAUTH":    {"NOAUTH — the connection has not authenticated and the server requires it (requirepass or an ACL user). The client's password is missing or it reconnected without re-sending AUTH", true},
	"WRONGPASS": {"WRONGPASS — the username or password is wrong. After an ACL change this is what every client does at once", true},
	"NOPERM":    {"NOPERM — the authenticated ACL user is not allowed this command, key pattern or channel. The connection works and the operation does not, which is why it looks like a bug in the application", true},

	// ---- scripting.
	"NOSCRIPT":   {"NOSCRIPT — EVALSHA named a script the server does not have cached. Normal after a restart or SCRIPT FLUSH; the client is expected to fall back to EVAL with the body", false},
	"BUSY":       {"BUSY — a Lua script or a function has been running too long and is still holding the server. Valkey executes one command at a time, so nothing else is being served; SCRIPT KILL ends it unless it has already written", true},
	"UNKILLABLE": {"UNKILLABLE — the running script has already written data, so it cannot be killed without breaking atomicity. Only SHUTDOWN NOSAVE ends this state", true},

	// ---- transactions and blocking.
	"EXECABORT": {"EXECABORT — the transaction was discarded because a command inside MULTI was rejected when it was queued. Nothing in it ran", false},
	"UNBLOCKED": {"UNBLOCKED — a blocking command was released by CLIENT UNBLOCK or because the key's type changed underneath it", false},
	"NOGROUP":   {"NOGROUP — the stream or consumer group named does not exist", false},

	// ---- ordinary application errors: reported, never flagged.
	"WRONGTYPE": {"", false},
	"ERR":       {"", false},
	"NOTBUSY":   {"", false},
	"BUSYKEY":   {"", false},
	"NOTMASTER": {"", false},
}

// valkeySplitError splits an error reply into its code word and the rest.
func valkeySplitError(s string) (string, string) {
	s = strings.TrimSpace(s)
	code, rest, ok := strings.Cut(s, " ")
	if !ok {
		return code, ""
	}
	// Only an all-caps first word is a code by convention; anything else is a plain
	// message and must not be presented as one.
	for _, c := range code {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return "ERR", s
		}
	}
	return code, rest
}

// valkeyErrIssue is the line the Issues list gets, or "" for an error that needs no
// commentary.
//
// Two codes are read together with the command that caused them, because the same code
// means different things depending on what asked: a MOVED in reply to a cluster-aware
// client's first attempt is routine, and a stream of them is a client with no slot map;
// an ERR whose text names an unknown command is a version mismatch rather than a bug.
func valkeyErrIssue(code, full string, req valkeyPending, vc *pktValkeyConn) string {
	e, known := valkeyErrCatalog[code]
	if known && e.issue != "" {
		// MOVED and ASK carry their target, which is the useful half.
		switch code {
		case "MOVED", "ASK":
			if f := strings.Fields(full); len(f) >= 3 {
				return fmt.Sprintf("%s → slot %s is on %s. %s", code, f[1], f[2], e.issue)
			}
		}
		return e.issue
	}
	if known {
		return "" // deliberately silent
	}
	low := strings.ToLower(full)
	switch {
	case strings.Contains(low, "unknown command"):
		return "Unknown command — the server does not have this command. A client built for a newer Valkey (or for a module that is not loaded here) talking to this server"
	case strings.Contains(low, "wrong number of arguments"):
		return "" // an application bug, not an operational finding
	case strings.Contains(low, "max number of clients"):
		return "maxclients reached — the server is refusing new connections. Existing ones are unaffected, which is why this looks like an intermittent outage from the outside"
	case strings.Contains(low, "protocol error"):
		return "Protocol error — the server could not parse what the client sent and will close the connection. A broken client library, a proxy corrupting the stream, or something that is not RESP talking to the port"
	case strings.Contains(low, "no such key"):
		return ""
	}
	// An unrecognised uppercase code still gets a line: Valkey and its modules add them
	// faster than any catalogue keeps up.
	if code != "ERR" && code != "" {
		return fmt.Sprintf("-%s — %s", code, pktEllipsis(pktPrintable(full), 120))
	}
	return ""
}

// valkeySevereCodes lists the codes worth a one-click filter.
func valkeySevereCodes() []string {
	var out []string
	for code, e := range valkeyErrCatalog {
		if e.severe {
			out = append(out, code)
		}
	}
	sort.Strings(out)
	return out
}
