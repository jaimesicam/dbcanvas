package main

// pktnet.go — the two protocols underneath a database problem that are not the database:
// ARP and DNS.
//
// A capture taken on a MySQL node with a port filter is mostly MySQL, but the frames that
// are not are often the explanation. Both of these turned up in a real 50 000-frame PXC
// capture, and both were being reported as "ARP frame, 42 bytes" and "UDP … 43 bytes":
//
//	DNS   every connection starts with a name lookup. A slow resolver adds its latency to
//	      every connect; an NXDOMAIN or SERVFAIL is what a client reports as
//	      2005 CR_UNKNOWN_HOST ("Unknown MySQL server host"), and no packet on 3306 will
//	      ever show it because the connection was never attempted.
//	ARP   layer 2. An unanswered who-has is a host that is not there, which surfaces much
//	      later as a connect timeout; two MAC addresses claiming one IP is an address
//	      conflict — and a *gratuitous* ARP is how a virtual IP announces that it has
//	      moved, which is exactly the moment a cluster's clients get disconnected.
//
// Neither is decoded speculatively: both have fixed, documented layouts, and anything
// that does not parse is reported as unparsed rather than guessed at.

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------- shared state

// pktNetState carries the cross-frame bookkeeping these two protocols need: a request in
// one frame is only interesting because of the reply in another, or the absence of it.
type pktNetState struct {
	// ARP: which MACs have claimed each IP (first frame each was seen in), and which
	// who-has requests are still unanswered.
	arpClaims   map[string]map[string]int
	arpPending  map[string][]int // target IP → frame numbers of unanswered requests
	arpConflict map[string]bool  // IPs already reported, so a noisy conflict is said once

	// DNS: outstanding queries by transaction id, so a response can be timed and an
	// unanswered query can be called out.
	dnsPending map[uint16]*pktDNSQuery
}

// pktDNSQuery is an outstanding lookup.
type pktDNSQuery struct {
	frame int
	ts    float64
	name  string
	qtype string
}

func newPktNetState() *pktNetState {
	return &pktNetState{
		arpClaims:   map[string]map[string]int{},
		arpPending:  map[string][]int{},
		arpConflict: map[string]bool{},
		dnsPending:  map[uint16]*pktDNSQuery{},
	}
}

// pktNetFinish reports what never got an answer. Only knowable once the whole capture has
// been read, so it runs as a post-pass — the same shape as the unanswered-SYN check.
func pktNetFinish(out *pktDecoded, st *pktNetState) {
	mark := func(frame int, issue string) {
		for i := range out.Packets {
			if out.Packets[i].No == frame {
				out.Packets[i].Issues = pktDedupe(append(out.Packets[i].Issues, issue))
				return
			}
		}
	}
	for ip, frames := range st.arpPending {
		for _, f := range frames {
			mark(f, fmt.Sprintf(
				"ARP unanswered — nothing replied for %s, so it is unreachable at layer 2 "+
					"(a client sees this later as a connect timeout)", ip))
		}
	}
	for _, q := range st.dnsPending {
		mark(q.frame, fmt.Sprintf(
			"DNS query unanswered — no response for %s, so the connection was never attempted", q.name))
	}
}

// ---------------------------------------------------------------- ARP

// pktARPDecode reads an ARP frame. The layout is fixed (RFC 826) and the only variable
// part is address length, which is checked rather than assumed.
func pktARPDecode(p *pktPacket, b []byte, st *pktNetState) {
	p.Proto = "ARP"
	if len(b) < 8 {
		p.Info = fmt.Sprintf("ARP frame, %d bytes (truncated)", len(b))
		return
	}
	hlen, plen := int(b[4]), int(b[5])
	op := binary.BigEndian.Uint16(b[6:])
	need := 8 + 2*hlen + 2*plen
	if hlen != 6 || plen != 4 || len(b) < need {
		p.Info = fmt.Sprintf("ARP frame, %d bytes (hardware %d / protocol %d address size)", len(b), hlen, plen)
		return
	}
	senderMAC := pktMAC(b[8 : 8+hlen])
	senderIP := pktIPv4(b[8+hlen : 8+hlen+plen])
	targetMAC := pktMAC(b[8+hlen+plen : 8+2*hlen+plen])
	targetIP := pktIPv4(b[8+2*hlen+plen : need])

	switch op {
	case 1: // request
		// A request whose sender and target IP are the same is a gratuitous ARP: an
		// announcement, not a question. That is how a virtual IP says it has moved.
		if senderIP == targetIP {
			p.Info = fmt.Sprintf("ARP gratuitous — %s announces it is at %s", senderIP, senderMAC)
			p.Issues = append(p.Issues, fmt.Sprintf(
				"Gratuitous ARP for %s — an address was just claimed or moved (a failover, a virtual IP)", senderIP))
		} else {
			p.Info = fmt.Sprintf("ARP who-has %s? tell %s (%s)", targetIP, senderIP, senderMAC)
			st.arpPending[targetIP] = append(st.arpPending[targetIP], p.No)
		}
		pktARPClaim(p, st, senderIP, senderMAC)

	case 2: // reply
		p.Info = fmt.Sprintf("ARP %s is-at %s (to %s)", senderIP, senderMAC, targetIP)
		delete(st.arpPending, senderIP) // answered
		pktARPClaim(p, st, senderIP, senderMAC)

	default:
		p.Info = fmt.Sprintf("ARP opcode %d, %s → %s", op, senderIP, targetIP)
	}
	_ = targetMAC
}

// pktARPClaim records who says they own an address, and flags the second answer.
//
// Two MACs claiming one IP is an address conflict: on a database network that is usually a
// virtual IP that has not finished moving, or two nodes configured with the same address —
// and it produces connections that reach the wrong host, which no amount of reading the
// MySQL protocol will explain.
func pktARPClaim(p *pktPacket, st *pktNetState, ip, mac string) {
	if ip == "0.0.0.0" || mac == "00:00:00:00:00:00" {
		return // an ARP probe claims nothing
	}
	if st.arpClaims[ip] == nil {
		st.arpClaims[ip] = map[string]int{}
	}
	if _, seen := st.arpClaims[ip][mac]; !seen {
		st.arpClaims[ip][mac] = p.No
	}
	if len(st.arpClaims[ip]) > 1 && !st.arpConflict[ip] {
		st.arpConflict[ip] = true
		var macs []string
		for m := range st.arpClaims[ip] {
			macs = append(macs, m)
		}
		p.Issues = append(p.Issues, fmt.Sprintf(
			"ARP conflict — %s is claimed by %s: connections to it can reach either host",
			ip, strings.Join(macs, " and ")))
	}
}

func pktMAC(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(parts, ":")
}

// ---------------------------------------------------------------- DNS

// pktDNSSlowMS is when a lookup is called slow. Every connection pays it, so the bar is
// lower than for a query's own latency.
const pktDNSSlowMS = 100

// pktDNSDecode reads a DNS message (RFC 1035) far enough to name the question, the answer
// and the response code.
func pktDNSDecode(p *pktPacket, b []byte, ts float64, st *pktNetState) {
	p.Proto = "DNS"
	if len(b) < 12 {
		p.Info = fmt.Sprintf("DNS message, %d bytes (truncated)", len(b))
		return
	}
	id := binary.BigEndian.Uint16(b)
	flags := binary.BigEndian.Uint16(b[2:])
	isResponse := flags&0x8000 != 0
	rcode := flags & 0x000f
	qdCount := int(binary.BigEndian.Uint16(b[4:]))
	anCount := int(binary.BigEndian.Uint16(b[6:]))

	name, qtype, off := "", "", 12
	if qdCount > 0 {
		name, off = pktDNSName(b, 12)
		if off+4 <= len(b) {
			qtype = pktDNSType(binary.BigEndian.Uint16(b[off:]))
			off += 4
		}
	}

	if !isResponse {
		p.Info = fmt.Sprintf("DNS query %s %s", qtype, name)
		p.Query = fmt.Sprintf("-- DNS: %s %s", qtype, name)
		st.dnsPending[id] = &pktDNSQuery{frame: p.No, ts: ts, name: name, qtype: qtype}
		return
	}

	// A response: time it against its query, and say what came back.
	var lat float64
	if q, ok := st.dnsPending[id]; ok {
		lat = (ts - q.ts) * 1000
		if name == "" {
			name, qtype = q.name, q.qtype
		}
		delete(st.dnsPending, id)
	}
	answers := pktDNSAnswers(b, off, anCount)
	rc := pktDNSRcode(rcode)
	switch {
	case rcode != 0:
		p.Status = fmt.Sprintf("DNS %s", rc)
		p.Info = fmt.Sprintf("DNS response %s for %s %s", rc, qtype, name)
		// This is where "Unknown MySQL server host" comes from: the client never gets
		// as far as opening a socket, so nothing on the database port explains it.
		p.Issues = append(p.Issues, fmt.Sprintf(
			"DNS %s for %s — the name did not resolve, which a client reports as 2005 CR_UNKNOWN_HOST", rc, name))
	case anCount == 0:
		p.Status = "DNS no answer"
		p.Info = fmt.Sprintf("DNS response NOERROR but no records for %s %s", qtype, name)
		// Only worth flagging for the types a connection actually needs. A resolver asks
		// A and AAAA in parallel, so an IPv4-only host answers every AAAA with NOERROR
		// and no records — perfectly normal, and flagging it produced 12 issues on a real
		// capture that meant nothing at all. Same for HTTPS/SVCB probes.
		if qtype == "A" || qtype == "SRV" || qtype == "PTR" {
			p.Issues = append(p.Issues, fmt.Sprintf(
				"DNS returned no %s record for %s — resolvable name, nothing to connect to", qtype, name))
		}
	default:
		p.Status = "Success"
		p.Info = fmt.Sprintf("DNS response %s %s → %s", qtype, name, strings.Join(answers, ", "))
	}
	if lat > 0 {
		p.LagMS = lat
		p.Info += fmt.Sprintf(" (%.1f ms)", lat)
		if lat >= pktDNSSlowMS {
			p.Issues = append(p.Issues, fmt.Sprintf(
				"Slow DNS response — %.0f ms, paid by every connection that resolves this name", lat))
		}
	}
}

// pktDNSName reads a (possibly compressed) domain name, returning it and the offset just
// past it. Compression pointers are followed once and bounded, so a malformed message
// cannot loop.
func pktDNSName(b []byte, off int) (string, int) {
	var parts []string
	end, jumped, hops := off, false, 0
	for off < len(b) && hops < 32 {
		n := int(b[off])
		switch {
		case n == 0:
			off++
			if !jumped {
				end = off
			}
			return strings.Join(parts, "."), end
		case n&0xc0 == 0xc0: // pointer
			if off+1 >= len(b) {
				return strings.Join(parts, "."), len(b)
			}
			ptr := int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
			if !jumped {
				end = off + 2
				jumped = true
			}
			if ptr >= len(b) || ptr == off {
				return strings.Join(parts, "."), end
			}
			off, hops = ptr, hops+1
		default:
			if off+1+n > len(b) {
				return strings.Join(parts, "."), len(b)
			}
			parts = append(parts, string(b[off+1:off+1+n]))
			off += 1 + n
			if !jumped {
				end = off
			}
		}
	}
	return strings.Join(parts, "."), end
}

// pktDNSAnswers summarises the answer section: addresses for A/AAAA, the target name for
// CNAME/PTR/SRV, and the type alone for anything else.
func pktDNSAnswers(b []byte, off, count int) []string {
	var out []string
	for i := 0; i < count && off < len(b); i++ {
		_, next := pktDNSName(b, off)
		off = next
		if off+10 > len(b) {
			break
		}
		typ := binary.BigEndian.Uint16(b[off:])
		rdLen := int(binary.BigEndian.Uint16(b[off+8:]))
		rd := off + 10
		if rd+rdLen > len(b) {
			break
		}
		switch {
		case typ == 1 && rdLen == 4:
			out = append(out, pktIPv4(b[rd:rd+4]))
		case typ == 28 && rdLen == 16:
			out = append(out, pktIPv6(b[rd:rd+16]))
		case typ == 5 || typ == 12:
			n, _ := pktDNSName(b, rd)
			out = append(out, n)
		default:
			out = append(out, pktDNSType(typ))
		}
		off = rd + rdLen
	}
	if len(out) == 0 {
		return []string{"(no records)"}
	}
	return out
}

func pktDNSType(t uint16) string {
	switch t {
	case 1:
		return "A"
	case 2:
		return "NS"
	case 5:
		return "CNAME"
	case 6:
		return "SOA"
	case 12:
		return "PTR"
	case 15:
		return "MX"
	case 16:
		return "TXT"
	case 28:
		return "AAAA"
	case 33:
		return "SRV"
	case 65:
		return "HTTPS"
	}
	return fmt.Sprintf("TYPE%d", t)
}

func pktDNSRcode(c uint16) string {
	switch c {
	case 0:
		return "NOERROR"
	case 1:
		return "FORMERR"
	case 2:
		return "SERVFAIL"
	case 3:
		return "NXDOMAIN"
	case 4:
		return "NOTIMP"
	case 5:
		return "REFUSED"
	}
	return fmt.Sprintf("RCODE%d", c)
}
