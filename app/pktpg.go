package main

// pktpg.go — the PostgreSQL frontend/backend protocol, version 3.0.
//
// The shape of the protocol, and why the decoder is built the way it is:
//
//   - Every message after the first is [1-byte type][int32 length][body], and the
//     length INCLUDES its own four bytes. That single rule makes reassembly simple
//     and, unlike MySQL, gives no 16 MB chunking to stitch back together.
//   - The FIRST client message has no type byte at all: [int32 length][int32 code],
//     where the code is either the protocol version (196608 = 3.0) for a startup
//     message or one of three magic numbers — SSLRequest, GSSENCRequest,
//     CancelRequest. A capture that joins an existing connection never sees it.
//   - The answer to SSLRequest is a single naked byte, 'S' or 'N', outside the
//     message framing entirely. Miss that and every following length is read four
//     bytes off, so the rest of the connection decodes as garbage.
//   - Requests and responses are NOT strictly alternating the way MySQL's are: the
//     extended query protocol pipelines Parse/Bind/Describe/Execute/Sync and the
//     server answers with a run of messages ending in ReadyForQuery. Latency is
//     therefore measured to the message that ends the cycle, not to "the next
//     server packet".
//
// Mid-capture joins are the normal case on a busy server, and the same rule applies
// here as in pktmysql.go: a frame whose meaning cannot be known is reported as
// "capture joined mid-connection" rather than decoded into something plausible.
// PostgreSQL gives an unusually good re-anchor for that — ReadyForQuery is always
// exactly the six bytes 'Z' 00 00 00 05 <'I'|'T'|'E'>, ends every command cycle,
// and cannot be confused with anything else, so one round trip after the capture
// starts the stream is aligned again.
//
// Replication rides the same protocol rather than a separate port (Galera's model),
// which is why so much of this file is about it: a walsender connection is a normal
// startup with replication=true/database, then CopyBothResponse, then a stream of
// CopyData whose first byte says what it is. That stream carries the LSNs of both
// ends, so a capture — with no access to the server at all — can state replication
// lag in bytes and in time. pg_basebackup is here too: a new replica cloning the
// primary is the PostgreSQL analogue of a Galera SST, and it looks like an ordinary
// COPY of the entire data directory.

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The first-message codes. Everything else is typed.
const (
	pgProtocol30    = 196608   // 0x00030000
	pgProtocol31    = 196609   // asked for by newer clients; servers answer NegotiateProtocolVersion
	pgSSLRequest    = 80877103 // 1234 << 16 | 5679
	pgCancelRequest = 80877102
	pgGSSENCRequest = 80877104
)

// pgMaxMsgLen bounds what is accepted as a message length. PostgreSQL itself allows
// up to 1 GB, but a length that large in a capture is overwhelmingly more likely to
// be misframing than a real message, and buffering it would cost the memory of the
// whole capture over again.
const pgMaxMsgLen = 64 << 20

// pgIdleInTxSeconds is when "idle in transaction" stops being a pause and starts
// being a problem: locks are held and VACUUM cannot clean up behind it.
const pgIdleInTxSeconds = 10.0

// pgLagBytes is the replication lag worth a line. 16 MB is one WAL segment — a
// standby a whole segment behind is no longer keeping up.
const pgLagBytes = 16 << 20

// pgParseRepeat is how many times the same SQL may be re-Parsed as an unnamed
// statement before it is worth saying that nothing is being reused.
const pgParseRepeat = 20

// pktPGDir is one direction's PostgreSQL state.
type pktPGDir struct {
	firstDone bool // this direction's first message has been consumed
	// aligned means the next byte in the buffer is known to start a message. It is
	// true from the first byte for a connection whose SYN was captured, and
	// otherwise only once an unmistakable message has been found (pktPGAnchor).
	aligned  bool
	desynced bool // framing was lost at least once — bytes are missing from the capture
	// Result-set bookkeeping for the current cycle.
	cols        int
	colNames    []string
	rows        int
	resultBytes int
	heavyNoted  bool
	copyMode    byte // 0 | 'i' CopyIn | 'o' CopyOut | 'b' CopyBoth
	copyBytes   int
	// Extended-protocol bookkeeping.
	lastParse string
	parseRuns int
}

// pktPGConn is per-connection PostgreSQL state.
type pktPGConn struct {
	startupSeen  bool
	sslRequested bool
	sslAnswered  bool
	gssRequested bool
	authMethod   string
	authDone     bool
	// replication is "" for an ordinary client, "physical" for a streaming standby
	// and "logical" for a subscriber — read from the startup parameters.
	replication string
	cancel      bool // this connection is a CancelRequest, not a session
	queries     int
	// Command timing. PostgreSQL pipelines, so the cycle starts at the first
	// message of a batch and ends at ReadyForQuery.
	cycleOpen bool
	cycleTS   time.Time
	cycleWhat string
	txStatus  byte      // last ReadyForQuery status: 'I' idle, 'T' in transaction, 'E' failed
	txSince   time.Time // when the connection last went idle inside a transaction
	idleNoted bool
	// Replication progress, in LSN units (bytes of WAL).
	sentLSN, flushLSN, applyLSN uint64
	lagNoted                    bool
	lastStandbyReply            time.Time
	basebackup                  bool
	baseBytes                   int
	baseNoted                   bool
	// adopted records that the replication classification came from the stream's own
	// sub-type bytes rather than from a startup message — i.e. the capture joined an
	// already-streaming standby, which is the usual case.
	adopted bool
	// Logical decoding: relation oid → name, so an Insert can name its table.
	relations map[uint32]string
	// Named prepared statements: name → SQL, so Bind/Execute can name the query.
	statements   map[string]string
	sawTerminate bool
}

func (c *pktConn) pgConn() *pktPGConn {
	if c.pg == nil {
		c.pg = &pktPGConn{relations: map[uint32]string{}, statements: map[string]string{}}
	}
	return c.pg
}

func (d *pktDirState) pgDir() *pktPGDir {
	if d.pg == nil {
		d.pg = &pktPGDir{}
	}
	return d.pg
}

// ---------------------------------------------------------------- entry point

// pktPGDecode consumes one frame's payload for a connection direction and annotates
// the frame with whatever it completes. It mirrors pktAppDecode's contract exactly,
// so pktdecode.go can dispatch on the connection's role and nothing else.
func pktPGDecode(p *pktPacket, c *pktConn, dir *pktDirState, fromClient bool, payload []byte, ts time.Time) {
	pc := c.pgConn()
	pd := dir.pgDir()

	// Encrypted, and therefore over: name the record and stop. Everything past the
	// TLS handshake on a PostgreSQL connection is the same protocol as before, but
	// none of it is readable, and guessing is how a decoder produces confident
	// nonsense.
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
	// The server's answer to SSLRequest is one naked byte outside the framing.
	if pc.sslRequested && !pc.sslAnswered && !fromClient && len(payload) > 0 {
		pc.sslAnswered = true
		switch payload[0] {
		case 'S':
			p.Proto, p.Info = "PostgreSQL", "SSL accepted ('S') — TLS handshake follows"
			c.sslRequested = true // pktdecode's TLS bookkeeping, shared with MySQL
			if len(payload) > 1 && pktLooksTLS(payload[1:]) {
				c.tls, c.tlsSealed = true, pktTLSSeals(payload[1:])
			}
			return
		case 'N':
			p.Proto = "PostgreSQL"
			p.Info = "SSL refused ('N') — the server has ssl=off or no certificate"
			p.Issues = append(p.Issues,
				"SSL refused by the server — a client with sslmode=require or verify-full aborts here; one with sslmode=prefer silently continues in the clear")
			return
		case 'E':
			// A pre-8.0 style error, or a server that cannot even consider SSL.
			p.Proto = "PostgreSQL"
			p.Info = "SSLRequest answered with an error message"
		default:
			// Not an SSL answer after all: fall through and frame it normally.
			pc.sslAnswered = false
		}
	}
	if pc.gssRequested && !pc.sslAnswered && !fromClient && len(payload) == 1 {
		pc.sslAnswered = true
		p.Proto = "PostgreSQL"
		if payload[0] == 'G' {
			p.Info = "GSSAPI encryption accepted ('G')"
			c.sslRequested = true
		} else {
			p.Info = "GSSAPI encryption refused ('N')"
		}
		return
	}
	// The client's TLS ClientHello may share the segment with, or immediately
	// follow, its SSLRequest.
	if c.sslRequested && pktLooksTLS(payload) {
		c.tls = true
		p.Proto, p.Status = "TLS", "Encrypted"
		p.Info = pktTLSInfo(payload, false)
		c.tlsSealed = pktTLSSeals(payload)
		return
	}

	dir.buf = append(dir.buf, payload...)
	var infos []string
	msgs, sawPG := 0, false
	for {
		typ, body, ok := pktNextPG(c, dir, fromClient)
		if !ok {
			break
		}
		msgs++
		sawPG = true
		var info string
		if fromClient {
			// Any client message ends an idle period, so the gap since the last
			// ReadyForQuery is measurable here and nowhere else.
			pgIdleCheck(p, pc, ts)
			info = pktPGClient(p, c, dir, typ, body, ts)
		} else {
			info = pktPGServer(p, c, dir, typ, body, ts)
		}
		if info != "" {
			infos = append(infos, info)
		}
		if c.tls {
			dir.buf = nil // the rest of this frame is a TLS record, not a message
			break
		}
	}
	if sawPG {
		p.Proto = "PostgreSQL"
	}
	switch {
	case len(infos) > 0:
		p.Info = strings.Join(infos, " | ")
		if len(infos) > 3 {
			p.Info = fmt.Sprintf("%s | +%d more", strings.Join(infos[:3], " | "), len(infos)-3)
		}
	case msgs > 0:
		// Rows and COPY payload are counted, not narrated, but the frame that
		// carried them still needs a line.
		what := "PostgreSQL data"
		if pd.rows > 0 {
			what = fmt.Sprintf("Row data (row %d of %d-column set)", pd.rows, pd.cols)
		} else if pd.copyMode != 0 {
			what = "COPY data"
		}
		p.Info = fmt.Sprintf("%s, %d bytes in %d message(s)", what, len(payload), msgs)
	case !c.synced && !pd.aligned:
		// The capture began in the middle of this connection, so where a message
		// starts is not yet known. Saying so is the honest answer; decoding these
		// bytes as if the first one were a message type is how a packet decoder
		// invents queries that were never run.
		p.Proto, p.Status = "PostgreSQL", "Unknown"
		p.Info = fmt.Sprintf(
			"[capture joined mid-connection] %d bytes, waiting for a message boundary (the next ReadyForQuery aligns the stream)", len(payload))
	case pd.desynced:
		p.Proto = "PostgreSQL"
		p.Info = fmt.Sprintf("[framing lost] %d bytes, hunting for the next message boundary", len(payload))
	case len(dir.buf) > 0:
		p.Proto = "PostgreSQL"
		p.Info = fmt.Sprintf("[continuation] %d bytes, %d buffered", len(payload), len(dir.buf))
	}
}

// pktNextPG pulls one complete message out of a direction's buffer.
//
// typ is 0 for the untyped first client message. A direction that is not yet
// aligned (the capture joined an existing connection) is re-anchored here rather
// than by the callers: nothing is returned until a byte sequence that can only be
// one thing has been found.
//
// It is a loop rather than the obvious recursion because a misframed direction can
// need thousands of bytes dropped before it lines up again, and one stack frame per
// dropped byte is how a decoder crashes on a capture with a gap in it.
func pktNextPG(c *pktConn, dir *pktDirState, fromClient bool) (typ byte, body []byte, ok bool) {
	pd := dir.pgDir()

	// The untyped first message, which only exists at the true start of a
	// connection — so only when the capture holds the SYN.
	if fromClient && !pd.firstDone && c.synced {
		if len(dir.buf) < 8 {
			return 0, nil, false
		}
		n := int(binary.BigEndian.Uint32(dir.buf))
		pd.firstDone = true
		if n >= 8 && n <= 10000 {
			if len(dir.buf) < n {
				pd.firstDone = false // wait for the rest of it
				return 0, nil, false
			}
			body = dir.buf[4:n]
			dir.buf = dir.buf[n:]
			pd.aligned = true
			return 0, body, true
		}
		// Not a startup message after all; fall through and treat this direction
		// like any other unaligned one.
	}

	for {
		if !c.synced && !pd.aligned {
			if !pktPGAnchor(dir, fromClient) {
				return 0, nil, false
			}
			pd.aligned = true
		}
		if len(dir.buf) < 5 {
			return 0, nil, false
		}
		typ = dir.buf[0]
		n := int(binary.BigEndian.Uint32(dir.buf[1:]))
		// A length that cannot be right means this direction is not aligned. On a
		// connection whose start was captured that means bytes are missing from the
		// capture itself (a snaplen cut, a kernel drop); on one joined mid-stream it
		// is simply expected. Either way the answer is to hunt for the next
		// unmistakable message rather than to decode nonsense.
		if n < 4 || n > pgMaxMsgLen || !pgKnownType(typ, fromClient) {
			pd.aligned = false
			pd.desynced = true
			dir.buf = dir.buf[1:]
			if !c.synced {
				continue
			}
			// A synced connection has no anchor search; resynchronise by hunting for
			// one anyway, since the alternative is decoding every following byte wrong.
			if !pktPGAnchor(dir, fromClient) {
				return 0, nil, false
			}
			pd.aligned = true
			continue
		}
		if len(dir.buf) < 1+n {
			return 0, nil, false
		}
		body = dir.buf[5 : 1+n]
		dir.buf = dir.buf[1+n:]
		if len(dir.buf) == 0 {
			dir.buf = nil
		}
		return typ, body, true
	}
}

// pktPGAnchor finds a byte sequence in an unaligned direction that can only be one
// message, and drops everything before it.
//
// ReadyForQuery is the anchor of choice from the server: 'Z' 00 00 00 05 and a
// status byte, six bytes that end every command cycle. From the client, a Query or
// a Parse whose length is plausible and whose body ends in a NUL is as good, and
// Terminate ('X' 00 00 00 04) is unmistakable.
//
// Replication streams need their own anchor, and it is the one that matters most on a
// real server: a standby that attached hours ago never sends another Query and never
// receives another ReadyForQuery, so neither of the anchors above will ever appear.
// What it does send, every few seconds forever, is CopyData whose body starts with one
// of four sub-type bytes — 'w' XLogData, 'k' keepalive, 'r' standby status, 'h' hot
// standby feedback — and whose length is exactly right for that sub-type. That is
// specific enough to anchor on, and anchoring on it is what makes replication lag
// readable from a capture of a cluster that was already running.
func pktPGAnchor(dir *pktDirState, fromClient bool) bool {
	buf := dir.buf
	for i := 0; i+6 <= len(buf); i++ {
		// CopyData carrying a replication message, in either direction.
		if buf[i] == 'd' {
			if n := int(binary.BigEndian.Uint32(buf[i+1:])); n >= 6 && n <= 1<<20 && i+1+n <= len(buf) {
				if pgReplSubtypeLen(buf[i+5], n) {
					dir.buf = buf[i:]
					return true
				}
			}
		}
		if !fromClient {
			if buf[i] == 'Z' && binary.BigEndian.Uint32(buf[i+1:]) == 5 &&
				(buf[i+5] == 'I' || buf[i+5] == 'T' || buf[i+5] == 'E') {
				dir.buf = buf[i:]
				return true
			}
			continue
		}
		switch buf[i] {
		case 'X':
			if binary.BigEndian.Uint32(buf[i+1:]) == 4 {
				dir.buf = buf[i:]
				return true
			}
		case 'Q', 'P':
			n := int(binary.BigEndian.Uint32(buf[i+1:]))
			if n < 6 || n > 1<<20 || i+1+n > len(buf) {
				continue
			}
			// The body of a Query is SQL text ending in a NUL; of a Parse, a
			// statement name then SQL then a NUL.
			if buf[i+n] != 0 {
				continue
			}
			if !pktMostlyPrintable(buf[i+5 : i+n]) {
				continue
			}
			dir.buf = buf[i:]
			return true
		}
	}
	// Nothing to anchor on yet. Keep only a tail: an anchor cannot span more than a
	// message, and holding the whole stream would grow without bound.
	if len(dir.buf) > 1<<20 {
		dir.buf = dir.buf[len(dir.buf)-(1<<16):]
	}
	return false
}

// pgReplSubtypeLen reports whether a CopyData length is exactly what its replication
// sub-type must be. Three of the four are fixed-size and the fourth has a fixed
// minimum, and requiring the match is what stops ordinary COPY data that happens to
// start with 'w' from being read as a WAL record. n is the length field's own value:
// it counts itself plus the body, so the whole message is n+1 bytes on the wire.
//
//	'w' XLogData             'w' + start + end + clock + WAL → n ≥ 30 (25 body + 4, plus WAL)
//	'k' Primary keepalive    'k' + walEnd + clock + reply    → n = 22 (18 body + 4)
//	'r' Standby status       'r' + write + flush + apply + clock + reply → n = 38
//	'h' Hot standby feedback 'h' + clock + xmin + epoch + catalog xmin + epoch → n = 29
func pgReplSubtypeLen(sub byte, n int) bool {
	switch sub {
	case 'w':
		return n >= 30
	case 'k':
		return n == 22
	case 'r':
		return n == 38
	case 'h':
		return n == 29
	}
	return false
}

// pgKnownType reports whether a byte is a message type this side ever sends. It is
// the sanity check that stops misframed bytes from being decoded as a message.
func pgKnownType(t byte, fromClient bool) bool {
	if fromClient {
		return strings.IndexByte("QPBEDCHSFfdcpX", t) >= 0
	}
	return strings.IndexByte("RKSZTDCIEnNtsGHWAdcv1233", t) >= 0
}

// ---------------------------------------------------------------- client → server

func pktPGClient(p *pktPacket, c *pktConn, dir *pktDirState, typ byte, body []byte, ts time.Time) string {
	pc, pd := c.pgConn(), dir.pgDir()

	if typ == 0 { // the untyped first message
		if len(body) < 4 {
			return "Malformed first message"
		}
		code := binary.BigEndian.Uint32(body)
		switch code {
		case pgSSLRequest:
			pc.sslRequested = true
			p.Command = "SSLRequest"
			return "SSLRequest — asking whether the server will speak TLS"
		case pgGSSENCRequest:
			pc.gssRequested = true
			p.Command = "GSSENCRequest"
			return "GSSENCRequest — asking for GSSAPI encryption"
		case pgCancelRequest:
			pc.cancel = true
			p.Command = "CancelRequest"
			pid, secret := 0, 0
			if len(body) >= 12 {
				pid = int(binary.BigEndian.Uint32(body[4:]))
				secret = int(binary.BigEndian.Uint32(body[8:]))
			}
			p.Issues = append(p.Issues, fmt.Sprintf(
				"Query cancellation requested for backend pid %d — a client timeout, a user interrupt, or statement_timeout on the client side; the running statement ends with 57014 query_canceled", pid))
			_ = secret
			return fmt.Sprintf("CancelRequest for pid %d", pid)
		}
		return pktPGStartup(p, c, pd, code, body)
	}

	switch typ {
	case 'Q': // simple query
		sql := pktCleanSQL(pgString(body))
		pc.queries++
		c.stream.Queries++
		pgCycleStart(pc, ts, "query")
		p.Command, p.Query = "Query", sql
		if r := pgReplicationCommand(sql); r != "" {
			pc.replication = r
			p.Issues = append(p.Issues, pgReplicationIssue(sql, r))
		}
		return "Query: " + pktEllipsis(sql, 160)

	case 'P': // Parse
		name, rest := pgTakeString(body)
		sql := pktCleanSQL(pgString(rest))
		pgCycleStart(pc, ts, "parse")
		if name != "" {
			pc.statements[name] = sql
		}
		// The unnamed statement being parsed over and over with identical SQL means
		// the client is paying for planning on every execution — the thing prepared
		// statements exist to avoid. Worth saying once per connection.
		if name == "" {
			if pd.lastParse == sql {
				pd.parseRuns++
			} else {
				pd.lastParse, pd.parseRuns = sql, 1
			}
			if pd.parseRuns == pgParseRepeat {
				p.Issues = append(p.Issues, fmt.Sprintf(
					"The same statement has been parsed %d times as an unnamed prepared statement — every execution is being re-planned; a named statement (or the driver's prepared-statement cache) would plan it once",
					pgParseRepeat))
			}
		}
		p.Command, p.Query = "Parse", sql
		if name == "" {
			return "Parse (unnamed): " + pktEllipsis(sql, 140)
		}
		return fmt.Sprintf("Parse %q: %s", name, pktEllipsis(sql, 130))

	case 'B': // Bind
		portal, rest := pgTakeString(body)
		stmt, rest2 := pgTakeString(rest)
		params, big := pgBindParams(rest2)
		pgCycleStart(pc, ts, "bind")
		p.Command = "Bind"
		if sql, ok := pc.statements[stmt]; ok && stmt != "" {
			p.Query = sql
		}
		out := fmt.Sprintf("Bind %s → portal %s, %d parameter(s)",
			pgNameOr(stmt, "unnamed statement"), pgNameOr(portal, "unnamed portal"), params)
		if big > 0 {
			out += fmt.Sprintf(", largest %s", pktBytes(big))
		}
		return out

	case 'D': // Describe
		if len(body) > 0 {
			what := "statement"
			if body[0] == 'P' {
				what = "portal"
			}
			return "Describe " + what + " " + pgNameOr(pgString(body[1:]), "unnamed")
		}
		return "Describe"

	case 'E': // Execute
		portal, rest := pgTakeString(body)
		max := 0
		if len(rest) >= 4 {
			max = int(binary.BigEndian.Uint32(rest))
		}
		pc.queries++
		c.stream.Queries++
		pgCycleStart(pc, ts, "execute")
		p.Command = "Execute"
		if max > 0 {
			return fmt.Sprintf("Execute portal %s, at most %d row(s)", pgNameOr(portal, "unnamed"), max)
		}
		return "Execute portal " + pgNameOr(portal, "unnamed")

	case 'C': // Close
		if len(body) > 0 {
			what := "statement"
			if body[0] == 'P' {
				what = "portal"
			}
			return "Close " + what + " " + pgNameOr(pgString(body[1:]), "unnamed")
		}
		return "Close"

	case 'H':
		return "Flush"
	case 'S':
		return "Sync"

	case 'F':
		return fmt.Sprintf("FunctionCall, %d bytes", len(body))

	case 'p': // PasswordMessage / SASL / GSS — which one depends on the auth method
		return pgPasswordInfo(p, pc, body)

	case 'd': // CopyData
		pd.copyBytes += len(body)
		if pgAdoptReplication(pc, body) {
			return pgStandbyMessage(p, pc, body, ts)
		}
		return "" // narrated in bulk by the caller
	case 'c':
		out := fmt.Sprintf("CopyDone (%s sent)", pktBytes(pd.copyBytes))
		pd.copyBytes, pd.copyMode = 0, 0
		return out
	case 'f':
		reason := pgString(body)
		p.Issues = append(p.Issues, "COPY aborted by the client: "+pktEllipsis(reason, 120))
		return "CopyFail: " + pktEllipsis(reason, 120)

	case 'X':
		pc.sawTerminate = true
		p.Command = "Terminate"
		// A session that never ran a statement is a health check or a connection
		// pool churning — invisible in the server log unless log_connections is on,
		// and obvious here.
		if pc.queries == 0 && pc.startupSeen && pc.replication == "" {
			p.Issues = append(p.Issues,
				"Connection opened and closed without running a statement — a TCP/health check or a connection pool churning; each one still costs a backend fork and a full authentication round trip")
		}
		return "Terminate"
	}
	return fmt.Sprintf("Client message %q, %d bytes", string(rune(typ)), len(body))
}

// pktPGStartup reads the startup parameters: who is connecting, to what, and
// whether this is a replication connection.
func pktPGStartup(p *pktPacket, c *pktConn, pd *pktPGDir, code uint32, body []byte) string {
	pc := c.pgConn()
	pc.startupSeen = true
	p.Command = "StartupMessage"
	major, minor := code>>16, code&0xffff
	params := map[string]string{}
	for _, kv := range pgStrings(body[4:]) {
		if len(kv) == 2 {
			params[kv[0]] = kv[1]
		}
	}
	c.user, c.database = params["user"], params["database"]
	if c.database == "" {
		c.database = c.user // PostgreSQL's default
	}
	switch params["replication"] {
	case "true", "on", "yes", "1":
		pc.replication = "physical"
	case "database":
		pc.replication = "logical"
	}
	out := fmt.Sprintf("StartupMessage %d.%d: user=%s database=%s", major, minor, c.user, c.database)
	if app := params["application_name"]; app != "" {
		out += " application_name=" + app
	}
	if pc.replication != "" {
		out += " replication=" + pc.replication
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Replication connection (%s) — this is a standby or a logical subscriber attaching, not application traffic", pc.replication))
	}
	switch {
	case code == pgProtocol31, major == 3 && minor > 0:
		// A real client asking for 3.1/3.2; the server answers NegotiateProtocolVersion
		// and the connection continues at 3.0.
		out += " (a newer minor protocol was requested)"
	case major != 3:
		// Not a version at all, in practice: something that is not a PostgreSQL client
		// is talking to the port, and the four bytes are being read as a version
		// because that is where a version would be.
		out = fmt.Sprintf("StartupMessage with protocol %d.%d — not a version this server can speak", major, minor)
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Unrecognised protocol version %d.%d in a startup message — a port scanner, a health check speaking the wrong protocol, or a client pointed at the wrong port; the server answers 0A000 and closes",
			major, minor))
	}
	return out
}

// pgPasswordInfo names what a 'p' message actually is. The type byte is reused for
// every authentication exchange, and only the server's preceding Authentication
// message says which — so the connection's auth method decides.
func pgPasswordInfo(p *pktPacket, pc *pktPGConn, body []byte) string {
	switch {
	case strings.HasPrefix(pc.authMethod, "SCRAM"):
		// The first SASL response carries the mechanism name, the rest are opaque.
		if len(body) > 0 && body[0] >= 'A' && body[0] <= 'Z' {
			mech, _ := pgTakeString(body)
			return "SASLInitialResponse: " + mech
		}
		return fmt.Sprintf("SASLResponse, %d bytes", len(body))
	case pc.authMethod == "cleartext password":
		return "PasswordMessage (cleartext)"
	case pc.authMethod == "MD5 password":
		return "PasswordMessage (md5 hash)"
	case strings.HasPrefix(pc.authMethod, "GSSAPI"), strings.HasPrefix(pc.authMethod, "SSPI"):
		return fmt.Sprintf("GSSResponse, %d bytes", len(body))
	}
	return fmt.Sprintf("PasswordMessage, %d bytes", len(body))
}

// ---------------------------------------------------------------- server → client

func pktPGServer(p *pktPacket, c *pktConn, dir *pktDirState, typ byte, body []byte, ts time.Time) string {
	pc, pd := c.pgConn(), dir.pgDir()

	switch typ {
	case 'R': // Authentication*
		return pgAuthInfo(p, c, pc, body)

	case 'K': // BackendKeyData
		if len(body) >= 8 {
			return fmt.Sprintf("BackendKeyData: backend pid %d", binary.BigEndian.Uint32(body))
		}
		return "BackendKeyData"

	case 'S': // ParameterStatus
		name, rest := pgTakeString(body)
		val := pgString(rest)
		if name == "server_version" {
			c.version = val
		}
		// Two of these decide how the rest of the capture should be read.
		switch name {
		case "server_version", "in_hot_standby", "is_superuser", "TimeZone", "client_encoding":
			return fmt.Sprintf("ParameterStatus: %s = %s", name, val)
		}
		return "" // the rest are a wall of defaults at connect time

	case 'Z': // ReadyForQuery
		// Whatever the client had half-sent before the capture began is stale; from
		// here its next byte starts a message.
		if cd := c.c2s.pgDir(); !cd.aligned {
			cd.aligned, c.c2s.buf = true, nil
		}
		if len(body) == 0 {
			return "ReadyForQuery"
		}
		return pgReadyForQuery(p, pc, pd, body[0], ts)

	case 'T': // RowDescription
		pd.cols, pd.colNames = pgRowDescription(body)
		pd.rows, pd.resultBytes, pd.heavyNoted = 0, 0, false
		if len(pd.colNames) > 0 {
			return fmt.Sprintf("Row description: %d column(s) — %s",
				pd.cols, pktEllipsis(strings.Join(pd.colNames, ", "), 90))
		}
		return fmt.Sprintf("Row description: %d column(s)", pd.cols)

	case 'D': // DataRow
		pd.rows++
		pd.resultBytes += len(body)
		if pd.resultBytes >= pktBigResultBytes && !pd.heavyNoted {
			pd.heavyNoted = true
			p.Issues = append(p.Issues, fmt.Sprintf(
				"Heavy result set — %s of rows on one statement; the client waits for all of it and the server holds it in memory as it is written",
				pktBytes(pd.resultBytes)))
		}
		return "" // counted, not narrated

	case 'C': // CommandComplete
		tag := pgString(body)
		p.Status = "Success"
		p.Rows = pgTagRows(tag)
		if pd.rows > 0 {
			p.Rows = pd.rows
		}
		out := "CommandComplete: " + tag
		if pd.rows > 0 {
			// The column count comes from RowDescription, which an extended-protocol
			// client only asks for once — every later Bind/Execute of the same
			// statement returns rows with no description at all. Reporting "0
			// column(s)" for those would be stating something false about the wire;
			// the honest answer is to leave it out.
			cols := ""
			if pd.cols > 0 {
				cols = fmt.Sprintf(", %d column(s)", pd.cols)
			}
			out = fmt.Sprintf("Result set complete: %d row(s)%s, %s — %s",
				pd.rows, cols, pktBytes(pd.resultBytes), tag)
		}
		pd.rows, pd.cols, pd.resultBytes, pd.colNames = 0, 0, 0, nil
		return out

	case 'I':
		return "EmptyQueryResponse"
	case 'n':
		return "NoData"
	case 't':
		n := 0
		if len(body) >= 2 {
			n = int(binary.BigEndian.Uint16(body))
		}
		return fmt.Sprintf("ParameterDescription: %d parameter(s)", n)
	case '1':
		return "ParseComplete"
	case '2':
		return "BindComplete"
	case '3':
		return "CloseComplete"
	case 's':
		return "PortalSuspended"

	case 'E': // ErrorResponse
		return pgErrorResponse(p, c, pc, body, true)
	case 'N': // NoticeResponse
		return pgErrorResponse(p, c, pc, body, false)

	case 'A': // NotificationResponse
		if len(body) >= 4 {
			ch, rest := pgTakeString(body[4:])
			return fmt.Sprintf("NotificationResponse on channel %q: %s", ch, pktEllipsis(pgString(rest), 80))
		}
		return "NotificationResponse"

	case 'G':
		pd.copyMode, pd.copyBytes = 'i', 0
		return "CopyInResponse — the server is ready to receive a COPY stream"
	case 'H':
		pd.copyMode, pd.copyBytes = 'o', 0
		if pc.basebackup {
			return "CopyOutResponse — base backup stream begins"
		}
		return "CopyOutResponse — a COPY stream begins"
	case 'W':
		pd.copyMode, pd.copyBytes = 'b', 0
		if pc.replication != "" {
			return fmt.Sprintf("CopyBothResponse — %s replication stream begins", pc.replication)
		}
		return "CopyBothResponse"

	case 'd': // CopyData
		pd.copyBytes += len(body)
		if pgAdoptReplication(pc, body) {
			return pgWALMessage(p, pc, body, ts)
		}
		if pc.basebackup {
			pc.baseBytes += len(body)
			if pc.baseBytes >= pktBigResultBytes && !pc.baseNoted {
				pc.baseNoted = true
				p.Issues = append(p.Issues, fmt.Sprintf(
					"Base backup is large — %s streamed so far; a new replica is cloning this server and the transfer competes with production traffic for disk and network",
					pktBytes(pc.baseBytes)))
			}
			return ""
		}
		return ""
	case 'c':
		out := fmt.Sprintf("CopyDone (%s)", pktBytes(pd.copyBytes))
		pd.copyBytes, pd.copyMode = 0, 0
		return out

	case 'v': // NegotiateProtocolVersion
		if len(body) >= 4 {
			return fmt.Sprintf("NegotiateProtocolVersion: the server speaks 3.%d",
				binary.BigEndian.Uint32(body)&0xffff)
		}
		return "NegotiateProtocolVersion"
	}
	return fmt.Sprintf("Server message %q, %d bytes", string(rune(typ)), len(body))
}

// pgAuthInfo decodes an Authentication message and flags the one that matters: a
// cleartext password on an unencrypted connection is a password on the wire, and a
// capture is the only place that is visible.
func pgAuthInfo(p *pktPacket, c *pktConn, pc *pktPGConn, body []byte) string {
	if len(body) < 4 {
		return "Authentication message"
	}
	switch binary.BigEndian.Uint32(body) {
	case 0:
		pc.authDone = true
		if pc.authMethod == "" {
			pc.authMethod = "trust (no password)"
			return "AuthenticationOk — no password was asked for (trust/peer in pg_hba.conf)"
		}
		return "AuthenticationOk"
	case 2:
		pc.authMethod = "Kerberos V5"
	case 3:
		pc.authMethod = "cleartext password"
		if !c.tls {
			p.Issues = append(p.Issues,
				"Cleartext password authentication on an unencrypted connection — the password crosses the network in the clear and is in this capture; scram-sha-256 (or TLS) fixes it")
		}
		return "AuthenticationCleartextPassword"
	case 5:
		pc.authMethod = "MD5 password"
		return "AuthenticationMD5Password (md5 is deprecated in favour of scram-sha-256)"
	case 6:
		pc.authMethod = "SCM credentials"
	case 7:
		pc.authMethod = "GSSAPI"
	case 8:
		return "AuthenticationGSSContinue"
	case 9:
		pc.authMethod = "SSPI"
	case 10:
		mechs := []string{}
		for _, s := range pgStringList(body[4:]) {
			if s != "" {
				mechs = append(mechs, s)
			}
		}
		pc.authMethod = "SCRAM"
		if len(mechs) > 0 {
			pc.authMethod = "SCRAM (" + strings.Join(mechs, ", ") + ")"
		}
		return "AuthenticationSASL: " + strings.Join(mechs, ", ")
	case 11:
		return "AuthenticationSASLContinue"
	case 12:
		return "AuthenticationSASLFinal"
	}
	return "Authentication " + pc.authMethod
}

// pgReadyForQuery closes a command cycle: it is where latency is measured, where a
// transaction left open becomes visible, and — for a capture that joined an existing
// connection — where the CLIENT direction becomes readable.
//
// That last one is the difference between a decoded capture and a wall of "joined
// mid-connection". A server anchors easily, because ReadyForQuery is unmistakable; a
// client is much harder, because its next message is usually Bind or Execute, which
// are five bytes of length and then binary parameters with nothing distinctive about
// them. But the protocol says what happens after ReadyForQuery: the server is idle
// and the client's next byte begins a message. So the anchor found in one direction
// is handed to the other, and one round trip after the capture starts, both are
// aligned. (pktmysql.go does the same thing for the same reason.)
func pgReadyForQuery(p *pktPacket, pc *pktPGConn, pd *pktPGDir, status byte, ts time.Time) string {
	name := map[byte]string{'I': "idle", 'T': "in transaction", 'E': "in failed transaction"}[status]
	if name == "" {
		name = fmt.Sprintf("status %q", string(rune(status)))
	}
	out := "ReadyForQuery (" + name + ")"
	if pc.cycleOpen && !pc.cycleTS.IsZero() {
		lag := ts.Sub(pc.cycleTS).Seconds() * 1000
		p.LagMS = lag
		if lag >= pktSlowResponseMS {
			p.Issues = append(p.Issues, fmt.Sprintf(
				"Slow response — %.0f ms from the client's %s to ReadyForQuery", lag, pc.cycleWhat))
		}
		out += fmt.Sprintf(", %.1f ms", lag)
	}
	pc.cycleOpen = false
	pc.txStatus = status
	if status == 'T' || status == 'E' {
		pc.txSince = ts
	} else {
		pc.txSince = time.Time{}
		pc.idleNoted = false
	}
	if status == 'E' {
		out += " — every statement until COMMIT or ROLLBACK will fail with 25P02"
	}
	pd.rows, pd.cols, pd.resultBytes = 0, 0, 0
	return out
}

// pgCycleStart marks the beginning of a request cycle, and reports a transaction
// that has been sitting idle. PostgreSQL's "idle in transaction" is not idleness:
// the snapshot is held, the locks are held, and VACUUM cannot clean up rows the
// transaction might still see.
func pgCycleStart(pc *pktPGConn, ts time.Time, what string) {
	if !pc.cycleOpen {
		pc.cycleOpen, pc.cycleTS, pc.cycleWhat = true, ts, what
	}
}

// pgIdleCheck is called on any client message so the gap can be measured against
// the last ReadyForQuery, and is separate from pgCycleStart because it must return
// an issue to the caller's frame.
func pgIdleCheck(p *pktPacket, pc *pktPGConn, ts time.Time) {
	if pc.txStatus != 'T' || pc.txSince.IsZero() || pc.idleNoted {
		return
	}
	if idle := ts.Sub(pc.txSince).Seconds(); idle >= pgIdleInTxSeconds {
		pc.idleNoted = true
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Idle in transaction for %.0f s before this message — the snapshot and every lock it took were held for that whole time, and VACUUM could not clean up behind it", idle))
	}
}

// ---------------------------------------------------------------- errors

// pgErrorResponse decodes an ErrorResponse or NoticeResponse. The fields are a
// NUL-separated list of tagged strings; S/V severity, C the SQLSTATE, M the message,
// and the rest context.
func pgErrorResponse(p *pktPacket, c *pktConn, pc *pktPGConn, body []byte, isError bool) string {
	fields := map[byte]string{}
	for _, f := range pgFields(body) {
		fields[f.tag] = f.val
	}
	severity, state, msg := fields['S'], fields['C'], fields['M']
	if v := fields['V']; v != "" {
		severity = v // untranslated severity, when the server sends it
	}
	detail, hint := fields['D'], fields['H']

	if !isError {
		out := fmt.Sprintf("Notice %s: %s", severity, pktEllipsis(pktPrintable(msg), 140))
		// A notice is usually noise, but two of them are the shutdown and the
		// recovery-conflict messages, which explain everything that follows.
		if iss := pgNoticeIssue(severity, msg); iss != "" {
			p.Issues = append(p.Issues, iss)
		}
		return out
	}

	c.stream.Errors++
	p.ErrState = state
	p.Status = fmt.Sprintf("Error %s: %s", state, pktEllipsis(pktPrintable(msg), 120))
	if name := pgStateName(state); name != "" {
		p.Status = fmt.Sprintf("Error %s (%s): %s", state, name, pktEllipsis(pktPrintable(msg), 110))
	}
	if iss := pgErrIssue(state, severity, msg, pc); iss != "" {
		p.Issues = append(p.Issues, iss)
	}
	// FATAL ends the connection; ERROR leaves it usable. An application reporting a
	// dropped connection is almost always looking at a FATAL it did not log.
	if strings.EqualFold(severity, "FATAL") || strings.EqualFold(severity, "PANIC") {
		p.Issues = append(p.Issues, fmt.Sprintf(
			"%s — the server closes the connection after this message; the client sees a lost connection rather than a query error", strings.ToUpper(severity)))
	}
	out := fmt.Sprintf("%s %s: %s", strings.ToUpper(pgOr(severity, "ERROR")), state,
		pktEllipsis(pktPrintable(msg), 140))
	if detail != "" {
		out += " | detail: " + pktEllipsis(pktPrintable(detail), 90)
	}
	if hint != "" {
		out += " | hint: " + pktEllipsis(pktPrintable(hint), 70)
	}
	return out
}

type pgField struct {
	tag byte
	val string
}

func pgFields(b []byte) []pgField {
	var out []pgField
	for len(b) > 0 {
		tag := b[0]
		if tag == 0 {
			break
		}
		s, rest := pgTakeString(b[1:])
		out = append(out, pgField{tag: tag, val: s})
		b = rest
	}
	return out
}

// ---------------------------------------------------------------- replication

// pgAdoptReplication reports whether this CopyData belongs to a replication stream,
// adopting the classification when the connection's startup was never captured.
//
// A standby that attached before the capture began has no startup message to read
// replication=true out of, and its stream is the most valuable thing on the port. The
// four streaming sub-types do not occur in an ordinary COPY, so a CopyData whose body
// starts with one — and whose length is exactly right for it (pgReplSubtypeLen) — is
// replication and nothing else. Physical is assumed, because the sub-types are shared;
// a logical stream corrects itself the moment a pgoutput message is recognised inside
// an XLogData.
func pgAdoptReplication(pc *pktPGConn, body []byte) bool {
	if pc.replication != "" {
		return true
	}
	if len(body) == 0 || !pgReplSubtypeLen(body[0], len(body)+4) {
		return false
	}
	pc.replication = "physical"
	pc.adopted = true
	return true
}

// pgWALMessage decodes one CopyData message on a walsender connection. The first
// byte says which of the four streaming messages it is.
func pgWALMessage(p *pktPacket, pc *pktPGConn, body []byte, ts time.Time) string {
	if len(body) == 0 {
		return ""
	}
	switch body[0] {
	case 'w': // XLogData
		if len(body) < 25 {
			return "XLogData (truncated)"
		}
		start := binary.BigEndian.Uint64(body[1:])
		end := binary.BigEndian.Uint64(body[9:])
		if end > pc.sentLSN {
			pc.sentLSN = end
		} else if start > pc.sentLSN {
			pc.sentLSN = start
		}
		wal := body[25:]
		// An adopted stream was assumed physical; a recognisable pgoutput message
		// proves it is logical, and saying so is the whole point of the distinction.
		if pc.replication == "logical" || pc.adopted {
			if info := pgLogicalMessage(pc, wal); info != "" {
				if pc.adopted && pc.replication != "logical" {
					pc.replication = "logical"
				}
				return fmt.Sprintf("XLogData at %s: %s", pgLSN(start), info)
			}
		}
		return fmt.Sprintf("XLogData: %s WAL at %s", pktBytes(len(wal)), pgLSN(start))

	case 'k': // Primary keepalive
		if len(body) < 18 {
			return "Primary keepalive (truncated)"
		}
		walEnd := binary.BigEndian.Uint64(body[1:])
		if walEnd > pc.sentLSN {
			pc.sentLSN = walEnd
		}
		out := fmt.Sprintf("Primary keepalive: WAL end %s", pgLSN(walEnd))
		if body[17] != 0 {
			out += ", reply requested"
			// The primary only asks when it has not heard back; on a healthy stream
			// the standby's own status updates arrive first.
			if !pc.lastStandbyReply.IsZero() && ts.Sub(pc.lastStandbyReply).Seconds() > 30 {
				p.Issues = append(p.Issues, fmt.Sprintf(
					"The primary is asking the standby to reply and has not heard from it for %.0f s — wal_receiver_status_interval has passed with no answer, which is what precedes a dropped replication connection",
					ts.Sub(pc.lastStandbyReply).Seconds()))
			}
		}
		return out
	}
	return fmt.Sprintf("Replication message %q, %d bytes", string(rune(body[0])), len(body))
}

// pgStandbyMessage decodes the standby's half of a replication stream, which is
// where lag becomes measurable: the standby reports the LSNs it has written,
// flushed and applied, and the difference from the primary's sent LSN is the lag,
// in bytes of WAL, with no access to either server.
func pgStandbyMessage(p *pktPacket, pc *pktPGConn, body []byte, ts time.Time) string {
	if len(body) == 0 {
		return ""
	}
	switch body[0] {
	case 'r': // Standby status update
		if len(body) < 34 {
			return "Standby status update (truncated)"
		}
		write := binary.BigEndian.Uint64(body[1:])
		flush := binary.BigEndian.Uint64(body[9:])
		apply := binary.BigEndian.Uint64(body[17:])
		pc.flushLSN, pc.applyLSN, pc.lastStandbyReply = flush, apply, ts
		out := fmt.Sprintf("Standby status: write %s, flush %s, apply %s",
			pgLSN(write), pgLSN(flush), pgLSN(apply))
		if pc.sentLSN > flush {
			behind := pc.sentLSN - flush
			out += fmt.Sprintf(", %s behind", pktBytes(int(behind)))
			if behind >= pgLagBytes && !pc.lagNoted {
				pc.lagNoted = true
				p.Issues = append(p.Issues, fmt.Sprintf(
					"Replication lag %s — the standby has flushed %s but the primary has sent up to %s; a synchronous standby this far behind stalls commits on the primary",
					pktBytes(int(behind)), pgLSN(flush), pgLSN(pc.sentLSN)))
			}
		}
		return out
	case 'h': // Hot standby feedback
		if len(body) < 13 {
			return "Hot standby feedback (truncated)"
		}
		return fmt.Sprintf("Hot standby feedback: xmin %d", binary.BigEndian.Uint32(body[9:]))
	}
	return fmt.Sprintf("Standby message %q, %d bytes", string(rune(body[0])), len(body))
}

// pgLogicalMessage decodes a pgoutput message inside XLogData — the built-in
// logical-decoding plugin, whose format is documented and stable. A third-party
// plugin (wal2json, Spock's own) is described by size instead of guessed at.
func pgLogicalMessage(pc *pktPGConn, b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 'B': // Begin
		if len(b) >= 21 {
			return fmt.Sprintf("BEGIN xid %d at %s", binary.BigEndian.Uint32(b[17:]), pgLSN(binary.BigEndian.Uint64(b[1:])))
		}
		return "BEGIN"
	case 'C': // Commit
		if len(b) >= 26 {
			return fmt.Sprintf("COMMIT at %s", pgLSN(binary.BigEndian.Uint64(b[9:])))
		}
		return "COMMIT"
	case 'R': // Relation
		if len(b) >= 6 {
			oid := binary.BigEndian.Uint32(b[1:])
			schema, rest := pgTakeString(b[5:])
			table, _ := pgTakeString(rest)
			pc.relations[oid] = schema + "." + table
			return "relation " + schema + "." + table
		}
		return "relation"
	case 'I', 'U', 'D':
		op := map[byte]string{'I': "INSERT", 'U': "UPDATE", 'D': "DELETE"}[b[0]]
		if len(b) >= 5 {
			if name := pc.relations[binary.BigEndian.Uint32(b[1:])]; name != "" {
				return op + " on " + name
			}
		}
		return op
	case 'T':
		return "TRUNCATE"
	case 'M':
		return "logical message"
	case 'O':
		return "origin"
	case 'S':
		return "stream start"
	}
	return ""
}

// pgReplicationCommand recognises the walsender commands, which are issued as
// ordinary simple queries on a replication connection.
func pgReplicationCommand(sql string) string {
	u := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(u, "IDENTIFY_SYSTEM"):
		return "physical"
	case strings.HasPrefix(u, "START_REPLICATION"):
		if strings.Contains(u, "LOGICAL") {
			return "logical"
		}
		return "physical"
	case strings.HasPrefix(u, "CREATE_REPLICATION_SLOT"):
		if strings.Contains(u, "LOGICAL") {
			return "logical"
		}
		return "physical"
	case strings.HasPrefix(u, "BASE_BACKUP"):
		return "physical"
	case strings.HasPrefix(u, "TIMELINE_HISTORY"), strings.HasPrefix(u, "READ_REPLICATION_SLOT"),
		strings.HasPrefix(u, "DROP_REPLICATION_SLOT"), strings.HasPrefix(u, "SHOW "):
		return ""
	}
	return ""
}

// pgReplicationIssue says what a walsender command means operationally.
func pgReplicationIssue(sql, kind string) string {
	u := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(u, "BASE_BACKUP"):
		return "BASE_BACKUP started — a new standby is cloning this server's entire data directory over this connection, the PostgreSQL equivalent of a full state transfer"
	case strings.HasPrefix(u, "START_REPLICATION"):
		return fmt.Sprintf("START_REPLICATION (%s) — WAL now streams continuously on this connection; its LSNs are what make lag measurable from a capture", kind)
	case strings.HasPrefix(u, "CREATE_REPLICATION_SLOT"):
		return "CREATE_REPLICATION_SLOT — a slot now holds WAL for this consumer; if the consumer stops and the slot is not dropped, the primary's disk fills"
	}
	return "Replication command: " + pktEllipsis(sql, 90)
}

// pgLSN formats a log sequence number the way PostgreSQL does everywhere else, so
// it can be pasted straight into pg_current_wal_lsn() comparisons.
func pgLSN(v uint64) string {
	return fmt.Sprintf("%X/%X", v>>32, v&0xffffffff)
}

// ---------------------------------------------------------------- small helpers

// pgString reads a NUL-terminated string from the head of b.
func pgString(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// pgTakeString reads a NUL-terminated string and returns the remainder.
func pgTakeString(b []byte) (string, []byte) {
	if i := indexByte(b, 0); i >= 0 {
		return string(b[:i]), b[i+1:]
	}
	return string(b), nil
}

// pgStringList splits a NUL-separated, NUL-terminated list.
func pgStringList(b []byte) []string {
	var out []string
	for len(b) > 0 {
		s, rest := pgTakeString(b)
		if s == "" {
			break
		}
		out = append(out, s)
		b = rest
	}
	return out
}

// pgStrings reads the startup message's key/value pairs.
func pgStrings(b []byte) [][]string {
	var out [][]string
	for len(b) > 0 {
		k, rest := pgTakeString(b)
		if k == "" {
			break
		}
		v, rest2 := pgTakeString(rest)
		out = append(out, []string{k, v})
		b = rest2
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// pgRowDescription reads the column count and as many names as are worth showing.
func pgRowDescription(b []byte) (int, []string) {
	if len(b) < 2 {
		return 0, nil
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	var names []string
	for i := 0; i < n && len(b) > 0; i++ {
		name, rest := pgTakeString(b)
		if len(rest) < 18 {
			break
		}
		b = rest[18:] // table oid, column attr, type oid, size, modifier, format
		if len(names) < 8 {
			names = append(names, name)
		}
	}
	if n > len(names) && len(names) == 8 {
		names = append(names, "…")
	}
	return n, names
}

// pgBindParams counts a Bind's parameters and returns the largest, since an
// oversized parameter is a real cause of slow round trips.
func pgBindParams(b []byte) (count, largest int) {
	if len(b) < 2 {
		return 0, 0
	}
	// Skip the parameter format codes.
	nf := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) < nf*2+2 {
		return 0, 0
	}
	b = b[nf*2:]
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	for i := 0; i < n && len(b) >= 4; i++ {
		l := int(int32(binary.BigEndian.Uint32(b)))
		b = b[4:]
		if l < 0 { // NULL
			continue
		}
		if l > largest {
			largest = l
		}
		if len(b) < l {
			break
		}
		b = b[l:]
	}
	return n, largest
}

// pgTagRows pulls the row count out of a CommandComplete tag ("INSERT 0 3",
// "UPDATE 12", "SELECT 42").
func pgTagRows(tag string) int {
	parts := strings.Fields(tag)
	if len(parts) == 0 {
		return 0
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0
	}
	return n
}

// pgNameOr quotes a statement or portal name, or names the unnamed one. Driver-
// generated names are hashes — asyncpg's are 51 characters — and printing one whole
// pushes the rest of the line off the screen, so they are shortened.
func pgNameOr(s, alt string) string {
	if s == "" {
		return alt
	}
	if len(s) > 22 {
		s = s[:20] + "…"
	}
	return strconv.Quote(s)
}

func pgOr(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}
