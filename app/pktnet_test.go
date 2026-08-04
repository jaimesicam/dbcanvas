package main

// pktnet_test.go — ARP and DNS, the two protocols that explain a database problem without
// carrying any database traffic.
//
// Both were added after a real 50 000-frame PXC capture showed them as "ARP frame, 42
// bytes" and "UDP … 43 bytes", and the cases below are the ones that capture contained
// plus the failures it did not: an address conflict, an unanswered lookup, NXDOMAIN.

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// arpFrame builds an Ethernet+ARP frame. op is 1 (request) or 2 (reply).
func arpFrame(op uint16, senderMAC, senderIP, targetMAC, targetIP string) []byte {
	eth := make([]byte, 14)
	copy(eth, parseMAC(targetMAC))
	copy(eth[6:], parseMAC(senderMAC))
	binary.BigEndian.PutUint16(eth[12:], 0x0806)

	arp := make([]byte, 28)
	binary.BigEndian.PutUint16(arp, 1)          // ethernet
	binary.BigEndian.PutUint16(arp[2:], 0x0800) // IPv4
	arp[4], arp[5] = 6, 4
	binary.BigEndian.PutUint16(arp[6:], op)
	copy(arp[8:], parseMAC(senderMAC))
	copy(arp[14:], parseIP4(senderIP))
	copy(arp[18:], parseMAC(targetMAC))
	copy(arp[24:], parseIP4(targetIP))
	return append(eth, arp...)
}

func parseMAC(s string) []byte {
	out := make([]byte, 6)
	for i, part := range strings.Split(s, ":") {
		if i >= 6 {
			break
		}
		var v int
		for _, c := range part {
			v *= 16
			switch {
			case c >= '0' && c <= '9':
				v += int(c - '0')
			case c >= 'a' && c <= 'f':
				v += int(c-'a') + 10
			}
		}
		out[i] = byte(v)
	}
	return out
}

// dnsName encodes a domain name in wire form.
func dnsName(n string) []byte {
	var out []byte
	for _, label := range strings.Split(n, ".") {
		out = append(out, byte(len(label)))
		out = append(out, []byte(label)...)
	}
	return append(out, 0)
}

// dnsFrame builds an Ethernet+IPv4+UDP+DNS frame. answers are IPv4 strings ("" for none).
func dnsFrame(srcIP, dstIP string, srcPort, dstPort int, id uint16, response bool, rcode uint16,
	name string, qtype uint16, answers []string) []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg, id)
	flags := uint16(0x0100) // recursion desired
	if response {
		flags |= 0x8000
	}
	flags |= rcode & 0x0f
	binary.BigEndian.PutUint16(msg[2:], flags)
	binary.BigEndian.PutUint16(msg[4:], 1)                    // one question
	binary.BigEndian.PutUint16(msg[6:], uint16(len(answers))) // answers
	msg = append(msg, dnsName(name)...)
	qt := make([]byte, 4)
	binary.BigEndian.PutUint16(qt, qtype)
	binary.BigEndian.PutUint16(qt[2:], 1) // IN
	msg = append(msg, qt...)
	for _, a := range answers {
		// A compression pointer back to the question's name, then an A record.
		rr := []byte{0xc0, 0x0c}
		t := make([]byte, 10)
		binary.BigEndian.PutUint16(t, qtype)
		binary.BigEndian.PutUint16(t[2:], 1)
		binary.BigEndian.PutUint32(t[4:], 60)
		binary.BigEndian.PutUint16(t[8:], 4)
		rr = append(rr, t...)
		rr = append(rr, parseIP4(a)...)
		msg = append(msg, rr...)
	}

	udp := make([]byte, 8)
	binary.BigEndian.PutUint16(udp, uint16(srcPort))
	binary.BigEndian.PutUint16(udp[2:], uint16(dstPort))
	binary.BigEndian.PutUint16(udp[4:], uint16(8+len(msg)))
	udp = append(udp, msg...)

	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+len(udp)))
	ip[8], ip[9] = 64, 17
	copy(ip[12:], parseIP4(srcIP))
	copy(ip[16:], parseIP4(dstIP))

	eth := make([]byte, 14)
	binary.BigEndian.PutUint16(eth[12:], 0x0800)
	out := append(eth, ip...)
	return append(out, udp...)
}

func TestPktARPDecode(t *testing.T) {
	b := newPcap(pktLinkEther)
	// A normal exchange: who-has, then is-at.
	b.frame(0, arpFrame(1, "aa:bb:cc:00:00:01", "10.0.0.9", "00:00:00:00:00:00", "10.0.0.5"))
	b.frame(time.Millisecond, arpFrame(2, "aa:bb:cc:00:00:05", "10.0.0.5", "aa:bb:cc:00:00:01", "10.0.0.9"))
	// A gratuitous announcement: sender and target are the same address.
	b.frame(10*time.Millisecond, arpFrame(1, "aa:bb:cc:00:00:07", "10.0.0.7", "00:00:00:00:00:00", "10.0.0.7"))
	// A second MAC claiming an address already claimed: a conflict.
	b.frame(20*time.Millisecond, arpFrame(2, "de:ad:be:ef:00:05", "10.0.0.5", "aa:bb:cc:00:00:01", "10.0.0.9"))
	// A request nothing ever answers.
	b.frame(30*time.Millisecond, arpFrame(1, "aa:bb:cc:00:00:01", "10.0.0.9", "00:00:00:00:00:00", "10.0.0.250"))

	d := decodeCap(t, b)
	if got := infoOf(d, 1); !strings.Contains(got, "who-has 10.0.0.5") || !strings.Contains(got, "aa:bb:cc:00:00:01") {
		t.Errorf("request: %q", got)
	}
	if got := infoOf(d, 2); !strings.Contains(got, "10.0.0.5 is-at aa:bb:cc:00:00:05") {
		t.Errorf("reply: %q", got)
	}
	if p := packetNo(d, 3); !strings.Contains(p.Info, "gratuitous") || !p.hasIssue("Gratuitous ARP") {
		t.Errorf("gratuitous: info=%q issues=%v", p.Info, p.Issues)
	}
	if p := packetNo(d, 4); !p.hasIssue("ARP conflict") {
		t.Errorf("a second MAC claiming 10.0.0.5 should be a conflict: %v", p.Issues)
	} else if !strings.Contains(p.Issues[len(p.Issues)-1], "de:ad:be:ef:00:05") {
		t.Errorf("the conflict should name both MACs: %v", p.Issues)
	}
	if p := packetNo(d, 5); !p.hasIssue("ARP unanswered") {
		t.Errorf("an unanswered who-has should be flagged: %v", p.Issues)
	}
	// The answered request must NOT be flagged.
	if p := packetNo(d, 1); p.hasIssue("ARP unanswered") {
		t.Errorf("an answered request was flagged as unanswered: %v", p.Issues)
	}
	for _, p := range d.Packets {
		if p.Proto != "ARP" {
			t.Errorf("#%d decoded as %s, want ARP", p.No, p.Proto)
		}
	}
}

func TestPktDNSDecode(t *testing.T) {
	const cli, srv = "10.0.0.9", "10.0.0.2"
	b := newPcap(pktLinkEther)
	// A lookup that works, answered 3 ms later.
	b.frame(0, dnsFrame(cli, srv, 44444, 53, 0x1001, false, 0, "mysql-1.example.net", 1, nil))
	b.frame(3*time.Millisecond, dnsFrame(srv, cli, 53, 44444, 0x1001, true, 0, "mysql-1.example.net", 1,
		[]string{"10.0.0.5"}))
	// A name that does not exist.
	b.frame(10*time.Millisecond, dnsFrame(cli, srv, 44445, 53, 0x1002, false, 0, "nosuchhost.example.net", 1, nil))
	b.frame(12*time.Millisecond, dnsFrame(srv, cli, 53, 44445, 0x1002, true, 3, "nosuchhost.example.net", 1, nil))
	// NOERROR with no records: worth flagging for A…
	b.frame(20*time.Millisecond, dnsFrame(cli, srv, 44446, 53, 0x1003, false, 0, "ipv4only.example.net", 1, nil))
	b.frame(21*time.Millisecond, dnsFrame(srv, cli, 53, 44446, 0x1003, true, 0, "ipv4only.example.net", 1, nil))
	// …and NOT for AAAA, which every IPv4-only host answers this way.
	b.frame(30*time.Millisecond, dnsFrame(cli, srv, 44447, 53, 0x1004, false, 0, "ipv4only.example.net", 28, nil))
	b.frame(31*time.Millisecond, dnsFrame(srv, cli, 53, 44447, 0x1004, true, 0, "ipv4only.example.net", 28, nil))
	// A slow answer.
	b.frame(40*time.Millisecond, dnsFrame(cli, srv, 44448, 53, 0x1005, false, 0, "slow.example.net", 1, nil))
	b.frame(300*time.Millisecond, dnsFrame(srv, cli, 53, 44448, 0x1005, true, 0, "slow.example.net", 1,
		[]string{"10.0.0.6"}))
	// A query nothing answers.
	b.frame(400*time.Millisecond, dnsFrame(cli, srv, 44449, 53, 0x1006, false, 0, "gone.example.net", 1, nil))

	d := decodeCap(t, b)
	for _, p := range d.Packets {
		if p.Proto != "DNS" {
			t.Errorf("#%d decoded as %s, want DNS", p.No, p.Proto)
		}
	}
	if got := infoOf(d, 1); !strings.Contains(got, "query A mysql-1.example.net") {
		t.Errorf("query: %q", got)
	}
	p := packetNo(d, 2)
	if !strings.Contains(p.Info, "10.0.0.5") || !strings.Contains(p.Info, "3.0 ms") {
		t.Errorf("response: %q", p.Info)
	}
	if p.LagMS < 2.9 || p.LagMS > 3.1 {
		t.Errorf("lookup latency = %v, want ~3ms", p.LagMS)
	}
	if p := packetNo(d, 4); !p.hasIssue("DNS NXDOMAIN") {
		t.Errorf("NXDOMAIN not flagged: %v", p.Issues)
	} else if !strings.Contains(p.Issues[0], "2005") {
		t.Errorf("NXDOMAIN should name the client's code: %v", p.Issues)
	}
	if p := packetNo(d, 6); !p.hasIssue("DNS returned no A record") {
		t.Errorf("an empty A answer should be flagged: %v (%s)", p.Issues, p.Info)
	}
	if p := packetNo(d, 8); len(p.Issues) != 0 {
		t.Errorf("an empty AAAA answer is normal for an IPv4-only host, not an issue: %v", p.Issues)
	}
	if p := packetNo(d, 10); !p.hasIssue("Slow DNS response") {
		t.Errorf("a 260 ms lookup should be flagged: %v", p.Issues)
	}
	if p := packetNo(d, 11); !p.hasIssue("DNS query unanswered") {
		t.Errorf("an unanswered query should be flagged: %v", p.Issues)
	}
	// And an answered query must not be.
	if p := packetNo(d, 1); p.hasIssue("DNS query unanswered") {
		t.Errorf("an answered query was flagged: %v", p.Issues)
	}
}

// Galera's ports must be recognised even when the caller declares no roles — an uploaded
// capture has no node to ask. Without this, 22 000 frames of a real PXC capture were run
// through the MySQL decoder and came out as "client payload, 72 bytes".
func TestPktDefaultPortRolesCoverGalera(t *testing.T) {
	b := newPcap(pktLinkEther)
	for i := 0; i < 3; i++ {
		b.frame(time.Duration(i)*time.Millisecond,
			ethIPv4TCP("10.0.0.5", "10.0.0.6", galeraGCSPort, galeraGCSPort,
				uint32(1+i*80), 1, tcpACK|tcpPSH, 64240, make([]byte, 80)))
	}
	// No PortRoles at all, as an upload has none.
	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: 3306})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range d.Packets {
		if p.Proto != "Galera/GCS" {
			t.Errorf("#%d decoded as %s, want Galera/GCS", p.No, p.Proto)
		}
		if strings.Contains(p.Info, "mid-connection") || strings.Contains(p.Info, "MySQL") {
			t.Errorf("#%d was run through the MySQL decoder: %q", p.No, p.Info)
		}
	}
}

// PXC reuses 1047 for "WSREP has not yet prepared node for application use". Reporting that
// as "Unknown command" is the code's original meaning and operationally useless — a real
// capture held 69 of them, and what they meant was that a node was refusing queries.
func TestPktWsrepNotReadyIsNamed(t *testing.T) {
	got := pktErrIssue(1047, "WSREP has not yet prepared node for application use")
	if !strings.Contains(got, "Node not ready") || !strings.Contains(got, "wsrep") {
		t.Errorf("wsrep 1047 = %q", got)
	}
	if pktIssueKind(got) != "Node not ready for application use" {
		t.Errorf("issue kind = %q, want a clean grouping label", pktIssueKind(got))
	}
	// A genuine unknown-command 1047 keeps its own meaning.
	if got := pktErrIssue(1047, "Unknown command"); !strings.Contains(got, "Unknown command") {
		t.Errorf("plain 1047 = %q", got)
	}
}
