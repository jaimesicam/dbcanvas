package main

// pktvalkey.go — Valkey traffic: RESP on the client port, and what the conversation means.
//
// Valkey's ports follow Galera's model rather than MongoDB's: two protocols on two ports.
//
//	6379   clients AND replication, both RESP (pktresp.go). A replica's link to its
//	       primary starts as an ordinary connection, sends PSYNC, and then never stops.
//	16379  the cluster bus: a binary gossip protocol, decoded in pktvalkeybus.go.
//	26379  Sentinel, which is RESP again — SENTINEL commands plus a pub/sub channel.
//
// What makes the RESP side interesting to decode is that there is **no request id**.
// Replies are matched to commands by order alone, and a client may pipeline fifty
// commands into one segment before reading any of them. So this file keeps a FIFO of
// outstanding commands per connection and pairs each reply with the head of it — which is
// also the only way to report a command's latency at all.
//
// Two conversations on the client port are not request/response and would break that
// queue if they were treated as such:
//
//   - **Replication.** After PSYNC the primary sends +FULLRESYNC, then an RDB payload,
//     then a continuous stream of write commands, forever, with no reply from the
//     replica except periodic REPLCONF ACK. Both ends' offsets are on the wire, which
//     makes replication lag measurable in bytes from the capture alone — the same trick
//     the PostgreSQL decoder plays with LSNs.
//   - **Subscriptions and RESP3 push.** After SUBSCRIBE (or with client tracking on) the
//     server sends unprompted messages. Matching those to queued commands would shift
//     the whole queue by one and mislabel every reply after it.

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The three ports a Valkey deployment uses. valkeyPort (6379) is valkey.go's.
const (
	valkeyClientPort   = valkeyPort
	valkeyBusOffset    = 10000 // the cluster bus is always the client port + 10000
	valkeySentinelPort = 26379
)

// Roles. pktRoleValkey decodes as RESP; the bus is a different protocol entirely.
const (
	pktRoleValkey    = "valkey"
	pktRoleValkeyBus = "valkey-bus"
	pktRoleSentinel  = "valkey-sentinel"
)

// pktValkeyPortRoles is the role map for a Valkey node.
func pktValkeyPortRoles(clientPort int) map[int]string {
	return map[int]string{
		clientPort:                   pktRoleValkey,
		clientPort + valkeyBusOffset: pktRoleValkeyBus,
		valkeySentinelPort:           pktRoleSentinel,
	}
}

// pktValkeyRoleLabel is how these roles appear in the protocol column.
func pktValkeyRoleLabel(role string) string {
	switch role {
	case pktRoleValkey:
		return "Valkey"
	case pktRoleValkeyBus:
		return "Valkey/bus"
	case pktRoleSentinel:
		return "Valkey/sentinel"
	}
	return ""
}

// pktValkeyRoleDescription explains a role once, for the UI and the docs.
func pktValkeyRoleDescription(role string) string {
	switch role {
	case pktRoleValkey:
		return "RESP: client commands and replication, which share this port"
	case pktRoleValkeyBus:
		return "the cluster bus: binary gossip between nodes — heartbeats, failure detection and failover votes"
	case pktRoleSentinel:
		return "Sentinel: monitoring and failover coordination for a non-clustered primary/replica pair"
	}
	return ""
}

// Connection kinds on the client port, which carries three different conversations.
const (
	valkeyKindClient  = "client"
	valkeyKindRepl    = "replication"
	valkeyKindSub     = "subscribe"
	valkeyKindMonitor = "monitor" // a MONITOR stream, or an INFO-polling monitor
)

func valkeyKindLabel(kind string) string {
	switch kind {
	case valkeyKindRepl:
		return "Valkey/replication"
	case valkeyKindSub:
		return "Valkey/pubsub"
	case valkeyKindMonitor:
		return "Valkey/monitor"
	}
	return "Valkey"
}

// valkeySlowMS is when a command's reply is worth flagging. Valkey is single-threaded for
// command execution and its own slowlog defaults to 10 ms, so this is deliberately much
// tighter than the SQL engines' 100 ms: on Valkey, 50 ms is a stall that blocked every
// other client too.
const valkeySlowMS = 50.0

// valkeyBigReplyBytes is when a reply is worth calling heavy.
const valkeyBigReplyBytes = 1 << 20

// valkeyLagBytes is the replication lag worth a line: the difference between the
// primary's offset and the replica's acknowledged offset.
const valkeyLagBytes = 8 << 20

// pktValkeyDir is one direction's state.
type pktValkeyDir struct {
	aligned  bool
	desynced bool
	// rdbRemaining is how many bytes of an in-flight RDB transfer are still to come.
	// While it is non-zero the bytes are not RESP at all.
	rdbRemaining int
	rdbTotal     int
	rdbDiskless  bool
	rdbBytes     int
	// expectRDB is set between a +FULLRESYNC reply and the RDB header that follows it.
	expectRDB bool
	// eofMark is the 40-byte delimiter a diskless transfer ends with ($EOF:<mark>).
	eofMark string
	// keepalives counts the bare newlines a primary sends while it forks and saves, to
	// stop the replica timing out. They are not RESP and must not reach the parser.
	keepalives int
	kaNoted    bool
}

// pktValkeyConn is per-connection state.
type pktValkeyConn struct {
	kind    string
	sawResp bool
	// pending is the FIFO of commands awaiting replies. RESP has no request id, so this
	// queue IS the correlation.
	pending []valkeyPending
	// Command bookkeeping for the summary.
	commands int
	authUser string
	respVer  int // 2 or 3, from HELLO
	// Replication: the primary's offset (from REPLCONF GETACK / the stream) and the
	// replica's acknowledged offset, which together are the lag.
	replID      string
	primaryOff  int64
	replicaOff  int64
	lagNoted    bool
	fullResync  bool
	partialSync bool
	subChannels int
	// Pipelining: the deepest batch seen, which is a property of the client worth
	// reporting once rather than per frame.
	maxPipeline  int
	pipelineNote bool
	dangerNoted  map[string]bool
}

// valkeyPending is a command awaiting its reply.
type valkeyPending struct {
	name string
	arg  string
	ts   time.Time
}

func (c *pktConn) valkeyConn() *pktValkeyConn {
	if c.valkey == nil {
		c.valkey = &pktValkeyConn{respVer: 2, dangerNoted: map[string]bool{}}
	}
	return c.valkey
}

func (d *pktDirState) valkeyDir() *pktValkeyDir {
	if d.valkey == nil {
		d.valkey = &pktValkeyDir{}
	}
	return d.valkey
}

// ---------------------------------------------------------------- entry point

// pktValkeyDecode consumes one frame's payload for a connection direction. Same contract
// as the other three engines' decoders.
func pktValkeyDecode(p *pktPacket, c *pktConn, dir *pktDirState, fromClient bool, payload []byte, ts time.Time) {
	vc := c.valkeyConn()
	vd := dir.valkeyDir()

	if c.tls {
		p.Proto = "TLS"
		p.Info = pktTLSInfo(payload, c.tlsSealed)
		p.Status = "Encrypted"
		if strings.Contains(p.Info, "Alert") {
			p.Issues = append(p.Issues,
				"TLS alert — handshake or session rejected; the client reports an SSL error and the connection ends")
		}
		if !c.tlsSealed && pktTLSSeals(payload) {
			c.tlsSealed = true
		}
		return
	}
	// Valkey's TLS is port-based (tls-port) with no in-band upgrade, so a handshake on
	// the first bytes is unambiguous.
	if !vc.sawResp && pktLooksTLS(payload) {
		c.tls, c.tlsSealed = true, pktTLSSeals(payload)
		p.Proto, p.Status = "TLS", "Encrypted"
		p.Info = pktTLSInfo(payload, false)
		return
	}

	dir.buf = append(dir.buf, payload...)
	var infos []string
	msgs := 0

	// An RDB transfer in flight: these bytes are a serialised dataset, not RESP, and there
	// may be gigabytes of them. They are counted and dropped rather than buffered.
	if vd.rdbRemaining > 0 || vd.rdbDiskless {
		if info := valkeyRDBConsume(p, c, dir, vd); info != "" {
			infos = append(infos, info)
		}
		if vd.rdbRemaining > 0 || vd.rdbDiskless {
			p.Proto = valkeyKindLabel(vc.kind)
			p.Info = strings.Join(infos, " | ")
			return
		}
	}

	for {
		// A primary sends bare "\n" bytes to a syncing replica while it forks and
		// serialises its dataset — a keep-alive so the replica does not time out during a
		// transfer that can take minutes. They are not RESP values, and buffering them
		// desynchronised the parser badly enough that the +FULLRESYNC line which follows
		// was thrown away by the re-anchor. Skip them, and say so once.
		for len(dir.buf) > 0 && (dir.buf[0] == '\n' || dir.buf[0] == '\r') {
			dir.buf = dir.buf[1:]
			vd.keepalives++
		}
		if vd.keepalives > 0 && !vd.kaNoted && vc.kind == valkeyKindRepl {
			vd.kaNoted = true
			infos = append(infos, fmt.Sprintf(
				"%d keep-alive newline(s) — the primary is forking and saving; these stop the replica timing out during the transfer", vd.keepalives))
		}
		// The RDB header, which comes straight after +FULLRESYNC and is NOT an ordinary
		// bulk string: its payload has no trailing CRLF, and it may be megabytes or
		// gigabytes. Parsing it as a bulk string would buffer the whole dataset in memory
		// and then never complete.
		if vd.expectRDB && len(dir.buf) > 0 {
			if info, consumed, done := valkeyRDBHeader(vd, dir.buf); done {
				dir.buf = dir.buf[consumed:]
				vd.expectRDB = false
				infos = append(infos, info)
				if len(dir.buf) > 0 {
					if extra := valkeyRDBConsume(p, c, dir, vd); extra != "" {
						infos = append(infos, extra)
					}
				}
				break
			} else if consumed < 0 {
				// Not an RDB header after all; fall through and parse normally.
				vd.expectRDB = false
			} else {
				break // the header line is still arriving
			}
		}
		if len(dir.buf) == 0 {
			break
		}
		v, n, ok, bad := respParse(dir.buf, 0)
		if bad {
			vd.aligned, vd.desynced = false, true
			if !valkeyReanchor(dir) {
				break
			}
			continue
		}
		if !ok {
			break
		}
		dir.buf = dir.buf[n:]
		if len(dir.buf) == 0 {
			dir.buf = nil
		}
		msgs++
		vc.sawResp = true
		vd.aligned = true
		var info string
		if fromClient {
			info = pktValkeyCommand(p, c, dir, v, ts)
		} else {
			info = pktValkeyReply(p, c, dir, v, ts)
		}
		if info != "" {
			infos = append(infos, info)
		}
		// An RDB transfer starts inside a reply; the rest of the frame is its payload.
		if vd.rdbRemaining > 0 || vd.rdbDiskless {
			if len(dir.buf) > 0 {
				if extra := valkeyRDBConsume(p, c, dir, vd); extra != "" {
					infos = append(infos, extra)
				}
			}
			break
		}
	}

	// An inline command — "PING\r\n" — has no RESP framing at all.
	if msgs == 0 && fromClient && len(dir.buf) > 0 {
		if line, n, ok := respInlineCommand(dir.buf); ok {
			dir.buf = dir.buf[n:]
			msgs++
			vc.sawResp = true
			name := strings.ToUpper(strings.Fields(line)[0])
			vc.pending = append(vc.pending, valkeyPending{name: name, ts: ts})
			vc.commands++
			c.stream.Queries++
			p.Command = name
			infos = append(infos, fmt.Sprintf("%s (inline, no RESP framing)", name))
			p.Issues = append(p.Issues,
				"Inline command — sent as bare text rather than a RESP array. Legal, and what a health check, a telnet session or a port probe sends; a real client library never does it")
		}
	}

	if msgs > 0 {
		p.Proto = valkeyKindLabel(vc.kind)
	}
	switch {
	case len(infos) > 0:
		p.Info = strings.Join(infos, " | ")
		if len(infos) > 3 {
			p.Info = fmt.Sprintf("%s | +%d more", strings.Join(infos[:3], " | "), len(infos)-3)
		}
	case msgs > 0:
		p.Info = fmt.Sprintf("Valkey data, %d value(s) in %d bytes", msgs, len(payload))
	case vd.desynced && !vd.aligned:
		p.Proto = "Valkey"
		p.Info = fmt.Sprintf("[framing lost] %d bytes, hunting for the next RESP value", len(payload))
	case !c.synced && !vd.aligned:
		p.Proto, p.Status = "Valkey", "Unknown"
		p.Info = fmt.Sprintf("[capture joined mid-connection] %d bytes, waiting for a value boundary", len(payload))
	case len(dir.buf) > 0:
		p.Proto = "Valkey"
		p.Info = fmt.Sprintf("[continuation] %d bytes, %d buffered", len(payload), len(dir.buf))
	}
}

// valkeyReanchor drops bytes until something that can only start a RESP value.
//
// RESP gives a weaker anchor than any of the other three protocols: a single type byte and
// a CRLF, which occurs constantly inside data. So the search requires a type byte AND a
// well-formed value AND, for the aggregate types that carry commands, that the value parse
// to completion. Being conservative here costs a few frames of "framing lost" and buys not
// inventing commands out of somebody's cached JSON.
func valkeyReanchor(dir *pktDirState) bool {
	buf := dir.buf
	for i := 1; i < len(buf); i++ {
		switch buf[i] {
		case respArray, respBulk, respSimple, respError, respInt, respPush, respMap, respSet:
		default:
			continue
		}
		if v, _, ok, bad := respParse(buf[i:], 0); ok && !bad {
			// A lone integer or simple string is too weak to anchor on: those two bytes
			// appear in any binary payload. Require an aggregate or a bulk string.
			switch v.Type {
			case respArray, respPush, respMap, respSet, respBulk:
				dir.buf = buf[i:]
				return true
			}
		}
	}
	if len(dir.buf) > 1<<20 {
		dir.buf = dir.buf[len(dir.buf)-(1<<16):]
	}
	return false
}

// ---------------------------------------------------------------- client → server

func pktValkeyCommand(p *pktPacket, c *pktConn, dir *pktDirState, v respValue, ts time.Time) string {
	vc := c.valkeyConn()

	// A replica's REPLCONF ACK is the only thing it sends on a replication link, and it
	// carries the offset that makes lag measurable.
	if !respIsCommand(v) {
		if v.Type == respArray || v.Type == respPush {
			return fmt.Sprintf("%s of %d (not a command)", respTypeName(v.Type), v.Int)
		}
		return ""
	}
	name := strings.ToUpper(v.Items[0].Str)
	args := make([]string, 0, len(v.Items)-1)
	for _, it := range v.Items[1:] {
		args = append(args, it.Str)
	}
	vc.commands++
	if !valkeyIsChatter(name) {
		c.stream.Queries++
	}
	p.Command = name
	key := ""
	if len(args) > 0 {
		key = args[0]
	}
	if key != "" {
		p.Query = name + " " + pktEllipsis(pktPrintable(key), 80)
	} else {
		p.Query = name
	}

	// Pipelining: how many commands arrived in this one frame. Counted by the caller's
	// loop, so this is the running total for the frame.
	vc.pending = append(vc.pending, valkeyPending{name: name, arg: key, ts: ts})
	if n := len(vc.pending); n > vc.maxPipeline {
		vc.maxPipeline = n
	}
	if len(vc.pending) >= 32 && !vc.pipelineNote {
		vc.pipelineNote = true
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Pipelining %d commands deep — the client is sending without waiting for replies, which is good for throughput; the replies are matched by order, so this is also the depth a decoder has to keep in step with",
			len(vc.pending)))
	}

	valkeyClassify(p, c, name, args)
	if iss := valkeyDangerous(name, args, vc); iss != "" {
		p.Issues = append(p.Issues, iss)
	}

	out := name
	if detail := valkeyCommandDetail(name, args, v); detail != "" {
		out += " " + detail
	}
	return out
}

// valkeyCommandDetail is the per-command part of the summary.
func valkeyCommandDetail(name string, args []string, v respValue) string {
	short := func(s string) string { return pktEllipsis(pktPrintable(s), 60) }
	switch name {
	case "GET", "DEL", "EXISTS", "TTL", "TYPE", "INCR", "DECR", "PERSIST", "DUMP", "UNLINK":
		return short(strings.Join(args, " "))
	case "SET", "SETEX", "GETSET", "APPEND", "SETNX":
		if len(args) >= 2 {
			return fmt.Sprintf("%s ← %s (%d bytes)%s", short(args[0]), short(args[1]), len(args[1]),
				valkeyOptions(args[2:]))
		}
	case "MGET", "MSET", "SUNION", "SINTER", "PFCOUNT", "WATCH":
		return fmt.Sprintf("%d key(s): %s", len(args), short(strings.Join(args[:min(len(args), 3)], " ")))
	case "HSET", "HGET", "HDEL", "HMGET", "HINCRBY", "HGETALL":
		if len(args) >= 2 {
			return fmt.Sprintf("%s field %s", short(args[0]), short(args[1]))
		}
		return short(strings.Join(args, " "))
	case "LPUSH", "RPUSH", "LPOP", "RPOP", "LRANGE", "LLEN", "LTRIM", "SADD", "SREM", "ZADD", "ZRANGE", "XADD", "XLEN":
		return short(strings.Join(args[:min(len(args), 3)], " "))
	case "BLPOP", "BRPOP", "BLMOVE", "BZPOPMIN", "BZPOPMAX":
		return fmt.Sprintf("%s — blocking, timeout %s", short(strings.Join(args[:max(len(args)-1, 0)], " ")),
			lastOr(args, "?"))
	case "SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "UNSUBSCRIBE":
		return fmt.Sprintf("%d channel(s): %s", len(args), short(strings.Join(args, " ")))
	case "PUBLISH", "SPUBLISH":
		if len(args) >= 2 {
			return fmt.Sprintf("channel %s, %d bytes", short(args[0]), len(args[1]))
		}
	case "AUTH":
		if len(args) >= 2 {
			return "user " + short(args[0]) + ", password (not shown)"
		}
		return "password (not shown)"
	case "HELLO":
		if len(args) >= 1 {
			return "protocol " + short(args[0])
		}
	case "PSYNC", "SYNC":
		return short(strings.Join(args, " "))
	case "REPLCONF":
		return short(strings.Join(args, " "))
	case "EVAL", "EVALSHA", "FCALL":
		if len(args) >= 1 {
			return fmt.Sprintf("script %s, %s key(s)", short(args[0]), lastOr(args[1:2], "?"))
		}
	case "SCAN", "HSCAN", "SSCAN", "ZSCAN":
		return "cursor " + short(strings.Join(args, " "))
	case "KEYS":
		return "pattern " + short(strings.Join(args, " "))
	case "CLUSTER", "CLIENT", "CONFIG", "COMMAND", "ACL", "MEMORY", "LATENCY", "OBJECT", "XINFO", "SLOWLOG":
		return short(strings.Join(args, " "))
	case "INFO", "PING", "MULTI", "EXEC", "DISCARD", "DBSIZE", "FLUSHALL", "FLUSHDB", "MONITOR":
		return short(strings.Join(args, " "))
	}
	if len(args) == 0 {
		return ""
	}
	return short(strings.Join(args[:min(len(args), 4)], " "))
}

// valkeyOptions picks the SET options worth showing — the expiry ones, because a SET
// without an expiry in a cache is how a memory problem starts.
func valkeyOptions(args []string) string {
	var parts []string
	for i, a := range args {
		switch strings.ToUpper(a) {
		case "EX", "PX", "EXAT", "PXAT":
			if i+1 < len(args) {
				parts = append(parts, strings.ToUpper(a)+" "+args[i+1])
			}
		case "KEEPTTL", "NX", "XX", "GET":
			parts = append(parts, strings.ToUpper(a))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, " ") + "]"
}

// ---------------------------------------------------------------- server → client

func pktValkeyReply(p *pktPacket, c *pktConn, dir *pktDirState, v respValue, ts time.Time) string {
	vc, vd := c.valkeyConn(), dir.valkeyDir()

	// A push message is unprompted: pub/sub delivery, or a client-tracking invalidation.
	// It must NOT consume a queued command, or every later reply is mislabelled.
	if v.Type == respPush || (v.Type == respArray && valkeyLooksPubSub(v)) {
		return valkeyPushInfo(p, vc, v)
	}

	// +FULLRESYNC / +CONTINUE answer PSYNC and change what the connection is.
	if v.Type == respSimple {
		if info, handled := valkeySyncReply(p, c, vd, v); handled {
			valkeyPopPending(vc)
			return info
		}
	}

	// After a sync, the primary's half of a replication link is a one-way stream of
	// propagated writes — plus periodic PING and REPLCONF GETACK. None of it is a reply,
	// and consuming the pending queue for it mislabelled every propagated command as the
	// answer to the replica's last REPLCONF ("REPLCONF → [\"SELECT\" \"0\"] (2948 ms)").
	if vc.kind == valkeyKindRepl && (vc.fullResync || vc.partialSync) && respIsCommand(v) {
		name := strings.ToUpper(v.Items[0].Str)
		// The primary's offset advances by the byte length of everything it sends, which
		// is exactly how the replica computes the offset it acknowledges — so counting it
		// here makes the lag against REPLCONF ACK a real measurement rather than a guess.
		vc.primaryOff += int64(v.Bytes)
		switch name {
		case "PING":
			return "propagated: PING (the primary's periodic keep-alive on the replication link)"
		case "REPLCONF":
			if len(v.Items) > 1 && strings.EqualFold(v.Items[1].Str, "GETACK") {
				return "REPLCONF GETACK — the primary is asking the replica to report its offset now"
			}
		}
		detail := ""
		if len(v.Items) > 1 {
			args := make([]string, 0, len(v.Items)-1)
			for _, it := range v.Items[1:] {
				args = append(args, it.Str)
			}
			detail = " " + valkeyCommandDetail(name, args, v)
		}
		return "propagated: " + name + detail
	}

	req, had := valkeyPopPending(vc)
	lag := 0.0
	if had && !req.ts.IsZero() {
		lag = ts.Sub(req.ts).Seconds() * 1000
		p.LagMS = lag
	}

	// An error reply is the whole diagnostic surface of RESP: the prefix IS the code.
	if v.Type == respError || v.Type == respBlobErr {
		c.stream.Errors++
		code, rest := valkeySplitError(v.Str)
		p.ErrState = code
		p.Status = fmt.Sprintf("Error %s: %s", code, pktEllipsis(pktPrintable(rest), 110))
		if iss := valkeyErrIssue(code, v.Str, req, vc); iss != "" {
			p.Issues = append(p.Issues, iss)
		}
		out := fmt.Sprintf("-%s %s", code, pktEllipsis(pktPrintable(rest), 120))
		if had {
			out = req.name + " → " + out
		}
		return out
	}

	p.Status = "Success"
	rendered := respRender(v, 1)
	switch v.Type {
	case respArray, respSet, respMap, respPush:
		p.Rows = int(v.Int)
	}
	if v.Bytes >= valkeyBigReplyBytes {
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Heavy reply — %s in one reply. Valkey is single-threaded for command execution, so the time spent serialising this blocked every other client on the server",
			pktBytes(v.Bytes)))
	}
	if lag >= valkeySlowMS && had && !valkeyBlocking(req.name) {
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Slow reply — %.1f ms for %s. Valkey executes commands one at a time, so this delayed every other client too (its own slowlog threshold is 10 ms)",
			lag, req.name))
	}
	// INFO's payload is where a replica's offsets live, and it is the one bulk reply
	// worth reading rather than measuring.
	if had && req.name == "INFO" {
		if note := valkeyInfoNote(vc, v.Str); note != "" {
			rendered = note
		}
	}
	out := rendered
	if had {
		out = req.name + " → " + rendered
	}
	if lag > 0 {
		out += fmt.Sprintf(" (%.1f ms)", lag)
	}
	return out
}

// valkeyPopPending takes the head of the command queue.
func valkeyPopPending(vc *pktValkeyConn) (valkeyPending, bool) {
	if len(vc.pending) == 0 {
		return valkeyPending{}, false
	}
	req := vc.pending[0]
	vc.pending = vc.pending[1:]
	return req, true
}

// valkeyLooksPubSub recognises RESP2's pub/sub delivery, which is an ordinary array
// whose first element is one of six fixed words. RESP3 uses a push type instead, but
// RESP2 is still the default for a client that never sends HELLO.
func valkeyLooksPubSub(v respValue) bool {
	if v.Type != respArray || len(v.Items) == 0 || v.Items[0].Type != respBulk {
		return false
	}
	switch strings.ToLower(v.Items[0].Str) {
	case "message", "pmessage", "smessage", "subscribe", "unsubscribe", "psubscribe", "punsubscribe", "ssubscribe":
		return true
	}
	return false
}

func valkeyPushInfo(p *pktPacket, vc *pktValkeyConn, v respValue) string {
	kind := ""
	if len(v.Items) > 0 {
		kind = strings.ToLower(v.Items[0].Str)
	}
	switch kind {
	case "message", "smessage":
		ch, payload := "", 0
		if len(v.Items) > 1 {
			ch = v.Items[1].Str
		}
		if len(v.Items) > 2 {
			payload = int(v.Items[2].Int)
			if payload == 0 {
				payload = len(v.Items[2].Str)
			}
		}
		return fmt.Sprintf("push: message on %s, %d bytes", strconv.Quote(pktEllipsis(ch, 40)), payload)
	case "pmessage":
		return "push: pattern message"
	case "subscribe", "psubscribe", "ssubscribe":
		vc.subChannels++
		return fmt.Sprintf("push: subscribed (%d channel(s) on this connection)", vc.subChannels)
	case "unsubscribe", "punsubscribe":
		return "push: unsubscribed"
	case "invalidate":
		p.Issues = append(p.Issues,
			"Client-side cache invalidation pushed — this connection has client tracking on (RESP3), and the server is telling it a cached key changed")
		return "push: invalidate"
	}
	return "push: " + respRender(v, 1)
}

// ---------------------------------------------------------------- replication

// valkeySyncReply handles the two answers to PSYNC, which decide what the rest of the
// connection is.
func valkeySyncReply(p *pktPacket, c *pktConn, vd *pktValkeyDir, v respValue) (string, bool) {
	vc := c.valkeyConn()
	s := v.Str
	switch {
	case strings.HasPrefix(s, "FULLRESYNC"):
		vc.kind, vc.fullResync = valkeyKindRepl, true
		vd.expectRDB = true
		f := strings.Fields(s)
		if len(f) >= 3 {
			vc.replID = f[1]
			vc.primaryOff, _ = strconv.ParseInt(f[2], 10, 64)
		}
		p.Issues = append(p.Issues, fmt.Sprintf(
			"FULLRESYNC — the primary is about to send its ENTIRE dataset as an RDB snapshot before any incremental stream. This is Valkey's most expensive operation: it forks, serialises everything, and on a large dataset it is the difference between a replica catching up in seconds and in minutes. A partial resync (+CONTINUE) was not possible, usually because the replication backlog no longer holds the replica's offset (repl-backlog-size)"))
		return fmt.Sprintf("+FULLRESYNC replid %s offset %d — a full dataset transfer follows",
			pktEllipsis(vc.replID, 16), vc.primaryOff), true
	case strings.HasPrefix(s, "CONTINUE"):
		vc.kind, vc.partialSync = valkeyKindRepl, true
		p.Issues = append(p.Issues,
			"Partial resynchronisation (+CONTINUE) — the replica reconnected and the primary still had its offset in the backlog, so only the missing stream is sent. This is the cheap path and the one you want after a brief disconnect")
		return "+CONTINUE — partial resync, no dataset transfer needed", true
	}
	return "", false
}

// valkeyRDBHeader reads the header that introduces an RDB transfer. Two forms:
//
//	$<len>\r\n            disk-based: exactly len bytes follow, with NO trailing CRLF
//	$EOF:<40 chars>\r\n   diskless: the length is not known in advance, and the payload
//	                      ends when the same 40-character delimiter appears again
//
// consumed is -1 when these bytes are not an RDB header at all, so the caller can fall
// back to ordinary parsing.
func valkeyRDBHeader(vd *pktValkeyDir, buf []byte) (info string, consumed int, done bool) {
	if buf[0] != respBulk {
		return "", -1, false
	}
	line, n, found := respLine(buf[1:])
	if !found {
		return "", 0, false // still arriving
	}
	hdr := 1 + n
	if mark, ok := strings.CutPrefix(line, "EOF:"); ok {
		vd.rdbDiskless, vd.eofMark, vd.rdbBytes = true, mark, 0
		return "RDB transfer begins (diskless, EOF-delimited) — the primary is streaming its dataset straight from the fork with no length known in advance", hdr, true
	}
	l, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || l < 0 {
		return "", -1, false
	}
	vd.rdbRemaining, vd.rdbTotal, vd.rdbBytes, vd.rdbDiskless = l, l, 0, false
	return fmt.Sprintf("RDB transfer begins, %s to come", pktBytes(l)), hdr, true
}

// valkeyRDBConsume takes RDB payload out of the direction's buffer, counting it rather
// than keeping it: a dataset transfer is exactly as large as the dataset, and holding it
// would mean holding the whole database in the decoder.
//
// The two forms end differently. A disk-based transfer announced its length, so it ends
// when that many bytes have arrived. A diskless one ends when the 40-character delimiter
// the primary announced appears in the stream — which can straddle a frame boundary, so
// the tail is kept.
func valkeyRDBConsume(p *pktPacket, c *pktConn, dir *pktDirState, vd *pktValkeyDir) string {
	if vd.rdbRemaining > 0 {
		take := min(len(dir.buf), vd.rdbRemaining)
		vd.rdbBytes += take
		vd.rdbRemaining -= take
		dir.buf = dir.buf[take:]
		if vd.rdbRemaining == 0 {
			done := vd.rdbBytes
			vd.rdbBytes, vd.rdbTotal = 0, 0
			return fmt.Sprintf("RDB transfer complete, %s — the incremental command stream starts now", pktBytes(done))
		}
		return fmt.Sprintf("RDB payload, %s of %s", pktBytes(vd.rdbBytes), pktBytes(vd.rdbTotal))
	}
	// Diskless.
	mark := []byte(vd.eofMark)
	if len(mark) > 0 {
		if i := bytes.Index(dir.buf, mark); i >= 0 {
			vd.rdbBytes += i
			dir.buf = dir.buf[i+len(mark):]
			vd.rdbDiskless, vd.eofMark = false, ""
			done := vd.rdbBytes
			vd.rdbBytes = 0
			return fmt.Sprintf("RDB transfer complete (diskless), %s — the delimiter arrived and the incremental command stream starts now",
				pktBytes(done))
		}
	}
	// Keep only enough tail for a delimiter that spans two frames.
	keep := len(mark) - 1
	if keep < 0 || keep > len(dir.buf) {
		keep = 0
	}
	vd.rdbBytes += len(dir.buf) - keep
	dir.buf = dir.buf[len(dir.buf)-keep:]
	return fmt.Sprintf("RDB payload (diskless), %s so far", pktBytes(vd.rdbBytes))
}

// valkeyInfoNote reads the replication section of an INFO reply, which is where a
// replica's offset and its primary's offset both appear.
func valkeyInfoNote(vc *pktValkeyConn, body string) string {
	var role, off, primaryOff, backlog string
	for _, line := range strings.Split(body, "\n") {
		k, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch k {
		case "role":
			role = val
		case "master_repl_offset":
			primaryOff = val
		case "slave_repl_offset", "slave_read_repl_offset":
			off = val
		case "repl_backlog_size":
			backlog = val
		}
	}
	var parts []string
	if role != "" {
		parts = append(parts, "role="+role)
	}
	if primaryOff != "" {
		parts = append(parts, "primary offset "+primaryOff)
	}
	if off != "" {
		parts = append(parts, "replica offset "+off)
	}
	if backlog != "" {
		parts = append(parts, "backlog "+backlog)
	}
	if len(parts) == 0 {
		return ""
	}
	return "INFO: " + strings.Join(parts, ", ")
}

// ---------------------------------------------------------------- classification

// valkeyClassify decides what a connection is for, and flags the commands that are events
// in their own right.
func valkeyClassify(p *pktPacket, c *pktConn, name string, args []string) {
	vc := c.valkeyConn()
	switch name {
	case "PSYNC", "SYNC":
		vc.kind = valkeyKindRepl
	case "REPLCONF":
		if vc.kind == "" || vc.kind == valkeyKindClient {
			vc.kind = valkeyKindRepl
		}
		// REPLCONF ACK <offset> is the replica telling the primary where it is, and it
		// is the only reason lag is measurable from a capture.
		if len(args) >= 2 && strings.EqualFold(args[0], "ACK") {
			if off, err := strconv.ParseInt(args[1], 10, 64); err == nil {
				vc.replicaOff = off
				if vc.primaryOff > off {
					behind := vc.primaryOff - off
					if behind >= valkeyLagBytes && !vc.lagNoted {
						vc.lagNoted = true
						p.Issues = append(p.Issues, fmt.Sprintf(
							"Replication lag %s — the replica has acknowledged offset %d while the primary is at %d. Past repl-backlog-size, a disconnect here forces a FULLRESYNC instead of a partial one",
							pktBytes(int(behind)), off, vc.primaryOff))
					}
				}
			}
		}
	case "SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE":
		vc.kind = valkeyKindSub
	case "MONITOR":
		vc.kind = valkeyKindMonitor
		p.Issues = append(p.Issues,
			"MONITOR — this connection now receives every command the server executes, from every client. It is a debugging tool with a real cost: the server serialises each command a second time for it")
	case "AUTH":
		if !c.tls {
			p.Issues = append(p.Issues,
				"AUTH on an unencrypted connection — Valkey sends the password as a plain RESP bulk string, so it is in this capture. Only a TLS port (tls-port) prevents that")
		}
		if len(args) >= 2 {
			vc.authUser = args[0]
			c.user = args[0]
		} else {
			vc.authUser = "default"
			c.user = "default"
		}
	case "HELLO":
		if len(args) >= 1 {
			if v, err := strconv.Atoi(args[0]); err == nil {
				vc.respVer = v
			}
		}
		// HELLO can carry AUTH, which puts the password on the wire just the same.
		for i, a := range args {
			if strings.EqualFold(a, "AUTH") && i+2 < len(args) && !c.tls {
				p.Issues = append(p.Issues,
					"HELLO … AUTH on an unencrypted connection — the password is a plain RESP bulk string in this capture")
				c.user = args[i+1]
			}
		}
	case "SELECT":
		if len(args) >= 1 {
			c.database = "db" + args[0]
		}
	case "FAILOVER":
		p.Issues = append(p.Issues,
			"FAILOVER — a coordinated primary handover was requested. Writes are refused for the duration, and clients see -READONLY or a MOVED to the new primary")
	case "CLUSTER":
		if len(args) >= 1 {
			switch strings.ToUpper(args[0]) {
			case "FAILOVER":
				p.Issues = append(p.Issues,
					"CLUSTER FAILOVER — a replica is being promoted; the slots it serves move with it and clients holding a stale slot map get MOVED")
			case "FORGET", "RESET", "SETSLOT", "DELSLOTS", "ADDSLOTS":
				p.Issues = append(p.Issues,
					"CLUSTER "+strings.ToUpper(args[0])+" — the cluster's own topology is being changed by hand")
			}
		}
	}
	if vc.kind == "" {
		vc.kind = valkeyKindClient
	}
	c.stream.Role = vc.kind
	c.stream.RoleLabel = valkeyKindLabel(vc.kind)
}

// valkeyIsChatter reports whether a command is protocol overhead rather than work.
func valkeyIsChatter(name string) bool {
	switch name {
	case "PING", "HELLO", "AUTH", "REPLCONF", "COMMAND", "INFO", "CLIENT", "SELECT", "ECHO":
		return true
	}
	return false
}

// valkeyBlocking reports whether a command is *supposed* to sit there. Flagging BLPOP as
// slow would be flagging the point of it — the same lesson MongoDB's awaitData getMore and
// PostgreSQL's streaming hello taught.
func valkeyBlocking(name string) bool {
	switch name {
	case "BLPOP", "BRPOP", "BLMOVE", "BRPOPLPUSH", "BLMPOP", "BZPOPMIN", "BZPOPMAX", "BZMPOP",
		"XREAD", "XREADGROUP", "WAIT", "SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "MONITOR",
		"PSYNC", "SYNC", "DEBUG":
		return true
	}
	return false
}

// valkeyKnownCommand is a coarse check used to recognise an inline command. It is not the
// full command table — a few dozen names that a probe or a health check might send.
func valkeyKnownCommand(name string) bool {
	switch name {
	case "PING", "ECHO", "INFO", "GET", "SET", "DEL", "EXISTS", "TTL", "TYPE", "KEYS",
		"SCAN", "DBSIZE", "SELECT", "AUTH", "HELLO", "QUIT", "COMMAND", "CONFIG",
		"CLUSTER", "CLIENT", "SUBSCRIBE", "PUBLISH", "MONITOR", "SHUTDOWN", "LOLWUT",
		"REPLCONF", "PSYNC", "SYNC", "FLUSHALL", "FLUSHDB", "TIME", "LASTSAVE", "ROLE":
		return true
	}
	return false
}

// valkeyDangerous flags the commands that are safe on a laptop and destructive on a
// production server. This is not a lint of somebody's application: each of these has a
// documented, specific cost that a capture is the only place to catch in the act.
func valkeyDangerous(name string, args []string, vc *pktValkeyConn) string {
	once := func(key, msg string) string {
		if vc.dangerNoted[key] {
			return ""
		}
		vc.dangerNoted[key] = true
		return msg
	}
	switch name {
	case "KEYS":
		pattern := ""
		if len(args) > 0 {
			pattern = args[0]
		}
		return once("KEYS", fmt.Sprintf(
			"KEYS %s — this walks the ENTIRE keyspace in one blocking operation. Valkey executes one command at a time, so on a large database every other client waits for it. SCAN does the same job in cursor-sized pieces",
			pktEllipsis(pattern, 20)))
	case "FLUSHALL", "FLUSHDB":
		sync := " (synchronous by default: the whole server blocks until it finishes)"
		for _, a := range args {
			if strings.EqualFold(a, "ASYNC") {
				sync = " (ASYNC: freed in the background)"
			}
		}
		return once(name, name+" — every key in "+
			map[string]string{"FLUSHALL": "every database", "FLUSHDB": "this database"}[name]+
			" is being deleted"+sync)
	case "DEBUG":
		if len(args) > 0 && strings.EqualFold(args[0], "SLEEP") {
			return once("DEBUG SLEEP", "DEBUG SLEEP — the server is being deliberately blocked for a fixed time; every other client is stalled for the duration")
		}
		if len(args) > 0 && strings.EqualFold(args[0], "SEGFAULT") {
			return "DEBUG SEGFAULT — the server is being made to crash on purpose"
		}
	case "SHUTDOWN":
		return "SHUTDOWN — the server is being stopped; every connection failure after this is a consequence"
	case "SAVE":
		return once("SAVE", "SAVE — a synchronous snapshot: the server is blocked for the whole write. BGSAVE forks instead")
	case "SWAPDB":
		return once("SWAPDB", "SWAPDB — two databases are being exchanged under the clients using them")
	case "SCRIPT":
		if len(args) > 0 && strings.EqualFold(args[0], "FLUSH") {
			return once("SCRIPT FLUSH", "SCRIPT FLUSH — every cached Lua script is gone, so every EVALSHA that follows fails with NOSCRIPT until the client resends the body")
		}
	case "CONFIG":
		if len(args) >= 2 && strings.EqualFold(args[0], "SET") {
			return once("CONFIG SET "+strings.ToLower(args[1]),
				"CONFIG SET "+args[1]+" — the server's configuration is being changed at runtime, which persists only until restart unless CONFIG REWRITE follows")
		}
	}
	return ""
}

// lastOr returns the last element of a slice, or a default.
func lastOr(s []string, def string) string {
	if len(s) == 0 {
		return def
	}
	return s[len(s)-1]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
