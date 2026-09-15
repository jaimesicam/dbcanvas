package main

// Database Explorer — the MySQL family: Percona Server, Percona XtraDB Cluster,
// MySQL Community, MariaDB, Group Replication members, and everything reached
// through HAProxy, ProxySQL or MySQL Router.
//
// They are one adapter because they are one wire protocol and one catalogue. Where
// they genuinely differ — MariaDB has no `performance_schema` JSON explain, older
// servers have no `information_schema.CHECK_CONSTRAINTS` — the difference is handled
// by asking and tolerating the absence, rather than by branching on a version
// string, because a version string on a lab canvas can be anything.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type dexMySQLAdapter struct {
	db   *sql.DB
	caps dexCaps
}

func (m *dexMySQLAdapter) Capabilities() dexCaps { return m.caps }

func (m *dexMySQLAdapter) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}

func (m *dexMySQLAdapter) TestConnection(ctx context.Context) error { return m.db.PingContext(ctx) }

func (m *dexMySQLAdapter) Version(ctx context.Context) (string, error) {
	var v, comment string
	err := m.db.QueryRowContext(ctx, "SELECT VERSION(), @@version_comment").Scan(&v, &comment)
	if err != nil {
		return "", err
	}
	if comment != "" {
		return v + " (" + comment + ")", nil
	}
	return v, nil
}

// dexMySQLSystemDB are the server's own schemas. They are listed — reading them is
// half of what a troubleshooting session is — but marked, so they sort below the
// databases somebody actually created.
var dexMySQLSystemDB = map[string]bool{
	"information_schema": true, "performance_schema": true, "mysql": true, "sys": true,
}

func (m *dexMySQLAdapter) ListDatabases(ctx context.Context) ([]dexTreeNode, error) {
	// DATA_LENGTH summed per schema rather than per table: the tree wants one
	// number per database and this is one pass over the table catalogue instead of
	// one query per database.
	const q = `SELECT s.SCHEMA_NAME, s.DEFAULT_CHARACTER_SET_NAME, s.DEFAULT_COLLATION_NAME,
	    COALESCE(t.sz, 0)
	  FROM information_schema.SCHEMATA s
	  LEFT JOIN (SELECT TABLE_SCHEMA, SUM(DATA_LENGTH + INDEX_LENGTH) sz
	             FROM information_schema.TABLES GROUP BY TABLE_SCHEMA) t
	    ON t.TABLE_SCHEMA = s.SCHEMA_NAME
	  ORDER BY s.SCHEMA_NAME`
	rows, err := m.db.QueryContext(ctx, q)
	if err != nil {
		return nil, dexMySQLError(err, ctx)
	}
	defer rows.Close()
	var out []dexTreeNode
	for rows.Next() {
		var name, charset, collation string
		var size sql.NullInt64
		if err := rows.Scan(&name, &charset, &collation, &size); err != nil {
			return nil, dexMySQLError(err, ctx)
		}
		n := dexTreeNode{ID: name, Name: name, Kind: dexKindDatabase, HasChild: true,
			System: dexMySQLSystemDB[name], Detail: charset}
		if size.Valid && size.Int64 > 0 {
			b := size.Int64
			n.Bytes = &b
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListSchemas is empty for MySQL: a schema and a database are the same thing, and
// the capability flag says so, so the tree draws one level fewer rather than a
// level with one node in it.
func (m *dexMySQLAdapter) ListSchemas(ctx context.Context, database string) ([]dexTreeNode, error) {
	return nil, nil
}

var dexMySQLFolders = []string{"Tables", "Views", "Procedures", "Functions", "Triggers"}

func (m *dexMySQLAdapter) ListObjects(ctx context.Context, database, schema, folder, cursor, filter string) (dexObjectPage, error) {
	if strings.TrimSpace(database) == "" {
		return dexObjectPage{}, fmt.Errorf("a database is required")
	}
	page := dexObjectPage{Folders: dexMySQLFolders, Nodes: []dexTreeNode{}}
	want := func(f string) bool { return folder == "" || folder == f }

	if want("Tables") || want("Views") {
		// TABLE_ROWS is InnoDB's estimate, not a count. Saying "~" in front of it in
		// the UI is the honest presentation; running COUNT(*) per table to draw a
		// tree is what this feature must never do.
		const q = `SELECT TABLE_NAME, TABLE_TYPE, ENGINE, TABLE_ROWS,
		    DATA_LENGTH + INDEX_LENGTH, COALESCE(TABLE_COMMENT, '')
		  FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`
		rows, err := m.db.QueryContext(ctx, q, database)
		if err != nil {
			return page, dexMySQLError(err, ctx)
		}
		for rows.Next() {
			var name, ttype string
			var engine, comment sql.NullString
			var nrows, size sql.NullInt64
			if rows.Scan(&name, &ttype, &engine, &nrows, &size, &comment) != nil {
				continue
			}
			kind, fold := dexKindTable, "Tables"
			if strings.Contains(strings.ToUpper(ttype), "VIEW") {
				kind, fold = dexKindView, "Views"
			}
			if !want(fold) || !dexMatches(name, filter) {
				continue
			}
			n := dexTreeNode{ID: name, Name: name, Kind: kind, Folder: fold, HasChild: true,
				Detail: comment.String, Badge: engine.String}
			if nrows.Valid {
				v := nrows.Int64
				n.Rows = &v
			}
			if size.Valid {
				v := size.Int64
				n.Bytes = &v
			}
			page.Nodes = append(page.Nodes, n)
		}
		rows.Close()
	}

	if want("Procedures") || want("Functions") {
		const q = `SELECT ROUTINE_NAME, ROUTINE_TYPE, COALESCE(DTD_IDENTIFIER, '')
		  FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? ORDER BY ROUTINE_NAME`
		if rows, err := m.db.QueryContext(ctx, q, database); err == nil {
			for rows.Next() {
				var name, rtype, ret string
				if rows.Scan(&name, &rtype, &ret) != nil {
					continue
				}
				kind, fold := dexKindProcedure, "Procedures"
				if strings.EqualFold(rtype, "FUNCTION") {
					kind, fold = dexKindFunction, "Functions"
				}
				if want(fold) && dexMatches(name, filter) {
					page.Nodes = append(page.Nodes, dexTreeNode{ID: name, Name: name, Kind: kind, Folder: fold, Detail: ret})
				}
			}
			rows.Close()
		}
	}

	if want("Triggers") {
		const q = `SELECT TRIGGER_NAME, EVENT_MANIPULATION, EVENT_OBJECT_TABLE, ACTION_TIMING
		  FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME`
		if rows, err := m.db.QueryContext(ctx, q, database); err == nil {
			for rows.Next() {
				var name, event, table, timing string
				if rows.Scan(&name, &event, &table, &timing) != nil {
					continue
				}
				if dexMatches(name, filter) {
					page.Nodes = append(page.Nodes, dexTreeNode{
						ID: name, Name: name, Kind: dexKindTrigger, Folder: "Triggers",
						Detail: timing + " " + event + " ON " + table,
					})
				}
			}
			rows.Close()
		}
	}
	return page, nil
}

func (m *dexMySQLAdapter) DescribeObject(ctx context.Context, database string, ref dexObjectRef) (dexObjectDetail, error) {
	d := dexObjectDetail{Ref: ref, Kind: ref.Kind}

	const colQ = `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COALESCE(COLUMN_DEFAULT,''),
	    COLUMN_KEY, EXTRA, COALESCE(COLUMN_COMMENT,''), ORDINAL_POSITION
	  FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
	  ORDER BY ORDINAL_POSITION`
	rows, err := m.db.QueryContext(ctx, colQ, database, ref.Name)
	if err != nil {
		return d, dexMySQLError(err, ctx)
	}
	for rows.Next() {
		var c dexColumnInfo
		var nullable string
		if rows.Scan(&c.Name, &c.Type, &nullable, &c.Default, &c.Key, &c.Extra, &c.Comment, &c.Position) != nil {
			continue
		}
		c.Nullable = strings.EqualFold(nullable, "YES")
		d.Columns = append(d.Columns, c)
	}
	rows.Close()

	// STATISTICS carries one row per column per index, in SEQ_IN_INDEX order, so
	// the index list is assembled rather than queried per index.
	const idxQ = `SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, INDEX_TYPE
	  FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
	  ORDER BY INDEX_NAME, SEQ_IN_INDEX`
	if rows, err := m.db.QueryContext(ctx, idxQ, database, ref.Name); err == nil {
		byName := map[string]*dexIndexInfo{}
		var order []string
		for rows.Next() {
			var name, col, itype string
			var nonUniq, seq int
			if rows.Scan(&name, &nonUniq, &seq, &col, &itype) != nil {
				continue
			}
			ix, ok := byName[name]
			if !ok {
				ix = &dexIndexInfo{Name: name, Unique: nonUniq == 0, Primary: name == "PRIMARY", Type: itype}
				byName[name] = ix
				order = append(order, name)
			}
			ix.Columns = append(ix.Columns, col)
		}
		rows.Close()
		for _, name := range order {
			ix := *byName[name]
			d.Indexes = append(d.Indexes, ix)
			if ix.Primary {
				d.PrimaryKey = ix.Columns
			}
		}
	}

	const fkQ = `SELECT k.CONSTRAINT_NAME, k.COLUMN_NAME, k.REFERENCED_TABLE_SCHEMA,
	    k.REFERENCED_TABLE_NAME, k.REFERENCED_COLUMN_NAME,
	    COALESCE(r.UPDATE_RULE,''), COALESCE(r.DELETE_RULE,'')
	  FROM information_schema.KEY_COLUMN_USAGE k
	  LEFT JOIN information_schema.REFERENTIAL_CONSTRAINTS r
	    ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME
	  WHERE k.TABLE_SCHEMA = ? AND k.TABLE_NAME = ? AND k.REFERENCED_TABLE_NAME IS NOT NULL
	  ORDER BY k.CONSTRAINT_NAME, k.ORDINAL_POSITION`
	if rows, err := m.db.QueryContext(ctx, fkQ, database, ref.Name); err == nil {
		byName := map[string]*dexForeignKeyInfo{}
		var order []string
		for rows.Next() {
			var name, col, rs, rt, rc, onUpd, onDel string
			if rows.Scan(&name, &col, &rs, &rt, &rc, &onUpd, &onDel) != nil {
				continue
			}
			fk, ok := byName[name]
			if !ok {
				fk = &dexForeignKeyInfo{Name: name, RefSchema: rs, RefTable: rt, OnUpdate: onUpd, OnDelete: onDel}
				byName[name] = fk
				order = append(order, name)
			}
			fk.Columns = append(fk.Columns, col)
			fk.RefColumns = append(fk.RefColumns, rc)
		}
		rows.Close()
		for _, n := range order {
			d.ForeignKeys = append(d.ForeignKeys, *byName[n])
		}
	}

	// CHECK_CONSTRAINTS is 8.0.16+ on MySQL and 10.2+ on MariaDB. An older server
	// simply has no such table; the absence is expected, so it is not an error.
	const ckQ = `SELECT c.CONSTRAINT_NAME, c.CHECK_CLAUSE
	  FROM information_schema.CHECK_CONSTRAINTS c
	  JOIN information_schema.TABLE_CONSTRAINTS t
	    ON t.CONSTRAINT_SCHEMA = c.CONSTRAINT_SCHEMA AND t.CONSTRAINT_NAME = c.CONSTRAINT_NAME
	  WHERE t.TABLE_SCHEMA = ? AND t.TABLE_NAME = ?`
	if rows, err := m.db.QueryContext(ctx, ckQ, database, ref.Name); err == nil {
		for rows.Next() {
			var name, clause string
			if rows.Scan(&name, &clause) == nil {
				d.Constraints = append(d.Constraints, dexConstraintInfo{Name: name, Type: "CHECK", Definition: clause})
			}
		}
		rows.Close()
	}

	const statQ = `SELECT ENGINE, TABLE_ROWS, DATA_LENGTH, INDEX_LENGTH, DATA_FREE,
	    COALESCE(TABLE_COLLATION,''), COALESCE(CREATE_OPTIONS,''), COALESCE(TABLE_COMMENT,''),
	    COALESCE(CAST(CREATE_TIME AS CHAR), ''), COALESCE(CAST(UPDATE_TIME AS CHAR), ''),
	    COALESCE(AUTO_INCREMENT, 0), TABLE_TYPE
	  FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`
	var engine, collation, createOpts, comment, created, updated, ttype string
	var nrows, dataLen, idxLen, dataFree, autoInc sql.NullInt64
	if err := m.db.QueryRowContext(ctx, statQ, database, ref.Name).Scan(
		&engine, &nrows, &dataLen, &idxLen, &dataFree, &collation, &createOpts,
		&comment, &created, &updated, &autoInc, &ttype); err == nil {
		if nrows.Valid {
			v := nrows.Int64
			d.RowEstimate = &v
		}
		total := dataLen.Int64 + idxLen.Int64
		d.Bytes = &total
		add := func(l, v string) {
			if v != "" {
				d.Props = append(d.Props, dexProp{Label: l, Value: v})
			}
		}
		add("Engine", engine)
		add("Collation", collation)
		add("Data size", byteSizeLabel(dataLen.Int64))
		add("Index size", byteSizeLabel(idxLen.Int64))
		if dataFree.Int64 > 0 {
			add("Free space", byteSizeLabel(dataFree.Int64))
		}
		if autoInc.Valid && autoInc.Int64 > 0 {
			add("Next AUTO_INCREMENT", strconv.FormatInt(autoInc.Int64, 10))
		}
		add("Create options", createOpts)
		add("Created", created)
		add("Updated", updated)
		add("Comment", comment)
	}

	// SHOW CREATE is the server's own rendering of the object, which is what the
	// DDL tab should show — nothing reassembled, nothing approximated.
	what := "TABLE"
	switch ref.Kind {
	case dexKindView:
		what = "VIEW"
	case dexKindProcedure:
		what = "PROCEDURE"
	case dexKindFunction:
		what = "FUNCTION"
	case dexKindTrigger:
		what = "TRIGGER"
	}
	full := dexQuoteIdent(dexMySQL, database) + "." + dexQuoteIdent(dexMySQL, ref.Name)
	if rows, err := m.db.QueryContext(ctx, "SHOW CREATE "+what+" "+full); err == nil {
		cols, _ := rows.Columns()
		if rows.Next() {
			hold := make([]any, len(cols))
			ptr := make([]any, len(cols))
			for i := range hold {
				ptr[i] = &hold[i]
			}
			if rows.Scan(ptr...) == nil {
				// The DDL column is second for a table and a view, third for a
				// routine; finding it by name avoids depending on which.
				for i, c := range cols {
					if strings.HasPrefix(strings.ToLower(c), "create") {
						d.DDL = dexStr(dexCell(hold[i], dexSemString))
					}
				}
			}
		}
		rows.Close()
	}

	d.Editable, d.EditReason = dexEditableFrom(d, m.caps.EditableRows)
	return d, nil
}

func (m *dexMySQLAdapter) Query(ctx context.Context, req dexQueryRequest) (dexResult, error) {
	res := dexResult{Engine: dexMySQL, Limit: req.Limit}
	src := strings.TrimSpace(req.SQL)
	if src == "" {
		res.Error = &dexError{Engine: dexMySQL, Message: "nothing to run", Display: "nothing to run"}
		return res, nil
	}
	limit := dexClampLimit(req.Limit)
	start := time.Now()

	conn, err := m.db.Conn(ctx)
	if err != nil {
		res.Error = dexMySQLError(err, ctx)
		return res, nil
	}
	defer conn.Close()
	if db := strings.TrimSpace(req.Database); db != "" {
		if _, err := conn.ExecContext(ctx, "USE "+dexQuoteIdent(dexMySQL, db)); err != nil {
			res.Error = dexMySQLError(err, ctx)
			return res, nil
		}
	}

	for _, st := range dexSplitSQL(src) {
		stmt := st.Text
		if req.Explain {
			stmt = dexMySQLExplainStmt(stmt)
		}
		set, derr := dexMySQLRun(ctx, conn, stmt, limit)
		if derr != nil {
			derr.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
			res.Error = derr
			res.DurationMs = derr.ElapsedMs
			res.Warnings = append(res.Warnings, dexMySQLWarnings(ctx, conn)...)
			return res, nil
		}
		set.Statement = st.Text
		if req.Explain {
			set.Kind = dexSetExplain
			if len(set.Rows) == 1 && len(set.Columns) == 1 {
				if s := dexStr(set.Rows[0][0]); strings.HasPrefix(strings.TrimSpace(s), "{") {
					st2 := dexJSONSet([]byte(s))
					st2.Statement = st.Text
					set = st2
				}
			}
		}
		res.Sets = append(res.Sets, set)
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	res.Warnings = append(res.Warnings, dexMySQLWarnings(ctx, conn)...)
	return res, nil
}

// dexMySQLRun executes one statement. Whether it returns rows is decided by asking
// the driver rather than by classifying the SQL: a CALL can return rows, a SELECT
// INTO does not, and the verb is a poor guide to either.
func dexMySQLRun(ctx context.Context, conn *sql.Conn, stmt string, limit int) (dexResultSet, *dexError) {
	rows, err := conn.QueryContext(ctx, stmt)
	if err != nil {
		var me *mysql.MySQLError
		// Statements the text protocol will not return a result for come back as
		// this specific error; re-running them as Exec is the ordinary path, not a
		// fallback for something going wrong.
		if errors.As(err, &me) && me.Number == 1064 {
			return dexResultSet{}, dexMySQLError(err, ctx)
		}
		r, xerr := conn.ExecContext(ctx, stmt)
		if xerr != nil {
			return dexResultSet{}, dexMySQLError(err, ctx)
		}
		n, _ := r.RowsAffected()
		id, _ := r.LastInsertId()
		return dexResultSet{Kind: dexSetAffected, AffectedRows: n, InsertID: id}, nil
	}
	defer rows.Close()
	set, err := dexScanRows(rows, limit)
	if err != nil {
		return set, dexMySQLError(err, ctx)
	}
	if len(set.Columns) == 0 {
		set.Kind = dexSetAffected
	}
	return set, nil
}

// dexMySQLExplainStmt asks for the JSON plan. FORMAT=JSON is what a plan visualiser
// needs, and it is available on every MySQL 5.6+ and MariaDB 10.1+; a server that
// rejects it reports so as an ordinary error, which is more useful than silently
// producing a different kind of output.
func dexMySQLExplainStmt(stmt string) string {
	up := strings.ToUpper(strings.TrimSpace(stmt))
	if strings.HasPrefix(up, "EXPLAIN") || strings.HasPrefix(up, "DESCRIBE") || strings.HasPrefix(up, "DESC ") {
		return stmt
	}
	return "EXPLAIN FORMAT=JSON " + stmt
}

// dexMySQLWarnings reads SHOW WARNINGS after a statement. MySQL reports a truncated
// value, an ignored index hint and a deprecated syntax this way and not through the
// error channel, so a client that never asks leaves the user wondering why the row
// that went in is not the row that comes back.
func dexMySQLWarnings(ctx context.Context, conn *sql.Conn) []string {
	rows, err := conn.QueryContext(ctx, "SHOW WARNINGS")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var level, msg string
		var code int
		if rows.Scan(&level, &code, &msg) == nil {
			out = append(out, fmt.Sprintf("%s %d: %s", level, code, msg))
		}
	}
	return out
}

// dexMySQLError presents a MySQL error the way the server would: the number, the
// SQLSTATE and the message, in the shape a client prints them.
//
//	ERROR 1146 (42S02): Table 'shop.foo' doesn't exist
func dexMySQLError(err error, ctx context.Context) *dexError {
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &dexError{Engine: dexMySQL, Code: "DBX_TIMEOUT", Name: "Timeout",
				Message: "the query exceeded its time limit", Display: "the query exceeded its time limit"}
		}
		return &dexError{Engine: dexMySQL, Code: "DBX_CANCELED", Name: "Canceled",
			Message: "the query was cancelled", Display: "the query was cancelled"}
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		e := &dexError{
			Engine: dexMySQL, Message: me.Message,
			Code: strconv.FormatUint(uint64(me.Number), 10), SQLState: string(me.SQLState[:]),
		}
		e.Display = fmt.Sprintf("ERROR %s (%s): %s", e.Code, e.SQLState, e.Message)
		return e
	}
	return &dexError{Engine: dexMySQL, Message: err.Error(), Display: err.Error()}
}
