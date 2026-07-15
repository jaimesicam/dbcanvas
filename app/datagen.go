package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Data Generator — generates realistic test data for tables in databases provisioned by
// Database Stacks. This is the PostgreSQL implementation (pg / patroni / repmgr nodes).
// All SQL runs via `docker exec psql` inside the node container, so it works whether or
// not the DB port is published to the host, and reuses the deployment's superuser creds.
//
// Flow: list connections → databases → tables → introspect columns (types, PK/FK,
// identity/generated, pgvector, TimescaleDB) → per-column generator template with smart
// inference → preview → generate (rows/batch/threads, FK-aware sampling) with progress.

// -------------------------------------------------------------------- exec helpers

// dbConn resolves a running DB node to its container, engine, and admin creds.
type dbConn struct {
	ContainerID string
	Engine      string // "postgres" | "mysql" | "mongodb"
	Super       string // admin user (postgres / root / admin)
	Password    string
	StackID     int64 // for joining the stack network (MongoDB driver dials the container IP)
}

// engineForType maps a design node type to a Data Generator engine ("" = unsupported).
func engineForType(t string) string {
	switch t {
	case "pg", "patroni", "repmgr", "spock":
		return "postgres"
	case "pxc", "ps", "mysql", "innodb":
		return "mysql"
	case "psm", "psmdb", "psmrs":
		return "mongodb"
	}
	return ""
}

func (a *App) dbConnFor(st Stack, nid string) (dbConn, bool) {
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		return dbConn{}, false
	}
	engine := engineForType(nodeTypeIn(st, nid))
	if engine == "" {
		return dbConn{}, false
	}
	dep = a.reconcileContainerID(context.Background(), st.ID, nid, dep)
	c := dbConn{ContainerID: dep.ContainerID, Engine: engine, StackID: st.ID}
	if engine == "mongodb" {
		var s mongoSecrets
		json.Unmarshal(dep.Secrets, &s)
		c.Super = s.AdminUser
		if c.Super == "" {
			c.Super = "admin"
		}
		c.Password = s.AdminPassword
	} else if engine == "postgres" {
		var s pgSecrets
		json.Unmarshal(dep.Secrets, &s)
		c.Super, c.Password = s.Super(), s.SuperPassword
	} else {
		var s pxcSecrets
		json.Unmarshal(dep.Secrets, &s)
		c.Super = s.RootUser
		if c.Super == "" {
			c.Super = "root"
		}
		c.Password = s.RootPassword
	}
	return c, true
}

// nodeTypeIn returns the design node type for nid in stack st.
func nodeTypeIn(st Stack, nid string) string {
	for _, n := range buildDoc(st).Nodes {
		if n.ID == nid {
			return n.Type
		}
	}
	return ""
}

// Super returns the superuser, defaulting to postgres.
func (s pgSecrets) Super() string {
	if s.SuperUser == "" {
		return "postgres"
	}
	return s.SuperUser
}

// queryJSON runs a query whose single-row single-column result is JSON and unmarshals it.
// PostgreSQL: psql runs as the postgres OS user (matching the image's local `peer` auth).
// MySQL: the mysql client authenticates as root via MYSQL_PWD (no password on argv).
func (a *App) queryJSON(ctx context.Context, c dbConn, db, sql string, out any) error {
	var res ExecResult
	var err error
	if c.Engine == "mysql" {
		res, err = a.engCtx(ctx).ExecInput(ctx, c.ContainerID, "",
			[]string{"mysql", "-u", c.Super, "-N", "--raw", "-B"},
			[]string{"MYSQL_PWD=" + c.Password}, []byte(sql))
	} else {
		res, err = a.engCtx(ctx).ExecAs(ctx, c.ContainerID, "postgres",
			[]string{"psql", "-U", c.Super, "-d", db, "-tAqc", sql}, nil)
	}
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	txt := strings.TrimSpace(res.Stdout)
	if txt == "" || txt == "NULL" {
		txt = "null"
	}
	return json.Unmarshal([]byte(txt), out)
}

// execSQL runs a statement (INSERT etc.). SQL is piped via stdin — not argv — so large
// multi-row INSERT batches can't hit the OS argument-length limit.
func (a *App) execSQL(ctx context.Context, c dbConn, db, sql string) error {
	var res ExecResult
	var err error
	if c.Engine == "mysql" {
		res, err = a.engCtx(ctx).ExecInput(ctx, c.ContainerID, "",
			[]string{"mysql", "-u", c.Super, "-D", db},
			[]string{"MYSQL_PWD=" + c.Password}, []byte(sql))
	} else {
		res, err = a.engCtx(ctx).ExecInput(ctx, c.ContainerID, "postgres",
			[]string{"psql", "-v", "ON_ERROR_STOP=1", "-U", c.Super, "-d", db, "-q", "-f", "-"}, nil, []byte(sql))
	}
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// -------------------------------------------------------------------- HTTP: connections

type dgConnection struct {
	StackID   int64  `json:"stackId"`
	StackName string `json:"stackName"`
	NodeID    string `json:"nodeId"`
	Label     string `json:"label"`
	Engine    string `json:"engine"` // "postgres"
	Type      string `json:"type"`   // pg | patroni | repmgr
}

// handleDataGenConnections lists running PostgreSQL nodes across the user's stacks.
func (a *App) handleDataGenConnections(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	out := []dgConnection{}
	for _, s := range stacks {
		// ListStacks omits design_json; reload the full stack so buildDoc sees its nodes.
		st, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		doc := buildDoc(st)
		for _, n := range doc.Nodes {
			engine := engineForType(n.Type)
			if engine == "" {
				continue
			}
			// A sharded cluster (psmdb) only accepts writes through its mongos router; offering a
			// config/shard member would just fail with "not master". A plain replica set (psmrs)
			// or standalone is a valid target as-is (the picked node must be the primary).
			if n.Type == "psmdb" && n.Role != "mongos" {
				continue
			}
			if dep, err := a.store.GetDeployment(st.ID, n.ID); err != nil || dep.State != DeployRunning {
				continue
			}
			out = append(out, dgConnection{
				StackID: st.ID, StackName: st.Name, NodeID: n.ID,
				Label: n.Label, Engine: engine, Type: n.Type,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// loadDGNode resolves + authorizes a stack/node and returns a live dbConn.
func (a *App) loadDGNode(w http.ResponseWriter, r *http.Request) (dbConn, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return dbConn{}, false
	}
	c, ok := a.dbConnFor(st, r.PathValue("nid"))
	if !ok {
		writeErr(w, http.StatusConflict, "node is not running")
		return dbConn{}, false
	}
	return c, true
}

// handleDataGenDatabases lists non-template databases.
func (a *App) handleDataGenDatabases(w http.ResponseWriter, r *http.Request) {
	c, ok := a.loadDGNode(w, r)
	if !ok {
		return
	}
	var dbs []string
	var err error
	if c.Engine == "mongodb" {
		dbs, err = a.mongoDatabases(r.Context(), c)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, dbs)
		return
	}
	if c.Engine == "mysql" {
		err = a.queryJSON(r.Context(), c, "",
			`SELECT COALESCE(JSON_ARRAYAGG(schema_name),JSON_ARRAY()) FROM information_schema.schemata WHERE schema_name NOT IN ('mysql','information_schema','performance_schema','sys')`, &dbs)
	} else {
		err = a.queryJSON(r.Context(), c, "postgres",
			`SELECT COALESCE(json_agg(datname ORDER BY datname),'[]') FROM pg_database WHERE datistemplate=false AND datallowconn`, &dbs)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dbs)
}

type dgTable struct {
	Schema  string `json:"schema"`
	Table   string `json:"table"`
	EstRows int64  `json:"estRows"`
}

// handleDataGenTables lists user tables (schema + estimated row count) in a database.
func (a *App) handleDataGenTables(w http.ResponseWriter, r *http.Request) {
	c, ok := a.loadDGNode(w, r)
	if !ok {
		return
	}
	db := dbParam(r)
	var tables []dgTable
	var q string
	if c.Engine == "mongodb" {
		tables, err := a.mongoCollections(r.Context(), c, db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, tables)
		return
	}
	if c.Engine == "mysql" {
		q = fmt.Sprintf(`SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('schema',table_schema,'table',table_name,'estRows',IFNULL(table_rows,0))),JSON_ARRAY())
		  FROM information_schema.tables WHERE table_type='BASE TABLE' AND table_schema=%s ORDER BY table_name`, sqlLit(db))
	} else {
		q = `SELECT COALESCE(json_agg(t),'[]') FROM (
		  SELECT n.nspname AS schema, c.relname AS table, GREATEST(c.reltuples,0)::bigint AS "estRows"
		  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema','_timescaledb_internal','_timescaledb_catalog','_timescaledb_config','timescaledb_information','timescaledb_experimental')
		  ORDER BY n.nspname, c.relname) t`
	}
	if err := a.queryJSON(r.Context(), c, db, q, &tables); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tables)
}

// -------------------------------------------------------------------- columns + inference

type dgFK struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Column string `json:"column"`
}

type dgColumn struct {
	Name         string   `json:"name"`
	DataType     string   `json:"dataType"` // formatted type e.g. varchar(50), numeric(10,2)
	UDT          string   `json:"udt"`      // underlying udt name (int4, varchar, vector, timestamptz…)
	Nullable     bool     `json:"nullable"`
	Default      string   `json:"default"`
	IsIdentity   bool     `json:"isIdentity"`
	IsGenerated  bool     `json:"isGenerated"`
	IsPrimaryKey bool     `json:"isPrimaryKey"`
	IsUnique     bool     `json:"isUnique"`
	CharLen      int      `json:"charLen"`
	NumPrecision int      `json:"numPrecision"`
	NumScale     int      `json:"numScale"`
	VectorDim    int      `json:"vectorDim"` // >0 for pgvector columns
	FK           *dgFK    `json:"fk"`
	Enum         []string `json:"enum"` // enum labels when the type is an enum
	Generator    string   `json:"generator"`
	Generators   []string `json:"generators"` // choices offered in the combobox
	// MongoDB only. UDT carries the BSON type ("objectId","string","double","int","long",
	// "decimal","bool","date","timestamp","binData","regex","null","object","array",
	// "minKey","maxKey"). Nested shapes recurse through these:
	Fields []dgColumn `json:"fields,omitempty"` // sub-fields when UDT=="object" (embedded document)
	Elem   *dgColumn  `json:"elem,omitempty"`   // element schema when UDT=="array"
}

type dgTableMeta struct {
	Database     string     `json:"database"`
	Schema       string     `json:"schema"`
	Table        string     `json:"table"`
	IsHypertable bool       `json:"isHypertable"`
	TimeColumn   string     `json:"timeColumn"`
	Columns      []dgColumn `json:"columns"`
}

// handleDataGenColumns introspects one table's columns + metadata and infers a generator.
func (a *App) handleDataGenColumns(w http.ResponseWriter, r *http.Request) {
	c, ok := a.loadDGNode(w, r)
	if !ok {
		return
	}
	meta, err := a.tableMeta(r.Context(), c, dbParam(r), r.URL.Query().Get("schema"), r.URL.Query().Get("table"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// tableMeta introspects a table and fills each column's inferred generator + choices,
// dispatching to the engine-specific implementation.
func (a *App) tableMeta(ctx context.Context, c dbConn, db, schema, table string) (dgTableMeta, error) {
	if c.Engine == "mongodb" {
		return a.mongoCollectionMeta(ctx, c, db, table)
	}
	if c.Engine == "mysql" {
		return a.myTableMeta(ctx, c, db, schema, table)
	}
	return a.pgTableMeta(ctx, c, db, schema, table)
}

// pgTableMeta introspects a PostgreSQL table's columns + constraints + pgvector/TimescaleDB
// metadata and fills each column's inferred generator + choices.
func (a *App) pgTableMeta(ctx context.Context, c dbConn, db, schema, table string) (dgTableMeta, error) {
	if !identOK(schema) || !identOK(table) {
		return dgTableMeta{}, fmt.Errorf("invalid schema/table")
	}

	// Column metadata (types, nullability, default, identity, generated, precision/len,
	// pgvector dimension, enum labels).
	var cols []dgColumn
	colQ := fmt.Sprintf(`SELECT COALESCE(json_agg(x ORDER BY x.ord),'[]') FROM (
	  SELECT a.attnum AS ord, a.attname AS name,
	    format_type(a.atttypid,a.atttypmod) AS "dataType",
	    t.typname AS udt,
	    NOT a.attnotnull AS nullable,
	    COALESCE(pg_get_expr(ad.adbin, ad.adrelid),'') AS default,
	    (a.attidentity <> '') AS "isIdentity",
	    (a.attgenerated <> '') AS "isGenerated",
	    COALESCE(information_schema._pg_char_max_length(a.atttypid,a.atttypmod),0) AS "charLen",
	    COALESCE(information_schema._pg_numeric_precision(a.atttypid,a.atttypmod),0) AS "numPrecision",
	    COALESCE(information_schema._pg_numeric_scale(a.atttypid,a.atttypmod),0) AS "numScale",
	    CASE WHEN t.typname IN ('vector','halfvec','sparsevec')
	         THEN COALESCE(NULLIF(regexp_replace(format_type(a.atttypid,a.atttypmod),'[^0-9]','','g'),'')::int,0) ELSE 0 END AS "vectorDim",
	    CASE WHEN t.typtype='e' THEN (SELECT json_agg(e.enumlabel ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid=a.atttypid) ELSE NULL END AS enum
	  FROM pg_attribute a
	  JOIN pg_class c ON c.oid=a.attrelid
	  JOIN pg_namespace n ON n.oid=c.relnamespace
	  JOIN pg_type t ON t.oid=a.atttypid
	  LEFT JOIN pg_attrdef ad ON ad.adrelid=a.attrelid AND ad.adnum=a.attnum
	  WHERE n.nspname=%s AND c.relname=%s AND a.attnum>0 AND NOT a.attisdropped) x`,
		sqlLit(schema), sqlLit(table))
	if err := a.queryJSON(ctx, c, db, colQ, &cols); err != nil {
		return dgTableMeta{}, err
	}

	// Primary key + unique columns.
	pk := map[string]bool{}
	uniq := map[string]bool{}
	var keyRows []struct {
		Column string `json:"column"`
		Kind   string `json:"kind"`
	}
	a.queryJSON(ctx, c, db, fmt.Sprintf(`SELECT COALESCE(json_agg(k),'[]') FROM (
	  SELECT a.attname AS column, CASE WHEN i.indisprimary THEN 'pk' ELSE 'unique' END AS kind
	  FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace
	  JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=ANY(i.indkey)
	  WHERE n.nspname=%s AND c.relname=%s AND (i.indisprimary OR i.indisunique)) k`,
		sqlLit(schema), sqlLit(table)), &keyRows)
	for _, k := range keyRows {
		if k.Kind == "pk" {
			pk[k.Column] = true
		}
		uniq[k.Column] = true
	}

	// Foreign keys (single-column).
	fks := map[string]*dgFK{}
	var fkRows []struct {
		Column    string `json:"column"`
		RefSchema string `json:"refSchema"`
		RefTable  string `json:"refTable"`
		RefColumn string `json:"refColumn"`
	}
	a.queryJSON(ctx, c, db, fmt.Sprintf(`SELECT COALESCE(json_agg(f),'[]') FROM (
	  SELECT att.attname AS column, rn.nspname AS "refSchema", rc.relname AS "refTable", ratt.attname AS "refColumn"
	  FROM pg_constraint con
	  JOIN pg_class c ON c.oid=con.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace
	  JOIN pg_class rc ON rc.oid=con.confrelid JOIN pg_namespace rn ON rn.oid=rc.relnamespace
	  JOIN pg_attribute att ON att.attrelid=con.conrelid AND att.attnum=con.conkey[1]
	  JOIN pg_attribute ratt ON ratt.attrelid=con.confrelid AND ratt.attnum=con.confkey[1]
	  WHERE con.contype='f' AND array_length(con.conkey,1)=1 AND n.nspname=%s AND c.relname=%s) f`,
		sqlLit(schema), sqlLit(table)), &fkRows)
	for _, f := range fkRows {
		fks[f.Column] = &dgFK{Schema: f.RefSchema, Table: f.RefTable, Column: f.RefColumn}
	}

	// TimescaleDB hypertable detection + its time column.
	meta := dgTableMeta{Database: db, Schema: schema, Table: table}
	var hyper []struct {
		TimeColumn string `json:"timeColumn"`
	}
	a.queryJSON(ctx, c, db, fmt.Sprintf(`SELECT COALESCE(json_agg(h),'[]') FROM (
	  SELECT column_name AS "timeColumn" FROM timescaledb_information.dimensions
	  WHERE hypertable_schema=%s AND hypertable_name=%s AND dimension_type='Time' LIMIT 1) h`,
		sqlLit(schema), sqlLit(table)), &hyper)
	if len(hyper) == 1 {
		meta.IsHypertable = true
		meta.TimeColumn = hyper[0].TimeColumn
	}

	for i := range cols {
		cols[i].IsPrimaryKey = pk[cols[i].Name]
		cols[i].IsUnique = uniq[cols[i].Name]
		cols[i].FK = fks[cols[i].Name]
		cols[i].Generators = generatorChoices(cols[i], meta)
		cols[i].Generator = inferGenerator(cols[i], meta)
	}
	meta.Columns = cols
	return meta, nil
}

// dbParam returns the ?db= query value, defaulting to postgres.
func dbParam(r *http.Request) string {
	db := r.URL.Query().Get("db")
	if !identOK(db) {
		return "postgres"
	}
	return db
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

func identOK(s string) bool { return s != "" && len(s) <= 63 && identRe.MatchString(s) }

// sqlLit renders a Postgres string literal (single-quote escaped).
func sqlLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
