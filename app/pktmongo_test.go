package main

// pktmongo_test.go — the MongoDB decoder, driven by hand-built captures.
//
// Same approach as the other two engines: real pcap bytes, real headers, real BSON. The
// builders below construct documents element by element, so a passing test means the
// decoder would read the same bytes off the wire.
//
// Four of these exist because the live replica set broke them:
//
//	TestMongoAnchorsRealHeader   an OP_MSG's document starts 21 bytes in, not 20 — the
//	                             section-kind byte. Off by that one byte, every genuine
//	                             header failed validation, so mid-stream connections
//	                             anchored on false positives and 2 009 frames read
//	                             "[framing lost]".
//	TestMongoSnappy              PSMDB negotiates snappy by default, so without it most
//	                             of a real capture is undecodable.
//	TestMongoOplogClassified     a getMore names its collection in a separate field, and
//	                             classifying before reading it made every oplog tail look
//	                             like an ordinary read.
//	TestMongoRepliesUseResponseTo mongos↔shard is 27017 to 27017, where no port rule can
//	                             say which end is the server. The header can.

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- BSON builders

// bdoc builds a BSON document from ordered elements.
func bdoc(elems ...[]byte) []byte {
	body := []byte{}
	for _, e := range elems {
		body = append(body, e...)
	}
	body = append(body, 0)
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(len(body)+4))
	return append(out, body...)
}

func bkey(t byte, key string) []byte {
	out := []byte{t}
	out = append(out, []byte(key)...)
	return append(out, 0)
}
func bStr(key, val string) []byte {
	out := bkey(bsonString, key)
	l := make([]byte, 4)
	binary.LittleEndian.PutUint32(l, uint32(len(val)+1))
	out = append(out, l...)
	out = append(out, []byte(val)...)
	return append(out, 0)
}
func bInt32(key string, v int32) []byte {
	out := bkey(bsonInt32, key)
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return append(out, b...)
}
func bInt64(key string, v int64) []byte {
	out := bkey(bsonInt64, key)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(v))
	return append(out, b...)
}
func bDouble(key string, v float64) []byte {
	out := bkey(bsonDouble, key)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, mathFloat64bits(v))
	return append(out, b...)
}
func bBool(key string, v bool) []byte {
	out := bkey(bsonBool, key)
	if v {
		return append(out, 1)
	}
	return append(out, 0)
}
func bSubDoc(key string, doc []byte) []byte {
	return append(bkey(bsonDoc, key), doc...)
}
func bArr(key string, items ...[]byte) []byte {
	return append(bkey(bsonArray, key), bdoc(items...)...)
}
func bTimestamp(key string, secs, inc uint32) []byte {
	out := bkey(bsonTimestmp, key)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, inc)
	binary.LittleEndian.PutUint32(b[4:], secs)
	return append(out, b...)
}
func bDateRaw(key string, ms int64) []byte {
	out := bkey(bsonDate, key)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(ms))
	return append(out, b...)
}

// mathFloat64bits avoids importing math into the test file for one call.
func mathFloat64bits(f float64) uint64 {
	var u uint64
	switch f {
	case 1:
		u = 0x3ff0000000000000
	case 0:
		u = 0
	default:
		// The tests only use 0 and 1 for doubles (ok: 1), so anything else is a mistake
		// worth failing loudly on rather than encoding wrongly.
		panic("mathFloat64bits: unexpected value in a test")
	}
	return u
}

// ---------------------------------------------------------------- wire builders

const mongoTestPort = 27017

func mgC2S(seq, ack uint32, flags uint8, payload []byte) []byte {
	return ethIPv4TCP(cliIP, srvIP, cliPort, mongoTestPort, seq, ack, flags, 64240, payload)
}
func mgS2C(seq, ack uint32, flags uint8, payload []byte) []byte {
	return ethIPv4TCP(srvIP, cliIP, mongoTestPort, cliPort, seq, ack, flags, 64240, payload)
}

// mongoMsg frames an OP_MSG carrying one command document, plus an optional kind-1
// document sequence.
func mongoMsg(requestID, responseTo int32, doc []byte, seqName string, seqDocs ...[]byte) []byte {
	body := []byte{0, 0, 0, 0} // flagBits
	body = append(body, 0)     // section kind 0
	body = append(body, doc...)
	if seqName != "" {
		seg := append([]byte(seqName), 0)
		for _, d := range seqDocs {
			seg = append(seg, d...)
		}
		sec := make([]byte, 4)
		binary.LittleEndian.PutUint32(sec, uint32(len(seg)+4))
		body = append(body, 1)
		body = append(body, sec...)
		body = append(body, seg...)
	}
	return mongoFrame(requestID, responseTo, mongoOpMsg, body)
}

// mongoFrame prepends the 16-byte header.
func mongoFrame(requestID, responseTo, op int32, body []byte) []byte {
	out := make([]byte, mongoHeaderLen)
	binary.LittleEndian.PutUint32(out, uint32(mongoHeaderLen+len(body)))
	binary.LittleEndian.PutUint32(out[4:], uint32(requestID))
	binary.LittleEndian.PutUint32(out[8:], uint32(responseTo))
	binary.LittleEndian.PutUint32(out[12:], uint32(op))
	return append(out, body...)
}

// mongoCompressedFrame wraps a message the way OP_COMPRESSED does.
func mongoCompressedFrame(requestID, responseTo, innerOp int32, compressor byte, inner, compressed []byte) []byte {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint32(body, uint32(innerOp))
	binary.LittleEndian.PutUint32(body[4:], uint32(len(inner)))
	body[8] = compressor
	body = append(body, compressed...)
	return mongoFrame(requestID, responseTo, mongoOpCompressed, body)
}

func decodeMongo(t *testing.T, b *pcapBuilder) *pktDecoded {
	t.Helper()
	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: mongoTestPort, Engine: pktEngineMongoDB})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d
}

// mgSession opens a captured connection with a SYN handshake.
func mgSession(b *pcapBuilder) *sampleConn {
	c := &sampleConn{b: b, cseq: 1000, sseq: 5000}
	c.b.frame(0, mgC2S(c.cseq, 0, tcpSYN, nil))
	c.cseq++
	c.b.frame(time.Millisecond, mgS2C(c.sseq, c.cseq, tcpSYN|tcpACK, nil))
	c.sseq++
	return c
}

// mgConn is a captured MongoDB connection with its OWN client port, for samples that
// need several at once. sampleConn's helpers all use one port, which silently collapsed
// four connections in the replica-set sample into a single stream — and a single stream
// keeps the classification of its first command, so heartbeats swallowed the oplog tail.
type mgConn struct {
	b          *pcapBuilder
	port       int
	cseq, sseq uint32
	at         time.Duration
}

func mgOpen(b *pcapBuilder, port int, at time.Duration) *mgConn {
	m := &mgConn{b: b, port: port, cseq: 1000, sseq: 5000, at: at}
	m.b.frame(m.at, ethIPv4TCP(cliIP, srvIP, port, mongoTestPort, m.cseq, 0, tcpSYN, 64240, nil))
	m.cseq++
	m.at += time.Millisecond
	m.b.frame(m.at, ethIPv4TCP(srvIP, cliIP, mongoTestPort, port, m.sseq, m.cseq, tcpSYN|tcpACK, 64240, nil))
	m.sseq++
	return m
}

func (m *mgConn) send(after time.Duration, payload []byte) {
	m.at += after
	m.b.frame(m.at, ethIPv4TCP(cliIP, srvIP, m.port, mongoTestPort, m.cseq, m.sseq, tcpACK|tcpPSH, 64240, payload))
	m.cseq += uint32(len(payload))
}

func (m *mgConn) recv(after time.Duration, payload []byte) {
	m.at += after
	m.b.frame(m.at, ethIPv4TCP(srvIP, cliIP, mongoTestPort, m.port, m.sseq, m.cseq, tcpACK|tcpPSH, 64240, payload))
	m.sseq += uint32(len(payload))
}

func mgSend(c *sampleConn, after time.Duration, payload []byte) {
	c.b.frame(c.tick(after), mgC2S(c.cseq, c.sseq, tcpACK|tcpPSH, payload))
	c.cseq += uint32(len(payload))
}
func mgRecv(c *sampleConn, after time.Duration, payload []byte) {
	c.b.frame(c.tick(after), mgS2C(c.sseq, c.cseq, tcpACK|tcpPSH, payload))
	c.sseq += uint32(len(payload))
}

// okReply is the common shape of a successful reply.
func okReply(responseTo int32, extra ...[]byte) []byte {
	elems := append([]([]byte){bDouble("ok", 1)}, extra...)
	return mongoMsg(responseTo+1000, responseTo, bdoc(elems...), "")
}

// errReply is a failed command: ok: 0 with a code, which is the ONLY signal MongoDB
// gives — there is nothing at the transport level to notice.
func errReply(responseTo int32, code int32, codeName, errmsg string) []byte {
	return mongoMsg(responseTo+1000, responseTo, bdoc(
		bDouble("ok", 0),
		bStr("errmsg", errmsg),
		bInt32("code", code),
		bStr("codeName", codeName),
	), "")
}

// ---------------------------------------------------------------- basics

func TestMongoSessionDecodes(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	// The handshake: hello with client metadata, then the server's view of itself.
	hello := bdoc(
		bInt32("hello", 1),
		bSubDoc("client", bdoc(
			bSubDoc("driver", bdoc(bStr("name", "nodejs"), bStr("version", "6.3.0"))),
			bSubDoc("application", bdoc(bStr("name", "hotelsim"))),
		)),
		bStr("$db", "admin"),
	)
	mgSend(c, time.Millisecond, mongoMsg(1, 0, hello, ""))
	mgRecv(c, time.Millisecond, okReply(1,
		bBool("isWritablePrimary", true), bStr("setName", "psmrs-00"),
		bStr("primary", "psmrs01.example.net:27017"),
	))
	// A find, and its cursor.
	find := bdoc(
		bStr("find", "bookings"),
		bSubDoc("filter", bdoc(bStr("status", "confirmed"))),
		bSubDoc("sort", bdoc(bInt32("createdAt", -1))),
		bInt32("limit", 20),
		bStr("$db", "hotelsim"),
	)
	mgSend(c, 2*time.Millisecond, mongoMsg(2, 0, find, ""))
	mgRecv(c, 3*time.Millisecond, mongoMsg(1002, 2, bdoc(
		bSubDoc("cursor", bdoc(
			bInt64("id", 0),
			bStr("ns", "hotelsim.bookings"),
			bArr("firstBatch", bSubDoc("0", bdoc(bStr("_id", "a"))), bSubDoc("1", bdoc(bStr("_id", "b")))),
		)),
		bDouble("ok", 1),
	), ""))
	// An insert with its documents in a kind-1 sequence, which is how a driver sends them.
	ins := bdoc(bStr("insert", "bookings"), bStr("$db", "hotelsim"))
	mgSend(c, time.Millisecond, mongoMsg(3, 0, ins, "documents",
		bdoc(bStr("_id", "c")), bdoc(bStr("_id", "d")), bdoc(bStr("_id", "e"))))
	mgRecv(c, time.Millisecond, okReply(3, bInt32("n", 3)))

	d := decodeMongo(t, b)
	for _, want := range []string{
		"hello admin — driver nodejs 6.3.0, app hotelsim",
		"PRIMARY, set psmrs-00",
		`find hotelsim.bookings — filter {status: "confirmed"}, sort {createdAt: -1}, limit 20`,
		"2 doc(s) in firstBatch, cursor exhausted",
		`insert hotelsim.bookings — 3 document(s) in a "documents" sequence`,
		"n=3",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
	if len(d.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(d.Streams))
	}
	st := d.Streams[0]
	// hello/insert/find: only the last two count as work.
	if st.Queries != 2 {
		t.Errorf("queries = %d, want 2 (hello is chatter, not work)", st.Queries)
	}
	if st.User != "hotelsim" {
		t.Errorf("stream user = %q, want the application name from the handshake", st.User)
	}
	if d.Engine != pktEngineMongoDB {
		t.Errorf("engine = %q", d.Engine)
	}
}

// TestMongoAnchorsRealHeader is the off-by-one that cost 2 009 frames on the live replica
// set: an OP_MSG's document begins after the header, the flag word AND the section-kind
// byte. A validator reading the length one byte early rejects every genuine header.
func TestMongoAnchorsRealHeader(t *testing.T) {
	msg := mongoMsg(0x32e9b, 0x92c8, bdoc(bInt32("n", 1), bStr("codeName", "")), "")
	// The exact shape from the capture: a 230-byte reply, mid-stream, no SYN.
	b := newPcap(pktLinkEther)
	c := &sampleConn{b: b, cseq: 7000, sseq: 9000}
	mgRecv(c, time.Millisecond, msg)
	mgRecv(c, time.Millisecond, msg)
	d := decodeMongo(t, b)
	for _, p := range d.Packets {
		if strings.Contains(p.Info, "framing lost") || strings.Contains(p.Info, "joined mid-connection") {
			t.Errorf("#%d did not anchor on a valid header: %q", p.No, p.Info)
		}
	}
	if !infoHas(d, "reply →") {
		t.Error("a mid-stream reply was not decoded")
	}
	// And the anchor itself, directly: a valid header must be found at offset 0.
	dir := &pktDirState{buf: append([]byte{}, msg...)}
	if !pktMongoAnchor(dir, mongoMidStreamMax) || len(dir.buf) != len(msg) {
		t.Errorf("anchor moved past a valid header at offset 0 (dropped %d bytes)", len(msg)-len(dir.buf))
	}
}

// TestMongoRepliesUseResponseTo covers the mongos case: 27017 talking to 27017, where the
// ports cannot say which end is the server, so the header's responseTo has to.
func TestMongoRepliesUseResponseTo(t *testing.T) {
	b := newPcap(pktLinkEther)
	// Both ends on 27017 — a router forwarding to a shard.
	cseq, sseq := uint32(1000), uint32(5000)
	c2s := func(payload []byte) []byte {
		return ethIPv4TCP(cliIP, srvIP, mongoTestPort, mongoTestPort, cseq, sseq, tcpACK|tcpPSH, 64240, payload)
	}
	s2c := func(payload []byte) []byte {
		return ethIPv4TCP(srvIP, cliIP, mongoTestPort, mongoTestPort, sseq, cseq, tcpACK|tcpPSH, 64240, payload)
	}
	b.frame(0, ethIPv4TCP(cliIP, srvIP, mongoTestPort, mongoTestPort, cseq, 0, tcpSYN, 64240, nil))
	cseq++
	b.frame(time.Millisecond, ethIPv4TCP(srvIP, cliIP, mongoTestPort, mongoTestPort, sseq, cseq, tcpSYN|tcpACK, 64240, nil))
	sseq++
	req := mongoMsg(77, 0, bdoc(
		bStr("find", "orders"),
		bSubDoc("filter", bdoc(bInt32("sk", 42))),
		bSubDoc("shardVersion", bdoc(bTimestamp("t", 1785830373, 4))),
		bStr("$db", "shlab"),
	), "")
	b.frame(2*time.Millisecond, c2s(req))
	cseq += uint32(len(req))
	rep := mongoMsg(999, 77, bdoc(
		bSubDoc("cursor", bdoc(bInt64("id", 0), bStr("ns", "shlab.orders"),
			bArr("firstBatch", bSubDoc("0", bdoc(bInt32("sk", 42)))))),
		bDouble("ok", 1)), "")
	b.frame(3*time.Millisecond, s2c(rep))

	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: mongoTestPort, Engine: pktEngineMongoDB})
	if err != nil {
		t.Fatal(err)
	}
	if !infoHas(d, "find shlab.orders") {
		t.Error("the routed request was not decoded as a command")
	}
	if !infoHas(d, "shardVersion") {
		t.Error("the shard version was not reported")
	}
	if !infoHas(d, "find → 1 doc(s) in firstBatch") {
		t.Error("the reply was not matched to its request — responseTo was not used")
	}
	// The connection is a routed one, and the label says so.
	if d.Streams[0].RoleLabel != "MongoDB/routed" {
		t.Errorf("role label = %q, want MongoDB/routed", d.Streams[0].RoleLabel)
	}
}

// ---------------------------------------------------------------- errors

func TestMongoErrors(t *testing.T) {
	cases := []struct {
		name      string
		code      int32
		codeName  string
		errmsg    string
		wantIssue string
		wantNone  bool
	}{
		{name: "not writable primary", code: 10107, codeName: "NotWritablePrimary",
			errmsg: "not primary", wantIssue: "NotWritablePrimary (10107)"},
		{name: "auth failed", code: 18, codeName: "AuthenticationFailed",
			errmsg: "Authentication failed.", wantIssue: "AuthenticationFailed (18)"},
		{name: "unauthorized", code: 13, codeName: "Unauthorized",
			errmsg: "not authorized on pktlab to execute command", wantIssue: "Unauthorized (13)"},
		{name: "stale config", code: 13388, codeName: "StaleConfig",
			errmsg: "version mismatch detected for shlab.orders", wantIssue: "StaleConfig (13388)"},
		{name: "write conflict", code: 112, codeName: "WriteConflict",
			errmsg: "WriteConflict error", wantIssue: "WriteConflict (112)"},
		{name: "no such transaction", code: 251, codeName: "NoSuchTransaction",
			errmsg: "Transaction 5 has been aborted", wantIssue: "NoSuchTransaction (251)"},
		{name: "cursor not found", code: 43, codeName: "CursorNotFound",
			errmsg: "cursor id 12345 not found", wantIssue: "CursorNotFound (43)"},
		{name: "max time expired", code: 50, codeName: "MaxTimeMSExpired",
			errmsg: "operation exceeded time limit", wantIssue: "MaxTimeMSExpired (50)"},
		{name: "write concern failed", code: 64, codeName: "WriteConcernFailed",
			errmsg: "waiting for replication timed out", wantIssue: "WriteConcernFailed (64)"},
		{name: "shutdown in progress", code: 91, codeName: "ShutdownInProgress",
			errmsg: "The server is in quiesce mode and will shut down", wantIssue: "ShutdownInProgress (91)"},
		// Driver discovery is not a finding: every driver probes for optional commands,
		// and 21 of these turned up in one two-minute capture of an idle replica set.
		{name: "command not found", code: 59, codeName: "CommandNotFound",
			errmsg: "no such command: 'atlasVersion'", wantNone: true},
		{name: "invalid options", code: 72, codeName: "InvalidOptions",
			errmsg: "no option found to get", wantNone: true},
		{name: "namespace not found", code: 26, codeName: "NamespaceNotFound",
			errmsg: "ns not found", wantNone: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newPcap(pktLinkEther)
			c := mgSession(b)
			mgSend(c, time.Millisecond, mongoMsg(5, 0, bdoc(
				bStr("find", "items"), bStr("$db", "pktlab")), ""))
			mgRecv(c, 2*time.Millisecond, errReply(5, tc.code, tc.codeName, tc.errmsg))
			d := decodeMongo(t, b)
			if !infoHas(d, tc.codeName) {
				t.Errorf("the reply does not name %s", tc.codeName)
			}
			if tc.wantIssue != "" && !issueHas(d, tc.wantIssue) {
				t.Errorf("issue %q not raised", tc.wantIssue)
			}
			if tc.wantNone {
				for _, p := range d.Packets {
					for _, i := range p.Issues {
						if !strings.HasPrefix(i, "TCP") {
							t.Errorf("driver discovery raised a finding: %q", i)
						}
					}
				}
			}
			// The error code lands in ErrCode, and the stream counts it.
			for _, p := range d.Packets {
				if strings.Contains(p.Info, tc.codeName) && p.ErrCode != int(tc.code) {
					t.Errorf("ErrCode = %d, want %d", p.ErrCode, tc.code)
				}
			}
			if d.Streams[0].Errors != 1 {
				t.Errorf("stream errors = %d, want 1", d.Streams[0].Errors)
			}
		})
	}
}

// TestMongoWriteErrorsInsideOKReply is the case a tool watching command status cannot see:
// the command succeeded, and individual documents did not.
func TestMongoWriteErrorsInsideOKReply(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	mgSend(c, time.Millisecond, mongoMsg(9, 0, bdoc(
		bStr("insert", "items"), bStr("$db", "pktlab")), "documents", bdoc(bInt32("_id", 1))))
	mgRecv(c, 2*time.Millisecond, mongoMsg(1009, 9, bdoc(
		bDouble("ok", 1),
		bInt32("n", 0),
		bArr("writeErrors", bSubDoc("0", bdoc(
			bInt32("index", 0),
			bInt32("code", 11000),
			bStr("errmsg", `E11000 duplicate key error collection: pktlab.items index: _id_ dup key: { _id: 1 }`),
		))),
	), ""))
	d := decodeMongo(t, b)
	if !infoHas(d, "1 write error(s), first 11000 DuplicateKey") {
		t.Error("a write error inside a successful reply was not surfaced")
	}
	if !issueHas(d, "DuplicateKey (11000)") {
		t.Error("the duplicate key was not flagged with its code")
	}
	if d.Streams[0].Errors != 1 {
		t.Error("a write error was not counted against the stream")
	}
}

// TestMongoWriteConcernError: the write happened, but not durably.
func TestMongoWriteConcernError(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	mgSend(c, time.Millisecond, mongoMsg(11, 0, bdoc(
		bStr("update", "items"), bStr("$db", "pktlab")), ""))
	mgRecv(c, 2*time.Millisecond, mongoMsg(1011, 11, bdoc(
		bDouble("ok", 1), bInt32("n", 1),
		bSubDoc("writeConcernError", bdoc(
			bInt32("code", 64), bStr("codeName", "WriteConcernFailed"),
			bStr("errmsg", "waiting for replication timed out"))),
	), ""))
	d := decodeMongo(t, b)
	if !issueHas(d, "Write concern not satisfied") {
		t.Error("a write-concern failure was not flagged")
	}
	if !issueHas(d, "may still be rolled back") {
		t.Error("the consequence of a write-concern failure was not stated")
	}
}

// ---------------------------------------------------------------- classification

// TestMongoConnectionKinds is the heart of MongoDB support: one port carries six kinds of
// conversation, and a capture that cannot tell them apart is mostly heartbeats.
func TestMongoConnectionKinds(t *testing.T) {
	cases := []struct {
		name  string
		doc   []byte
		label string
	}{
		{"heartbeat", bdoc(bInt32("replSetHeartbeat", 1), bStr("$db", "admin")), "MongoDB/heartbeat"},
		{"replpos", bdoc(bInt32("replSetUpdatePosition", 1), bStr("$db", "admin")), "MongoDB/replpos"},
		{"election", bdoc(bInt32("replSetRequestVotes", 1), bStr("$db", "admin")), "MongoDB/election"},
		{"config", bdoc(bStr("find", "chunks"), bStr("$db", "config")), "MongoDB/config"},
		{"client", bdoc(bStr("find", "bookings"), bStr("$db", "hotelsim")), "MongoDB"},
		{"routed", bdoc(bStr("find", "orders"),
			bSubDoc("shardVersion", bdoc(bTimestamp("t", 1785830373, 4))), bStr("$db", "shlab")), "MongoDB/routed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newPcap(pktLinkEther)
			c := mgSession(b)
			mgSend(c, time.Millisecond, mongoMsg(1, 0, tc.doc, ""))
			mgRecv(c, time.Millisecond, okReply(1))
			d := decodeMongo(t, b)
			if got := d.Streams[0].RoleLabel; got != tc.label {
				t.Errorf("connection labelled %q, want %q", got, tc.label)
			}
			found := false
			for _, p := range d.Packets {
				if p.Proto == tc.label {
					found = true
				}
			}
			if !found {
				t.Errorf("no packet carries the protocol label %q", tc.label)
			}
		})
	}
}

// TestMongoOplogClassified covers the ordering bug: a getMore names its collection in a
// separate field, and classifying before reading it made every oplog tail — the single
// most important connection in a replica-set capture — look like an ordinary read.
func TestMongoOplogClassified(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	// The find that opens the tail, then the getMores that are all a capture usually sees.
	mgSend(c, time.Millisecond, mongoMsg(1, 0, bdoc(
		bStr("find", "oplog.rs"),
		bSubDoc("filter", bdoc(bSubDoc("ts", bdoc(bTimestamp("$gte", 1785830000, 1))))),
		bBool("tailable", true), bBool("awaitData", true),
		bStr("$db", "local"),
	), ""))
	mgRecv(c, time.Millisecond, mongoMsg(1001, 1, bdoc(
		bSubDoc("cursor", bdoc(bInt64("id", 7648922318530284142), bStr("ns", "local.oplog.rs"),
			bArr("firstBatch", bSubDoc("0", bdoc(bTimestamp("ts", 1785830001, 1)))))),
		bDouble("ok", 1)), ""))
	mgSend(c, 10*time.Millisecond, mongoMsg(2, 0, bdoc(
		bInt64("getMore", 7648922318530284142),
		bStr("collection", "oplog.rs"),
		bInt32("maxTimeMS", 5000),
		bStr("$db", "local"),
	), ""))
	mgRecv(c, 20*time.Millisecond, mongoMsg(1002, 2, bdoc(
		bSubDoc("cursor", bdoc(bInt64("id", 7648922318530284142), bStr("ns", "local.oplog.rs"),
			bArr("nextBatch", bSubDoc("0", bdoc(bTimestamp("ts", 1785830002, 1)))))),
		bDouble("ok", 1)), ""))

	d := decodeMongo(t, b)
	if got := d.Streams[0].RoleLabel; got != "MongoDB/oplog" {
		t.Errorf("connection labelled %q, want MongoDB/oplog", got)
	}
	if !infoHas(d, "find local.oplog.rs") || !infoHas(d, "tailable") {
		t.Error("the oplog find was not decoded")
	}
	if !infoHas(d, "getMore local.oplog.rs") {
		t.Error("the getMore did not resolve its namespace from the collection field")
	}
	// A tailing getMore blocks on purpose: it must never be called slow.
	for _, p := range d.Packets {
		for _, i := range p.Issues {
			if strings.Contains(i, "Slow response") {
				t.Errorf("an awaitData getMore was flagged as slow: %q", i)
			}
		}
	}
}

// TestMongoElectionFlagged: the seconds in which the primary changes.
func TestMongoElectionFlagged(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	mgSend(c, time.Millisecond, mongoMsg(1, 0, bdoc(
		bInt32("replSetStepDown", 20), bStr("$db", "admin")), ""))
	mgRecv(c, time.Millisecond, okReply(1))
	mgSend(c, time.Millisecond, mongoMsg(2, 0, bdoc(
		bInt32("replSetRequestVotes", 1), bStr("$db", "admin")), ""))
	mgRecv(c, time.Millisecond, okReply(2))
	d := decodeMongo(t, b)
	if !issueHas(d, "replSetStepDown") {
		t.Error("a step-down was not flagged")
	}
	if !issueHas(d, "Election in progress") {
		t.Error("an election was not flagged")
	}
	if !issueHas(d, "NotWritablePrimary") {
		t.Error("the consequence for the application was not stated")
	}
}

// ---------------------------------------------------------------- compression

// TestMongoSnappy is why pktsnappy.go exists: PSMDB negotiates snappy by default, so
// without it most of a real MongoDB capture is undecodable.
func TestMongoSnappy(t *testing.T) {
	inner := mongoMsg(4, 0, bdoc(
		bStr("find", "bookings"), bStr("$db", "hotelsim")), "")
	// A snappy block of only literals is valid snappy, which is enough to prove the
	// wrapper is unwrapped and the inner message decoded.
	comp := snappyLiterals(inner[mongoHeaderLen:])
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	mgSend(c, time.Millisecond, mongoCompressedFrame(4, 0, mongoOpMsg, 1, inner[mongoHeaderLen:], comp))
	mgRecv(c, time.Millisecond, okReply(4))
	d := decodeMongo(t, b)
	if !infoHas(d, "[snappy] find hotelsim.bookings") {
		t.Errorf("a snappy-compressed command was not decoded: %q", d.Packets[2].Info)
	}
	// zstd stays honest rather than guessed at.
	b2 := newPcap(pktLinkEther)
	c2 := mgSession(b2)
	mgSend(c2, time.Millisecond, mongoCompressedFrame(5, 0, mongoOpMsg, 3,
		inner[mongoHeaderLen:], []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x01, 0x02}))
	d2 := decodeMongo(t, b2)
	if !infoHas(d2, "compressed with zstd") || !infoHas(d2, "not decoded") {
		t.Error("zstd should be named and left alone, not guessed at")
	}
}

// snappyLiterals encodes bytes as a snappy block using only literal tags — the simplest
// valid encoding, and enough to exercise the decoder's framing.
func snappyLiterals(src []byte) []byte {
	out := []byte{}
	// varint length
	n := len(src)
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	out = append(out, byte(n))
	for len(src) > 0 {
		chunk := src
		if len(chunk) > 60 {
			chunk = chunk[:60]
		}
		out = append(out, byte(len(chunk)-1)<<2) // literal, length-1 in the tag
		out = append(out, chunk...)
		src = src[len(chunk):]
	}
	return out
}

func TestSnappyDecode(t *testing.T) {
	// Literals only.
	want := []byte("the quick brown fox jumps over the lazy dog")
	got, err := snappyDecode(snappyLiterals(want), len(want))
	if err != nil || string(got) != string(want) {
		t.Fatalf("literal round trip: %q, %v", got, err)
	}
	// A back-reference: "abcabcabc" as a literal "abc" plus a copy of 6 at offset 3,
	// which is how snappy encodes a repeat and where an overlapping copy has to work.
	// The copy tag encodes (length-4) in bits 2-4, so 6 bytes is (2<<2)|0x01 = 0x09.
	block := []byte{9, 0x08, 'a', 'b', 'c', 0x09, 0x03}
	got, err = snappyDecode(block, 9)
	if err != nil || string(got) != "abcabcabc" {
		t.Fatalf("overlapping copy: %q, %v", got, err)
	}
	// Corrupt input must be refused, not guessed at.
	for _, bad := range [][]byte{
		{},                         // nothing
		{9, 0x08, 'a'},             // literal runs past the block
		{9, 0x15, 0x03},            // a copy with nothing decoded yet
		{200, 0x08, 'a', 'b', 'c'}, // declared length nothing like the content
	} {
		if _, err := snappyDecode(bad, -1); err == nil {
			t.Errorf("corrupt block accepted: %v", bad)
		}
	}
	// A length disagreement between the block and the message header is corruption.
	if _, err := snappyDecode(snappyLiterals(want), len(want)+1); err == nil {
		t.Error("a size mismatch between the wrapper and the block was accepted")
	}
}

// ---------------------------------------------------------------- BSON

func TestBSONReading(t *testing.T) {
	doc := bdoc(
		bStr("find", "items"),
		bInt32("limit", 20),
		bInt64("cursorId", 7648922318530284142),
		bBool("tailable", true),
		bDouble("ok", 1),
		bTimestamp("ts", 1785830373, 5),
		bDateRaw("when", 1785830373000),
		// A Date holding an OpTime's raw bits, which is what a heartbeat reply carries
		// and what produced "243057045-12-23T22:12:28.801Z" on live traffic.
		bDateRaw("electionTime", int64(1785830373)<<32|5),
		bSubDoc("filter", bdoc(bStr("status", "confirmed"))),
		bArr("pipeline", bSubDoc("0", bdoc(bInt32("$match", 1))), bSubDoc("1", bdoc(bInt32("$group", 1)))),
	)
	elems, ok := bsonElems(doc)
	if !ok {
		t.Fatal("a well-formed document did not parse")
	}
	if elems[0].Key != "find" {
		t.Errorf("first key = %q — the command name depends on element order", elems[0].Key)
	}
	if got := bsonStr(mustGet(elems, "find")); got != "items" {
		t.Errorf("string = %q", got)
	}
	if v, _ := bsonInt(mustGet(elems, "limit")); v != 20 {
		t.Errorf("int32 = %d", v)
	}
	if v, _ := bsonInt(mustGet(elems, "cursorId")); v != 7648922318530284142 {
		t.Errorf("int64 = %d", v)
	}
	for _, tc := range []struct{ key, want string }{
		{"tailable", "true"},
		{"ts", "Timestamp(1785830373, 5)"},
		{"when", "2026-08-04T07:59:33.000Z"},
		{"electionTime", "Timestamp(1785830373, 5) [in a Date-typed field]"},
		{"filter", `{status: "confirmed"}`},
	} {
		got := bsonValue(mustGet(elems, tc.key), 1)
		if !strings.HasPrefix(got, tc.want) && got != tc.want {
			t.Errorf("%s rendered as %q, want %q", tc.key, got, tc.want)
		}
	}
	// Nesting is bounded so a deep pipeline cannot produce a wall of text.
	if got := bsonValue(mustGet(elems, "pipeline"), 0); !strings.Contains(got, "…2 items") {
		t.Errorf("array at depth 0 rendered as %q", got)
	}
	// A truncated document yields what was readable and says it was truncated.
	if _, ok := bsonElems(doc[:len(doc)/2]); ok {
		t.Error("a truncated document reported itself as complete")
	}
	// Rubbish must never parse.
	for _, bad := range [][]byte{
		{}, {1, 2, 3},
		{0xff, 0xff, 0xff, 0x7f, 0x00},    // absurd length
		{0x06, 0x00, 0x00, 0x00, 0x99, 0}, // unknown element type
	} {
		if elems, ok := bsonElems(bad); ok && len(elems) > 0 {
			t.Errorf("rubbish parsed as %d elements: %v", len(elems), bad)
		}
	}
	// bsonSummary skips the fields every message carries.
	sum := bsonSummary(elems, 4)
	if strings.Contains(sum, "$db") || strings.Contains(sum, "lsid") {
		t.Errorf("summary includes per-message noise: %q", sum)
	}
}

// ---------------------------------------------------------------- probes, legacy, junk

func TestMongoBareConnectFlagged(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	c.b.frame(c.tick(10*time.Millisecond), mgC2S(c.cseq, c.sseq, tcpACK|tcpFIN, nil))
	d := decodeMongo(t, b)
	if !issueHas(d, "Connection opened and closed without sending anything") {
		t.Error("a bare connect/close was not recognised as a health check or probe")
	}
}

func TestMongoLegacyOpcodes(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	// OP_QUERY on $cmd is the legacy handshake, which is normal and must not be flagged.
	ns := append([]byte("admin.$cmd"), 0)
	body := append([]byte{0, 0, 0, 0}, ns...)
	body = append(body, make([]byte, 8)...) // skip, limit
	body = append(body, bdoc(bInt32("isMaster", 1))...)
	mgSend(c, time.Millisecond, mongoFrame(1, 0, mongoOpQuery, body))
	mgRecv(c, time.Millisecond, mongoFrame(1001, 1, mongoOpReply, func() []byte {
		out := make([]byte, 20)
		binary.LittleEndian.PutUint32(out[16:], 1) // numberReturned
		return append(out, bdoc(bBool("ismaster", true), bDouble("ok", 1))...)
	}()))
	// A legacy INSERT is a finding: it was removed from the server in 5.1.
	mgSend(c, time.Millisecond, mongoFrame(2, 0, mongoOpInsert,
		append(append([]byte{0, 0, 0, 0}, append([]byte("pktlab.items"), 0)...), bdoc(bInt32("_id", 1))...)))

	d := decodeMongo(t, b)
	if !infoHas(d, "OP_QUERY admin.$cmd") {
		t.Error("the legacy handshake was not decoded")
	}
	if !infoHas(d, "OP_REPLY 1 doc(s)") {
		t.Error("OP_REPLY was not decoded")
	}
	if !issueHas(d, "OP_INSERT — a legacy opcode removed in MongoDB 5.1") {
		t.Error("a removed opcode was not flagged")
	}
	// The handshake itself must not be flagged as legacy usage.
	for _, p := range d.Packets {
		for _, i := range p.Issues {
			if strings.Contains(i, "legacy read path") {
				t.Errorf("the handshake was flagged as a legacy query: %q", i)
			}
		}
	}
}

// TestMongoGarbageIsNotDecoded is the rule the whole file protects: bytes that cannot be
// a message must not produce one — and MongoDB is the easiest engine to fool, because
// "small int32 then 2013" appears by chance in enough binary data.
func TestMongoGarbageIsNotDecoded(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	junk := make([]byte, 512)
	for i := range junk {
		junk[i] = byte(i * 7 % 251)
	}
	mgSend(c, time.Millisecond, junk)
	d := decodeMongo(t, b)
	for _, p := range d.Packets {
		if p.Rows > 1<<20 {
			t.Errorf("junk produced an absurd document count: %d", p.Rows)
		}
		if strings.Contains(p.Info, "doc(s) in firstBatch") {
			t.Errorf("junk decoded as a cursor reply: %q", p.Info)
		}
	}
}

// TestMongoSniffEngine covers the upload path: a capture arrives with nobody to say what
// is in it.
func TestMongoSniffEngine(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := mgSession(b)
	mgSend(c, time.Millisecond, mongoMsg(1, 0, bdoc(
		bStr("find", "items"), bStr("$db", "pktlab")), ""))
	mgRecv(c, time.Millisecond, okReply(1))
	if got := pktSniffEngine(b.buf, mongoTestPort); got != pktEngineMongoDB {
		t.Errorf("sniffed %q, want mongodb", got)
	}
	// On a non-standard port, the payload still decides.
	odd := 37017
	b2 := newPcap(pktLinkEther)
	msg := mongoMsg(1, 0, bdoc(bStr("find", "items"), bStr("$db", "pktlab")), "")
	b2.frame(0, ethIPv4TCP(cliIP, srvIP, cliPort, odd, 999, 0, tcpSYN, 64240, nil))
	b2.frame(time.Millisecond, ethIPv4TCP(srvIP, cliIP, odd, cliPort, 4999, 1000, tcpSYN|tcpACK, 64240, nil))
	b2.frame(2*time.Millisecond, ethIPv4TCP(cliIP, srvIP, cliPort, odd, 1000, 5000, tcpACK|tcpPSH, 64240, msg))
	if got := pktSniffEngine(b2.buf, odd); got != pktEngineMongoDB {
		t.Errorf("sniffed %q on port %d, want mongodb", got, odd)
	}
	// An empty capture on 27017 falls back to the port.
	if got := pktSniffEngine(newPcap(pktLinkEther).buf, mongoClientPort); got != pktEngineMongoDB {
		t.Errorf("empty capture on 27017 sniffed as %q", got)
	}
	// And the other two engines must not regress.
	if got := pktEngineForPort(3306); got != pktEngineMySQL {
		t.Errorf("3306 → %q", got)
	}
	if got := pktEngineForPort(pgClientPort); got != pktEnginePostgres {
		t.Errorf("5432 → %q", got)
	}
}

// ---------------------------------------------------------------- the server log

func TestMongoLogClassification(t *testing.T) {
	log := strings.Join([]string{
		`{"t":{"$date":"2026-08-04T07:38:57.037+00:00"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted","attr":{"remote":"172.30.0.11:49636","connectionId":65}}`,
		`{"t":{"$date":"2026-08-04T07:38:58.000+00:00"},"s":"I","c":"NETWORK","id":22944,"ctx":"conn65","msg":"Connection ended","attr":{"remote":"172.30.0.11:49636"}}`,
		`{"t":{"$date":"2026-08-04T07:39:06.958+00:00"},"s":"I","c":"WRITE","id":51803,"ctx":"conn73","msg":"Slow query","attr":{"type":"update","ns":"hotelsim.dailyInventory","planSummary":"IXSCAN { date: 1 }","keysExamined":5600,"docsExamined":5600,"nModified":5600,"numYields":12,"durationMillis":151}}`,
		`{"t":{"$date":"2026-08-04T07:40:00.000+00:00"},"s":"I","c":"ELECTION","id":20698,"ctx":"conn1","msg":"Election succeeded, assuming primary role","attr":{"term":2}}`,
		`{"t":{"$date":"2026-08-04T07:40:01.000+00:00"},"s":"I","c":"REPL","id":21358,"ctx":"conn1","msg":"Replica set state transition","attr":{"newState":"PRIMARY","oldState":"SECONDARY"}}`,
		`{"t":{"$date":"2026-08-04T07:40:02.000+00:00"},"s":"I","c":"ACCESS","id":20883,"ctx":"conn9","msg":"Authentication failed","attr":{"user":"admin","error":"AuthenticationFailed: mechanism SCRAM-SHA-256"}}`,
		`{"t":{"$date":"2026-08-04T07:40:03.000+00:00"},"s":"W","c":"REPL","id":9999999,"ctx":"conn2","msg":"Heartbeat failed after 2 retries","attr":{"error":"HostUnreachable"}}`,
		`{"t":{"$date":"2026-08-04T07:40:04.000+00:00"},"s":"E","c":"STORAGE","id":9999998,"ctx":"conn3","msg":"Something the catalogue has never heard of"}`,
		`not json at all`,
		`{"t":{"$date":"2026-08-04T07:40:05.000+00:00"},"s":"I","c":"CONTROL","id":23016,"ctx":"listener","msg":"Waiting for connections","attr":{"port":27017}}`,
	}, "\n")

	entries := pktParseServerLog([]byte(log), pktEngineMongoDB)
	if len(entries) != 9 {
		t.Fatalf("got %d entries, want 9 (one line is not JSON)", len(entries))
	}
	byLabel := map[string]pktLogEntry{}
	for _, e := range entries {
		byLabel[e.Label] = e
		if e.TS == 0 {
			t.Errorf("%q has no parsed timestamp", e.Label)
		}
	}
	for label, class := range map[string]pktLogClass{
		"Connection accepted": pktLogOther,
		"Connection ended":    pktLogAbort,
		"Slow query":          pktLogOther,
		"Election succeeded — this member is now primary":      pktLogCluster,
		"Replica set state transition":                         pktLogRepl,
		"Authentication failed":                                pktLogAuth,
		"Heartbeat failed — this is what precedes an election": pktLogRepl,
		"Waiting for connections":                              pktLogLifecycle,
	} {
		e, ok := byLabel[label]
		if !ok {
			t.Errorf("no record labelled %q", label)
			continue
		}
		if e.Class != class {
			t.Errorf("%q classified %q, want %q", label, e.Class, class)
		}
	}
	// The attributes a capture cannot show are what make the correlation worth having.
	slow := byLabel["Slow query"]
	for _, want := range []string{"planSummary=IXSCAN", "docsExamined=5600", "durationMillis=151"} {
		if !strings.Contains(slow.Message, want) {
			t.Errorf("the slow-query record lost %q: %q", want, slow.Message)
		}
	}
	// An unrecognised id with an error severity is still surfaced rather than buried.
	if e, ok := byLabel["STORAGE"]; !ok || e.Class != pktLogAbort {
		t.Errorf("an ERROR-severity record with an unknown id was not surfaced: %+v", e)
	}
	// Severities are spelled out.
	if byLabel["Connection accepted"].Level != "INFO" {
		t.Errorf("severity not expanded: %q", byLabel["Connection accepted"].Level)
	}
}

func TestMongoLogEngineSniff(t *testing.T) {
	mg := `{"t":{"$date":"2026-08-04T07:38:57.037+00:00"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted"}` + "\n"
	if got := pktSniffLogEngine([]byte(strings.Repeat(mg, 6))); got != pktEngineMongoDB {
		t.Errorf("a MongoDB log sniffed as %q", got)
	}
	// And the other two must not regress.
	pg := "2026-08-04 06:02:11.032 UTC [2596] LOG:  database system is ready to accept connections\n"
	if got := pktSniffLogEngine([]byte(strings.Repeat(pg, 6))); got != pktEnginePostgres {
		t.Errorf("a PostgreSQL log sniffed as %q", got)
	}
	my := "2026-08-03T19:19:01.501234Z 12 [Note] [MY-010914] [Server] Aborted connection 12 to db: 'x' user: 'y' host: 'h' (Got timeout reading communication packets).\n"
	if got := pktSniffLogEngine([]byte(strings.Repeat(my, 6))); got != pktEngineMySQL {
		t.Errorf("a MySQL log sniffed as %q", got)
	}
	entries := pktParseServerLog([]byte(strings.Repeat(mg, 3)), "")
	if len(entries) != 3 {
		t.Errorf("auto-detected parse gave %d entries", len(entries))
	}
}

func TestMongoAbortStatsSkipped(t *testing.T) {
	var a App
	st := a.pktAbortStatsFor(nil, "container", pktEngineMongoDB, "u", "p")
	if !strings.Contains(st.Hint, "22943") {
		t.Errorf("no explanation for the absent counters: %+v", st)
	}
	if len(st.Counters) != 0 {
		t.Errorf("counters invented for MongoDB: %+v", st.Counters)
	}
}
