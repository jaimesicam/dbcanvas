package main

// pktbson.go — just enough BSON to read a MongoDB message.
//
// MongoDB's wire protocol carries BSON documents, so decoding a capture means reading
// BSON. What this needs is not a general-purpose codec: it is the ability to walk a
// document, pick out a handful of named fields, and summarise the rest in one line —
// on bytes that may be truncated by the snaplen, corrupted by a capture gap, or not
// BSON at all because the framing slipped.
//
// So the rules here are:
//
//   - Nothing panics and nothing allocates per element. Every read is bounds-checked
//     against the buffer, and a document that claims a length past the end of the
//     buffer is reported as unreadable rather than parsed as far as it goes.
//   - Values are rendered for a human reading one line, not round-tripped: an
//     ObjectId becomes its hex, a $date becomes an RFC3339 instant, a 40 KB array
//     becomes "[…128 items]". A decoder that printed a whole document per packet
//     would be unusable on a real capture.
//   - The FIRST key of a command document is the command name (MongoDB's own rule),
//     so walking in order matters — a map would lose exactly the thing that
//     identifies the message.
//
// Deliberately not implemented: decimal128 arithmetic (reported as raw hex), code
// with scope (named, not parsed), and DBPointer/Symbol/Undefined, which are
// deprecated types no current server emits.

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// BSON element types, from the spec.
const (
	bsonDouble   = 0x01
	bsonString   = 0x02
	bsonDoc      = 0x03
	bsonArray    = 0x04
	bsonBinary   = 0x05
	bsonUndef    = 0x06
	bsonObjectID = 0x07
	bsonBool     = 0x08
	bsonDate     = 0x09
	bsonNull     = 0x0a
	bsonRegex    = 0x0b
	bsonDBPtr    = 0x0c
	bsonCode     = 0x0d
	bsonSymbol   = 0x0e
	bsonCodeWS   = 0x0f
	bsonInt32    = 0x10
	bsonTimestmp = 0x11
	bsonInt64    = 0x12
	bsonDecimal  = 0x13
	bsonMinKey   = 0xff
	bsonMaxKey   = 0x7f
)

// bsonMaxDoc bounds what is accepted as a document length. MongoDB's own limit is
// 16 MB for a user document; a command message can legitimately be larger (a bulk
// insert is 48 MB by default), so this is the message ceiling rather than the
// document one. Past it, the bytes are not BSON.
const bsonMaxDoc = 64 << 20

// bsonElem is one element of a document, in the order it appeared.
type bsonElem struct {
	Type byte
	Key  string
	// Raw is the element's value bytes, still encoded. Kept rather than decoded so a
	// caller that only wants two fields does not pay for the rest.
	Raw []byte
}

// bsonDocOK reports whether b starts with a plausible document: a length that fits,
// and a terminating NUL where the length says it should be.
func bsonDocOK(b []byte) bool {
	if len(b) < 5 {
		return false
	}
	n := int(int32(binary.LittleEndian.Uint32(b)))
	return n >= 5 && n <= bsonMaxDoc && n <= len(b) && b[n-1] == 0
}

// bsonElems walks a document and returns its elements in order. A malformed or
// truncated document yields whatever was readable plus ok=false, because a capture's
// last document is very often cut short by the snaplen and the fields before the cut
// are still worth reading.
func bsonElems(b []byte) (elems []bsonElem, ok bool) {
	if len(b) < 5 {
		return nil, false
	}
	n := int(int32(binary.LittleEndian.Uint32(b)))
	if n < 5 || n > bsonMaxDoc {
		return nil, false
	}
	if n > len(b) {
		n = len(b) // truncated: read as far as the buffer goes
		ok = false
	} else {
		ok = true
	}
	pos := 4
	for pos < n {
		t := b[pos]
		if t == 0 { // end of document
			return elems, ok
		}
		pos++
		key, rest, kok := bsonCString(b[pos:n])
		if !kok {
			return elems, false
		}
		pos = n - len(rest)
		size, sok := bsonValueLen(t, b[pos:n])
		if !sok {
			return elems, false
		}
		elems = append(elems, bsonElem{Type: t, Key: key, Raw: b[pos : pos+size]})
		pos += size
		// A command document with hundreds of fields is a pipeline, not a command;
		// the caller only ever wants the first few and the rest are summarised.
		if len(elems) >= 256 {
			return elems, ok
		}
	}
	return elems, ok
}

// bsonCString reads a NUL-terminated key.
func bsonCString(b []byte) (string, []byte, bool) {
	for i := 0; i < len(b); i++ {
		if b[i] == 0 {
			return string(b[:i]), b[i+1:], true
		}
	}
	return "", nil, false
}

// bsonValueLen is the encoded length of a value of type t at the head of b.
func bsonValueLen(t byte, b []byte) (int, bool) {
	fixed := map[byte]int{
		bsonDouble: 8, bsonObjectID: 12, bsonBool: 1, bsonDate: 8, bsonNull: 0,
		bsonUndef: 0, bsonInt32: 4, bsonTimestmp: 8, bsonInt64: 8, bsonDecimal: 16,
		bsonMinKey: 0, bsonMaxKey: 0,
	}
	if n, okFixed := fixed[t]; okFixed {
		if len(b) < n {
			return 0, false
		}
		return n, true
	}
	switch t {
	case bsonString, bsonCode, bsonSymbol:
		if len(b) < 4 {
			return 0, false
		}
		n := int(int32(binary.LittleEndian.Uint32(b)))
		if n < 1 || 4+n > len(b) {
			return 0, false
		}
		return 4 + n, true
	case bsonDoc, bsonArray, bsonCodeWS:
		if len(b) < 4 {
			return 0, false
		}
		n := int(int32(binary.LittleEndian.Uint32(b)))
		if n < 5 || n > len(b) {
			return 0, false
		}
		return n, true
	case bsonBinary:
		if len(b) < 5 {
			return 0, false
		}
		n := int(int32(binary.LittleEndian.Uint32(b)))
		if n < 0 || 5+n > len(b) {
			return 0, false
		}
		return 5 + n, true
	case bsonRegex:
		// two cstrings back to back
		_, rest, ok := bsonCString(b)
		if !ok {
			return 0, false
		}
		_, rest2, ok := bsonCString(rest)
		if !ok {
			return 0, false
		}
		return len(b) - len(rest2), true
	case bsonDBPtr:
		if len(b) < 4 {
			return 0, false
		}
		n := int(int32(binary.LittleEndian.Uint32(b)))
		if n < 1 || 4+n+12 > len(b) {
			return 0, false
		}
		return 4 + n + 12, true
	}
	return 0, false
}

// bsonGet finds a top-level element by key. Order-preserving lookup, so the first
// match wins — which is what a duplicate key in a hand-built document means anyway.
func bsonGet(elems []bsonElem, key string) (bsonElem, bool) {
	for _, e := range elems {
		if e.Key == key {
			return e, true
		}
	}
	return bsonElem{}, false
}

// bsonStr reads a string element's value.
func bsonStr(e bsonElem) string {
	if e.Type != bsonString || len(e.Raw) < 5 {
		return ""
	}
	n := int(int32(binary.LittleEndian.Uint32(e.Raw)))
	if n < 1 || 4+n > len(e.Raw) {
		return ""
	}
	return pktPrintable(string(e.Raw[4 : 4+n-1]))
}

// bsonInt reads any integral element (int32, int64, double, bool) as an int64, which
// is what the fields this decoder cares about are: counts, codes, flags.
func bsonInt(e bsonElem) (int64, bool) {
	switch e.Type {
	case bsonInt32:
		if len(e.Raw) < 4 {
			return 0, false
		}
		return int64(int32(binary.LittleEndian.Uint32(e.Raw))), true
	case bsonInt64, bsonTimestmp:
		if len(e.Raw) < 8 {
			return 0, false
		}
		return int64(binary.LittleEndian.Uint64(e.Raw)), true
	case bsonDouble:
		if len(e.Raw) < 8 {
			return 0, false
		}
		return int64(math.Float64frombits(binary.LittleEndian.Uint64(e.Raw))), true
	case bsonBool:
		if len(e.Raw) < 1 {
			return 0, false
		}
		if e.Raw[0] != 0 {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// bsonSub returns a document or array element's own elements.
func bsonSub(e bsonElem) []bsonElem {
	if e.Type != bsonDoc && e.Type != bsonArray {
		return nil
	}
	sub, _ := bsonElems(e.Raw)
	return sub
}

// bsonPath walks nested documents by key ("cursor", "id").
func bsonPath(elems []bsonElem, path ...string) (bsonElem, bool) {
	cur := elems
	for i, key := range path {
		e, ok := bsonGet(cur, key)
		if !ok {
			return bsonElem{}, false
		}
		if i == len(path)-1 {
			return e, true
		}
		cur = bsonSub(e)
		if cur == nil {
			return bsonElem{}, false
		}
	}
	return bsonElem{}, false
}

// bsonValue renders one value for a single line of output. depth bounds nesting so a
// deep aggregation pipeline cannot produce a wall of text.
func bsonValue(e bsonElem, depth int) string {
	switch e.Type {
	case bsonString:
		s := bsonStr(e)
		return strconv.Quote(pktEllipsis(s, 60))
	case bsonInt32, bsonInt64:
		if v, ok := bsonInt(e); ok {
			return strconv.FormatInt(v, 10)
		}
	case bsonDouble:
		if len(e.Raw) >= 8 {
			return strconv.FormatFloat(math.Float64frombits(binary.LittleEndian.Uint64(e.Raw)), 'g', 6, 64)
		}
	case bsonBool:
		if len(e.Raw) >= 1 && e.Raw[0] != 0 {
			return "true"
		}
		return "false"
	case bsonNull:
		return "null"
	case bsonUndef:
		return "undefined"
	case bsonMinKey:
		return "MinKey"
	case bsonMaxKey:
		return "MaxKey"
	case bsonObjectID:
		if len(e.Raw) >= 12 {
			return "ObjectId(" + hex.EncodeToString(e.Raw[:12]) + ")"
		}
	case bsonDate:
		if len(e.Raw) >= 8 {
			ms := int64(binary.LittleEndian.Uint64(e.Raw))
			// A Date whose value cannot be a date is not a date. MongoDB's internal
			// replies carry OpTimes in Date-typed fields — a heartbeat's electionTime is
			// (seconds << 32 | increment) tagged 0x09 — and rendering that as a calendar
			// instant produced "243057045-12-23T22:12:28.801Z" on live traffic. The raw
			// value, with the reading that actually makes sense, is the honest answer.
			if ms < 0 || ms > 4e12 { // 4e12 ms ≈ the year 2096
				if secs := uint32(uint64(ms) >> 32); secs > 1e9 && secs < 4e9 {
					return fmt.Sprintf("Timestamp(%d, %d) [in a Date-typed field]",
						secs, uint32(uint64(ms)))
				}
				return fmt.Sprintf("Date(raw %d)", ms)
			}
			return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
		}
	case bsonTimestmp:
		// A BSON timestamp is two uint32s: increment then seconds. This is the type
		// oplog entries and $clusterTime use, so it is worth rendering the way
		// MongoDB's own tools do.
		if len(e.Raw) >= 8 {
			inc := binary.LittleEndian.Uint32(e.Raw)
			secs := binary.LittleEndian.Uint32(e.Raw[4:])
			return fmt.Sprintf("Timestamp(%d, %d)", secs, inc)
		}
	case bsonBinary:
		if len(e.Raw) >= 5 {
			n := int(int32(binary.LittleEndian.Uint32(e.Raw)))
			return fmt.Sprintf("BinData(%d, %d bytes)", e.Raw[4], n)
		}
	case bsonRegex:
		if pat, rest, ok := bsonCString(e.Raw); ok {
			opts, _, _ := bsonCString(rest)
			return "/" + pktEllipsis(pktPrintable(pat), 40) + "/" + pktPrintable(opts)
		}
	case bsonCode, bsonCodeWS:
		return "Code(…)"
	case bsonDecimal:
		if len(e.Raw) >= 16 {
			return "Decimal128(0x" + hex.EncodeToString(e.Raw[:16]) + ")"
		}
	case bsonDoc, bsonArray:
		sub := bsonSub(e)
		if depth <= 0 {
			if e.Type == bsonArray {
				return fmt.Sprintf("[…%d items]", len(sub))
			}
			return fmt.Sprintf("{…%d fields}", len(sub))
		}
		var parts []string
		for i, s := range sub {
			if i >= 4 {
				parts = append(parts, fmt.Sprintf("…+%d", len(sub)-4))
				break
			}
			if e.Type == bsonArray {
				parts = append(parts, bsonValue(s, depth-1))
			} else {
				parts = append(parts, s.Key+": "+bsonValue(s, depth-1))
			}
		}
		if e.Type == bsonArray {
			return "[" + strings.Join(parts, ", ") + "]"
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return "?"
}

// bsonSummary renders a document as a one-line summary, skipping the fields every
// message carries — they are noise on every single row, and the ones that matter
// (lsid, txnNumber, $clusterTime) are reported by the caller when they are relevant.
func bsonSummary(elems []bsonElem, max int) string {
	skip := map[string]bool{
		"$db": true, "$clusterTime": true, "$readPreference": true, "lsid": true,
		"$configTime": true, "$topologyTime": true, "$audit": true, "$client": true,
		"$configServerState": true, "operationTime": true, "signature": true,
		"clusterTime": true, "mayBypassWriteBlocking": true,
	}
	var parts []string
	for _, e := range elems {
		if skip[e.Key] {
			continue
		}
		parts = append(parts, e.Key+": "+bsonValue(e, 1))
		if len(parts) >= max {
			break
		}
	}
	return strings.Join(parts, ", ")
}
