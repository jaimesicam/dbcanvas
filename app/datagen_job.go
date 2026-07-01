package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------- config + job registry

type dgColConfig struct {
	Name      string         `json:"name"`
	Generator string         `json:"generator"`
	Skip      bool           `json:"skip"`
	Options   map[string]any `json:"options"`
}

type dgGenConfig struct {
	Database     string        `json:"database"`
	Schema       string        `json:"schema"`
	Table        string        `json:"table"`
	Rows         int64         `json:"rows"`
	Batch        int           `json:"batch"`
	Threads      int           `json:"threads"`
	Seed         int64         `json:"seed"`
	StopOnError  bool          `json:"stopOnError"`
	FKSampleSize int           `json:"fkSampleSize"`
	Columns      []dgColConfig `json:"columns"`
}

type dgJob struct {
	ID       string    `json:"id"`
	StackID  int64     `json:"-"`
	Table    string    `json:"-"`
	Total    int64     `json:"total"`
	Inserted int64     `json:"inserted"`
	Errors   int64     `json:"errors"`
	Status   string    `json:"status"` // running | done | error | canceled
	Message  string    `json:"message"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	cancel   context.CancelFunc
}

var dgJobs = struct {
	sync.Mutex
	m map[string]*dgJob
}{m: map[string]*dgJob{}}

func newJobID() string {
	b := make([]byte, 8)
	crand.Read(b)
	return hex.EncodeToString(b)
}

// resolveColGens turns the request's column configs into ordered generators, keeping
// only the columns that will be inserted (skips: identity/generated/serial/default/skip).
func resolveColGens(meta dgTableMeta, cfg dgGenConfig) []colGen {
	byName := map[string]dgColConfig{}
	for _, c := range cfg.Columns {
		byName[c.Name] = c
	}
	var out []colGen
	for _, col := range meta.Columns {
		cc, ok := byName[col.Name]
		gen := col.Generator
		if ok && cc.Generator != "" {
			gen = cc.Generator
		}
		if (ok && cc.Skip) || gen == genSkip || gen == genDefault {
			continue
		}
		// Never insert into database-managed columns.
		if col.IsGenerated || col.IsIdentity || strings.Contains(col.Default, "nextval(") {
			continue
		}
		opts := map[string]any{}
		if ok {
			opts = cc.Options
		}
		out = append(out, colGen{col: col, gen: gen, opts: opts})
	}
	return out
}

// sampleFKs pre-queries each FK column's referenced table for ready-to-inject literals
// (quote_nullable → correct quoting for the ref column's type). Returns a warning string
// and whether a non-nullable FK has no rows (fatal).
func (a *App) sampleFKs(ctx context.Context, c dbConn, db string, gens []colGen, n int) (map[string][]string, string, bool) {
	if n <= 0 {
		n = 500
	}
	fk := map[string][]string{}
	var warn []string
	fatal := false
	for _, g := range gens {
		if g.gen != genFK || g.col.FK == nil {
			continue
		}
		ref := g.col.FK
		col := qIdent(c.Engine, ref.Column)
		var q string
		if c.Engine == "mysql" {
			q = fmt.Sprintf(`SELECT COALESCE(JSON_ARRAYAGG(q),JSON_ARRAY()) FROM (SELECT QUOTE(%s) AS q FROM %s.%s WHERE %s IS NOT NULL ORDER BY RAND() LIMIT %d) s`,
				col, qIdent(c.Engine, ref.Schema), qIdent(c.Engine, ref.Table), col, n)
		} else {
			q = fmt.Sprintf(`SELECT COALESCE(json_agg(q),'[]') FROM (SELECT quote_nullable(%s) AS q FROM %s.%s WHERE %s IS NOT NULL ORDER BY random() LIMIT %d) s`,
				col, qIdent(c.Engine, ref.Schema), qIdent(c.Engine, ref.Table), col, n)
		}
		var vals []string
		a.queryJSON(ctx, c, db, q, &vals)
		fk[g.col.Name] = vals
		if len(vals) == 0 {
			warn = append(warn, fmt.Sprintf("FK %s → %s.%s(%s) has no rows", g.col.Name, ref.Schema, ref.Table, ref.Column))
			if !g.col.Nullable {
				fatal = true
			}
		}
	}
	return fk, strings.Join(warn, "; "), fatal
}

// qIdent quotes an identifier: backticks for MySQL, double-quotes for PostgreSQL.
func qIdent(engine, s string) string {
	if engine == "mysql" {
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// ------------------------------------------------------------- preview

func (a *App) handleDataGenPreview(w http.ResponseWriter, r *http.Request) {
	c, ok := a.loadDGNode(w, r)
	if !ok {
		return
	}
	var cfg dgGenConfig
	if err := decode(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	meta, err := a.tableMeta(r.Context(), c, cfg.Database, cfg.Schema, cfg.Table)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	gens := resolveColGens(meta, cfg)
	seed := cfg.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	fk, _, _ := a.sampleFKs(r.Context(), c, cfg.Database, gens, 200)
	gc := &genCtx{rng: newRand(seed), fk: fk, engine: c.Engine, tsStart: time.Now().Add(-720 * time.Hour), tsStep: time.Minute}
	nrows := 10
	rows := make([]map[string]string, 0, nrows)
	for i := 0; i < nrows; i++ {
		gc.row = int64(i)
		row := map[string]string{}
		for _, g := range gens {
			row[g.col.Name] = displayLit(g.value(gc))
		}
		rows = append(rows, row)
	}
	var order []string
	for _, g := range gens {
		order = append(order, g.col.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": order, "rows": rows})
}

// displayLit turns a SQL literal into a friendlier preview value.
func displayLit(lit string) string {
	if lit == "NULL" || lit == "DEFAULT" {
		return lit
	}
	if i := strings.Index(lit, "'::"); i >= 0 { // strip a trailing cast
		lit = lit[:i+1]
	}
	if strings.HasPrefix(lit, "'") && strings.HasSuffix(lit, "'") {
		return strings.ReplaceAll(lit[1:len(lit)-1], "''", "'")
	}
	return lit
}

// ------------------------------------------------------------- generate

func (a *App) handleDataGenGenerate(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	c, ok := a.dbConnFor(st, r.PathValue("nid"))
	if !ok {
		writeErr(w, http.StatusConflict, "node is not running")
		return
	}
	var cfg dgGenConfig
	if err := decode(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if cfg.Rows <= 0 {
		cfg.Rows = 1000
	}
	if cfg.Batch <= 0 || cfg.Batch > 20000 {
		cfg.Batch = 1000
	}
	if cfg.Threads <= 0 || cfg.Threads > 16 {
		cfg.Threads = 4
	}
	meta, err := a.tableMeta(r.Context(), c, cfg.Database, cfg.Schema, cfg.Table)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	gens := resolveColGens(meta, cfg)
	if len(gens) == 0 {
		writeErr(w, http.StatusBadRequest, "no insertable columns selected")
		return
	}

	job := &dgJob{ID: newJobID(), StackID: st.ID, Table: cfg.Schema + "." + cfg.Table,
		Total: cfg.Rows, Status: "running", Start: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	dgJobs.Lock()
	dgJobs.m[job.ID] = job
	dgJobs.Unlock()

	go a.runGenJob(ctx, c, cfg, meta, gens, job)
	writeJSON(w, http.StatusOK, map[string]any{"jobId": job.ID})
}

func (a *App) runGenJob(ctx context.Context, c dbConn, cfg dgGenConfig, meta dgTableMeta, gens []colGen, job *dgJob) {
	defer func() {
		job.End = time.Now()
		if job.Status == "running" {
			job.Status = "done"
		}
		switch job.Status {
		case "done":
			a.notifyStack(job.StackID, "datagen.done", "success", "Data generation completed",
				fmt.Sprintf("Inserted %s rows into %s.", fmtInt(atomic.LoadInt64(&job.Inserted)), job.Table), "")
		case "error":
			a.notifyStack(job.StackID, "datagen.error", "error", "Data generation failed",
				job.Table+": "+job.Message, "")
		case "canceled":
			a.notifyStack(job.StackID, "datagen.canceled", "warning", "Data generation canceled",
				fmt.Sprintf("%s — inserted %s rows before cancel.", job.Table, fmtInt(atomic.LoadInt64(&job.Inserted))), "")
		}
	}()

	// FK sampling (fatal if a non-nullable FK has no referenced rows).
	fk, warn, fatal := a.sampleFKs(ctx, c, cfg.Database, gens, cfg.FKSampleSize)
	if warn != "" {
		job.Message = warn
	}
	if fatal {
		job.Status = "error"
		job.Message = "referenced table empty for a NOT NULL foreign key — " + warn
		return
	}

	tableRef := qIdent(c.Engine, cfg.Schema) + "." + qIdent(c.Engine, cfg.Table)
	colList := make([]string, len(gens))
	for i, g := range gens {
		colList[i] = qIdent(c.Engine, g.col.Name)
	}
	insertPrefix := "INSERT INTO " + tableRef + " (" + strings.Join(colList, ",") + ") VALUES "

	seed := cfg.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	// Hand out globally unique row-index ranges so sequential/unique generators never
	// overlap across workers. take() returns (count, startIndex).
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

	// Per-job nonce keeps unique-column values from colliding with other jobs' data.
	nonce := job.ID
	if len(nonce) > 6 {
		nonce = nonce[:6]
	}

	var wg sync.WaitGroup
	for wkr := 0; wkr < cfg.Threads; wkr++ {
		wg.Add(1)
		go func(wkr int) {
			defer wg.Done()
			gc := &genCtx{rng: newRand(seed + int64(wkr)*1_000_003), fk: fk, uniq: nonce, engine: c.Engine,
				tsStart: time.Now().Add(-720 * time.Hour), tsStep: time.Minute}
			for {
				if ctx.Err() != nil {
					return
				}
				n, start := take()
				if n <= 0 {
					return
				}
				var b strings.Builder
				b.WriteString(insertPrefix)
				for i := int64(0); i < n; i++ {
					gc.row = start + i
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteByte('(')
					for j, g := range gens {
						if j > 0 {
							b.WriteByte(',')
						}
						b.WriteString(g.value(gc))
					}
					b.WriteByte(')')
				}
				if err := a.execSQL(ctx, c, cfg.Database, b.String()); err != nil {
					atomic.AddInt64(&job.Errors, n)
					job.Message = truncErr(err.Error())
					if cfg.StopOnError {
						job.Status = "error"
						job.cancel()
						return
					}
					continue
				}
				atomic.AddInt64(&job.Inserted, n)
			}
		}(wkr)
	}
	wg.Wait()
}

// fmtInt formats an integer with thousands separators (fmt-only, no extra imports).
func fmtInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := ""
	if len(s) > 0 && s[0] == '-' {
		neg, s = "-", s[1:]
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return neg + string(out)
}

func truncErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// handleDataGenJob returns a running/finished job's progress snapshot.
func (a *App) handleDataGenJob(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	dgJobs.Lock()
	job := dgJobs.m[r.PathValue("job")]
	dgJobs.Unlock()
	if job == nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	inserted := atomic.LoadInt64(&job.Inserted)
	end := job.End
	if end.IsZero() {
		end = time.Now()
	}
	elapsed := end.Sub(job.Start).Seconds()
	rps := 0.0
	if elapsed > 0 {
		rps = float64(inserted) / elapsed
	}
	remain := 0.0
	if rps > 0 && job.Total > inserted {
		remain = float64(job.Total-inserted) / rps
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": job.ID, "status": job.Status, "total": job.Total,
		"inserted": inserted, "errors": atomic.LoadInt64(&job.Errors),
		"rowsPerSec": rps, "elapsedSec": elapsed, "etaSec": remain, "message": job.Message,
	})
}

// handleDataGenCancel cancels a running job.
func (a *App) handleDataGenCancel(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	dgJobs.Lock()
	job := dgJobs.m[r.PathValue("job")]
	dgJobs.Unlock()
	if job == nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if job.cancel != nil {
		job.cancel()
	}
	if job.Status == "running" {
		job.Status = "canceled"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": job.Status})
}

func newRand(seed int64) *mrand.Rand { return mrand.New(mrand.NewSource(seed)) }
