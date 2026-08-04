package main

// pktvalkey_test.go — the Valkey decoder, driven by hand-built captures.
//
// RESP is text, which makes these fixtures the most readable of the four engines' and the
// decoder the easiest to fool: a type byte and a CRLF occur constantly inside data. So
// several tests here are about NOT decoding — junk that must not become a command, a bulk
// payload containing CRLF that must not desynchronise the parser.
//
// Four exist because the live cluster and the live replication link broke them:
//
//	TestValkeyKeepaliveNewlines   a primary sends bare "\n" while it forks; buffering them
//	                             desynchronised the parser so badly that the +FULLRESYNC
//	                             line which followed was discarded by the re-anchor.
//	TestValkeyRDBTransfer         the RDB payload has NO trailing CRLF and can be
//	                             gigabytes, so parsing it as a bulk string buffers the
//	                             whole dataset and never completes.
//	TestValkeyPropagatedWrites    the primary's half of a replication link is a one-way
//	                             stream, and consuming the reply queue for it mislabelled
//	                             every propagated write as an answer to REPLCONF.
//	TestValkeyBusDecodes          the cluster bus is binary, and its header is worth
//	                             decoding: sender, type, epochs and the slot bitmap.

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- RESP builders

func respArr(items ...string) []byte {
	out := []byte(fmt.Sprintf("*%d\r\n", len(items)))
	for _, it := range items {
		out = append(out, []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(it), it))...)
	}
	return out
}
func respSimpleStr(s string) []byte { return []byte("+" + s + "\r\n") }
func respErr(s string) []byte       { return []byte("-" + s + "\r\n") }
func respIntVal(i int) []byte       { return []byte(fmt.Sprintf(":%d\r\n", i)) }
func respBulkStr(s string) []byte   { return []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)) }
func respNil() []byte               { return []byte("$-1\r\n") }
func respPushMsg(items ...string) []byte {
	out := []byte(fmt.Sprintf(">%d\r\n", len(items)))
	for _, it := range items {
		out = append(out, []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(it), it))...)
	}
	return out
}

const valkeyTestPort = 6379

func vkC2S(seq, ack uint32, flags uint8, payload []byte) []byte {
	return ethIPv4TCP(cliIP, srvIP, cliPort, valkeyTestPort, seq, ack, flags, 64240, payload)
}
func vkS2C(seq, ack uint32, flags uint8, payload []byte) []byte {
	return ethIPv4TCP(srvIP, cliIP, valkeyTestPort, cliPort, seq, ack, flags, 64240, payload)
}

func decodeValkey(t *testing.T, b *pcapBuilder) *pktDecoded {
	t.Helper()
	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: valkeyTestPort, Engine: pktEngineValkey})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d
}

func vkSession(b *pcapBuilder) *sampleConn {
	c := &sampleConn{b: b, cseq: 1000, sseq: 5000}
	c.b.frame(0, vkC2S(c.cseq, 0, tcpSYN, nil))
	c.cseq++
	c.b.frame(time.Millisecond, vkS2C(c.sseq, c.cseq, tcpSYN|tcpACK, nil))
	c.sseq++
	return c
}

func vkSend(c *sampleConn, after time.Duration, payload []byte) {
	c.b.frame(c.tick(after), vkC2S(c.cseq, c.sseq, tcpACK|tcpPSH, payload))
	c.cseq += uint32(len(payload))
}
func vkRecv(c *sampleConn, after time.Duration, payload []byte) {
	c.b.frame(c.tick(after), vkS2C(c.sseq, c.cseq, tcpACK|tcpPSH, payload))
	c.sseq += uint32(len(payload))
}

// ---------------------------------------------------------------- basics

func TestValkeySessionDecodes(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	vkSend(c, time.Millisecond, respArr("AUTH", "default", "s3cret"))
	vkRecv(c, time.Millisecond, respSimpleStr("OK"))
	vkSend(c, time.Millisecond, respArr("SET", "user:1000", "alice", "EX", "60"))
	vkRecv(c, time.Millisecond, respSimpleStr("OK"))
	vkSend(c, time.Millisecond, respArr("GET", "user:1000"))
	vkRecv(c, 2*time.Millisecond, respBulkStr("alice"))
	vkSend(c, time.Millisecond, respArr("INCR", "counter"))
	vkRecv(c, time.Millisecond, respIntVal(42))
	vkSend(c, time.Millisecond, respArr("GET", "missing"))
	vkRecv(c, time.Millisecond, respNil())
	vkSend(c, time.Millisecond, respArr("LRANGE", "mylist", "0", "-1"))
	vkRecv(c, time.Millisecond, respArr("a", "b", "c"))

	d := decodeValkey(t, b)
	for _, want := range []string{
		"AUTH user default, password (not shown)",
		"SET user:1000 ← alice (5 bytes) [EX 60]",
		`GET → "alice"`,
		"INCR → 42",
		"GET → (nil)",
		`LRANGE → ["a" "b" "c"]`,
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
	// The password is never rendered, in any form.
	for _, p := range d.Packets {
		if strings.Contains(p.Info, "s3cret") || strings.Contains(p.Query, "s3cret") {
			t.Errorf("#%d leaked the password: %q / %q", p.No, p.Info, p.Query)
		}
	}
	if !issueHas(d, "AUTH on an unencrypted connection") {
		t.Error("a password on an unencrypted connection was not flagged")
	}
	st := d.Streams[0]
	if st.User != "default" {
		t.Errorf("stream user = %q", st.User)
	}
	if st.Queries != 5 {
		t.Errorf("queries = %d, want 5 (AUTH is chatter, not work)", st.Queries)
	}
	if d.Engine != pktEngineValkey {
		t.Errorf("engine = %q", d.Engine)
	}
}

// TestValkeyBulkWithCRLF is the parser's central trap: a bulk string's payload is binary
// and its length is authoritative, so a value holding CRLF must not shift the framing.
func TestValkeyBulkWithCRLF(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	payload := "line one\r\nline two\r\n*3\r\n$3\r\nGET\r\n"
	vkSend(c, time.Millisecond, respArr("SET", "doc", payload))
	vkRecv(c, time.Millisecond, respSimpleStr("OK"))
	vkSend(c, time.Millisecond, respArr("GET", "doc"))
	vkRecv(c, time.Millisecond, respBulkStr(payload))
	vkSend(c, time.Millisecond, respArr("PING"))
	vkRecv(c, time.Millisecond, respSimpleStr("PONG"))

	d := decodeValkey(t, b)
	// The embedded "*3 $3 GET" must NOT have become a command, and PING must still line up.
	if !infoHas(d, "PING → PONG") {
		t.Error("the parser lost its place after a bulk payload containing CRLF")
	}
	n := 0
	for _, p := range d.Packets {
		if p.Command == "GET" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("GET decoded %d times — the payload's embedded RESP was parsed as a command", n)
	}
}

func TestValkeyPipelining(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	// 40 commands in one segment, then 40 replies in another: RESP has no request id, so
	// the pairing is by order and nothing else.
	var batch, replies []byte
	for i := 0; i < 40; i++ {
		batch = append(batch, respArr("SET", fmt.Sprintf("k:%d", i), "v")...)
		replies = append(replies, respSimpleStr("OK")...)
	}
	vkSend(c, time.Millisecond, batch)
	vkRecv(c, 2*time.Millisecond, replies)
	d := decodeValkey(t, b)
	if !issueHas(d, "Pipelining") {
		t.Error("a deep pipeline was not reported")
	}
	if d.Streams[0].Queries != 40 {
		t.Errorf("queries = %d, want 40", d.Streams[0].Queries)
	}
}

// ---------------------------------------------------------------- errors

func TestValkeyErrors(t *testing.T) {
	cases := []struct {
		name      string
		reply     string
		wantState string
		wantIssue string
		wantNone  bool
	}{
		{"moved", "MOVED 12182 172.31.0.5:6379", "MOVED", "slot 12182 is on 172.31.0.5:6379", false},
		{"ask", "ASK 12182 172.31.0.5:6379", "ASK", "must NOT update its slot map", false},
		{"crossslot", "CROSSSLOT Keys in request don't hash to the same slot", "CROSSSLOT", "hash tags", false},
		{"clusterdown", "CLUSTERDOWN The cluster is down", "CLUSTERDOWN", "slots are uncovered", false},
		{"readonly", "READONLY You can't write against a read only replica.", "READONLY", "a write reached a replica", false},
		{"loading", "LOADING Valkey is loading the dataset in memory", "LOADING", "cannot serve anything yet", false},
		{"misconf", "MISCONF Errors writing to the AOF", "MISCONF", "a background save keeps failing", false},
		{"oom", "OOM command not allowed when used memory > 'maxmemory'.", "OOM", "eviction policy cannot free anything", false},
		{"noauth", "NOAUTH Authentication required.", "NOAUTH", "has not authenticated", false},
		{"wrongpass", "WRONGPASS invalid username-password pair", "WRONGPASS", "username or password is wrong", false},
		{"noperm", "NOPERM this user has no permissions to run the 'get' command", "NOPERM", "ACL user is not allowed", false},
		{"busy", "BUSY Valkey is busy running a script", "BUSY", "nothing else is being served", false},
		{"noreplicas", "NOREPLICAS Not enough good replicas to write.", "NOREPLICAS", "min-replicas-to-write", false},
		// Ordinary application errors: reported, never flagged.
		{"wrongtype", "WRONGTYPE Operation against a key holding the wrong kind of value", "WRONGTYPE", "", true},
		{"noscript", "NOSCRIPT No matching script.", "NOSCRIPT", "", false},
		{"syntax", "ERR wrong number of arguments for 'get' command", "ERR", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newPcap(pktLinkEther)
			c := vkSession(b)
			vkSend(c, time.Millisecond, respArr("GET", "somekey"))
			vkRecv(c, time.Millisecond, respErr(tc.reply))
			d := decodeValkey(t, b)
			if !infoHas(d, tc.wantState) {
				t.Errorf("the reply does not name %s", tc.wantState)
			}
			for _, p := range d.Packets {
				if strings.Contains(p.Info, tc.wantState) && p.ErrState != tc.wantState {
					t.Errorf("ErrState = %q, want %q", p.ErrState, tc.wantState)
				}
			}
			if tc.wantIssue != "" && !issueHas(d, tc.wantIssue) {
				t.Errorf("issue %q not raised", tc.wantIssue)
			}
			if tc.wantNone {
				for _, p := range d.Packets {
					for _, i := range p.Issues {
						if !strings.HasPrefix(i, "TCP") && !strings.HasPrefix(i, "AUTH") {
							t.Errorf("an ordinary error raised a finding: %q", i)
						}
					}
				}
			}
			if d.Streams[0].Errors != 1 {
				t.Errorf("stream errors = %d, want 1", d.Streams[0].Errors)
			}
		})
	}
}

// TestValkeyDangerousCommands covers the commands that are safe on a laptop and
// destructive on a busy server — each flagged once per connection, not once per call.
func TestValkeyDangerousCommands(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	for i := 0; i < 3; i++ {
		vkSend(c, time.Millisecond, respArr("KEYS", "*"))
		vkRecv(c, time.Millisecond, respArr("a", "b"))
	}
	vkSend(c, time.Millisecond, respArr("FLUSHALL"))
	vkRecv(c, time.Millisecond, respSimpleStr("OK"))
	vkSend(c, time.Millisecond, respArr("DEBUG", "SLEEP", "1"))
	vkRecv(c, 1100*time.Millisecond, respSimpleStr("OK"))
	vkSend(c, time.Millisecond, respArr("CONFIG", "SET", "maxmemory", "0"))
	vkRecv(c, time.Millisecond, respSimpleStr("OK"))

	d := decodeValkey(t, b)
	for _, want := range []string{
		"KEYS * — this walks the ENTIRE keyspace",
		"FLUSHALL — every key in every database",
		"DEBUG SLEEP",
		"CONFIG SET maxmemory",
	} {
		if !issueHas(d, want) {
			t.Errorf("issue %q not raised", want)
		}
	}
	// KEYS three times, flagged once.
	n := 0
	for _, p := range d.Packets {
		for _, i := range p.Issues {
			if strings.HasPrefix(i, "KEYS") {
				n++
			}
		}
	}
	if n != 1 {
		t.Errorf("KEYS flagged %d times, want once per connection", n)
	}
	// DEBUG SLEEP blocks on purpose and must not also be called slow.
	if issueHas(d, "Slow reply") {
		t.Error("a deliberately blocking command was flagged as slow")
	}
}

// ---------------------------------------------------------------- replication

// TestValkeyKeepaliveNewlines is the live bug: a primary sends bare newlines while it
// forks, and buffering them threw away the +FULLRESYNC that followed.
func TestValkeyKeepaliveNewlines(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	vkSend(c, time.Millisecond, respArr("PSYNC", "?", "-1"))
	// The fork takes a while, and the primary keeps the connection warm with newlines.
	vkRecv(c, 500*time.Millisecond, []byte("\n"))
	vkRecv(c, 500*time.Millisecond, []byte("\n"))
	vkRecv(c, 500*time.Millisecond, []byte("\n"))
	// …then the real answer, and the transfer.
	rdb := strings.Repeat("R", 4096)
	vkRecv(c, time.Millisecond, append(
		respSimpleStr("FULLRESYNC 31b51a3dbeef7ab0f2f0a34e0e4d5a5b6c7d8e9f 22238"),
		[]byte(fmt.Sprintf("$%d\r\n%s", len(rdb), rdb))...))
	vkRecv(c, time.Millisecond, respArr("SELECT", "0"))

	d := decodeValkey(t, b)
	if !infoHas(d, "keep-alive newline") {
		t.Error("the primary's keep-alive newlines were not recognised")
	}
	if !infoHas(d, "+FULLRESYNC") {
		t.Error("the FULLRESYNC reply was lost — the newlines desynchronised the parser")
	}
	if !issueHas(d, "the primary is about to send its ENTIRE dataset") {
		t.Error("a full resynchronisation was not flagged")
	}
	if d.Streams[0].RoleLabel != "Valkey/replication" {
		t.Errorf("connection labelled %q, want Valkey/replication", d.Streams[0].RoleLabel)
	}
}

// TestValkeyRDBTransfer covers both forms, and the fact that the payload has no trailing
// CRLF and must be counted rather than buffered.
func TestValkeyRDBTransfer(t *testing.T) {
	// Disk-based: an announced length.
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	vkSend(c, time.Millisecond, respArr("PSYNC", "?", "-1"))
	vkRecv(c, time.Millisecond, respSimpleStr("FULLRESYNC abc 100"))
	vkRecv(c, time.Millisecond, []byte("$8192\r\n"))
	for i := 0; i < 4; i++ {
		vkRecv(c, time.Millisecond, []byte(strings.Repeat("R", 2048)))
	}
	vkRecv(c, time.Millisecond, respArr("SET", "after", "rdb"))
	d := decodeValkey(t, b)
	if !infoHas(d, "RDB transfer begins, 8.0 KB to come") {
		t.Error("the disk-based RDB header was not read")
	}
	if !infoHas(d, "RDB transfer complete, 8.0 KB") {
		t.Error("the RDB transfer never completed — the payload has no trailing CRLF")
	}
	if !infoHas(d, "propagated: SET") {
		t.Error("the command stream after the RDB was not decoded")
	}

	// Diskless: an EOF delimiter instead of a length.
	mark := "0123456789abcdef0123456789abcdef01234567"
	b2 := newPcap(pktLinkEther)
	c2 := vkSession(b2)
	vkSend(c2, time.Millisecond, respArr("PSYNC", "?", "-1"))
	vkRecv(c2, time.Millisecond, respSimpleStr("FULLRESYNC abc 100"))
	vkRecv(c2, time.Millisecond, []byte("$EOF:"+mark+"\r\n"))
	vkRecv(c2, time.Millisecond, []byte(strings.Repeat("D", 3000)))
	vkRecv(c2, time.Millisecond, append([]byte(strings.Repeat("D", 1000)), []byte(mark)...))
	vkRecv(c2, time.Millisecond, respArr("SET", "after", "diskless"))
	d2 := decodeValkey(t, b2)
	if !infoHas(d2, "diskless, EOF-delimited") {
		t.Error("the diskless RDB header was not read")
	}
	if !infoHas(d2, "RDB transfer complete (diskless)") {
		t.Error("the diskless transfer's delimiter was not found")
	}
	if !infoHas(d2, "propagated: SET") {
		t.Error("the stream after a diskless transfer was not decoded")
	}
}

// TestValkeyPropagatedWrites: the primary's half of a replication link is one-way, and
// treating it as replies mislabelled every write.
func TestValkeyPropagatedWrites(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	vkSend(c, time.Millisecond, respArr("REPLCONF", "listening-port", "6379"))
	vkRecv(c, time.Millisecond, respSimpleStr("OK"))
	vkSend(c, time.Millisecond, respArr("PSYNC", "?", "-1"))
	vkRecv(c, time.Millisecond, respSimpleStr("CONTINUE"))
	vkRecv(c, time.Millisecond, respArr("SELECT", "0"))
	vkRecv(c, time.Millisecond, respArr("SET", "prop:1", "v1"))
	vkRecv(c, time.Millisecond, respArr("PING"))
	vkRecv(c, time.Millisecond, respArr("REPLCONF", "GETACK", "*"))
	vkSend(c, time.Millisecond, respArr("REPLCONF", "ACK", "12345"))

	d := decodeValkey(t, b)
	if !issueHas(d, "Partial resynchronisation") {
		t.Error("+CONTINUE was not recognised as the cheap path")
	}
	for _, want := range []string{
		"propagated: SELECT",
		"propagated: SET",
		"propagated: PING (the primary's periodic keep-alive",
		"REPLCONF GETACK — the primary is asking the replica to report its offset",
		"REPLCONF ACK 12345",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
	// The propagated writes must not have been paired with REPLCONF as replies.
	for _, p := range d.Packets {
		if strings.Contains(p.Info, "REPLCONF → [") {
			t.Errorf("#%d paired a propagated write with a pending command: %q", p.No, p.Info)
		}
	}
}

// TestValkeyReplicationLag: both offsets are on the wire, so the lag is measurable.
func TestValkeyReplicationLag(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	vkSend(c, time.Millisecond, respArr("PSYNC", "?", "-1"))
	vkRecv(c, time.Millisecond, respSimpleStr("FULLRESYNC abc 0"))
	vkRecv(c, time.Millisecond, []byte("$16\r\n"))
	vkRecv(c, time.Millisecond, []byte(strings.Repeat("R", 16)))
	// ~10 MB of propagated writes, and a replica that acknowledges almost none of it.
	//
	// Modest values on purpose: an IP packet's total-length field is 16 bits, so a 100 KB
	// value cannot be one frame — the first version of this fixture built truncated
	// frames and invented TCP gaps, which is a fixture bug that looks exactly like a
	// decoder bug. On the wire a large value arrives as many MSS-sized segments, which is
	// covered by the live 479 KB RDB transfer instead.
	big := strings.Repeat("x", 8000)
	for i := 0; i < 1250; i++ {
		vkRecv(c, time.Millisecond, respArr("SET", fmt.Sprintf("k:%d", i), big))
	}
	vkSend(c, time.Millisecond, respArr("REPLCONF", "ACK", "1000"))
	d := decodeValkey(t, b)
	if !issueHas(d, "Replication lag") {
		t.Error("a replica far behind its primary was not flagged")
	}
}

// ---------------------------------------------------------------- pub/sub, RESP3

func TestValkeyPubSubAndPush(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	vkSend(c, time.Millisecond, respArr("SUBSCRIBE", "news"))
	vkRecv(c, time.Millisecond, respArr("subscribe", "news", "1"))
	// A delivery is unprompted: it must not consume a queued command.
	vkRecv(c, 100*time.Millisecond, respArr("message", "news", "hello subscribers"))
	vkRecv(c, 100*time.Millisecond, respArr("message", "news", "again"))
	d := decodeValkey(t, b)
	if d.Streams[0].RoleLabel != "Valkey/pubsub" {
		t.Errorf("connection labelled %q, want Valkey/pubsub", d.Streams[0].RoleLabel)
	}
	if !infoHas(d, "push: message on") {
		t.Error("a pub/sub delivery was not decoded")
	}

	// RESP3: HELLO 3, a map reply, and a client-tracking invalidation push.
	b2 := newPcap(pktLinkEther)
	c2 := vkSession(b2)
	vkSend(c2, time.Millisecond, respArr("HELLO", "3"))
	vkRecv(c2, time.Millisecond, []byte("%2\r\n$6\r\nserver\r\n$6\r\nvalkey\r\n$5\r\nproto\r\n:3\r\n"))
	vkSend(c2, time.Millisecond, respArr("GET", "k"))
	vkRecv(c2, time.Millisecond, respBulkStr("v"))
	vkRecv(c2, 50*time.Millisecond, respPushMsg("invalidate", "k"))
	d2 := decodeValkey(t, b2)
	if !infoHas(d2, "HELLO protocol 3") {
		t.Error("HELLO 3 was not decoded")
	}
	if !infoHas(d2, "server") || !infoHas(d2, "{") {
		t.Error("a RESP3 map reply was not rendered")
	}
	if !issueHas(d2, "Client-side cache invalidation pushed") {
		t.Error("a tracking invalidation was not flagged")
	}
	if !infoHas(d2, `GET → "v"`) {
		t.Error("the push consumed the queued GET's reply")
	}
}

// ---------------------------------------------------------------- the cluster bus

// vkBusMsg builds a cluster-bus message header.
func vkBusMsg(typ uint16, sender string, currentEpoch, configEpoch, offset uint64, slots int, primary string) []byte {
	msg := make([]byte, valkeyBusHdrLen)
	copy(msg, valkeyBusSig)
	binary.BigEndian.PutUint32(msg[4:], uint32(len(msg)))
	binary.BigEndian.PutUint16(msg[8:], 1)
	binary.BigEndian.PutUint16(msg[10:], 6379)
	binary.BigEndian.PutUint16(msg[12:], typ)
	binary.BigEndian.PutUint16(msg[14:], 1)
	binary.BigEndian.PutUint64(msg[16:], currentEpoch)
	binary.BigEndian.PutUint64(msg[24:], configEpoch)
	binary.BigEndian.PutUint64(msg[32:], offset)
	copy(msg[40:80], sender)
	// Claim `slots` slots by setting that many bits.
	for i := 0; i < slots; i++ {
		msg[valkeyBusSlotsAt+i/8] |= 1 << (i % 8)
	}
	copy(msg[2128:2168], primary)
	return msg
}

func TestValkeyBusDecodes(t *testing.T) {
	busPort := valkeyTestPort + valkeyBusOffset
	b := newPcap(pktLinkEther)
	cseq, sseq := uint32(1000), uint32(5000)
	b.frame(0, ethIPv4TCP(cliIP, srvIP, cliPort, busPort, cseq, 0, tcpSYN, 64240, nil))
	cseq++
	b.frame(time.Millisecond, ethIPv4TCP(srvIP, cliIP, busPort, cliPort, sseq, cseq, tcpSYN|tcpACK, 64240, nil))
	sseq++
	send := func(at time.Duration, msg []byte) {
		b.frame(at, ethIPv4TCP(cliIP, srvIP, cliPort, busPort, cseq, sseq, tcpACK|tcpPSH, 64240, msg))
		cseq += uint32(len(msg))
	}
	send(2*time.Millisecond, vkBusMsg(busPing, "00089dc7c673aaaabbbbccccddddeeeeffff0000", 3, 1, 0, 5461, ""))
	send(3*time.Millisecond, vkBusMsg(busPong, "43080f81daebda5d8ae03150c03393b73fe593b3", 3, 3, 12345, 5461, ""))
	send(4*time.Millisecond, vkBusMsg(busMeet, "9f710104eb03b450dcd4d00875977b0eeac9329c", 3, 0, 0, 0, ""))
	send(5*time.Millisecond, vkBusMsg(busFail, "00089dc7c673aaaabbbbccccddddeeeeffff0000", 4, 1, 0, 5461, ""))
	send(6*time.Millisecond, vkBusMsg(busFailoverAuthReq, "43080f81daebda5d8ae03150c03393b73fe593b3", 5, 3, 0, 0,
		"00089dc7c673aaaabbbbccccddddeeeeffff0000"))
	send(7*time.Millisecond, vkBusMsg(busFailoverAuthAck, "9f710104eb03b450dcd4d00875977b0eeac9329c", 5, 2, 0, 5462, ""))

	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: valkeyTestPort, Engine: pktEngineValkey,
		PortRoles: pktValkeyPortRoles(valkeyTestPort)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Streams[0].RoleLabel != "Valkey/bus" {
		t.Errorf("role label = %q", d.Streams[0].RoleLabel)
	}
	for _, want := range []string{
		"PING from 00089dc7c673…, claims 5461 slot(s), epoch 3/1",
		"PONG from 43080f81daeb…, claims 5461 slot(s), epoch 3/3, offset 12345",
		"MEET from 9f710104eb03…",
		"replica of 00089dc7c673…",
	} {
		if !infoHas(d, want) {
			t.Errorf("no packet shows %q", want)
		}
	}
	for _, want := range []string{
		"MEET from",
		"FAIL message",
		"FAILOVER_AUTH_REQUEST",
		"FAILOVER_AUTH_ACK",
		"Cluster epoch rose",
	} {
		if !issueHas(d, want) {
			t.Errorf("issue %q not raised", want)
		}
	}
	// A heartbeat is not a finding: PING/PONG must be silent.
	for _, p := range d.Packets {
		if strings.HasPrefix(p.Info, "PING from") || strings.HasPrefix(p.Info, "PONG from") {
			for _, i := range p.Issues {
				if !strings.HasPrefix(i, "TCP") && !strings.Contains(i, "epoch rose") {
					t.Errorf("a heartbeat raised a finding: %q", i)
				}
			}
		}
	}
}

// ---------------------------------------------------------------- probes and junk

func TestValkeyInlineCommand(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	vkSend(c, time.Millisecond, []byte("PING\r\n"))
	vkRecv(c, time.Millisecond, respSimpleStr("PONG"))
	d := decodeValkey(t, b)
	if !infoHas(d, "PING (inline, no RESP framing)") {
		t.Error("an inline command was not decoded")
	}
	if !issueHas(d, "Inline command") {
		t.Error("an inline command was not flagged")
	}
}

func TestValkeyBareConnectFlagged(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	c.b.frame(c.tick(10*time.Millisecond), vkC2S(c.cseq, c.sseq, tcpACK|tcpFIN, nil))
	d := decodeValkey(t, b)
	if !issueHas(d, "Connection opened and closed without sending anything") {
		t.Error("a bare connect/close was not recognised")
	}
}

// TestValkeyGarbageIsNotDecoded is the rule this file protects, and RESP is the easiest of
// the four protocols to fool: it is text, so a type byte and a CRLF appear in anything.
func TestValkeyGarbageIsNotDecoded(t *testing.T) {
	b := newPcap(pktLinkEther)
	c := vkSession(b)
	junk := []byte("\x01\x02:notanumber\r\n*abc\r\n$xyz\r\n\xff\xfe\xfd+\r\n\r\n")
	vkSend(c, time.Millisecond, junk)
	d := decodeValkey(t, b)
	for _, p := range d.Packets {
		if p.Command != "" && p.Command != "PING" {
			t.Errorf("junk decoded as command %q: %q", p.Command, p.Info)
		}
		if p.Rows > 1<<20 {
			t.Errorf("junk produced an absurd count: %d", p.Rows)
		}
	}
	// A bus message must not be conjured out of a plausible-looking length either.
	dir := &pktDirState{buf: []byte("RCmb\x00\x00\x00\x10short")}
	p2 := &pktPacket{}
	pktValkeyBusDecode(p2, &pktConn{}, dir, dir.buf)
	if strings.Contains(p2.Info, "from") {
		t.Errorf("a too-short bus message was decoded: %q", p2.Info)
	}
}

func TestValkeySniffEngine(t *testing.T) {
	// A RESP command array on a non-standard port.
	odd := 16380
	b := newPcap(pktLinkEther)
	b.frame(0, ethIPv4TCP(cliIP, srvIP, cliPort, odd, 999, 0, tcpSYN, 64240, nil))
	b.frame(time.Millisecond, ethIPv4TCP(srvIP, cliIP, odd, cliPort, 4999, 1000, tcpSYN|tcpACK, 64240, nil))
	b.frame(2*time.Millisecond, ethIPv4TCP(cliIP, srvIP, cliPort, odd, 1000, 5000, tcpACK|tcpPSH, 64240,
		respArr("SET", "user:1000", "alice")))
	if got := pktSniffEngine(b.buf, odd); got != pktEngineValkey {
		t.Errorf("sniffed %q on port %d, want valkey", got, odd)
	}
	// The cluster bus alone identifies Valkey too — a capture of it holds no RESP at all.
	b2 := newPcap(pktLinkEther)
	busPort := valkeyTestPort + valkeyBusOffset
	b2.frame(0, ethIPv4TCP(cliIP, srvIP, cliPort, busPort, 1000, 5000, tcpACK|tcpPSH, 64240,
		vkBusMsg(busPing, "00089dc7c673aaaabbbbccccddddeeeeffff0000", 1, 1, 0, 5461, "")))
	if got := pktSniffEngine(b2.buf, 0); got != pktEngineValkey {
		t.Errorf("a cluster-bus capture sniffed as %q", got)
	}
	// And an empty capture on 6379 falls back to the port.
	if got := pktSniffEngine(newPcap(pktLinkEther).buf, valkeyClientPort); got != pktEngineValkey {
		t.Errorf("empty capture on 6379 sniffed as %q", got)
	}
	// The other three must not regress.
	for port, want := range map[int]string{
		3306: pktEngineMySQL, pgClientPort: pktEnginePostgres,
		mongoClientPort: pktEngineMongoDB, valkeyClientPort: pktEngineValkey,
	} {
		if got := pktEngineForPort(port); got != want {
			t.Errorf("port %d → %q, want %q", port, got, want)
		}
	}
}

// ---------------------------------------------------------------- RESP unit tests

func TestRESPParse(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"+OK\r\n", "OK"},
		{"-ERR nope\r\n", "-ERR nope"},
		{":42\r\n", "42"},
		{"$5\r\nhello\r\n", `"hello"`},
		{"$-1\r\n", "(nil)"},
		{"*-1\r\n", "(nil array)"},
		{"*2\r\n$3\r\nGET\r\n$1\r\nk\r\n", `["GET" "k"]`},
		{"_\r\n", "(nil)"},
		{"#t\r\n", "true"},
		{",3.14\r\n", "3.14"},
		{"(12345678901234567890\r\n", "12345678901234567890"},
		{"%1\r\n$1\r\na\r\n:1\r\n", `{"a" 1}`},
		{"~2\r\n:1\r\n:2\r\n", "[1 2]"},
		{">2\r\n$7\r\nmessage\r\n$2\r\nch\r\n", `["message" "ch"]`},
	}
	for _, tc := range cases {
		v, n, ok, bad := respParse([]byte(tc.in), 0)
		if !ok || bad {
			t.Errorf("%q: ok=%v bad=%v", tc.in, ok, bad)
			continue
		}
		if n != len(tc.in) {
			t.Errorf("%q consumed %d of %d bytes", tc.in, n, len(tc.in))
		}
		if got := respRender(v, 1); got != tc.want {
			t.Errorf("%q rendered as %q, want %q", tc.in, got, tc.want)
		}
	}
	// Incomplete values are "wait", not "malformed" — the difference between buffering a
	// segment boundary and re-anchoring a broken stream.
	for _, in := range []string{"$5\r\nhel", "*2\r\n$3\r\nGET\r\n", "+OK", ":12"} {
		if _, _, ok, bad := respParse([]byte(in), 0); ok || bad {
			t.Errorf("%q: expected incomplete (ok=false bad=false), got ok=%v bad=%v", in, ok, bad)
		}
	}
	// Malformed values are "bad", so the caller re-anchors instead of waiting forever.
	for _, in := range []string{"$abc\r\n", "*xyz\r\n", ":notanint\r\n", "\x01\x02\x03\r\n"} {
		if _, _, _, bad := respParse([]byte(in), 0); !bad {
			t.Errorf("%q was not reported as malformed", in)
		}
	}
	// A command array is recognised; a reply array is not mistaken for one.
	cmd, _, _, _ := respParse([]byte("*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"), 0)
	if !respIsCommand(cmd) {
		t.Error("a command array was not recognised")
	}
	reply, _, _, _ := respParse([]byte("*2\r\n:1\r\n:2\r\n"), 0)
	if respIsCommand(reply) {
		t.Error("an array of integers was mistaken for a command")
	}
}
