package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// BSON-type generator ids (MongoDB only). The SQL generators (genEmail, genFirstName, …) are
// reused for string fields; these cover the types that have no SQL analogue.
const (
	genObjectID   = "objectid"
	genInt64      = "int64"      // BSON 64-bit int (long)
	genDouble     = "double"     // BSON double
	genDecimal128 = "decimal128" // BSON Decimal128
	genBSONDate   = "bson_date"  // BSON UTC datetime
	genBSONTS     = "bson_ts"    // BSON internal Timestamp
	genBinary     = "binary"     // BSON binary (generic)
	genBSONUUID   = "bson_uuid"  // BSON binary subtype 4 (UUID)
	genRegex      = "regex"      // BSON regular expression
	genJavaScript = "javascript" // BSON JavaScript code
	genMinKey     = "minkey"     // BSON MinKey
	genMaxKey     = "maxkey"     // BSON MaxKey
	genEmbedded   = "embedded"   // nested document (recurse into Fields)
	genArray      = "array"      // array of Elem
	genMongoNull  = genNull      // reuse
)

// mongoSampleSize bounds how many documents $sample reads to infer a collection's shape.
const mongoSampleSize = 200

// reUUID matches field names that suggest a binary field should hold a UUID (subtype 4).
var reUUID = mustRe(`(?i)(^|_)(uuid|guid|uid)($|_)`)

// Data Generator — MongoDB backend. Unlike the SQL engines (which run psql/mysql via `docker
// exec`), MongoDB is driven with the official Go driver over the *stack network*: we join the
// dbcanvas container to the stack's Docker network and dial the node's container IP directly —
// the same mechanism the query-run and benchmark tools use for Postgres/MySQL (dialNodeDSN,
// queryrun_run.go). This gives us native BSON: the driver's primitive.* types are every BSON
// type as first-class Go values, so a generated document is just a bson.D — no Extended JSON
// text to quote or a shell to escape.
//
// directConnection=true: we can only reach the *one* node the user picked (Docker's embedded DNS
// does not resolve the Intranet's *.<domain> member hostnames, so driver auto-discovery of the
// rest of a replica set would fail). Writes therefore require that node to be a PRIMARY or a
// mongos router; a secondary is reported as such rather than hanging.

// mongoConnectTimeout bounds the initial handshake — a secondary or an unreachable node should
// fail fast, not stall the request.
const mongoConnectTimeout = 15 * time.Second

// mongoClientFor opens a *mongo.Client to the node behind c, joining the stack network first.
// The returned closer disconnects the client; callers must always invoke it.
func (a *App) mongoClientFor(ctx context.Context, c dbConn) (*mongo.Client, func(), error) {
	netName := networkName(c.StackID)
	if err := a.engCtx(ctx).NetworkConnect(ctx, netName, qrAppContainerID()); err != nil {
		return nil, nil, fmt.Errorf("join stack network: %v", err)
	}
	ip, err := a.engCtx(ctx).ContainerIP(ctx, c.ContainerID, netName)
	if err != nil || ip == "" {
		return nil, nil, fmt.Errorf("could not resolve node address on the stack network")
	}
	uri := fmt.Sprintf("mongodb://%s:%s@%s:27017/?authSource=admin&directConnection=true",
		url.QueryEscape(c.Super), url.QueryEscape(c.Password), ip)

	cctx, cancel := context.WithTimeout(ctx, mongoConnectTimeout)
	defer cancel()
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %v", err)
	}
	closer := func() { _ = client.Disconnect(context.Background()) }
	if err := client.Ping(cctx, nil); err != nil {
		closer()
		return nil, nil, fmt.Errorf("connect: %v", err)
	}
	return client, closer, nil
}

// -------------------------------------------------------------------- introspection

// mongoDatabases lists user databases (the admin/local/config system databases are hidden).
func (a *App) mongoDatabases(ctx context.Context, c dbConn) ([]string, error) {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return nil, err
	}
	defer closer()
	names, err := client.ListDatabaseNames(ctx,
		bson.M{"name": bson.M{"$nin": bson.A{"admin", "local", "config"}}})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// mongoCollections lists a database's collections (skipping system.*) with estimated doc counts.
func (a *App) mongoCollections(ctx context.Context, c dbConn, db string) ([]dgTable, error) {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return nil, err
	}
	defer closer()
	d := client.Database(db)
	names, err := d.ListCollectionNames(ctx, bson.D{{Key: "type", Value: "collection"}})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	tables := make([]dgTable, 0, len(names))
	for _, n := range names {
		if len(n) >= 7 && n[:7] == "system." {
			continue
		}
		est, _ := d.Collection(n).EstimatedDocumentCount(ctx)
		tables = append(tables, dgTable{Table: n, EstRows: est})
	}
	return tables, nil
}

// mongoCollectionMeta samples the collection and infers a field schema (BSON type per field,
// recursing into embedded documents and arrays), then assigns each field a generator + choices.
// An empty collection yields just an `_id` ObjectId — the user builds the rest of the schema.
func (a *App) mongoCollectionMeta(ctx context.Context, c dbConn, db, coll string) (dgTableMeta, error) {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return dgTableMeta{}, err
	}
	defer closer()

	cur, err := client.Database(db).Collection(coll).Aggregate(ctx,
		mongo.Pipeline{{{Key: "$sample", Value: bson.D{{Key: "size", Value: mongoSampleSize}}}}})
	if err != nil {
		return dgTableMeta{}, err
	}
	defer cur.Close(ctx)
	var docs []bson.Raw
	for cur.Next(ctx) {
		docs = append(docs, bson.Raw(append([]byte(nil), cur.Current...)))
	}
	if err := cur.Err(); err != nil {
		return dgTableMeta{}, err
	}

	cols := inferDocFields(docs, len(docs))
	if len(cols) == 0 {
		cols = []dgColumn{{Name: "_id", UDT: "objectId", DataType: "objectId"}}
	}
	for i := range cols {
		fillMongoGen(&cols[i])
	}
	return dgTableMeta{Database: db, Table: coll, Columns: cols}, nil
}

// inferDocFields infers the field schema shared across a set of sampled documents, preserving
// first-seen field order. sampleN is the document count (a field seen in fewer docs is nullable).
func inferDocFields(docs []bson.Raw, sampleN int) []dgColumn {
	var order []string
	seen := map[string]bool{}
	vals := map[string][]bson.RawValue{}
	for _, d := range docs {
		els, err := d.Elements()
		if err != nil {
			continue
		}
		for _, el := range els {
			k := el.Key()
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
			vals[k] = append(vals[k], el.Value())
		}
	}
	cols := make([]dgColumn, 0, len(order))
	for _, k := range order {
		cols = append(cols, inferFieldSchema(k, vals[k], sampleN))
	}
	return cols
}

// inferFieldSchema picks the dominant BSON type for one field (or array-element bag) and recurses
// into embedded documents and array elements.
func inferFieldSchema(name string, vals []bson.RawValue, sampleN int) dgColumn {
	typeCount := map[string]int{}
	var objDocs []bson.Raw
	var arrElems []bson.RawValue
	for _, rv := range vals {
		t := bsonTypeName(rv.Type)
		typeCount[t]++
		switch t {
		case "object":
			if d, ok := rv.DocumentOK(); ok {
				objDocs = append(objDocs, d)
			}
		case "array":
			if arr, ok := rv.ArrayOK(); ok {
				if els, err := arr.Elements(); err == nil {
					for _, e := range els {
						arrElems = append(arrElems, e.Value())
					}
				}
			}
		}
	}
	dom := dominantType(typeCount)
	col := dgColumn{
		Name: name, UDT: dom, DataType: dom,
		Nullable: len(vals) < sampleN || typeCount["null"] > 0,
	}
	switch dom {
	case "object":
		col.Fields = inferDocFields(objDocs, len(objDocs))
	case "array":
		e := inferFieldSchema("", arrElems, len(arrElems))
		col.Elem = &e
	}
	return col
}

// dominantType returns the most common non-null BSON type, falling back to "string".
func dominantType(counts map[string]int) string {
	best, bestN := "", -1
	for t, n := range counts {
		if t == "null" {
			continue
		}
		if n > bestN || (n == bestN && t < best) {
			best, bestN = t, n
		}
	}
	if best == "" {
		return "string"
	}
	return best
}

// bsonTypeName maps a BSON wire type to the string carried in dgColumn.UDT.
func bsonTypeName(t bsontype.Type) string {
	switch t {
	case bsontype.Double:
		return "double"
	case bsontype.String:
		return "string"
	case bsontype.EmbeddedDocument:
		return "object"
	case bsontype.Array:
		return "array"
	case bsontype.Binary:
		return "binData"
	case bsontype.ObjectID:
		return "objectId"
	case bsontype.Boolean:
		return "bool"
	case bsontype.DateTime:
		return "date"
	case bsontype.Null, bsontype.Undefined:
		return "null"
	case bsontype.Regex:
		return "regex"
	case bsontype.JavaScript, bsontype.CodeWithScope:
		return "javascript"
	case bsontype.Int32:
		return "int"
	case bsontype.Timestamp:
		return "timestamp"
	case bsontype.Int64:
		return "long"
	case bsontype.Decimal128:
		return "decimal"
	case bsontype.MinKey:
		return "minKey"
	case bsontype.MaxKey:
		return "maxKey"
	}
	return "string"
}

// -------------------------------------------------------------------- generator inference

// fillMongoGen assigns a field its inferred default generator and combobox choices, recursing
// into embedded documents and array elements.
func fillMongoGen(c *dgColumn) {
	c.Generator = mongoInferGenerator(*c)
	c.Generators = mongoGeneratorChoices(*c)
	for i := range c.Fields {
		fillMongoGen(&c.Fields[i])
	}
	if c.Elem != nil {
		fillMongoGen(c.Elem)
	}
}

// mongoBaseChoices are the options every field offers. genSkip omits the field entirely (there
// are no column defaults in MongoDB, so there is no genDefault).
var mongoBaseChoices = []string{genAuto, genSkip, genConstant, genMongoNull}

// stringGenByName maps a field name to a semantic string generator using the same regexes the
// SQL inferrer uses ("" = no match).
func stringGenByName(name string) string {
	switch {
	case reEmail.MatchString(name):
		return genEmail
	case reFirst.MatchString(name):
		return genFirstName
	case reLast.MatchString(name):
		return genLastName
	case reUser.MatchString(name):
		return genUsername
	case rePhone.MatchString(name):
		return genPhone
	case reCity.MatchString(name):
		return genCity
	case reCountry.MatchString(name):
		return genCountry
	case reCompany.MatchString(name):
		return genCompany
	case reJob.MatchString(name):
		return genJobTitle
	case reURL.MatchString(name):
		return genURL
	case reIP.MatchString(name):
		return genIP
	case reAddr.MatchString(name):
		return genAddress
	case reName.MatchString(name):
		return genFullName
	}
	return ""
}

// mongoInferGenerator picks the best default generator for a field, given its BSON type and name.
func mongoInferGenerator(c dgColumn) string {
	if c.Name == "_id" && c.UDT == "objectId" {
		return genObjectID
	}
	switch c.UDT {
	case "objectId":
		return genObjectID
	case "string":
		if g := stringGenByName(c.Name); g != "" {
			return g
		}
		if reStatus.MatchString(c.Name) {
			return genEnum
		}
		return genText
	case "double":
		return genDouble
	case "int":
		return genRandInt
	case "long":
		return genInt64
	case "decimal":
		return genDecimal128
	case "bool":
		return genBool
	case "date":
		return genBSONDate
	case "timestamp":
		return genBSONTS
	case "binData":
		if reUUID.MatchString(c.Name) {
			return genBSONUUID
		}
		return genBinary
	case "regex":
		return genRegex
	case "javascript":
		return genJavaScript
	case "object":
		return genEmbedded
	case "array":
		return genArray
	case "minKey":
		return genMinKey
	case "maxKey":
		return genMaxKey
	}
	return genText
}

// mongoGeneratorChoices returns the combobox options for a field (most-relevant first).
func mongoGeneratorChoices(c dgColumn) []string {
	head := func(opts ...string) []string { return append(opts, mongoBaseChoices...) }
	switch c.UDT {
	case "objectId":
		return head(genObjectID)
	case "string":
		return append(head(genText, genLorem), genFirstName, genLastName, genFullName,
			genUsername, genEmail, genPhone, genCity, genCountry, genCompany, genJobTitle,
			genAddress, genURL, genUuid, genEnum)
	case "double":
		return head(genDouble, genDecimal128, genRandInt)
	case "int":
		return head(genRandInt, genSeqInt, genInt64)
	case "long":
		return head(genInt64, genRandInt, genSeqInt)
	case "decimal":
		return head(genDecimal128, genDouble)
	case "bool":
		return head(genBool)
	case "date":
		return head(genBSONDate)
	case "timestamp":
		return head(genBSONTS, genBSONDate)
	case "binData":
		return head(genBinary, genBSONUUID)
	case "regex":
		return head(genRegex)
	case "javascript":
		return head(genJavaScript, genText)
	case "object":
		return head(genEmbedded)
	case "array":
		return head(genArray)
	case "minKey":
		return head(genMinKey)
	case "maxKey":
		return head(genMaxKey)
	}
	return head(genText)
}

// -------------------------------------------------------------------- config resolution

// resolveMongoGens turns the client's authoritative field schema into top-level generators,
// recursively resolving embedded documents and array elements. A field with genSkip is dropped.
// This is what lets the UI add fields or define a schema on an empty collection — the client
// schema, not a fresh sample, drives generation.
func resolveMongoGens(cfg dgGenConfig) []colGen {
	var out []colGen
	for _, cc := range cfg.Columns {
		if cc.Skip || cc.Generator == genSkip {
			continue
		}
		out = append(out, colGen{col: mongoColOf(cc), gen: mongoGenOf(cc), opts: optsOr(cc.Options)})
	}
	return out
}

// mongoColOf converts a client field config into a dgColumn (recursing into nested shapes) so the
// value emitter has the BSON type and sub-schema it needs.
func mongoColOf(cc dgColConfig) dgColumn {
	c := dgColumn{Name: cc.Name, UDT: cc.UDT, DataType: cc.UDT, Nullable: true, Generator: mongoGenOf(cc)}
	for _, f := range cc.Fields {
		c.Fields = append(c.Fields, mongoColOf(f))
	}
	if cc.Elem != nil {
		e := mongoColOf(*cc.Elem)
		c.Elem = &e
	}
	return c
}

func mongoGenOf(cc dgColConfig) string {
	if cc.Generator != "" {
		return cc.Generator
	}
	return mongoInferGenerator(dgColumn{Name: cc.Name, UDT: cc.UDT})
}

func optsOr(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// -------------------------------------------------------------------- document generation

// mongoDoc builds one document from the resolved top-level field generators.
func mongoDoc(gens []colGen, ctx *genCtx) bson.D {
	doc := make(bson.D, 0, len(gens))
	for _, g := range gens {
		if v, include := mongoValue(g.col, g.gen, g.opts, ctx); include {
			doc = append(doc, bson.E{Key: g.col.Name, Value: v})
		}
	}
	return doc
}

// mongoValue produces one BSON value for a field as a native Go/driver value. The bool return
// is whether the field is included at all: genSkip omits the key entirely, whereas genNull
// includes it with a BSON null. Recurses into embedded documents and array elements.
func mongoValue(c dgColumn, gen string, opts map[string]any, ctx *genCtx) (any, bool) {
	r := ctx.rng
	optF := func(k string, def float64) float64 {
		if v, ok := opts[k]; ok {
			if f, ok := v.(float64); ok {
				return f
			}
		}
		return def
	}
	optS := func(k, def string) string {
		if v, ok := opts[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		return def
	}

	if gen == genSkip {
		return nil, false
	}
	if c.Nullable {
		if p := optF("nullPct", 0); p > 0 && r.Float64()*100 < p {
			return nil, true // BSON null
		}
	}
	if gen == genAuto || gen == "" {
		gen = mongoInferGenerator(c)
	}

	switch gen {
	case genMongoNull:
		return nil, true
	case genObjectID:
		return primitive.NewObjectID(), true
	case genConstant:
		switch c.UDT {
		case "int":
			return int32(optF("value", 0)), true
		case "long":
			return int64(optF("value", 0)), true
		case "double", "decimal":
			return optF("value", 0), true
		case "bool":
			return optS("value", "false") == "true", true
		default:
			return optS("value", ""), true
		}

	// numbers
	case genRandInt:
		lo, hi := int64(optF("min", 0)), int64(optF("max", 1_000_000))
		return int32(lo + r.Int63n(maxI(hi-lo, 1))), true
	case genSeqInt:
		return int32(int64(optF("start", 1)) + ctx.row), true
	case genInt64:
		lo, hi := int64(optF("min", 0)), int64(optF("max", 1_000_000_000))
		return lo + r.Int63n(maxI(hi-lo, 1)), true
	case genDouble:
		lo, hi := optF("min", 0), optF("max", 10000)
		return lo + r.Float64()*(hi-lo), true
	case genDecimal128:
		lo, hi := optF("min", 0), optF("max", 10000)
		d, err := primitive.ParseDecimal128(strconv.FormatFloat(lo+r.Float64()*(hi-lo), 'f', 4, 64))
		if err != nil {
			return primitive.NewDecimal128(0, 0), true
		}
		return d, true
	case genBool:
		return r.Intn(2) == 0, true

	// time
	case genBSONDate:
		return primitive.NewDateTimeFromTime(randDate(r).UTC()), true
	case genBSONTS:
		return primitive.Timestamp{T: uint32(randDate(r).Unix()), I: uint32(ctx.row)}, true

	// binary / special
	case genBinary:
		n := int(optF("len", 12))
		b := make([]byte, n)
		r.Read(b)
		return primitive.Binary{Subtype: 0x00, Data: b}, true
	case genBSONUUID:
		b := make([]byte, 16)
		r.Read(b)
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return primitive.Binary{Subtype: 0x04, Data: b}, true
	case genRegex:
		return primitive.Regex{Pattern: pick(r, words), Options: "i"}, true
	case genJavaScript:
		return primitive.JavaScript("function(){ return " + strconv.Itoa(r.Intn(1000)) + "; }"), true
	case genMinKey:
		return primitive.MinKey{}, true
	case genMaxKey:
		return primitive.MaxKey{}, true

	// nested
	case genEmbedded:
		sub := make(bson.D, 0, len(c.Fields))
		for _, f := range c.Fields {
			if v, inc := mongoValue(f, f.Generator, nil, ctx); inc {
				sub = append(sub, bson.E{Key: f.Name, Value: v})
			}
		}
		return sub, true
	case genArray:
		n := int(optF("len", 0))
		if n <= 0 {
			n = 1 + r.Intn(4)
		}
		arr := make(bson.A, 0, n)
		for i := 0; i < n; i++ {
			if c.Elem == nil {
				arr = append(arr, pick(r, words))
				continue
			}
			if v, inc := mongoValue(*c.Elem, c.Elem.Generator, nil, ctx); inc {
				arr = append(arr, v)
			}
		}
		return arr, true

	// strings (reuse the shared corpus)
	case genUuid:
		return randUUID(r), true
	case genFirstName:
		return pick(r, firstNames), true
	case genLastName:
		return pick(r, lastNames), true
	case genFullName:
		return pick(r, firstNames) + " " + pick(r, lastNames), true
	case genUsername:
		return strings.ToLower(pick(r, firstNames)) + strconv.Itoa(r.Intn(1000)), true
	case genEmail:
		return strings.ToLower(pick(r, firstNames)+"."+pick(r, lastNames)) + strconv.Itoa(r.Intn(1000)) + "@" + pick(r, domains), true
	case genPhone:
		return fmt.Sprintf("+1-%03d-%03d-%04d", 200+r.Intn(800), r.Intn(1000), r.Intn(10000)), true
	case genCity:
		return pick(r, cities), true
	case genCountry:
		return pick(r, countries), true
	case genCompany:
		return pick(r, companies), true
	case genJobTitle:
		return pick(r, jobTitles), true
	case genAddress:
		return fmt.Sprintf("%d %s %s", 1+r.Intn(9999), pick(r, lastNames), pick(r, streets)), true
	case genURL:
		return "https://" + pick(r, domains) + "/" + pick(r, words), true
	case genIP:
		return fmt.Sprintf("%d.%d.%d.%d", r.Intn(256), r.Intn(256), r.Intn(256), 1+r.Intn(254)), true
	case genMAC:
		return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256)), true
	case genEnum:
		if len(c.Enum) > 0 {
			return pick(r, c.Enum), true
		}
		return pick(r, statuses), true
	case genLorem:
		return lorem(r, 3+r.Intn(10)), true
	case genText:
		return pick(r, words) + " " + pick(r, words) + " " + pick(r, words), true
	}
	return pick(r, words), true
}

// mongoPreview renders a handful of sample documents as relaxed Extended JSON — the same shape
// the generator will insert, so the user sees BSON types (ObjectId, dates, Decimal128…) rendered
// the way mongosh would show them.
func (a *App) mongoPreview(w http.ResponseWriter, gens []colGen, seed int64) {
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	gc := &genCtx{rng: newRand(seed), engine: "mongodb", uniq: "preview",
		tsStart: time.Now().Add(-720 * time.Hour), tsStep: time.Minute}
	docs := make([]json.RawMessage, 0, 5)
	for i := 0; i < 5; i++ {
		gc.row = int64(i)
		b, err := bson.MarshalExtJSON(mongoDoc(gens, gc), false, false)
		if err != nil {
			continue
		}
		docs = append(docs, json.RawMessage(b))
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// -------------------------------------------------------------------- job runner

// runGenMongoJob is the MongoDB analogue of runGenJob: workers build batches of documents and
// InsertMany them (unordered, so one bad document does not sink the batch). It shares one client
// (the driver pools connections and is goroutine-safe) and reuses the row-range handout, seeding,
// progress counters, cancel, and notification scaffolding.
func (a *App) runGenMongoJob(ctx context.Context, c dbConn, cfg dgGenConfig, meta dgTableMeta, gens []colGen, job *dgJob) {
	defer func() {
		job.End = time.Now()
		if job.Status == "running" {
			job.Status = "done"
		}
		switch job.Status {
		case "done":
			a.notifyStack(job.StackID, "datagen.done", "success", "Data generation completed",
				fmt.Sprintf("Inserted %s documents into %s.", fmtInt(atomic.LoadInt64(&job.Inserted)), job.Table), "")
		case "error":
			a.notifyStack(job.StackID, "datagen.error", "error", "Data generation failed",
				job.Table+": "+job.Message, "")
		case "canceled":
			a.notifyStack(job.StackID, "datagen.canceled", "warning", "Data generation canceled",
				fmt.Sprintf("%s — inserted %s documents before cancel.", job.Table, fmtInt(atomic.LoadInt64(&job.Inserted))), "")
		}
	}()

	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		job.Status = "error"
		job.Message = err.Error()
		return
	}
	defer closer()
	coll := client.Database(cfg.Database).Collection(cfg.Table)

	seed := cfg.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	// Hand out globally unique row-index ranges so sequential generators never overlap across
	// workers. take() returns (count, startIndex).
	var next int64
	var remMu sync.Mutex
	take := func() (int64, int64) {
		remMu.Lock()
		defer remMu.Unlock()
		if next >= cfg.Rows {
			return 0, 0
		}
		n := int64(cfg.Batch)
		if next+n > cfg.Rows {
			n = cfg.Rows - next
		}
		start := next
		next += n
		return n, start
	}

	var wg sync.WaitGroup
	for wkr := 0; wkr < cfg.Threads; wkr++ {
		wg.Add(1)
		go func(wkr int) {
			defer wg.Done()
			gc := &genCtx{rng: newRand(seed + int64(wkr)*1_000_003), engine: "mongodb", uniq: job.ID,
				tsStart: time.Now().Add(-720 * time.Hour), tsStep: time.Minute}
			for {
				if ctx.Err() != nil {
					return
				}
				n, start := take()
				if n <= 0 {
					return
				}
				docs := make([]any, 0, n)
				for i := int64(0); i < n; i++ {
					gc.row = start + i
					docs = append(docs, mongoDoc(gens, gc))
				}
				res, err := coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
				inserted := n
				if res != nil {
					inserted = int64(len(res.InsertedIDs))
				}
				if err != nil {
					atomic.AddInt64(&job.Inserted, inserted)
					atomic.AddInt64(&job.Errors, n-inserted)
					job.Message = truncErr(err.Error())
					if cfg.StopOnError {
						job.Status = "error"
						job.cancel()
						return
					}
					continue
				}
				atomic.AddInt64(&job.Inserted, inserted)
			}
		}(wkr)
	}
	wg.Wait()
}
