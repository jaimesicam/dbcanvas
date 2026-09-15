package main

// Database Explorer — the SQL that is shared between MySQL, PostgreSQL and
// ClickHouse: how a statement is split and classified, how a read-only connection
// refuses one, and how a driver's rows become the engine-neutral result envelope.
//
// The classifier is not a security boundary and is not the only thing standing
// between a PMM database and an UPDATE. PMM's connections are read-only *at the
// server*: PostgreSQL runs them inside BEGIN READ ONLY, which refuses a write with
// SQLSTATE 25006, and ClickHouse runs them with readonly=1, which refuses one with
// error 164. Both were verified against a running PMM Server rather than assumed.
// What the classifier adds is a better refusal — one that names the statement and
// happens before anything is sent — and defence in depth if a future transport ever
// loses one of those server-side controls. It is never the only control.

import (
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// dexStmt is one statement out of a submission, with the offset it started at so an
// error position reported against the statement can be mapped back to the editor.
type dexStmt struct {
	Text   string
	Offset int
	Verb   string // the leading keyword, upper-cased
}

// dexSplitSQL splits a submission into statements on semicolons, respecting string
// literals, quoted identifiers, dollar-quoting, and both comment styles.
//
// It exists for two reasons that pull the same way. The grid needs to know which
// statement produced which result set, and a read-only connection needs to inspect
// *every* statement rather than the first — "SELECT 1; DROP TABLE x" is the oldest
// trick there is, and a classifier that reads only as far as the first semicolon
// waves it through.
func dexSplitSQL(src string) []dexStmt {
	var out []dexStmt
	start := 0
	i := 0
	n := len(src)
	flush := func(end int) {
		frag := src[start:end]
		if t := strings.TrimSpace(frag); t != "" {
			out = append(out, dexStmt{Text: t, Offset: start + (len(frag) - len(strings.TrimLeft(frag, " \t\r\n"))), Verb: dexVerbOf(t)})
		}
		start = end + 1
	}
	for i < n {
		c := src[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			q := c
			i++
			for i < n {
				if src[i] == '\\' && q != '`' && i+1 < n {
					i += 2
					continue
				}
				if src[i] == q {
					// A doubled quote is an escaped quote, not the end.
					if i+1 < n && src[i+1] == q {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '$':
			// PostgreSQL dollar-quoting: $tag$ … $tag$.
			if j := strings.IndexByte(src[i+1:], '$'); j >= 0 && dexIsDollarTag(src[i+1:i+1+j]) {
				tag := src[i : i+1+j+1]
				if k := strings.Index(src[i+len(tag):], tag); k >= 0 {
					i += len(tag) + k + len(tag)
					continue
				}
			}
			i++
		case c == '-' && i+1 < n && src[i+1] == '-':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '#' && i+1 < n && (src[i+1] == ' ' || src[i+1] == '\t'):
			// MySQL's hash comment. Guarded on whitespace so a `#` inside an
			// unquoted identifier does not swallow the rest of the line.
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			if j := strings.Index(src[i+2:], "*/"); j >= 0 {
				i += 2 + j + 2
			} else {
				i = n
			}
		case c == ';':
			flush(i)
			i++
		default:
			i++
		}
	}
	if start < n {
		flush(n)
	}
	return out
}

func dexIsDollarTag(s string) bool {
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// dexVerbOf is the leading keyword of a statement, with leading comments and any
// CTE preamble skipped — a WITH … SELECT is a read, and a WITH … DELETE is not.
func dexVerbOf(s string) string {
	s = dexStripLeadingComments(s)
	f := dexFirstWord(s)
	if f != "WITH" {
		return f
	}
	// Walk to the statement the CTE feeds. The data-modifying keyword may be
	// anywhere inside a WITH, so the strongest verb present decides.
	up := strings.ToUpper(s)
	for _, v := range []string{"INSERT", "UPDATE", "DELETE", "MERGE"} {
		if dexHasWord(up, v) {
			return v
		}
	}
	return "SELECT"
}

func dexStripLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s[2:], "*/"); i >= 0 {
				s = s[2+i+2:]
				continue
			}
			return ""
		}
		return s
	}
}

func dexFirstWord(s string) string {
	s = strings.TrimLeft(s, " \t\r\n(")
	i := 0
	for i < len(s) && (s[i] == '_' || (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z')) {
		i++
	}
	return strings.ToUpper(s[:i])
}

// dexHasWord reports whether up (already upper-cased) contains word as a whole word.
func dexHasWord(up, word string) bool {
	from := 0
	for {
		i := strings.Index(up[from:], word)
		if i < 0 {
			return false
		}
		i += from
		before := byte(' ')
		if i > 0 {
			before = up[i-1]
		}
		after := byte(' ')
		if i+len(word) < len(up) {
			after = up[i+len(word)]
		}
		if !dexWordByte(before) && !dexWordByte(after) {
			return true
		}
		from = i + len(word)
	}
}

func dexWordByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// dexReadVerbs are the statements a read-only connection allows. Everything absent
// is refused, which is the right default: a list of what is permitted cannot be
// out-flanked by a keyword nobody thought of, whereas a list of what is forbidden
// can. SET is not on it — a read-only session that can change its own settings is
// not read-only — and neither is CALL, whose body is opaque.
var dexReadVerbs = map[string]bool{
	"SELECT": true, "WITH": true, "SHOW": true, "EXPLAIN": true, "DESCRIBE": true,
	"DESC": true, "TABLE": true, "VALUES": true, "EXISTS": true,
}

// dexReadOnlyRefusal returns the refusal for a submission a read-only connection
// must not run, or nil. It reports the first offending statement by name, which is
// what makes the message useful: "rejected: DROP" says what to remove.
func dexReadOnlyRefusal(engine, src string) *dexError {
	stmts := dexSplitSQL(src)
	if len(stmts) == 0 {
		return nil
	}
	for _, st := range stmts {
		if dexReadVerbs[st.Verb] {
			continue
		}
		verb := st.Verb
		if verb == "" {
			verb = "this statement"
		}
		return &dexError{
			Engine: engine, Code: "DBX_READONLY", Name: "ReadOnly",
			Message: fmt.Sprintf("%s is not allowed on a read-only connection", verb),
			Detail: "This connection exposes PMM's internal databases for inspection. " +
				"They are read-only here, and the database itself refuses writes on this session as well.",
			Display: fmt.Sprintf("REFUSED: %s is not allowed on a read-only connection", verb),
		}
	}
	// A read-only connection also refuses several statements in one submission even
	// when each is a read. It gains nothing (the editor can run them one at a time)
	// and it is the shape every attempt to smuggle a write past a classifier takes.
	if len(stmts) > 1 {
		return &dexError{
			Engine: engine, Code: "DBX_MULTI", Name: "ReadOnly",
			Message: "a read-only connection runs one statement at a time",
			Detail:  "Several statements were submitted together. Run them individually.",
			Display: "REFUSED: a read-only connection runs one statement at a time",
		}
	}
	return nil
}

// dexIsDestructive reports whether a statement would change data or schema. The
// editor uses it to require a deliberate second click rather than to refuse: an
// ordinary lab database is meant to be written to, but nothing here should run a
// DROP because a keystroke landed in the wrong pane.
func dexIsDestructive(verb string) bool {
	switch verb {
	case "DROP", "TRUNCATE", "DELETE", "UPDATE", "ALTER", "RENAME", "REPLACE",
		"GRANT", "REVOKE", "KILL", "SHUTDOWN", "RESET", "FLUSH":
		return true
	}
	return false
}

// ---------------------------------------------------------------- rows → envelope

// dexSemanticOf maps a database type name to the semantic type the grid renders by.
// One function for all three SQL engines, because the shapes overlap far more than
// they differ and the differences that matter (ClickHouse's UInt64, PostgreSQL's
// jsonb, MySQL's BLOB) are all named distinctly enough to catch here.
func dexSemanticOf(dbType string) string {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	// Strip modifiers: NULLABLE(X), LOWCARDINALITY(X), ARRAY(X), VARCHAR(20), X[].
	for {
		switch {
		case strings.HasPrefix(t, "NULLABLE("), strings.HasPrefix(t, "LOWCARDINALITY("):
			if i := strings.IndexByte(t, '('); i >= 0 && strings.HasSuffix(t, ")") {
				t = t[i+1 : len(t)-1]
				continue
			}
		case strings.HasPrefix(t, "ARRAY("), strings.HasSuffix(t, "[]"):
			return dexSemArray
		}
		break
	}
	if i := strings.IndexByte(t, '('); i > 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	// The MySQL driver reports an unsigned column as "UNSIGNED BIGINT", with the
	// modifier in front; PostgreSQL and ClickHouse spell the same thing as part of
	// the type name. Without stripping it, a perfectly ordinary integer column reads
	// as a string: left-aligned in the grid, sorted as text, and not offered as a
	// chart's value axis.
	for _, mod := range []string{"UNSIGNED ", "SIGNED ", "ZEROFILL "} {
		t = strings.TrimPrefix(t, mod)
	}
	for _, mod := range []string{" UNSIGNED", " SIGNED", " ZEROFILL"} {
		t = strings.TrimSuffix(t, mod)
	}
	t = strings.TrimSpace(t)
	switch {
	case t == "JSON" || t == "JSONB":
		return dexSemJSON
	case t == "BOOL" || t == "BOOLEAN" || t == "BIT":
		return dexSemBool
	case strings.Contains(t, "TIMESTAMP") || t == "DATETIME" || t == "DATETIME64":
		return dexSemDateTime
	case t == "DATE" || t == "DATE32":
		return dexSemDate
	case t == "TIME" || t == "TIMETZ":
		return dexSemTime
	case strings.HasPrefix(t, "INT") || strings.HasPrefix(t, "UINT") ||
		t == "BIGINT" || t == "SMALLINT" || t == "TINYINT" || t == "MEDIUMINT" ||
		t == "SERIAL" || t == "BIGSERIAL" || t == "INT2" || t == "INT4" || t == "INT8" ||
		t == "YEAR":
		return dexSemInteger
	case strings.HasPrefix(t, "FLOAT") || strings.HasPrefix(t, "DOUBLE") ||
		strings.HasPrefix(t, "DECIMAL") || strings.HasPrefix(t, "NUMERIC") ||
		t == "REAL" || t == "MONEY" || t == "FLOAT4" || t == "FLOAT8":
		return dexSemNumber
	case strings.Contains(t, "BLOB") || strings.Contains(t, "BINARY") || t == "BYTEA" ||
		t == "VARBINARY" || t == "FIXEDSTRING":
		return dexSemBinary
	case t == "":
		return dexSemUnknown
	}
	return dexSemString
}

// dexBinPreview is how many bytes of a binary value are rendered as hex inline. The
// rest is still carried, base64-encoded, so "copy cell" gives the whole thing.
const dexBinPreview = 32

// dexCell converts one driver value to something JSON can carry without lying about
// it. Three cases earn their own wrapper:
//
//   - NULL becomes a JSON null, and never an empty string. Every grid that conflates
//     the two makes an empty VARCHAR indistinguishable from a missing value, which is
//     precisely the distinction someone opens a database client to check.
//   - []byte becomes dexBinary unless the column is textual, because a BLOB rendered
//     as UTF-8 is at best unreadable and at worst mistaken for the real contents.
//   - A uint64 past 2^53 becomes dexBigNum, because JSON.parse would round it and the
//     grid would display a number the database does not hold.
func dexCell(v any, sem string) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		if sem == dexSemBinary {
			return dexBinaryCell(x)
		}
		if !dexLooksTextual(x) {
			return dexBinaryCell(x)
		}
		return string(x)
	case time.Time:
		return dexFormatTime(x)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return dexBigNum{Marker: "bignum", Text: strconv.FormatFloat(x, 'g', -1, 64)}
		}
		return x
	case float32:
		return float64(x)
	case uint64:
		if x > 1<<53 {
			return dexBigNum{Marker: "bignum", Text: strconv.FormatUint(x, 10)}
		}
		return float64(x)
	case int64:
		if x > 1<<53 || x < -(1<<53) {
			return dexBigNum{Marker: "bignum", Text: strconv.FormatInt(x, 10)}
		}
		return x
	case bool, string, int, int32, uint32:
		return x
	}
	return fmt.Sprintf("%v", v)
}

func dexBinaryCell(b []byte) dexBinary {
	h := b
	if len(h) > dexBinPreview {
		h = h[:dexBinPreview]
	}
	return dexBinary{Marker: "binary", Base64: base64.StdEncoding.EncodeToString(b), Len: len(b), Hex: hex.EncodeToString(h)}
}

// dexLooksTextual decides whether a driver's []byte is text. MySQL hands back every
// string column as bytes, so without this every VARCHAR in the grid would render as
// a base64 blob.
func dexLooksTextual(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if len(b) > 1<<20 {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
		if c < 0x09 || (c > 0x0d && c < 0x20) {
			return false
		}
	}
	return true
}

// dexFormatTime renders a timestamp one way everywhere, so two columns from two
// engines in the same grid do not disagree about what a moment looks like. UTC and
// RFC3339-with-milliseconds: unambiguous, sortable as text, and the same string the
// server would print.
func dexFormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// dexScanRows drains a *sql.Rows into a result set, stopping at limit and reporting
// truncation rather than silently returning fewer rows than exist. The extra row it
// reads past the limit is what makes "truncated" honest — without it, a result of
// exactly `limit` rows is indistinguishable from a result that was cut.
func dexScanRows(rows *sql.Rows, limit int) (dexResultSet, error) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return dexResultSet{}, err
	}
	set := dexResultSet{Kind: dexSetRows, Rows: [][]any{}}
	sems := make([]string, len(cts))
	for i, ct := range cts {
		dbt := ct.DatabaseTypeName()
		sems[i] = dexSemanticOf(dbt)
		set.Columns = append(set.Columns, dexColumn{Name: ct.Name(), DatabaseType: dbt, SemanticType: sems[i]})
	}
	for rows.Next() {
		if len(set.Rows) >= limit {
			set.Truncated = true
			break
		}
		hold := make([]any, len(cts))
		ptr := make([]any, len(cts))
		for i := range hold {
			ptr[i] = &hold[i]
		}
		if err := rows.Scan(ptr...); err != nil {
			return set, err
		}
		row := make([]any, len(cts))
		for i := range hold {
			row[i] = dexCell(hold[i], sems[i])
		}
		set.Rows = append(set.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return set, err
	}
	set.RowCount = len(set.Rows)
	return set, nil
}

// dexTextSet builds a one-column result set out of lines of text — what a textual
// EXPLAIN is, and what a SHOW CREATE TABLE returns.
func dexTextSet(col string, lines []string) dexResultSet {
	set := dexResultSet{
		Kind:    dexSetExplain,
		Columns: []dexColumn{{Name: col, SemanticType: dexSemString}},
		Rows:    make([][]any, 0, len(lines)),
	}
	for _, l := range lines {
		set.Rows = append(set.Rows, []any{l})
	}
	set.RowCount = len(set.Rows)
	return set
}

// dexJSONSet carries a structured plan: a single JSON payload the Explain view can
// render as a tree, plus a text rendering so the result is readable even before a
// richer visualiser exists.
func dexJSONSet(raw json.RawMessage) dexResultSet {
	var buf strings.Builder
	var v any
	if json.Unmarshal(raw, &v) == nil {
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.Encode(v)
	} else {
		buf.Write(raw)
	}
	set := dexTextSet("QUERY PLAN", strings.Split(strings.TrimRight(buf.String(), "\n"), "\n"))
	set.Payload = raw
	return set
}

// dexQuoteIdent quotes an identifier for an engine. Used only for identifiers this
// process read out of the database's own catalogue and for the ones a user picked in
// the tree — never to assemble a predicate out of a value, which is what
// dexRowIdentity uses placeholders for.
func dexQuoteIdent(engine, name string) string {
	switch engine {
	case dexMySQL:
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	default: // postgres, clickhouse
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
}

// dexQuoteLiteral quotes a string literal for an engine. It is used only where an
// engine has no placeholder available — ClickHouse over the exec transport, MySQL's
// SHOW statements, and the catalogue queries that name a schema — and the values
// reaching it are object names chosen from the tree, never free-form user input.
//
// The escaping is not the same everywhere and pretending otherwise is how a name
// with an apostrophe in it becomes a syntax error at best. PostgreSQL with
// standard_conforming_strings on (the default since 9.1) treats a backslash as an
// ordinary character and only doubles the quote; MySQL and ClickHouse treat a
// backslash as an escape, so it has to be doubled too.
func dexQuoteLiteral(engine, s string) string {
	if engine == dexPostgres {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(s) + "'"
}
