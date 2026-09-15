package main

// Database Explorer — ClickHouse.
//
// One generic adapter, deliberately. PMM Server carries a ClickHouse (its Query
// Analytics store) and that is the only one a DBCanvas stack has today, but writing
// a "PMM query API" would have baked PMM's shape into the only ClickHouse support
// the app has. Instead this is an ordinary ClickHouse adapter with two orthogonal
// knobs:
//
//	transport   how a query is sent — HTTP for a ClickHouse that listens on a
//	            network address, exec for one that listens on loopback inside a
//	            container (which is PMM's).
//	policy      whether the connection is read-only. PMM's is, through
//	            readonly=1 — a ClickHouse setting the *server* enforces, not a
//	            keyword list. A provisioned lab ClickHouse would not be.
//
// So a standalone ClickHouse node added to the canvas later is supported by setting
// neither knob, and nothing about PMM has to be unpicked first.
//
// Both transports ask for JSONCompact, which is the format that carries what a result
// grid needs: `meta` with each column's name and its real ClickHouse type, and `data`
// as positional arrays. ClickHouse renders 64-bit integers as JSON strings in this
// format precisely so they do not lose digits, which is why the reader converts by
// declared type rather than by what JSON happened to produce.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// dexCHTransport sends one query and returns the raw JSONCompact body.
type dexCHTransport interface {
	send(ctx context.Context, database, query string, readOnly bool) ([]byte, *dexError)
	close()
}

type dexCHAdapter struct {
	t         dexCHTransport
	defaultDB string
	readOnly  bool
	caps      dexCaps
}

func (c *dexCHAdapter) Capabilities() dexCaps { return c.caps }
func (c *dexCHAdapter) Close() error          { c.t.close(); return nil }

func (c *dexCHAdapter) db(database string) string {
	if strings.TrimSpace(database) != "" {
		return database
	}
	return c.defaultDB
}

// run is every internal query in this file: send, parse, done. The limit is applied
// by the caller through the SQL itself (ClickHouse has LIMIT and no cursor), so this
// only reads what came back.
func (c *dexCHAdapter) run(ctx context.Context, database, query string) (dexResultSet, *dexError) {
	body, derr := c.t.send(ctx, c.db(database), query, c.readOnly)
	if derr != nil {
		return dexResultSet{}, derr
	}
	return dexCHParse(body)
}

func (c *dexCHAdapter) TestConnection(ctx context.Context) error {
	_, err := c.Version(ctx)
	return err
}

func (c *dexCHAdapter) Version(ctx context.Context) (string, error) {
	set, derr := c.run(ctx, "", "SELECT version()")
	if derr != nil {
		return "", derr
	}
	if len(set.Rows) == 0 {
		return "", nil
	}
	return dexStr(set.Rows[0][0]), nil
}

// dexCHSystemDB are ClickHouse's own databases. Shown, because `system.query_log` is
// exactly the kind of thing this feature exists to make reachable — but marked.
var dexCHSystemDB = map[string]bool{
	"system": true, "information_schema": true, "INFORMATION_SCHEMA": true, "default": true,
}

func (c *dexCHAdapter) ListDatabases(ctx context.Context) ([]dexTreeNode, error) {
	set, derr := c.run(ctx, "", `SELECT name, engine FROM system.databases ORDER BY name`)
	if derr != nil {
		return nil, derr
	}
	out := make([]dexTreeNode, 0, len(set.Rows))
	for _, r := range set.Rows {
		name := dexStr(r[0])
		out = append(out, dexTreeNode{
			ID: name, Name: name, Kind: dexKindDatabase, HasChild: true,
			System: dexCHSystemDB[name], Detail: dexStr(r[1]),
		})
	}
	return out, nil
}

// ListSchemas is empty: ClickHouse has databases and tables and nothing between.
func (c *dexCHAdapter) ListSchemas(ctx context.Context, database string) ([]dexTreeNode, error) {
	return nil, nil
}

var dexCHFolders = []string{"Tables", "Views", "Dictionaries"}

func (c *dexCHAdapter) ListObjects(ctx context.Context, database, schema, folder, cursor, filter string) (dexObjectPage, error) {
	page := dexObjectPage{Folders: dexCHFolders, Nodes: []dexTreeNode{}}
	want := func(f string) bool { return folder == "" || folder == f }
	db := c.db(database)

	if want("Tables") || want("Views") {
		q := `SELECT name, engine, total_rows, total_bytes, COALESCE(comment, '')
		  FROM system.tables WHERE database = ` + dexQuoteLiteral(dexClickHouse, db) + ` ORDER BY name`
		set, derr := c.run(ctx, db, q)
		if derr != nil {
			return page, derr
		}
		for _, r := range set.Rows {
			name, engine := dexStr(r[0]), dexStr(r[1])
			kind, fold := dexKindTable, "Tables"
			if strings.Contains(engine, "View") {
				kind, fold = dexKindView, "Views"
			}
			if !want(fold) || !dexMatches(name, filter) {
				continue
			}
			n := dexTreeNode{ID: name, Name: name, Kind: kind, Folder: fold, HasChild: true,
				Badge: engine, Detail: dexStr(r[4])}
			if v, ok := dexInt(r[2]); ok {
				n.Rows = &v
			}
			if v, ok := dexInt(r[3]); ok {
				n.Bytes = &v
			}
			page.Nodes = append(page.Nodes, n)
		}
	}
	if want("Dictionaries") {
		q := `SELECT name, status, element_count FROM system.dictionaries
		  WHERE database = ` + dexQuoteLiteral(dexClickHouse, db) + ` ORDER BY name`
		if set, derr := c.run(ctx, db, q); derr == nil {
			for _, r := range set.Rows {
				if !dexMatches(dexStr(r[0]), filter) {
					continue
				}
				n := dexTreeNode{ID: dexStr(r[0]), Name: dexStr(r[0]), Kind: dexKindDictionary,
					Folder: "Dictionaries", Badge: dexStr(r[1])}
				if v, ok := dexInt(r[2]); ok {
					n.Rows = &v
				}
				page.Nodes = append(page.Nodes, n)
			}
		}
	}
	return page, nil
}

func (c *dexCHAdapter) DescribeObject(ctx context.Context, database string, ref dexObjectRef) (dexObjectDetail, error) {
	db := c.db(database)
	d := dexObjectDetail{Ref: ref, Kind: ref.Kind}
	dbl, tbl := dexQuoteLiteral(dexClickHouse, db), dexQuoteLiteral(dexClickHouse, ref.Name)

	colQ := `SELECT name, type, default_kind, default_expression, COALESCE(comment,''),
	    is_in_primary_key, is_in_sorting_key, is_in_partition_key, position
	  FROM system.columns WHERE database = ` + dbl + ` AND table = ` + tbl + ` ORDER BY position`
	set, derr := c.run(ctx, db, colQ)
	if derr != nil {
		return d, derr
	}
	for _, r := range set.Rows {
		pos, _ := dexInt(r[8])
		inPK, _ := dexBool(r[5])
		typ := dexStr(r[1])
		ci := dexColumnInfo{
			Name: dexStr(r[0]), Type: typ, Position: int(pos), Comment: dexStr(r[4]),
			// ClickHouse's nullability lives in the type, not in a flag.
			Nullable: strings.HasPrefix(typ, "Nullable("),
			Default:  strings.TrimSpace(dexStr(r[2]) + " " + dexStr(r[3])),
		}
		if inPK {
			ci.Key = "PRI"
			d.PrimaryKey = append(d.PrimaryKey, ci.Name)
		}
		d.Columns = append(d.Columns, ci)
	}

	// The engine and its keys are the whole story of a ClickHouse table, so they
	// are the Overview rather than a footnote.
	metaQ := `SELECT engine, COALESCE(partition_key,''), COALESCE(sorting_key,''),
	    COALESCE(primary_key,''), COALESCE(sampling_key,''), total_rows, total_bytes,
	    COALESCE(create_table_query,''), COALESCE(comment,'')
	  FROM system.tables WHERE database = ` + dbl + ` AND name = ` + tbl
	if set, derr := c.run(ctx, db, metaQ); derr == nil && len(set.Rows) == 1 {
		r := set.Rows[0]
		add := func(l, v string) {
			if strings.TrimSpace(v) != "" {
				d.Props = append(d.Props, dexProp{Label: l, Value: v})
			}
		}
		add("Engine", dexStr(r[0]))
		add("Partition key", dexStr(r[1]))
		add("Sorting key", dexStr(r[2]))
		add("Primary key", dexStr(r[3]))
		add("Sampling key", dexStr(r[4]))
		if v, ok := dexInt(r[5]); ok {
			d.RowEstimate = &v
		}
		if v, ok := dexInt(r[6]); ok {
			d.Bytes = &v
			add("On disk", byteSizeLabel(v))
		}
		add("Comment", dexStr(r[8]))
		d.DDL = dexStr(r[7])
		// TTL is part of the create statement rather than a column of its own, so
		// it is lifted out of it where there is one.
		if ttl := dexCHTTLOf(d.DDL); ttl != "" {
			add("TTL", ttl)
		}
	}

	// Data-skipping indexes. Not every build exposes system.data_skipping_indices;
	// its absence is expected, not an error.
	idxQ := `SELECT name, type, expr, granularity FROM system.data_skipping_indices
	  WHERE database = ` + dbl + ` AND table = ` + tbl
	if set, derr := c.run(ctx, db, idxQ); derr == nil {
		for _, r := range set.Rows {
			d.Indexes = append(d.Indexes, dexIndexInfo{
				Name: dexStr(r[0]), Type: dexStr(r[1]), Columns: []string{dexStr(r[2])},
			})
		}
	}

	// ClickHouse has no OLTP row identity, so the grid never offers in-place edits
	// for it — see dexCapsFor. Saying so plainly beats a disabled button.
	d.Editable = false
	d.EditReason = "ClickHouse has no row identity to update by — change data with an explicit statement (ALTER TABLE … UPDATE / DELETE) instead"
	return d, nil
}

// dexCHTTLOf lifts the TTL clause out of a CREATE TABLE statement.
func dexCHTTLOf(ddl string) string {
	i := strings.Index(ddl, "\nTTL ")
	if i < 0 {
		if i = strings.Index(ddl, " TTL "); i < 0 {
			return ""
		}
	}
	rest := ddl[i+5:]
	for _, stop := range []string{"\nSETTINGS", "\nORDER BY", "\nPARTITION BY", "\nPRIMARY KEY"} {
		if j := strings.Index(rest, stop); j >= 0 {
			rest = rest[:j]
		}
	}
	return strings.TrimSpace(rest)
}

func (c *dexCHAdapter) Query(ctx context.Context, req dexQueryRequest) (dexResult, error) {
	res := dexResult{Engine: dexClickHouse, Limit: req.Limit, ReadOnly: c.readOnly}
	src := strings.TrimSpace(req.SQL)
	if src == "" {
		res.Error = &dexError{Engine: dexClickHouse, Message: "nothing to run", Display: "nothing to run"}
		return res, nil
	}
	if c.readOnly {
		if e := dexReadOnlyRefusal(dexClickHouse, src); e != nil {
			res.Error = e
			return res, nil
		}
	}
	limit := dexClampLimit(req.Limit)
	start := time.Now()
	for _, st := range dexSplitSQL(src) {
		stmt := st.Text
		if req.Explain {
			stmt = dexCHExplainStmt(stmt)
		}
		// ClickHouse will happily stream a billion rows at a browser; runLimited is
		// what stops it — see dexCHWithLimit.
		set, derr := c.runLimited(ctx, c.db(req.Database), stmt, limit)
		if derr != nil {
			derr.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
			res.Error = derr
			res.DurationMs = derr.ElapsedMs
			return res, nil
		}
		set.Statement = st.Text
		if req.Explain {
			set.Kind = dexSetExplain
		}
		res.Sets = append(res.Sets, set)
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	return res, nil
}

// dexCHExplainStmt asks for the query plan. EXPLAIN alone gives the plan tree;
// EXPLAIN PIPELINE and EXPLAIN ESTIMATE exist too and a user can type either, which
// is why an explicit EXPLAIN is passed through untouched.
func dexCHExplainStmt(stmt string) string {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt)), "EXPLAIN") {
		return stmt
	}
	return "EXPLAIN " + stmt
}

func (c *dexCHAdapter) runLimited(ctx context.Context, db, stmt string, limit int) (dexResultSet, *dexError) {
	body, derr := c.t.send(ctx, db, dexCHWithLimit(stmt, limit), c.readOnly)
	if derr != nil {
		return dexResultSet{}, derr
	}
	set, derr := dexCHParse(body)
	if derr != nil {
		return set, derr
	}
	if len(set.Rows) > limit {
		set.Rows = set.Rows[:limit]
		set.RowCount = limit
		set.Truncated = true
	}
	return set, nil
}

// dexCHWithLimit appends the row ceiling as ClickHouse settings rather than by
// rewriting the user's SQL — the query that runs is the query they wrote, and the
// result simply stops.
//
// Three settings, and each earns its place. `max_result_rows` is the ceiling;
// `result_overflow_mode = 'break'` makes the server stop producing rows there rather
// than raising an error, which is what a grid wants (a partial answer marked as
// partial beats nothing at all); and `max_block_size` matters more than it looks,
// because the first two are enforced at block boundaries. Without it, a ceiling of
// 500 rows against a large table still ships a whole 65,536-row block across the
// transport before stopping. One row past the ceiling is asked for so that
// "truncated" is a fact rather than a guess.
func dexCHWithLimit(stmt string, limit int) string {
	s := strings.TrimRight(strings.TrimSpace(stmt), ";")
	// Only a statement that returns rows has a result to cap. On a CREATE or an
	// INSERT a trailing SETTINGS clause is not a no-op — ClickHouse reads it as the
	// *table's* settings and rejects max_result_rows there — so appending one would
	// break every write. Unreachable while the only ClickHouse is read-only, which
	// is exactly why it had to be found the moment one could be written to.
	if !dexReadVerbs[dexVerbOf(s)] {
		return s
	}
	if strings.Contains(strings.ToUpper(s), "SETTINGS") {
		return s // the user set their own; do not argue with them
	}
	n := limit + 1
	return fmt.Sprintf("%s SETTINGS max_result_rows = %d, result_overflow_mode = 'break', max_block_size = %d", s, n, n)
}

// ---------------------------------------------------------------- JSONCompact

// dexCHEnvelope is the JSONCompact document ClickHouse returns.
type dexCHEnvelope struct {
	Meta []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"meta"`
	Data       []json.RawMessage `json:"data"`
	Rows       int               `json:"rows"`
	Statistics struct {
		Elapsed   float64 `json:"elapsed"`
		RowsRead  int64   `json:"rows_read"`
		BytesRead int64   `json:"bytes_read"`
	} `json:"statistics"`
	Exception string `json:"exception"`
}

func dexCHParse(body []byte) (dexResultSet, *dexError) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		// A statement that returns nothing (a DDL, or SET) produces an empty body.
		return dexResultSet{Kind: dexSetAffected, Message: "OK"}, nil
	}
	if body[0] != '{' {
		// Not an envelope: an EXPLAIN in text form, or a plain-format reply.
		lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
		return dexTextSet("explain", lines), nil
	}
	var env dexCHEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return dexResultSet{}, &dexError{Engine: dexClickHouse,
			Message: "could not read the result: " + err.Error(), Display: "could not read the result"}
	}
	if env.Exception != "" {
		return dexResultSet{}, dexCHError(env.Exception)
	}
	set := dexResultSet{Kind: dexSetRows, Rows: make([][]any, 0, len(env.Data))}
	sems := make([]string, len(env.Meta))
	for i, m := range env.Meta {
		sems[i] = dexSemanticOf(m.Type)
		set.Columns = append(set.Columns, dexColumn{Name: m.Name, DatabaseType: m.Type, SemanticType: sems[i]})
	}
	for _, raw := range env.Data {
		var cells []json.RawMessage
		if json.Unmarshal(raw, &cells) != nil {
			continue
		}
		row := make([]any, len(set.Columns))
		for i := range set.Columns {
			if i < len(cells) {
				row[i] = dexCHCell(cells[i], sems[i])
			}
		}
		set.Rows = append(set.Rows, row)
	}
	set.RowCount = len(set.Rows)
	return set, nil
}

// dexCHCell converts one JSONCompact value. ClickHouse writes 64-bit integers as JSON
// strings so they survive the round trip; a grid that treated them as text would
// left-align them and refuse to sort them numerically, so the declared type decides.
func dexCHCell(raw json.RawMessage, sem string) any {
	s := strings.TrimSpace(string(raw))
	if s == "null" || s == "" {
		return nil
	}
	if s[0] == '[' || s[0] == '{' {
		return s // an Array, Tuple, Map or Nested value keeps its JSON
	}
	if s[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return s
		}
		switch sem {
		case dexSemInteger:
			if i, err := strconv.ParseInt(str, 10, 64); err == nil {
				if i > 1<<53 || i < -(1<<53) {
					return dexBigNum{Marker: "bignum", Text: str}
				}
				return i
			}
			if _, err := strconv.ParseUint(str, 10, 64); err == nil {
				return dexBigNum{Marker: "bignum", Text: str}
			}
		case dexSemNumber:
			if f, err := strconv.ParseFloat(str, 64); err == nil {
				return f
			}
		}
		return str
	}
	if s == "true" || s == "false" {
		return s == "true"
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// dexCHError reads ClickHouse's own exception text back into the structured form.
// The format is stable and carries both halves a user needs:
//
//	Code: 60. DB::Exception: Table pmm.foo does not exist. (UNKNOWN_TABLE)
func dexCHError(text string) *dexError {
	t := strings.TrimSpace(text)
	e := &dexError{Engine: dexClickHouse, Message: t, Display: t}
	if i := strings.Index(t, "Code: "); i >= 0 {
		rest := t[i+6:]
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j > 0 {
			e.Code = rest[:j]
		}
	}
	// The exception name is the last parenthesised token on the message.
	if i := strings.LastIndex(t, "("); i >= 0 {
		if j := strings.Index(t[i:], ")"); j > 0 {
			name := t[i+1 : i+j]
			if name != "" && name == strings.ToUpper(name) && !strings.Contains(name, " ") {
				e.Name = name
			}
		}
	}
	if i := strings.Index(t, "DB::Exception: "); i >= 0 {
		msg := t[i+len("DB::Exception: "):]
		if j := strings.Index(msg, "\n"); j >= 0 {
			msg = msg[:j]
		}
		e.Message = strings.TrimSpace(msg)
	}
	if e.Code != "" {
		e.Display = fmt.Sprintf("Code: %s. %s", e.Code, e.Message)
		if e.Name != "" {
			e.Display += " (" + e.Name + ")"
		}
	}
	return e
}

// ---------------------------------------------------------------- exec transport

// dexCHExec reaches a ClickHouse that listens on loopback by running curl inside its
// container, against the server's own HTTP interface.
//
// It uses HTTP rather than the bundled clickhouse-client, and that is not a
// preference. On percona/pmm-server:3 the client cannot execute an INSERT at all: it
// fails with `Code: 100. Unknown packet 11 from server` — the client and the server
// it ships beside are far enough apart in the native protocol that the client does
// not recognise a packet the server sends while inserting. SELECT is unaffected,
// which is why it went unnoticed while these connections were read-only, and why it
// surfaced the moment one could be written to.
//
// The HTTP interface has none of that coupling, returns its errors inside the same
// JSONCompact envelope the reader already understands, and is on 127.0.0.1:8123 in
// the same container. curl is in the image (PMM's own readiness probe uses it).
type dexCHExec struct {
	run  dexExecRunner
	user string
	pass string
	// port is the container-local HTTP port. 0 means ClickHouse's default.
	port int
}

func (x *dexCHExec) close() {}

// dexCHHTTPPort is ClickHouse's HTTP interface.
const dexCHHTTPPort = 8123

func (x *dexCHExec) send(ctx context.Context, database, query string, readOnly bool) ([]byte, *dexError) {
	q := url.Values{}
	q.Set("default_format", "JSONCompact")
	if database != "" {
		q.Set("database", database)
	}
	if readOnly {
		// The server-side half of read-only, and the reason it is 2 and not 1.
		//
		// Both refuse every write — INSERT, CREATE, DROP, ALTER … UPDATE/DELETE all
		// come back as error 164 (READONLY), verified against a running PMM Server.
		// They differ on settings: readonly=1 refuses to change any, which also
		// refuses the result ceiling this adapter needs (dexCHWithLimit), so a
		// read-only connection would be the one that could not cap its own results.
		// readonly=2 permits settings — but not that one: ClickHouse refuses to
		// modify `readonly` itself in readonly mode, so a session cannot lift its own
		// restriction. Also verified, because the whole guarantee rests on it.
		q.Set("readonly", "2")
	}
	port := x.port
	if port == 0 {
		port = dexCHHTTPPort
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/?%s", port, q.Encode())

	// The credentials go into a curl config file written from the environment, so
	// the password never appears on the command line — an argv is visible in the
	// container's own process list, and a password in one is a password anybody
	// with a shell in there can read without having to look for it.
	script := `set -e
umask 077
printf '%s\n' "$DBX_CH_AUTH" > "$DBX_CH_CONF"
trap 'rm -f "$DBX_CH_CONF"' EXIT
exec curl -sS -K "$DBX_CH_CONF" --data-binary @- "$DBX_CH_URL"`
	conf := fmt.Sprintf("/tmp/.dbx-ch-%d", time.Now().UnixNano())
	env := []string{
		"DBX_CH_AUTH=" + fmt.Sprintf("user = %q", x.user+":"+x.pass),
		"DBX_CH_CONF=" + conf,
		"DBX_CH_URL=" + endpoint,
	}
	res, err := x.run.run(ctx, []string{"bash", "-c", script}, env, []byte(query))
	if err != nil {
		if ctx.Err() != nil {
			return nil, dexCHCtxError(ctx)
		}
		return nil, &dexError{Engine: dexClickHouse, Message: err.Error(), Display: err.Error()}
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("the ClickHouse request exited with status %d", res.Code)
		}
		return nil, dexCHError(msg)
	}
	// A ClickHouse error arrives inside the envelope with an ordinary 200, so the
	// body is always parsed rather than being trusted because the transport was fine.
	return []byte(res.Stdout), nil
}

func dexCHCtxError(ctx context.Context) *dexError {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &dexError{Engine: dexClickHouse, Code: "DBX_TIMEOUT", Name: "Timeout",
			Message: "the query exceeded its time limit", Display: "the query exceeded its time limit"}
	}
	return &dexError{Engine: dexClickHouse, Code: "DBX_CANCELED", Name: "Canceled",
		Message: "the query was cancelled", Display: "the query was cancelled"}
}

// ---------------------------------------------------------------- HTTP transport

// dexCHHTTP is the transport for a ClickHouse reachable over the network — a
// standalone node on the canvas, whenever DBCanvas grows one. It is here now so that
// the ClickHouse support is not PMM-shaped: adding the node type later means adding a
// discovery entry, not an adapter.
type dexCHHTTP struct {
	base   string // http://host:8123
	user   string
	pass   string
	client *http.Client
}

func (h *dexCHHTTP) close() {}

func (h *dexCHHTTP) send(ctx context.Context, database, query string, readOnly bool) ([]byte, *dexError) {
	q := url.Values{}
	q.Set("default_format", "JSONCompact")
	if database != "" {
		q.Set("database", database)
	}
	if readOnly {
		q.Set("readonly", "2")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/?"+q.Encode(), strings.NewReader(query))
	if err != nil {
		return nil, &dexError{Engine: dexClickHouse, Message: err.Error(), Display: err.Error()}
	}
	// Credentials go in headers rather than the query string: a URL is the one part
	// of a request that ends up in logs.
	req.Header.Set("X-ClickHouse-User", h.user)
	req.Header.Set("X-ClickHouse-Key", h.pass)
	resp, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, dexCHCtxError(ctx)
		}
		return nil, &dexError{Engine: dexClickHouse, Message: err.Error(), Display: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, dexCHError(string(body))
	}
	return body, nil
}
