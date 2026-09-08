package main

// pktmongo.go — the MongoDB wire protocol.
//
// Framing is the easiest of the three engines: every message is a 16-byte header
// (length including itself, requestID, responseTo, opCode) and then a body whose shape
// the opCode decides. There is no chunking, no naked negotiation byte, and no
// mid-stream ambiguity about where a message starts — the length is right there.
//
// What makes MongoDB different is the other end of the problem: **everything is on one
// port**. A Galera member separates its cluster traffic by port (4567/4568/4444) and a
// Patroni member by port (8008/2379/2380), so a capture can be classified before a
// single byte of payload is read. A MongoDB member listens on 27017 and that one port
// carries all of:
//
//	application queries and writes        a driver's connection pool
//	replica-set heartbeats                replSetHeartbeat, every 2 s between members
//	oplog tailing                          a secondary's find/getMore on local.oplog.rs
//	elections                              replSetRequestVotes, replSetStepUp
//	mongos → shard routing                 the same commands, forwarded, with shard versions
//	mongos → config server reads           config.shards, config.chunks, config.collections
//	monitoring                             hello/isMaster from every client every few seconds
//
// So classification here is by CONTENT, not by port: the command name, the database,
// the client metadata in the handshake, and the namespace. That is done in
// pktmongorepl.go, and it is the part of this decoder that makes a capture of a busy
// cluster legible instead of being 90 % heartbeats.
//
// Payloads are BSON (pktbson.go). OP_MSG (2013) is everything since MongoDB 3.6;
// OP_QUERY (2004) still appears for the very first handshake of some drivers and for
// pre-3.6 clients, and OP_COMPRESSED (2012) wraps any of them. Compression is only
// decompressed when it can be done without a new dependency — zlib and noop are the
// stdlib; snappy and zstd are named and their payload left alone, which is the same
// rule Galera's SST stream follows.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
)

// Op codes. The removed ones are still recognised: a capture may hold traffic from an
// old driver, and "OP_INSERT, removed in MongoDB 5.1" is a better answer than "TCP
// data" for somebody wondering why their application stopped working.
const (
	mongoOpReply       = 1
	mongoOpUpdate      = 2001
	mongoOpInsert      = 2002
	mongoOpQuery       = 2004
	mongoOpGetMore     = 2005
	mongoOpDelete      = 2006
	mongoOpKillCursors = 2007
	mongoOpCompressed  = 2012
	mongoOpMsg         = 2013
)

// mongoHeaderLen is the fixed message header: length, requestID, responseTo, opCode.
const mongoHeaderLen = 16

// mongoMsgDocOff is where an OP_MSG's first document begins: the 16-byte header, the
// 4-byte flag word, and the one-byte section kind. Off by that single byte, every
// validity check on a real header fails.
const mongoMsgDocOff = mongoHeaderLen + 4 + 1

// mongoClientPort is what every MongoDB process in a dbcanvas stack listens on —
// mongod members, config servers and mongos routers alike (mongodb.go's mongoPort).
const mongoClientPort = mongoPort

// pktRoleMongo is the only MongoDB role there is. Unlike Galera and Patroni, MongoDB
// has no separate cluster ports to give roles to: heartbeats, elections, oplog tailing
// and mongos→shard routing all arrive on the client port, and what a connection carries
// is decided from its content instead (pktmongorepl.go).
const pktRoleMongo = "mongodb"

// pktMongoPortRoles is the role map for a MongoDB node: one port, one role.
func pktMongoPortRoles(clientPort int) map[int]string {
	return map[int]string{clientPort: pktRoleMongo}
}

// mongoMaxMsg bounds what is accepted as a message length. The server's own limit for
// a command is 48 MB; anything past this ceiling is misframing, not a message.
const mongoMaxMsg = 128 << 20

// mongoSlowMS is when a command's response is worth flagging. MongoDB's own
// slow-query threshold is 100 ms, so the capture agrees with the server's log.
const mongoSlowMS = 100.0

// mongoBigResultBytes is when a reply is worth calling heavy.
const mongoBigResultBytes = 1 << 20

// mongoStallBytes is how much unparsed payload in one direction means the anchor was
// wrong rather than the message being large. The server's own command limit is 48 MB, so
// this is generous by a factor of four for a real message and still bounded.
const mongoStallBytes = 4 << 20

// mongoStallFrames is how many frames may add payload without completing the message the
// anchor claims before the anchor is treated as wrong. A real message spanning more than
// this many segments is possible, so the check also requires the claimed length to be
// larger than everything buffered so far by a wide margin — see pktNextMongo.
const mongoStallFrames = 24

// mongoMidStreamMax bounds the length a *guessed* anchor may claim. A connection whose
// SYN was captured is trusted with anything up to mongoMaxMsg; one that was joined
// mid-stream is not, because a false header inside a compressed or truncated body can
// claim megabytes and stall the direction. Losing one real large message and
// re-anchoring on the next is much better than losing the rest of the connection.
const mongoMidStreamMax = 1 << 20

// pktMongoDir is one direction's MongoDB state.
type pktMongoDir struct {
	aligned  bool // the next byte in the buffer starts a message
	desynced bool // framing was lost at least once
	// waiting counts consecutive frames that added payload without completing the
	// message the current anchor claims. A mid-stream anchor is a guess, and a guess
	// that lands inside a compressed body can claim a length the connection will never
	// produce — after which the direction waits forever and its buffer grows. Counting
	// the wait is what turns that into a re-anchor instead of a dead connection.
	waiting int
}

// pktMongoConn is per-connection MongoDB state.
type pktMongoConn struct {
	// Requests waiting for a reply, by requestID. MongoDB is not request/response in
	// order — a driver pipelines and the server may answer out of order — so a reply
	// is matched by its responseTo field rather than by position.
	pending map[int32]mongoPending
	// What this connection is for, decided from its content (pktmongorepl.go).
	kind    string
	sawMsg  bool // at least one MongoDB message has been decoded on this connection
	appName string
	driver  string
	// Cursor bookkeeping: a getMore names a cursor id, and the namespace it belongs to
	// came from the find that opened it — which may be the only place it appears.
	cursors map[int64]string
	// Session and transaction state, which is how a retryable write or a multi-document
	// transaction shows up on the wire.
	inTxn      bool
	txnNumber  int64
	txnNoted   bool
	authUser   string
	authMech   string
	compressor string
	opsSeen    int
	heartbeats int
}

// mongoPending is a request awaiting its reply.
type mongoPending struct {
	command string
	ns      string
	ts      time.Time
	summary string
}

func (c *pktConn) mongoConn() *pktMongoConn {
	if c.mongo == nil {
		c.mongo = &pktMongoConn{pending: map[int32]mongoPending{}, cursors: map[int64]string{}}
	}
	return c.mongo
}

func (d *pktDirState) mongoDir() *pktMongoDir {
	if d.mongo == nil {
		d.mongo = &pktMongoDir{}
	}
	return d.mongo
}

// ---------------------------------------------------------------- entry point

// pktMongoDecode consumes one frame's payload for a connection direction. Same
// contract as pktAppDecode and pktPGDecode, so pktdecode.go dispatches on role alone.
func pktMongoDecode(p *pktPacket, c *pktConn, dir *pktDirState, fromClient bool, payload []byte, ts time.Time) {
	mc := c.mongoConn()
	md := dir.mongoDir()

	// Encrypted, and therefore over.
	if c.tls {
		p.Proto = "TLS"
		p.Info = pktTLSInfo(payload, c.tlsSealed)
		p.Status = "Encrypted"
		if strings.Contains(p.Info, "Alert") {
			p.Issues = append(p.Issues,
				"TLS alert — handshake or session rejected; the driver reports an SSL error and the connection ends")
		}
		if !c.tlsSealed && pktTLSSeals(payload) {
			c.tlsSealed = true
		}
		return
	}
	// MongoDB has no in-band upgrade: TLS either starts the connection or never
	// happens. A ClientHello on the first bytes is therefore unambiguous.
	if !mc.sawMsg && pktLooksTLS(payload) {
		c.tls, c.tlsSealed = true, pktTLSSeals(payload)
		p.Proto, p.Status = "TLS", "Encrypted"
		p.Info = pktTLSInfo(payload, false)
		return
	}

	dir.buf = append(dir.buf, payload...)
	var infos []string
	msgs, sawMongo := 0, false
	for {
		hdr, body, ok := pktNextMongo(c, dir)
		if !ok {
			break
		}
		msgs++
		sawMongo, mc.sawMsg = true, true
		if info := pktMongoMessage(p, c, dir, hdr, body, fromClient, ts); info != "" {
			infos = append(infos, info)
		}
	}
	if sawMongo {
		// Not just "MongoDB": the kind, so the packet list separates a heartbeat from a
		// query without anybody having to read the Info column. On a real member most
		// rows are NOT the application.
		p.Proto = mongoKindLabel(mc.kind)
	}
	switch {
	case len(infos) > 0:
		p.Info = strings.Join(infos, " | ")
		if len(infos) > 3 {
			p.Info = fmt.Sprintf("%s | +%d more", strings.Join(infos[:3], " | "), len(infos)-3)
		}
	case msgs > 0:
		p.Info = fmt.Sprintf("MongoDB data, %d bytes in %d message(s)", len(payload), msgs)
	case !md.aligned && !c.synced:
		p.Proto, p.Status = "MongoDB", "Unknown"
		p.Info = fmt.Sprintf(
			"[capture joined mid-connection] %d bytes, waiting for a message boundary", len(payload))
	case md.desynced:
		p.Proto = "MongoDB"
		p.Info = fmt.Sprintf("[framing lost] %d bytes, hunting for the next message header", len(payload))
	case len(dir.buf) > 0:
		p.Proto = "MongoDB"
		p.Info = fmt.Sprintf("[continuation] %d bytes, %d buffered", len(payload), len(dir.buf))
	}
}

// mongoHeader is a decoded message header.
type mongoHeader struct {
	Length     int
	RequestID  int32
	ResponseTo int32
	OpCode     int32
}

// pktNextMongo pulls one complete message out of a direction's buffer.
//
// A connection whose SYN was captured is aligned from its first byte. One that was
// joined mid-stream has to find a header, and MongoDB gives a good test for that: four
// little-endian int32s where the first is a sane length and the fourth is a known op
// code. An op code is 1 or in 2001–2013, which is a narrow enough target that a false
// positive needs 8 specific bytes to line up.
func pktNextMongo(c *pktConn, dir *pktDirState) (mongoHeader, []byte, bool) {
	md := dir.mongoDir()
	if c.synced {
		md.aligned = true
	}
	for {
		if !c.synced && !md.aligned {
			if !pktMongoAnchor(dir, mongoMidStreamMax) {
				return mongoHeader{}, nil, false
			}
			md.aligned, md.waiting = true, 0
		}
		if len(dir.buf) < mongoHeaderLen {
			return mongoHeader{}, nil, false
		}
		h := mongoHeader{
			Length:     int(int32(binary.LittleEndian.Uint32(dir.buf))),
			RequestID:  int32(binary.LittleEndian.Uint32(dir.buf[4:])),
			ResponseTo: int32(binary.LittleEndian.Uint32(dir.buf[8:])),
			OpCode:     int32(binary.LittleEndian.Uint32(dir.buf[12:])),
		}
		if h.Length < mongoHeaderLen || h.Length > mongoMaxMsg || !mongoKnownOp(h.OpCode) {
			md.aligned, md.desynced = false, true
			dir.buf = dir.buf[1:]
			if !pktMongoAnchor(dir, mongoMidStreamMax) {
				return mongoHeader{}, nil, false
			}
			md.aligned, md.waiting = true, 0
			continue
		}
		if len(dir.buf) < h.Length {
			// A false anchor claims a length the connection will never produce, and then
			// this direction waits forever while its buffer grows. Two guards, because
			// one is not enough on real traffic: an absolute byte ceiling, and a count of
			// frames spent waiting for a message that is still far from complete. The
			// second is the one that fires in practice — a bogus header found inside a
			// compressed body claims a few hundred kilobytes, not gigabytes.
			md.waiting++
			hopeless := md.waiting > mongoStallFrames && h.Length > len(dir.buf)*2
			if (len(dir.buf) > mongoStallBytes && h.Length > mongoStallBytes) || hopeless {
				md.aligned, md.desynced, md.waiting = false, true, 0
				dir.buf = dir.buf[1:]
				continue
			}
			return mongoHeader{}, nil, false
		}
		md.waiting = 0
		body := dir.buf[mongoHeaderLen:h.Length]
		dir.buf = dir.buf[h.Length:]
		if len(dir.buf) == 0 {
			dir.buf = nil
		}
		return h, body, true
	}
}

// pktMongoAnchor finds a plausible message header and drops everything before it.
func pktMongoAnchor(dir *pktDirState, max int) bool {
	buf := dir.buf
	for i := 0; i+mongoHeaderLen <= len(buf); i++ {
		n := int(int32(binary.LittleEndian.Uint32(buf[i:])))
		if n < mongoHeaderLen || n > max {
			continue
		}
		op := int32(binary.LittleEndian.Uint32(buf[i+12:]))
		if !mongoKnownOp(op) {
			continue
		}
		// A length alone is far too weak a signal: four bytes that happen to be a small
		// number plus four that happen to be 2013 will match somewhere in any large
		// buffer, and a false anchor with a big length stalls the direction forever
		// while it waits for bytes that were never a message. So the candidate has to
		// survive two more checks, both of which a real header passes trivially:
		//
		//   - its body has to start the way its opcode says (OP_MSG: a flag word then a
		//     BSON document whose own length fits inside the message), and
		//   - if the buffer already holds the whole message, whatever follows it has to
		//     look like another header — messages come in streams, not alone.
		switch op {
		case mongoOpMsg:
			// The command document does NOT start right after the flag word: there is a
			// section-kind byte between them (0 for the body, 1 for a document
			// sequence). Reading the length one byte early rejected every genuine
			// header, which on a mid-stream connection meant anchoring on a false
			// positive further in and then waiting forever for a message that was never
			// there — 2 000 frames of it on a live replica set.
			if i+mongoMsgDocOff+4 <= len(buf) {
				doc := buf[i+mongoMsgDocOff:]
				if !bsonDocOK(doc) {
					continue
				}
				if dl := int(int32(binary.LittleEndian.Uint32(doc))); dl+mongoMsgDocOff-mongoHeaderLen > n-mongoHeaderLen {
					continue // the document claims more room than the message has
				}
			}
		case mongoOpReply:
			// OP_REPLY's own fields are checkable, and worth checking: a run of BSON
			// bytes matched "length + opcode 1" often enough to produce a reply
			// claiming 1.6 billion documents.
			if i+36 <= len(buf) {
				numReturned := int(int32(binary.LittleEndian.Uint32(buf[i+mongoHeaderLen+16:])))
				if numReturned < 0 || numReturned > 1<<20 {
					continue
				}
				if !bsonDocOK(buf[i+mongoHeaderLen+20:]) {
					continue
				}
			}
		}
		if end := i + n; end+mongoHeaderLen <= len(buf) {
			nn := int(int32(binary.LittleEndian.Uint32(buf[end:])))
			nop := int32(binary.LittleEndian.Uint32(buf[end+12:]))
			if nn < mongoHeaderLen || nn > mongoMaxMsg || !mongoKnownOp(nop) {
				continue
			}
		}
		dir.buf = buf[i:]
		return true
	}
	if len(dir.buf) > 1<<20 {
		dir.buf = dir.buf[len(dir.buf)-(1<<16):]
	}
	return false
}

func mongoKnownOp(op int32) bool {
	switch op {
	case mongoOpReply, mongoOpUpdate, mongoOpInsert, mongoOpQuery, mongoOpGetMore,
		mongoOpDelete, mongoOpKillCursors, mongoOpCompressed, mongoOpMsg:
		return true
	}
	return false
}

func mongoOpName(op int32) string {
	switch op {
	case mongoOpReply:
		return "OP_REPLY"
	case mongoOpUpdate:
		return "OP_UPDATE"
	case mongoOpInsert:
		return "OP_INSERT"
	case mongoOpQuery:
		return "OP_QUERY"
	case mongoOpGetMore:
		return "OP_GET_MORE"
	case mongoOpDelete:
		return "OP_DELETE"
	case mongoOpKillCursors:
		return "OP_KILL_CURSORS"
	case mongoOpCompressed:
		return "OP_COMPRESSED"
	case mongoOpMsg:
		return "OP_MSG"
	}
	return fmt.Sprintf("opcode %d", op)
}

// ---------------------------------------------------------------- one message

// pktMongoMessage decodes one message and returns its one-line summary.
func pktMongoMessage(p *pktPacket, c *pktConn, dir *pktDirState, h mongoHeader, body []byte,
	fromClient bool, ts time.Time) string {

	switch h.OpCode {
	case mongoOpCompressed:
		return pktMongoCompressed(p, c, dir, h, body, fromClient, ts)
	case mongoOpMsg:
		return pktMongoMsg(p, c, dir, h, body, fromClient, ts)
	case mongoOpQuery:
		return pktMongoQuery(p, c, h, body, ts)
	case mongoOpReply:
		return pktMongoReply(p, c, h, body, ts)
	case mongoOpGetMore, mongoOpKillCursors, mongoOpInsert, mongoOpUpdate, mongoOpDelete:
		// Removed from the server in 5.1 and from every supported driver long before.
		// Seeing one is itself the finding.
		name := mongoOpName(h.OpCode)
		p.Command = name
		p.Issues = append(p.Issues, fmt.Sprintf(
			"%s — a legacy opcode removed in MongoDB 5.1; a driver still sending these will fail against any current server", name))
		return fmt.Sprintf("%s (legacy), %d bytes", name, len(body))
	}
	return fmt.Sprintf("%s, %d bytes", mongoOpName(h.OpCode), len(body))
}

// pktMongoCompressed unwraps OP_COMPRESSED. The wrapper is: original op code,
// uncompressed size, compressor id, then the compressed body.
func pktMongoCompressed(p *pktPacket, c *pktConn, dir *pktDirState, h mongoHeader, body []byte,
	fromClient bool, ts time.Time) string {

	if len(body) < 9 {
		return "OP_COMPRESSED (truncated)"
	}
	inner := int32(binary.LittleEndian.Uint32(body))
	size := int(int32(binary.LittleEndian.Uint32(body[4:])))
	comp := body[8]
	data := body[9:]
	name := mongoCompressorName(comp)
	c.mongoConn().compressor = name

	// Three of the four compressors can be read: noop is not compression at all, zlib is
	// in the standard library, and snappy's block format is small enough to implement
	// (pktsnappy.go) — which matters because snappy is what MongoDB actually negotiates
	// by default, so leaving it undecoded would leave most of a real capture undecoded.
	var plain []byte
	switch comp {
	case 0:
		if size >= 0 && size <= len(data) {
			plain = data[:size]
		}
	case 1:
		if out, err := snappyDecode(data, size); err == nil {
			plain = out
		}
	case 2:
		if out, err := mongoInflate(data, size); err == nil {
			plain = out
		}
	}
	if plain != nil {
		return fmt.Sprintf("[%s] ", name) +
			pktMongoMessage(p, c, dir, mongoHeader{Length: mongoHeaderLen + len(plain),
				RequestID: h.RequestID, ResponseTo: h.ResponseTo, OpCode: inner}, plain, fromClient, ts)
	}
	// zstd remains described rather than decoded: it is Huffman plus FSE plus a
	// dictionary format, which is a dependency, not a function. Same for anything that
	// failed to decompress — saying so beats printing what a bad guess produced.
	p.Status = "Compressed"
	return fmt.Sprintf("%s compressed with %s, %s on the wire → %s uncompressed (not decoded)",
		mongoOpName(inner), name, pktBytes(len(data)), pktBytes(size))
}

func mongoCompressorName(id byte) string {
	switch id {
	case 0:
		return "noop"
	case 1:
		return "snappy"
	case 2:
		return "zlib"
	case 3:
		return "zstd"
	}
	return fmt.Sprintf("compressor %d", id)
}

// mongoInflate decompresses a zlib payload, refusing to allocate more than the
// declared size — a corrupted length field must not turn into a memory problem.
func mongoInflate(data []byte, size int) ([]byte, error) {
	if size < 0 || size > mongoMaxMsg {
		return nil, fmt.Errorf("implausible uncompressed size %d", size)
	}
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := make([]byte, 0, size)
	buf := bytes.NewBuffer(out)
	if _, err := io.CopyN(buf, r, int64(size)); err != nil && err != io.EOF {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pktMongoMsg decodes OP_MSG, which is every command since MongoDB 3.6.
//
// Sections: kind 0 is the command document; kind 1 is a document sequence (the
// documents of an insert, the updates of an update) hoisted out of the command so the
// server can stream them. A bulk insert of 10 000 documents is one kind-1 section, and
// counting it beats printing it.
func pktMongoMsg(p *pktPacket, c *pktConn, dir *pktDirState, h mongoHeader, body []byte,
	fromClient bool, ts time.Time) string {

	if len(body) < 4 {
		return "OP_MSG (truncated)"
	}
	flags := binary.LittleEndian.Uint32(body)
	rest := body[4:]
	// checksumPresent trims a 4-byte CRC off the end.
	if flags&0x1 != 0 && len(rest) >= 4 {
		rest = rest[:len(rest)-4]
	}
	var cmdDoc []bsonElem
	seqCount, seqName, seqBytes := 0, "", 0
	for len(rest) > 0 {
		kind := rest[0]
		rest = rest[1:]
		if kind == 0 {
			if !bsonDocOK(rest) {
				elems, _ := bsonElems(rest)
				cmdDoc = elems
				break
			}
			n := int(int32(binary.LittleEndian.Uint32(rest)))
			elems, _ := bsonElems(rest[:n])
			cmdDoc = elems
			rest = rest[n:]
			continue
		}
		if kind == 1 {
			if len(rest) < 5 {
				break
			}
			n := int(int32(binary.LittleEndian.Uint32(rest)))
			if n < 5 || n > len(rest) {
				break
			}
			seg := rest[4:n]
			name, docs, _ := bsonCString(seg)
			seqName, seqBytes = name, len(seg)
			// Count the documents without decoding them.
			for len(docs) >= 4 {
				dn := int(int32(binary.LittleEndian.Uint32(docs)))
				if dn < 5 || dn > len(docs) {
					break
				}
				seqCount++
				docs = docs[dn:]
			}
			rest = rest[n:]
			continue
		}
		break // unknown section kind: stop rather than guess
	}
	if len(cmdDoc) == 0 {
		return fmt.Sprintf("OP_MSG, %d bytes (no readable command document)", len(body))
	}

	// Which side this is comes from the message itself, not from the TCP direction.
	// responseTo is 0 in a request and the request's id in a reply — and that matters
	// because a mongos↔shard connection is 27017 to 27017, where no port comparison can
	// say which end is the server. Trusting the header makes the decode correct for
	// every topology, including a capture taken on the router.
	if h.ResponseTo == 0 {
		return pktMongoCommand(p, c, h, cmdDoc, seqName, seqCount, seqBytes, ts)
	}
	return pktMongoResponse(p, c, h, cmdDoc, len(body), ts)
}

// pktMongoCommand reads a command document: its name (the first key, by MongoDB's own
// rule), its namespace, and the fields worth putting on one line.
func pktMongoCommand(p *pktPacket, c *pktConn, h mongoHeader, doc []bsonElem,
	seqName string, seqCount, seqBytes int, ts time.Time) string {

	mc := c.mongoConn()
	cmd := doc[0].Key
	// Commands that carry application intent are counted as queries, so the summary's
	// "queries" number means the same thing it does for the other two engines. A
	// heartbeat or a hello is traffic, not work.
	if !mongoIsChatter(cmd) {
		c.stream.Queries++
	}
	db := bsonStr(mustGet(doc, "$db"))
	coll := ""
	if s := bsonStr(doc[0]); s != "" && doc[0].Type == bsonString {
		coll = s
	}
	ns := db
	if coll != "" {
		ns = db + "." + coll
	}
	mc.opsSeen++

	// The namespace has to be complete BEFORE classifying, because for the two commands
	// that name their collection separately it is the namespace that decides what the
	// connection is. A getMore carries "getMore: <cursor>, collection: oplog.rs, $db:
	// local" — and classifying on "$db" alone made every oplog tail on the testbed look
	// like an ordinary read, which is the one connection kind most worth naming.
	switch cmd {
	case "getMore":
		if e, ok := bsonGet(doc, "getMore"); ok {
			if id, ok2 := bsonInt(e); ok2 {
				if known := mc.cursors[id]; known != "" {
					ns = known
				}
			}
		}
		if s := bsonStr(mustGet(doc, "collection")); s != "" {
			ns = db + "." + s
		}
	case "killCursors":
		if s := bsonStr(mustGet(doc, "killCursors")); s != "" {
			ns = db + "." + s
		}
	}

	// What kind of connection this is, decided from the command, the namespace and the
	// handshake metadata.
	pktMongoClassify(p, c, cmd, ns, doc)

	// Session and transaction state.
	if e, ok := bsonGet(doc, "txnNumber"); ok {
		if v, ok2 := bsonInt(e); ok2 {
			mc.txnNumber = v
		}
	}
	if e, ok := bsonGet(doc, "startTransaction"); ok {
		if v, _ := bsonInt(e); v != 0 {
			mc.inTxn, mc.txnNoted = true, false
		}
	}

	p.Command, p.Query = cmd, ns
	mc.pending[h.RequestID] = mongoPending{command: cmd, ns: ns, ts: ts}

	// The line itself: command, namespace, and the arguments that matter for the
	// command in question rather than every field it carries.
	out := cmd
	if ns != "" {
		out += " " + ns
	}
	if detail := mongoCommandDetail(cmd, doc, seqName, seqCount, seqBytes); detail != "" {
		out += " — " + detail
	}
	// On a routed command, the shard version is the field the shard checks before
	// answering, and a mismatch is what produces StaleConfig. Saying that it is present
	// (and which shard generation it claims) is what makes a routed row readable as
	// routing rather than as an ordinary command that happens to be forwarded.
	if e, ok := bsonGet(doc, "shardVersion"); ok {
		if v, ok2 := bsonPath(bsonSub(e), "t"); ok2 {
			out += " [shardVersion " + bsonValue(v, 0) + "]"
		} else {
			out += " [shardVersion present]"
		}
	}
	if mc.inTxn && !mc.txnNoted {
		mc.txnNoted = true
		out += fmt.Sprintf(" [transaction %d begins]", mc.txnNumber)
	}
	return out
}

// mongoCommandDetail is the per-command part of the summary. Only the commands a
// capture is actually full of get their own treatment; the rest are summarised
// generically, which is honest and still readable.
func mongoCommandDetail(cmd string, doc []bsonElem, seqName string, seqCount, seqBytes int) string {
	get := func(k string) (bsonElem, bool) { return bsonGet(doc, k) }
	switch cmd {
	case "find":
		var parts []string
		if e, ok := get("filter"); ok {
			if sub := bsonSub(e); len(sub) > 0 {
				parts = append(parts, "filter "+bsonValue(e, 1))
			} else {
				parts = append(parts, "no filter (full scan of the collection)")
			}
		}
		if e, ok := get("sort"); ok {
			parts = append(parts, "sort "+bsonValue(e, 1))
		}
		if e, ok := get("limit"); ok {
			if v, _ := bsonInt(e); v != 0 {
				parts = append(parts, fmt.Sprintf("limit %d", v))
			}
		}
		if e, ok := get("batchSize"); ok {
			if v, _ := bsonInt(e); v != 0 {
				parts = append(parts, fmt.Sprintf("batch %d", v))
			}
		}
		if e, ok := get("tailable"); ok {
			if v, _ := bsonInt(e); v != 0 {
				parts = append(parts, "tailable")
			}
		}
		return strings.Join(parts, ", ")
	case "insert":
		if seqCount > 0 {
			return fmt.Sprintf("%d document(s) in a %q sequence, %s", seqCount, seqName, pktBytes(seqBytes))
		}
		if e, ok := get("documents"); ok {
			return fmt.Sprintf("%d document(s)", len(bsonSub(e)))
		}
	case "update":
		n := seqCount
		if n == 0 {
			if e, ok := get("updates"); ok {
				n = len(bsonSub(e))
			}
		}
		return fmt.Sprintf("%d update(s)", n)
	case "delete":
		n := seqCount
		if n == 0 {
			if e, ok := get("deletes"); ok {
				n = len(bsonSub(e))
			}
		}
		return fmt.Sprintf("%d delete(s)", n)
	case "aggregate":
		if e, ok := get("pipeline"); ok {
			stages := bsonSub(e)
			var names []string
			for i, st := range stages {
				if i >= 4 {
					names = append(names, fmt.Sprintf("…+%d", len(stages)-4))
					break
				}
				if sub := bsonSub(st); len(sub) > 0 {
					names = append(names, sub[0].Key)
				}
			}
			return fmt.Sprintf("%d-stage pipeline: %s", len(stages), strings.Join(names, " → "))
		}
	case "getMore":
		var parts []string
		if e, ok := get("getMore"); ok {
			if v, _ := bsonInt(e); v != 0 {
				parts = append(parts, fmt.Sprintf("cursor %d", v))
			}
		}
		if e, ok := get("maxTimeMS"); ok {
			if v, _ := bsonInt(e); v != 0 {
				parts = append(parts, fmt.Sprintf("maxTimeMS %d", v))
			}
		}
		return strings.Join(parts, ", ")
	case "findAndModify":
		var parts []string
		if e, ok := get("query"); ok {
			parts = append(parts, "query "+bsonValue(e, 1))
		}
		if _, ok := get("remove"); ok {
			parts = append(parts, "remove")
		}
		if _, ok := get("update"); ok {
			parts = append(parts, "update")
		}
		return strings.Join(parts, ", ")
	case "hello", "isMaster", "ismaster":
		var parts []string
		if e, ok := get("client"); ok {
			if sub := bsonSub(e); len(sub) > 0 {
				if d, ok2 := bsonGet(sub, "driver"); ok2 {
					dd := bsonSub(d)
					name, ver := bsonStr(mustGet(dd, "name")), bsonStr(mustGet(dd, "version"))
					parts = append(parts, "driver "+strings.TrimSpace(name+" "+ver))
				}
				if a, ok2 := bsonGet(sub, "application"); ok2 {
					if app := bsonStr(mustGet(bsonSub(a), "name")); app != "" {
						parts = append(parts, "app "+app)
					}
				}
			}
		}
		if e, ok := get("topologyVersion"); ok {
			_ = e
			parts = append(parts, "streaming (awaitable)")
		}
		if e, ok := get("maxAwaitTimeMS"); ok {
			if v, _ := bsonInt(e); v != 0 {
				parts = append(parts, fmt.Sprintf("maxAwaitTimeMS %d", v))
			}
		}
		return strings.Join(parts, ", ")
	case "saslStart", "saslContinue":
		var parts []string
		if s := bsonStr(mustGet(doc, "mechanism")); s != "" {
			parts = append(parts, s)
		}
		return strings.Join(parts, ", ")
	case "createIndexes":
		if e, ok := get("indexes"); ok {
			return fmt.Sprintf("%d index(es)", len(bsonSub(e)))
		}
	}
	// Everything else: the first few fields, minus the ones every message carries.
	if s := bsonSummary(doc[1:], 3); s != "" {
		return s
	}
	return ""
}

// mustGet is bsonGet without the second return, for the many places where a missing
// field simply means "not applicable".
func mustGet(elems []bsonElem, key string) bsonElem {
	e, _ := bsonGet(elems, key)
	return e
}

// pktMongoResponse reads a reply document: ok/errmsg/code, the counts a write returns,
// and the cursor a read returns.
func pktMongoResponse(p *pktPacket, c *pktConn, h mongoHeader, doc []bsonElem, size int, ts time.Time) string {
	mc := c.mongoConn()
	req, hadReq := mc.pending[h.ResponseTo]
	if hadReq {
		delete(mc.pending, h.ResponseTo)
	}
	lag := 0.0
	if hadReq && !req.ts.IsZero() {
		lag = ts.Sub(req.ts).Seconds() * 1000
		p.LagMS = lag
	}

	okVal, hasOK := bsonInt(mustGet(doc, "ok"))
	errmsg := bsonStr(mustGet(doc, "errmsg"))
	code, hasCode := bsonInt(mustGet(doc, "code"))
	codeName := bsonStr(mustGet(doc, "codeName"))

	// A failed command is the interesting case, and MongoDB reports it in the reply
	// body rather than by any transport-level signal: ok: 0 with a code.
	if (hasOK && okVal == 0) || errmsg != "" {
		c.stream.Errors++
		p.ErrCode = int(code)
		name := codeName
		if name == "" && hasCode {
			name = mongoCodeName(int(code))
		}
		p.Status = fmt.Sprintf("Error %d (%s): %s", code, name, pktEllipsis(errmsg, 110))
		if iss := mongoErrIssue(int(code), name, errmsg, mc); iss != "" {
			p.Issues = append(p.Issues, iss)
		}
		out := fmt.Sprintf("error %d %s: %s", code, name, pktEllipsis(errmsg, 130))
		if hadReq {
			out = req.command + " → " + out
		}
		return out
	}

	// A write can succeed at the command level and still have failed for individual
	// documents — writeErrors is where a duplicate key actually appears.
	if e, ok := bsonGet(doc, "writeErrors"); ok {
		errs := bsonSub(e)
		if len(errs) > 0 {
			first := bsonSub(errs[0])
			wcode, _ := bsonInt(mustGet(first, "code"))
			wmsg := bsonStr(mustGet(first, "errmsg"))
			c.stream.Errors++
			p.ErrCode = int(wcode)
			p.Status = fmt.Sprintf("Write error %d: %s", wcode, pktEllipsis(wmsg, 110))
			if iss := mongoErrIssue(int(wcode), mongoCodeName(int(wcode)), wmsg, mc); iss != "" {
				p.Issues = append(p.Issues, iss)
			}
			return fmt.Sprintf("%s → %d write error(s), first %d %s: %s",
				reqName(req, hadReq), len(errs), wcode, mongoCodeName(int(wcode)), pktEllipsis(wmsg, 90))
		}
	}
	// writeConcernError is separate again: the write happened, but not durably enough.
	if e, ok := bsonGet(doc, "writeConcernError"); ok {
		sub := bsonSub(e)
		wcode, _ := bsonInt(mustGet(sub, "code"))
		wmsg := bsonStr(mustGet(sub, "errmsg"))
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Write concern not satisfied (%d %s): %s — the write was applied on this member but not acknowledged by enough of the replica set, so it may still be rolled back if this primary steps down",
			wcode, mongoCodeName(int(wcode)), pktEllipsis(wmsg, 90)))
		return fmt.Sprintf("%s → ok, but write concern failed: %s", reqName(req, hadReq), pktEllipsis(wmsg, 90))
	}

	p.Status = "Success"
	var parts []string
	// Cursor replies: the id, the batch, and the namespace, which is where a getMore's
	// namespace comes from later.
	if e, ok := bsonGet(doc, "cursor"); ok {
		cur := bsonSub(e)
		id, _ := bsonInt(mustGet(cur, "id"))
		ns := bsonStr(mustGet(cur, "ns"))
		batchKey := "firstBatch"
		if _, ok2 := bsonGet(cur, "nextBatch"); ok2 {
			batchKey = "nextBatch"
		}
		n := len(bsonSub(mustGet(cur, batchKey)))
		if id != 0 && ns != "" {
			mc.cursors[id] = ns
		}
		p.Rows = n
		switch {
		case id != 0:
			parts = append(parts, fmt.Sprintf("%d doc(s) in %s, cursor %d stays open", n, batchKey, id))
		default:
			parts = append(parts, fmt.Sprintf("%d doc(s) in %s, cursor exhausted", n, batchKey))
		}
	}
	// Write replies.
	for _, k := range []string{"n", "nModified", "nUpserted", "nMatched", "nRemoved", "nInserted"} {
		if e, ok := bsonGet(doc, k); ok {
			if v, ok2 := bsonInt(e); ok2 {
				parts = append(parts, fmt.Sprintf("%s=%d", k, v))
				if k == "n" {
					p.Rows = int(v)
				}
			}
		}
	}
	// A hello reply says what this member thinks it is, which is the single most
	// useful fact in a replica-set capture.
	if s := mongoHelloSummary(doc); s != "" {
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		if s := bsonSummary(doc, 3); s != "" {
			parts = append(parts, s)
		}
	}
	if size >= mongoBigResultBytes {
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Heavy reply — %s in one message; the driver waits for all of it and the server holds it in memory while writing it",
			pktBytes(size)))
	}
	if lag >= mongoSlowMS && hadReq && !mongoAwaitable(req.command) {
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Slow response — %.0f ms from the %s request to its reply", lag, req.command))
	}
	out := strings.Join(parts, ", ")
	if hadReq {
		out = req.command + " → " + out
	} else {
		out = "reply → " + out
	}
	if lag > 0 {
		out += fmt.Sprintf(" (%.1f ms)", lag)
	}
	return out
}

func reqName(req mongoPending, had bool) string {
	if had {
		return req.command
	}
	return "reply"
}

// mongoIsChatter reports whether a command is protocol overhead rather than work: a
// replica set's heartbeats and every driver's topology monitoring, which on a real
// member outnumber the application's commands many times over.
func mongoIsChatter(cmd string) bool {
	switch cmd {
	case "hello", "isMaster", "ismaster", "ping", "replSetHeartbeat", "replSetUpdatePosition",
		"buildInfo", "getParameter", "whatsmyuri", "endSessions", "listDatabases",
		"connectionStatus", "getLog", "atlasVersion", "topology":
		return true
	}
	return false
}

// mongoAwaitable reports whether a command is *supposed* to take a long time: a
// streaming hello and a tailing getMore both block on the server on purpose, and
// flagging them as slow would be flagging the design.
func mongoAwaitable(cmd string) bool {
	switch cmd {
	case "hello", "isMaster", "ismaster", "getMore", "replSetHeartbeat", "awaitData", "aggregate":
		return true
	}
	return false
}

// mongoHelloSummary pulls the topology facts out of a hello/isMaster reply.
func mongoHelloSummary(doc []bsonElem) string {
	var parts []string
	if e, ok := bsonGet(doc, "isWritablePrimary"); ok {
		if v, _ := bsonInt(e); v != 0 {
			parts = append(parts, "PRIMARY")
		} else {
			parts = append(parts, "not primary")
		}
	} else if e, ok := bsonGet(doc, "ismaster"); ok {
		if v, _ := bsonInt(e); v != 0 {
			parts = append(parts, "PRIMARY")
		} else {
			parts = append(parts, "not primary")
		}
	}
	if e, ok := bsonGet(doc, "secondary"); ok {
		if v, _ := bsonInt(e); v != 0 {
			parts = append(parts, "secondary")
		}
	}
	if s := bsonStr(mustGet(doc, "setName")); s != "" {
		parts = append(parts, "set "+s)
	}
	if s := bsonStr(mustGet(doc, "primary")); s != "" {
		parts = append(parts, "primary is "+s)
	}
	if s := bsonStr(mustGet(doc, "msg")); s == "isdbgrid" {
		parts = append(parts, "this is a mongos router")
	}
	if e, ok := bsonGet(doc, "arbiterOnly"); ok {
		if v, _ := bsonInt(e); v != 0 {
			parts = append(parts, "arbiter")
		}
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------- legacy opcodes

// pktMongoQuery decodes OP_QUERY, which still carries the first handshake of some
// drivers (and every pre-3.6 client): flags, a namespace, skip/limit, then the query
// document.
func pktMongoQuery(p *pktPacket, c *pktConn, h mongoHeader, body []byte, ts time.Time) string {
	if len(body) < 4 {
		return "OP_QUERY (truncated)"
	}
	ns, rest, ok := bsonCString(body[4:])
	if !ok || len(rest) < 8 {
		return "OP_QUERY (truncated namespace)"
	}
	doc, _ := bsonElems(rest[8:])
	cmd := ns
	if len(doc) > 0 {
		cmd = doc[0].Key
	}
	c.mongoConn().pending[h.RequestID] = mongoPending{command: cmd, ns: ns, ts: ts}
	c.mongoConn().opsSeen++
	pktMongoClassify(p, c, cmd, ns, doc)
	p.Command, p.Query = cmd, ns
	out := fmt.Sprintf("OP_QUERY %s", ns)
	if len(doc) > 0 {
		if detail := mongoCommandDetail(cmd, doc, "", 0, 0); detail != "" {
			out += " — " + cmd + ": " + detail
		} else {
			out += " — " + cmd
		}
	}
	// The legacy handshake is normal on connection setup; the legacy *query* path is
	// not, and it is gone from modern servers.
	if !strings.HasSuffix(ns, ".$cmd") {
		p.Issues = append(p.Issues,
			"OP_QUERY used for a query rather than a handshake — the legacy read path, removed in MongoDB 5.1")
	}
	return out
}

// pktMongoReply decodes OP_REPLY, the answer to OP_QUERY.
func pktMongoReply(p *pktPacket, c *pktConn, h mongoHeader, body []byte, ts time.Time) string {
	if len(body) < 20 {
		return "OP_REPLY (truncated)"
	}
	cursorID := int64(binary.LittleEndian.Uint64(body[4:]))
	n := int(int32(binary.LittleEndian.Uint32(body[16:])))
	doc, _ := bsonElems(body[20:])
	mc := c.mongoConn()
	req, had := mc.pending[h.ResponseTo]
	if had {
		delete(mc.pending, h.ResponseTo)
	}
	lag := 0.0
	if had && !req.ts.IsZero() {
		lag = ts.Sub(req.ts).Seconds() * 1000
		p.LagMS = lag
	}
	p.Rows = n
	out := fmt.Sprintf("OP_REPLY %d doc(s)", n)
	if had {
		out = req.command + " → " + out
	}
	if cursorID != 0 {
		out += fmt.Sprintf(", cursor %d", cursorID)
	}
	if s := mongoHelloSummary(doc); s != "" {
		out += " — " + s
	}
	if okVal, hasOK := bsonInt(mustGet(doc, "ok")); hasOK && okVal == 0 {
		code, _ := bsonInt(mustGet(doc, "code"))
		errmsg := bsonStr(mustGet(doc, "errmsg"))
		c.stream.Errors++
		p.ErrCode = int(code)
		p.Status = fmt.Sprintf("Error %d: %s", code, pktEllipsis(errmsg, 110))
		if iss := mongoErrIssue(int(code), mongoCodeName(int(code)), errmsg, mc); iss != "" {
			p.Issues = append(p.Issues, iss)
		}
		out += fmt.Sprintf(" — error %d: %s", code, pktEllipsis(errmsg, 100))
	}
	if lag > 0 {
		out += fmt.Sprintf(" (%.1f ms)", lag)
	}
	return out
}
