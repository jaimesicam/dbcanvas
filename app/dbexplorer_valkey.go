package main

// Database Explorer — Valkey, and the small RESP client it is built on.
//
// There is no Valkey client library in this repository and this feature does not add
// one. RESP is a handful of frame types — simple string, error, integer, bulk string,
// array, and RESP3's map/set/double/boolean/null — and what the browser needs from it
// is a typed reply it can render, which is easier to produce by reading the protocol
// than by unwrapping somebody else's abstraction over it. It is also one fewer
// dependency to license-check against GPL-3.0.
//
// -------------------------------------------------------------------- SCAN, not KEYS
//
// The key browser uses SCAN and never KEYS. KEYS walks the entire keyspace in a single
// blocking call: on a database with any real number of keys it stalls the server for
// every other client, which is precisely the failure a lab is supposed to teach you to
// avoid rather than to commit on your behalf. SCAN returns a cursor and a handful of
// keys, so the browser pages — and the cursor is carried through the API as the page
// token, which is what makes the tree lazy all the way down.
//
// SCAN's guarantees are worth stating because the UI has to be honest about them: a
// full iteration sees every key present throughout, a key added or removed mid-scan
// may or may not appear, and a page can come back empty with a non-zero cursor. So
// "no keys" is only ever said when the cursor has returned to 0.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- RESP

// dexRESPValue is one decoded reply. Kind is what the server said it was, which is
// what lets the viewer render a set differently from an array and an error
// differently from a bulk string.
type dexRESPValue struct {
	Kind  string          `json:"kind"` // string|status|error|int|double|bool|null|array|set|map
	Str   string          `json:"str,omitempty"`
	Int   int64           `json:"int,omitempty"`
	Num   float64         `json:"num,omitempty"`
	Bool  bool            `json:"bool,omitempty"`
	Items []dexRESPValue  `json:"items,omitempty"`
	Pairs []dexRESPPair   `json:"pairs,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

type dexRESPPair struct {
	Key   dexRESPValue `json:"key"`
	Value dexRESPValue `json:"value"`
}

const (
	dexRESPString = "string"
	dexRESPStatus = "status"
	dexRESPError  = "error"
	dexRESPInt    = "int"
	dexRESPDouble = "double"
	dexRESPBool   = "bool"
	dexRESPNull   = "null"
	dexRESPArray  = "array"
	dexRESPSet    = "set"
	dexRESPMap    = "map"
)

// dexRESPConn is one connection to a Valkey server.
type dexRESPConn struct {
	c  net.Conn
	br *bufio.Reader
}

func dexRESPDial(ctx context.Context, addr, password string, db int) (*dexRESPConn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	rc := &dexRESPConn{c: c, br: bufio.NewReaderSize(c, 64<<10)}
	if password != "" {
		if v, err := rc.do(ctx, "AUTH", password); err != nil {
			rc.Close()
			return nil, err
		} else if v.Kind == dexRESPError {
			rc.Close()
			// The password is never echoed back into the error, only the server's
			// own refusal.
			return nil, fmt.Errorf("%s", v.Str)
		}
	}
	if db > 0 {
		if v, err := rc.do(ctx, "SELECT", strconv.Itoa(db)); err != nil {
			rc.Close()
			return nil, err
		} else if v.Kind == dexRESPError {
			rc.Close()
			return nil, fmt.Errorf("%s", v.Str)
		}
	}
	return rc, nil
}

func (r *dexRESPConn) Close() error { return r.c.Close() }

// do writes one command and reads its reply. The deadline comes from the context, so
// a cancelled request closes the socket rather than leaving a goroutine blocked on a
// server that is not answering.
func (r *dexRESPConn) do(ctx context.Context, args ...string) (dexRESPValue, error) {
	if dl, ok := ctx.Deadline(); ok {
		r.c.SetDeadline(dl)
	} else {
		r.c.SetDeadline(time.Now().Add(30 * time.Second))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := r.c.Write([]byte(b.String())); err != nil {
		return dexRESPValue{}, err
	}
	return r.read(0)
}

// dexRESPMaxDepth bounds nesting. A reply deep enough to exhaust the stack would have
// to be hostile, and a lab server can be made to send one.
const dexRESPMaxDepth = 16

func (r *dexRESPConn) read(depth int) (dexRESPValue, error) {
	if depth > dexRESPMaxDepth {
		return dexRESPValue{}, fmt.Errorf("reply nested too deeply")
	}
	line, err := r.readLine()
	if err != nil {
		return dexRESPValue{}, err
	}
	if line == "" {
		return dexRESPValue{}, fmt.Errorf("empty reply")
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return dexRESPValue{Kind: dexRESPStatus, Str: body}, nil
	case '-':
		return dexRESPValue{Kind: dexRESPError, Str: body}, nil
	case ':':
		n, _ := strconv.ParseInt(body, 10, 64)
		return dexRESPValue{Kind: dexRESPInt, Int: n}, nil
	case ',': // RESP3 double
		f, _ := strconv.ParseFloat(body, 64)
		return dexRESPValue{Kind: dexRESPDouble, Num: f}, nil
	case '#': // RESP3 boolean
		return dexRESPValue{Kind: dexRESPBool, Bool: body == "t"}, nil
	case '_': // RESP3 null
		return dexRESPValue{Kind: dexRESPNull}, nil
	case '$', '=':
		n, err := strconv.Atoi(body)
		if err != nil {
			return dexRESPValue{}, fmt.Errorf("bad bulk length")
		}
		if n < 0 {
			return dexRESPValue{Kind: dexRESPNull}, nil
		}
		buf := make([]byte, n+2)
		if _, err := ioReadFull(r.br, buf); err != nil {
			return dexRESPValue{}, err
		}
		return dexRESPValue{Kind: dexRESPString, Str: string(buf[:n])}, nil
	case '*', '~', '>':
		n, err := strconv.Atoi(body)
		if err != nil {
			return dexRESPValue{}, fmt.Errorf("bad array length")
		}
		if n < 0 {
			return dexRESPValue{Kind: dexRESPNull}, nil
		}
		kind := dexRESPArray
		if line[0] == '~' {
			kind = dexRESPSet
		}
		v := dexRESPValue{Kind: kind, Items: make([]dexRESPValue, 0, n)}
		for i := 0; i < n; i++ {
			item, err := r.read(depth + 1)
			if err != nil {
				return v, err
			}
			v.Items = append(v.Items, item)
		}
		return v, nil
	case '%':
		n, err := strconv.Atoi(body)
		if err != nil {
			return dexRESPValue{}, fmt.Errorf("bad map length")
		}
		v := dexRESPValue{Kind: dexRESPMap}
		for i := 0; i < n; i++ {
			k, err := r.read(depth + 1)
			if err != nil {
				return v, err
			}
			val, err := r.read(depth + 1)
			if err != nil {
				return v, err
			}
			v.Pairs = append(v.Pairs, dexRESPPair{Key: k, Value: val})
		}
		return v, nil
	}
	return dexRESPValue{}, fmt.Errorf("unexpected reply byte %q", line[0])
}

func (r *dexRESPConn) readLine() (string, error) {
	s, err := r.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// strs flattens an array reply to strings, which is what most commands return.
func (v dexRESPValue) strs() []string {
	out := make([]string, 0, len(v.Items))
	for _, it := range v.Items {
		out = append(out, it.text())
	}
	return out
}

func (v dexRESPValue) text() string {
	switch v.Kind {
	case dexRESPInt:
		return strconv.FormatInt(v.Int, 10)
	case dexRESPDouble:
		return strconv.FormatFloat(v.Num, 'g', -1, 64)
	case dexRESPBool:
		return strconv.FormatBool(v.Bool)
	case dexRESPNull:
		return ""
	case dexRESPArray, dexRESPSet, dexRESPMap:
		b, _ := json.Marshal(v)
		return string(b)
	}
	return v.Str
}

// ---------------------------------------------------------------- adapter

type dexValkeyAdapter struct {
	addr string
	pass string
	caps dexCaps
}

func (v *dexValkeyAdapter) Capabilities() dexCaps { return v.caps }
func (v *dexValkeyAdapter) Close() error          { return nil }

// conn opens a connection for one operation. Valkey connections are cheap and
// stateful (SELECT picks a database), so one per request is simpler and safer than a
// pool whose state depends on which request touched it last.
func (v *dexValkeyAdapter) conn(ctx context.Context, db int) (*dexRESPConn, *dexError) {
	c, err := dexRESPDial(ctx, v.addr, v.pass, db)
	if err != nil {
		return nil, dexValkeyError(err.Error(), ctx)
	}
	return c, nil
}

func (v *dexValkeyAdapter) TestConnection(ctx context.Context) error {
	c, derr := v.conn(ctx, 0)
	if derr != nil {
		return derr
	}
	defer c.Close()
	_, err := c.do(ctx, "PING")
	return err
}

func (v *dexValkeyAdapter) Version(ctx context.Context) (string, error) {
	c, derr := v.conn(ctx, 0)
	if derr != nil {
		return "", derr
	}
	defer c.Close()
	r, err := c.do(ctx, "INFO", "server")
	if err != nil {
		return "", err
	}
	for _, ln := range strings.Split(r.Str, "\n") {
		if strings.HasPrefix(ln, "valkey_version:") || strings.HasPrefix(ln, "redis_version:") {
			return strings.TrimSpace(strings.SplitN(ln, ":", 2)[1]), nil
		}
	}
	return "", nil
}

// ListDatabases returns the logical databases (SELECT 0..N-1) and how many keys are
// in each. A cluster has exactly one, which INFO reports the same way, so the tree
// does not need a special case for it.
func (v *dexValkeyAdapter) ListDatabases(ctx context.Context) ([]dexTreeNode, error) {
	c, derr := v.conn(ctx, 0)
	if derr != nil {
		return nil, derr
	}
	defer c.Close()

	n := 16
	if r, err := c.do(ctx, "CONFIG", "GET", "databases"); err == nil && len(r.Items) == 2 {
		if k, err := strconv.Atoi(r.Items[1].text()); err == nil && k > 0 {
			n = k
		}
	}
	// A cluster only ever has database 0, and asking for more would offer fifteen
	// that every command refuses.
	if r, err := c.do(ctx, "INFO", "cluster"); err == nil && strings.Contains(r.Str, "cluster_enabled:1") {
		n = 1
	}
	counts := map[int]int64{}
	if r, err := c.do(ctx, "INFO", "keyspace"); err == nil {
		for _, ln := range strings.Split(r.Str, "\n") {
			ln = strings.TrimSpace(ln)
			if !strings.HasPrefix(ln, "db") {
				continue
			}
			head, rest, ok := strings.Cut(ln, ":")
			if !ok {
				continue
			}
			idx, err := strconv.Atoi(strings.TrimPrefix(head, "db"))
			if err != nil {
				continue
			}
			for _, f := range strings.Split(rest, ",") {
				if k, vv, ok := strings.Cut(f, "="); ok && k == "keys" {
					cnt, _ := strconv.ParseInt(vv, 10, 64)
					counts[idx] = cnt
				}
			}
		}
	}
	out := make([]dexTreeNode, 0, n)
	for i := 0; i < n; i++ {
		node := dexTreeNode{ID: strconv.Itoa(i), Name: "db" + strconv.Itoa(i), Kind: dexKindKeyspace, HasChild: true}
		if cnt, ok := counts[i]; ok {
			node.Rows = &cnt
		} else {
			zero := int64(0)
			node.Rows = &zero
		}
		out = append(out, node)
	}
	return out, nil
}

func (v *dexValkeyAdapter) ListSchemas(ctx context.Context, database string) ([]dexTreeNode, error) {
	return nil, nil
}

// dexValkeyScanCount is how many keys one SCAN round asks the server to examine. It
// is a hint, not a page size: SCAN may return more or fewer, which is why the caller
// loops until it has a page or the cursor comes home.
const dexValkeyScanCount = 500

// dexValkeyPage is how many keys one browser page holds.
const dexValkeyPage = 200

// ListObjects browses keys with SCAN. `folder` carries a key-type filter ("string",
// "hash", …) and `filter` a glob prefix — both are passed to the server as SCAN's own
// TYPE and MATCH options, so the filtering happens where the keys are rather than
// after shipping them all here.
func (v *dexValkeyAdapter) ListObjects(ctx context.Context, database, schema, folder, cursor, filter string) (dexObjectPage, error) {
	db, _ := strconv.Atoi(database)
	c, derr := v.conn(ctx, db)
	if derr != nil {
		return dexObjectPage{}, derr
	}
	defer c.Close()

	if cursor == "" {
		cursor = "0"
	}
	match := strings.TrimSpace(filter)
	if match == "" {
		match = "*"
	} else if !strings.ContainsAny(match, "*?[") {
		// A bare prefix is what people type; turning it into a glob is what they
		// mean by it. `users:` becomes `users:*`.
		match += "*"
	}

	page := dexObjectPage{Nodes: []dexTreeNode{}, Folders: dexValkeyTypes}
	rounds := 0
	for len(page.Nodes) < dexValkeyPage {
		args := []string{"SCAN", cursor, "MATCH", match, "COUNT", strconv.Itoa(dexValkeyScanCount)}
		if folder != "" && folder != "all" {
			args = append(args, "TYPE", folder)
		}
		r, err := c.do(ctx, args...)
		if err != nil {
			return page, dexValkeyError(err.Error(), ctx)
		}
		if r.Kind == dexRESPError {
			return page, dexValkeyError(r.Str, ctx)
		}
		if len(r.Items) != 2 {
			return page, dexValkeyError("unexpected SCAN reply", ctx)
		}
		cursor = r.Items[0].text()
		for _, k := range r.Items[1].strs() {
			page.Nodes = append(page.Nodes, dexTreeNode{ID: k, Name: k, Kind: dexKindKey, Folder: folder})
		}
		rounds++
		// SCAN may return an empty page with a live cursor. Looping forever on a
		// sparse keyspace would hang the request, so the round count is bounded and
		// the cursor is handed back for the user to continue.
		if cursor == "0" || rounds >= 20 {
			break
		}
	}
	// Type, TTL and size per key, which is what makes the list readable. One
	// pipeline's worth of round trips per page, not per keyspace.
	for i := range page.Nodes {
		k := page.Nodes[i].Name
		if t, err := c.do(ctx, "TYPE", k); err == nil {
			page.Nodes[i].Badge = t.text()
			page.Nodes[i].Folder = t.text()
		}
		if ttl, err := c.do(ctx, "TTL", k); err == nil && ttl.Int > 0 {
			page.Nodes[i].Detail = "TTL " + (time.Duration(ttl.Int) * time.Second).String()
		}
		if n, ok := dexValkeyLength(ctx, c, k, page.Nodes[i].Badge); ok {
			page.Nodes[i].Rows = &n
		}
	}
	page.Cursor = cursor
	page.More = cursor != "0"
	return page, nil
}

// dexValkeyTypes are the key types the browser filters by.
var dexValkeyTypes = []string{"string", "list", "set", "zset", "hash", "stream"}

// dexValkeyLength is the number of elements in a key, by type. MEMORY USAGE would
// give a byte size but samples and can be slow on a large collection, so the element
// count is what the list shows and the byte size is left to the detail view.
func dexValkeyLength(ctx context.Context, c *dexRESPConn, key, typ string) (int64, bool) {
	cmd := map[string]string{
		"list": "LLEN", "set": "SCARD", "zset": "ZCARD", "hash": "HLEN",
		"string": "STRLEN", "stream": "XLEN",
	}[typ]
	if cmd == "" {
		return 0, false
	}
	r, err := c.do(ctx, cmd, key)
	if err != nil || r.Kind != dexRESPInt {
		return 0, false
	}
	return r.Int, true
}

// DescribeObject reads one key: its type, TTL, size and, for the collection types,
// enough of its contents to be worth looking at.
func (v *dexValkeyAdapter) DescribeObject(ctx context.Context, database string, ref dexObjectRef) (dexObjectDetail, error) {
	db, _ := strconv.Atoi(database)
	c, derr := v.conn(ctx, db)
	if derr != nil {
		return dexObjectDetail{}, derr
	}
	defer c.Close()

	d := dexObjectDetail{Ref: ref, Kind: dexKindKey}
	t, err := c.do(ctx, "TYPE", ref.Name)
	if err != nil {
		return d, dexValkeyError(err.Error(), ctx)
	}
	typ := t.text()
	if typ == "none" {
		return d, &dexError{Engine: dexValkey, Name: "NoSuchKey",
			Message: "the key does not exist (it may have expired)",
			Display: "the key does not exist (it may have expired)"}
	}
	add := func(l, val string) {
		if val != "" {
			d.Props = append(d.Props, dexProp{Label: l, Value: val})
		}
	}
	add("Type", typ)
	if ttl, err := c.do(ctx, "TTL", ref.Name); err == nil {
		switch {
		case ttl.Int == -1:
			add("TTL", "no expiry")
		case ttl.Int == -2:
			add("TTL", "expired")
		default:
			add("TTL", (time.Duration(ttl.Int) * time.Second).String())
		}
	}
	if n, ok := dexValkeyLength(ctx, c, ref.Name, typ); ok {
		d.RowEstimate = &n
		add("Elements", strconv.FormatInt(n, 10))
	}
	if mem, err := c.do(ctx, "MEMORY", "USAGE", ref.Name); err == nil && mem.Kind == dexRESPInt {
		b := mem.Int
		d.Bytes = &b
		add("Memory", byteSizeLabel(b))
	}
	if enc, err := c.do(ctx, "OBJECT", "ENCODING", ref.Name); err == nil {
		add("Encoding", enc.text())
	}
	d.Editable = v.caps.EditableRows
	if !d.Editable {
		d.EditReason = "this connection is read-only"
	}
	return d, nil
}

// ---------------------------------------------------------------- query

func (v *dexValkeyAdapter) Query(ctx context.Context, req dexQueryRequest) (dexResult, error) {
	res := dexResult{Engine: dexValkey, Limit: req.Limit}
	db, _ := strconv.Atoi(req.Database)
	limit := dexClampLimit(req.Limit)
	start := time.Now()

	c, derr := v.conn(ctx, db)
	if derr != nil {
		res.Error = derr
		return res, nil
	}
	defer c.Close()

	// Two shapes reach here: "open this key with the viewer its type deserves"
	// (Mongo's View Data equivalent), and "run this command", which is the console.
	var set dexResultSet
	if req.Mongo.Collection != "" && strings.TrimSpace(req.Command) == "" {
		set, derr = dexValkeyOpenKey(ctx, c, req.Mongo.Collection, limit)
	} else {
		set, derr = dexValkeyCommand(ctx, c, req.Command, limit)
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	if derr != nil {
		derr.ElapsedMs = res.DurationMs
		res.Error = derr
		return res, nil
	}
	res.Sets = []dexResultSet{set}
	return res, nil
}

// dexValkeyCommand runs one arbitrary command from the console and renders its reply
// structurally rather than dumping the protocol. A hash comes back as a field/value
// table, INFO as a section/key/value table, SLOWLOG as a table with the entry's
// fields as columns — because "show me the response" means "show me what it says".
func dexValkeyCommand(ctx context.Context, c *dexRESPConn, cmd string, limit int) (dexResultSet, *dexError) {
	args, err := dexSplitCommand(cmd)
	if err != nil {
		return dexResultSet{}, &dexError{Engine: dexValkey, Message: err.Error(), Display: err.Error()}
	}
	if len(args) == 0 {
		return dexResultSet{}, &dexError{Engine: dexValkey, Message: "nothing to run", Display: "nothing to run"}
	}
	name := strings.ToUpper(args[0])
	switch name {
	case "SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "MONITOR", "SYNC", "PSYNC":
		// These turn the connection into a stream that never returns, which a
		// request/response API has no way to represent. Refusing with a reason
		// beats a request that hangs until its timeout.
		return dexResultSet{}, &dexError{Engine: dexValkey, Name: "Unsupported",
			Message: name + " turns the connection into a subscription, which the Explorer cannot render",
			Display: name + " turns the connection into a subscription, which the Explorer cannot render"}
	case "KEYS":
		// Offered as a refusal rather than silently rewritten: the point is that
		// the user learns why, and SCAN is one click away in the key browser.
		return dexResultSet{}, &dexError{Engine: dexValkey, Name: "Refused",
			Message: "KEYS scans the whole keyspace in one blocking call. Use the key browser (which uses SCAN), or SCAN directly.",
			Display: "KEYS scans the whole keyspace in one blocking call — use SCAN, or the key browser"}
	}
	r, err := c.do(ctx, args...)
	if err != nil {
		return dexResultSet{}, dexValkeyError(err.Error(), ctx)
	}
	if r.Kind == dexRESPError {
		return dexResultSet{}, dexValkeyError(r.Str, ctx)
	}
	if name == "INFO" {
		return dexValkeyInfoSet(r.text()), nil
	}
	return dexValkeyRenderReply(name, r, limit), nil
}

// dexSplitCommand splits a console line into arguments, honouring quotes so a value
// with a space in it can be written.
func dexSplitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote := byte(0)
	started := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case inQuote != 0:
			if ch == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				continue
			}
			if ch == inQuote {
				inQuote = 0
				continue
			}
			cur.WriteByte(ch)
		case ch == '\'' || ch == '"':
			inQuote = ch
			started = true
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			if started || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(ch)
			started = true
		}
	}
	if inQuote != 0 {
		return nil, fmt.Errorf("unclosed quote")
	}
	if started || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

// dexValkeyRenderReply turns a decoded reply into a result set. An array of pairs
// (HGETALL, CONFIG GET, ZRANGE … WITHSCORES) becomes two columns, a flat array
// becomes an indexed list, and a scalar becomes a single cell — with the raw typed
// reply travelling alongside as the payload so the viewer can show the protocol shape
// when that is the interesting part.
func dexValkeyRenderReply(name string, r dexRESPValue, limit int) dexResultSet {
	payload, _ := json.Marshal(r)
	set := dexResultSet{Kind: dexSetReply, Payload: payload, Rows: [][]any{}}
	switch r.Kind {
	case dexRESPMap:
		set.Columns = []dexColumn{{Name: "field", SemanticType: dexSemString}, {Name: "value", SemanticType: dexSemString}}
		for i, p := range r.Pairs {
			if i >= limit {
				set.Truncated = true
				break
			}
			set.Rows = append(set.Rows, []any{p.Key.text(), p.Value.text()})
		}
	case dexRESPArray, dexRESPSet:
		if dexValkeyPairwise(name) && len(r.Items)%2 == 0 && len(r.Items) > 0 {
			set.Columns = []dexColumn{{Name: "field", SemanticType: dexSemString}, {Name: "value", SemanticType: dexSemString}}
			for i := 0; i+1 < len(r.Items); i += 2 {
				if len(set.Rows) >= limit {
					set.Truncated = true
					break
				}
				set.Rows = append(set.Rows, []any{r.Items[i].text(), r.Items[i+1].text()})
			}
			break
		}
		set.Columns = []dexColumn{{Name: "#", SemanticType: dexSemInteger}, {Name: "value", SemanticType: dexSemString}}
		for i, it := range r.Items {
			if i >= limit {
				set.Truncated = true
				break
			}
			set.Rows = append(set.Rows, []any{int64(i), it.text()})
		}
	default:
		set.Columns = []dexColumn{{Name: "reply", SemanticType: dexValkeySem(r.Kind)}}
		var cell any = r.text()
		if r.Kind == dexRESPInt {
			cell = r.Int
		} else if r.Kind == dexRESPNull {
			cell = nil
		}
		set.Rows = append(set.Rows, []any{cell})
	}
	set.RowCount = len(set.Rows)
	return set
}

func dexValkeySem(kind string) string {
	switch kind {
	case dexRESPInt:
		return dexSemInteger
	case dexRESPDouble:
		return dexSemNumber
	case dexRESPBool:
		return dexSemBool
	}
	return dexSemString
}

// dexValkeyPairwise are the commands whose flat array is really a field/value list.
func dexValkeyPairwise(name string) bool {
	switch name {
	case "HGETALL", "CONFIG", "HRANDFIELD", "XPENDING":
		return true
	}
	return false
}

// dexValkeyInfoSet parses INFO into a section/key/value table. INFO's own output is a
// text blob with `# Section` headers; keeping the section as a column is what lets
// the grid's filter find "used_memory" without the reader scrolling for it.
func dexValkeyInfoSet(text string) dexResultSet {
	set := dexResultSet{
		Kind: dexSetRows, Rows: [][]any{},
		Columns: []dexColumn{
			{Name: "section", SemanticType: dexSemString},
			{Name: "key", SemanticType: dexSemString},
			{Name: "value", SemanticType: dexSemString},
		},
	}
	section := ""
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			continue
		}
		if strings.HasPrefix(ln, "#") {
			section = strings.TrimSpace(strings.TrimPrefix(ln, "#"))
			continue
		}
		k, v, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		set.Rows = append(set.Rows, []any{section, k, v})
	}
	set.RowCount = len(set.Rows)
	return set
}

// dexValkeyOpenKey is View Data for a key: the viewer its type deserves. A hash is a
// field/value table, a sorted set is member and score, a stream is id/field/value,
// a list is indexed. Each is read incrementally where the type allows it.
func dexValkeyOpenKey(ctx context.Context, c *dexRESPConn, key string, limit int) (dexResultSet, *dexError) {
	t, err := c.do(ctx, "TYPE", key)
	if err != nil {
		return dexResultSet{}, dexValkeyError(err.Error(), ctx)
	}
	typ := t.text()
	cols := func(names ...string) []dexColumn {
		out := make([]dexColumn, 0, len(names))
		for _, n := range names {
			out = append(out, dexColumn{Name: n, SemanticType: dexSemString})
		}
		return out
	}
	set := dexResultSet{Kind: dexSetRows, Rows: [][]any{}}
	switch typ {
	case "string":
		r, err := c.do(ctx, "GET", key)
		if err != nil {
			return set, dexValkeyError(err.Error(), ctx)
		}
		set.Columns = cols("value")
		s := r.text()
		// A string that is valid JSON is offered as JSON, so the viewer pretty-prints
		// it rather than showing one very long line.
		if json.Valid([]byte(s)) && (strings.HasPrefix(strings.TrimSpace(s), "{") || strings.HasPrefix(strings.TrimSpace(s), "[")) {
			set.Columns[0].SemanticType = dexSemJSON
		}
		set.Rows = append(set.Rows, []any{s})
	case "hash":
		// HSCAN rather than HGETALL: a hash can be as large as a keyspace, and the
		// same argument that rules out KEYS rules out HGETALL on an unknown hash.
		set.Columns = cols("field", "value")
		cursor := "0"
		for {
			r, err := c.do(ctx, "HSCAN", key, cursor, "COUNT", strconv.Itoa(dexValkeyScanCount))
			if err != nil {
				return set, dexValkeyError(err.Error(), ctx)
			}
			if len(r.Items) != 2 {
				break
			}
			cursor = r.Items[0].text()
			items := r.Items[1].Items
			for i := 0; i+1 < len(items); i += 2 {
				if len(set.Rows) >= limit {
					set.Truncated = true
					break
				}
				set.Rows = append(set.Rows, []any{items[i].text(), items[i+1].text()})
			}
			if cursor == "0" || set.Truncated {
				break
			}
		}
	case "list":
		set.Columns = []dexColumn{{Name: "index", SemanticType: dexSemInteger}, {Name: "value", SemanticType: dexSemString}}
		r, err := c.do(ctx, "LRANGE", key, "0", strconv.Itoa(limit))
		if err != nil {
			return set, dexValkeyError(err.Error(), ctx)
		}
		for i, it := range r.Items {
			if i >= limit {
				set.Truncated = true
				break
			}
			set.Rows = append(set.Rows, []any{int64(i), it.text()})
		}
	case "set":
		set.Columns = cols("member")
		cursor := "0"
		for {
			r, err := c.do(ctx, "SSCAN", key, cursor, "COUNT", strconv.Itoa(dexValkeyScanCount))
			if err != nil {
				return set, dexValkeyError(err.Error(), ctx)
			}
			if len(r.Items) != 2 {
				break
			}
			cursor = r.Items[0].text()
			for _, m := range r.Items[1].strs() {
				if len(set.Rows) >= limit {
					set.Truncated = true
					break
				}
				set.Rows = append(set.Rows, []any{m})
			}
			if cursor == "0" || set.Truncated {
				break
			}
		}
	case "zset":
		set.Columns = []dexColumn{{Name: "member", SemanticType: dexSemString}, {Name: "score", SemanticType: dexSemNumber}}
		r, err := c.do(ctx, "ZRANGE", key, "0", strconv.Itoa(limit), "WITHSCORES")
		if err != nil {
			return set, dexValkeyError(err.Error(), ctx)
		}
		for i := 0; i+1 < len(r.Items); i += 2 {
			if len(set.Rows) >= limit {
				set.Truncated = true
				break
			}
			score, _ := strconv.ParseFloat(r.Items[i+1].text(), 64)
			set.Rows = append(set.Rows, []any{r.Items[i].text(), score})
		}
	case "stream":
		set.Columns = cols("id", "field", "value")
		r, err := c.do(ctx, "XRANGE", key, "-", "+", "COUNT", strconv.Itoa(limit))
		if err != nil {
			return set, dexValkeyError(err.Error(), ctx)
		}
		for _, entry := range r.Items {
			if len(entry.Items) != 2 {
				continue
			}
			id := entry.Items[0].text()
			fields := entry.Items[1].Items
			for i := 0; i+1 < len(fields); i += 2 {
				if len(set.Rows) >= limit {
					set.Truncated = true
					break
				}
				set.Rows = append(set.Rows, []any{id, fields[i].text(), fields[i+1].text()})
			}
		}
	case "none":
		return set, &dexError{Engine: dexValkey, Name: "NoSuchKey",
			Message: "the key does not exist (it may have expired)",
			Display: "the key does not exist (it may have expired)"}
	default:
		set.Columns = cols("value")
		set.Rows = append(set.Rows, []any{"unsupported key type " + typ})
	}
	set.RowCount = len(set.Rows)
	sort.SliceStable(set.Rows, func(i, j int) bool { return false }) // keep server order
	return set, nil
}

// dexValkeyError presents a protocol or server error cleanly. Valkey's errors begin
// with an upper-case token — WRONGTYPE, NOAUTH, MOVED, CROSSSLOT — which is the part
// worth pulling out, because it is what the error actually is.
func dexValkeyError(msg string, ctx context.Context) *dexError {
	if ctx != nil && ctx.Err() != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return &dexError{Engine: dexValkey, Code: "DBX_TIMEOUT", Name: "Timeout",
				Message: "the command exceeded its time limit", Display: "the command exceeded its time limit"}
		}
		return &dexError{Engine: dexValkey, Code: "DBX_CANCELED", Name: "Canceled",
			Message: "the command was cancelled", Display: "the command was cancelled"}
	}
	msg = strings.TrimSpace(msg)
	e := &dexError{Engine: dexValkey, Message: msg, Display: msg}
	if head, rest, ok := strings.Cut(msg, " "); ok && head == strings.ToUpper(head) && head != "" {
		isToken := true
		for _, r := range head {
			if !(r >= 'A' && r <= 'Z') {
				isToken = false
				break
			}
		}
		if isToken {
			e.Name, e.Message = head, strings.TrimSpace(rest)
			e.Display = head + ": " + e.Message
		}
	}
	// MOVED tells a client which shard owns the slot. Saying so is more useful than
	// the raw line, because on a cluster it is the commonest thing to hit.
	if e.Name == "MOVED" || e.Name == "ASK" {
		e.Hint = "this key lives on another shard of the cluster — pick that shard's connection in the tree"
	}
	return e
}
