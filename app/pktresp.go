package main

// pktresp.go — RESP, the Valkey/Redis serialisation protocol.
//
// RESP is the only text protocol of the four engines here, and that makes it the easiest
// to read and the easiest to get subtly wrong. Every value is a type byte, a payload and
// a CRLF:
//
//	+OK\r\n                          simple string
//	-ERR unknown command 'foo'\r\n   error
//	:42\r\n                          integer
//	$5\r\nhello\r\n                  bulk string (length-prefixed, may contain CRLF)
//	*2\r\n$3\r\nGET\r\n$1\r\nk\r\n   array — which is how every command is sent
//
// RESP3 (negotiated with HELLO 3) adds nulls, booleans, doubles, big numbers, verbatim
// strings, blob errors, maps, sets, attributes and — the one that changes the shape of a
// conversation — **push** messages, which the server sends unprompted for pub/sub and for
// client-side-caching invalidation. A decoder that assumes one reply per command gets the
// rest of a RESP3 connection wrong.
//
// Three things here matter more than the type table:
//
//   - **A bulk string's payload is binary.** Its length is authoritative and it may
//     contain \r\n, so scanning for a line terminator inside one is how a parser
//     desynchronises on any value holding a newline — a serialised JSON document, an RDB
//     chunk, a Lua script.
//   - **Pipelining is normal.** A client may put fifty commands in one segment and read
//     fifty replies later; there is no request id, so replies are matched by order alone
//     and a decoder that loses count loses the connection.
//   - **Inline commands exist.** "PING\r\n" with no framing at all is legal, and it is
//     what a health check or a telnet session sends.

import (
	"strconv"
	"strings"
)

// RESP type bytes.
const (
	respSimple   = '+'
	respError    = '-'
	respInt      = ':'
	respBulk     = '$'
	respArray    = '*'
	respNull     = '_' // RESP3
	respBool     = '#'
	respDouble   = ','
	respBigNum   = '('
	respBlobErr  = '!'
	respVerbatim = '='
	respMap      = '%'
	respSet      = '~'
	respAttr     = '|'
	respPush     = '>'
)

// respMaxBulk bounds a bulk string's declared length. Valkey's proto-max-bulk-len is
// 512 MB by default; past that ceiling the length is misframing, not a value.
const respMaxBulk = 512 << 20

// respMaxDepth bounds nesting. A reply can legitimately be an array of arrays; a hundred
// levels deep is a misparse.
const respMaxDepth = 12

// respValue is one decoded RESP value. Only what a one-line summary needs is kept: the
// type, a rendering, and for an array its elements, because a command *is* an array and
// its first element is the command name.
type respValue struct {
	Type  byte
	Str   string      // simple string, error, bulk payload (possibly truncated for display)
	Int   int64       // integer, or the element count for aggregates
	Items []respValue // array/set/map/push elements
	// Bytes is the value's whole encoded size, so a caller can report volume without
	// keeping the payload.
	Bytes int
	// Truncated means the payload was longer than respKeepBytes and was cut for display.
	Truncated bool
}

// respKeepBytes is how much of a bulk payload is kept. A capture holding 10 000 SETs of
// 4 KB values must not hold them all a second time.
const respKeepBytes = 160

// respParse decodes one value from the head of b. n is how many bytes it consumed; ok is
// false when b holds only part of a value, which is the normal case at a segment boundary
// and means "wait for more" rather than "malformed".
//
// bad is true for bytes that cannot be RESP at all — that distinction is what lets the
// caller tell a truncated value (keep buffering) from a desynchronised stream (re-anchor).
func respParse(b []byte, depth int) (v respValue, n int, ok bool, bad bool) {
	if len(b) == 0 {
		return respValue{}, 0, false, false
	}
	if depth > respMaxDepth {
		return respValue{}, 0, false, true
	}
	typ := b[0]
	line, lineLen, found := respLine(b[1:])
	if !found {
		// No CRLF yet. A line longer than any plausible header is not RESP.
		if len(b) > 1<<16 {
			return respValue{}, 0, false, true
		}
		return respValue{}, 0, false, false
	}
	hdr := 1 + lineLen

	switch typ {
	case respSimple, respError, respBigNum, respDouble, respBool, respNull:
		return respValue{Type: typ, Str: line, Bytes: hdr}, hdr, true, false

	case respInt:
		i, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
		if err != nil {
			return respValue{}, 0, false, true
		}
		return respValue{Type: typ, Int: i, Str: line, Bytes: hdr}, hdr, true, false

	case respBulk, respVerbatim, respBlobErr:
		l, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return respValue{}, 0, false, true
		}
		if l < 0 {
			// $-1 is RESP2's null bulk string.
			return respValue{Type: respNull, Str: "(nil)", Bytes: hdr}, hdr, true, false
		}
		if l > respMaxBulk {
			return respValue{}, 0, false, true
		}
		// The payload is BINARY and may contain CRLF: the length decides where it ends,
		// never a scan for a terminator.
		if len(b) < hdr+l+2 {
			return respValue{}, 0, false, false
		}
		payload := b[hdr : hdr+l]
		v := respValue{Type: typ, Int: int64(l), Bytes: hdr + l + 2}
		if l > respKeepBytes {
			v.Str, v.Truncated = string(payload[:respKeepBytes]), true
		} else {
			v.Str = string(payload)
		}
		return v, hdr + l + 2, true, false

	case respArray, respSet, respPush, respMap, respAttr:
		count, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return respValue{}, 0, false, true
		}
		if count < 0 {
			// *-1 is RESP2's null array.
			return respValue{Type: respNull, Str: "(nil array)", Bytes: hdr}, hdr, true, false
		}
		// A map declares pairs, so it holds twice as many values as its count.
		items := count
		if typ == respMap || typ == respAttr {
			items = count * 2
		}
		if items > 1<<20 {
			return respValue{}, 0, false, true
		}
		v := respValue{Type: typ, Int: int64(count), Bytes: hdr}
		off := hdr
		for i := 0; i < items; i++ {
			el, used, elOK, elBad := respParse(b[off:], depth+1)
			if elBad {
				return respValue{}, 0, false, true
			}
			if !elOK {
				return respValue{}, 0, false, false
			}
			off += used
			v.Bytes += used
			// Only the first few elements are kept: a 10 000-element LRANGE reply is
			// summarised by its count, not by its contents.
			if len(v.Items) < 8 {
				v.Items = append(v.Items, el)
			}
		}
		return v, off, true, false
	}
	return respValue{}, 0, false, true
}

// respLine returns the text up to the next CRLF and how many bytes that took.
func respLine(b []byte) (string, int, bool) {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\r' && b[i+1] == '\n' {
			return string(b[:i]), i + 2, true
		}
	}
	return "", 0, false
}

// respTypeName names a type byte, for the detail panel.
func respTypeName(t byte) string {
	switch t {
	case respSimple:
		return "simple string"
	case respError:
		return "error"
	case respInt:
		return "integer"
	case respBulk:
		return "bulk string"
	case respArray:
		return "array"
	case respNull:
		return "null"
	case respBool:
		return "boolean"
	case respDouble:
		return "double"
	case respBigNum:
		return "big number"
	case respBlobErr:
		return "blob error"
	case respVerbatim:
		return "verbatim string"
	case respMap:
		return "map"
	case respSet:
		return "set"
	case respAttr:
		return "attribute"
	case respPush:
		return "push"
	}
	return "?"
}

// respRender renders a value for one line of output.
func respRender(v respValue, depth int) string {
	switch v.Type {
	case respSimple:
		return v.Str
	case respError, respBlobErr:
		return "-" + v.Str
	case respInt:
		return strconv.FormatInt(v.Int, 10)
	case respNull:
		if v.Str != "" {
			return v.Str
		}
		return "(nil)"
	case respBool:
		if strings.HasPrefix(v.Str, "t") {
			return "true"
		}
		return "false"
	case respDouble, respBigNum:
		return v.Str
	case respBulk, respVerbatim:
		s := pktPrintable(v.Str)
		if v.Truncated {
			return strconv.Quote(pktEllipsis(s, 60)) + "…(" + pktBytes(int(v.Int)) + ")"
		}
		return strconv.Quote(pktEllipsis(s, 60))
	case respArray, respSet, respPush, respMap, respAttr:
		if depth <= 0 {
			return "[" + strconv.FormatInt(v.Int, 10) + " items]"
		}
		var parts []string
		for _, el := range v.Items {
			parts = append(parts, respRender(el, depth-1))
		}
		if int(v.Int) > len(v.Items) {
			parts = append(parts, "…+"+strconv.Itoa(int(v.Int)-len(v.Items)))
		}
		open, close := "[", "]"
		if v.Type == respMap || v.Type == respAttr {
			open, close = "{", "}"
		}
		return open + strings.Join(parts, " ") + close
	}
	return "?"
}

// respIsCommand reports whether a value could be a client command: an array whose
// elements are all bulk strings, the first of which is a plausible command name.
func respIsCommand(v respValue) bool {
	if v.Type != respArray || v.Int < 1 || len(v.Items) == 0 {
		return false
	}
	first := v.Items[0]
	if first.Type != respBulk || first.Str == "" || len(first.Str) > 32 {
		return false
	}
	for _, c := range first.Str {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && c != '.' && c != '_' {
			return false
		}
	}
	return true
}

// respInlineCommand recognises an inline command — "PING\r\n", with no RESP framing at
// all. It is legal, and it is what a health check, a telnet session or a port probe
// sends; on a busy connection it is also a very good sign that something is not a Valkey
// client at all, so the check is deliberately narrow.
func respInlineCommand(b []byte) (string, int, bool) {
	line, n, found := respLine(b)
	if !found || line == "" || len(line) > 128 {
		return "", 0, false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", 0, false
	}
	name := strings.ToUpper(fields[0])
	for _, c := range name {
		if !(c >= 'A' && c <= 'Z') && c != '.' && c != '_' {
			return "", 0, false
		}
	}
	if !valkeyKnownCommand(name) {
		return "", 0, false
	}
	return line, n, true
}
