package main

// pktsamples_test.go — sample captures for manual checking, generated on demand.
//
// No capture files are committed: a MySQL packet only splits above 0xffffff bytes, so a
// capture that exercises the oversized-row path is necessarily ~17 MB, and real traffic
// carries whatever happened to be on the wire that day. These are built byte-exactly by
// the same pcap builder the decoder tests use — reproducible rather than a recording of
// one lucky run — and written wherever you ask for them:
//
//	PKT_SAMPLE_DIR=/tmp/pkt-samples go test -run TestWriteSampleCaptures ./...
//
// pktSampleCaptures below is the single list behind both writing the files and asserting
// they still demonstrate their case, so the two cannot drift apart.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pktSampleCaptures is every sample, what it is for, and the string that proves it still
// demonstrates it. minPackets guards against a builder that silently stops emitting.
var pktSampleCaptures = []struct {
	name       string
	build      func() []byte
	want       string
	minPackets int
}{
	{"mysql-oversized-blob.pcap", sampleOversizedBlob, "Result set complete: 1 row(s)", 100},
	{"mysql-midstream-join.pcap", sampleMidStreamJoin, "mid-connection", 3},
	{"mysql-tls-upgrade.pcap", sampleTLSUpgrade, "SSLRequest", 5},
	{"mysql-tcp-trouble.pcap", sampleTCPTrouble, "TCP retransmission", 5},
	{"net-arp-dns.pcap", sampleARPandDNS, "who-has", 10},
	// PostgreSQL. The engine is not passed to the decoder for these on purpose — the
	// sniffer has to reach the right answer from the bytes, which is what an upload
	// depends on.
	{"pg-session-errors.pcap", samplePGSession, "ERROR 40P01", 20},
	{"pg-replication.pcap", samplePGReplication, "Standby status", 8},
	{"pg-patroni-cluster.pcap", samplePGPatroni, "Patroni/REST", 8},
}

// sampleOpts is how each sample is decoded. The MySQL ones name their port; the
// PostgreSQL ones name nothing but the port a real capture would have been taken on,
// leaving the protocol to pktSniffEngine — the sample then also tests the path an
// upload takes. The Patroni sample carries its cluster port roles, exactly as a
// capture of a Patroni member does.
func sampleOpts(name string) pktDecodeOpts {
	switch {
	case strings.HasPrefix(name, "pg-patroni"):
		return pktDecodeOpts{ServerPort: pgClientPort, PortRoles: pktPGPortRoles(pgClientPort)}
	case strings.HasPrefix(name, "pg-"):
		return pktDecodeOpts{ServerPort: pgClientPort}
	}
	return pktDecodeOpts{ServerPort: srvPort}
}

func TestWriteSampleCaptures(t *testing.T) {
	dir := os.Getenv("PKT_SAMPLE_DIR")
	if dir == "" {
		t.Skip("set PKT_SAMPLE_DIR to write sample captures")
	}
	for _, s := range pktSampleCaptures {
		buf := s.build()
		path := filepath.Join(dir, s.name)
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		d, err := pktDecode(buf, sampleOpts(s.name))
		if err != nil {
			t.Fatalf("%s: decodes with an error: %v", s.name, err)
		}
		t.Logf("wrote %s (%d bytes, %d packets)", path, len(buf), len(d.Packets))
	}
}

// sampleConn tracks both directions' sequence numbers so generated samples contain
// NO accidental gaps or retransmissions. Hand-written arithmetic drifted the first
// time and produced samples that showed a "TCP gap" nobody had asked for — a sample
// that invents a fault is worse than no sample.
type sampleConn struct {
	b          *pcapBuilder
	cseq, sseq uint32
	at         time.Duration
}

func newSampleConn(b *pcapBuilder) *sampleConn {
	return &sampleConn{b: b, cseq: 1000, sseq: 5000}
}

// tick advances the sample clock and returns the new offset.
func (s *sampleConn) tick(d time.Duration) time.Duration {
	s.at += d
	return s.at
}

// toServer emits a client→server frame and advances the client's sequence.
func (s *sampleConn) toServer(after time.Duration, flags uint8, payload []byte) {
	s.b.frame(s.tick(after), ethIPv4TCP(cliIP, srvIP, cliPort, srvPort, s.cseq, s.sseq, flags, 64240, payload))
	s.cseq += uint32(len(payload))
	if flags&(tcpSYN|tcpFIN) != 0 {
		s.cseq++
	}
}

// toClient emits a server→client frame and advances the server's sequence.
func (s *sampleConn) toClient(after time.Duration, flags uint8, payload []byte) {
	s.b.frame(s.tick(after), ethIPv4TCP(srvIP, cliIP, srvPort, cliPort, s.sseq, s.cseq, flags, 64240, payload))
	s.sseq += uint32(len(payload))
	if flags&(tcpSYN|tcpFIN) != 0 {
		s.sseq++
	}
}

// toClientSegmented sends one byte stream as MTU-sized segments, the way a large
// result set actually arrives.
func (s *sampleConn) toClientSegmented(stream []byte, per int, gap time.Duration) {
	for off := 0; off < len(stream); off += per {
		end := off + per
		if end > len(stream) {
			end = len(stream)
		}
		s.toClient(gap, tcpACK|tcpPSH, stream[off:end])
	}
}

// handshake is SYN / SYN,ACK / ACK, greeting, login, OK — a connection captured
// from its first byte, so the decoder is fully synchronised.
func (s *sampleConn) handshake() {
	s.toServer(0, tcpSYN, nil)
	s.toClient(time.Millisecond, tcpSYN|tcpACK, nil)
	s.toServer(time.Millisecond, tcpACK, nil)
	s.toClient(time.Millisecond, tcpACK|tcpPSH, greeting("8.0.46-37"))
	resp := make([]byte, 32)
	binary.LittleEndian.PutUint32(resp, 0x000a8285)
	resp = append(resp, []byte("appuser")...)
	resp = append(resp, 0)
	s.toServer(time.Millisecond, tcpACK|tcpPSH, mysqlPkt(1, resp))
	s.toClient(time.Millisecond, tcpACK|tcpPSH, okPacket(2, 0, 0))
}

// sampleOversizedBlob is a SELECT returning a row larger than a MySQL packet may
// carry, so the server splits it at 0xffffff and the decoder has to put it back.
func sampleOversizedBlob() []byte {
	c := newSampleConn(newPcap(pktLinkEther))
	c.handshake()
	q := append([]byte{comQuery}, []byte("SELECT payload FROM documents WHERE id = 42")...)
	c.toServer(5*time.Millisecond, tcpACK|tcpPSH, mysqlPkt(0, q))

	const rowBytes = (16 << 20) + 65536
	row := make([]byte, 9+rowBytes)
	row[0] = 0xfe // an 8-byte length follows: this is why such a row looks like an EOF
	binary.LittleEndian.PutUint64(row[1:], uint64(rowBytes))
	for i := 9; i < len(row); i++ {
		row[i] = byte('a' + i%26)
	}
	var stream []byte
	stream = append(stream, mysqlPkt(1, []byte{0x01})...)
	stream = append(stream, mysqlPkt(2, []byte{3, 'd', 'e', 'f'})...)
	stream = append(stream, 0xff, 0xff, 0xff, 3)
	stream = append(stream, row[:0xffffff]...)
	rest := row[0xffffff:]
	stream = append(stream, byte(len(rest)), byte(len(rest)>>8), byte(len(rest)>>16), 4)
	stream = append(stream, rest...)
	stream = append(stream, mysqlPkt(5, []byte{0xfe, 0, 0, 2, 0})...)
	c.toClientSegmented(stream, 1448, 20*time.Microsecond)
	return c.b.buf
}

// sampleMidStreamJoin is what a capture of a busy server mostly looks like: a
// connection that was already running, which becomes readable one round-trip in.
func sampleMidStreamJoin() []byte {
	c := newSampleConn(newPcap(pktLinkEther))
	// No SYN. These server bytes are the tail of something the capture never saw —
	// and they begin with 0x00, which a careless decoder reads as an OK packet.
	c.toClient(0, tcpACK|tcpPSH, mysqlPkt(7, []byte{0x00, 0xfb, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}))
	q := append([]byte{comQuery}, []byte("UPDATE inventory SET qty = qty - 1 WHERE sku = 'A-1001'")...)
	c.toServer(20*time.Millisecond, tcpACK|tcpPSH, mysqlPkt(0, q))
	c.toClient(15*time.Millisecond, tcpACK|tcpPSH, okPacket(1, 1, 0))
	q2 := append([]byte{comQuery}, []byte("SELECT qty FROM inventory WHERE sku = 'A-1001'")...)
	c.toServer(5*time.Millisecond, tcpACK|tcpPSH, mysqlPkt(0, q2))
	rs := mysqlPkt(1, []byte{0x01})
	rs = append(rs, mysqlPkt(2, []byte{3, 'q', 't', 'y'})...)
	rs = append(rs, mysqlPkt(3, []byte{2, '4', '1'})...)
	rs = append(rs, mysqlPkt(4, []byte{0xfe, 0, 0, 2, 0})...)
	c.toClient(time.Millisecond, tcpACK|tcpPSH, rs)
	return c.b.buf
}

// sampleTLSUpgrade is a connection that switches to TLS: greeting in the clear, an
// SSLRequest, then records whose contents a capture cannot read.
func sampleTLSUpgrade() []byte {
	c := newSampleConn(newPcap(pktLinkEther))
	c.toServer(0, tcpSYN, nil)
	c.toClient(time.Millisecond, tcpSYN|tcpACK, nil)
	c.toServer(time.Millisecond, tcpACK, nil)
	c.toClient(time.Millisecond, tcpACK|tcpPSH, greeting("8.0.46-37"))
	ssl := make([]byte, 32)
	binary.LittleEndian.PutUint32(ssl, 0x000a8285|capSSL)
	c.toServer(time.Millisecond, tcpACK|tcpPSH, mysqlPkt(1, ssl))
	c.toServer(time.Millisecond, tcpACK|tcpPSH,
		append([]byte{0x16, 0x03, 0x01, 0x00, 0x2c, 0x01, 0, 0, 0x28}, make([]byte, 0x28)...))
	c.toClient(2*time.Millisecond, tcpACK|tcpPSH,
		append([]byte{0x16, 0x03, 0x03, 0x00, 0x2c, 0x02, 0, 0, 0x28}, make([]byte, 0x28)...))
	c.toClient(time.Millisecond, tcpACK|tcpPSH,
		append([]byte{0x14, 0x03, 0x03, 0x00, 0x01}, 0x01))
	for i := 0; i < 4; i++ {
		app := append([]byte{0x17, 0x03, 0x03, 0x00, 0x40}, make([]byte, 0x40)...)
		c.toServer(3*time.Millisecond, tcpACK|tcpPSH, app)
		c.toClient(time.Millisecond, tcpACK|tcpPSH, app)
	}
	return c.b.buf
}

// sampleTCPTrouble is one connection carrying each transport-level fault in turn.
func sampleTCPTrouble() []byte {
	c := newSampleConn(newPcap(pktLinkEther))
	c.handshake()
	q := append([]byte{comQuery}, []byte("SELECT * FROM orders WHERE created_at > NOW() - INTERVAL 1 HOUR")...)
	req := mysqlPkt(0, q)
	c.toServer(5*time.Millisecond, tcpACK|tcpPSH, req)
	// The same request again, from the same sequence: a retransmission.
	c.cseq -= uint32(len(req))
	c.toServer(200*time.Millisecond, tcpACK|tcpPSH, req)
	// A late, large answer: high latency, and a run of duplicate ACKs behind it.
	rs := mysqlPkt(1, []byte{0x01})
	rs = append(rs, mysqlPkt(2, []byte{3, 'd', 'e', 'f'})...)
	for i := 0; i < 40; i++ {
		rs = append(rs, mysqlPkt(byte(3+i), append([]byte{19}, []byte("row-payload--------")...))...)
	}
	rs = append(rs, mysqlPkt(60, []byte{0xfe, 0, 0, 2, 0})...)
	c.toClient(350*time.Millisecond, tcpACK|tcpPSH, rs)
	for i := 0; i < 3; i++ {
		c.toServer(time.Millisecond, tcpACK, nil)
	}
	// A gap: 5000 bytes the capture never saw.
	c.sseq += 5000
	c.toClient(20*time.Millisecond, tcpACK|tcpPSH, mysqlPkt(1, []byte{0x00, 1, 0, 2, 0, 0, 0}))
	// A zero window from the client, then a reset.
	c.b.frame(c.tick(20*time.Millisecond),
		ethIPv4TCP(cliIP, srvIP, cliPort, srvPort, c.cseq, c.sseq, tcpACK, 0, nil))
	c.toServer(80*time.Millisecond, tcpRST, nil)
	return c.b.buf
}

// sampleARPandDNS is the traffic underneath a database problem that is not database
// traffic: a name that resolves, one that does not, and layer-2 trouble.
func sampleARPandDNS() []byte {
	b := newPcap(pktLinkEther)
	const cli, srv, res = "10.0.0.9", "10.0.0.5", "10.0.0.2"
	// A lookup that works, then the connection it enables.
	b.frame(0, dnsFrame(cli, res, 44444, 53, 0x2001, false, 0, "mysql-1.example.net", 1, nil))
	b.frame(2*time.Millisecond, dnsFrame(res, cli, 53, 44444, 0x2001, true, 0, "mysql-1.example.net", 1, []string{srv}))
	// The AAAA half of the same dual-stack lookup: NOERROR, no records, entirely normal.
	b.frame(3*time.Millisecond, dnsFrame(cli, res, 44445, 53, 0x2002, false, 0, "mysql-1.example.net", 28, nil))
	b.frame(4*time.Millisecond, dnsFrame(res, cli, 53, 44445, 0x2002, true, 0, "mysql-1.example.net", 28, nil))
	// A name that does not exist — a client reports this as 2005, and nothing on the
	// database port will ever show it.
	b.frame(20*time.Millisecond, dnsFrame(cli, res, 44446, 53, 0x2003, false, 0, "mysql-9.example.net", 1, nil))
	b.frame(23*time.Millisecond, dnsFrame(res, cli, 53, 44446, 0x2003, true, 3, "mysql-9.example.net", 1, nil))
	// A slow resolver: every connection pays this.
	b.frame(40*time.Millisecond, dnsFrame(cli, res, 44447, 53, 0x2004, false, 0, "slow.example.net", 1, nil))
	b.frame(320*time.Millisecond, dnsFrame(res, cli, 53, 44447, 0x2004, true, 0, "slow.example.net", 1, []string{"10.0.0.8"}))
	// Layer 2: a normal exchange, a gratuitous announcement (a virtual IP moving), an
	// address claimed by a second MAC, and a request nothing answers.
	b.frame(400*time.Millisecond, arpFrame(1, "aa:bb:cc:00:00:09", cli, "00:00:00:00:00:00", srv))
	b.frame(401*time.Millisecond, arpFrame(2, "aa:bb:cc:00:00:05", srv, "aa:bb:cc:00:00:09", cli))
	b.frame(500*time.Millisecond, arpFrame(1, "aa:bb:cc:00:00:07", "10.0.0.7", "00:00:00:00:00:00", "10.0.0.7"))
	b.frame(600*time.Millisecond, arpFrame(2, "de:ad:be:ef:00:05", srv, "aa:bb:cc:00:00:09", cli))
	b.frame(700*time.Millisecond, arpFrame(1, "aa:bb:cc:00:00:09", cli, "00:00:00:00:00:00", "10.0.0.250"))
	return b.buf
}

// TestSampleCapturesDecode keeps the samples honest: each one must still decode, and
// must still contain the thing it exists to demonstrate. A sample that quietly stops
// demonstrating its case is a trap for whoever reaches for it to check a change.
//
// It works on the bytes in memory, so it runs on every `go test` rather than only when
// somebody has generated the files — the oversized-row path in particular is asserted
// nowhere else. (An earlier version read from disk and skipped when the files were
// absent, which is the same as not having a test.)
func TestSampleCapturesDecode(t *testing.T) {
	for _, s := range pktSampleCaptures {
		d, err := pktDecode(s.build(), sampleOpts(s.name))
		if err != nil {
			t.Errorf("%s: %v", s.name, err)
			continue
		}
		if len(d.Packets) < s.minPackets {
			t.Errorf("%s: %d packets, expected at least %d", s.name, len(d.Packets), s.minPackets)
		}
		found := false
		for _, p := range d.Packets {
			if strings.Contains(p.Info, s.want) || strings.Contains(p.Proto, s.want) {
				found = true
				break
			}
			for _, is := range p.Issues {
				if strings.Contains(is, s.want) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s no longer demonstrates %q", s.name, s.want)
		}
	}
}

// ---------------------------------------------------------------- PostgreSQL samples

// samplePGSession is one connection that does everything an application connection
// does, and then fails in the four ways worth recognising: a deadlock, a write to a
// read-only connection, a cancelled statement, and a FATAL that ends the session.
func samplePGSession() []byte {
	b := newPcap(pktLinkEther)
	c := pgSession(b)
	pgSend(c, time.Millisecond, pgStartup("user", "carsim", "database", "rental", "application_name", "carsim"))
	pgRecv(c, time.Millisecond, pgMsg('R', []byte{0, 0, 0, 10, 'S', 'C', 'R', 'A', 'M', '-', 'S', 'H', 'A', '-', '2', '5', '6', 0, 0}))
	pgSend(c, time.Millisecond, pgMsg('p', []byte("SCRAM-SHA-256\x00n,,n=,r=rOprNGfwEbeRWgbNEkqO")))
	pgRecv(c, time.Millisecond, pgMsg('R', []byte{0, 0, 0, 11}))
	pgSend(c, time.Millisecond, pgMsg('p', []byte("c=biws,r=rOprNGfwEbeRWgbNEkqO,p=dHzbZapWIk4jUhN+")))
	pgRecv(c, 2*time.Millisecond, concat(
		pgMsg('R', []byte{0, 0, 0, 12}), pgAuthOK(),
		pgMsg('S', []byte("server_version\x0016.14\x00")),
		pgMsg('K', []byte{0, 0, 0x0b, 0x94, 0x11, 0x22, 0x33, 0x44}),
		pgReady('I')))

	// A simple query with a result set.
	pgSend(c, 3*time.Millisecond, pgQuery("SELECT id, plate, mileage FROM cars WHERE branch_id = 12 ORDER BY id"))
	pgRecv(c, 4*time.Millisecond, concat(
		pgRowDesc("id", "plate", "mileage"),
		pgDataRow("1", "ABC-1234", "84210"), pgDataRow("2", "XYZ-9876", "12995"),
		pgCmdDone("SELECT 2"), pgReady('I')))

	// The extended protocol: a named statement bound and executed inside a transaction.
	parse := pgMsg('P', concat([]byte("s1\x00"),
		[]byte("UPDATE cars SET mileage = mileage + $1 WHERE id = $2"), []byte{0, 0, 0}))
	bind := pgMsg('B', concat([]byte{0}, []byte("s1\x00"), []byte{0, 0, 0, 2},
		[]byte{0, 0, 0, 3}, []byte("120"), []byte{0, 0, 0, 1}, []byte("1"), []byte{0, 0}))
	pgSend(c, 2*time.Millisecond, concat(pgQuery("BEGIN")))
	pgRecv(c, time.Millisecond, concat(pgCmdDone("BEGIN"), pgReady('T')))
	pgSend(c, time.Millisecond, concat(parse, bind, pgMsg('E', []byte{0, 0, 0, 0, 0}), pgMsg('S', nil)))
	pgRecv(c, 2*time.Millisecond, concat(pgMsg('1', nil), pgMsg('2', nil), pgCmdDone("UPDATE 1")))
	pgRecv(c, time.Millisecond, pgReady('T'))

	// A deadlock ends the transaction.
	pgSend(c, time.Millisecond, pgQuery("UPDATE branches SET fleet = fleet - 1 WHERE id = 12"))
	pgRecv(c, 1200*time.Millisecond, concat(
		pgErr('E', "ERROR", "40P01", "deadlock detected", map[byte]string{
			'D': "Process 5539 waits for ShareLock on transaction 821; blocked by process 5540.",
			'H': "See server log for query details."}),
		pgReady('E')))
	pgSend(c, time.Millisecond, pgQuery("ROLLBACK"))
	pgRecv(c, time.Millisecond, concat(pgCmdDone("ROLLBACK"), pgReady('I')))

	// A write that reached a standby: the load balancer sent it to the wrong node.
	pgSend(c, 2*time.Millisecond, pgQuery("INSERT INTO bookings (car_id, day) VALUES (1, current_date)"))
	pgRecv(c, time.Millisecond, concat(
		pgErr('E', "ERROR", "25006", "cannot execute INSERT in a read-only transaction", nil),
		pgReady('I')))

	// A statement cancelled by statement_timeout, then a FATAL that ends the session.
	pgSend(c, 2*time.Millisecond, pgQuery("SELECT pg_sleep(30)"))
	pgRecv(c, 300*time.Millisecond, concat(
		pgErr('E', "ERROR", "57014", "canceling statement due to statement timeout", nil),
		pgReady('I')))
	pgRecv(c, 5*time.Millisecond, pgErr('E', "FATAL", "57P01",
		"terminating connection due to administrator command", nil))
	c.b.frame(c.tick(time.Millisecond), pgS2C(c.sseq, c.cseq, tcpACK|tcpFIN, nil))
	return b.buf
}

// samplePGReplication is a standby streaming from a primary, captured mid-stream — the
// normal case, since replication connections outlive any capture. It carries the LSNs
// that make lag measurable, then falls a WAL segment behind.
func samplePGReplication() []byte {
	b := newPcap(pktLinkEther)
	// No SYN: this connection is older than the capture.
	c := &sampleConn{b: b, cseq: 70000, sseq: 90000}
	sent := uint64(0x25211CE8)
	flushed := sent
	for i := 0; i < 6; i++ {
		pgSend(c, 400*time.Millisecond, pgStandbyStatus(flushed, flushed, flushed))
		sent += 0x1000
		pgRecv(c, 100*time.Millisecond, pgXLogData(flushed, sent, make([]byte, 296)))
		flushed = sent
	}
	// The standby stops keeping up: the primary streams on, the standby's flush LSN
	// stays where it was, and the gap passes a WAL segment.
	for i := 0; i < 4; i++ {
		sent += 0x600000
		pgRecv(c, 200*time.Millisecond, pgXLogData(sent-0x600000, sent, make([]byte, 1400)))
	}
	pgSend(c, 400*time.Millisecond, pgStandbyStatus(flushed, flushed, flushed))
	pgRecv(c, 100*time.Millisecond, pgKeepalive(sent, 1))
	return b.buf
}

// samplePGPatroni is the traffic that decides who leads: HAProxy's health checks against
// two members' REST APIs, and Patroni renewing its lease in etcd. The replica answering
// 503 on /primary is correct behaviour, not a fault, and the sample exists partly to
// show that it is not flagged.
func samplePGPatroni() []byte {
	b := newPcap(pktLinkEther)
	http := func(port int, cseq, sseq uint32, at time.Duration, req, res string) {
		b.frame(at, ethIPv4TCP(cliIP, srvIP, cliPort+int(cseq%97), port, cseq, sseq, tcpACK|tcpPSH, 64240, []byte(req)))
		b.frame(at+2*time.Millisecond, ethIPv4TCP(srvIP, cliIP, port, cliPort+int(cseq%97), sseq, cseq+uint32(len(req)), tcpACK|tcpPSH, 64240, []byte(res)))
	}
	// The leader answers 200 on /primary; the same check on a replica answers 503.
	http(patroniRESTPort, 1000, 5000, 0,
		"GET /primary HTTP/1.1\r\nHost: patroni01:8008\r\nUser-Agent: HAProxy\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"state\": \"running\", \"role\": \"primary\", \"timeline\": 1}")
	http(patroniRESTPort, 2000, 6000, 2*time.Second,
		"GET /replica HTTP/1.1\r\nHost: patroni01:8008\r\nUser-Agent: HAProxy\r\n\r\n",
		"HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\n\r\n{\"state\": \"running\", \"role\": \"primary\"}")
	// Patroni renewing the lease that holds the leader lock, and reading cluster state.
	http(etcdClientPort, 3000, 7000, 3*time.Second,
		"POST /v3/lease/keepalive HTTP/1.1\r\nHost: patroni01:2379\r\nContent-Length: 29\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n")
	http(etcdClientPort, 4000, 8000, 4*time.Second,
		"POST /v3/kv/range HTTP/1.1\r\nHost: patroni01:2379\r\nContent-Length: 96\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n")
	// And the failure that precedes every Patroni failover: etcd stops answering.
	http(etcdClientPort, 5000, 9000, 5*time.Second,
		"POST /v3/kv/txn HTTP/1.1\r\nHost: patroni01:2379\r\nContent-Length: 142\r\n\r\n",
		"HTTP/1.1 503 Service Unavailable\r\n\r\netcdserver: request timed out")
	return b.buf
}

// concat joins byte slices, which the PostgreSQL samples need constantly: one TCP
// segment usually carries several protocol messages, and nested append() calls are
// unreadable at four deep.
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
