package main

// Database Explorer — MongoDB: standalone, replica-set member, replica set as a
// whole, and mongos.
//
// MongoDB is not made to look like SQL here, and that is a design decision rather
// than an omission. A find has a filter, a projection, a sort and a limit, and an
// aggregation is a pipeline; squeezing either through a text box labelled "SQL"
// would teach the wrong thing about the database and make the interesting half —
// nested documents — unreachable.
//
// Every result travels twice: once as Documents, which is the real thing with its
// nesting intact, and once as Rows, a flattened top-level view so the grid, the
// chart builder, the CSV export and the filter all work exactly as they do for a SQL
// result. The flattening is lossy by construction, so it never replaces the
// documents — a nested object or array becomes a compact preview in the table whose
// cell opens the real value.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type dexMongoAdapter struct {
	client *mongo.Client
	closer func()
	caps   dexCaps
}

func (m *dexMongoAdapter) Capabilities() dexCaps { return m.caps }

func (m *dexMongoAdapter) Close() error {
	if m.closer != nil {
		m.closer()
	}
	return nil
}

func (m *dexMongoAdapter) TestConnection(ctx context.Context) error {
	return m.client.Ping(ctx, nil)
}

func (m *dexMongoAdapter) Version(ctx context.Context) (string, error) {
	var res bson.M
	err := m.client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&res)
	if err != nil {
		return "", dexMongoError(err, ctx)
	}
	v, _ := res["version"].(string)
	return v, nil
}

// dexMongoSystemDB are the server's own databases: `admin` holds users and roles,
// `local` the oplog, `config` the sharding metadata. All three are worth reading on
// a lab canvas, so they are listed and marked rather than hidden.
var dexMongoSystemDB = map[string]bool{"admin": true, "local": true, "config": true}

func (m *dexMongoAdapter) ListDatabases(ctx context.Context) ([]dexTreeNode, error) {
	res, err := m.client.ListDatabases(ctx, bson.D{})
	if err != nil {
		return nil, dexMongoError(err, ctx)
	}
	out := make([]dexTreeNode, 0, len(res.Databases))
	for _, d := range res.Databases {
		n := dexTreeNode{ID: d.Name, Name: d.Name, Kind: dexKindDatabase, HasChild: true,
			System: dexMongoSystemDB[d.Name]}
		if d.SizeOnDisk > 0 {
			b := d.SizeOnDisk
			n.Bytes = &b
		}
		out = append(out, n)
	}
	return out, nil
}

// ListSchemas is empty: MongoDB has databases and collections and nothing between.
func (m *dexMongoAdapter) ListSchemas(ctx context.Context, database string) ([]dexTreeNode, error) {
	return nil, nil
}

var dexMongoFolders = []string{"Collections", "Views"}

func (m *dexMongoAdapter) ListObjects(ctx context.Context, database, schema, folder, cursor, filter string) (dexObjectPage, error) {
	page := dexObjectPage{Folders: dexMongoFolders, Nodes: []dexTreeNode{}}
	if strings.TrimSpace(database) == "" {
		return page, fmt.Errorf("a database is required")
	}
	cur, err := m.client.Database(database).ListCollections(ctx, bson.D{})
	if err != nil {
		return page, dexMongoError(err, ctx)
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var spec struct {
			Name string `bson:"name"`
			Type string `bson:"type"`
		}
		if cur.Decode(&spec) != nil {
			continue
		}
		if !dexMatches(spec.Name, filter) {
			continue
		}
		kind, fold := dexKindCollection, "Collections"
		if spec.Type == "view" {
			kind, fold = dexKindView, "Views"
		}
		if folder != "" && folder != fold {
			continue
		}
		n := dexTreeNode{ID: spec.Name, Name: spec.Name, Kind: kind, Folder: fold,
			HasChild: true, System: strings.HasPrefix(spec.Name, "system.")}
		// An estimated count rather than countDocuments: the estimate is metadata
		// and is instant, and a tree that counted every collection's documents to
		// draw itself would be unusable on any collection worth having.
		if kind == dexKindCollection {
			if c, err := m.client.Database(database).Collection(spec.Name).EstimatedDocumentCount(ctx); err == nil {
				n.Rows = &c
			}
		}
		page.Nodes = append(page.Nodes, n)
	}
	return page, cur.Err()
}

func (m *dexMongoAdapter) DescribeObject(ctx context.Context, database string, ref dexObjectRef) (dexObjectDetail, error) {
	d := dexObjectDetail{Ref: ref, Kind: ref.Kind}
	coll := m.client.Database(database).Collection(ref.Name)

	var stats bson.M
	err := m.client.Database(database).RunCommand(ctx, bson.D{
		{Key: "collStats", Value: ref.Name},
	}).Decode(&stats)
	if err == nil {
		add := func(l string, v any) {
			if s := dexMongoScalar(v); s != "" {
				d.Props = append(d.Props, dexProp{Label: l, Value: s})
			}
		}
		if n, ok := dexMongoNumber(stats["count"]); ok {
			d.RowEstimate = &n
		}
		if n, ok := dexMongoNumber(stats["size"]); ok {
			d.Bytes = &n
			add("Data size", byteSizeLabel(n))
		}
		if n, ok := dexMongoNumber(stats["storageSize"]); ok {
			add("Storage size", byteSizeLabel(n))
		}
		if n, ok := dexMongoNumber(stats["totalIndexSize"]); ok {
			add("Index size", byteSizeLabel(n))
		}
		if n, ok := dexMongoNumber(stats["avgObjSize"]); ok {
			add("Average document", byteSizeLabel(n))
		}
		add("Sharded", stats["sharded"])
		add("Capped", stats["capped"])
	}

	if cur, err := coll.Indexes().List(ctx); err == nil {
		defer cur.Close(ctx)
		for cur.Next(ctx) {
			var spec struct {
				Name   string   `bson:"name"`
				Key    bson.D   `bson:"key"`
				Unique bool     `bson:"unique"`
				Sparse bool     `bson:"sparse"`
				TTL    *int32   `bson:"expireAfterSeconds"`
				Hidden bool     `bson:"hidden"`
				Fields []string `bson:"-"`
			}
			if cur.Decode(&spec) != nil {
				continue
			}
			ix := dexIndexInfo{Name: spec.Name, Unique: spec.Unique, Primary: spec.Name == "_id_"}
			for _, kv := range spec.Key {
				ix.Columns = append(ix.Columns, fmt.Sprintf("%s: %v", kv.Key, kv.Value))
			}
			if spec.TTL != nil {
				ix.Type = fmt.Sprintf("TTL %ds", *spec.TTL)
			}
			if spec.Sparse {
				ix.Partial = "sparse"
			}
			d.Indexes = append(d.Indexes, ix)
		}
	}

	// The "columns" of a collection are inferred, not declared, and the UI says so.
	// A sample rather than a scan: reading a hundred documents tells you what the
	// common fields are, and reading all of them to find out would be the same
	// mistake as counting every collection to draw a tree.
	if fields, err := m.inferFields(ctx, coll); err == nil {
		d.Columns = fields
	}
	d.PrimaryKey = []string{"_id"}
	d.Editable = m.caps.EditableRows
	if !d.Editable {
		d.EditReason = "this connection is read-only"
	}
	return d, nil
}

// dexMongoSample is how many documents field inference looks at.
const dexMongoSample = 100

// inferFields reports the fields seen across a sample, most common first, with the
// BSON types each was seen as. A field that is a string in some documents and an int
// in others says so — which on a schemaless database is often the finding.
func (m *dexMongoAdapter) inferFields(ctx context.Context, coll *mongo.Collection) ([]dexColumnInfo, error) {
	cur, err := coll.Find(ctx, bson.D{}, options.Find().SetLimit(dexMongoSample))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	type seen struct {
		count int
		types map[string]bool
		first int
	}
	fields := map[string]*seen{}
	order := 0
	docs := 0
	for cur.Next(ctx) {
		var doc bson.D
		if cur.Decode(&doc) != nil {
			continue
		}
		docs++
		for _, e := range doc {
			f, ok := fields[e.Key]
			if !ok {
				f = &seen{types: map[string]bool{}, first: order}
				fields[e.Key] = f
				order++
			}
			f.count++
			f.types[dexBSONTypeName(e.Value)] = true
		}
	}
	out := make([]dexColumnInfo, 0, len(fields))
	for name, f := range fields {
		types := make([]string, 0, len(f.types))
		for t := range f.types {
			types = append(types, t)
		}
		sort.Strings(types)
		out = append(out, dexColumnInfo{
			Name: name, Type: strings.Join(types, " | "), Nullable: f.count < docs,
			Position: f.first + 1,
			Extra:    fmt.Sprintf("in %d of %d sampled", f.count, docs),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out, cur.Err()
}

func dexBSONTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case int32, int64:
		return "int"
	case float64:
		return "double"
	case bool:
		return "bool"
	case bson.D, bson.M:
		return "object"
	case bson.A:
		return "array"
	}
	return fmt.Sprintf("%T", v)
}

// ---------------------------------------------------------------- query

func (m *dexMongoAdapter) Query(ctx context.Context, req dexQueryRequest) (dexResult, error) {
	res := dexResult{Engine: dexMongoDB, Limit: req.Limit}
	db := strings.TrimSpace(req.Database)
	if db == "" {
		res.Error = &dexError{Engine: dexMongoDB, Message: "a database is required", Display: "a database is required"}
		return res, nil
	}
	limit := dexClampLimit(req.Limit)
	start := time.Now()

	op := req.Mongo.Operation
	if op == "" {
		op = "find"
	}
	var set dexResultSet
	var derr *dexError
	switch op {
	case "find":
		set, derr = m.find(ctx, db, req, limit)
	case "aggregate":
		set, derr = m.aggregate(ctx, db, req, limit)
	case "count":
		set, derr = m.count(ctx, db, req)
	case "indexes":
		set, derr = m.indexList(ctx, db, req)
	case "stats":
		set, derr = m.collStats(ctx, db, req)
	case "command":
		set, derr = m.runCommand(ctx, db, req)
	default:
		derr = &dexError{Engine: dexMongoDB, Message: "unsupported operation " + strconv.Quote(op),
			Display: "unsupported operation " + strconv.Quote(op)}
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	if derr != nil {
		derr.ElapsedMs = res.DurationMs
		res.Error = derr
		return res, nil
	}
	set.Statement = dexMongoStatement(req)
	res.Sets = []dexResultSet{set}
	return res, nil
}

// dexMongoStatement renders what was run, for the history list. It is the shell form
// of the operation, which is what somebody would paste somewhere else.
func dexMongoStatement(req dexQueryRequest) string {
	c := req.Mongo.Collection
	switch req.Mongo.Operation {
	case "aggregate":
		return fmt.Sprintf("db.%s.aggregate(%s)", c, dexCompactJSON(req.Mongo.Pipeline, "[]"))
	case "count":
		return fmt.Sprintf("db.%s.countDocuments(%s)", c, dexCompactJSON(req.Mongo.Filter, "{}"))
	case "indexes":
		return fmt.Sprintf("db.%s.getIndexes()", c)
	case "stats":
		return fmt.Sprintf("db.%s.stats()", c)
	case "command":
		return "db.runCommand(" + dexCompactJSON(req.Mongo.Filter, "{}") + ")"
	}
	return fmt.Sprintf("db.%s.find(%s, %s)", c, dexCompactJSON(req.Mongo.Filter, "{}"), dexCompactJSON(req.Mongo.Projection, "{}"))
}

func dexCompactJSON(raw json.RawMessage, def string) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return def
	}
	return s
}

// dexMongoParse turns a JSON fragment from the editor into a BSON document. Extended
// JSON is used rather than plain JSON so that the types MongoDB actually stores can
// be written: {"_id": {"$oid": "..."}} is how a user names a document by its id, and
// a plain-JSON parser would turn it into a nested object that matches nothing.
func dexMongoParse(raw json.RawMessage, what string) (bson.D, *dexError) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return bson.D{}, nil
	}
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(s), false, &d); err != nil {
		return nil, &dexError{Engine: dexMongoDB, Name: "BadValue",
			Message: what + " is not valid JSON: " + err.Error(),
			Display: what + " is not valid JSON: " + err.Error()}
	}
	return d, nil
}

func dexMongoParsePipeline(raw json.RawMessage) (mongo.Pipeline, *dexError) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return mongo.Pipeline{}, nil
	}
	var p []bson.D
	if err := bson.UnmarshalExtJSON([]byte(s), false, &p); err != nil {
		return nil, &dexError{Engine: dexMongoDB, Name: "BadValue",
			Message: "the pipeline is not valid JSON: " + err.Error(),
			Display: "the pipeline is not valid JSON: " + err.Error()}
	}
	return mongo.Pipeline(p), nil
}

func (m *dexMongoAdapter) find(ctx context.Context, db string, req dexQueryRequest, limit int) (dexResultSet, *dexError) {
	if req.Mongo.Collection == "" {
		return dexResultSet{}, &dexError{Engine: dexMongoDB, Message: "a collection is required", Display: "a collection is required"}
	}
	filter, derr := dexMongoParse(req.Mongo.Filter, "the filter")
	if derr != nil {
		return dexResultSet{}, derr
	}
	opt := options.Find().SetLimit(int64(limit) + 1)
	if proj, derr := dexMongoParse(req.Mongo.Projection, "the projection"); derr != nil {
		return dexResultSet{}, derr
	} else if len(proj) > 0 {
		opt.SetProjection(proj)
	}
	if sortD, derr := dexMongoParse(req.Mongo.Sort, "the sort"); derr != nil {
		return dexResultSet{}, derr
	} else if len(sortD) > 0 {
		opt.SetSort(sortD)
	}
	if req.Mongo.Skip > 0 {
		opt.SetSkip(req.Mongo.Skip)
	}
	cur, err := m.client.Database(db).Collection(req.Mongo.Collection).Find(ctx, filter, opt)
	if err != nil {
		return dexResultSet{}, dexMongoError(err, ctx)
	}
	defer cur.Close(ctx)
	return dexMongoCursorSet(ctx, cur, limit)
}

func (m *dexMongoAdapter) aggregate(ctx context.Context, db string, req dexQueryRequest, limit int) (dexResultSet, *dexError) {
	if req.Mongo.Collection == "" {
		return dexResultSet{}, &dexError{Engine: dexMongoDB, Message: "a collection is required", Display: "a collection is required"}
	}
	pipeline, derr := dexMongoParsePipeline(req.Mongo.Pipeline)
	if derr != nil {
		return dexResultSet{}, derr
	}
	// The ceiling is added as a $limit stage on the end rather than applied to the
	// cursor, so a pipeline that already limits itself is untouched and one that
	// does not cannot return a million groups. It is a stage the user can see in
	// the statement history, not a hidden rewrite of what they asked for.
	pipeline = append(pipeline, bson.D{{Key: "$limit", Value: int64(limit) + 1}})
	cur, err := m.client.Database(db).Collection(req.Mongo.Collection).Aggregate(ctx, pipeline,
		options.Aggregate().SetAllowDiskUse(true))
	if err != nil {
		return dexResultSet{}, dexMongoError(err, ctx)
	}
	defer cur.Close(ctx)
	return dexMongoCursorSet(ctx, cur, limit)
}

func (m *dexMongoAdapter) count(ctx context.Context, db string, req dexQueryRequest) (dexResultSet, *dexError) {
	filter, derr := dexMongoParse(req.Mongo.Filter, "the filter")
	if derr != nil {
		return dexResultSet{}, derr
	}
	n, err := m.client.Database(db).Collection(req.Mongo.Collection).CountDocuments(ctx, filter)
	if err != nil {
		return dexResultSet{}, dexMongoError(err, ctx)
	}
	return dexResultSet{
		Kind:    dexSetRows,
		Columns: []dexColumn{{Name: "count", SemanticType: dexSemInteger}},
		Rows:    [][]any{{n}}, RowCount: 1,
	}, nil
}

func (m *dexMongoAdapter) indexList(ctx context.Context, db string, req dexQueryRequest) (dexResultSet, *dexError) {
	cur, err := m.client.Database(db).Collection(req.Mongo.Collection).Indexes().List(ctx)
	if err != nil {
		return dexResultSet{}, dexMongoError(err, ctx)
	}
	defer cur.Close(ctx)
	return dexMongoCursorSet(ctx, cur, 500)
}

func (m *dexMongoAdapter) collStats(ctx context.Context, db string, req dexQueryRequest) (dexResultSet, *dexError) {
	var out bson.D
	err := m.client.Database(db).RunCommand(ctx, bson.D{{Key: "collStats", Value: req.Mongo.Collection}}).Decode(&out)
	if err != nil {
		return dexResultSet{}, dexMongoError(err, ctx)
	}
	return dexMongoDocsSet([]bson.D{out}, 1)
}

// runCommand is the escape hatch: a database command written as a document. It is
// how serverStatus, explain, currentOp and the rest are reached without this file
// growing a method per command.
func (m *dexMongoAdapter) runCommand(ctx context.Context, db string, req dexQueryRequest) (dexResultSet, *dexError) {
	cmd, derr := dexMongoParse(req.Mongo.Filter, "the command")
	if derr != nil {
		return dexResultSet{}, derr
	}
	if len(cmd) == 0 {
		return dexResultSet{}, &dexError{Engine: dexMongoDB, Message: "a command document is required",
			Display: "a command document is required"}
	}
	var out bson.D
	if err := m.client.Database(db).RunCommand(ctx, cmd).Decode(&out); err != nil {
		return dexResultSet{}, dexMongoError(err, ctx)
	}
	return dexMongoDocsSet([]bson.D{out}, 1)
}

func dexMongoCursorSet(ctx context.Context, cur *mongo.Cursor, limit int) (dexResultSet, *dexError) {
	var docs []bson.D
	for cur.Next(ctx) {
		var d bson.D
		if cur.Decode(&d) != nil {
			continue
		}
		docs = append(docs, d)
		if len(docs) > limit {
			break
		}
	}
	if err := cur.Err(); err != nil {
		return dexResultSet{}, dexMongoError(err, ctx)
	}
	return dexMongoDocsSet(docs, limit)
}

// dexMongoDocsSet builds both halves of a MongoDB result: the documents as canonical
// Extended JSON (so an ObjectId stays an ObjectId and a date stays a date) and the
// flattened table view over the union of top-level fields.
//
// The column order is the order fields were first met, which is the order they are
// stored in and the order a shell would print them — alphabetical would put `_id`
// in the middle of a document that starts with it.
func dexMongoDocsSet(docs []bson.D, limit int) (dexResultSet, *dexError) {
	set := dexResultSet{Kind: dexSetDocuments, Rows: [][]any{}}
	if len(docs) > limit {
		docs = docs[:limit]
		set.Truncated = true
	}
	var order []string
	seen := map[string]int{}
	for _, d := range docs {
		for _, e := range d {
			if _, ok := seen[e.Key]; !ok {
				seen[e.Key] = len(order)
				order = append(order, e.Key)
			}
		}
	}
	for _, name := range order {
		set.Columns = append(set.Columns, dexColumn{Name: name, SemanticType: dexSemUnknown})
	}
	sems := make([]map[string]int, len(order))
	for i := range sems {
		sems[i] = map[string]int{}
	}
	for _, d := range docs {
		raw, err := bson.MarshalExtJSON(d, false, false)
		if err != nil {
			return set, &dexError{Engine: dexMongoDB, Message: "could not render a document: " + err.Error(),
				Display: "could not render a document"}
		}
		set.Documents = append(set.Documents, json.RawMessage(raw))
		row := make([]any, len(order))
		for _, e := range d {
			i := seen[e.Key]
			v, sem := dexMongoCell(e.Value)
			row[i] = v
			sems[i][sem]++
		}
		set.Rows = append(set.Rows, row)
	}
	// A column's semantic type is whichever one most of its values had — which is
	// what makes numeric alignment and chart-field suggestion work on a collection
	// whose documents mostly agree.
	for i := range set.Columns {
		best, n := dexSemUnknown, 0
		for s, c := range sems[i] {
			if c > n {
				best, n = s, c
			}
		}
		set.Columns[i].SemanticType = best
	}
	set.RowCount = len(set.Rows)
	return set, nil
}

// dexMongoCell renders one BSON value for the flattened table view. A nested object
// or array becomes its Extended JSON text, tagged as a document or an array, so the
// grid shows a compact preview and the cell opens the real structure rather than a
// truncated string — nothing is destroyed to fit a column.
func dexMongoCell(v any) (any, string) {
	switch x := v.(type) {
	case nil:
		return nil, dexSemUnknown
	case string:
		return x, dexSemString
	case bool:
		return x, dexSemBool
	case int32:
		return int64(x), dexSemInteger
	case int64:
		if x > 1<<53 || x < -(1<<53) {
			return dexBigNum{Marker: "bignum", Text: strconv.FormatInt(x, 10)}, dexSemInteger
		}
		return x, dexSemInteger
	case float64:
		return x, dexSemNumber
	case bson.D, bson.M:
		return dexMongoExt(v), dexSemDocument
	case bson.A:
		return dexMongoExt(v), dexSemArray
	}
	// ObjectId, DateTime, Decimal128, Binary and the rest go through Extended JSON,
	// which is the representation that names what they are.
	s := dexMongoExt(v)
	// A plain scalar comes back quoted; unwrap it so the grid does not show quotes
	// around every string that arrived by this path.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var str string
		if json.Unmarshal([]byte(s), &str) == nil {
			return str, dexSemString
		}
	}
	return s, dexSemJSON
}

// dexMongoExt renders one BSON value as Extended JSON.
//
// The driver marshals documents, not bare values — there is no MarshalExtJSONValue —
// so the value is wrapped in a one-field document and the field's rendering is read
// back out. That keeps an ObjectId an {"$oid": …} and a date a {"$date": …} rather
// than flattening either into a string that cannot be told from one.
func dexMongoExt(v any) string {
	raw, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, false)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	var wrap map[string]json.RawMessage
	if json.Unmarshal(raw, &wrap) != nil {
		return fmt.Sprintf("%v", v)
	}
	if inner, ok := wrap["v"]; ok {
		return strings.TrimSpace(string(inner))
	}
	return fmt.Sprintf("%v", v)
}

func dexMongoScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case string:
		return x
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return ""
}

func dexMongoNumber(v any) (int64, bool) {
	switch x := v.(type) {
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// dexMongoError presents a MongoDB error with the code and name the server gave it,
// which is what makes one searchable: "Unauthorized (13)" is an answer, "command
// failed" is not.
func dexMongoError(err error, ctx context.Context) *dexError {
	if ctx != nil && ctx.Err() != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return &dexError{Engine: dexMongoDB, Code: "DBX_TIMEOUT", Name: "Timeout",
				Message: "the query exceeded its time limit", Display: "the query exceeded its time limit"}
		}
		return &dexError{Engine: dexMongoDB, Code: "DBX_CANCELED", Name: "Canceled",
			Message: "the query was cancelled", Display: "the query was cancelled"}
	}
	if ce, ok := err.(mongo.CommandError); ok {
		e := &dexError{Engine: dexMongoDB, Message: ce.Message,
			Code: strconv.FormatInt(int64(ce.Code), 10), Name: ce.Name}
		e.Display = fmt.Sprintf("%s (%s): %s", e.Name, e.Code, e.Message)
		if e.Name == "" {
			e.Display = fmt.Sprintf("MongoServerError %s: %s", e.Code, e.Message)
		}
		return e
	}
	if we, ok := err.(mongo.WriteException); ok && len(we.WriteErrors) > 0 {
		w := we.WriteErrors[0]
		return &dexError{Engine: dexMongoDB, Message: w.Message,
			Code: strconv.Itoa(w.Code), Name: "WriteError",
			Display: fmt.Sprintf("WriteError (%d): %s", w.Code, w.Message)}
	}
	return &dexError{Engine: dexMongoDB, Message: err.Error(), Display: err.Error()}
}
