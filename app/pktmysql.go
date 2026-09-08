package main

// pktmysql.go — the MySQL half of the Packet Inspector's decoder, plus enough TLS
// to describe a connection that went encrypted.
//
// MySQL's client/server protocol is a stream of length-prefixed packets, not one
// packet per TCP segment: a 40 KB result set arrives as many segments, and a busy
// connection puts several MySQL packets in one segment. So each direction of each
// connection carries a byte buffer, and this file drains it whenever new payload
// arrives. A frame's Info line describes whatever *completed* inside it, which is
// what Wireshark shows and what makes a capture readable top to bottom.
//
// What is decoded, and why that list:
//
//	greeting            server version + connection id + whether TLS is offered
//	handshake response  the user, the schema, and CLIENT_SSL — the moment a
//	                    connection decides to encrypt, which is where a plaintext
//	                    inspector goes blind and has to say so
//	COM_QUERY etc.      the SQL text, which is the reason anyone opens this tool
//	OK / ERR / EOF      affected rows, insert id, and the error code + SQLSTATE,
//	                    the other half of "what actually happened"
//	result sets         column count, then rows counted to the terminator, so a
//	                    response reads "42 rows, 8.1 KB" instead of "binary data"
//	replication         COM_BINLOG_DUMP[_GTID] and the event stream that follows —
//	                    a primary's port carries this next to ordinary queries, and
//	                    mistaking it for a broken result set would poison the rest
//	                    of the stream
//
// Reference: MySQL's "Client/Server Protocol" chapter. Only the parts a capture
// can be sure of are read; anything ambiguous (a binary-protocol row, a compressed
// stream) is reported as such rather than guessed at.

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MySQL commands, from include/my_command.h.
const (
	comQuit           = 0x01
	comInitDB         = 0x02
	comQuery          = 0x03
	comFieldList      = 0x04
	comCreateDB       = 0x05
	comDropDB         = 0x06
	comRefresh        = 0x07
	comStatistics     = 0x09
	comProcessInfo    = 0x0a
	comProcessKill    = 0x0c
	comDebug          = 0x0d
	comPing           = 0x0e
	comChangeUser     = 0x11
	comBinlogDump     = 0x12
	comRegisterSlave  = 0x15
	comStmtPrepare    = 0x16
	comStmtExecute    = 0x17
	comStmtSendLong   = 0x18
	comStmtClose      = 0x19
	comStmtReset      = 0x1a
	comSetOption      = 0x1b
	comStmtFetch      = 0x1c
	comResetConn      = 0x1f
	comBinlogDumpGTID = 0x1e
)

// pktComName is the command's protocol name, used for the Command field and the
// Info line. Unknown bytes are shown numerically rather than dropped — a capture
// of a newer server should still be readable.
func pktComName(b byte) string {
	switch b {
	case comQuit:
		return "COM_QUIT"
	case comInitDB:
		return "COM_INIT_DB"
	case comQuery:
		return "COM_QUERY"
	case comFieldList:
		return "COM_FIELD_LIST"
	case comCreateDB:
		return "COM_CREATE_DB"
	case comDropDB:
		return "COM_DROP_DB"
	case comRefresh:
		return "COM_REFRESH"
	case comStatistics:
		return "COM_STATISTICS"
	case comProcessInfo:
		return "COM_PROCESS_INFO"
	case comProcessKill:
		return "COM_PROCESS_KILL"
	case comDebug:
		return "COM_DEBUG"
	case comPing:
		return "COM_PING"
	case comChangeUser:
		return "COM_CHANGE_USER"
	case comBinlogDump:
		return "COM_BINLOG_DUMP"
	case comBinlogDumpGTID:
		return "COM_BINLOG_DUMP_GTID"
	case comRegisterSlave:
		return "COM_REGISTER_SLAVE"
	case comStmtPrepare:
		return "COM_STMT_PREPARE"
	case comStmtExecute:
		return "COM_STMT_EXECUTE"
	case comStmtSendLong:
		return "COM_STMT_SEND_LONG_DATA"
	case comStmtClose:
		return "COM_STMT_CLOSE"
	case comStmtReset:
		return "COM_STMT_RESET"
	case comSetOption:
		return "COM_SET_OPTION"
	case comStmtFetch:
		return "COM_STMT_FETCH"
	case comResetConn:
		return "COM_RESET_CONNECTION"
	}
	return fmt.Sprintf("COM_0x%02x", b)
}

// Capability flags that matter here.
const (
	capSSL      = 0x00000800 // CLIENT_SSL — the connection switches to TLS
	capCompress = 0x00000020 // CLIENT_COMPRESS — zlib framing wraps every packet
	capZstd     = 1 << 26    // CLIENT_ZSTD_COMPRESSION_ALGORITHM
)

// pktEOFMaxLen bounds what may be an EOF/OK terminator rather than a row. The
// classic EOF is 5 bytes; the OK that replaces it under CLIENT_DEPRECATE_EOF adds
// affected-rows, insert-id, status flags, warnings and optionally session-state
// tracking, which stays well inside this. A ROW starting with 0xfe carries an
// 8-byte length, so it is at least 16 MB — orders of magnitude away.
const pktEOFMaxLen = 512

// ---------------------------------------------------------------- entry point

// pktAppDecode consumes one frame's TCP payload for a connection direction and
// annotates the frame with whatever it completes.
func pktAppDecode(p *pktPacket, c *pktConn, dir *pktDirState, fromClient bool, payload []byte, ts time.Time) {
	// Once a connection is encrypted there is nothing to parse — say what the
	// record is and stop. Guessing MySQL structure out of ciphertext is how a
	// packet decoder produces confident nonsense.
	if c.tls {
		p.Proto = "TLS"
		p.Info = pktTLSInfo(payload, c.tlsSealed)
		p.Status = "Encrypted"
		// A TLS alert is how a handshake or a session fails; the client reports it
		// as 2026 CR_SSL_CONNECTION_ERROR (or 2075 when the server rejects the SNI).
		if strings.Contains(p.Info, "Alert") {
			p.Issues = append(p.Issues,
				"TLS alert — handshake or session rejected, client sees 2026 CR_SSL_CONNECTION_ERROR")
		}
		if !c.tlsSealed && pktTLSSeals(payload) {
			c.tlsSealed = true
		}
		return
	}
	// A compressed stream is unreadable for the same reason ciphertext is.
	if c.compressed {
		p.Proto = "MySQL/compressed"
		p.Status = "Compressed"
		p.Info = fmt.Sprintf("Compressed payload, %d bytes (not decoded)", len(payload))
		return
	}
	// The client's SSLRequest is followed immediately by a TLS ClientHello, which
	// may even share the segment; from the first TLS record on, treat the
	// connection as encrypted.
	if c.sslRequested && pktLooksTLS(payload) {
		c.tls = true
		p.Proto = "TLS"
		p.Info = pktTLSInfo(payload, false)
		p.Status = "Encrypted"
		c.tlsSealed = pktTLSSeals(payload)
		return
	}

	dir.buf = append(dir.buf, payload...)
	var infos []string
	var seqNote string
	sawMySQL, msgs := false, 0
	for {
		msg, seq, lastSeq, ok := pktNextMySQL(dir)
		if !ok {
			break
		}
		sawMySQL, msgs = true, msgs+1
		// MySQL's packet sequence byte belongs to the CONNECTION, not to a
		// direction: it starts at 0 with each client command and then increments
		// across both directions for that command's whole response (greeting 0,
		// handshake response 1, OK 2). A break in it means the byte stream was
		// corrupted or re-ordered — the server's 1156 ER_NET_PACKETS_OUT_OF_ORDER
		// and the client's 2027 CR_MALFORMED_PACKET. A client packet numbered 0 is
		// always a legitimate restart.
		if c.synced && c.haveSeqByte && seq != c.nextSeqByte && !(fromClient && seq == 0) {
			seqNote = fmt.Sprintf(
				"MySQL packet sequence expected %d, got %d — server reports 1156, client 2027",
				c.nextSeqByte, seq)
		}
		// lastSeq, not seq: an oversized payload spans several chunks and consumes a
		// sequence number for each one.
		c.haveSeqByte, c.nextSeqByte = true, lastSeq+1
		info := ""
		if fromClient {
			info = pktDecodeClient(p, c, dir, msg, seq, ts)
		} else {
			info = pktDecodeServer(p, c, dir, msg, seq, ts)
		}
		if info != "" {
			infos = append(infos, info)
		}
		if c.tls {
			// The SSLRequest just flipped the connection; anything left in this
			// frame is TLS, not MySQL.
			dir.buf = nil
			break
		}
	}
	if sawMySQL {
		p.Proto = "MySQL"
	}
	// A sequence break next to a decoded error is not news: the error IS the reason
	// the framing stopped lining up (a server refusing an oversized packet mid-read,
	// for instance). Reporting both says the same thing twice and buries the useful
	// half. Only an unexplained break is worth a line of its own.
	if seqNote != "" && p.ErrCode == 0 {
		p.Issues = append(p.Issues, seqNote)
	}
	switch {
	case len(infos) > 0:
		p.Info = strings.Join(infos, " | ")
		if len(infos) > 3 {
			p.Info = fmt.Sprintf("%s | +%d more", strings.Join(infos[:3], " | "), len(infos)-3)
		}
	case msgs > 0:
		// Result-set rows and definition packets are counted, not narrated — but
		// the frame that carried them still needs a line of its own.
		what := "MySQL data"
		if dir.inResults {
			what = fmt.Sprintf("Result-set data (row %d of %d-column set)", dir.resultRow, dir.resultCol)
		}
		p.Info = fmt.Sprintf("%s, %d bytes in %d packet(s)", what, len(payload), msgs)
	case len(dir.buf) > 0:
		// Payload that is part of a MySQL packet still being assembled.
		p.Proto = "MySQL"
		p.Info = fmt.Sprintf("[continuation] %d bytes, %d buffered", len(payload), len(dir.buf))
	}
}

// pktNextMySQL pulls one complete MySQL packet out of a direction's buffer.
// Payloads of exactly 0xffffff bytes continue in the next packet; those are
// concatenated so a large statement or row is seen whole.
func pktNextMySQL(dir *pktDirState) (payload []byte, seq, lastSeq byte, ok bool) {
	// First pass: walk the chunk headers WITHOUT copying. A 16 MB packet arrives
	// over thousands of segments and this function runs on every one of them; the
	// original version accumulated the payload as it went and threw the copy away
	// each time the tail turned out to be missing, which made one real 20 MB row
	// take 6.5 seconds to decode. Now a failed attempt costs one pass over the
	// chunk headers — there are two of them for a 20 MB row.
	consumed, chunks, total := 0, 0, 0
	var first, last byte
	for {
		if len(dir.buf) < consumed+4 {
			return nil, 0, 0, false
		}
		h := dir.buf[consumed:]
		n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
		if len(dir.buf) < consumed+4+n {
			return nil, 0, 0, false
		}
		if chunks == 0 {
			first = h[3]
		}
		last = h[3]
		chunks++
		total += n
		consumed += 4 + n
		if n != 0xffffff {
			break
		}
	}

	// Second pass: hand back the payload. The common case is one chunk, where the
	// buffer already holds it contiguously and no copy is needed at all — the
	// caller only reads it before the next append, and appends land at or past
	// `consumed`, never inside what is returned.
	if chunks == 1 {
		payload = dir.buf[4:consumed]
	} else {
		payload = make([]byte, 0, total)
		off := 0
		for i := 0; i < chunks; i++ {
			n := int(dir.buf[off]) | int(dir.buf[off+1])<<8 | int(dir.buf[off+2])<<16
			payload = append(payload, dir.buf[off+4:off+4+n]...)
			off += 4 + n
		}
	}
	dir.buf = dir.buf[consumed:]
	if len(dir.buf) == 0 {
		dir.buf = nil
	}
	return payload, first, last, true
}

// ---------------------------------------------------------------- client → server

func pktDecodeClient(p *pktPacket, c *pktConn, dir *pktDirState, msg []byte, seq byte, ts time.Time) string {
	if len(msg) == 0 {
		return ""
	}
	// The first thing a client sends is its handshake response, not a command —
	// but only if this really is the start of the connection.
	if !dir.greeted && c.synced {
		dir.greeted = true
		return pktDecodeHandshakeResponse(p, c, msg)
	}
	// Joined mid-connection: a command is only believable if it looks like one.
	// Anything else is the tail of something the capture never saw the start of,
	// and saying so beats decoding a fragment as a command.
	if !dir.synced && !pktPlausibleCommand(msg) {
		p.Status = "Unsynchronised"
		return fmt.Sprintf("Client payload, %d bytes (capture joined mid-connection)", len(msg))
	}
	dir.synced = true

	cmd := msg[0]
	name := pktComName(cmd)
	if strings.HasPrefix(name, "COM_0x") && !pktPlausibleCommand(msg) {
		// Not a command MySQL defines, and it does not read like one either.
		p.Status = "Unrecognised"
		return fmt.Sprintf("Unrecognised client payload, %d bytes (first byte 0x%02x)", len(msg), cmd)
	}
	p.Command = name
	arg := msg[1:]

	// Remember what was asked so the response can be given a latency and the
	// request row can be told how it turned out.
	c.pendingCmd, c.pendingTS, c.pendingOpen = name, ts, true
	c.pendingQuery = ""

	// A complete command means the server's next packet starts its response, so
	// the other direction is now anchored even if this capture began mid-flight.
	// Whatever was buffered there is the tail of a response nothing can interpret.
	if !c.s2c.synced {
		c.s2c.synced = true
		c.s2c.buf = nil
		c.s2c.inResults, c.s2c.pendingDefs = false, 0
		c.s2c.resultRow, c.s2c.resultCol, c.s2c.resultLen = 0, 0, 0
	}

	switch cmd {
	case comQuery, comStmtPrepare:
		sql := pktCleanSQL(string(arg))
		c.pendingQuery = sql
		p.Query = sql
		c.stream.Queries++
		verb := "Query"
		if cmd == comStmtPrepare {
			verb = "Prepare"
		}
		return fmt.Sprintf("%s: %s", verb, pktEllipsis(sql, 120))
	case comInitDB:
		c.database = string(arg)
		p.Query = "USE " + c.database
		return "Init DB: " + c.database
	case comStmtExecute:
		if len(arg) >= 4 {
			id := binary.LittleEndian.Uint32(arg)
			p.Query = fmt.Sprintf("-- execute prepared statement %d", id)
			return fmt.Sprintf("Execute stmt %d", id)
		}
		return "Execute prepared statement"
	case comStmtClose, comStmtReset, comStmtFetch:
		if len(arg) >= 4 {
			return fmt.Sprintf("%s stmt %d", strings.TrimPrefix(name, "COM_STMT_"), binary.LittleEndian.Uint32(arg))
		}
		return name
	case comBinlogDump, comBinlogDumpGTID:
		// This connection is a replica's IO thread, and everything the server
		// sends from here is a binlog event stream rather than query results.
		c.s2c.binlog = true
		p.Query = "-- replication: " + name
		if cmd == comBinlogDump && len(arg) >= 10 {
			return fmt.Sprintf("Binlog dump from pos %d (server_id %d)",
				binary.LittleEndian.Uint32(arg), binary.LittleEndian.Uint32(arg[6:]))
		}
		return "Binlog dump (GTID auto-position)"
	case comRegisterSlave:
		if len(arg) >= 4 {
			return fmt.Sprintf("Register replica (server_id %d)", binary.LittleEndian.Uint32(arg))
		}
		return "Register replica"
	case comChangeUser:
		if i := strings.IndexByte(string(arg), 0); i > 0 {
			c.user = string(arg[:i])
		}
		return "Change user: " + c.user
	case comQuit:
		c.pendingOpen = false // no response is coming
		return "Quit"
	case comPing:
		return "Ping"
	}
	if len(arg) > 0 {
		return fmt.Sprintf("%s (%d bytes)", name, len(arg))
	}
	return name
}

// pktDecodeHandshakeResponse reads the client's answer to the greeting. A response
// that is only the 32-byte fixed header with CLIENT_SSL set is an SSLRequest: the
// real response comes back inside TLS, and this is the last plaintext the inspector
// will see on the connection.
func pktDecodeHandshakeResponse(p *pktPacket, c *pktConn, msg []byte) string {
	if len(msg) < 4 {
		return "Handshake response (truncated)"
	}
	caps := binary.LittleEndian.Uint32(msg)
	if caps&capSSL != 0 && len(msg) <= 36 {
		c.sslRequested = true
		p.Status = "Encrypted"
		p.Query = "-- SSLRequest: connection switches to TLS here"
		return "SSLRequest — TLS starts, payload no longer readable"
	}
	// A compressed connection wraps every MySQL packet in a zlib/zstd frame, so
	// nothing below this point parses. Say so rather than emitting garbage — and
	// name the errors that live here, since a broken compressed stream is what
	// produces 1157 on the server and 2065/2066 on the client.
	if caps&(capCompress|capZstd) != 0 {
		c.compressed = true
		p.Status = "Compressed"
		p.Issues = append(p.Issues,
			"Compressed protocol negotiated — payload not decoded (uncompress failures surface as 1157 / 2065 / 2066)")
		p.Query = "-- CLIENT_COMPRESS negotiated: packets are zlib/zstd framed from here"
		return "Login request with compression — payload no longer readable"
	}
	// HandshakeResponse41: caps(4) max_packet(4) charset(1) reserved(23) user\0
	if len(msg) > 32 {
		rest := msg[32:]
		if i := bytesIndexZero(rest); i >= 0 {
			c.user = string(rest[:i])
			// The schema follows the auth response, whose own length is encoded
			// per capability flags; the user is what matters here, and a schema
			// arrives as COM_INIT_DB often enough to make guessing unnecessary.
		}
	}
	// The handshake response can also carry CLIENT_SSL when TLS is not actually
	// used (the flag is negotiated, the switch is what counts) — only the short
	// SSLRequest above flips the connection.
	if c.user != "" {
		p.Query = "-- login as " + c.user
		return "Login request: user " + c.user
	}
	return "Handshake response"
}

// ---------------------------------------------------------------- server → client

func pktDecodeServer(p *pktPacket, c *pktConn, dir *pktDirState, msg []byte, seq byte, ts time.Time) string {
	if len(msg) == 0 {
		return ""
	}
	if !dir.greeted && c.synced {
		dir.greeted = true
		return pktDecodeGreeting(p, c, msg)
	}

	// Authentication continues after the greeting with packet types that exist
	// nowhere else in the protocol. Reading them as ordinary responses is how a
	// caching_sha2_password login — the default on MySQL 8 — decoded as a
	// one-column result set: its AuthMoreData packet begins with 0x01, which is
	// also a length-encoded column count of 1.
	if c.authPhase {
		switch {
		case msg[0] == 0x00: // OK: authentication finished
			c.authPhase = false
			rows, _, _, info := pktParseOK(msg)
			_ = rows
			if info = pktPrintable(info); info != "" {
				return "Login OK — " + pktEllipsis(info, 60)
			}
			p.Status = "Success"
			return "Login OK"
		case msg[0] == 0xff: // ERR: rejected
			c.authPhase = false
			code, state, text := pktParseERR(msg)
			text = pktPrintable(text)
			p.ErrCode = code
			p.Status = fmt.Sprintf("Error %d (%s): %s", code, state, text)
			p.Issues = append(p.Issues, pktErrIssue(code, text))
			c.stream.Errors++
			verb := "Login refused"
			if e, ok := pktErrCatalog[code]; ok && e.Class == pktErrNet {
				verb = "Connection dropped during handshake"
			}
			return fmt.Sprintf("%s: %d %s", verb, code, pktEllipsis(text, 80))
		case msg[0] == 0xfe: // AuthSwitchRequest: the server names another plugin
			name := ""
			if i := bytesIndexZero(msg[1:]); i > 0 {
				name = pktPrintable(string(msg[1 : 1+i]))
			}
			return "Auth switch request: " + name
		case msg[0] == 0x01: // AuthMoreData: plugin-specific continuation
			what := "continuation"
			if len(msg) >= 2 {
				switch msg[1] {
				case 0x03:
					what = "fast auth succeeded"
				case 0x04:
					what = "full authentication required"
				}
			}
			return "Auth more data (" + what + ")"
		}
	}

	// Give the response the request's latency, once, on the first packet back.
	if c.pendingOpen {
		p.LagMS = float64(ts.Sub(c.pendingTS).Microseconds()) / 1000
		if p.LagMS >= pktSlowResponseMS {
			p.Issues = append(p.Issues,
				fmt.Sprintf("High latency — %.0f ms to first response byte", p.LagMS))
		}
		if c.pendingQuery != "" && p.Query == "" {
			p.Query = c.pendingQuery
		}
		p.Command = c.pendingCmd
		c.pendingOpen = false
	}

	// A binlog stream is a run of event packets, each an OK byte followed by the
	// event header. It never terminates with an EOF, so it must not be run
	// through the result-set state machine. A replica's connection normally
	// outlives any capture, so the COM_BINLOG_DUMP that started it is usually not
	// in the file — hence the shape test as well as the flag.
	if msg[0] == 0x00 && (dir.binlog || pktLooksBinlogEvent(msg)) {
		dir.binlog = true
		return pktBinlogEventInfo(msg)
	}
	// Mid-connection, the only server packet whose meaning is unambiguous is an
	// ERR: it carries its own marker byte, a code and a SQLSTATE. An OK and a
	// binary result-set row both start with 0x00, and a text row can start with
	// anything, so without the connection's history the rest is just bytes.
	if !dir.synced {
		if pktPlausibleERR(msg) {
			code, state, text := pktParseERR(msg)
			p.ErrCode = code
			p.Status = fmt.Sprintf("Error %d (%s): %s", code, state, pktPrintable(text))
			p.Issues = append(p.Issues, pktErrIssue(code, pktPrintable(text)))
			c.stream.Errors++
			return fmt.Sprintf("Error %d: %s", code, pktEllipsis(pktPrintable(text), 100))
		}
		if p.Status == "" {
			p.Status = "Unsynchronised"
		}
		return fmt.Sprintf("Server payload, %d bytes (capture joined mid-connection)", len(msg))
	}

	// Column and parameter definitions carry nothing a reader wants and would
	// otherwise be counted as result rows. Consume them quietly, plus the EOF that
	// closes each block on a client without CLIENT_DEPRECATE_EOF.
	if dir.pendingDefs > 0 && msg[0] != 0xff {
		dir.pendingDefs--
		return ""
	}

	switch {
	case msg[0] == 0xff:
		code, state, text := pktParseERR(msg)
		text = pktPrintable(text)
		p.ErrCode = code
		p.Status = fmt.Sprintf("Error %d (%s): %s", code, state, text)
		p.Issues = append(p.Issues, pktErrIssue(code, text))
		c.stream.Errors++
		dir.inResults, dir.pendingDefs = false, 0
		pktAnchorClient(c)
		return fmt.Sprintf("Error %d: %s", code, pktEllipsis(text, 100))

	// COM_STMT_PREPARE gets its own OK shape: 0x00, statement id, column count,
	// parameter count — read as a plain OK it claims a nonsense number of affected
	// rows, which is what the first live capture showed.
	case msg[0] == 0x00 && !dir.inResults && c.pendingCmd == "COM_STMT_PREPARE" && len(msg) >= 12:
		id := binary.LittleEndian.Uint32(msg[1:])
		cols := int(binary.LittleEndian.Uint16(msg[5:]))
		params := int(binary.LittleEndian.Uint16(msg[7:]))
		dir.pendingDefs = cols + params
		p.Status = "Success"
		c.pendingCmd = ""
		if !c.c2s.synced {
			c.c2s.synced, c.c2s.buf = true, nil
		}
		return fmt.Sprintf("Prepared OK: stmt %d, %d column(s), %d parameter(s)", id, cols, params)

	case msg[0] == 0x00 && !dir.inResults:
		rows, insertID, warn, info := pktParseOK(msg)
		p.Status = "Success"
		p.Rows = rows
		s := fmt.Sprintf("OK: %d row(s) affected", rows)
		if insertID > 0 {
			s += fmt.Sprintf(", insert_id %d", insertID)
		}
		if warn > 0 {
			// Reported, never flagged: MySQL raises warnings for entirely ordinary
			// statements, so an Issues entry per warning buries the real problems.
			s += fmt.Sprintf(", %d warning(s)", warn)
		}
		if info = pktPrintable(info); info != "" {
			s += " — " + pktEllipsis(info, 60)
		}
		pktAnchorClient(c)
		return s

	// The 5-byte EOF that closes the column-definition block on a client without
	// CLIENT_DEPRECATE_EOF. It looks exactly like the end of the result set, and
	// swallowing it here is what keeps an empty result set from reporting twice.
	case dir.expectDefsEOF && dir.pendingDefs == 0 && msg[0] == 0xfe && len(msg) <= 5 && dir.resultRow == 0:
		dir.expectDefsEOF = false
		return ""

	// EOF, or the OK packet that replaces it under CLIENT_DEPRECATE_EOF.
	//
	// The SIZE test is load-bearing. 0xfe is also the length-encoded-integer marker
	// for "an 8-byte length follows", so a text-protocol row whose first column is
	// 16 MB or larger — a LONGBLOB — begins with 0xfe too. Treating any 0xfe packet
	// inside a result set as the terminator swallowed a real 20 MB row and reported
	// "0 row(s), 0 B". A terminator is always small; such a row is always enormous,
	// so there is no overlap between the two.
	case msg[0] == 0xfe && len(msg) <= pktEOFMaxLen:
		if dir.inResults {
			dir.expectDefsEOF, dir.pendingDefs = false, 0
			dir.inResults = false
			rows := dir.resultRow
			p.Rows = rows
			p.Status = "Success"
			s := fmt.Sprintf("Result set complete: %d row(s), %d column(s), %s",
				rows, dir.resultCol, pktBytes(dir.resultLen))
			if dir.resultLen >= pktBigResultBytes {
				p.Issues = append(p.Issues, fmt.Sprintf("Heavy result set — %s", pktBytes(dir.resultLen)))
			}
			dir.resultRow, dir.resultCol, dir.resultLen = 0, 0, 0
			pktAnchorClient(c)
			return s
		}
		return "EOF"

	// Only outside a result set: 0xfb is NULL for a text-protocol row, so a row
	// whose first column is NULL would otherwise read as a LOCAL INFILE request.
	case msg[0] == 0xfb && !dir.inResults:
		return "LOCAL INFILE request"

	case dir.inResults:
		dir.resultRow++
		dir.resultLen += len(msg)
		return "" // rows are counted, not narrated: one line per row is noise

	default:
		// A length-encoded column count opens a result set, and is followed by
		// exactly that many column-definition packets. Those are consumed quietly —
		// counting them as rows is why a 9-column, 8-row answer first reported
		// "17 row(s)".
		if n, _, ok := pktLenEncInt(msg); ok && n > 0 && n < 4096 {
			dir.inResults = true
			dir.resultCol = int(n)
			dir.pendingDefs = int(n)
			dir.expectDefsEOF = true
			dir.resultRow, dir.resultLen = 0, 0
			return fmt.Sprintf("Result set: %d column(s)", n)
		}
		return fmt.Sprintf("Response (%d bytes)", len(msg))
	}
}

// pktAnchorClient re-anchors the client direction after a response has completed:
// the next thing the client sends is the start of a command. The mirror of the
// anchor a command gives the server direction — between them, a connection that
// was already running when the capture started becomes fully decodable one
// request/response round-trip in.
func pktAnchorClient(c *pktConn) {
	if !c.c2s.synced {
		c.c2s.synced, c.c2s.buf = true, nil
	}
}

// pktDecodeGreeting reads the server's initial handshake: protocol version, server
// version string, connection id, and whether the server offers TLS.
func pktDecodeGreeting(p *pktPacket, c *pktConn, msg []byte) string {
	if len(msg) < 2 {
		return "Server greeting (truncated)"
	}
	if msg[0] == 0xff {
		// A server that rejects the connection before the greeting: too many
		// connections, or the host is blocked.
		code, state, text := pktParseERR(msg)
		p.ErrCode, p.Status = code, fmt.Sprintf("Error %d (%s): %s", code, state, text)
		p.Issues = append(p.Issues, pktErrIssue(code, text))
		c.stream.Errors++
		return fmt.Sprintf("Connection refused by server: %d %s", code, pktEllipsis(text, 80))
	}
	proto := msg[0]
	rest := msg[1:]
	i := bytesIndexZero(rest)
	if i < 0 || proto != 10 {
		// Protocol 10 is the only greeting a MySQL 5.x/8.x server sends; anything
		// else here means this is not the start of a connection after all.
		return fmt.Sprintf("Server payload, %d bytes (not a greeting)", len(msg))
	}
	c.version = pktPrintable(string(rest[:i]))
	c.authPhase = true
	p.Query = "-- server greeting: MySQL " + c.version
	out := fmt.Sprintf("Server greeting: %s (protocol %d)", c.version, proto)
	if len(rest) > i+5 {
		out += fmt.Sprintf(", connection id %d", binary.LittleEndian.Uint32(rest[i+1:]))
	}
	return out
}

// ---------------------------------------------------------------- plausibility

// pktPlausibleCommand reports whether a client payload really looks like the start
// of a command packet. Used only when the capture joined mid-connection, where the
// alternative is decoding the tail of a large INSERT as whatever command byte its
// bytes happen to spell.
func pktPlausibleCommand(msg []byte) bool {
	if len(msg) == 0 {
		return false
	}
	switch msg[0] {
	case comQuery, comStmtPrepare, comInitDB, comCreateDB, comDropDB, comFieldList:
		// Text-bearing commands: the argument must read as text.
		return pktMostlyPrintable(msg[1:])
	case comQuit, comPing, comStatistics, comProcessInfo, comResetConn, comDebug:
		return len(msg) == 1
	case comStmtExecute, comStmtClose, comStmtReset, comStmtFetch, comStmtSendLong:
		return len(msg) >= 5
	case comBinlogDump, comBinlogDumpGTID, comRegisterSlave, comChangeUser, comSetOption, comRefresh, comProcessKill:
		return true
	}
	return false
}

// pktPlausibleERR reports whether a server payload is an ERR packet: the marker
// byte, a non-zero error code in MySQL's range, and the '#' SQLSTATE marker with a
// printable message. Every other server packet type is ambiguous mid-stream.
func pktPlausibleERR(msg []byte) bool {
	if len(msg) < 10 || msg[0] != 0xff {
		return false
	}
	code := int(binary.LittleEndian.Uint16(msg[1:]))
	if code < 1000 || code > 4000 {
		return false
	}
	if msg[3] != '#' {
		return false
	}
	return pktMostlyPrintable(msg[9:])
}

// pktLooksBinlogEvent reports whether a server payload is a binlog event: the OK
// byte, then a 19-byte event header whose declared size matches the payload and
// whose type is one a server actually sends.
func pktLooksBinlogEvent(msg []byte) bool {
	if len(msg) < 20 || msg[0] != 0x00 {
		return false
	}
	b := msg[1:]
	size := int(binary.LittleEndian.Uint32(b[9:]))
	if size != len(b) {
		return false
	}
	return !strings.HasPrefix(pktBinlogEventName(b[4]), "event type")
}

// pktMostlyPrintable reports whether a byte slice reads as text. SQL is ASCII plus
// whatever the client's charset carries in literals, so the bar is "not binary".
func pktMostlyPrintable(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	bad := 0
	for _, c := range b {
		if c < 0x09 || (c > 0x0d && c < 0x20) || c == 0x7f {
			bad++
		}
	}
	return bad*10 < len(b) // under 10% control bytes
}

// pktPrintable strips control bytes from a string taken off the wire, so a
// misparse cannot smuggle escape sequences into a log line or the UI.
func pktPrintable(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
}

// ---------------------------------------------------------------- payload readers

// pktParseOK reads an OK packet: affected rows, last insert id, warnings and the
// human-readable info string (which carries "Records: 3 Duplicates: 0 …").
func pktParseOK(msg []byte) (rows int, insertID uint64, warnings int, info string) {
	b := msg[1:]
	n, w, ok := pktLenEncInt(b)
	if !ok {
		return 0, 0, 0, ""
	}
	rows = int(n)
	b = b[w:]
	if id, w2, ok2 := pktLenEncInt(b); ok2 {
		insertID = id
		b = b[w2:]
	}
	if len(b) >= 4 {
		warnings = int(binary.LittleEndian.Uint16(b[2:]))
		b = b[4:]
		info = strings.TrimSpace(string(b))
	}
	return rows, insertID, warnings, info
}

// pktParseERR reads an ERR packet: code, SQLSTATE and message.
func pktParseERR(msg []byte) (code int, sqlState, text string) {
	if len(msg) < 3 {
		return 0, "", ""
	}
	code = int(binary.LittleEndian.Uint16(msg[1:]))
	b := msg[3:]
	if len(b) > 6 && b[0] == '#' {
		sqlState = string(b[1:6])
		b = b[6:]
	}
	return code, sqlState, strings.TrimSpace(string(b))
}

// pktLenEncInt reads MySQL's length-encoded integer.
func pktLenEncInt(b []byte) (v uint64, width int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	switch f := b[0]; {
	case f < 0xfb:
		return uint64(f), 1, true
	case f == 0xfc && len(b) >= 3:
		return uint64(binary.LittleEndian.Uint16(b[1:])), 3, true
	case f == 0xfd && len(b) >= 4:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, 4, true
	case f == 0xfe && len(b) >= 9:
		return binary.LittleEndian.Uint64(b[1:]), 9, true
	}
	return 0, 0, false
}

// pktBinlogEventInfo names a replication event. The header is fixed: timestamp(4)
// type(1) server_id(4) event_size(4) log_pos(4) flags(2).
func pktBinlogEventInfo(msg []byte) string {
	b := msg[1:]
	if len(b) < 19 {
		return "Binlog event (truncated)"
	}
	typ := b[4]
	size := binary.LittleEndian.Uint32(b[9:])
	pos := binary.LittleEndian.Uint32(b[13:])
	return fmt.Sprintf("Binlog event: %s, %d bytes, next pos %d", pktBinlogEventName(typ), size, pos)
}

// pktBinlogEventName covers the event types a running primary actually emits;
// anything else is shown by number.
func pktBinlogEventName(t byte) string {
	switch t {
	case 0x02:
		return "QUERY_EVENT"
	case 0x04:
		return "ROTATE_EVENT"
	case 0x0f:
		return "FORMAT_DESCRIPTION_EVENT"
	case 0x10:
		return "XID_EVENT"
	case 0x13:
		return "TABLE_MAP_EVENT"
	case 0x1e:
		return "WRITE_ROWS_EVENT"
	case 0x1f:
		return "UPDATE_ROWS_EVENT"
	case 0x20:
		return "DELETE_ROWS_EVENT"
	case 0x21:
		return "GTID_LOG_EVENT"
	case 0x22:
		return "ANONYMOUS_GTID_LOG_EVENT"
	case 0x23:
		return "PREVIOUS_GTIDS_LOG_EVENT"
	case 0x1b:
		return "HEARTBEAT_LOG_EVENT"
	case 0x27:
		return "TRANSACTION_PAYLOAD_EVENT"
	}
	return fmt.Sprintf("event type 0x%02x", t)
}

// ---------------------------------------------------------------- error catalog

// pktErrClass is what an ERR packet tells you about, which decides how loudly the
// UI says it: a network or auth failure is an incident, a syntax error is a bug in
// somebody's SQL.
type pktErrClass string

const (
	pktErrNet   pktErrClass = "network"  // the connection itself failed
	pktErrAuth  pktErrClass = "auth"     // rejected at connect / handshake
	pktErrLimit pktErrClass = "limit"    // a server limit was reached
	pktErrLock  pktErrClass = "lock"     // contention
	pktErrTopo  pktErrClass = "topology" // replication / read-only
	pktErrSQL   pktErrClass = "sql"      // the statement was wrong
)

// pktErrInfo describes one MySQL error code.
type pktErrInfo struct {
	Sym   string
	Label string
	Class pktErrClass
}

// pktErrCatalog is the server-side error codes worth naming when they cross the wire.
//
// Scope is deliberate: these are the codes a SERVER puts in an ERR packet, so a
// capture can see them. MySQL's 2xxx codes (CR_CONN_HOST_ERROR, CR_SERVER_LOST,
// CR_NET_PACKET_TOO_LARGE …) are the *client library's* own diagnoses and never
// appear on the wire at all — what a capture can see is the evidence the client drew
// them from, which pktConnEvidence detects separately. The MY-xxxxxx numbers are
// error-log identifiers and exist only in the log, which is what pktserverlog.go
// reads.
var pktErrCatalog = map[int]pktErrInfo{
	// Communication errors — the connection broke or the framing did.
	1152: {"ER_ABORTING_CONNECTION", "Aborted connection", pktErrNet},
	1153: {"ER_NET_PACKET_TOO_LARGE", "Packet bigger than max_allowed_packet", pktErrNet},
	1154: {"ER_NET_READ_ERROR_FROM_PIPE", "Read error from the connection pipe", pktErrNet},
	1155: {"ER_NET_FCNTL_ERROR", "fcntl() error on the connection", pktErrNet},
	1156: {"ER_NET_PACKETS_OUT_OF_ORDER", "Packets out of order", pktErrNet},
	1157: {"ER_NET_UNCOMPRESS_ERROR", "Could not uncompress a packet", pktErrNet},
	1158: {"ER_NET_READ_ERROR", "Error reading communication packets", pktErrNet},
	1159: {"ER_NET_READ_INTERRUPTED", "Timeout reading communication packets", pktErrNet},
	1160: {"ER_NET_ERROR_ON_WRITE", "Error writing communication packets", pktErrNet},
	1161: {"ER_NET_WRITE_INTERRUPTED", "Timeout writing communication packets", pktErrNet},
	1184: {"ER_NEW_ABORTING_CONNECTION", "Aborted connection", pktErrNet},
	1835: {"ER_MALFORMED_PACKET", "Malformed communication packet", pktErrNet},

	// Replication transport.
	1189: {"ER_SOURCE_NET_READ", "Replication: net error reading from source", pktErrTopo},
	1190: {"ER_SOURCE_NET_WRITE", "Replication: net error writing to source", pktErrTopo},

	// Connection establishment and handshake.
	1040: {"ER_CON_COUNT_ERROR", "Too many connections", pktErrLimit},
	1042: {"ER_BAD_HOST_ERROR", "Cannot resolve the client's address", pktErrNet},
	1043: {"ER_HANDSHAKE_ERROR", "Bad handshake", pktErrAuth},
	1045: {"ER_ACCESS_DENIED_ERROR", "Authentication failed", pktErrAuth},
	1047: {"ER_UNKNOWN_COM_ERROR", "Unknown command", pktErrNet},
	1053: {"ER_SERVER_SHUTDOWN", "Server shutdown in progress", pktErrNet},
	1129: {"ER_HOST_IS_BLOCKED", "Host blocked after too many connection errors", pktErrAuth},
	1130: {"ER_HOST_NOT_PRIVILEGED", "Host not allowed to connect", pktErrAuth},
	1203: {"ER_TOO_MANY_USER_CONNECTIONS", "User exceeded max_user_connections", pktErrLimit},

	// Contention and topology — not in the network list, but the operational errors
	// most often chased alongside it.
	1205: {"ER_LOCK_WAIT_TIMEOUT", "Lock wait timeout", pktErrLock},
	1213: {"ER_LOCK_DEADLOCK", "Deadlock detected", pktErrLock},
	1290: {"ER_OPTION_PREVENTS_STATEMENT", "Read-only server rejected a write", pktErrTopo},
	1301: {"ER_WARN_ALLOWED_PACKET_OVERFLOWED", "Result truncated at max_allowed_packet", pktErrNet},
	1317: {"ER_QUERY_INTERRUPTED", "Query interrupted", pktErrNet},
	1836: {"ER_READ_ONLY_MODE", "Read-only server rejected a write", pktErrTopo},
}

// pktErrIssue turns a MySQL error into the Issues text: the human label, the code and
// the symbol, so it can be searched for by any of the three. Unlisted codes are still
// reported — the point is that the listed ones read as operational events rather than
// as a number.
//
// One code needs its message read as well as its number. PXC reuses **1047**
// (ER_UNKNOWN_COM_ERROR) for "WSREP has not yet prepared node for application use" — a
// node that is joining, donating or otherwise not synced, refusing queries. Labelling
// that "Unknown command" is technically the code's original meaning and operationally
// useless: a real capture of a PXC node held 69 of them, and what they meant was that
// clients were being turned away by a node that was not ready.
func pktErrIssue(code int, text string) string {
	if code == 1047 && strings.Contains(strings.ToUpper(text), "WSREP") {
		return "Node not ready for application use (1047 wsrep)"
	}
	if e, ok := pktErrCatalog[code]; ok {
		return fmt.Sprintf("%s (%d %s)", e.Label, code, e.Sym)
	}
	return fmt.Sprintf("MySQL error %d: %s", code, pktEllipsis(text, 60))
}

// pktErrIsSevere reports whether an error code is an operational failure rather than
// a bad statement. Drives the timeline's red-vs-amber and the summary's ordering.
func pktErrIsSevere(code int) bool {
	e, ok := pktErrCatalog[code]
	if !ok {
		return false
	}
	switch e.Class {
	case pktErrNet, pktErrAuth, pktErrLimit, pktErrLock, pktErrTopo:
		return true
	}
	return false
}

// pktErrLabels is every label the catalog can produce, so the severity table in
// pktinspect.go stays in step with the catalog instead of repeating it.
func pktErrLabels(severeOnly bool) []string {
	seen := map[string]bool{}
	var out []string
	for code, e := range pktErrCatalog {
		if severeOnly && !pktErrIsSevere(code) {
			continue
		}
		if !seen[e.Label] {
			seen[e.Label] = true
			out = append(out, e.Label)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- TLS

// pktLooksTLS reports whether a payload starts with a plausible TLS record: a
// handshake/alert/appdata type byte and a 3.x version.
func pktLooksTLS(b []byte) bool {
	if len(b) < 3 {
		return false
	}
	switch b[0] {
	case 0x14, 0x15, 0x16, 0x17:
		return b[1] == 0x03 && b[2] <= 0x04
	}
	return false
}

// pktTLSInfo describes the TLS records in a payload — the handshake steps by name,
// and application data by size. This is the honest ceiling of a plaintext capture:
// the sizes and the timing are real, the contents are not available.
//
// `sealed` says the handshake has gone encrypted, which changes what may be claimed. A
// handshake record's first body byte names the message (ClientHello, Certificate…) only
// while the handshake is in the clear; after a ChangeCipherSpec — and under TLS 1.3,
// after the ServerHello — that byte is ciphertext, and reading it produced a live capture
// labelled "Handshake: ClientHello" for a record that was nothing of the kind.
func pktTLSInfo(b []byte, sealed bool) string {
	var parts []string
	for len(b) >= 5 {
		typ, ver, n := b[0], binary.BigEndian.Uint16(b[1:]), int(binary.BigEndian.Uint16(b[3:]))
		if n <= 0 || 5+n > len(b) {
			// Record split across segments: report what is known.
			parts = append(parts, fmt.Sprintf("%s (partial, %d bytes)", pktTLSRecordName(typ, nil, sealed), len(b)-5))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s (%d bytes)",
			pktTLSVersionName(ver), pktTLSRecordName(typ, b[5:5+n], sealed), n))
		b = b[5+n:]
	}
	if len(parts) == 0 {
		return fmt.Sprintf("TLS record (partial, %d bytes)", len(b))
	}
	return strings.Join(parts, " | ")
}

// pktTLSSeals reports whether this payload ends the readable part of the handshake: a
// ChangeCipherSpec, or a ServerHello, after which TLS 1.3 encrypts what follows.
func pktTLSSeals(b []byte) bool {
	for len(b) >= 5 {
		typ, n := b[0], int(binary.BigEndian.Uint16(b[3:]))
		if typ == 0x14 { // ChangeCipherSpec
			return true
		}
		if typ == 0x16 && n > 0 && 5+n <= len(b) && b[5] == 0x02 { // ServerHello
			return true
		}
		if n <= 0 || 5+n > len(b) {
			return false
		}
		b = b[5+n:]
	}
	return false
}

func pktTLSRecordName(typ byte, body []byte, sealed bool) string {
	switch typ {
	case 0x14:
		return "ChangeCipherSpec"
	case 0x15:
		return "Alert"
	case 0x16:
		if sealed {
			return "Handshake (encrypted)" // the message type is inside the encryption
		}
		if len(body) > 0 {
			switch body[0] {
			case 0x01:
				return "Handshake: ClientHello"
			case 0x02:
				return "Handshake: ServerHello"
			case 0x0b:
				return "Handshake: Certificate"
			case 0x0c:
				return "Handshake: ServerKeyExchange"
			case 0x0d:
				return "Handshake: CertificateRequest"
			case 0x0e:
				return "Handshake: ServerHelloDone"
			case 0x10:
				return "Handshake: ClientKeyExchange"
			case 0x14:
				return "Handshake: Finished"
			}
		}
		return "Handshake"
	case 0x17:
		return "Application Data"
	}
	return fmt.Sprintf("record type 0x%02x", typ)
}

func pktTLSVersionName(v uint16) string {
	switch v {
	case 0x0301:
		return "TLS 1.0"
	case 0x0302:
		return "TLS 1.1"
	case 0x0303:
		return "TLS 1.2"
	case 0x0304:
		return "TLS 1.3"
	}
	return fmt.Sprintf("TLS 0x%04x", v)
}

// ---------------------------------------------------------------- small helpers

func bytesIndexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// pktCleanSQL collapses a statement onto one line so it reads in a table cell, and
// bounds what is kept.
func pktCleanSQL(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	s = strings.TrimSpace(s)
	if len(s) > pktQueryTextMax {
		s = s[:pktQueryTextMax] + " …"
	}
	return s
}

func pktEllipsis(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// pktBytes formats a byte count the way the detail panel shows payload size.
func pktBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}
