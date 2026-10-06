package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"supportsim/internal/search"
)

// pg_workshop.go — the Index Workshop on PostgreSQL with pgvector.
//
// The choices are pgvector's own: which index (none, HNSW, IVFFlat), which distance
// operator and operator class, how the graph is built (m, ef_construction), how much
// it searches (hnsw.ef_search, ivfflat.probes), how the vector is stored (vector,
// halfvec, a binary-quantized expression index with a re-rank), and whether the
// vectors are normalized. Each variant gets its own table holding the same 2,000
// tickets, because the planner chooses among the indexes on a table by itself — one
// table per index is the only way to be sure which one a measurement measured.

// pgVariant is one way of indexing and querying the same vectors.
type pgVariant struct {
	Variant
	table  string // vv_<name>
	column string // what the table's embedding column holds (an expression over vv_base)
	ctype  string // its type
	index  string // the USING clause; "" = no index
	order  string // the ORDER BY distance expression ($1 = the query vector)
	score  string // what is reported as its score
	rerank bool   // binary quantization: candidates by Hamming distance, re-ranked by cosine
}

func (v pgVariant) ddl() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s.%s AS SELECT id, subject, team, resolved_kb, %s AS embedding FROM %s.vv_base;\n", pgSchema, v.table, v.column, pgSchema)
	if v.index == "" {
		b.WriteString("-- no index: every query computes every row's distance")
	} else {
		fmt.Fprintf(&b, "CREATE INDEX %s_idx ON %s.%s USING %s;", v.table, pgSchema, v.table, v.index)
	}
	return b.String()
}

var pgVariants = []pgVariant{
	{Variant: Variant{Name: "pg_exact", Path: "embedding vector(384)", Similarity: "<=> cosine distance", Storage: "vector, no index",
		Lesson: "No index: PostgreSQL computes every row's distance and sorts for the top k. This is the ground truth the other rows are measured against, and still a correct answer, just one that slows as the table grows."},
		table: "vv_exact", column: "embedding", ctype: "vector(384)", order: "embedding <=> $1::vector", score: "1 - (embedding <=> $1::vector)"},
	{Variant: Variant{Name: "pg_hnsw_cosine", Path: "embedding vector(384)", Similarity: "<=> · vector_cosine_ops", Storage: "vector · HNSW m=16, ef_construction=64",
		Lesson: "The default and usual choice: an HNSW graph with pgvector's defaults. hnsw.ef_search (default 40) is how many candidates the search keeps: more means better recall and more work. It must be at least your LIMIT."},
		table: "vv_hnsw_cosine", column: "embedding", ctype: "vector(384)", index: "hnsw (embedding vector_cosine_ops)", order: "embedding <=> $1::vector", score: "1 - (embedding <=> $1::vector)"},
	{Variant: Variant{Name: "pg_hnsw_l2", Path: "embedding vector(384)", Similarity: "<-> · vector_l2_ops", Storage: "vector · HNSW",
		Lesson: "Euclidean distance. On unit-length vectors it ranks exactly like cosine (|a−b|² = 2 − 2·cos), and the score is a distance, so lower is closer. The operator in the query must match the index's operator class, or the planner can't use the index."},
		table: "vv_hnsw_l2", column: "embedding", ctype: "vector(384)", index: "hnsw (embedding vector_l2_ops)", order: "embedding <-> $1::vector", score: "embedding <-> $1::vector"},
	{Variant: Variant{Name: "pg_hnsw_ip", Path: "embedding vector(384)", Similarity: "<#> · vector_ip_ops", Storage: "vector · HNSW",
		Lesson: "Inner product. `<#>` returns the *negative* inner product (so smaller sorts first). On normalized vectors it is the cosine and the fastest of the three."},
		table: "vv_hnsw_ip", column: "embedding", ctype: "vector(384)", index: "hnsw (embedding vector_ip_ops)", order: "embedding <#> $1::vector", score: "(embedding <#> $1::vector) * -1"},
	{Variant: Variant{Name: "pg_hnsw_m4", Path: "embedding vector(384)", Similarity: "<=> · vector_cosine_ops", Storage: "vector · HNSW m=4, ef_construction=16",
		Lesson: "A sparse graph (4 links per node) built cheaply: faster to build and smaller, at the cost of recall. m and ef_construction are build-time choices; ef_search is query time."},
		table: "vv_hnsw_m4", column: "embedding", ctype: "vector(384)", index: "hnsw (embedding vector_cosine_ops) WITH (m = 4, ef_construction = 16)", order: "embedding <=> $1::vector", score: "1 - (embedding <=> $1::vector)"},
	{Variant: Variant{Name: "pg_ivfflat", Path: "embedding vector(384)", Similarity: "<=> · vector_cosine_ops", Storage: "vector · IVFFlat lists=45",
		Lesson: "IVFFlat clusters the vectors into lists (√rows is a good start) and searches only ivfflat.probes of them (default 1). It builds fast and is small, but recall is poor until you raise probes. Build it after the data is loaded, since its clusters come from the rows present."},
		table: "vv_ivfflat", column: "embedding", ctype: "vector(384)", index: "ivfflat (embedding vector_cosine_ops) WITH (lists = 45)", order: "embedding <=> $1::vector", score: "1 - (embedding <=> $1::vector)"},
	{Variant: Variant{Name: "pg_halfvec", Path: "embedding halfvec(384)", Similarity: "<=> · halfvec_cosine_ops", Storage: "halfvec (2 bytes/dim) · HNSW",
		Lesson: "Half-precision floats: half the storage and half the index, with practically the same neighbours. Store halfvec, or index an expression `(embedding::halfvec(384))`."},
		table: "vv_halfvec", column: "embedding::halfvec(384)", ctype: "halfvec(384)", index: "hnsw (embedding halfvec_cosine_ops)", order: "embedding <=> $1::halfvec(384)", score: "1 - (embedding <=> $1::halfvec(384))"},
	{Variant: Variant{Name: "pg_binary", Path: "binary_quantize(embedding)", Quantization: "binary + re-rank", Similarity: "<~> Hamming, then <=>", Storage: "vector · HNSW on bit(384) expression",
		Lesson: "One bit per dimension, 32× smaller than float32: the index searches by Hamming distance, then the candidates are re-ranked by real cosine distance in an outer query (pgvector's documented pattern). Recall depends on how many candidates you re-rank."},
		table: "vv_binary", column: "embedding", ctype: "vector(384)", index: "hnsw ((binary_quantize(embedding)::bit(384)) bit_hamming_ops)", order: "binary_quantize(embedding)::bit(384) <~> binary_quantize($1::vector)", score: "1 - (embedding <=> $1::vector)", rerank: true},
	{Variant: Variant{Name: "pg_raw_ip", Path: "embedding vector(384), NOT normalized", Similarity: "<#> · vector_ip_ops", Storage: "vector · HNSW, random lengths",
		Lesson: "The same vectors given random lengths, searched by inner product: long vectors win regardless of meaning. Inner product (and L2) need normalized vectors; cosine does not care."},
		table: "vv_raw_ip", column: "embedding_raw", ctype: "vector(384)", index: "hnsw (embedding vector_ip_ops)", order: "embedding <#> $1::vector", score: "(embedding <#> $1::vector) * -1"},
}

type pgWorkshop struct {
	b  *pgBackend
	mu sync.Mutex
	// The playground's probe session settings.
	efSearch  int
	iterative string
	building  string // the async statement running, if any
	buildErr  string
}

func (w *pgWorkshop) Variants() []Variant {
	out := make([]Variant, len(pgVariants))
	for i, v := range pgVariants {
		out[i] = v.Variant
		out[i].ddl = v.ddl()
	}
	return out
}

func (w *pgWorkshop) t(name string) string { return pgSchema + "." + name }

// Detect reports a variant set built before a restart.
func (w *pgWorkshop) Detect(ctx context.Context) (bool, int) {
	var tables int
	w.b.pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = $1 AND tablename LIKE 'vv\_%'`, pgSchema).Scan(&tables)
	if tables < len(pgVariants)+2 { // + vv_base, vv_play
		return false, 0
	}
	// A playground table from an older build, without vip, is rebuilt.
	var vip bool
	w.b.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
WHERE table_schema = $1 AND table_name = 'vv_play' AND column_name = 'vip')`, pgSchema).Scan(&vip)
	if !vip {
		return false, 0
	}
	var n int
	w.b.pool.QueryRow(ctx, "SELECT count(*) FROM "+w.t("vv_base")).Scan(&n)
	return n > 0, n
}

// Build writes vv_base, then one table and one index per variant, timing each build.
func (w *pgWorkshop) Build(ctx context.Context, src []search.Doc, progress func(phase, detail string)) error {
	if !w.b.hasVector() {
		return fmt.Errorf("the workshop compares real pgvector indexes — install pgvector (tick it on the PostgreSQL node or cluster)")
	}
	ex := w.b.exec
	ex(ctx, "DROP TABLE IF EXISTS "+w.t("vv_meta"))
	for _, v := range pgVariants {
		ex(ctx, "DROP TABLE IF EXISTS "+w.t(v.table))
	}
	ex(ctx, "DROP TABLE IF EXISTS "+w.t("vv_play"))
	ex(ctx, "DROP TABLE IF EXISTS "+w.t("vv_base"))
	if err := ex(ctx, `CREATE TABLE `+w.t("vv_base")+` (id bigint PRIMARY KEY, subject text, team text, resolved_kb text,
  embedding vector(384) NOT NULL, embedding_raw vector(384) NOT NULL, raw_length real)`); err != nil {
		return err
	}
	ex(ctx, "CREATE TABLE "+w.t("vv_meta")+" (name text PRIMARY KEY, build_ms real)")
	progress("copying", fmt.Sprintf("writing %d resolved tickets", len(src)))
	for i := 0; i < len(src); i += 200 {
		batch := &pgx.Batch{}
		for _, d := range src[i:min(i+200, len(src))] {
			id, _ := d.ID.(int64)
			s := rawScale(id)
			raw := make([]float32, len(d.Vec))
			for j, x := range d.Vec {
				raw[j] = x * s
			}
			batch.Queue("INSERT INTO "+w.t("vv_base")+" VALUES ($1,$2,$3,$4,$5,$6,$7)", id, d.Title, d.Team, d.KB, vecLit(d.Vec), vecLit(raw), s)
		}
		if err := w.b.pool.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
	}
	// The playground's table: four copies of every row, so an index build lasts long
	// enough to watch. Each copy is nudged by a little noise: HNSW keeps identical
	// vectors in one graph element, so exact copies would hide what ef_search does.
	// vip marks 2% of rows — a selective filter for the iterative-scan lessons.
	if err := ex(ctx, `CREATE TABLE `+w.t("vv_play")+` AS SELECT b.id * 10 + g AS id, b.subject, b.team, b.resolved_kb,
  ((b.id * 4 + g) * 37) % 50 = 0 AS vip,
  (b.embedding + (SELECT array_agg((random() - 0.5) * 0.004) FROM generate_series(1, 384) WHERE b.id >= 0 AND g >= 0)::vector(384))::vector(384) AS embedding
FROM `+w.t("vv_base")+` b, generate_series(0, 3) g`); err != nil {
		return err
	}
	ex(ctx, "ALTER TABLE "+w.t("vv_play")+" ADD PRIMARY KEY (id)")
	conn, err := w.b.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// Session settings stay with a pooled connection after release, so put them back.
	defer conn.Exec(context.Background(), "RESET ALL")
	conn.Exec(ctx, "SET maintenance_work_mem = '256MB'")
	for i, v := range pgVariants {
		progress("indexing", fmt.Sprintf("building %d of %d: %s", i+1, len(pgVariants), v.Name))
		if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE TABLE %s AS SELECT id, subject, team, resolved_kb, %s AS embedding FROM %s",
			w.t(v.table), v.column, w.t("vv_base"))); err != nil {
			return fmt.Errorf("%s: %w", v.Name, err)
		}
		ms := 0.0
		if v.index != "" {
			t0 := time.Now()
			if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE INDEX %s_idx ON %s USING %s", v.table, w.t(v.table), v.index)); err != nil {
				return fmt.Errorf("index %s: %w", v.Name, err)
			}
			ms = msSince(t0)
		}
		conn.Exec(ctx, "ANALYZE "+w.t(v.table))
		conn.Exec(ctx, "INSERT INTO "+w.t("vv_meta")+" VALUES ($1, $2)", v.Name, ms)
	}
	// The playground starts with its index in place: lesson 1 drops it, and the tuning
	// lessons need one to tune.
	progress("indexing", "building the playground index")
	if _, err := conn.Exec(ctx, playIndexSQL(w.t("vv_play"))); err != nil {
		return fmt.Errorf("playground index: %w", err)
	}
	conn.Exec(ctx, "ANALYZE "+w.t("vv_play"))
	return nil
}

func playIndexSQL(table string) string {
	return "CREATE INDEX IF NOT EXISTS vv_play_hnsw ON " + table + " USING hnsw (embedding vector_cosine_ops)"
}

// ensurePlayIndex puts the playground index back when a tuning lesson runs after it was
// dropped: ef_search and iterative scans mean nothing to a sequential scan.
func (w *pgWorkshop) ensurePlayIndex(ctx context.Context) error {
	return w.b.exec(ctx, playIndexSQL(w.t("vv_play")))
}

func (w *pgWorkshop) query(v pgVariant, k, cands int) string {
	if v.rerank {
		return fmt.Sprintf(`SELECT id, resolved_kb, %s AS score FROM (
  SELECT * FROM %s ORDER BY %s LIMIT %d
) candidates ORDER BY embedding <=> $1::vector LIMIT %d`, v.score, w.t(v.table), v.order, cands, k)
	}
	return fmt.Sprintf("SELECT id, resolved_kb, %s AS score FROM %s ORDER BY %s LIMIT %d", v.score, w.t(v.table), v.order, k)
}

func (w *pgWorkshop) settings(p BenchParams) []string {
	return []string{"SET LOCAL hnsw.ef_search = " + strconv.Itoa(p.EfSearch), "SET LOCAL ivfflat.probes = " + strconv.Itoa(p.Probes)}
}

type pgRow struct {
	id    int64
	kb    string
	score float64
}

func (w *pgWorkshop) run(ctx context.Context, sets []string, sql string, vec []float32) ([]pgRow, time.Duration, error) {
	t0 := time.Now()
	tx, err := w.b.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(context.Background())
	for _, s := range sets {
		if _, err := tx.Exec(ctx, s); err != nil {
			return nil, 0, err
		}
	}
	rows, err := tx.Query(ctx, sql, vecLit(vec))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []pgRow
	for rows.Next() {
		var r pgRow
		var kb *string
		if rows.Scan(&r.id, &kb, &r.score) == nil {
			if kb != nil {
				r.kb = *kb
			}
			out = append(out, r)
		}
	}
	return out, time.Since(t0), rows.Err()
}

// Bench asks every variant the same questions, under the same ef_search and probes.
func (w *pgWorkshop) Bench(ctx context.Context, items []evalItem, p BenchParams) (BenchResult, error) {
	t0 := time.Now()
	out := BenchResult{Queries: len(items), K: p.K, NumCandidates: p.EfSearch, EfSearch: p.EfSearch, Probes: p.Probes, Example: items[0].t.Subject}
	sets := w.settings(p)
	truth := make([]map[int64]bool, len(items))
	for i, it := range items {
		rows, _, err := w.run(ctx, nil, w.query(pgVariants[0], p.K, p.K), it.vec)
		if err != nil {
			return out, fmt.Errorf("exact search: %w", err)
		}
		truth[i] = map[int64]bool{}
		for _, r := range rows {
			truth[i][r.id] = true
		}
	}
	meta := map[string]float64{}
	if rows, err := w.b.pool.Query(ctx, "SELECT name, build_ms FROM "+w.t("vv_meta")); err == nil {
		for rows.Next() {
			var n string
			var ms float64
			if rows.Scan(&n, &ms) == nil {
				meta[n] = ms
			}
		}
		rows.Close()
	}
	for _, v := range pgVariants {
		row := BenchRow{Variant: v.Variant, Shell: v.ddl(), BuildMs: meta[v.Name]}
		w.b.pool.QueryRow(ctx, "SELECT avg(pg_column_size(embedding))::float8 FROM "+w.t(v.table)).Scan(&row.FieldBytes)
		if v.index != "" {
			var sz int64
			w.b.pool.QueryRow(ctx, "SELECT pg_relation_size($1::regclass)", w.t(v.table+"_idx")).Scan(&sz)
			row.IndexBytes = float64(sz)
		}
		row.Query = pgShow(sets, w.query(v, p.K, max(p.EfSearch, p.K)), []any{vecLit(items[0].vec)})
		var lat []float64
		recall, right, lenSum, lenN := 0.0, 0, 0.0, 0
		for i, it := range items {
			rows, d, err := w.run(ctx, sets, w.query(v, p.K, max(p.EfSearch, p.K)), it.vec)
			if err != nil {
				row.Error = firstLineOf(err.Error())
				break
			}
			lat = append(lat, float64(d.Microseconds())/1000)
			got := 0
			votes := map[string]int{}
			for j, r := range rows {
				if truth[i][r.id] {
					got++
				}
				if j < 5 {
					votes[r.kb]++
				}
				if j == 0 && i == 0 {
					row.TopScore = r.score
				}
			}
			if v.Name == "pg_raw_ip" {
				for _, r := range rows {
					var l float64
					w.b.pool.QueryRow(ctx, "SELECT raw_length FROM "+w.t("vv_base")+" WHERE id = $1", r.id).Scan(&l)
					lenSum += l
					lenN++
				}
			}
			recall += float64(got) / float64(max(len(truth[i]), 1))
			best, bv := "", 0
			for kb, c := range votes {
				if c > bv {
					best, bv = kb, c
				}
			}
			if best == it.t.TruthKB {
				right++
			}
		}
		if row.Error == "" && len(lat) > 0 {
			row.Recall = recall / float64(len(items))
			row.Accuracy = float64(right) / float64(len(items))
			sort.Float64s(lat)
			row.P50Ms = lat[len(lat)/2]
			sum := 0.0
			for _, x := range lat {
				sum += x
			}
			row.MeanMs = sum / float64(len(lat))
			if lenN > 0 {
				row.MeanLength = lenSum / float64(lenN)
			}
		}
		out.Rows = append(out.Rows, row)
	}
	out.Seconds = time.Since(t0).Seconds()
	return out, nil
}

// ---------------------------------------------------------------- playground

var pgPlaygroundActions = []PlaygroundAction{
	{ID: "drop", Label: "1 · Drop the index",
		Shell: `DROP INDEX CONCURRENTLY IF EXISTS supportsim.vv_play_hnsw;`,
		Hint:  "Unlike a search index elsewhere, nothing breaks: the planner switches to a sequential scan plus a sort, and the results are still exactly right, just computed the slow way. Watch the plan column."},
	{ID: "create", Label: "2 · CREATE INDEX CONCURRENTLY",
		Shell: `SET maintenance_work_mem = '8MB';
CREATE INDEX CONCURRENTLY vv_play_hnsw ON supportsim.vv_play USING hnsw (embedding vector_cosine_ops);`,
		Hint: "Built without blocking writes. pg_stat_progress_create_index shows its phase while queries keep running on the sequential scan; the plan switches to the index the moment it is valid. maintenance_work_mem is kept small here so the build is slow enough to watch. Give it more and it is much faster."},
	{ID: "ef_small", Label: "3 · SET hnsw.ef_search = 5",
		Shell: `SET hnsw.iterative_scan = off;
SET hnsw.ef_search = 5;   -- the probes ask for LIMIT 10`,
		Hint: "A classic: an HNSW scan returns at most ef_search rows, so a LIMIT 10 query now returns 5, with no error. ef_search must be at least your LIMIT, unless iterative scans (pgvector 0.8+) are on to keep searching."},
	{ID: "iter_off", Label: "4 · Filter with iterative scans off",
		Shell: `SET hnsw.ef_search = 40;
SET hnsw.iterative_scan = off;   -- the filter probe: WHERE vip … LIMIT 10`,
		Hint: "The filter probe keeps only VIP tickets — 2% of the table. The index hands back its 40 nearest candidates and the WHERE is applied to those: one or two are VIP, so a LIMIT 10 query comes back short, with no error. Before pgvector 0.8 this was the only behaviour; the usual fix was a much larger ef_search, or a partial index per filter value."},
	{ID: "iter_on", Label: "5 · …and with iterative scans on",
		Shell: `SET hnsw.iterative_scan = relaxed_order;   -- pgvector 0.8+`,
		Hint:  "pgvector 0.8 keeps walking the graph until enough rows pass the filter (up to hnsw.max_scan_tuples). The filtered probe now returns its 10. relaxed_order may return them slightly out of order; strict_order does not."},
	{ID: "reindex", Label: "6 · REINDEX CONCURRENTLY",
		Shell: `REINDEX INDEX CONCURRENTLY supportsim.vv_play_hnsw;`,
		Hint:  "A fresh graph built beside the old one; queries keep using the old index until the swap. Useful after many updates and deletes, which leave the graph's dead entries for VACUUM."},
}

func (w *pgWorkshop) PlaygroundActions() []PlaygroundAction { return pgPlaygroundActions }

func (w *pgWorkshop) setProbe(ef int, it string) {
	w.mu.Lock()
	if ef > 0 {
		w.efSearch = ef
	}
	if it != "" {
		w.iterative = it
	}
	w.mu.Unlock()
}

// background runs a long statement (an index build) on its own connection, so the
// probes keep running and can watch it.
func (w *pgWorkshop) background(label string, stmts ...string) {
	w.mu.Lock()
	w.building, w.buildErr = label, ""
	w.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		conn, err := w.b.pool.Acquire(ctx)
		if err == nil {
			defer conn.Release()
			for _, s := range stmts {
				if _, err = conn.Exec(ctx, s); err != nil {
					break
				}
			}
			conn.Exec(ctx, "RESET ALL") // pooled connections keep session settings
		}
		w.mu.Lock()
		w.building = ""
		if err != nil {
			w.buildErr = causeOf(err.Error())
		}
		w.mu.Unlock()
	}()
}

func (w *pgWorkshop) PlaygroundDo(ctx context.Context, id string) error {
	if w.efSearch == 0 {
		w.setProbe(40, "relaxed_order")
	}
	switch id {
	case "drop":
		w.setProbe(40, "relaxed_order")
		return w.b.exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+w.t("vv_play_hnsw"))
	case "create":
		w.setProbe(40, "relaxed_order")
		w.b.exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+w.t("vv_play_hnsw"))
		w.background("CREATE INDEX CONCURRENTLY", "SET maintenance_work_mem = '8MB'", "SET max_parallel_maintenance_workers = 0",
			"CREATE INDEX CONCURRENTLY vv_play_hnsw ON "+w.t("vv_play")+" USING hnsw (embedding vector_cosine_ops)")
	case "ef_small", "iter_off", "iter_on", "reindex":
		w.mu.Lock()
		busy := w.building != ""
		w.mu.Unlock()
		if !busy {
			if err := w.ensurePlayIndex(ctx); err != nil {
				return err
			}
		}
	}
	switch id {
	case "drop", "create":
	case "ef_small":
		// Iterative scans would keep walking past ef_search; off, the cap shows.
		w.setProbe(5, "off")
	case "iter_off":
		w.setProbe(40, "off")
	case "iter_on":
		w.setProbe(40, "relaxed_order")
	case "reindex":
		w.background("REINDEX CONCURRENTLY", "SET maintenance_work_mem = '8MB'", "SET max_parallel_maintenance_workers = 0",
			"REINDEX INDEX CONCURRENTLY "+w.t("vv_play_hnsw"))
	default:
		return fmt.Errorf("unknown action %q", id)
	}
	return nil
}

// PlaygroundProbe reads the index's state (and any build's progress), the plan the
// probe query gets, and runs it plain and filtered.
func (w *pgWorkshop) PlaygroundProbe(ctx context.Context, vec []float32) PGEvent {
	w.mu.Lock()
	ef, it, building, berr := w.efSearch, w.iterative, w.building, w.buildErr
	w.mu.Unlock()
	if ef == 0 {
		ef, it = 40, "relaxed_order"
	}
	ev := PGEvent{Status: "DOES_NOT_EXIST"}
	var valid, ready bool
	if w.b.pool.QueryRow(ctx, `SELECT i.indisvalid, i.indisready FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = 'vv_play_hnsw'`, pgSchema).Scan(&valid, &ready) == nil {
		ev.Status, ev.Queryable = "INVALID", valid
		if valid {
			ev.Status = "VALID"
		}
	}
	var phase string
	var td, tt, bd, bt int64
	if w.b.pool.QueryRow(ctx, `SELECT phase, tuples_done, tuples_total, blocks_done, blocks_total FROM pg_stat_progress_create_index
WHERE relid = $1::regclass`, w.t("vv_play")).Scan(&phase, &td, &tt, &bd, &bt) == nil {
		ev.Status = "BUILDING"
		ev.Detail = phase
		if tt > 0 {
			ev.Detail += fmt.Sprintf(" · %d/%d tuples", td, tt)
		} else if bt > 0 {
			ev.Detail += fmt.Sprintf(" · %d/%d blocks", bd, bt)
		}
	} else if building != "" {
		ev.Detail = building + " running"
	}
	if berr != "" {
		ev.Detail = strings.TrimSpace(ev.Detail + " · build failed: " + berr)
	}
	ev.Detail = strings.TrimPrefix(strings.TrimSpace(ev.Detail+fmt.Sprintf(" · ef_search=%d, iterative_scan=%s", ef, it)), "· ")
	sets := []string{"SET LOCAL hnsw.ef_search = " + strconv.Itoa(ef), "SET LOCAL hnsw.iterative_scan = " + it}
	plain := "SELECT id, team, 1 - (embedding <=> $1::vector) FROM " + w.t("vv_play") + " ORDER BY embedding <=> $1::vector LIMIT 10"
	filtered := "SELECT id, team, 1 - (embedding <=> $1::vector) FROM " + w.t("vv_play") + " WHERE vip ORDER BY embedding <=> $1::vector LIMIT 10"
	ev.Version = w.planOf(ctx, sets, plain, vec)
	probe := func(sql string) string {
		t0 := time.Now()
		tx, err := w.b.pool.Begin(ctx)
		if err != nil {
			return "error: " + causeOf(err.Error())
		}
		defer tx.Rollback(context.Background())
		for _, s := range sets {
			tx.Exec(ctx, s)
		}
		rows, err := tx.Query(ctx, sql, vecLit(vec))
		if err != nil {
			return "error: " + shorten(causeOf(err.Error()), 140)
		}
		n, top := 0, 0.0
		for rows.Next() {
			var id int64
			var team string
			var sim float64
			if rows.Scan(&id, &team, &sim) == nil {
				if n == 0 {
					top = sim
				}
				n++
			}
		}
		rows.Close()
		if n == 0 {
			return "0 results"
		}
		return fmt.Sprintf("%d of 10 results · top similarity %.3f · %s", n, top, fmtMs(msSince(t0)))
	}
	ev.Plain, ev.Filter = probe(plain), probe(filtered)
	return ev
}

func fmtMs(ms float64) string {
	if ms < 10 {
		return fmt.Sprintf("%.1f ms", ms)
	}
	return fmt.Sprintf("%.0f ms", ms)
}

// planOf names the plan's access path: which index, or a sequential scan.
func (w *pgWorkshop) planOf(ctx context.Context, sets []string, sql string, vec []float32) string {
	tx, err := w.b.pool.Begin(ctx)
	if err != nil {
		return ""
	}
	defer tx.Rollback(context.Background())
	for _, s := range sets {
		tx.Exec(ctx, s)
	}
	var js string
	if tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, vecLit(vec)).Scan(&js) != nil {
		return ""
	}
	var plan []struct {
		Plan map[string]any `json:"Plan"`
	}
	if json.Unmarshal([]byte(js), &plan) != nil || len(plan) == 0 {
		return ""
	}
	var path []string
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		t, _ := n["Node Type"].(string)
		if ix, ok := n["Index Name"].(string); ok {
			t += " using " + ix
		}
		if t != "Limit" {
			path = append(path, t)
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	walk(plan[0].Plan)
	return strings.Join(path, " → ")
}

// ---------------------------------------------------------------- explain

// Explain runs EXPLAIN (ANALYZE, BUFFERS) on a vector query against a variant table
// or the desk's own tickets.
func (w *pgWorkshop) Explain(ctx context.Context, r ExplainRequest, vec []float32) ExplainResult {
	if r.K <= 0 || r.K > 50 {
		r.K = 5
	}
	ef := r.NumCandidates
	if ef < r.K {
		ef = 40
	}
	probes := r.Probes
	if probes <= 0 {
		probes = 1
	}
	var sql string
	table, order, score := w.t("tickets"), "embedding <=> $1::vector", "1 - (embedding <=> $1::vector)"
	var rerank *pgVariant
	if r.Index != "" && r.Index != "tickets_vector" {
		for i := range pgVariants {
			if pgVariants[i].Name == r.Index {
				v := pgVariants[i]
				table, order, score = w.t(v.table), v.order, v.score
				if v.rerank {
					rerank = &v
				}
			}
		}
	}
	where := ""
	if r.Team != "" {
		where = "WHERE team = '" + strings.ReplaceAll(r.Team, "'", "''") + "' "
	}
	if rerank != nil {
		sql = fmt.Sprintf("SELECT id, %s AS score FROM (SELECT * FROM %s %sORDER BY %s LIMIT %d) c ORDER BY embedding <=> $1::vector LIMIT %d",
			score, table, where, order, max(ef, r.K), r.K)
	} else {
		sql = fmt.Sprintf("SELECT id, %s AS score FROM %s %sORDER BY %s LIMIT %d", score, table, where, order, r.K)
	}
	sets := []string{"SET LOCAL hnsw.ef_search = " + strconv.Itoa(ef), "SET LOCAL ivfflat.probes = " + strconv.Itoa(probes)}
	if r.Team != "" {
		sets = append(sets, "SET LOCAL hnsw.iterative_scan = relaxed_order")
	}
	if r.Exact {
		sets = append(sets, "SET LOCAL enable_indexscan = off", "SET LOCAL enable_bitmapscan = off")
	}
	out := ExplainResult{Pipeline: pgShow(sets, "EXPLAIN (ANALYZE, BUFFERS)\n"+sql, []any{vecLit(vec)})}
	tx, err := w.b.pool.Begin(ctx)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer tx.Rollback(context.Background())
	for _, s := range sets {
		tx.Exec(ctx, s)
	}
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, vecLit(vec))
	if err != nil {
		out.Error = err.Error()
		return out
	}
	var lines []string
	for rows.Next() {
		var l string
		if rows.Scan(&l) == nil {
			lines = append(lines, l)
		}
	}
	rows.Close()
	out.Plan = vecLitRE.ReplaceAllStringFunc(strings.Join(lines, "\n"), abbrevVec)
	out.Raw = out.Plan
	var js string
	if tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, vecLit(vec)).Scan(&js) == nil {
		var plan []struct {
			Plan          map[string]any `json:"Plan"`
			ExecutionTime float64        `json:"Execution Time"`
			PlanningTime  float64        `json:"Planning Time"`
		}
		if json.Unmarshal([]byte(js), &plan) == nil && len(plan) > 0 {
			out.QueryMs = plan[0].ExecutionTime
			var access []string
			hit, read := 0.0, 0.0
			var walk func(n map[string]any)
			walk = func(n map[string]any) {
				t, _ := n["Node Type"].(string)
				if ix, ok := n["Index Name"].(string); ok {
					t += " using " + ix
					out.QueryType = t
				} else if t == "Seq Scan" && out.QueryType == "" {
					out.QueryType = "Seq Scan (no index used)"
				}
				if strings.Contains(t, "Scan") {
					access = append(access, fmt.Sprintf("%s: %v rows", t, n["Actual Rows"]))
				}
				hit += toF(n["Shared Hit Blocks"])
				read += toF(n["Shared Read Blocks"])
				if kids, ok := n["Plans"].([]any); ok {
					for _, k := range kids {
						if m, ok := k.(map[string]any); ok {
							walk(m)
						}
					}
				}
			}
			walk(plan[0].Plan)
			var total int64
			w.b.pool.QueryRow(ctx, "SELECT reltuples::bigint FROM pg_class WHERE oid = $1::regclass", table).Scan(&total)
			out.TotalDocs = float64(total)
			out.Summary = []KV{
				{"access path", strings.Join(access, " · ")},
				{"execution", fmt.Sprintf("%.2f ms (planning %.2f ms)", plan[0].ExecutionTime, plan[0].PlanningTime)},
				{"buffers", fmt.Sprintf("%.0f hit in shared_buffers · %.0f read", hit, read)},
				{"settings", strings.Join(sets, "; ")},
			}
		}
	}
	return out
}

// vecLitRE finds a vector literal in a plan; abbrevVec shortens it the way the
// pipelines do, so a plan stays readable.
var vecLitRE = regexp.MustCompile(`'\[[-0-9.e,]{200,}\]'`)

func abbrevVec(s string) string {
	parts := strings.SplitN(s[2:], ",", 5)
	return "'[" + strings.Join(parts[:4], ", ") + ", … " + strconv.Itoa(strings.Count(s, ",")+1) + " numbers]'"
}

// ---------------------------------------------------------------- metrics

// PGMetrics is the Workshop's live panel on PostgreSQL: the server's vector-related
// settings, every index in the schema with its use and cache behaviour, and — when
// pg_stat_statements is installed — the vector queries the database actually ran.
type PGMetrics struct {
	Version    string    `json:"version"`
	Vector     string    `json:"vector"`
	Settings   []KV      `json:"settings"`
	Indexes    []PGIndex `json:"indexes"`
	Statements []PGStmt  `json:"statements"`
	StmtsNote  string    `json:"stmtsNote,omitempty"`
}

// PGIndex is one index's numbers.
type PGIndex struct {
	Table   string  `json:"table"`
	Name    string  `json:"name"`
	Method  string  `json:"method"`
	Bytes   int64   `json:"bytes"`
	Scans   int64   `json:"scans"`
	Tuples  int64   `json:"tuples"`
	HitPct  float64 `json:"hitPct"` // share of index block reads served from shared_buffers
	Def     string  `json:"def"`
	IsValid bool    `json:"valid"`
}

// PGStmt is one statement from pg_stat_statements.
type PGStmt struct {
	Query  string  `json:"query"`
	Calls  int64   `json:"calls"`
	MeanMs float64 `json:"meanMs"`
	Rows   int64   `json:"rows"`
}

func (w *pgWorkshop) Metrics(ctx context.Context) MetricsView {
	m := &PGMetrics{Vector: w.b.vectorVersion()}
	conn, err := w.b.pool.Acquire(ctx)
	if err != nil {
		return MetricsView{PG: m}
	}
	defer conn.Release()
	conn.QueryRow(ctx, "SELECT current_setting('server_version')").Scan(&m.Version)
	if w.b.hasVector() {
		// pgvector's settings are registered when the library loads in a session.
		conn.Exec(ctx, "SELECT '[1]'::vector")
	}
	rows, err := conn.Query(ctx, `SELECT name, setting || coalesce(' ' || unit, '') FROM pg_settings
WHERE name IN ('shared_buffers','effective_cache_size','work_mem','maintenance_work_mem','max_parallel_maintenance_workers',
  'hnsw.ef_search','hnsw.iterative_scan','hnsw.max_scan_tuples','ivfflat.probes','ivfflat.iterative_scan')
ORDER BY name`)
	if err == nil {
		for rows.Next() {
			var kv KV
			if rows.Scan(&kv.K, &kv.V) == nil {
				m.Settings = append(m.Settings, kv)
			}
		}
		rows.Close()
	}
	rows, err = conn.Query(ctx, `SELECT s.relname, s.indexrelname, am.amname, pg_relation_size(s.indexrelid), s.idx_scan, s.idx_tup_read,
  coalesce(round(100.0 * io.idx_blks_hit / nullif(io.idx_blks_hit + io.idx_blks_read, 0), 1), 0)::float8,
  pg_get_indexdef(s.indexrelid), i.indisvalid
FROM pg_stat_user_indexes s
JOIN pg_statio_user_indexes io ON io.indexrelid = s.indexrelid
JOIN pg_index i ON i.indexrelid = s.indexrelid
JOIN pg_class c ON c.oid = s.indexrelid JOIN pg_am am ON am.oid = c.relam
WHERE s.schemaname = $1 AND am.amname IN ('hnsw', 'ivfflat', 'gin')
ORDER BY s.relname, s.indexrelname`, pgSchema)
	if err == nil {
		for rows.Next() {
			var x PGIndex
			if rows.Scan(&x.Table, &x.Name, &x.Method, &x.Bytes, &x.Scans, &x.Tuples, &x.HitPct, &x.Def, &x.IsValid) == nil {
				m.Indexes = append(m.Indexes, x)
			}
		}
		rows.Close()
	}
	var has bool
	conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')").Scan(&has)
	if !has {
		m.StmtsNote = "pg_stat_statements is not installed in this database, so per-query statistics are not available. Turn on query analytics (pg_stat_statements) on the PostgreSQL node to see them here."
	} else {
		rows, err = conn.Query(ctx, `SELECT query, calls, mean_exec_time, rows FROM pg_stat_statements
WHERE query ~ '(<=>|<->|<#>|<~>|tsv @@)' AND query NOT LIKE 'EXPLAIN%'
ORDER BY total_exec_time DESC LIMIT 8`)
		if err != nil {
			m.StmtsNote = "pg_stat_statements: " + causeOf(err.Error())
		} else {
			for rows.Next() {
				var s PGStmt
				if rows.Scan(&s.Query, &s.Calls, &s.MeanMs, &s.Rows) == nil {
					m.Statements = append(m.Statements, s)
				}
			}
			rows.Close()
		}
	}
	return MetricsView{PG: m}
}
