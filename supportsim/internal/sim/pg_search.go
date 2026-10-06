package sim

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"supportsim/internal/search"
	"supportsim/internal/store"
)

// pg_search.go — the three kinds of query, in SQL.
//
//   - vector:  ORDER BY embedding <=> $1 LIMIT k. `<=>` is cosine distance (1 − cos);
//     the planner answers it from the HNSW index because the index's operator class
//     (vector_cosine_ops) matches the operator and the query has a LIMIT. A filter is a
//     plain WHERE — and pgvector 0.8's iterative scans (hnsw.iterative_scan) keep
//     walking the graph until enough rows pass it, which before 0.8 was the classic
//     "my filtered query returns 3 rows instead of 10".
//   - keyword: PostgreSQL's own full-text search — a generated tsvector, a GIN index,
//     ts_rank_cd. The terms are ORed, so a long ticket is not required to contain
//     every one of its own words.
//   - hybrid:  both, fused in one statement — reciprocal rank fusion or normalized
//     score fusion — written out so it can be read and copied.
//
// Every statement runs in its own short transaction so `SET LOCAL hnsw.ef_search`
// applies to it alone (and so it works through PgBouncer in transaction mode).

type pgSearcher struct {
	b     *pgBackend
	mu    sync.RWMutex
	ready bool
}

func (s *pgSearcher) SetReady(ok bool) { s.mu.Lock(); s.ready = ok; s.mu.Unlock() }
func (s *pgSearcher) Ready() bool      { s.mu.RLock(); defer s.mu.RUnlock(); return s.ready }

func pgTable(coll string) string {
	if coll == store.KB {
		return "kb_articles"
	}
	return "tickets"
}

func pgCols(coll string) string {
	if coll == store.KB {
		return "slug AS id, title, team, '' AS status, slug AS kb, '' AS plan, now() AS created_at"
	}
	return "id::text AS id, subject AS title, team, status, coalesce(resolved_kb, '') AS kb, plan, created_at"
}

// pgWhere renders a Filter as WHERE conditions, numbering placeholders from next.
func pgWhere(f search.Filter, next int) (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, next+len(args)-1))
	}
	if f.Team != "" {
		add("team = $%d", f.Team)
	}
	if f.Plan != "" {
		add("plan = $%d", f.Plan)
	}
	if f.Customer != "" {
		add("customer = $%d", f.Customer)
	}
	if len(f.Status) == 1 {
		add("status = $%d", f.Status[0])
	} else if len(f.Status) > 1 {
		add("status = ANY($%d)", f.Status)
	}
	if !f.Since.IsZero() {
		add("created_at >= $%d", f.Since)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// vectorSQLArgs is the statement and its arguments ($1 = the query vector).
func (s *pgSearcher) vectorSQLArgs(q search.VectorQuery) (string, []any) {
	where, wargs := pgWhere(q.Filter, 2)
	args := append([]any{vecLit(q.Vector)}, wargs...)
	args = append(args, q.K)
	sql := fmt.Sprintf(`SELECT %s, 1 - (embedding <=> $1::vector) AS similarity
FROM %s
%s
ORDER BY embedding <=> $1::vector
LIMIT $%d`, pgCols(q.Collection), s.b.t(pgTable(q.Collection)), where, len(args))
	return strings.Replace(sql, "\n\n", "\n", 1), args
}

// settings are the SET LOCALs a vector query runs under.
func (s *pgSearcher) settings(q search.VectorQuery) []string {
	if q.Exact {
		// No index: every row's distance, then a top-k sort — the ground truth.
		return []string{"SET LOCAL enable_indexscan = off", "SET LOCAL enable_bitmapscan = off"}
	}
	ef := q.NumCandidates
	if ef < q.K {
		ef = q.K
	}
	if ef > 1000 {
		ef = 1000
	}
	out := []string{"SET LOCAL hnsw.ef_search = " + strconv.Itoa(ef)}
	if !q.Filter.IsEmpty() {
		out = append(out, "SET LOCAL hnsw.iterative_scan = relaxed_order")
	}
	if q.ForceIndex {
		out = append(out, "SET LOCAL enable_seqscan = off   -- a table this small would be scanned instead")
	}
	return out
}

// vectorSQL is the displayable form: the settings, the statement, and what $1 is.
func (s *pgSearcher) vectorSQL(q search.VectorQuery) (string, []any) {
	sql, args := s.vectorSQLArgs(q)
	return pgShow(s.settings(q), sql, args), args
}

// pgShow renders statements for the page: the SET LOCALs, the SQL, and the
// parameters, the query vector abbreviated.
func pgShow(sets []string, sql string, args []any) string {
	var b strings.Builder
	if len(sets) > 0 {
		b.WriteString("BEGIN;\n")
		for _, st := range sets {
			// A trailing comment keeps its semicolon in front of it, so the text pastes into psql.
			if i := strings.Index(st, "   --"); i > 0 {
				st = st[:i] + ";" + st[i:]
			} else {
				st += ";"
			}
			b.WriteString(st + "\n")
		}
	}
	b.WriteString(sql + ";\n")
	if len(sets) > 0 {
		b.WriteString("COMMIT;\n")
	}
	for i, a := range args {
		v := fmt.Sprint(a)
		if s, ok := a.(string); ok && strings.HasPrefix(s, "[") && strings.Count(s, ",") > 10 {
			parts := strings.SplitN(s[1:], ",", 5)
			v = "'[" + strings.Join(parts[:4], ", ") + ", … " + strconv.Itoa(strings.Count(s, ",")+1) + " numbers]'"
		} else if s, ok := a.(string); ok {
			v = "'" + s + "'"
		} else if t, ok := a.(time.Time); ok {
			v = "'" + t.UTC().Format(time.RFC3339) + "'"
		}
		fmt.Fprintf(&b, "-- $%d = %s\n", i+1, v)
	}
	return strings.TrimRight(b.String(), "\n")
}

// run executes a statement under its settings and reads hits.
func (s *pgSearcher) run(ctx context.Context, sets []string, sql string, args []any, extra int) ([]search.Hit, time.Duration, error) {
	t0 := time.Now()
	tx, err := s.b.pool.Begin(ctx)
	if err != nil {
		return nil, time.Since(t0), err
	}
	defer tx.Rollback(context.Background())
	for _, st := range sets {
		if _, err := tx.Exec(ctx, st); err != nil {
			return nil, time.Since(t0), err
		}
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, time.Since(t0), err
	}
	hits, err := scanHits(rows, extra)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return hits, time.Since(t0), err
}

// scanHits reads id, title, team, status, kb, plan, created_at, score, then extra
// columns of fusion detail (v_rank, v_raw, k_rank, k_raw).
func scanHits(rows pgx.Rows, extra int) ([]search.Hit, error) {
	defer rows.Close()
	var out []search.Hit
	for rows.Next() {
		var h search.Hit
		var id string
		dest := []any{&id, &h.Title, &h.Team, &h.Status, &h.KB, &h.Plan, &h.CreatedAt, &h.Score}
		var vr, kr *int64
		var vraw, kraw *float64
		var vv, kv float64 // each side's contribution to the fused score
		if extra > 0 {
			dest = append(dest, &vr, &vraw, &kr, &kraw, &vv, &kv)
		}
		if err := rows.Scan(dest...); err != nil {
			return out, err
		}
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			h.ID = n
		} else {
			h.ID = id
		}
		if extra > 0 {
			if vr != nil {
				h.Parts = append(h.Parts, search.Part{Pipeline: "vector", Rank: int(*vr), Raw: deref(vraw), Value: vv})
			}
			if kr != nil {
				h.Parts = append(h.Parts, search.Part{Pipeline: "keyword", Rank: int(*kr), Raw: deref(kraw), Value: kv})
			}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func (s *pgSearcher) Vector(ctx context.Context, q search.VectorQuery) search.Result {
	if q.K <= 0 {
		q.K = 5
	}
	if q.NumCandidates <= 0 {
		q.NumCandidates = 40
	}
	sql, args := s.vectorSQLArgs(q)
	shown := pgShow(s.settings(q), sql, args)
	if !s.Ready() {
		t0 := time.Now()
		hits := s.b.mirror.Scan(q.Collection, q.Vector, q.K, q.Filter)
		for i := range hits {
			hits[i].Score = hits[i].Cosine
		}
		return search.Result{Hits: hits, Pipeline: shown, Engine: "app", Ms: float64(time.Since(t0).Microseconds()) / 1000,
			Note: "No pgvector: this is an exact cosine scan of every row in the app's memory — the SQL above is what would run with pgvector installed."}
	}
	hits, d, err := s.run(ctx, s.settings(q), sql, args, 0)
	r := search.Result{Pipeline: shown, Engine: "pgvector", Ms: float64(d.Microseconds()) / 1000}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	for i := range hits {
		hits[i].Cosine = hits[i].Score
	}
	r.Hits = hits
	return r
}

// tsq is the ORed full-text query for a piece of text.
const tsq = "to_tsquery('english', replace(plainto_tsquery('english', $1)::text, ' & ', ' | '))"

func (s *pgSearcher) keywordSQL(coll, query string, k int, resolvedOnly bool) (string, []any) {
	where := "WHERE tsv @@ q"
	if resolvedOnly {
		where += " AND status = 'resolved'"
	}
	return fmt.Sprintf(`SELECT %s, ts_rank_cd(tsv, q) AS rank
FROM %s, %s AS q
%s
ORDER BY rank DESC
LIMIT $2`, pgCols(coll), s.b.t(pgTable(coll)), tsq, where), []any{query, k}
}

func (s *pgSearcher) Keyword(ctx context.Context, coll, query string, k int, resolvedOnly bool) search.Result {
	if k <= 0 {
		k = 5
	}
	sql, args := s.keywordSQL(coll, query, k, resolvedOnly)
	hits, d, err := s.run(ctx, nil, sql, args, 0)
	r := search.Result{Hits: hits, Pipeline: pgShow(nil, sql, args), Engine: "postgres", Ms: float64(d.Microseconds()) / 1000}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// hybridSQL fuses the two rankings in one statement. Rank fusion: Σ weight / (60 +
// rank). Score fusion: each side's raw score normalized (sigmoid, min-max or not at
// all) and combined by the weights.
func (s *pgSearcher) hybridSQL(q search.VectorQuery, query string, resolvedOnly bool, o search.HybridOpts) (string, []any) {
	o = o.Normalize()
	t := s.b.t(pgTable(q.Collection))
	vf := search.Filter{Status: nil}
	if resolvedOnly {
		vf.Status = []string{"resolved"}
	}
	vwhere, _ := pgWhere(vf, 99)
	vwhere = strings.Replace(vwhere, "$99", "'resolved'", 1)
	kwhere := "WHERE tsv @@ q"
	if resolvedOnly {
		kwhere += " AND status = 'resolved'"
	}
	// The two halves of the fused score, kept apart so each hit can show what it got
	// from each side.
	var vpart, kpart string
	switch {
	case o.Method == "rank":
		vpart, kpart = "coalesce($3::float8 / (60 + v.rank), 0)", "coalesce($4::float8 / (60 + k.rank), 0)"
	case o.Normalization == "minMaxScaler":
		vpart, kpart = "coalesce($3::float8 * (v.raw - v.lo) / nullif(v.hi - v.lo, 0), 0)", "coalesce($4::float8 * (k.raw - k.lo) / nullif(k.hi - k.lo, 0), 0)"
	case o.Normalization == "none":
		vpart, kpart = "coalesce($3::float8 * v.raw, 0)", "coalesce($4::float8 * k.raw, 0)"
	default: // sigmoid
		vpart, kpart = "coalesce($3::float8 / (1 + exp(-v.raw)), 0)", "coalesce($4::float8 / (1 + exp(-k.raw)), 0)"
	}
	score := vpart + " + " + kpart
	sql := fmt.Sprintf(`WITH v AS (
  SELECT id, rank() OVER (ORDER BY d) AS rank, 1 - d AS raw, min(1 - d) OVER () AS lo, max(1 - d) OVER () AS hi
  FROM (SELECT %[1]s AS id, embedding <=> $1::vector AS d FROM %[2]s %[3]s ORDER BY embedding <=> $1::vector LIMIT 20) top
), k AS (
  SELECT id, rank() OVER (ORDER BY r DESC) AS rank, r AS raw, min(r) OVER () AS lo, max(r) OVER () AS hi
  FROM (SELECT %[1]s AS id, ts_rank_cd(tsv, q) AS r FROM %[2]s, %[4]s AS q %[5]s ORDER BY r DESC LIMIT 20) top
)
SELECT %[6]s, %[7]s AS score, v.rank, v.raw, k.rank, k.raw, %[8]s, %[9]s
FROM v FULL OUTER JOIN k USING (id) JOIN %[2]s t ON t.%[1]s = id
ORDER BY score DESC
LIMIT $5`, pgKey(q.Collection), t, vwhere, strings.Replace(tsq, "$1", "$2", 1), kwhere, pgColsT(q.Collection), score, vpart, kpart)
	return sql, []any{vecLit(q.Vector), query, o.VectorWeight, 1 - o.VectorWeight, q.K}
}

func pgKey(coll string) string {
	if coll == store.KB {
		return "slug"
	}
	return "id"
}

func pgColsT(coll string) string {
	if coll == store.KB {
		return "t.slug AS id, t.title, t.team, '' AS status, t.slug AS kb, '' AS plan, now() AS created_at"
	}
	return "t.id::text AS id, t.subject AS title, t.team, t.status, coalesce(t.resolved_kb, '') AS kb, t.plan, t.created_at"
}

func (s *pgSearcher) Hybrid(ctx context.Context, q search.VectorQuery, query string, resolvedOnly bool, o search.HybridOpts) search.Result {
	if q.K <= 0 {
		q.K = 5
	}
	sql, args := s.hybridSQL(q, query, resolvedOnly, o)
	sets := []string{"SET LOCAL hnsw.ef_search = 100"}
	if !s.Ready() {
		return search.Result{Pipeline: pgShow(sets, sql, args), Engine: "app", Error: "hybrid search needs pgvector in the database"}
	}
	hits, d, err := s.run(ctx, sets, sql, args, 1)
	for i := range hits {
		for j := range hits[i].Parts {
			if hits[i].Parts[j].Pipeline == "vector" {
				hits[i].Parts[j].Weight = o.Normalize().VectorWeight
			} else {
				hits[i].Parts[j].Weight = 1 - o.Normalize().VectorWeight
			}
		}
	}
	r := search.Result{Hits: hits, Pipeline: pgShow(sets, sql, args), Engine: "pgvector", Ms: float64(d.Microseconds()) / 1000}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}
