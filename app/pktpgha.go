package main

// pktpgha.go — the traffic around a PostgreSQL cluster that is not the PostgreSQL
// protocol.
//
// A Patroni member carries four conversations, and only one of them is the protocol
// pktpg.go decodes:
//
//	5432  clients ↔ PostgreSQL, and standbys streaming WAL from the leader. One
//	      port, both roles: replication is a normal connection with
//	      replication=true in its startup parameters (see pktpg.go).
//	8008  Patroni's REST API. HAProxy polls /primary and /replica here to decide
//	      where to route, patronictl drives switchovers through it, and every
//	      member exposes /cluster. Plain HTTP/1.1, and readable.
//	2379  etcd client API — where Patroni takes and renews the leader lock. This is
//	      the DCS: if a member cannot reach it, that member gives up the leader
//	      lock and demotes itself, whatever PostgreSQL is doing.
//	2380  etcd peer traffic — raft. Continuous, and the heartbeat that keeps the
//	      etcd cluster's own quorum.
//
// The two etcd ports are described rather than decoded. etcd v3 is gRPC over HTTP/2,
// so the request bodies are protobuf inside HPACK-compressed HTTP/2 frames: reading
// them properly means implementing HPACK with per-connection state, and reading them
// improperly means printing plausible-looking nonsense. What is decodable without
// guessing is the frame layer — HTTP/2 frame types and stream ids are fixed-format —
// and the gRPC method names, which in practice arrive as literal strings in the
// HEADERS frame because etcd's client does not pre-populate a dynamic table for
// them. Those are reported as what they are: strings seen in a header frame.
//
// Why bother at all, rather than leaving it as "TCP data"? Because the failure this
// answers is the one every Patroni cluster eventually has: PostgreSQL is fine, the
// application is down, and the reason is that a member lost the DCS and demoted
// itself. On the wire that is visible — the etcd traffic stops or starts failing
// seconds before the PostgreSQL side changes — and no PostgreSQL-only capture can
// show it.

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Default ports for a PostgreSQL-family node. patroniRESTPort, etcdClientPort and
// etcdPeerPort are patroni.go's — the same constants the deployment itself uses, so
// the capture cannot drift from what is actually listening. An All-in-One instance
// uses its slot's ports instead (aioPortsFor), which is why the capture request
// carries a port→role map rather than these constants.
const (
	pgClientPort  = patroniPGPort // 5432, and pg.go's standalone port too
	pgBouncerPort = 6432
	// HAProxy in front of a Patroni/repmgr/Spock cluster: writes on 5000 to the
	// leader, reads round-robin on 5001. Both carry the PostgreSQL protocol.
	pgProxyRWPort = haproxyWritePort
	pgProxyROPort = haproxyReadPort
)

// Roles. pktRolePostgres decodes as the PostgreSQL protocol; the rest do not.
const (
	pktRolePostgres    = "postgres"
	pktRolePatroniREST = "patroni-rest"
	pktRoleEtcdClient  = "etcd-client"
	pktRoleEtcdPeer    = "etcd-peer"
)

// pktPGPortRoles is the role map for a PostgreSQL-family node. Every port that
// carries the frontend/backend protocol maps to pktRolePostgres — a connection
// through pgBouncer or HAProxy is the same protocol as a direct one, which is
// precisely why those proxies can sit in front of it.
func pktPGPortRoles(clientPort int) map[int]string {
	return map[int]string{
		clientPort:      pktRolePostgres,
		pgBouncerPort:   pktRolePostgres,
		pgProxyRWPort:   pktRolePostgres,
		pgProxyROPort:   pktRolePostgres,
		patroniRESTPort: pktRolePatroniREST,
		etcdClientPort:  pktRoleEtcdClient,
		etcdPeerPort:    pktRoleEtcdPeer,
	}
}

// pktPGIsPostgresRole reports whether a role is decoded by pktpg.go.
func pktPGIsPostgresRole(role string) bool { return role == pktRolePostgres }

// pktPGRoleLabel is how these roles appear in the protocol column. It is separate
// from pktRoleLabel (pktgalera.go) so neither engine's labels leak into the other's.
func pktPGRoleLabel(role string) string {
	switch role {
	case pktRolePatroniREST:
		return "Patroni/REST"
	case pktRoleEtcdClient:
		return "etcd/client"
	case pktRoleEtcdPeer:
		return "etcd/raft"
	case pktRolePostgres:
		return "PostgreSQL"
	}
	return ""
}

// pktPGRoleDescription explains a role once, for the UI and the docs.
func pktPGRoleDescription(role string) string {
	switch role {
	case pktRolePatroniREST:
		return "Patroni's REST API: HAProxy's health checks, patronictl, and every member's view of the cluster"
	case pktRoleEtcdClient:
		return "etcd client API: where Patroni takes and renews the leader lock — losing it demotes the member"
	case pktRoleEtcdPeer:
		return "etcd peer traffic: raft heartbeats and log replication between the etcd members themselves"
	case pktRolePostgres:
		return "PostgreSQL frontend/backend protocol — client sessions and WAL streaming alike"
	}
	return ""
}

// ---------------------------------------------------------------- Patroni REST

// pktPatroniDecode reads the HTTP/1.1 exchange on Patroni's REST port.
//
// The endpoints answer by status code rather than by body, which is the whole point:
// GET /primary is 200 on the leader and 503 everywhere else, so HAProxy needs no
// PostgreSQL knowledge to route correctly. A 503 here is therefore normal traffic,
// not a fault — flagging it would bury the capture in noise. What is worth flagging
// is a 500, and a member that answers nothing at all.
func pktPatroniDecode(p *pktPacket, c *pktConn, dir *pktDirState, fromClient bool, payload []byte) {
	p.Proto = pktPGRoleLabel(pktRolePatroniREST)
	dir.appBytes += len(payload)

	line := pktHTTPFirstLine(payload)
	if line == "" {
		// A body that arrived in its own frame, which is the common case for
		// /cluster and /patroni: the head and the JSON do not share a segment. The
		// JSON is the interesting half, so it is read here rather than skipped.
		if note := pktPatroniJSONNote(string(payload)); note != "" {
			p.Info = fmt.Sprintf("Patroni response body (%d bytes): %s", len(payload), note)
			return
		}
		p.Info = fmt.Sprintf("Patroni REST data, %d bytes", len(payload))
		return
	}
	if fromClient {
		method, path := pktHTTPRequest(line)
		if method == "" {
			p.Info = pktEllipsis(line, 120)
			return
		}
		c.pgHA().lastPath = path
		p.Command = method + " " + path
		p.Info = fmt.Sprintf("%s %s — %s", method, path, pktPatroniEndpoint(method, path))
		return
	}
	code, reason := pktHTTPStatus(line)
	if code == 0 {
		p.Info = pktEllipsis(line, 120)
		return
	}
	path := c.pgHA().lastPath
	p.Status = strconv.Itoa(code) + " " + reason
	p.Info = fmt.Sprintf("HTTP %d %s%s", code, reason, pktPatroniStatusNote(path, code))
	// A JSON body on /cluster or /patroni says which member this is and what it
	// thinks it is doing — the two facts a capture cannot otherwise establish.
	if body := pktHTTPBody(payload); body != "" {
		if note := pktPatroniJSONNote(body); note != "" {
			p.Info += " | " + note
		}
	}
	if code >= 500 && code != 503 {
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Patroni REST returned %d — Patroni itself is failing, not just declining to be the leader; HAProxy will route away from this member and patronictl will not be able to drive it either", code))
	}
}

// pktPatroniEndpoint says what an endpoint is for. These are the ones a capture
// actually contains; the rest are named generically.
func pktPatroniEndpoint(method, path string) string {
	base := strings.SplitN(strings.TrimPrefix(path, "/"), "?", 2)[0]
	switch strings.ToLower(base) {
	case "primary", "master", "leader", "read-write":
		return "\"am I the leader?\" — 200 yes, 503 no; this is HAProxy's write-port health check"
	case "replica", "read-only":
		return "\"am I a healthy replica?\" — HAProxy's read-port health check"
	case "sync", "quorum", "async":
		return "synchronous-standby state check"
	case "health", "liveness", "readiness":
		return "liveness/readiness probe"
	case "cluster":
		return "the whole cluster's state as this member sees it (what patronictl list prints)"
	case "patroni", "":
		return "this member's own state, timeline and role"
	case "config":
		if method == "PATCH" || method == "PUT" {
			return "changing the cluster-wide configuration held in the DCS"
		}
		return "the cluster-wide configuration held in the DCS"
	case "switchover":
		return "a controlled role change — the leader steps down to a named candidate"
	case "failover":
		return "a failover — promoting a member without the current leader's cooperation"
	case "restart", "reload":
		return "restarting or reloading PostgreSQL through Patroni"
	case "reinitialize":
		return "wiping this member's data directory and re-cloning it from the leader"
	}
	return "Patroni REST endpoint"
}

// pktPatroniStatusNote reads a status code in the light of the endpoint it answers.
func pktPatroniStatusNote(path string, code int) string {
	base := strings.ToLower(strings.SplitN(strings.TrimPrefix(path, "/"), "?", 2)[0])
	switch {
	case code == 200 && (base == "primary" || base == "master" || base == "leader"):
		return " — this member IS the leader"
	case code == 503 && (base == "primary" || base == "master" || base == "leader"):
		return " — this member is not the leader (normal for a replica)"
	case code == 200 && (base == "replica" || base == "read-only"):
		return " — a healthy replica"
	case code == 503 && (base == "replica" || base == "read-only"):
		return " — not a healthy replica right now (the leader answers this way too)"
	case code == 202:
		return " — accepted; the action runs asynchronously"
	case code == 412:
		return " — precondition failed: the cluster is not in a state that allows it"
	case code == 503:
		return " — declining"
	}
	return ""
}

// pktPatroniJSONNote pulls the few fields worth reading out of a Patroni JSON body
// without unmarshalling it: the response can be large, and a capture's copy may be
// truncated by the snaplen, which json.Unmarshal would simply reject.
func pktPatroniJSONNote(body string) string {
	var parts []string
	for _, key := range []string{"role", "state", "timeline", "pending_restart", "paused"} {
		if v := pktJSONScalar(body, key); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	if strings.Contains(body, "\"scheduled_switchover\"") {
		parts = append(parts, "a switchover is scheduled")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// pktJSONScalar finds "key": value for a string, number or boolean, flatly and
// without parsing. Good enough to read a status body, and it cannot fail on a
// truncated one.
func pktJSONScalar(s, key string) string {
	i := strings.Index(s, "\""+key+"\"")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(s[i+len(key)+2:])
	if !strings.HasPrefix(rest, ":") {
		return ""
	}
	rest = strings.TrimSpace(rest[1:])
	if strings.HasPrefix(rest, "\"") {
		if end := strings.IndexByte(rest[1:], '"'); end >= 0 {
			return pktPrintable(rest[1 : 1+end])
		}
		return ""
	}
	end := strings.IndexAny(rest, ",}\n")
	if end < 0 {
		end = len(rest)
	}
	return pktPrintable(strings.TrimSpace(rest[:end]))
}

// ---------------------------------------------------------------- etcd

// pktEtcdDecode describes etcd traffic: the HTTP/2 frame layer for the client API,
// HTTP/1.1 request lines for raft, and volume for both.
func pktEtcdDecode(p *pktPacket, c *pktConn, dir *pktDirState, role string, fromClient bool, payload []byte) {
	p.Proto = pktPGRoleLabel(role)
	dir.appBytes += len(payload)
	ha := c.pgHA()

	// The HTTP/2 connection preface, which is how a gRPC connection opens.
	if strings.HasPrefix(string(payload), "PRI * HTTP/2.0") {
		ha.http2 = true
		p.Info = "HTTP/2 connection preface — gRPC (etcd v3 API) begins"
		return
	}
	if line := pktHTTPFirstLine(payload); line != "" && !ha.http2 {
		if method, path := pktHTTPRequest(line); method != "" {
			ha.lastPath = path
			p.Command = method + " " + path
			p.Info = fmt.Sprintf("%s %s — %s", method, path, pktEtcdPath(path))
			return
		}
		if code, reason := pktHTTPStatus(line); code != 0 {
			p.Status = strconv.Itoa(code) + " " + reason
			p.Info = fmt.Sprintf("HTTP %d %s (%s)", code, reason, pktEtcdPath(ha.lastPath))
			if code >= 500 {
				p.Issues = append(p.Issues, fmt.Sprintf(
					"etcd answered %d — a member that cannot read or write the DCS gives up the Patroni leader lock and demotes itself, whatever PostgreSQL is doing", code))
			}
			return
		}
	}
	if ha.http2 || len(payload) >= 9 {
		if info, method := pktHTTP2Frames(payload); info != "" {
			ha.http2 = true
			if method != "" {
				p.Command = method
				p.Info = fmt.Sprintf("%s | gRPC method seen in headers: %s", info, method)
				if note := pktEtcdMethodNote(method); note != "" {
					p.Info += " — " + note
				}
				return
			}
			p.Info = info
			return
		}
	}
	p.Info = fmt.Sprintf("%s data, %d bytes (%s so far)",
		pktPGRoleLabel(role), len(payload), pktBytes(dir.appBytes))
}

// pktEtcdPath explains the etcd HTTP paths a Patroni cluster produces.
func pktEtcdPath(path string) string {
	switch {
	case path == "":
		return "etcd"
	case strings.HasPrefix(path, "/raft/stream"):
		return "raft stream: a long-lived connection carrying log entries and heartbeats between etcd members"
	case strings.HasPrefix(path, "/raft"):
		return "raft message"
	case strings.HasPrefix(path, "/v3/kv/txn"):
		return "a transaction — how Patroni takes the leader lock atomically"
	case strings.HasPrefix(path, "/v3/kv/range"):
		return "reading keys — the cluster state Patroni acts on"
	case strings.HasPrefix(path, "/v3/kv/put"):
		return "writing a key — a member updating its own state"
	case strings.HasPrefix(path, "/v3/lease"):
		return "a lease — the TTL behind the leader lock; when it expires, the leader is gone"
	case strings.HasPrefix(path, "/v3/watch"):
		return "a watch — waiting for another member's change"
	case strings.HasPrefix(path, "/v2/keys"):
		return "etcd v2 key API"
	case strings.HasPrefix(path, "/version"), strings.HasPrefix(path, "/health"):
		return "etcd liveness check"
	}
	return "etcd API"
}

// pktEtcdMethodNote explains the gRPC methods worth naming.
func pktEtcdMethodNote(m string) string {
	switch {
	case strings.Contains(m, "Lease/LeaseKeepAlive"):
		return "renewing the lease that holds the leader lock; when these stop, the lock expires and the cluster fails over"
	case strings.Contains(m, "Lease/LeaseGrant"):
		return "taking a new lease"
	case strings.Contains(m, "KV/Txn"):
		return "an atomic compare-and-set — how the leader lock is contested"
	case strings.Contains(m, "KV/Range"):
		return "reading cluster state"
	case strings.Contains(m, "KV/Put"):
		return "writing member state"
	case strings.Contains(m, "Watch/Watch"):
		return "watching for changes made by other members"
	case strings.Contains(m, "Maintenance/Status"):
		return "an etcd health/status check"
	}
	return ""
}

// pktHTTP2Frames names the HTTP/2 frames in a payload and, if a HEADERS frame
// happens to carry a readable path, returns it.
//
// HPACK is not decoded: the strings that appear are the ones sent as literals, which
// for etcd's client is the :path in practice. Anything else is reported as a frame
// type and a length, which is honest and still useful — a stream of DATA frames with
// no HEADERS is a long-lived watch, and PING frames are the client checking a
// connection it has not used for a while.
func pktHTTP2Frames(b []byte) (info, method string) {
	names := map[byte]string{
		0x0: "DATA", 0x1: "HEADERS", 0x2: "PRIORITY", 0x3: "RST_STREAM",
		0x4: "SETTINGS", 0x5: "PUSH_PROMISE", 0x6: "PING", 0x7: "GOAWAY",
		0x8: "WINDOW_UPDATE", 0x9: "CONTINUATION",
	}
	var seen []string
	off, frames := 0, 0
	for off+9 <= len(b) && frames < 8 {
		l := int(b[off])<<16 | int(b[off+1])<<8 | int(b[off+2])
		typ := b[off+3]
		name, ok := names[typ]
		if !ok || l < 0 || off+9+l > len(b) {
			break
		}
		frames++
		stream := binary.BigEndian.Uint32(b[off+5:]) & 0x7fffffff
		switch typ {
		case 0x1, 0x9: // HEADERS / CONTINUATION
			if m := pktGRPCMethod(b[off+9 : off+9+l]); m != "" && method == "" {
				method = m
			}
			seen = append(seen, fmt.Sprintf("%s(stream %d)", name, stream))
		case 0x7:
			seen = append(seen, "GOAWAY — the connection is being closed by the peer")
		case 0x3:
			seen = append(seen, fmt.Sprintf("RST_STREAM(stream %d)", stream))
		default:
			seen = append(seen, fmt.Sprintf("%s(%d bytes)", name, l))
		}
		off += 9 + l
	}
	if len(seen) == 0 {
		return "", ""
	}
	return "HTTP/2 " + strings.Join(seen, ", "), method
}

// pktGRPCMethod looks for a gRPC method path inside a HEADERS frame. It is a
// substring search on purpose — see the file comment.
func pktGRPCMethod(b []byte) string {
	s := string(b)
	i := strings.Index(s, "/etcdserverpb.")
	if i < 0 {
		return ""
	}
	end := i
	for end < len(s) && (s[end] == '/' || s[end] == '.' ||
		(s[end] >= 'A' && s[end] <= 'Z') || (s[end] >= 'a' && s[end] <= 'z') ||
		(s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	return s[i:end]
}

// ---------------------------------------------------------------- HTTP helpers

// pktHTTPFirstLine returns the first CRLF-terminated line of a payload if it looks
// like the head of an HTTP/1.x message.
func pktHTTPFirstLine(b []byte) string {
	if len(b) < 5 || len(b) > 1<<20 {
		return ""
	}
	end := len(b)
	if i := indexByte(b, '\r'); i > 0 {
		end = i
	} else if i := indexByte(b, '\n'); i > 0 {
		end = i
	} else {
		return ""
	}
	line := string(b[:end])
	if !pktMostlyPrintable(b[:end]) {
		return ""
	}
	if strings.HasPrefix(line, "HTTP/") || strings.Contains(line, " HTTP/1.") {
		return line
	}
	return ""
}

// pktHTTPRequest splits a request line into method and path.
func pktHTTPRequest(line string) (method, path string) {
	f := strings.Fields(line)
	if len(f) < 3 || !strings.HasPrefix(f[2], "HTTP/") {
		return "", ""
	}
	switch f[0] {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return f[0], f[1]
	}
	return "", ""
}

// pktHTTPStatus splits a status line into code and reason.
func pktHTTPStatus(line string) (int, string) {
	if !strings.HasPrefix(line, "HTTP/") {
		return 0, ""
	}
	f := strings.SplitN(line, " ", 3)
	if len(f) < 2 {
		return 0, ""
	}
	code, err := strconv.Atoi(f[1])
	if err != nil || code < 100 || code > 599 {
		return 0, ""
	}
	reason := ""
	if len(f) == 3 {
		reason = pktPrintable(f[2])
	}
	return code, reason
}

// pktHTTPBody returns the body of an HTTP/1.x message in this payload, if the head
// and the start of the body arrived together (they do for these small responses).
func pktHTTPBody(b []byte) string {
	i := strings.Index(string(b), "\r\n\r\n")
	if i < 0 || i+4 >= len(b) {
		return ""
	}
	body := b[i+4:]
	if len(body) > 4096 {
		body = body[:4096]
	}
	if !pktMostlyPrintable(body) {
		return ""
	}
	return string(body)
}

// pktPGHA is the per-connection state these decoders need: which path the last
// request asked for, so a response can be read in its light, and whether the
// connection turned out to be HTTP/2.
type pktPGHA struct {
	lastPath string
	http2    bool
}

func (c *pktConn) pgHA() *pktPGHA {
	if c.pgha == nil {
		c.pgha = &pktPGHA{}
	}
	return c.pgha
}
