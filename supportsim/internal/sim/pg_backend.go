package sim

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"supportsim/internal/corpus"
	"supportsim/internal/search"
	"supportsim/internal/store"
)

// pg_backend.go — the desk on PostgreSQL with pgvector.
//
// The same tickets, the same questions, a different machine underneath. A vector here
// is a column type (`vector(384)`), an index is an ordinary index (`USING hnsw`), and a
// search is an ORDER BY with a LIMIT that the planner can satisfy from that index —
// inside the executor, inside the transaction. There is no second process to keep in
// sync: a row is findable by vector search the moment its INSERT commits (Lab step 6
// shows it), and a standby has the index because it has the WAL.
//
// Everything lives in one schema, `supportsim`, in a database of the same name when
// the user may create one, else in the database the DSN names — the same "my own
// namespace, never yours" rule the MongoDB side follows.

const pgSchema = "supportsim"

type pgBackend struct {
	pool    *pgxpool.Pool
	db      string
	mirror  *search.Mirror
	search  *pgSearcher
	ws      *pgWorkshop
	pgvec   bool   // the vector extension is in the database
	vecVer  string // its version
	srvVer  string
	setName string // "primary" / "standby" note for the header

	mu sync.Mutex
}

// NewPGBackend connects. dsn names any database; the backend moves to its own
// `supportsim` database when it is allowed to create it.
func NewPGBackend(ctx context.Context, dsn string, mirror *search.Mirror) (Backend, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := waitPG(ctx, pool); err != nil {
		return nil, err
	}
	db := currentDB(ctx, pool)
	// Our own database if we may: CREATE DATABASE cannot run in a transaction, and a
	// pooler in transaction mode may refuse it — either way the fallback is a schema in
	// the database we were given.
	if db != pgSchema {
		_, cerr := pool.Exec(ctx, "CREATE DATABASE "+pgSchema)
		var pe *pgconn.PgError
		if cerr == nil || errors.As(cerr, &pe) && pe.Code == "42P04" { // duplicate_database
			if own, err := pgxpool.New(ctx, withDB(dsn, pgSchema)); err == nil && waitPG(ctx, own) == nil {
				pool.Close()
				pool, db = own, pgSchema
			}
		}
	}
	b := &pgBackend{pool: pool, db: db, mirror: mirror}
	b.search = &pgSearcher{b: b}
	b.ws = &pgWorkshop{b: b}
	return b, nil
}

func waitPG(ctx context.Context, pool *pgxpool.Pool) error {
	for i := 0; ; i++ {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := pool.Ping(cctx)
		cancel()
		if err == nil {
			return nil
		}
		if i >= 60 || ctx.Err() != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		log.Printf("supportsim: waiting for PostgreSQL: %v", err)
		time.Sleep(3 * time.Second)
	}
}

func currentDB(ctx context.Context, pool *pgxpool.Pool) string {
	var db string
	pool.QueryRow(ctx, "SELECT current_database()").Scan(&db)
	return db
}

// withDB swaps the database in a postgres:// DSN (or a key=value one).
func withDB(dsn, db string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + db
		return u.String()
	}
	return dsn + " dbname=" + db
}

func (b *pgBackend) Engine() string            { return "postgres" }
func (b *pgBackend) Searcher() Searcher        { return b.search }
func (b *pgBackend) Workshop() WorkshopBackend { return b.ws }
func (b *pgBackend) t(table string) string     { return pgSchema + "." + table }
func (b *pgBackend) hasVector() bool           { b.mu.Lock(); defer b.mu.Unlock(); return b.pgvec }
func (b *pgBackend) vectorVersion() string     { b.mu.Lock(); defer b.mu.Unlock(); return b.vecVer }
func (b *pgBackend) colType() string {
	return map[bool]string{true: "vector(384)", false: "real[]"}[b.hasVector()]
}
func (b *pgBackend) exec(ctx context.Context, sql string, args ...any) error {
	_, err := b.pool.Exec(ctx, sql, args...)
	return err
}

// Info is the header's view: server version, primary or standby, pgvector's version.
func (b *pgBackend) Info(ctx context.Context) store.ServerInfo {
	var ver string
	var inRecovery bool
	b.pool.QueryRow(ctx, "SELECT current_setting('server_version'), pg_is_in_recovery()").Scan(&ver, &inRecovery)
	topo := "primary"
	if inRecovery {
		topo = "standby (read-only)"
	}
	return store.ServerInfo{Version: strings.Fields(ver + " ")[0], Topology: topo, SetName: "pgvector " + orNone(b.vectorVersion())}
}

func orNone(s string) string {
	if s == "" {
		return "not installed"
	}
	return s
}

// vecLit renders a vector as pgvector's text input, '[0.0213,-0.0441,…]'.
func vecLit(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v) * 10)
	sb.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(x), 'g', 7, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

// arrLit is the same numbers as a real[] literal, for a server without pgvector.
func arrLit(v []float32) string {
	s := vecLit(v)
	return "{" + s[1:len(s)-1] + "}"
}

func (b *pgBackend) vecArg(v []float32) string {
	if b.hasVector() {
		return vecLit(v)
	}
	return arrLit(v)
}

// Prepare creates the extension (when allowed), the schema and the tables. Without
// pgvector the embeddings are stored as real[] and searched in the app — the desk
// still runs, and the page says what it is missing.
func (b *pgBackend) Prepare(ctx context.Context) error {
	b.exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	var ver string
	if b.pool.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname = 'vector'").Scan(&ver) == nil {
		b.mu.Lock()
		b.pgvec, b.vecVer = true, ver
		b.mu.Unlock()
	}
	ct := b.colType()
	stmts := []string{
		"CREATE SCHEMA IF NOT EXISTS " + pgSchema,
		`CREATE TABLE IF NOT EXISTS ` + b.t("kb_articles") + ` (
  slug text PRIMARY KEY, title text NOT NULL, team text NOT NULL, summary text NOT NULL, steps text NOT NULL,
  embedding ` + ct + ` NOT NULL,
  tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', title || ' ' || summary || ' ' || steps)) STORED)`,
		`CREATE TABLE IF NOT EXISTS ` + b.t("tickets") + ` (
  id bigint PRIMARY KEY, subject text NOT NULL, body text NOT NULL, customer text NOT NULL, plan text NOT NULL,
  created_at timestamptz NOT NULL, status text NOT NULL, team text NOT NULL,
  resolved_kb text, resolved_by text, resolved_at timestamptz, suggested_kb text, suggested_cosine real,
  duplicate_of bigint, incident int, truth_archetype text, truth_team text, truth_kb text,
  embedding ` + ct + ` NOT NULL,
  tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', subject || ' ' || body)) STORED)`,
		"CREATE SEQUENCE IF NOT EXISTS " + b.t("ticket_seq") + " START 1000",
		"CREATE INDEX IF NOT EXISTS tickets_status_created ON " + b.t("tickets") + " (status, created_at DESC)",
		"CREATE INDEX IF NOT EXISTS tickets_customer_created ON " + b.t("tickets") + " (customer, created_at DESC)",
	}
	for _, s := range stmts {
		if err := b.exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", firstLineOf(s), err)
		}
	}
	return nil
}

func (b *pgBackend) SaveArticle(ctx context.Context, a corpus.Article, vec []float32) error {
	return b.exec(ctx, `INSERT INTO `+b.t("kb_articles")+` (slug, title, team, summary, steps, embedding)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (slug) DO UPDATE SET title = EXCLUDED.title, team = EXCLUDED.team, summary = EXCLUDED.summary,
  steps = EXCLUDED.steps, embedding = EXCLUDED.embedding`,
		a.Slug, a.Title, a.Team, a.Summary, strings.Join(a.Steps, "\n"), b.vecArg(vec))
}

func (b *pgBackend) TicketCount(ctx context.Context) (int64, error) {
	var n int64
	err := b.pool.QueryRow(ctx, "SELECT count(*) FROM "+b.t("tickets")).Scan(&n)
	return n, err
}

func (b *pgBackend) NextTicketNumber(ctx context.Context) (int64, error) {
	var n int64
	err := b.pool.QueryRow(ctx, "SELECT nextval('"+b.t("ticket_seq")+"')").Scan(&n)
	return n, err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (b *pgBackend) InsertTickets(ctx context.Context, recs []TicketRecord) error {
	batch := &pgx.Batch{}
	for _, r := range recs {
		var resolvedAt any
		if r.ResolvedKB != "" {
			resolvedAt = r.Created.Add(2 * time.Hour)
		}
		var sugCos any
		if r.SuggestedKB != "" {
			sugCos = r.SuggestedCos
		}
		batch.Queue(`INSERT INTO `+b.t("tickets")+` (id, subject, body, customer, plan, created_at, status, team,
  resolved_kb, resolved_by, resolved_at, suggested_kb, suggested_cosine, duplicate_of, truth_archetype, truth_team, truth_kb, embedding)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			r.ID, r.T.Subject, r.T.Body, r.T.Customer, r.T.Plan, r.Created, r.Status, r.Team,
			nullStr(r.ResolvedKB), nullStr(r.ResolvedBy), resolvedAt, nullStr(r.SuggestedKB), sugCos, r.DuplicateOf,
			r.T.Archetype, r.T.TruthTeam, r.T.TruthKB, b.vecArg(r.Vec))
	}
	return b.pool.SendBatch(ctx, batch).Close()
}

// parseVec reads pgvector's (or real[]'s) text output.
func parseVec(s string) []float32 {
	s = strings.Trim(s, "[]{}")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, _ := strconv.ParseFloat(strings.TrimSpace(p), 32)
		out[i] = float32(f)
	}
	return out
}

func (b *pgBackend) LoadTickets(ctx context.Context, put func(*search.Doc)) error {
	rows, err := b.pool.Query(ctx, `SELECT id, subject, team, status, coalesce(resolved_kb, ''), plan, customer, created_at, embedding::text
FROM `+b.t("tickets")+` WHERE status <> 'probe'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d search.Doc
		var id int64
		var emb string
		if rows.Scan(&id, &d.Title, &d.Team, &d.Status, &d.KB, &d.Plan, &d.Customer, &d.CreatedAt, &emb) == nil {
			d.ID, d.Vec = id, parseVec(emb)
			put(&d)
		}
	}
	return rows.Err()
}

func (b *pgBackend) UpdateTicket(ctx context.Context, id any, s TicketSet) error {
	var sets []string
	var args []any
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if s.Status != "" {
		add("status", s.Status)
	}
	if s.Team != "" {
		add("team", s.Team)
	}
	if s.ResolvedKB != "" {
		add("resolved_kb", s.ResolvedKB)
	}
	if s.Resolved {
		add("resolved_at", time.Now())
	}
	if s.Incident != 0 {
		add("incident", s.Incident)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	return b.exec(ctx, fmt.Sprintf("UPDATE %s SET %s WHERE id = $%d", b.t("tickets"), strings.Join(sets, ", "), len(args)), args...)
}

func (b *pgBackend) TicketArchetype(ctx context.Context, id any) string {
	var a string
	b.pool.QueryRow(ctx, "SELECT coalesce(truth_archetype, '') FROM "+b.t("tickets")+" WHERE id = $1", id).Scan(&a)
	return a
}

// Prune deletes the oldest resolved tickets beyond keep. The HNSW index follows the
// delete in the same transaction; VACUUM later reclaims the graph's dead entries.
func (b *pgBackend) Prune(ctx context.Context, keep int64) []any {
	rows, err := b.pool.Query(ctx, `DELETE FROM `+b.t("tickets")+` WHERE id IN (
  SELECT id FROM `+b.t("tickets")+` WHERE status = 'resolved' ORDER BY created_at
  LIMIT greatest((SELECT count(*) FROM `+b.t("tickets")+`) - $1, 0)) RETURNING id`, keep)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []any
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// The desk's search indexes: an HNSW index per vector column and a GIN index per
// tsvector, so all three kinds of query have an index to use.
var pgDeskIndexes = []struct{ Table, Name, Type, Using string }{
	{"kb_articles", "kb_embedding_hnsw", "vector (hnsw)", "hnsw (embedding vector_cosine_ops)"},
	{"kb_articles", "kb_tsv_gin", "full-text (gin)", "gin (tsv)"},
	{"tickets", "tickets_embedding_hnsw", "vector (hnsw)", "hnsw (embedding vector_cosine_ops)"},
	{"tickets", "tickets_tsv_gin", "full-text (gin)", "gin (tsv)"},
}

// EnsureSearch creates the indexes (a plain CREATE INDEX: done when it returns, and
// usable by the very next query) and reports them.
func (b *pgBackend) EnsureSearch(ctx context.Context) SearchState {
	if !b.hasVector() {
		return SearchState{Err: "pgvector is not installed in this database, so there is no vector type and no vector index. The desk runs on an in-app brute-force scan instead — tick pgvector on the PostgreSQL node or cluster (or the Kubernetes frame) to use it."}
	}
	st := SearchState{Ready: true}
	for _, ix := range pgDeskIndexes {
		if ix.Type == "vector (hnsw)" || ix.Type == "full-text (gin)" {
			if err := b.exec(ctx, "CREATE INDEX IF NOT EXISTS "+ix.Name+" ON "+b.t(ix.Table)+" USING "+ix.Using); err != nil {
				st.Err = err.Error()
			}
		}
		is := store.IndexStatus{Collection: ix.Table, Name: ix.Name, Type: ix.Type, Status: "DOES_NOT_EXIST"}
		var valid bool
		var def string
		if b.pool.QueryRow(ctx, `SELECT i.indisvalid, pg_get_indexdef(i.indexrelid) FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`, pgSchema, ix.Name).Scan(&valid, &def) == nil {
			is.Status, is.Queryable, is.Shell = "INVALID", valid, def+";"
			if valid {
				is.Status = "READY"
			}
		}
		st.Ready = st.Ready && is.Queryable
		st.Indexes = append(st.Indexes, is)
	}
	return st
}

// Freshness is PostgreSQL's whole difference from mongot, measured: the index is
// updated inside the writing transaction, so the writer sees its row immediately,
// nobody else sees it until COMMIT, and everybody sees it from COMMIT on — no polling.
func (b *pgBackend) Freshness(ctx context.Context, v []float32, text string) Freshness {
	out := Freshness{Engine: "pgvector", Text: text}
	q := search.VectorQuery{Collection: store.Tickets, Vector: v, K: 1, NumCandidates: 40, Filter: search.Filter{Status: []string{"probe"}}}
	sqlText, _ := b.search.vectorSQL(q)
	out.Pipeline = sqlText
	id := -(time.Now().UnixNano() % 1_000_000_000_000)
	writer, err := b.pool.Acquire(ctx)
	if err != nil {
		out.Text = err.Error()
		return out
	}
	defer writer.Release()
	tx, err := writer.Begin(ctx)
	if err != nil {
		out.Text = err.Error()
		return out
	}
	defer tx.Rollback(context.Background())
	defer b.exec(context.Background(), "DELETE FROM "+b.t("tickets")+" WHERE id = $1", id)

	found := func(qr func(sql string, args ...any) pgx.Row) bool {
		s, args := b.search.vectorSQLArgs(q)
		want := strconv.FormatInt(id, 10)
		var got string
		err := qr("SELECT id FROM ("+s+") x WHERE id = $"+strconv.Itoa(len(args)+1), append(args, want)...).Scan(&got)
		return err == nil && got == want
	}
	step := func(when, who string, f func() bool) {
		t0 := time.Now()
		ok := f()
		out.Steps = append(out.Steps, FreshStep{When: when, Session: who, Found: ok, Ms: msSince(t0)})
	}
	t0 := time.Now()
	_, err = tx.Exec(ctx, `INSERT INTO `+b.t("tickets")+` (id, subject, body, customer, plan, created_at, status, team, embedding)
VALUES ($1, $2, '', 'probe', 'probe', now(), 'probe', 'probe', $3)`, id, text, b.vecArg(v))
	out.InsertMs = msSince(t0)
	if err != nil {
		out.Text = "insert failed: " + err.Error()
		return out
	}
	step("after INSERT, before COMMIT", "the writing session", func() bool {
		return found(func(s string, a ...any) pgx.Row { return tx.QueryRow(ctx, s, a...) })
	})
	step("after INSERT, before COMMIT", "another session", func() bool {
		return found(func(s string, a ...any) pgx.Row { return b.pool.QueryRow(ctx, s, a...) })
	})
	tc := time.Now()
	if err := tx.Commit(ctx); err != nil {
		out.Text = "commit failed: " + err.Error()
		return out
	}
	out.Attempts = 1
	step("right after COMMIT", "another session", func() bool {
		return found(func(s string, a ...any) pgx.Row { return b.pool.QueryRow(ctx, s, a...) })
	})
	out.FoundMs = msSince(tc)
	out.Found = out.Steps[len(out.Steps)-1].Found
	return out
}
