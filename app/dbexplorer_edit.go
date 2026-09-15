package main

// Database Explorer — changing a row, a document or a key.
//
// The rule that shapes this file: a change is only offered when the target can be
// named exactly. For a SQL table that means a primary key, or a unique index over
// non-nullable columns (dexEditableFrom); the identity is checked against that key
// here as well, so a request that skipped the UI cannot get a looser predicate than
// the grid would have allowed. An UPDATE or DELETE assembled from whatever columns
// happened to be visible is how a grid silently rewrites every duplicate row, and it
// is never generated here.
//
// Two further things hold throughout:
//
//   - values are bound, never interpolated. Identifiers are quoted with the engine's
//     own rule because they cannot be bound, and they come from the catalogue rather
//     than from the request: a column name that is not a real column of the table is
//     rejected before any SQL exists.
//   - every mutation can be previewed. The statement the server would run is returned
//     without running it, so "show me what this will do" is an answer and not a
//     promise. A delete additionally requires the client to have confirmed.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

type dexMutateRequest struct {
	ConnectionID string `json:"connectionId"`
	Database     string `json:"database"`
	Schema       string `json:"schema"`
	Object       string `json:"object"`
	Op           string `json:"op"` // insert | update | delete
	// Values are the columns to write, by name. A JSON null means SQL NULL — which
	// is why this is map[string]json.RawMessage and not map[string]string: the two
	// have to stay distinguishable all the way down.
	Values map[string]json.RawMessage `json:"values"`
	// Identity names the row: the primary key's columns, or a unique index's.
	Identity map[string]json.RawMessage `json:"identity"`
	Preview  bool                       `json:"preview"`
	Confirm  bool                       `json:"confirm"`
	// AllowWrites arms a connection that is read-only by policy, exactly as it does
	// for a query. See dexUnlock: the administrator setting has to agree as well.
	AllowWrites bool `json:"allowWrites"`
	// MongoDB.
	Document json.RawMessage `json:"document"`
	// Valkey.
	Key     string `json:"key"`
	KeyType string `json:"keyType"`
	Field   string `json:"field"`
	Value   string `json:"value"`
	Member  string `json:"member"`
	Score   string `json:"score"`
	Index   int64  `json:"index"`
	TTL     int64  `json:"ttl"`
}

type dexMutateResult struct {
	Preview      string    `json:"preview"`
	Executed     bool      `json:"executed"`
	AffectedRows int64     `json:"affectedRows"`
	DurationMs   float64   `json:"durationMs"`
	Error        *dexError `json:"error,omitempty"`
	Message      string    `json:"message,omitempty"`
}

// dexMutate applies one change. The adapter is already open and the connection
// already re-authorized; what is left is deciding whether the change is expressible
// safely, and saying so when it is not.
func (a *App) dexMutate(ctx context.Context, ad dexAdapter, t dexTarget, req dexMutateRequest) (dexMutateResult, error) {
	if t.Conn.ReadOnly || !ad.Capabilities().EditableRows {
		return dexMutateResult{Error: &dexError{
			Engine: t.Conn.Engine, Code: "DBX_READONLY", Name: "ReadOnly",
			Message: "this connection is read-only",
			Display: "this connection is read-only",
		}}, nil
	}
	if req.Op == "delete" && !req.Confirm && !req.Preview {
		return dexMutateResult{Error: &dexError{
			Engine: t.Conn.Engine, Code: "DBX_CONFIRM", Name: "ConfirmRequired",
			Message: "a delete has to be confirmed",
			Display: "a delete has to be confirmed",
		}}, nil
	}
	switch ad := ad.(type) {
	case *dexMySQLAdapter:
		return a.dexMutateMySQL(ctx, ad, t, req)
	case *dexPGAdapter:
		return a.dexMutatePG(ctx, ad, t, req)
	case *dexMongoAdapter:
		return a.dexMutateMongo(ctx, ad, t, req)
	case *dexValkeyAdapter:
		return a.dexMutateValkey(ctx, ad, t, req)
	}
	return dexMutateResult{Error: &dexError{Engine: t.Conn.Engine,
		Message: "this engine does not support editing here",
		Display: "this engine does not support editing here"}}, nil
}

// ---------------------------------------------------------------- SQL

// dexSQLMutation is the statement plus its bound values, and the human-readable
// preview. The preview interpolates the values *for display only* — it is never the
// string that is executed, which is why it can afford to be readable.
type dexSQLMutation struct {
	SQL     string
	Args    []any
	Preview string
}

// dexBuildMutation turns a request into a statement for a SQL engine.
//
// It validates in this order, and stops at the first failure: the object is named,
// the identity covers a key the database guarantees is unique, and every column
// mentioned is a real column of the object. Only then is any SQL produced.
func dexBuildMutation(engine string, detail dexObjectDetail, qualified string, req dexMutateRequest) (dexSQLMutation, *dexError) {
	fail := func(msg string) (dexSQLMutation, *dexError) {
		return dexSQLMutation{}, &dexError{Engine: engine, Code: "DBX_EDIT", Name: "CannotEdit",
			Message: msg, Display: msg}
	}
	known := map[string]bool{}
	for _, c := range detail.Columns {
		known[c.Name] = true
	}
	checkCols := func(m map[string]json.RawMessage, what string) *dexError {
		for name := range m {
			if !known[name] {
				msg := fmt.Sprintf("%q is not a column of this table", name)
				return &dexError{Engine: engine, Code: "DBX_EDIT", Name: "UnknownColumn", Message: msg, Display: msg}
			}
		}
		return nil
	}

	switch req.Op {
	case "insert":
		if len(req.Values) == 0 {
			return fail("nothing to insert")
		}
		if e := checkCols(req.Values, "values"); e != nil {
			return dexSQLMutation{}, e
		}
		// Column order follows the table's, not the map's, so the generated
		// statement is stable and readable rather than randomised per request.
		var cols []string
		var args []any
		var display []string
		for _, c := range detail.Columns {
			raw, ok := req.Values[c.Name]
			if !ok {
				continue
			}
			v, err := dexDecodeValue(raw)
			if err != nil {
				return fail(fmt.Sprintf("the value for %q is not valid JSON", c.Name))
			}
			cols = append(cols, dexQuoteIdent(engine, c.Name))
			args = append(args, v)
			display = append(display, dexDisplayValue(v))
		}
		ph := strings.TrimSuffix(strings.Repeat(dexPlaceholder(engine, 0)+", ", len(cols)), ", ")
		if engine == dexPostgres {
			ph = dexNumberedPlaceholders(1, len(cols))
		}
		sqlText := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", qualified, strings.Join(cols, ", "), ph)
		preview := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", qualified, strings.Join(cols, ", "), strings.Join(display, ", "))
		return dexSQLMutation{SQL: sqlText, Args: args, Preview: preview}, nil

	case "update", "delete":
		if e := checkCols(req.Identity, "identity"); e != nil {
			return dexSQLMutation{}, e
		}
		keyCols, derr := dexIdentityKey(engine, detail, req.Identity)
		if derr != nil {
			return dexSQLMutation{}, derr
		}
		n := 1
		var setParts, setDisplay []string
		var args []any
		if req.Op == "update" {
			if len(req.Values) == 0 {
				return fail("nothing to change")
			}
			if e := checkCols(req.Values, "values"); e != nil {
				return dexSQLMutation{}, e
			}
			for _, c := range detail.Columns {
				raw, ok := req.Values[c.Name]
				if !ok {
					continue
				}
				v, err := dexDecodeValue(raw)
				if err != nil {
					return fail(fmt.Sprintf("the value for %q is not valid JSON", c.Name))
				}
				setParts = append(setParts, dexQuoteIdent(engine, c.Name)+" = "+dexPlaceholder(engine, n))
				setDisplay = append(setDisplay, dexQuoteIdent(engine, c.Name)+" = "+dexDisplayValue(v))
				args = append(args, v)
				n++
			}
		}
		var whereParts, whereDisplay []string
		for _, name := range keyCols {
			v, err := dexDecodeValue(req.Identity[name])
			if err != nil {
				return fail(fmt.Sprintf("the identity value for %q is not valid JSON", name))
			}
			if v == nil {
				// `col = NULL` is never true, so a NULL in the identity would make
				// the predicate match nothing — or, with the wrong rewrite, match
				// everything. Refusing is the only safe answer.
				return fail(fmt.Sprintf("the identity column %q is NULL, so this row cannot be named uniquely", name))
			}
			whereParts = append(whereParts, dexQuoteIdent(engine, name)+" = "+dexPlaceholder(engine, n))
			whereDisplay = append(whereDisplay, dexQuoteIdent(engine, name)+" = "+dexDisplayValue(v))
			args = append(args, v)
			n++
		}
		where := strings.Join(whereParts, " AND ")
		whereShown := strings.Join(whereDisplay, " AND ")
		if req.Op == "delete" {
			return dexSQLMutation{
				SQL:     fmt.Sprintf("DELETE FROM %s WHERE %s", qualified, where),
				Args:    args,
				Preview: fmt.Sprintf("DELETE FROM %s WHERE %s", qualified, whereShown),
			}, nil
		}
		return dexSQLMutation{
			SQL:     fmt.Sprintf("UPDATE %s SET %s WHERE %s", qualified, strings.Join(setParts, ", "), where),
			Args:    args,
			Preview: fmt.Sprintf("UPDATE %s SET %s WHERE %s", qualified, strings.Join(setDisplay, ", "), whereShown),
		}, nil
	}
	return fail("unsupported operation " + strconv.Quote(req.Op))
}

// dexIdentityKey picks the key the WHERE clause will be built from and checks that
// the request supplied every one of its columns. The primary key is preferred; a
// unique index over non-nullable columns is accepted; nothing else is, which is the
// whole point.
func dexIdentityKey(engine string, detail dexObjectDetail, identity map[string]json.RawMessage) ([]string, *dexError) {
	has := func(cols []string) bool {
		if len(cols) == 0 {
			return false
		}
		for _, c := range cols {
			if _, ok := identity[c]; !ok {
				return false
			}
		}
		return true
	}
	if has(detail.PrimaryKey) {
		return detail.PrimaryKey, nil
	}
	nullable := map[string]bool{}
	for _, c := range detail.Columns {
		nullable[c.Name] = c.Nullable
	}
	for _, ix := range detail.Indexes {
		if !ix.Unique || !has(ix.Columns) {
			continue
		}
		ok := true
		for _, c := range ix.Columns {
			if n, known := nullable[c]; !known || n {
				ok = false
				break
			}
		}
		if ok {
			return ix.Columns, nil
		}
	}
	msg := "this row cannot be identified uniquely — the table has no primary key, and no unique index over non-nullable columns was supplied"
	return nil, &dexError{Engine: engine, Code: "DBX_NO_IDENTITY", Name: "NoRowIdentity",
		Message: msg, Display: msg,
		Hint: "Add a primary key to the table, or change the data with an explicit statement in the editor."}
}

func dexPlaceholder(engine string, n int) string {
	if engine == dexPostgres {
		return "$" + strconv.Itoa(n)
	}
	return "?"
}

func dexNumberedPlaceholders(from, count int) string {
	parts := make([]string, 0, count)
	for i := 0; i < count; i++ {
		parts = append(parts, "$"+strconv.Itoa(from+i))
	}
	return strings.Join(parts, ", ")
}

// dexDecodeValue turns one JSON value from the grid into something a driver can bind.
// A JSON null becomes a Go nil, which every driver writes as SQL NULL — which is why
// the empty string and NULL never get confused on the way back in either.
func dexDecodeValue(raw json.RawMessage) (any, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return i, nil
		}
		return strconv.ParseFloat(x.String(), 64)
	case map[string]any, []any:
		// A JSON or array column: bind the text, which is what both engines accept
		// for a json/jsonb column and what MySQL accepts for JSON.
		return s, nil
	}
	return v, nil
}

// dexDisplayValue renders a bound value for the preview only.
func dexDisplayValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

func (a *App) dexMutateMySQL(ctx context.Context, ad *dexMySQLAdapter, t dexTarget, req dexMutateRequest) (dexMutateResult, error) {
	const engine = dexMySQL
	detail, err := ad.DescribeObject(ctx, req.Database, dexObjectRef{
		Database: req.Database, Name: req.Object, Kind: dexKindTable,
	})
	if err != nil {
		return dexMutateResult{Error: dexAsDexError(engine, err)}, nil
	}
	if !detail.Editable {
		return dexMutateResult{Error: &dexError{Engine: engine, Code: "DBX_NO_IDENTITY",
			Name: "NoRowIdentity", Message: detail.EditReason, Display: detail.EditReason}}, nil
	}
	qualified := dexQuoteIdent(engine, req.Database) + "." + dexQuoteIdent(engine, req.Object)
	m, derr := dexBuildMutation(engine, detail, qualified, req)
	if derr != nil {
		return dexMutateResult{Error: derr}, nil
	}
	if req.Preview {
		return dexMutateResult{Preview: m.Preview}, nil
	}
	start := time.Now()
	res, xerr := ad.db.ExecContext(ctx, m.SQL, m.Args...)
	out := dexMutateResult{Preview: m.Preview, Executed: true,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000}
	if xerr != nil {
		out.Executed = false
		out.Error = dexMySQLError(xerr, ctx)
		return out, nil
	}
	out.AffectedRows, _ = res.RowsAffected()
	return out, nil
}

func (a *App) dexMutatePG(ctx context.Context, ad *dexPGAdapter, t dexTarget, req dexMutateRequest) (dexMutateResult, error) {
	net, ok := ad.t.(*dexPGNet)
	if !ok {
		msg := "this connection is read-only"
		return dexMutateResult{Error: &dexError{Engine: dexPostgres, Code: "DBX_READONLY",
			Name: "ReadOnly", Message: msg, Display: msg}}, nil
	}
	schema := req.Schema
	if schema == "" {
		schema = "public"
	}
	detail, err := ad.DescribeObject(ctx, req.Database, dexObjectRef{
		Database: req.Database, Schema: schema, Name: req.Object, Kind: dexKindTable,
	})
	if err != nil {
		return dexMutateResult{Error: dexAsDexError(dexPostgres, err)}, nil
	}
	if !detail.Editable {
		return dexMutateResult{Error: &dexError{Engine: dexPostgres, Code: "DBX_NO_IDENTITY",
			Name: "NoRowIdentity", Message: detail.EditReason, Display: detail.EditReason}}, nil
	}
	qualified := dexQuoteIdent(dexPostgres, schema) + "." + dexQuoteIdent(dexPostgres, req.Object)
	m, derr := dexBuildMutation(dexPostgres, detail, qualified, req)
	if derr != nil {
		return dexMutateResult{Error: derr}, nil
	}
	if req.Preview {
		return dexMutateResult{Preview: m.Preview}, nil
	}
	db, err := net.pool(ad.db(req.Database))
	if err != nil {
		return dexMutateResult{Error: dexPGError(err, ctx)}, nil
	}
	start := time.Now()
	res, xerr := db.ExecContext(ctx, m.SQL, m.Args...)
	out := dexMutateResult{Preview: m.Preview, Executed: true,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000}
	if xerr != nil {
		out.Executed = false
		out.Error = dexPGError(xerr, ctx)
		return out, nil
	}
	out.AffectedRows, _ = res.RowsAffected()
	return out, nil
}

// ---------------------------------------------------------------- MongoDB

// dexMutateMongo edits a document. `_id` is the identity — every document has one and
// it is unique by construction, which is why MongoDB needs none of the key-hunting
// the SQL path does. An update replaces the document rather than patching it, because
// that is what editing a document in a viewer means and a partial $set would silently
// leave removed fields behind.
func (a *App) dexMutateMongo(ctx context.Context, ad *dexMongoAdapter, t dexTarget, req dexMutateRequest) (dexMutateResult, error) {
	coll := ad.client.Database(req.Database).Collection(req.Object)
	mkErr := func(msg string) dexMutateResult {
		return dexMutateResult{Error: &dexError{Engine: dexMongoDB, Code: "DBX_EDIT",
			Name: "BadValue", Message: msg, Display: msg}}
	}
	switch req.Op {
	case "insert":
		var doc bson.D
		if err := bson.UnmarshalExtJSON(req.Document, false, &doc); err != nil {
			return mkErr("the document is not valid JSON: " + err.Error()), nil
		}
		preview := fmt.Sprintf("db.%s.insertOne(%s)", req.Object, strings.TrimSpace(string(req.Document)))
		if req.Preview {
			return dexMutateResult{Preview: preview}, nil
		}
		start := time.Now()
		res, err := coll.InsertOne(ctx, doc)
		if err != nil {
			return dexMutateResult{Preview: preview, Error: dexMongoError(err, ctx)}, nil
		}
		return dexMutateResult{Preview: preview, Executed: true, AffectedRows: 1,
			DurationMs: float64(time.Since(start).Microseconds()) / 1000,
			Message:    fmt.Sprintf("inserted _id %v", res.InsertedID)}, nil

	case "update", "delete":
		idRaw, ok := req.Identity["_id"]
		if !ok || len(idRaw) == 0 {
			return mkErr("_id is required to change a document"), nil
		}
		var filter bson.D
		if err := bson.UnmarshalExtJSON([]byte(`{"_id":`+string(idRaw)+`}`), false, &filter); err != nil {
			return mkErr("_id is not a valid value: " + err.Error()), nil
		}
		if req.Op == "delete" {
			preview := fmt.Sprintf("db.%s.deleteOne({_id: %s})", req.Object, strings.TrimSpace(string(idRaw)))
			if req.Preview {
				return dexMutateResult{Preview: preview}, nil
			}
			start := time.Now()
			res, err := coll.DeleteOne(ctx, filter)
			if err != nil {
				return dexMutateResult{Preview: preview, Error: dexMongoError(err, ctx)}, nil
			}
			return dexMutateResult{Preview: preview, Executed: true, AffectedRows: res.DeletedCount,
				DurationMs: float64(time.Since(start).Microseconds()) / 1000}, nil
		}
		var doc bson.D
		if err := bson.UnmarshalExtJSON(req.Document, false, &doc); err != nil {
			return mkErr("the document is not valid JSON: " + err.Error()), nil
		}
		preview := fmt.Sprintf("db.%s.replaceOne({_id: %s}, %s)", req.Object,
			strings.TrimSpace(string(idRaw)), strings.TrimSpace(string(req.Document)))
		if req.Preview {
			return dexMutateResult{Preview: preview}, nil
		}
		start := time.Now()
		res, err := coll.ReplaceOne(ctx, filter, doc)
		if err != nil {
			return dexMutateResult{Preview: preview, Error: dexMongoError(err, ctx)}, nil
		}
		return dexMutateResult{Preview: preview, Executed: true, AffectedRows: res.ModifiedCount,
			DurationMs: float64(time.Since(start).Microseconds()) / 1000}, nil
	}
	return mkErr("unsupported operation " + strconv.Quote(req.Op)), nil
}

// ---------------------------------------------------------------- Valkey

// dexMutateValkey applies a type-appropriate change: the command a person would type,
// chosen by what the key is. Nothing here writes a key of one type over a key of
// another — the type comes from the request and is checked against the server.
func (a *App) dexMutateValkey(ctx context.Context, ad *dexValkeyAdapter, t dexTarget, req dexMutateRequest) (dexMutateResult, error) {
	db, _ := strconv.Atoi(req.Database)
	mkErr := func(msg string) dexMutateResult {
		return dexMutateResult{Error: &dexError{Engine: dexValkey, Code: "DBX_EDIT",
			Name: "BadValue", Message: msg, Display: msg}}
	}
	if strings.TrimSpace(req.Key) == "" {
		return mkErr("a key is required"), nil
	}
	var args []string
	switch req.Op {
	case "delete":
		args = []string{"DEL", req.Key}
	case "ttl":
		if req.TTL < 0 {
			args = []string{"PERSIST", req.Key}
		} else {
			args = []string{"EXPIRE", req.Key, strconv.FormatInt(req.TTL, 10)}
		}
	case "insert", "update":
		switch req.KeyType {
		case "string", "":
			args = []string{"SET", req.Key, req.Value}
		case "hash":
			if req.Field == "" {
				return mkErr("a field is required for a hash"), nil
			}
			args = []string{"HSET", req.Key, req.Field, req.Value}
		case "list":
			if req.Op == "insert" {
				args = []string{"RPUSH", req.Key, req.Value}
			} else {
				args = []string{"LSET", req.Key, strconv.FormatInt(req.Index, 10), req.Value}
			}
		case "set":
			args = []string{"SADD", req.Key, req.Member}
		case "zset":
			if req.Score == "" {
				return mkErr("a score is required for a sorted set"), nil
			}
			args = []string{"ZADD", req.Key, req.Score, req.Member}
		default:
			return mkErr("editing a " + req.KeyType + " is not supported here — use the console"), nil
		}
	case "remove-field":
		switch req.KeyType {
		case "hash":
			args = []string{"HDEL", req.Key, req.Field}
		case "set":
			args = []string{"SREM", req.Key, req.Member}
		case "zset":
			args = []string{"ZREM", req.Key, req.Member}
		default:
			return mkErr("a " + req.KeyType + " has no field to remove"), nil
		}
	default:
		return mkErr("unsupported operation " + strconv.Quote(req.Op)), nil
	}

	preview := dexValkeyPreview(args)
	if req.Preview {
		return dexMutateResult{Preview: preview}, nil
	}
	c, derr := ad.conn(ctx, db)
	if derr != nil {
		return dexMutateResult{Preview: preview, Error: derr}, nil
	}
	defer c.Close()
	start := time.Now()
	r, err := c.do(ctx, args...)
	out := dexMutateResult{Preview: preview, Executed: true,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000}
	if err != nil {
		out.Executed = false
		out.Error = dexValkeyError(err.Error(), ctx)
		return out, nil
	}
	if r.Kind == dexRESPError {
		out.Executed = false
		out.Error = dexValkeyError(r.Str, ctx)
		return out, nil
	}
	if r.Kind == dexRESPInt {
		out.AffectedRows = r.Int
	} else {
		out.AffectedRows = 1
	}
	out.Message = r.text()
	return out, nil
}

// dexValkeyPreview renders a command for display, quoting only the arguments that
// need it so a preview reads like something you could paste into valkey-cli.
func dexValkeyPreview(args []string) string {
	parts := make([]string, 0, len(args))
	for i, a := range args {
		if i > 0 && (a == "" || strings.ContainsAny(a, " \t\"'")) {
			parts = append(parts, strconv.Quote(a))
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// dexAsDexError normalises an adapter error that is already presented, and wraps one
// that is not.
func dexAsDexError(engine string, err error) *dexError {
	if err == nil {
		return nil
	}
	if de, ok := err.(*dexError); ok {
		return de
	}
	return &dexError{Engine: engine, Message: err.Error(), Display: err.Error()}
}
