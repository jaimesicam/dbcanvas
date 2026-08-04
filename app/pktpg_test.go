package main

// pktpg_test.go — the PostgreSQL decoder, driven by hand-built captures.
//
// Same approach as pktinspect_test.go: real pcap bytes with real Ethernet/IPv4/TCP
// headers, so a passing test means the decoder would read the same thing off the wire.
// The builders below (pgMsg, pgStartup, pgErr…) construct the protocol exactly as
// PostgreSQL frames it, including the two places that are easy to get wrong and were
// got wrong here first: the length field counts itself, and the answer to SSLRequest
// is a naked byte outside the framing entirely.
//
// Several of these exist because the live cluster broke them. A capture of a busy
// Patroni leader is almost entirely connections older than the capture, and the first
// version decoded the server's half and left every client frame as "joined
// mid-connection" — 22 000 of them (TestPGClientAnchorsOnReadyForQuery). Its standby
// streams never anchored at all, because a walsender sends neither Query nor
// ReadyForQuery, ever (TestPGReplicationAnchorsMidStream).

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- builders

const pgPort = 5432

func pgC2S(seq, ack uint32, flags uint8, payload []byte) []byte {
	return ethIPv4TCP(cliIP, srvIP, cliPort, pgPort, seq, ack, flags, 64240, payload)
}
func pgS2C(seq, ack uint32, flags uint8, payload []byte) []byte {
	return ethIPv4TCP(srvIP, cliIP, pgPort, cliPort, seq, ack, flags, 64240, payload)
}

// pgMsg frames a typed message: type byte, int32 length INCLUDING the length field,
// then the body.
func pgMsg(typ byte, body []byte) []byte {
	out := make([]byte, 5, 5+len(body))
	out[0] = typ
	binary.BigEndian.PutUint32(out[1:], uint32(4+len(body)))
	return append(out, body...)
}

// pgStartup builds the untyped first message: int32 length, int32 protocol, then
// NUL-terminated key/value pairs and a final NUL.
func pgStartup(params ...string) []byte {
	var body []byte
	for _, s := range params {
		body = append(body, []byte(s)...)
		body = append(body, 0)
	}
	body = append(body, 0)
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	binary.BigEndian.PutUint32(out[4:], pgProtocol30)
	return append(out, body...)
}

// pgFirst builds one of the three magic first messages.
func pgFirst(code uint32, extra []byte) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, uint32(8+len(extra)))
	binary.BigEndian.PutUint32(out[4:], code)
	return append(out, extra...)
}

func pgAuthOK() []byte       { return pgMsg('R', []byte{0, 0, 0, 0}) }
func pgReady(st byte) []byte { return pgMsg('Z', []byte{st}) }
func pgQuery(sql string) []byte {
	return pgMsg('Q', append([]byte(sql), 0))
}
func pgCmdDone(tag string) []byte { return pgMsg('C', append([]byte(tag), 0)) }

// pgErr builds an ErrorResponse (or a NoticeResponse) out of tagged fields.
func pgErr(typ byte, severity, state, msg string, extra map[byte]string) []byte {
	var body []byte
	add := func(tag byte, v string) {
		body = append(body, tag)
		body = append(body, []byte(v)...)
		body = append(body, 0)
	}
	add('S', severity)
	add('V', severity)
	add('C', state)
	add('M', msg)
	for tag, v := range extra {
		add(tag, v)
	}
	body = append(body, 0)
	return pgMsg(typ, body)
}

// pgRowDesc builds a RowDescription for the named columns.
func pgRowDesc(names ...string) []byte {
	body := make([]byte, 2)
	binary.BigEndian.PutUint16(body, uint16(len(names)))
	for _, n := range names {
		body = append(body, []byte(n)...)
		body = append(body, 0)
		body = append(body, make([]byte, 18)...) // table/column/type/size/modifier/format
	}
	return pgMsg('T', body)
}

// pgDataRow builds a DataRow with one column per value.
func pgDataRow(vals ...string) []byte {
	body := make([]byte, 2)
	binary.BigEndian.PutUint16(body, uint16(len(vals)))
	for _, v := range vals {
		l := make([]byte, 4)
		binary.BigEndian.PutUint32(l, uint32(len(v)))
		body = append(body, l...)
		body = append(body, []byte(v)...)
	}
	return pgMsg('D', body)
}

// pgCopyData wraps a replication sub-message in CopyData.
func pgCopyData(body []byte) []byte { return pgMsg('d', body) }

// pgXLogData builds a 'w' message: LSNs, clock, then WAL bytes.
func pgXLogData(start, end uint64, wal []byte) []byte {
	b := make([]byte, 25)
	b[0] = 'w'
	binary.BigEndian.PutUint64(b[1:], start)
	binary.BigEndian.PutUint64(b[9:], end)
	binary.BigEndian.PutUint64(b[17:], 700000000)
	return pgCopyData(append(b, wal...))
}

// pgStandbyStatus builds an 'r' message: the LSNs the standby has reached.
func pgStandbyStatus(write, flush, apply uint64) []byte {
	b := make([]byte, 34)
	b[0] = 'r'
	binary.BigEndian.PutUint64(b[1:], write)
	binary.BigEndian.PutUint64(b[9:], flush)
	binary.BigEndian.PutUint64(b[17:], apply)
	binary.BigEndian.PutUint64(b[25:], 700000000)
	return pgCopyData(b)
}

// pgKeepalive builds a 'k' message.
func pgKeepalive(walEnd uint64, reply byte) []byte {
	b := make([]byte, 18)
	b[0] = 'k'
	binary.BigEndian.PutUint64(b[1:], walEnd)
	binary.BigEndian.PutUint64(b[9:], 700000000)
	b[17] = reply
	return pgCopyData(b)
}

// decodePG decodes a capture as PostgreSQL on 5432.
func decodePG(t *testing.T, b *pcapBuilder) *pktDecoded {
	t.Helper()
	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: pgPort, Engine: pktEnginePostgres})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d
}

// pgSession opens a captured connection with a SYN handshake, so the decoder knows it
// is reading the stream from its first byte.
func pgSession(b *pcapBuilder) *sampleConn {
	c := &sampleConn{b: b, cseq: 1000, sseq: 5000}
	c.b.frame(0, pgC2S(c.cseq, 0, tcpSYN, nil))
	c.cseq++
	c.b.frame(time.Millisecond, pgS2C(c.sseq, c.cseq, tcpSYN|tcpACK, nil))
	c.sseq++
	return c
}

func pgSend(c *sampleConn, after time.Duration, payload []byte) {
	c.b.frame(c.tick(after), pgC2S(c.cseq, c.sseq, tcpACK|tcpPSH, payload))
	c.cseq += uint32(len(payload))
}
func pgRecv(c *sampleConn, after time.Duration, payload []byte) {
	c.b.frame(c.tick(after), pgS2C(c.sseq, c.cseq, tcpACK|tcpPSH, payload))
	c.sseq += uint32(len(payload))
}

// infoHas reports whether any packet's Info contains s.
func infoHas(d *pktDecoded, s string) bool {
	for _, p := range d.Packets {
		if strings.Contains(p.Info, s) {
			return true
		}
	}
	return false
}

// issueHas reports whether any packet raised an issue containing s.
func issueHas(d *pktDecoded, s string) bool {
	for _, p := range d.Packets {
		for _, i := range p.Issues {
			if strings.Contains(i, s) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- basics

func TestPGSessionDecodes(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "alice", "database", "rental", "application_name", "carsim"))
	pgRecv(c, time.Millisecond, append(pgMsg('R', []byte{0, 0, 0, 10, 'S', 'C', 'R', 'A', 'M', '-', 'S', 'H', 'A', '-', '2', '5', '6', 0, 0}), nil...))
	pgSend(c, time.Millisecond, pgMsg('p', []byte("SCRAM-SHA-256\x00n,,n=,r=abc")))
	pgRecv(c, time.Millisecond, append(append(pgAuthOK(),
		pgMsg('S', []byte("server_version\x0016.14\x00"))...), pgReady('I')...))
	pgSend(c, 2*time.Millisecond, pgQuery("SELECT id, name FROM cars WHERE id = 7"))
	pgRecv(c, 3*time.Millisecond, append(append(append(
		pgRowDesc("id", "name"), pgDataRow("7", "Corolla")...), pgCmdDone("SELECT 1")...), pgReady('I')...))
	pgSend(c, time.Millisecond, pgMsg('X', nil))

	d := decodePG(t, b)
	for _, want := range []string{
		"StartupMessage 3.0: user=alice database=rental application_name=carsim",
		"AuthenticationSASL: SCRAM-SHA-256",
		"SASLInitialResponse: SCRAM-SHA-256",
		"Query: SELECT id, name FROM cars WHERE id = 7",
		"Row description: 2 column(s) — id, name",
		"Result set complete: 1 row(s), 2 column(s)",
		"ReadyForQuery (idle)",
		"Terminate",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
	if len(d.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(d.Streams))
	}
	st := d.Streams[0]
	if st.User != "alice" || st.Database != "rental" || st.Version != "16.14" {
		t.Errorf("stream identity: user=%q db=%q version=%q", st.User, st.Database, st.Version)
	}
	if st.Queries != 1 {
		t.Errorf("queries = %d, want 1", st.Queries)
	}
	if d.Engine != pktEnginePostgres {
		t.Errorf("engine = %q", d.Engine)
	}
}

func TestPGExtendedProtocol(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))

	// Parse a named statement, then Bind/Execute/Sync it — the shape every driver
	// with a statement cache produces.
	parse := pgMsg('P', append(append([]byte("stmt1\x00"), []byte("UPDATE cars SET mileage=$1 WHERE id=$2")...), 0, 0, 0))
	bind := pgMsg('B', func() []byte {
		var body []byte
		body = append(body, []byte("\x00")...)      // portal: unnamed
		body = append(body, []byte("stmt1\x00")...) // statement
		body = append(body, 0, 0)                   // no format codes
		body = append(body, 0, 2)                   // two parameters
		for _, v := range []string{"12345", "7"} {
			l := make([]byte, 4)
			binary.BigEndian.PutUint32(l, uint32(len(v)))
			body = append(body, l...)
			body = append(body, []byte(v)...)
		}
		body = append(body, 0, 0) // no result format codes
		return body
	}())
	exec := pgMsg('E', append([]byte("\x00"), 0, 0, 0, 0))
	pgSend(c, time.Millisecond, append(append(append(parse, bind...), exec...), pgMsg('S', nil)...))
	pgRecv(c, 2*time.Millisecond, append(append(
		pgMsg('1', nil), pgMsg('2', nil)...), pgCmdDone("UPDATE 1")...))
	// ReadyForQuery in its own frame: a frame's Info line shows the first three
	// messages and then "+N more", so a fourth one would be summarised away and the
	// transaction status — the thing being checked here — would not be visible.
	pgRecv(c, time.Millisecond, pgReady('T'))

	d := decodePG(t, b)
	for _, want := range []string{
		`Parse "stmt1": UPDATE cars SET mileage=$1 WHERE id=$2`,
		`Bind "stmt1" → portal unnamed portal, 2 parameter(s)`,
		"Execute portal unnamed",
		"ParseComplete",
		"BindComplete",
		"CommandComplete: UPDATE 1",
		"ReadyForQuery (in transaction)",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
	// The Bind carried the statement's SQL forward, which is what makes a Bind row
	// readable at all — the SQL is not in the Bind message. (The frame's Command is
	// the LAST message it completed, so the SQL is what identifies it, not Command.)
	found := false
	for _, p := range d.Packets {
		if strings.Contains(p.Query, "UPDATE cars SET mileage=$1 WHERE id=$2") {
			found = true
		}
	}
	if !found {
		t.Error("Bind did not carry the prepared statement's SQL")
	}
}

// TestPGUnnamedParseRepeated is the one extended-protocol finding that is about cost
// rather than failure: an unnamed statement re-parsed every time is a plan thrown away
// every time.
func TestPGUnnamedParseRepeated(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))
	sql := "SELECT * FROM bookings WHERE id = $1"
	for i := 0; i < pgParseRepeat+2; i++ {
		pgSend(c, time.Millisecond, append(
			pgMsg('P', append(append([]byte("\x00"), []byte(sql)...), 0, 0, 0)), pgMsg('S', nil)...))
		pgRecv(c, time.Millisecond, append(pgMsg('1', nil), pgReady('I')...))
	}
	d := decodePG(t, b)
	if !issueHas(d, "parsed 20 times as an unnamed prepared statement") {
		t.Error("re-parsing the same unnamed statement was not flagged")
	}
	// Once, not twenty-two times.
	n := 0
	for _, p := range d.Packets {
		for _, i := range p.Issues {
			if strings.Contains(i, "unnamed prepared statement") {
				n++
			}
		}
	}
	if n != 1 {
		t.Errorf("flagged %d times, want exactly 1", n)
	}
}

// ---------------------------------------------------------------- errors

func TestPGErrorResponses(t *testing.T) {
	cases := []struct {
		name             string
		severity, state  string
		msg              string
		wantInfo         string
		wantIssue        string
		wantNoIssueAtAll bool
	}{
		{name: "deadlock", severity: "ERROR", state: "40P01", msg: "deadlock detected",
			wantInfo: "ERROR 40P01: deadlock detected", wantIssue: "Deadlock detected"},
		{name: "too many connections", severity: "FATAL", state: "53300",
			msg:      "sorry, too many clients already",
			wantInfo: "FATAL 53300", wantIssue: "Too many connections"},
		{name: "read only", severity: "ERROR", state: "25006",
			msg:      "cannot execute INSERT in a read-only transaction",
			wantInfo: "ERROR 25006", wantIssue: "A write was attempted on a read-only connection"},
		{name: "recovery conflict", severity: "ERROR", state: "57014",
			msg:      "canceling statement due to conflict with recovery",
			wantInfo: "57014", wantIssue: "standby"},
		{name: "statement timeout", severity: "ERROR", state: "57014",
			msg:      "canceling statement due to statement timeout",
			wantInfo: "57014", wantIssue: "statement_timeout"},
		{name: "no pg_hba entry", severity: "FATAL", state: "28000",
			msg:      `no pg_hba.conf entry for host "10.0.0.9", user "bob", database "rental", no encryption`,
			wantInfo: "28000", wantIssue: "No pg_hba.conf entry matched"},
		{name: "bad protocol", severity: "FATAL", state: "0A000",
			msg:      "unsupported frontend protocol 18501.19532: server supports 3.0 to 3.0",
			wantInfo: "0A000", wantIssue: "protocol version this server does not speak"},
		// An ordinary application error is reported but is NOT a finding: a capture full
		// of unique violations from a workload that expects them would otherwise drown
		// everything worth seeing.
		{name: "unique violation", severity: "ERROR", state: "23505",
			msg:      `duplicate key value violates unique constraint "bookings_pkey"`,
			wantInfo: "ERROR 23505", wantNoIssueAtAll: true},
		{name: "syntax error", severity: "ERROR", state: "42601",
			msg: `syntax error at or near "form"`, wantInfo: "42601", wantNoIssueAtAll: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newPcap(pktLinkEther)
			c := pgSession(b)
			pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
			pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))
			pgSend(c, time.Millisecond, pgQuery("SELECT 1"))
			pgRecv(c, time.Millisecond, append(
				pgErr('E', tc.severity, tc.state, tc.msg, map[byte]string{'D': "some detail"}),
				pgReady('E')...))
			d := decodePG(t, b)
			if !infoHas(d, tc.wantInfo) {
				t.Errorf("no packet shows %q", tc.wantInfo)
			}
			if tc.wantIssue != "" && !issueHas(d, tc.wantIssue) {
				t.Errorf("issue %q not raised", tc.wantIssue)
			}
			if tc.wantNoIssueAtAll {
				for _, p := range d.Packets {
					for _, i := range p.Issues {
						if !strings.HasPrefix(i, "TCP") {
							t.Errorf("ordinary SQL error raised a finding: %q", i)
						}
					}
				}
			}
			// SQLSTATE lands in its own field, not in MySQL's numeric one.
			for _, p := range d.Packets {
				if strings.Contains(p.Info, tc.state) && p.ErrState != tc.state {
					t.Errorf("ErrState = %q, want %q", p.ErrState, tc.state)
				}
				if p.ErrCode != 0 {
					t.Errorf("ErrCode set on a PostgreSQL error: %d", p.ErrCode)
				}
			}
			if d.Streams[0].Errors != 1 {
				t.Errorf("stream errors = %d, want 1", d.Streams[0].Errors)
			}
		})
	}
}

// TestPGFatalEndsConnection checks the distinction an application never sees in its own
// logs: ERROR leaves the session usable, FATAL does not.
func TestPGFatalEndsConnection(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, pgErr('E', "FATAL", "28P01",
		`password authentication failed for user "bob"`, nil))
	d := decodePG(t, b)
	if !issueHas(d, "the server closes the connection after this message") {
		t.Error("a FATAL was not reported as ending the connection")
	}
	if !issueHas(d, "Password authentication failed") {
		t.Error("28P01 was not named")
	}
}

// ---------------------------------------------------------------- TLS / SSL

func TestPGSSLRequestAccepted(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgFirst(pgSSLRequest, nil))
	pgRecv(c, time.Millisecond, []byte{'S'})
	// A ClientHello: TLS record, handshake, version, length.
	hello := []byte{0x16, 0x03, 0x01, 0x00, 0x2c, 0x01, 0x00, 0x00, 0x28, 0x03, 0x03}
	hello = append(hello, make([]byte, 0x28-7)...)
	pgSend(c, time.Millisecond, hello)
	d := decodePG(t, b)
	if !infoHas(d, "SSLRequest") {
		t.Error("SSLRequest not decoded")
	}
	if !infoHas(d, "SSL accepted ('S')") {
		t.Error("the server's naked 'S' was not decoded")
	}
	if !d.Streams[0].TLS {
		t.Error("stream not marked as TLS")
	}
	for _, p := range d.Packets {
		if p.PayloadLen > 0 && strings.Contains(p.Info, "Query") {
			t.Errorf("invented SQL inside TLS: %q", p.Info)
		}
	}
}

func TestPGSSLRefused(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgFirst(pgSSLRequest, nil))
	pgRecv(c, time.Millisecond, []byte{'N'})
	// sslmode=require: the client gives up here, and the capture holds nothing else.
	c.b.frame(c.tick(time.Millisecond), pgC2S(c.cseq, c.sseq, tcpACK|tcpFIN, nil))
	d := decodePG(t, b)
	if !infoHas(d, "SSL refused ('N')") {
		t.Error("a refused SSLRequest was not decoded")
	}
	if !issueHas(d, "sslmode=require") {
		t.Error("the consequence of a refusal was not stated")
	}
}

// TestPGSSLRefusedThenPlaintext is the shape of every connection to a server with
// ssl = off from a client at its default sslmode: ask, be refused, carry on in the
// clear. The StartupMessage that follows the refusal is ANOTHER untyped message, and
// treating the connection's untyped state as used up by the SSLRequest cost the
// startup parameters and the whole authentication exchange — silently, since the
// stream recovered at the first ReadyForQuery and the queries decoded fine.
//
// Found by capturing against a Patroni member, which has ssl = off: the client frames
// read "[framing lost] 63 bytes" while the server's half decoded perfectly.
func TestPGSSLRefusedThenPlaintext(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgFirst(pgSSLRequest, nil))
	pgRecv(c, time.Millisecond, []byte{'N'})
	// …and now the real startup, in the clear.
	pgSend(c, time.Millisecond, pgStartup("user", "postgres", "database", "postgres", "application_name", "psql"))
	pgRecv(c, time.Millisecond, pgMsg('R', []byte{0, 0, 0, 10, 'S', 'C', 'R', 'A', 'M', '-', 'S', 'H', 'A', '-', '2', '5', '6', 0, 0}))
	pgSend(c, time.Millisecond, pgMsg('p', []byte("SCRAM-SHA-256\x00n,,n=,r=rOprNGfwEbeRWgbN")))
	pgRecv(c, time.Millisecond, pgMsg('R', []byte{0, 0, 0, 11}))
	pgSend(c, time.Millisecond, pgMsg('p', []byte("c=biws,r=rOprNGfwEbeRWgbN,p=dHzbZapWIk4jUhN+")))
	pgRecv(c, time.Millisecond, concat(pgMsg('R', []byte{0, 0, 0, 12}), pgAuthOK(),
		pgMsg('S', []byte("server_version\x0016.14\x00")), pgReady('I')))
	pgSend(c, time.Millisecond, pgQuery("select current_setting('ssl')"))
	pgRecv(c, time.Millisecond, concat(pgRowDesc("current_setting"), pgDataRow("off"),
		pgCmdDone("SELECT 1"), pgReady('I')))
	pgSend(c, time.Millisecond, pgMsg('X', nil))

	d := decodePG(t, b)
	for _, want := range []string{
		"SSL refused ('N')",
		"StartupMessage 3.0: user=postgres database=postgres application_name=psql",
		"AuthenticationSASL: SCRAM-SHA-256",
		"SASLInitialResponse: SCRAM-SHA-256",
		"Query: select current_setting('ssl')",
		"Terminate",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
	// Nothing may be reported as unreadable: every byte of this connection is in the
	// clear and the capture holds all of it.
	for _, p := range d.Packets {
		if strings.Contains(p.Info, "framing lost") || strings.Contains(p.Info, "joined mid-connection") {
			t.Errorf("#%d could not be framed: %q", p.No, p.Info)
		}
	}
	// The connection's identity comes from a StartupMessage that arrives AFTER the
	// refusal, so losing it lost the user and the database too.
	if st := d.Streams[0]; st.User != "postgres" || st.Database != "postgres" || st.Version != "16.14" {
		t.Errorf("stream identity: user=%q db=%q version=%q", st.User, st.Database, st.Version)
	}
	if d.Streams[0].TLS {
		t.Error("the connection is in the clear and must not be marked TLS")
	}
}

// TestPGGSSThenSSLThenStartup is libpq with both negotiations declined: three untyped
// messages in a row before anything typed.
func TestPGGSSThenSSLThenStartup(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgFirst(pgGSSENCRequest, nil))
	pgRecv(c, time.Millisecond, []byte{'N'})
	pgSend(c, time.Millisecond, pgFirst(pgSSLRequest, nil))
	pgRecv(c, time.Millisecond, []byte{'N'})
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, concat(pgAuthOK(), pgReady('I')))
	d := decodePG(t, b)
	for _, want := range []string{
		"GSSENCRequest", "GSSAPI encryption refused ('N')",
		"SSLRequest", "SSL refused ('N')",
		"StartupMessage 3.0: user=bob database=rental",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
}

// TestPGCleartextPasswordFlagged covers a finding only a capture can make: the password
// is on the wire, in this file.
func TestPGCleartextPasswordFlagged(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, pgMsg('R', []byte{0, 0, 0, 3}))
	pgSend(c, time.Millisecond, pgMsg('p', append([]byte("hunter2"), 0)))
	d := decodePG(t, b)
	if !infoHas(d, "AuthenticationCleartextPassword") {
		t.Error("cleartext password request not decoded")
	}
	if !issueHas(d, "crosses the network in the clear") {
		t.Error("a cleartext password on an unencrypted connection was not flagged")
	}
	if !infoHas(d, "PasswordMessage (cleartext)") {
		t.Error("the password message was not named for what it is")
	}
}

// ---------------------------------------------------------------- cancellation

func TestPGCancelRequest(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	extra := make([]byte, 8)
	binary.BigEndian.PutUint32(extra, 4242)      // backend pid
	binary.BigEndian.PutUint32(extra[4:], 99999) // secret
	pgSend(c, time.Millisecond, pgFirst(pgCancelRequest, extra))
	d := decodePG(t, b)
	if !infoHas(d, "CancelRequest for pid 4242") {
		t.Error("CancelRequest not decoded")
	}
	if !issueHas(d, "Query cancellation requested for backend pid 4242") {
		t.Error("CancelRequest not flagged")
	}
}

// ---------------------------------------------------------------- replication

func TestPGPhysicalReplication(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "repl", "database", "postgres", "replication", "true"))
	pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))
	pgSend(c, time.Millisecond, pgQuery("START_REPLICATION 0/3000000 TIMELINE 1"))
	pgRecv(c, time.Millisecond, pgMsg('W', []byte{0, 0, 0}))
	// The primary streams WAL; the standby reports how far it has got. 32 MB behind
	// is past the one-segment threshold, so it is worth a line.
	pgRecv(c, time.Millisecond, pgXLogData(0x3000000, 0x5100000, make([]byte, 200)))
	pgSend(c, time.Millisecond, pgStandbyStatus(0x3000100, 0x3000100, 0x3000100))
	pgRecv(c, time.Millisecond, pgKeepalive(0x5100000, 1))

	d := decodePG(t, b)
	for _, want := range []string{
		"replication=physical",
		"CopyBothResponse — physical replication stream begins",
		"XLogData: 200 B WAL at 0/3000000",
		"Standby status: write 0/3000100",
		"Primary keepalive: WAL end 0/5100000",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
	if !issueHas(d, "START_REPLICATION (physical)") {
		t.Error("START_REPLICATION was not flagged")
	}
	if !issueHas(d, "Replication lag") {
		t.Error("lag past a WAL segment was not flagged")
	}
	if !issueHas(d, "behind") && !infoHas(d, "behind") {
		t.Error("the standby's distance behind the primary was never stated")
	}
}

func TestPGLogicalReplication(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "repl", "database", "rental", "replication", "database"))
	pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))
	pgSend(c, time.Millisecond, pgQuery(`START_REPLICATION SLOT sub1 LOGICAL 0/0 (proto_version '1', publication_names 'pub1')`))
	pgRecv(c, time.Millisecond, pgMsg('W', []byte{0, 0, 0}))

	// pgoutput: Begin, Relation, Insert, Commit.
	begin := make([]byte, 21)
	begin[0] = 'B'
	binary.BigEndian.PutUint64(begin[1:], 0x1A2B3C4D)
	binary.BigEndian.PutUint32(begin[17:], 821)
	rel := []byte{'R', 0, 0, 0x40, 0x01}
	rel = append(rel, []byte("public\x00cars\x00")...)
	ins := []byte{'I', 0, 0, 0x40, 0x01, 'N'}
	commit := make([]byte, 26)
	commit[0] = 'C'
	binary.BigEndian.PutUint64(commit[9:], 0x1A2B3C99)

	for _, m := range [][]byte{begin, rel, ins, commit} {
		pgRecv(c, time.Millisecond, pgXLogData(0x1A2B0000, 0x1A2B3C99, m))
	}
	d := decodePG(t, b)
	for _, want := range []string{
		"replication=logical",
		"BEGIN xid 821",
		"relation public.cars",
		"INSERT on public.cars",
		"COMMIT at 0/1A2B3C99",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
}

// TestPGReplicationAnchorsMidStream is the case the live Patroni cluster exposed: a
// standby that attached before the capture began sends neither a startup message nor a
// Query, and receives no ReadyForQuery — ever. Without an anchor of its own the whole
// stream is undecodable, which is exactly what the first version reported.
func TestPGReplicationAnchorsMidStream(t *testing.T) {
	b := newPcap(pktLinkEther)
	// No SYN: the connection is older than the capture.
	c := &sampleConn{b: b, cseq: 7000, sseq: 9000}
	pgSend(c, time.Millisecond, pgStandbyStatus(0x25211CE8, 0x25211CE8, 0x25211B98))
	pgRecv(c, time.Millisecond, pgXLogData(0x25211CE8, 0x25211E10, make([]byte, 296)))
	pgSend(c, time.Millisecond, pgStandbyStatus(0x25211E10, 0x25211E10, 0x25211E10))

	d := decodePG(t, b)
	if !infoHas(d, "Standby status: write 0/25211CE8") {
		t.Error("a mid-stream standby status update was not decoded")
	}
	if !infoHas(d, "XLogData: 296 B WAL") {
		t.Error("a mid-stream XLogData was not decoded")
	}
	if infoHas(d, "joined mid-connection") {
		t.Error("a replication stream should anchor on its own sub-types, not stay unknown")
	}
}

// TestPGClientAnchorsOnReadyForQuery is the other half of that lesson, and the reason a
// capture of a busy server is readable at all: a client's next message after
// ReadyForQuery starts at a known boundary, even when the connection predates the
// capture and the message is a Bind full of binary parameters.
func TestPGClientAnchorsOnReadyForQuery(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := &sampleConn{b: b, cseq: 3000, sseq: 4000}
	// Mid-message client bytes, which cannot be decoded and must not be guessed at.
	pgSend(c, time.Millisecond, []byte{0x11, 0x22, 0x33, 0x44, 0x55})
	// The server completes a cycle: now both directions are aligned.
	pgRecv(c, time.Millisecond, pgReady('I'))
	pgSend(c, time.Millisecond, append(pgQuery("SELECT now()"), pgMsg('S', nil)...))
	pgRecv(c, time.Millisecond, append(pgCmdDone("SELECT 1"), pgReady('I')...))

	d := decodePG(t, b)
	if !infoHas(d, "joined mid-connection") {
		t.Error("the unreadable leading bytes should say so")
	}
	if !infoHas(d, "Query: SELECT now()") {
		t.Error("the client direction did not re-anchor after ReadyForQuery")
	}
}

// ---------------------------------------------------------------- probes, COPY, heavy

// TestPGBareConnectFlagged covers the shape of a TCP health check: a completed
// handshake, nothing said, then a close. There is no protocol to decode, which is
// exactly why it needs its own detection.
func TestPGBareConnectFlagged(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	c.b.frame(c.tick(10*time.Millisecond), pgC2S(c.cseq, c.sseq, tcpACK|tcpFIN, nil))
	d := decodePG(t, b)
	if !issueHas(d, "Connection opened and closed without sending anything") {
		t.Error("a bare connect/close was not recognised as a health check or probe")
	}
	if !issueHas(d, "incomplete startup packet") {
		t.Error("the finding should name what the server log calls it")
	}
}

func TestPGHeavyResultSet(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))
	pgSend(c, time.Millisecond, pgQuery("SELECT * FROM big"))
	pgRecv(c, time.Millisecond, pgRowDesc("payload"))
	row := strings.Repeat("x", 40000)
	for i := 0; i < 30; i++ {
		pgRecv(c, time.Millisecond, pgDataRow(row))
	}
	pgRecv(c, time.Millisecond, append(pgCmdDone("SELECT 30"), pgReady('I')...))
	d := decodePG(t, b)
	if !issueHas(d, "Heavy result set") {
		t.Error("a megabyte-plus result set was not flagged")
	}
	if !infoHas(d, "Result set complete: 30 row(s), 1 column(s)") {
		t.Error("the completed result set was not summarised")
	}
}

func TestPGCopyBoth(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))
	pgSend(c, time.Millisecond, pgQuery("COPY cars FROM STDIN"))
	pgRecv(c, time.Millisecond, pgMsg('G', []byte{0, 0, 0}))
	pgSend(c, time.Millisecond, pgCopyData([]byte("1,Corolla\n2,Civic\n")))
	pgSend(c, time.Millisecond, pgMsg('c', nil))
	pgRecv(c, time.Millisecond, append(pgCmdDone("COPY 2"), pgReady('I')...))
	d := decodePG(t, b)
	if !infoHas(d, "CopyInResponse") {
		t.Error("CopyInResponse not decoded")
	}
	if !infoHas(d, "CopyDone") {
		t.Error("CopyDone not decoded")
	}
	// COPY payload must not be read as a replication message just because it is
	// CopyData: the sub-type check is what keeps them apart.
	if infoHas(d, "XLogData") || infoHas(d, "Standby status") {
		t.Error("ordinary COPY data was decoded as replication")
	}
}

// TestPGIdleInTransaction covers the state PostgreSQL is uniquely punished by: a
// transaction that is open and doing nothing still holds its locks and its snapshot.
func TestPGIdleInTransaction(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))
	pgSend(c, time.Millisecond, pgQuery("BEGIN"))
	pgRecv(c, time.Millisecond, append(pgCmdDone("BEGIN"), pgReady('T')...))
	pgSend(c, 25*time.Second, pgQuery("COMMIT"))
	pgRecv(c, time.Millisecond, append(pgCmdDone("COMMIT"), pgReady('I')...))
	d := decodePG(t, b)
	if !issueHas(d, "Idle in transaction for 25 s") {
		t.Error("a transaction left open for 25 s was not flagged")
	}
}

// ---------------------------------------------------------------- engine detection

// TestPGSniffEngine covers the upload case: a pcap arrives with nobody to say what is
// in it, and the port may be anything.
func TestPGSniffEngine(t *testing.T) {
	// PostgreSQL on a non-standard port, decoded correctly with no engine given.
	b := newPcap(pktLinkEther)
	odd := 15432
	c2s := func(payload []byte) []byte {
		return ethIPv4TCP(cliIP, srvIP, cliPort, odd, 1000, 5000, tcpACK|tcpPSH, 64240, payload)
	}
	s2c := func(payload []byte) []byte {
		return ethIPv4TCP(srvIP, cliIP, odd, cliPort, 5000, 1000, tcpACK|tcpPSH, 64240, payload)
	}
	b.frame(0, ethIPv4TCP(cliIP, srvIP, cliPort, odd, 999, 0, tcpSYN, 64240, nil))
	b.frame(time.Millisecond, ethIPv4TCP(srvIP, cliIP, odd, cliPort, 4999, 1000, tcpSYN|tcpACK, 64240, nil))
	b.frame(2*time.Millisecond, c2s(pgStartup("user", "bob", "database", "rental")))
	b.frame(3*time.Millisecond, s2c(append(pgAuthOK(), pgReady('I')...)))

	if got := pktSniffEngine(b.buf, odd); got != pktEnginePostgres {
		t.Errorf("sniffed %q on port %d, want postgres", got, odd)
	}
	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: odd})
	if err != nil {
		t.Fatal(err)
	}
	if d.Engine != pktEnginePostgres || !infoHas(d, "StartupMessage") {
		t.Errorf("engine=%q, startup decoded=%v", d.Engine, infoHas(d, "StartupMessage"))
	}

	// A MySQL capture must still be read as MySQL — the sniffer cannot regress the
	// engine that was here first.
	mb := newPcap(pktLinkEther)
	mb.frame(0, c2sMySQL(1000, 0, tcpSYN, nil))
	mb.frame(time.Millisecond, s2cMySQL(1, 1001, tcpSYN|tcpACK, nil))
	mb.frame(2*time.Millisecond, s2cMySQL(2, 1001, tcpACK|tcpPSH, greeting("8.0.46-37")))
	if got := pktSniffEngine(mb.buf, srvPort); got != pktEngineMySQL {
		t.Errorf("sniffed %q for a MySQL capture, want mysql", got)
	}
	// And a capture with nothing recognisable falls back to the port.
	empty := newPcap(pktLinkEther)
	if got := pktSniffEngine(empty.buf, pgClientPort); got != pktEnginePostgres {
		t.Errorf("empty capture on 5432 sniffed as %q", got)
	}
	if got := pktSniffEngine(empty.buf, 3306); got != pktEngineMySQL {
		t.Errorf("empty capture on 3306 sniffed as %q", got)
	}
}

// c2sMySQL/s2cMySQL are the MySQL-port helpers under their own names, since this file's
// c2s/s2c would otherwise be ambiguous about which port they mean.
func c2sMySQL(seq, ack uint32, flags uint8, payload []byte) []byte {
	return c2s(seq, ack, flags, payload)
}
func s2cMySQL(seq, ack uint32, flags uint8, payload []byte) []byte {
	return s2c(seq, ack, flags, payload)
}

// ---------------------------------------------------------------- misframing

// TestPGGarbageIsNotDecoded is the rule this whole file exists to protect: bytes that
// cannot be a message must never produce a message.
func TestPGGarbageIsNotDecoded(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "bob", "database", "rental"))
	pgRecv(c, time.Millisecond, append(pgAuthOK(), pgReady('I')...))
	// A payload of random-looking bytes with impossible lengths.
	junk := []byte{0x51, 0xff, 0xff, 0xff, 0xff, 0x7a, 0x00, 0x01, 0x02, 0x03}
	pgSend(c, time.Millisecond, junk)
	d := decodePG(t, b)
	for _, p := range d.Packets {
		if strings.Contains(p.Info, "Query: ") && !strings.Contains(p.Info, "SELECT") {
			t.Errorf("junk decoded as a query: %q", p.Info)
		}
		if p.Rows > 1000000 {
			t.Errorf("junk produced an absurd row count: %d", p.Rows)
		}
	}
}
