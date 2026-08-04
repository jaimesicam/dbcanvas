package main

// pktdecode.go — the Packet Inspector's offline decoder: the bytes tcpdump wrote,
// in; a flat list of annotated packets, out.
//
// Everything here is pure and self-contained on purpose. Decoding is the part of
// this feature most likely to be wrong on real traffic, so it is a library of
// functions over a byte slice with no engine, no store and no HTTP — which is what
// lets pktdecode_test.go drive it with hand-built captures and a fixture taken off
// a live node.
//
// Layers, in order:
//
//	capture file   classic pcap (µs and ns) and pcapng — tcpdump writes the first,
//	               Wireshark saves the second, and an upload can be either
//	link           Ethernet (+VLAN tags), Linux cooked v1/v2 (`tcpdump -i any`),
//	               raw IP, and BSD loopback
//	network        IPv4 / IPv6
//	transport      TCP (payload, flags, seq/ack, window)
//	application    MySQL client/server protocol, reassembled across segments, and
//	               TLS records once a connection has switched to encryption
//
// No third-party dependency: gopacket would bring a large tree for what is, at
// this layer, a dozen struct offsets, and MySQL's protocol has to be written by
// hand regardless.

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------- limits

const (
	// pktMaxPackets caps a decode. A minute of heavy sysbench traffic is ~300k
	// frames; past this the browser cannot usefully be handed more, and the
	// summary says how many were dropped rather than silently truncating.
	pktMaxPackets = 400000
	// pktQueryTextMax bounds the SQL kept per packet. A LOAD DATA or a giant IN()
	// list would otherwise sit in memory a second time.
	pktQueryTextMax = 8192
	// pktSlowResponseMS is when a server response gets flagged as high latency.
	// Deliberately generous: this is a lab tool on a loopback-ish network, where
	// anything above a few ms is worth a look.
	pktSlowResponseMS = 100
	// pktBigResultBytes is when a response is flagged as a heavy result set.
	pktBigResultBytes = 1 << 20
)

// ---------------------------------------------------------------- model

// pktPacket is one captured frame, decoded. Field names are the JSON the browser
// consumes; the packet list shows Time/Src/Dst/Proto/Info/Issues and the detail
// panel everything else.
type pktPacket struct {
	No     int     `json:"no"`     // 1-based capture order — the stable id
	TSUnix float64 `json:"ts"`     // epoch seconds with fraction (µs or ns as captured)
	Stream int     `json:"stream"` // TCP connection index, 0-based
	Dir    string  `json:"dir"`    // c2s | s2c | "" (non-TCP)

	Src string `json:"src"` // ip:port
	Dst string `json:"dst"`

	Proto  string   `json:"proto"` // MySQL | TLS | TCP | UDP | IP
	Info   string   `json:"info"`  // the one-line summary, Wireshark-style
	Issues []string `json:"issues,omitempty"`

	FrameLen   int `json:"frameLen"`   // bytes on the wire
	PayloadLen int `json:"payloadLen"` // TCP payload bytes in this frame

	// TCP detail.
	Flags  string `json:"flags,omitempty"`
	Seq    uint32 `json:"seq,omitempty"`
	Ack    uint32 `json:"ack,omitempty"`
	Window int    `json:"window,omitempty"`

	// Application detail. Query is the SQL text or a description of the command;
	// Status/Rows/ErrCode describe a server response.
	Query   string  `json:"query,omitempty"`
	Command string  `json:"command,omitempty"` // COM_QUERY, COM_STMT_EXECUTE, …
	Status  string  `json:"status,omitempty"`  // Success | Error 1064 (42000): … | Encrypted
	Rows    int     `json:"rows,omitempty"`
	ErrCode int     `json:"errCode,omitempty"`
	LagMS   float64 `json:"lagMs,omitempty"` // request→response for a response; SYN→SYN,ACK for a handshake

	// Offset/length of the frame inside the capture buffer, so the detail endpoint
	// can produce a hex dump without a second copy of every packet in memory.
	off, cap int
}

// pktStream is one TCP connection seen in the capture.
type pktStream struct {
	Index     int     `json:"index"`
	Client    string  `json:"client"` // ip:port
	Server    string  `json:"server"`
	Packets   int     `json:"packets"`
	Bytes     int     `json:"bytes"`
	StartTS   float64 `json:"startTs"`
	EndTS     float64 `json:"endTs"`
	Queries   int     `json:"queries"`
	Errors    int     `json:"errors"`
	Role      string  `json:"role"`      // pktRole*: the protocol this connection carries
	RoleLabel string  `json:"roleLabel"` // MySQL | Galera/GCS | Galera/IST | Galera/SST
	TLS       bool    `json:"tls"`       // the connection switched to encryption
	User      string  `json:"user"`      // from the handshake response
	Database  string  `json:"database"`  // schema named at connect / by COM_INIT_DB
	Version   string  `json:"version"`   // server version from the greeting
	Reset     bool    `json:"reset"`     // ended with RST
}

// pktDecoded is a whole decoded capture.
type pktDecoded struct {
	Packets  []pktPacket `json:"packets"`
	Streams  []pktStream `json:"streams"`
	LinkType int         `json:"linkType"`
	Format   string      `json:"format"` // pcap | pcap-ns | pcapng
	Dropped  int         `json:"dropped"`
	Truncat  int         `json:"truncated"` // frames the snaplen cut short
}

// ---------------------------------------------------------------- capture file

// link-layer types this decoder understands (tcpdump's DLT_* / LINKTYPE_*).
const (
	pktLinkNull    = 0   // BSD loopback: 4-byte AF_ prefix
	pktLinkEther   = 1   // the common case
	pktLinkRaw     = 101 // raw IP, no link header
	pktLinkSLL     = 113 // Linux cooked, `tcpdump -i any` on older libpcap
	pktLinkSLL2    = 276 // Linux cooked v2, what `-i any` writes now
	pktLinkRawAlt  = 12  // DLT_RAW on some platforms
	pktLinkLoopAlt = 108 // DLT_LOOP, big-endian AF prefix
)

type pktReader struct {
	buf      []byte
	le       binary.ByteOrder
	nano     bool
	linkType int
	format   string
	pos      int
	// pcapng only: each block is length-prefixed, and the interface's linktype
	// arrives in an IDB rather than a file header.
	ng bool
}

// pktOpen validates a capture's header and returns a reader positioned at the
// first packet record.
func pktOpen(buf []byte) (*pktReader, error) {
	if len(buf) < 24 {
		return nil, fmt.Errorf("capture is too short to be a pcap file (%d bytes)", len(buf))
	}
	be, le := binary.BigEndian, binary.LittleEndian
	switch {
	case le.Uint32(buf) == 0xa1b2c3d4:
		return &pktReader{buf: buf, le: le, linkType: int(le.Uint32(buf[20:])), format: "pcap", pos: 24}, nil
	case be.Uint32(buf) == 0xa1b2c3d4:
		return &pktReader{buf: buf, le: be, linkType: int(be.Uint32(buf[20:])), format: "pcap", pos: 24}, nil
	case le.Uint32(buf) == 0xa1b23c4d:
		return &pktReader{buf: buf, le: le, nano: true, linkType: int(le.Uint32(buf[20:])), format: "pcap-ns", pos: 24}, nil
	case be.Uint32(buf) == 0xa1b23c4d:
		return &pktReader{buf: buf, le: be, nano: true, linkType: int(be.Uint32(buf[20:])), format: "pcap-ns", pos: 24}, nil
	case le.Uint32(buf) == 0x0a0d0d0a:
		return pktOpenNG(buf)
	}
	return nil, fmt.Errorf("not a capture file: unknown magic %08x", le.Uint32(buf))
}

// pktOpenNG reads a pcapng Section Header Block plus the first Interface
// Description Block, which is where the link type lives. Uploads from Wireshark
// arrive in this format; tcpdump only writes it when asked.
func pktOpenNG(buf []byte) (*pktReader, error) {
	r := &pktReader{buf: buf, le: binary.LittleEndian, format: "pcapng", ng: true, linkType: pktLinkEther}
	if len(buf) < 12 {
		return nil, fmt.Errorf("pcapng file is truncated")
	}
	// The byte-order magic inside the SHB decides endianness for the whole file.
	if binary.LittleEndian.Uint32(buf[8:]) != 0x1a2b3c4d {
		r.le = binary.BigEndian
		if binary.BigEndian.Uint32(buf[8:]) != 0x1a2b3c4d {
			return nil, fmt.Errorf("pcapng byte-order magic is not recognisable")
		}
	}
	// Walk blocks until the first IDB to learn the link type; leave pos at the
	// start of the section so packet blocks are read normally afterwards.
	pos := 0
	for pos+12 <= len(buf) {
		typ := r.le.Uint32(buf[pos:])
		blen := int(r.le.Uint32(buf[pos+4:]))
		if blen < 12 || pos+blen > len(buf) {
			break
		}
		if typ == 0x00000001 && pos+10 <= len(buf) { // IDB
			r.linkType = int(r.le.Uint16(buf[pos+8:]))
			break
		}
		pos += blen
	}
	return r, nil
}

// next returns the next raw frame: its timestamp, the bytes captured, the original
// wire length, and the frame's offset in the buffer. ok is false at end of file.
func (r *pktReader) next() (ts time.Time, data []byte, origLen, off int, ok bool) {
	if r.ng {
		return r.nextNG()
	}
	if r.pos+16 > len(r.buf) {
		return ts, nil, 0, 0, false
	}
	h := r.buf[r.pos : r.pos+16]
	sec := int64(r.le.Uint32(h))
	frac := int64(r.le.Uint32(h[4:]))
	capLen := int(r.le.Uint32(h[8:]))
	orig := int(r.le.Uint32(h[12:]))
	if capLen < 0 || r.pos+16+capLen > len(r.buf) {
		return ts, nil, 0, 0, false // truncated final record: stop cleanly
	}
	nsec := frac * 1000
	if r.nano {
		nsec = frac
	}
	off = r.pos + 16
	r.pos = off + capLen
	return time.Unix(sec, nsec).UTC(), r.buf[off : off+capLen], orig, off, true
}

// nextNG walks pcapng blocks, returning Enhanced Packet Blocks (and legacy simple
// packet blocks) and skipping everything else.
func (r *pktReader) nextNG() (ts time.Time, data []byte, origLen, off int, ok bool) {
	for r.pos+12 <= len(r.buf) {
		typ := r.le.Uint32(r.buf[r.pos:])
		blen := int(r.le.Uint32(r.buf[r.pos+4:]))
		if blen < 12 || r.pos+blen > len(r.buf) {
			return ts, nil, 0, 0, false
		}
		body := r.buf[r.pos+8 : r.pos+blen-4]
		start := r.pos
		r.pos += blen
		switch typ {
		case 0x00000006: // Enhanced Packet Block
			if len(body) < 20 {
				continue
			}
			hi := uint64(r.le.Uint32(body[4:]))
			lo := uint64(r.le.Uint32(body[8:]))
			usec := int64(hi<<32 | lo) // default if_tsresol is 10^-6
			capLen := int(r.le.Uint32(body[12:]))
			orig := int(r.le.Uint32(body[16:]))
			if 20+capLen > len(body) {
				continue
			}
			return time.Unix(usec/1e6, (usec%1e6)*1000).UTC(),
				body[20 : 20+capLen], orig, start + 8 + 20, true
		case 0x00000003: // Simple Packet Block: original length only, no timestamp
			if len(body) < 4 {
				continue
			}
			orig := int(r.le.Uint32(body))
			n := len(body) - 4
			return time.Time{}, body[4 : 4+n], orig, start + 12, true
		}
	}
	return ts, nil, 0, 0, false
}

// ---------------------------------------------------------------- link / IP / TCP

// pktL3 is the network-layer result: the IP payload and who sent it.
type pktL3 struct {
	srcIP, dstIP string
	proto        uint8
	payload      []byte
	frag         bool
	ok           bool
}

// pktStripLink removes the link header for the capture's link type and returns the
// network-layer bytes plus the EtherType-ish protocol (0x0800 IPv4, 0x86dd IPv6).
func pktStripLink(linkType int, b []byte) (etherType uint16, rest []byte, ok bool) {
	switch linkType {
	case pktLinkEther:
		if len(b) < 14 {
			return 0, nil, false
		}
		et := binary.BigEndian.Uint16(b[12:])
		off := 14
		// 802.1Q / 802.1ad: each tag is 4 bytes and re-declares the type.
		for (et == 0x8100 || et == 0x88a8) && len(b) >= off+4 {
			et = binary.BigEndian.Uint16(b[off+2:])
			off += 4
		}
		return et, b[off:], true
	case pktLinkSLL:
		if len(b) < 16 {
			return 0, nil, false
		}
		return binary.BigEndian.Uint16(b[14:]), b[16:], true
	case pktLinkSLL2:
		if len(b) < 20 {
			return 0, nil, false
		}
		return binary.BigEndian.Uint16(b[0:]), b[20:], true
	case pktLinkNull, pktLinkLoopAlt:
		if len(b) < 4 {
			return 0, nil, false
		}
		af := binary.LittleEndian.Uint32(b)
		if linkType == pktLinkLoopAlt {
			af = binary.BigEndian.Uint32(b)
		}
		et := uint16(0x0800)
		if af == 24 || af == 28 || af == 30 { // AF_INET6 varies by OS
			et = 0x86dd
		}
		return et, b[4:], true
	case pktLinkRaw, pktLinkRawAlt:
		if len(b) == 0 {
			return 0, nil, false
		}
		if b[0]>>4 == 6 {
			return 0x86dd, b, true
		}
		return 0x0800, b, true
	}
	return 0, nil, false
}

// pktParseIP parses IPv4/IPv6 far enough to find the transport payload.
func pktParseIP(etherType uint16, b []byte) pktL3 {
	switch etherType {
	case 0x0800:
		if len(b) < 20 || b[0]>>4 != 4 {
			return pktL3{}
		}
		ihl := int(b[0]&0x0f) * 4
		total := int(binary.BigEndian.Uint16(b[2:]))
		if ihl < 20 || len(b) < ihl {
			return pktL3{}
		}
		// A snaplen-truncated frame reports more total length than was captured;
		// trust what is present.
		end := total
		if end > len(b) || end < ihl {
			end = len(b)
		}
		fragOff := binary.BigEndian.Uint16(b[6:])
		return pktL3{
			srcIP: pktIPv4(b[12:16]), dstIP: pktIPv4(b[16:20]),
			proto: b[9], payload: b[ihl:end],
			frag: fragOff&0x1fff != 0 || fragOff&0x2000 != 0, ok: true,
		}
	case 0x86dd:
		if len(b) < 40 || b[0]>>4 != 6 {
			return pktL3{}
		}
		plen := int(binary.BigEndian.Uint16(b[4:]))
		end := 40 + plen
		if end > len(b) {
			end = len(b)
		}
		return pktL3{
			srcIP: pktIPv6(b[8:24]), dstIP: pktIPv6(b[24:40]),
			proto: b[6], payload: b[40:end], ok: true,
		}
	}
	return pktL3{}
}

func pktIPv4(b []byte) string {
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}

// pktIPv6 formats an address the short way (:: for the longest zero run), which is
// what a reader expects to see next to a port.
func pktIPv6(b []byte) string {
	var groups [8]uint16
	for i := range groups {
		groups[i] = binary.BigEndian.Uint16(b[i*2:])
	}
	bestI, bestN, curI, curN := -1, 0, -1, 0
	for i, g := range groups {
		if g == 0 {
			if curI < 0 {
				curI, curN = i, 0
			}
			curN++
			if curN > bestN {
				bestI, bestN = curI, curN
			}
		} else {
			curI, curN = -1, 0
		}
	}
	var sb strings.Builder
	for i := 0; i < 8; i++ {
		if bestN > 1 && i == bestI {
			sb.WriteString("::")
			i += bestN - 1
			continue
		}
		if sb.Len() > 0 && !strings.HasSuffix(sb.String(), ":") {
			sb.WriteByte(':')
		}
		fmt.Fprintf(&sb, "%x", groups[i])
	}
	return sb.String()
}

// TCP flag bits.
const (
	tcpFIN = 1 << 0
	tcpSYN = 1 << 1
	tcpRST = 1 << 2
	tcpPSH = 1 << 3
	tcpACK = 1 << 4
	tcpURG = 1 << 5
)

type pktTCP struct {
	srcPort, dstPort int
	seq, ack         uint32
	flags            uint8
	window           int
	payload          []byte
	ok               bool
}

func pktParseTCP(b []byte) pktTCP {
	if len(b) < 20 {
		return pktTCP{}
	}
	off := int(b[12]>>4) * 4
	if off < 20 {
		return pktTCP{}
	}
	if off > len(b) {
		off = len(b) // truncated by snaplen: header only, no payload
	}
	return pktTCP{
		srcPort: int(binary.BigEndian.Uint16(b)), dstPort: int(binary.BigEndian.Uint16(b[2:])),
		seq: binary.BigEndian.Uint32(b[4:]), ack: binary.BigEndian.Uint32(b[8:]),
		flags: b[13], window: int(binary.BigEndian.Uint16(b[14:])),
		payload: b[off:], ok: true,
	}
}

// pktFlagString renders flags the way tcpdump and Wireshark do, most significant
// first, so a reader can pattern-match a handshake at a glance.
func pktFlagString(f uint8) string {
	var out []string
	for _, x := range []struct {
		bit  uint8
		name string
	}{{tcpSYN, "SYN"}, {tcpACK, "ACK"}, {tcpPSH, "PSH"}, {tcpFIN, "FIN"}, {tcpRST, "RST"}, {tcpURG, "URG"}} {
		if f&x.bit != 0 {
			out = append(out, x.name)
		}
	}
	return strings.Join(out, ",")
}

// ---------------------------------------------------------------- decode

// pktDirState is one half of a connection: what MySQL bytes are still unparsed,
// and enough sequence bookkeeping to recognise a retransmission or a dup ACK.
type pktDirState struct {
	buf     []byte // MySQL bytes not yet forming a complete packet
	nextSeq uint32
	haveSeq bool
	lastAck uint32
	lastWin int
	dupAcks int
	greeted bool // server greeting seen (s2c) / handshake response sent (c2s)
	// synced means this direction's next MySQL packet boundary is also a known
	// *semantic* boundary, so OK/ERR/result-set decoding is trustworthy. It starts
	// true only for a connection whose SYN was captured, and is then re-anchored
	// as the conversation goes: MySQL is strictly request/response, so a complete
	// client command proves the server's next packet begins a response, and a
	// completed response proves the client's next packet begins a command. That is
	// what makes a capture of an already-busy server readable instead of a wall of
	// "joined mid-connection".
	synced        bool
	inResults     bool // mid result set
	resultCol     int
	resultRow     int
	resultLen     int
	pendingDefs   int  // column/param definition packets still to be consumed quietly
	expectDefsEOF bool // a classic 5-byte EOF may follow the definition block
	binlog        bool // this direction is a binlog event stream
	// Galera state-transfer bookkeeping (pktgalera.go): how much has crossed in this
	// direction, what the stream is, and whether "this is large" was already said.
	appBytes    int
	xferStarted bool
	xferBig     bool
	xferFormat  string
}

// pktConn is the decoder's per-connection state.
type pktConn struct {
	idx            int
	client, server string // ip:port
	clientKey      string
	c2s, s2c       pktDirState
	// synced is true only when the capture contains this connection's SYN, i.e.
	// the decoder is reading the byte stream from its first byte. On a busy
	// server most connections are OLDER than the capture, and then there is no
	// greeting to read and no way to know whether the next server packet is an OK
	// or the middle of a result set — pktmysql.go decodes those conservatively
	// instead of inventing structure. Getting this wrong is what made a captured
	// binlog stream report "OK: 87024254250021057 rows affected".
	synced       bool
	tls          bool
	sslRequested bool
	tlsSealed    bool   // the handshake is encrypted: record message types are no longer readable
	compressed   bool   // CLIENT_COMPRESS negotiated: packets are zlib/zstd framed
	authPhase    bool   // between the greeting and the first OK/ERR: auth packets only
	role         string // pktRole*: which protocol this connection carries
	// A Galera state transfer is announced once per connection, not per direction:
	// data flows one way and acknowledgements the other, and both were raising the
	// "transfer started" flag on the live cluster.
	xferAnnounced bool
	xferBig       bool
	xferFormat    string
	// MySQL's packet sequence byte is per connection and shared by both directions
	// within a command's cycle — tracked here to spot a break in the framing (the
	// condition behind 1156 / 2027).
	haveSeqByte    bool
	nextSeqByte    byte
	user, database string
	version        string
	pendingCmd     string
	pendingQuery   string
	pendingTS      time.Time
	pendingOpen    bool
	synTS          time.Time
	haveSyn        bool
	synNo          int  // frame number of the client's SYN, for the post-pass
	synAcked       bool // a SYN,ACK came back
	sawData        bool // any payload in either direction
	stream         pktStream
}

// pktDecodeOpts controls a decode run.
type pktDecodeOpts struct {
	// ServerPort tells the decoder which side is the server, which is what makes
	// direction (and therefore the whole MySQL state machine) unambiguous. A
	// capture of an All-in-One instance is not on 3306 at all, so this cannot be
	// inferred from a constant.
	ServerPort int
	// PortRoles maps a server port to the protocol it carries (pktRole*). A PXC
	// capture covers 4567/4568/4444 as well as 3306, and those three are NOT the
	// MySQL protocol — running the MySQL decoder over Galera's group communication
	// would manufacture nonsense. Empty means "everything is MySQL on ServerPort".
	PortRoles  map[int]string
	MaxPackets int
}

// pktDecode turns capture bytes into annotated packets.
func pktDecode(buf []byte, opts pktDecodeOpts) (*pktDecoded, error) {
	r, err := pktOpen(buf)
	if err != nil {
		return nil, err
	}
	if opts.MaxPackets <= 0 || opts.MaxPackets > pktMaxPackets {
		opts.MaxPackets = pktMaxPackets
	}
	// Roles decide which decoder each connection gets. A caller that knows the node
	// (a capture taken here) passes them explicitly, including an All-in-One instance's
	// slot ports; anything else — an upload — gets the well-known ones, so a PXC capture
	// from somebody else's server is still read correctly.
	roles := opts.PortRoles
	if len(roles) == 0 {
		roles = pktGaleraPortRoles(opts.ServerPort)
	}
	out := &pktDecoded{LinkType: r.linkType, Format: r.format}
	conns := map[string]*pktConn{}
	var order []*pktConn
	net := newPktNetState()
	no := 0

	for {
		ts, data, origLen, off, ok := r.next()
		if !ok {
			break
		}
		no++
		if len(out.Packets) >= opts.MaxPackets {
			out.Dropped++
			continue
		}
		p := pktPacket{
			No: no, TSUnix: pktTS(ts), FrameLen: origLen, Proto: "IP", Stream: -1,
			off: off, cap: len(data),
		}
		if origLen > len(data) {
			out.Truncat++
		}
		et, l2, okLink := pktStripLink(r.linkType, data)
		if !okLink {
			p.Proto, p.Info = "?", fmt.Sprintf("unparsable link frame (%d bytes)", len(data))
			out.Packets = append(out.Packets, p)
			continue
		}
		if et == 0x0806 { // ARP — layer 2, no IP header at all
			pktARPDecode(&p, l2, net)
			p.Issues = pktDedupe(p.Issues)
			out.Packets = append(out.Packets, p)
			continue
		}
		l3 := pktParseIP(et, l2)
		if !l3.ok {
			p.Proto = pktNonIPName(et)
			p.Info = fmt.Sprintf("%s frame, %d bytes", p.Proto, origLen)
			out.Packets = append(out.Packets, p)
			continue
		}
		p.Src, p.Dst = l3.srcIP, l3.dstIP
		switch l3.proto {
		case 6: // TCP
		case 17:
			p.Proto = "UDP"
			p.Info = fmt.Sprintf("UDP %s → %s, %d bytes", l3.srcIP, l3.dstIP, len(l3.payload))
			if len(l3.payload) >= 8 {
				sp := int(binary.BigEndian.Uint16(l3.payload))
				dp := int(binary.BigEndian.Uint16(l3.payload[2:]))
				p.Src = fmt.Sprintf("%s:%d", l3.srcIP, sp)
				p.Dst = fmt.Sprintf("%s:%d", l3.dstIP, dp)
				// Name resolution: the step before every connection, and the one that
				// explains a failure no packet on the database port can.
				if sp == 53 || dp == 53 {
					pktDNSDecode(&p, l3.payload[8:], p.TSUnix, net)
					p.Issues = pktDedupe(p.Issues)
					out.Packets = append(out.Packets, p)
					continue
				}
			}
			// Galera can carry group communication over UDP multicast; label it
			// rather than leaving it as an anonymous datagram.
			if len(l3.payload) >= 8 && len(opts.PortRoles) > 0 {
				sp, dp := int(binary.BigEndian.Uint16(l3.payload)), int(binary.BigEndian.Uint16(l3.payload[2:]))
				for _, port := range []int{dp, sp} {
					if r, ok := opts.PortRoles[port]; ok && r != pktRoleMySQL {
						p.Proto = pktRoleLabel(r) + "/UDP"
						p.Info = fmt.Sprintf("%s over UDP, %d bytes", pktRoleLabel(r), len(l3.payload)-8)
						break
					}
				}
			}
			out.Packets = append(out.Packets, p)
			continue
		case 1:
			p.Proto = "ICMP"
			p.Info = pktICMPInfo(l3.payload)
			if strings.Contains(p.Info, "unreachable") {
				p.Issues = append(p.Issues, "ICMP "+p.Info)
			}
			out.Packets = append(out.Packets, p)
			continue
		default:
			p.Info = fmt.Sprintf("IP protocol %d, %d bytes", l3.proto, len(l3.payload))
			out.Packets = append(out.Packets, p)
			continue
		}

		tcp := pktParseTCP(l3.payload)
		if !tcp.ok {
			p.Proto, p.Info = "TCP", "truncated TCP header"
			out.Packets = append(out.Packets, p)
			continue
		}
		p.Proto = "TCP"
		p.Src = fmt.Sprintf("%s:%d", l3.srcIP, tcp.srcPort)
		p.Dst = fmt.Sprintf("%s:%d", l3.dstIP, tcp.dstPort)
		p.Flags, p.Seq, p.Ack, p.Window = pktFlagString(tcp.flags), tcp.seq, tcp.ack, tcp.window
		p.PayloadLen = len(tcp.payload)

		// Which end is the server, and what is it speaking? A port with a declared
		// role identifies both at once; otherwise the configured MySQL port decides,
		// and failing that the lower port wins, which is the usual convention.
		role := pktRoleMySQL
		fromClient := tcp.dstPort == opts.ServerPort ||
			(tcp.srcPort != opts.ServerPort && tcp.dstPort < tcp.srcPort)
		if r, ok := roles[tcp.dstPort]; ok {
			role, fromClient = r, true
		} else if r, ok := roles[tcp.srcPort]; ok {
			role, fromClient = r, false
		}
		var ckey, skey string
		if fromClient {
			ckey, skey = p.Src, p.Dst
		} else {
			ckey, skey = p.Dst, p.Src
		}
		key := ckey + "|" + skey
		c := conns[key]
		if c == nil {
			c = &pktConn{idx: len(order), client: ckey, server: skey, role: role}
			c.stream = pktStream{Index: c.idx, Client: ckey, Server: skey, StartTS: p.TSUnix,
				Role: role, RoleLabel: pktRoleLabel(role)}
			conns[key] = c
			order = append(order, c)
		}
		p.Stream = c.idx
		if fromClient {
			p.Dir = "c2s"
		} else {
			p.Dir = "s2c"
		}
		c.stream.Packets++
		c.stream.Bytes += origLen
		c.stream.EndTS = p.TSUnix

		dir := &c.s2c
		if fromClient {
			dir = &c.c2s
		}
		pktTCPHealth(&p, c, dir, tcp, ts)

		// Application decode runs on payload bytes only, and only for the protocol
		// this connection actually carries.
		if len(tcp.payload) > 0 && !p.hasIssue("TCP retransmission") {
			if c.role == pktRoleMySQL {
				pktAppDecode(&p, c, dir, fromClient, tcp.payload, ts)
			} else {
				pktGaleraDecode(&p, c, dir, c.role, tcp.payload)
			}
		}
		if p.Info == "" {
			p.Info = pktTCPInfo(p, tcp)
		}
		// One frame can complete several MySQL messages, each of which may raise
		// the same issue; the list is a summary, not a log.
		p.Issues = pktDedupe(p.Issues)
		out.Packets = append(out.Packets, p)
	}

	pktNetFinish(out, net)

	// A SYN with no SYN,ACK anywhere in the capture is a connection attempt that went
	// unanswered — the client waits out its connect timeout and reports 2003. This
	// can only be known once the whole capture has been read.
	for _, c := range order {
		if c.haveSyn && !c.synAcked && c.synNo > 0 {
			for i := range out.Packets {
				if out.Packets[i].No == c.synNo {
					out.Packets[i].Issues = pktDedupe(append(out.Packets[i].Issues,
						"Connection attempt unanswered — client waits its connect timeout, then 2003 CR_CONN_HOST_ERROR"))
					break
				}
			}
		}
	}
	for _, c := range order {
		c.stream.TLS = c.tls
		c.stream.User, c.stream.Database, c.stream.Version = c.user, c.database, c.version
		out.Streams = append(out.Streams, c.stream)
	}
	sort.Slice(out.Streams, func(i, j int) bool { return out.Streams[i].Index < out.Streams[j].Index })
	return out, nil
}

func pktTS(ts time.Time) float64 {
	if ts.IsZero() {
		return 0
	}
	return float64(ts.UnixNano()) / 1e9
}

func (p *pktPacket) hasIssue(s string) bool {
	for _, i := range p.Issues {
		if strings.HasPrefix(i, s) {
			return true
		}
	}
	return false
}

// pktDedupe keeps the first occurrence of each issue, preserving order.
func pktDedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func pktNonIPName(et uint16) string {
	switch et {
	case 0x0806:
		return "ARP"
	case 0x8035:
		return "RARP"
	case 0x88cc:
		return "LLDP"
	}
	return fmt.Sprintf("0x%04x", et)
}

func pktICMPInfo(b []byte) string {
	if len(b) < 1 {
		return "ICMP"
	}
	switch b[0] {
	case 0:
		return "echo reply"
	case 8:
		return "echo request"
	case 3:
		codes := map[byte]string{0: "net", 1: "host", 2: "protocol", 3: "port", 4: "fragmentation needed"}
		what := "destination"
		if len(b) > 1 {
			if c, ok := codes[b[1]]; ok {
				what = c
			}
		}
		return what + " unreachable"
	case 11:
		return "time exceeded"
	}
	return fmt.Sprintf("ICMP type %d", b[0])
}

// ---------------------------------------------------------------- TCP health

// pktTCPHealth annotates the transport-level problems a DBA actually chases: a
// retransmission (the network dropped it), a reset (someone hung up), duplicate
// ACKs (loss being signalled), a zero window (the receiver is full), and a SYN
// that nothing answered.
func pktTCPHealth(p *pktPacket, c *pktConn, dir *pktDirState, tcp pktTCP, ts time.Time) {
	n := uint32(len(tcp.payload))
	if tcp.flags&(tcpSYN|tcpFIN) != 0 {
		n++ // SYN and FIN each consume a sequence number
	}

	switch {
	case tcp.flags&tcpSYN != 0 && tcp.flags&tcpACK == 0:
		// Keep the FIRST attempt's frame number: a client retries a SYN several
		// times before giving up, and the useful thing to point at is where the
		// attempt began, not where it was abandoned.
		if !c.haveSyn {
			c.synTS, c.synNo = ts, p.No
		}
		c.haveSyn = true
		// The capture starts at this connection's first byte, so the application
		// decoder can trust its state machine from here.
		c.synced, c.c2s.synced, c.s2c.synced = true, true, true
	case tcp.flags&tcpSYN != 0 && tcp.flags&tcpACK != 0 && c.haveSyn:
		p.LagMS = float64(ts.Sub(c.synTS).Microseconds()) / 1000
		c.synAcked = true
	}
	if len(tcp.payload) > 0 {
		c.sawData = true
	}

	if dir.haveSeq {
		switch {
		case n > 0 && tcp.seq+n <= dir.nextSeq:
			// Every byte has been seen before.
			p.Issues = append(p.Issues, "TCP retransmission")
		case tcp.seq > dir.nextSeq:
			p.Issues = append(p.Issues, fmt.Sprintf("TCP gap — %d bytes missing", tcp.seq-dir.nextSeq))
			// A gap means the decoder's byte stream is broken; drop the partial
			// MySQL buffer rather than decoding across the hole.
			dir.buf = nil
		}
	}
	if !dir.haveSeq || tcp.seq+n > dir.nextSeq {
		dir.nextSeq, dir.haveSeq = tcp.seq+n, true
	}

	if tcp.flags&tcpRST != 0 {
		p.Issues = append(p.Issues, "TCP reset")
		c.stream.Reset = true
		// A reset answering a SYN is a refused connection: nothing is listening, or
		// something in between said no. The client reports this as 2003
		// CR_CONN_HOST_ERROR, a code that never crosses the wire.
		if c.haveSyn && !c.synAcked {
			p.Issues = append(p.Issues, "Connection refused — client sees 2003 CR_CONN_HOST_ERROR")
		}
	}
	// The server hanging up with a command still unanswered is what a client reports
	// as 2013 (lost during query) or 2006 (server has gone away). Both are the
	// client's words for this frame.
	if !fromClientDir(p) && tcp.flags&(tcpRST|tcpFIN) != 0 && c.pendingOpen {
		what := "2013 CR_SERVER_LOST"
		if tcp.flags&tcpFIN != 0 && tcp.flags&tcpRST == 0 {
			what = "2006 CR_SERVER_GONE_ERROR"
		}
		p.Issues = append(p.Issues,
			fmt.Sprintf("Server closed the connection with %s in flight — client sees %s", c.pendingCmd, what))
	}
	if tcp.window == 0 && tcp.flags&(tcpRST|tcpSYN) == 0 {
		p.Issues = append(p.Issues, "TCP zero window — receiver buffer full")
	}
	// A pure ACK that acknowledges nothing new, with no payload: classic dup ACK.
	if len(tcp.payload) == 0 && tcp.flags&tcpACK != 0 && tcp.flags&(tcpSYN|tcpFIN|tcpRST) == 0 {
		if dir.lastAck == tcp.ack && dir.lastWin == tcp.window {
			dir.dupAcks++
			if dir.dupAcks >= 2 {
				p.Issues = append(p.Issues, fmt.Sprintf("TCP duplicate ACK (#%d)", dir.dupAcks))
			}
		} else {
			dir.dupAcks = 0
		}
		dir.lastAck, dir.lastWin = tcp.ack, tcp.window
	}
}

// fromClientDir reports whether a packet travelled client→server. The health checks
// need the direction without threading it through every signature.
func fromClientDir(p *pktPacket) bool { return p.Dir == "c2s" }

// pktTCPInfo is the fallback one-liner for a frame with no application payload.
func pktTCPInfo(p pktPacket, tcp pktTCP) string {
	if p.Flags == "" {
		return fmt.Sprintf("TCP %d → %d", tcp.srcPort, tcp.dstPort)
	}
	s := fmt.Sprintf("[%s] seq=%d ack=%d win=%d", p.Flags, tcp.seq, tcp.ack, tcp.window)
	if len(tcp.payload) > 0 {
		s += fmt.Sprintf(" len=%d", len(tcp.payload))
	}
	return s
}
