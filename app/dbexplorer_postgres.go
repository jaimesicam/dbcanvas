package main

// Database Explorer — the PostgreSQL family: PostgreSQL, Patroni, repmgr, Spock, and
// PMM Server's own internal database.
//
// One adapter, two transports. An ordinary lab node is dialled with pgx over the
// stack network; PMM's internal PostgreSQL listens on 127.0.0.1 inside its container
// and cannot be dialled at all, so it is reached by running psql in there. Every
// catalogue query in this file is written once and runs over whichever transport the
// connection has — which is the point of the seam, and the reason the PMM connection
// is not a second, half-featured implementation.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// dexPGTransport is how a statement reaches a PostgreSQL server.
type dexPGTransport interface {
	// exec runs one statement against one database and returns it as a result set.
	// limit caps the rows materialised; the transport reports truncation.
	exec(ctx context.Context, database, stmt string, limit int) (dexResultSet, *dexError)
	// readOnly reports whether the server itself refuses writes on this session.
	readOnly() bool
	close()
}

type dexPGAdapter struct {
	t         dexPGTransport
	defaultDB string
	caps      dexCaps
}

func (p *dexPGAdapter) Capabilities() dexCaps { return p.caps }
func (p *dexPGAdapter) Close() error          { p.t.close(); return nil }

func (p *dexPGAdapter) db(database string) string {
	if strings.TrimSpace(database) != "" {
		return database
	}
	return p.defaultDB
}

func (p *dexPGAdapter) TestConnection(ctx context.Context) error {
	_, err := p.Version(ctx)
	return err
}

func (p *dexPGAdapter) Version(ctx context.Context) (string, error) {
	set, derr := p.t.exec(ctx, p.defaultDB, "SELECT version()", 1)
	if derr != nil {
		return "", derr
	}
	if len(set.Rows) == 0 {
		return "", nil
	}
	return dexStr(set.Rows[0][0]), nil
}

// ---------------------------------------------------------------- the tree

func (p *dexPGAdapter) ListDatabases(ctx context.Context) ([]dexTreeNode, error) {
	const q = `SELECT d.datname,
	  pg_catalog.pg_database_size(d.oid),
	  d.datistemplate,
	  pg_catalog.pg_get_userbyid(d.datdba),
	  pg_catalog.pg_encoding_to_char(d.encoding)
	FROM pg_catalog.pg_database d
	WHERE d.datallowconn
	ORDER BY d.datname`
	set, derr := p.t.exec(ctx, p.defaultDB, q, 5000)
	if derr != nil {
		return nil, derr
	}
	out := make([]dexTreeNode, 0, len(set.Rows))
	for _, r := range set.Rows {
		name := dexStr(r[0])
		n := dexTreeNode{ID: name, Name: name, Kind: dexKindDatabase, HasChild: true,
			System: dexPGSystemDB(name), Detail: dexStr(r[4])}
		if b, ok := dexInt(r[1]); ok {
			n.Bytes = &b
		}
		out = append(out, n)
	}
	return out, nil
}

func dexPGSystemDB(name string) bool {
	return name == "postgres" || strings.HasPrefix(name, "template")
}

func (p *dexPGAdapter) ListSchemas(ctx context.Context, database string) ([]dexTreeNode, error) {
	const q = `SELECT n.nspname,
	  pg_catalog.pg_get_userbyid(n.nspowner),
	  (n.nspname IN ('pg_catalog','information_schema','pg_toast') OR n.nspname LIKE 'pg\_%')
	FROM pg_catalog.pg_namespace n
	WHERE n.nspname NOT LIKE 'pg\_temp\_%' AND n.nspname NOT LIKE 'pg\_toast\_temp\_%'
	ORDER BY 3, 1`
	set, derr := p.t.exec(ctx, p.db(database), q, 5000)
	if derr != nil {
		return nil, derr
	}
	out := make([]dexTreeNode, 0, len(set.Rows))
	for _, r := range set.Rows {
		sys, _ := dexBool(r[2])
		out = append(out, dexTreeNode{
			ID: dexStr(r[0]), Name: dexStr(r[0]), Kind: dexKindSchema,
			HasChild: true, System: sys, Detail: dexStr(r[1]),
		})
	}
	return out, nil
}

// dexPGFolders are the object groups a PostgreSQL schema is shown as.
var dexPGFolders = []string{"Tables", "Views", "Materialized Views", "Sequences", "Functions"}

func (p *dexPGAdapter) ListObjects(ctx context.Context, database, schema, folder, cursor, filter string) (dexObjectPage, error) {
	if strings.TrimSpace(schema) == "" {
		schema = "public"
	}
	page := dexObjectPage{Folders: dexPGFolders, Nodes: []dexTreeNode{}}
	want := func(f string) bool { return folder == "" || folder == f }

	if want("Tables") || want("Views") || want("Materialized Views") {
		// reltuples is an estimate maintained by ANALYZE, not a count. It is what
		// every database client shows for a table's size, because the alternative —
		// COUNT(*) on every table in a schema to draw a tree — is the thing this
		// feature must never do.
		const q = `SELECT c.relname, c.relkind, c.reltuples::bigint,
		    pg_catalog.pg_total_relation_size(c.oid),
		    COALESCE(obj_description(c.oid, 'pg_class'), '')
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $SCHEMA$ AND c.relkind IN ('r','p','v','m','f')
		  ORDER BY c.relname`
		set, derr := p.t.exec(ctx, p.db(database), strings.ReplaceAll(q, "$SCHEMA$", dexQuoteLiteral(dexPostgres, schema)), 20000)
		if derr != nil {
			return page, derr
		}
		for _, r := range set.Rows {
			kind, fold := dexKindTable, "Tables"
			switch dexStr(r[1]) {
			case "v":
				kind, fold = dexKindView, "Views"
			case "m":
				kind, fold = dexKindMatView, "Materialized Views"
			case "f":
				fold = "Tables"
			}
			if !want(fold) || !dexMatches(dexStr(r[0]), filter) {
				continue
			}
			n := dexTreeNode{ID: dexStr(r[0]), Name: dexStr(r[0]), Kind: kind, Folder: fold,
				HasChild: true, Detail: dexStr(r[4])}
			if v, ok := dexInt(r[2]); ok && v >= 0 {
				n.Rows = &v
			}
			if v, ok := dexInt(r[3]); ok {
				n.Bytes = &v
			}
			page.Nodes = append(page.Nodes, n)
		}
	}
	if want("Sequences") {
		const q = `SELECT c.relname FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $SCHEMA$ AND c.relkind = 'S' ORDER BY 1`
		set, derr := p.t.exec(ctx, p.db(database), strings.ReplaceAll(q, "$SCHEMA$", dexQuoteLiteral(dexPostgres, schema)), 20000)
		if derr == nil {
			for _, r := range set.Rows {
				if dexMatches(dexStr(r[0]), filter) {
					page.Nodes = append(page.Nodes, dexTreeNode{ID: dexStr(r[0]), Name: dexStr(r[0]), Kind: dexKindSequence, Folder: "Sequences"})
				}
			}
		}
	}
	if want("Functions") {
		const q = `SELECT p.proname,
		    pg_catalog.pg_get_function_identity_arguments(p.oid),
		    pg_catalog.pg_get_function_result(p.oid)
		  FROM pg_catalog.pg_proc p
		  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		  WHERE n.nspname = $SCHEMA$ AND p.prokind IN ('f','a','w')
		  ORDER BY 1`
		set, derr := p.t.exec(ctx, p.db(database), strings.ReplaceAll(q, "$SCHEMA$", dexQuoteLiteral(dexPostgres, schema)), 20000)
		if derr == nil {
			for _, r := range set.Rows {
				if !dexMatches(dexStr(r[0]), filter) {
					continue
				}
				page.Nodes = append(page.Nodes, dexTreeNode{
					ID: dexStr(r[0]) + "(" + dexStr(r[1]) + ")", Name: dexStr(r[0]),
					Kind: dexKindFunction, Folder: "Functions",
					Detail: "(" + dexStr(r[1]) + ") → " + dexStr(r[2]),
				})
			}
		}
	}
	return page, nil
}

// ---------------------------------------------------------------- describe

func (p *dexPGAdapter) DescribeObject(ctx context.Context, database string, ref dexObjectRef) (dexObjectDetail, error) {
	schema := ref.Schema
	if schema == "" {
		schema = "public"
	}
	db := p.db(database)
	d := dexObjectDetail{Ref: ref, Kind: ref.Kind}
	sl, tl := dexQuoteLiteral(dexPostgres, schema), dexQuoteLiteral(dexPostgres, ref.Name)

	colQ := `SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
	    COALESCE(pg_catalog.pg_get_expr(ad.adbin, ad.adrelid), ''),
	    a.attidentity <> '' OR a.attgenerated <> '',
	    COALESCE(col_description(a.attrelid, a.attnum), ''), a.attnum
	  FROM pg_catalog.pg_attribute a
	  JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
	  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	  LEFT JOIN pg_catalog.pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
	  WHERE n.nspname = ` + sl + ` AND c.relname = ` + tl + ` AND a.attnum > 0 AND NOT a.attisdropped
	  ORDER BY a.attnum`
	set, derr := p.t.exec(ctx, db, colQ, 5000)
	if derr != nil {
		return d, derr
	}
	for _, r := range set.Rows {
		notnull, _ := dexBool(r[2])
		gen, _ := dexBool(r[4])
		pos, _ := dexInt(r[6])
		ci := dexColumnInfo{
			Name: dexStr(r[0]), Type: dexStr(r[1]), Nullable: !notnull,
			Default: dexStr(r[3]), Comment: dexStr(r[5]), Position: int(pos),
		}
		if gen {
			ci.Extra = "generated"
		}
		d.Columns = append(d.Columns, ci)
	}

	// Indexes, with the primary key called out. pg_get_indexdef gives the exact
	// definition the DDL tab prints, so nothing here has to be reassembled.
	idxQ := `SELECT i.relname, ix.indisunique, ix.indisprimary,
	    pg_catalog.pg_get_indexdef(ix.indexrelid),
	    am.amname, pg_catalog.pg_relation_size(i.oid),
	    COALESCE(pg_catalog.pg_get_expr(ix.indpred, ix.indrelid), '')
	  FROM pg_catalog.pg_index ix
	  JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid
	  JOIN pg_catalog.pg_class c ON c.oid = ix.indrelid
	  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	  JOIN pg_catalog.pg_am am ON am.oid = i.relam
	  WHERE n.nspname = ` + sl + ` AND c.relname = ` + tl + `
	  ORDER BY ix.indisprimary DESC, i.relname`
	if set, derr := p.t.exec(ctx, db, idxQ, 2000); derr == nil {
		for _, r := range set.Rows {
			uniq, _ := dexBool(r[1])
			prim, _ := dexBool(r[2])
			ii := dexIndexInfo{
				Name: dexStr(r[0]), Unique: uniq, Primary: prim,
				Type: dexStr(r[4]), Columns: dexPGIndexColumns(dexStr(r[3])),
				Partial: dexStr(r[6]),
			}
			if b, ok := dexInt(r[5]); ok {
				ii.Size = &b
			}
			d.Indexes = append(d.Indexes, ii)
			if prim {
				d.PrimaryKey = ii.Columns
			}
		}
	}

	conQ := `SELECT con.conname, con.contype, pg_catalog.pg_get_constraintdef(con.oid)
	  FROM pg_catalog.pg_constraint con
	  JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
	  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	  WHERE n.nspname = ` + sl + ` AND c.relname = ` + tl + ` ORDER BY con.contype, con.conname`
	if set, derr := p.t.exec(ctx, db, conQ, 2000); derr == nil {
		for _, r := range set.Rows {
			name, typ, def := dexStr(r[0]), dexStr(r[1]), dexStr(r[2])
			if typ == "f" {
				d.ForeignKeys = append(d.ForeignKeys, dexPGParseFK(name, def))
				continue
			}
			d.Constraints = append(d.Constraints, dexConstraintInfo{
				Name: name, Type: dexPGConType(typ), Definition: def,
			})
		}
	}

	if ref.Kind == dexKindTable || ref.Kind == "" {
		trgQ := `SELECT t.tgname, pg_catalog.pg_get_triggerdef(t.oid)
		  FROM pg_catalog.pg_trigger t
		  JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = ` + sl + ` AND c.relname = ` + tl + ` AND NOT t.tgisinternal ORDER BY 1`
		if set, derr := p.t.exec(ctx, db, trgQ, 500); derr == nil {
			for _, r := range set.Rows {
				d.Triggers = append(d.Triggers, dexTreeNode{ID: dexStr(r[0]), Name: dexStr(r[0]), Kind: dexKindTrigger, Detail: dexStr(r[1])})
			}
		}
	}

	statQ := `SELECT c.reltuples::bigint, pg_catalog.pg_total_relation_size(c.oid),
	    pg_catalog.pg_table_size(c.oid), pg_catalog.pg_indexes_size(c.oid),
	    COALESCE(pg_catalog.pg_get_userbyid(c.relowner),''), c.relpersistence,
	    COALESCE(am.amname,''), COALESCE(obj_description(c.oid,'pg_class'),'')
	  FROM pg_catalog.pg_class c
	  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	  LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam
	  WHERE n.nspname = ` + sl + ` AND c.relname = ` + tl
	if set, derr := p.t.exec(ctx, db, statQ, 1); derr == nil && len(set.Rows) == 1 {
		r := set.Rows[0]
		if v, ok := dexInt(r[0]); ok && v >= 0 {
			d.RowEstimate = &v
		}
		if v, ok := dexInt(r[1]); ok {
			d.Bytes = &v
		}
		add := func(l, v string) {
			if v != "" {
				d.Props = append(d.Props, dexProp{Label: l, Value: v})
			}
		}
		add("Owner", dexStr(r[4]))
		add("Persistence", map[string]string{"p": "permanent", "u": "unlogged", "t": "temporary"}[dexStr(r[5])])
		add("Access method", dexStr(r[6]))
		if v, ok := dexInt(r[2]); ok {
			add("Table size", byteSizeLabel(v))
		}
		if v, ok := dexInt(r[3]); ok {
			add("Index size", byteSizeLabel(v))
		}
		add("Comment", dexStr(r[7]))
	}

	if ref.Kind == dexKindView || ref.Kind == dexKindMatView {
		q := `SELECT pg_catalog.pg_get_viewdef(c.oid, true) FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = ` + sl + ` AND c.relname = ` + tl
		if set, derr := p.t.exec(ctx, db, q, 1); derr == nil && len(set.Rows) == 1 {
			verb := "VIEW"
			if ref.Kind == dexKindMatView {
				verb = "MATERIALIZED VIEW"
			}
			d.DDL = fmt.Sprintf("CREATE %s %s.%s AS\n%s",
				verb, dexQuoteIdent(dexPostgres, schema), dexQuoteIdent(dexPostgres, ref.Name), dexStr(set.Rows[0][0]))
		}
	} else {
		d.DDL = dexPGBuildDDL(schema, ref.Name, d)
	}

	d.Editable, d.EditReason = dexEditableFrom(d, p.caps.EditableRows)
	return d, nil
}

// dexPGIndexColumns pulls the column list out of a pg_get_indexdef string. Reading
// it back off the definition rather than joining pg_attribute through indkey is
// deliberate: an expression index has no attribute to join to, and the definition is
// the only place its expression is written down.
func dexPGIndexColumns(def string) []string {
	i := strings.Index(def, "(")
	if i < 0 {
		return nil
	}
	depth, end := 0, -1
	for j := i; j < len(def); j++ {
		switch def[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = j
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}
	var out []string
	depth = 0
	cur := strings.Builder{}
	for _, c := range def[i+1 : end] {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
		}
		cur.WriteRune(c)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func dexPGConType(t string) string {
	switch t {
	case "p":
		return "PRIMARY KEY"
	case "u":
		return "UNIQUE"
	case "c":
		return "CHECK"
	case "x":
		return "EXCLUDE"
	case "t":
		return "TRIGGER"
	}
	return t
}

// dexPGParseFK reads a FOREIGN KEY constraint definition. pg_get_constraintdef is the
// canonical form, so parsing it gives the same answer the server would print.
func dexPGParseFK(name, def string) dexForeignKeyInfo {
	fk := dexForeignKeyInfo{Name: name}
	rest := def
	if i := strings.Index(rest, "FOREIGN KEY ("); i >= 0 {
		rest = rest[i+len("FOREIGN KEY ("):]
		if j := strings.Index(rest, ")"); j >= 0 {
			fk.Columns = dexSplitTrim(rest[:j])
			rest = rest[j+1:]
		}
	}
	if i := strings.Index(rest, "REFERENCES "); i >= 0 {
		rest = rest[i+len("REFERENCES "):]
		if j := strings.Index(rest, "("); j >= 0 {
			ref := strings.TrimSpace(rest[:j])
			if dot := strings.LastIndex(ref, "."); dot >= 0 {
				fk.RefSchema, fk.RefTable = dexUnquote(ref[:dot]), dexUnquote(ref[dot+1:])
			} else {
				fk.RefTable = dexUnquote(ref)
			}
			rest = rest[j+1:]
			if k := strings.Index(rest, ")"); k >= 0 {
				fk.RefColumns = dexSplitTrim(rest[:k])
				rest = rest[k+1:]
			}
		}
	}
	for _, act := range []struct {
		kw  string
		set *string
	}{
		{"ON UPDATE ", &fk.OnUpdate}, {"ON DELETE ", &fk.OnDelete},
	} {
		if i := strings.Index(rest, act.kw); i >= 0 {
			v := strings.TrimSpace(rest[i+len(act.kw):])
			for _, stop := range []string{" ON ", " DEFERRABLE", " NOT "} {
				if j := strings.Index(v, stop); j >= 0 {
					v = v[:j]
				}
			}
			*act.set = strings.TrimSpace(v)
		}
	}
	return fk
}

func dexSplitTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = dexUnquote(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dexUnquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// dexPGBuildDDL reassembles a CREATE TABLE. PostgreSQL has no SHOW CREATE TABLE and
// pg_dump is not available inside the app, so the statement is rebuilt from the
// catalogue rows already fetched. It is labelled as reconstructed in the UI, because
// it is a faithful description rather than a byte-for-byte replay of what was run.
func dexPGBuildDDL(schema, name string, d dexObjectDetail) string {
	if len(d.Columns) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (\n", dexQuoteIdent(dexPostgres, schema), dexQuoteIdent(dexPostgres, name))
	parts := make([]string, 0, len(d.Columns)+len(d.Constraints))
	for _, c := range d.Columns {
		line := "    " + dexQuoteIdent(dexPostgres, c.Name) + " " + c.Type
		if c.Default != "" {
			if c.Extra == "generated" {
				line += " GENERATED ALWAYS AS (" + c.Default + ") STORED"
			} else {
				line += " DEFAULT " + c.Default
			}
		}
		if !c.Nullable {
			line += " NOT NULL"
		}
		parts = append(parts, line)
	}
	for _, c := range d.Constraints {
		parts = append(parts, "    CONSTRAINT "+dexQuoteIdent(dexPostgres, c.Name)+" "+c.Definition)
	}
	for _, f := range d.ForeignKeys {
		ref := dexQuoteIdent(dexPostgres, f.RefTable)
		if f.RefSchema != "" {
			ref = dexQuoteIdent(dexPostgres, f.RefSchema) + "." + ref
		}
		line := fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
			dexQuoteIdent(dexPostgres, f.Name), strings.Join(dexQuoteAll(dexPostgres, f.Columns), ", "),
			ref, strings.Join(dexQuoteAll(dexPostgres, f.RefColumns), ", "))
		if f.OnUpdate != "" {
			line += " ON UPDATE " + f.OnUpdate
		}
		if f.OnDelete != "" {
			line += " ON DELETE " + f.OnDelete
		}
		parts = append(parts, line)
	}
	b.WriteString(strings.Join(parts, ",\n"))
	b.WriteString("\n);\n")
	for _, ix := range d.Indexes {
		if ix.Primary {
			continue
		}
		fmt.Fprintf(&b, "\n-- index %s\n", ix.Name)
	}
	return b.String()
}

func dexQuoteAll(engine string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, dexQuoteIdent(engine, n))
	}
	return out
}

// ---------------------------------------------------------------- query

func (p *dexPGAdapter) Query(ctx context.Context, req dexQueryRequest) (dexResult, error) {
	res := dexResult{Engine: dexPostgres, Limit: req.Limit, ReadOnly: !p.caps.EditableRows && p.t.readOnly()}
	src := strings.TrimSpace(req.SQL)
	if src == "" {
		res.Error = &dexError{Engine: dexPostgres, Message: "nothing to run", Display: "nothing to run"}
		return res, nil
	}
	if p.t.readOnly() {
		if e := dexReadOnlyRefusal(dexPostgres, src); e != nil {
			res.Error = e
			return res, nil
		}
	}
	start := time.Now()
	stmts := dexSplitSQL(src)
	for _, st := range stmts {
		stmt := st.Text
		if req.Explain {
			stmt = dexPGExplainStmt(stmt)
		}
		set, derr := p.t.exec(ctx, p.db(req.Database), stmt, dexClampLimit(req.Limit))
		if derr != nil {
			derr.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
			derr.Position += st.Offset // map back to the whole submission
			res.Error = derr
			res.DurationMs = derr.ElapsedMs
			return res, nil
		}
		set.Statement = st.Text
		if req.Explain {
			set.Kind = dexSetExplain
			if len(set.Rows) == 1 && len(set.Columns) == 1 {
				if s := dexStr(set.Rows[0][0]); strings.HasPrefix(strings.TrimSpace(s), "[") {
					// EXPLAIN (FORMAT JSON) returns the whole plan in one cell.
					// Carrying it as a payload keeps a richer plan view possible
					// later without changing anything the grid does today.
					set = dexJSONSet(json.RawMessage(s))
					set.Statement = st.Text
				}
			}
		}
		res.Sets = append(res.Sets, set)
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	return res, nil
}

// dexPGExplainStmt turns a statement into a plan request. FORMAT JSON because that is
// the form a plan visualiser needs, and deliberately without ANALYZE: ANALYZE runs
// the query, and a user asking what a DELETE would do should not have it happen.
func dexPGExplainStmt(stmt string) string {
	up := strings.ToUpper(strings.TrimSpace(stmt))
	if strings.HasPrefix(up, "EXPLAIN") {
		return stmt
	}
	return "EXPLAIN (FORMAT JSON, VERBOSE, COSTS) " + stmt
}

// ---------------------------------------------------------------- network transport

// dexPGNet dials PostgreSQL with pgx. PostgreSQL connections are per-database, so one
// pool is held per database name and opened on first use — switching database in the
// tree must not cost a reconnect every time you click.
type dexPGNet struct {
	dsn func(database string) string
	mu  sync.Mutex
	db  map[string]*sql.DB
}

func (n *dexPGNet) readOnly() bool { return false }

func (n *dexPGNet) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, d := range n.db {
		d.Close()
	}
	n.db = nil
}

func (n *dexPGNet) pool(database string) (*sql.DB, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.db == nil {
		n.db = map[string]*sql.DB{}
	}
	if d, ok := n.db[database]; ok {
		return d, nil
	}
	d, err := sql.Open("pgx", n.dsn(database))
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(4)
	d.SetMaxIdleConns(2)
	d.SetConnMaxIdleTime(5 * time.Minute)
	n.db[database] = d
	return d, nil
}

func (n *dexPGNet) exec(ctx context.Context, database, stmt string, limit int) (dexResultSet, *dexError) {
	db, err := n.pool(database)
	if err != nil {
		return dexResultSet{}, &dexError{Engine: dexPostgres, Message: err.Error(), Display: err.Error()}
	}
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		// A statement that returns nothing (an INSERT without RETURNING) is not an
		// error; pgx reports it through Query as an ordinary empty result, so the
		// only thing reaching here is a real failure.
		return dexResultSet{}, dexPGError(err, ctx)
	}
	defer rows.Close()
	set, err := dexScanRows(rows, limit)
	if err != nil {
		return set, dexPGError(err, ctx)
	}
	if len(set.Columns) == 0 {
		set.Kind = dexSetAffected
	}
	return set, nil
}

// dexPGError turns a driver error into the presented form, keeping the parts a user
// would see in psql — the SQLSTATE, the detail, the hint and the character position —
// and nothing a user would not, which is why the DSN never appears in it.
func dexPGError(err error, ctx context.Context) *dexError {
	if ctx != nil && ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return &dexError{Engine: dexPostgres, Code: "DBX_CANCELED", Name: "Canceled",
			Message: "the query was cancelled", Display: "the query was cancelled"}
	}
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &dexError{Engine: dexPostgres, Code: "DBX_TIMEOUT", Name: "Timeout",
			Message: "the query exceeded its time limit", Display: "the query exceeded its time limit"}
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		e := &dexError{
			Engine: dexPostgres, Message: pg.Message, SQLState: pg.Code, Name: pg.Severity,
			Detail: pg.Detail, Hint: pg.Hint, Position: int(pg.Position),
		}
		e.Display = fmt.Sprintf("ERROR:  %s: %s", pg.Code, pg.Message)
		return e
	}
	return &dexError{Engine: dexPostgres, Message: err.Error(), Display: err.Error()}
}

// ---------------------------------------------------------------- exec transport

// dexPGExec runs psql inside a container, for a PostgreSQL that listens on loopback
// only. See dbexplorer_pmm.go for why that is PMM's situation and for the read-only
// guarantee this transport carries.
type dexPGExec struct {
	run  dexExecRunner
	user string // the psql -U role
	ro   bool
}

func (x *dexPGExec) readOnly() bool { return x.ro }
func (x *dexPGExec) close()         {}

// dexPGColsMarker / dexPGRowsMarker separate the two halves of the script's output.
// psql writes them with \echo, so they cannot be confused with data: a value would
// have to be a whole line equal to the marker, and the markers are not words.
const (
	dexPGColsMarker = "__DBX_COLS__"
	dexPGRowsMarker = "__DBX_ROWS__"
)

// dexPGRowsQuery renders one bounded page of a query as positional JSON arrays.
// The %d is limit+1: one row past the ceiling is fetched so "truncated" can be
// stated as a fact rather than inferred from a result that happens to be full.
const dexPGRowsQuery = `SELECT COALESCE(json_agg(arr), '[]'::json) FROM (
  SELECT (SELECT json_agg(e.value ORDER BY e.ord)
          FROM json_each(to_json(_dbx)) WITH ORDINALITY AS e(key, value, ord)) AS arr
  FROM (SELECT * FROM (%s) _i LIMIT %d) _dbx
) _rows;
`

func (x *dexPGExec) exec(ctx context.Context, database, stmt string, limit int) (dexResultSet, *dexError) {
	script, wrapped := dexPGScript(database, stmt, limit, x.ro)
	argv := []string{"psql", "-U", x.user, "-d", database, "-X", "-q", "-t", "-A", "-F", "|",
		"-P", "footer=off", "-P", "pager=off", "-v", "ON_ERROR_STOP=1", "-f", "-"}
	res, err := x.run.run(ctx, argv, nil, []byte(script))
	if err != nil || res.Code != 0 {
		if e := dexPGParsePsqlError(res, err); e != nil {
			return dexResultSet{}, e
		}
		return dexResultSet{}, dexExecErr(dexPostgres, res, err)
	}
	if !wrapped {
		return dexPGParsePlain(res.Stdout, limit), nil
	}
	return dexPGParseScript(res.Stdout, limit)
}

// dexPGScript builds the psql input for one statement.
//
// The interesting half is the wrapped form, and it is built the way it is because
// psql's own output formats each lose something a database client must not lose.
// `--csv` renders SQL NULL and the empty string identically — the distinction
// somebody opens a client to check. Its aligned tables are decoration. And neither
// carries a column's type.
//
// So the wrapped form asks for three things at once:
//
//	<q> \gdesc              the real column names and PostgreSQL types, from the
//	                        server's own describe — and without running the query
//
//	json_each(to_json(row)) WITH ORDINALITY
//	                        each row as a positional JSON array. Positional, so two
//	                        columns called `x` both survive (an object would keep
//	                        one); JSON, so NULL stays null and "" stays "", a wide
//	bigint keeps every digit, and a json/jsonb column arrives nested rather than
//	flattened into a string.
//
// A statement that cannot be a subquery (SHOW, EXPLAIN) is sent unwrapped and read
// from psql's unaligned output. Those return text and never a NULL, so nothing is
// lost by it.
func dexPGScript(database, stmt string, limit int, readOnly bool) (script string, wrapped bool) {
	var b strings.Builder
	b.WriteString("\\set VERBOSITY verbose\n")
	if readOnly {
		// The server-side half of read-only: PostgreSQL refuses any write inside
		// this transaction with SQLSTATE 25006, whatever the statement looked like
		// to the classifier that let it through.
		b.WriteString("BEGIN READ ONLY;\n")
	}
	verb := dexVerbOf(stmt)
	trimmed := strings.TrimRight(strings.TrimSpace(stmt), ";")
	switch verb {
	case "SELECT", "WITH", "TABLE", "VALUES":
		wrapped = true
		b.WriteString("\\echo " + dexPGColsMarker + "\n")
		b.WriteString(trimmed + " \\gdesc\n")
		b.WriteString("\\echo " + dexPGRowsMarker + "\n")
		fmt.Fprintf(&b, dexPGRowsQuery, trimmed, limit+1)
	default:
		b.WriteString(trimmed + ";\n")
	}
	if readOnly {
		b.WriteString("COMMIT;\n")
	}
	return b.String(), wrapped
}

// dexPGParseScript reads the two-part output of the wrapped form.
func dexPGParseScript(out string, limit int) (dexResultSet, *dexError) {
	set := dexResultSet{Kind: dexSetRows, Rows: [][]any{}}
	lines := strings.Split(out, "\n")
	stage := 0
	var jsonLines []string
	for _, ln := range lines {
		t := strings.TrimRight(ln, "\r")
		switch strings.TrimSpace(t) {
		case dexPGColsMarker:
			stage = 1
			continue
		case dexPGRowsMarker:
			stage = 2
			continue
		}
		switch stage {
		case 1:
			if i := strings.LastIndex(t, "|"); i > 0 {
				name, typ := t[:i], t[i+1:]
				set.Columns = append(set.Columns, dexColumn{Name: name, DatabaseType: typ, SemanticType: dexSemanticOf(typ)})
			}
		case 2:
			jsonLines = append(jsonLines, t)
		}
	}
	raw := strings.TrimSpace(strings.Join(jsonLines, "\n"))
	if raw == "" {
		return set, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var rows [][]json.RawMessage
	if err := dec.Decode(&rows); err != nil {
		return set, &dexError{Engine: dexPostgres, Message: "could not read the result: " + err.Error(),
			Display: "could not read the result"}
	}
	for i, r := range rows {
		if i >= limit {
			set.Truncated = true
			break
		}
		row := make([]any, len(set.Columns))
		for j := range set.Columns {
			if j < len(r) {
				row[j] = dexPGJSONCell(r[j], set.Columns[j].SemanticType)
			}
		}
		set.Rows = append(set.Rows, row)
	}
	set.RowCount = len(set.Rows)
	return set, nil
}

// dexPGJSONCell converts one JSON value from psql into a grid cell, keeping a wide
// integer exact rather than letting it round through a float64.
func dexPGJSONCell(raw json.RawMessage, sem string) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	switch x := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			if i > 1<<53 || i < -(1<<53) {
				return dexBigNum{Marker: "bignum", Text: x.String()}
			}
			return i
		}
		f, err := strconv.ParseFloat(x.String(), 64)
		if err != nil {
			return x.String()
		}
		return f
	case string:
		return x
	case bool:
		return x
	}
	// A nested object or array (json/jsonb, a composite, an array column) keeps its
	// JSON text; the grid's JSON viewer opens it and nothing about it is flattened.
	return string(raw)
}

// dexPGParsePlain reads psql's unaligned output for an unwrapped statement.
func dexPGParsePlain(out string, limit int) dexResultSet {
	set := dexResultSet{Kind: dexSetRows, Rows: [][]any{}, Columns: []dexColumn{{Name: "result", SemanticType: dexSemString}}}
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if len(set.Rows) >= limit {
			set.Truncated = true
			break
		}
		set.Rows = append(set.Rows, []any{strings.TrimRight(ln, "\r")})
	}
	set.RowCount = len(set.Rows)
	return set
}

// dexPGParsePsqlError reads psql's verbose error format back into the structured
// form, so an error from the exec transport reads the same as one from the driver:
//
//	psql:<file>:<line>: ERROR:  42P01: relation "foo" does not exist
//	LINE 1: SELECT * FROM foo;
//	                      ^
func dexPGParsePsqlError(res ExecResult, err error) *dexError {
	if err != nil {
		return nil
	}
	txt := res.Stderr
	i := strings.Index(txt, "ERROR:  ")
	if i < 0 {
		return nil
	}
	rest := txt[i+len("ERROR:  "):]
	line := rest
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		line = rest[:j]
	}
	e := &dexError{Engine: dexPostgres, Name: "ERROR"}
	if j := strings.Index(line, ": "); j > 0 && j <= 6 {
		e.SQLState, e.Message = line[:j], line[j+2:]
	} else {
		e.Message = line
	}
	for _, kw := range []struct {
		key string
		set *string
	}{{"DETAIL:  ", &e.Detail}, {"HINT:  ", &e.Hint}} {
		if k := strings.Index(rest, kw.key); k >= 0 {
			v := rest[k+len(kw.key):]
			if j := strings.IndexByte(v, '\n'); j >= 0 {
				v = v[:j]
			}
			*kw.set = v
		}
	}
	// The caret line under "LINE n:" gives the character offset the server objected
	// to, which is what lets the editor put the cursor there.
	if k := strings.Index(rest, "\nLINE "); k >= 0 {
		seg := rest[k+1:]
		nl := strings.IndexByte(seg, '\n')
		if nl > 0 {
			head := seg[:nl]
			caret := seg[nl+1:]
			if c := strings.IndexByte(caret, '^'); c >= 0 {
				if p := strings.Index(head, ": "); p > 0 {
					e.Position = c - (p + 2) + 1
					if e.Position < 0 {
						e.Position = 0
					}
				}
			}
		}
	}
	if e.SQLState != "" {
		e.Display = fmt.Sprintf("ERROR:  %s: %s", e.SQLState, e.Message)
	} else {
		e.Display = "ERROR:  " + e.Message
	}
	return e
}

// ---------------------------------------------------------------- cell helpers

// dexStr / dexInt / dexBool read a cell out of a metadata result. They exist because
// the two transports hand back slightly different Go types for the same catalogue
// column — pgx gives typed values, psql gives whatever JSON carried — and every
// catalogue query in this file is written once for both.
func dexStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case dexBigNum:
		return x.Text
	}
	return fmt.Sprintf("%v", v)
}

func dexInt(v any) (int64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	case dexBigNum:
		i, err := strconv.ParseInt(x.Text, 10, 64)
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err == nil {
			return i, true
		}
		f, err2 := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return int64(f), err2 == nil
	}
	return 0, false
}

func dexBool(v any) (bool, bool) {
	switch x := v.(type) {
	case nil:
		return false, false
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "t", "true", "1", "yes", "on":
			return true, true
		case "f", "false", "0", "no", "off":
			return false, true
		}
	case int64:
		return x != 0, true
	case float64:
		return x != 0, true
	}
	return false, false
}

// dexMatches is the tree's filter: a case-insensitive substring, which is what a
// person typing into a schema filter means.
func dexMatches(name, filter string) bool {
	if filter == "" {
		return true
	}
	return strings.Contains(strings.ToLower(name), strings.ToLower(filter))
}

// dexEditableFrom decides whether the grid may offer edit and delete for an object.
// The rule is the one that matters for safety: a row must be identifiable by a key
// the database guarantees is unique. A primary key qualifies; a unique index over
// non-nullable columns qualifies; a set of visible column values does not, because
// an UPDATE built from those would silently change every duplicate.
func dexEditableFrom(d dexObjectDetail, allowed bool) (bool, string) {
	if !allowed {
		return false, "this connection is read-only"
	}
	if d.Kind != dexKindTable && d.Kind != "" {
		return false, "only a table's rows can be edited in place"
	}
	if len(d.PrimaryKey) > 0 {
		return true, ""
	}
	nullable := map[string]bool{}
	for _, c := range d.Columns {
		nullable[c.Name] = c.Nullable
	}
	for _, ix := range d.Indexes {
		if !ix.Unique || len(ix.Columns) == 0 {
			continue
		}
		ok := true
		for _, c := range ix.Columns {
			if _, known := nullable[c]; !known || nullable[c] {
				ok = false
				break
			}
		}
		if ok {
			return true, ""
		}
	}
	return false, "this table has no primary key and no unique index over non-nullable columns, so a single row cannot be identified safely"
}

// dexSortTreeNodes orders a listing: folders keep their declared order, names sort
// within them, and system objects sink below user ones.
func dexSortTreeNodes(nodes []dexTreeNode, folders []string) {
	rank := map[string]int{}
	for i, f := range folders {
		rank[f] = i
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Folder != nodes[j].Folder {
			return rank[nodes[i].Folder] < rank[nodes[j].Folder]
		}
		if nodes[i].System != nodes[j].System {
			return !nodes[i].System
		}
		return nodes[i].Name < nodes[j].Name
	})
}
