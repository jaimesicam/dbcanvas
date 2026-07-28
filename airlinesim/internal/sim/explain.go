package sim

import (
	"context"
	"database/sql"
	"log"
	"strconv"

	"airlinesim/internal/store"
)

// verifyExplain runs classic EXPLAIN over the just-executed query (go-sql-driver
// prepares EXPLAIN statements over the server's binary protocol the same as any
// other statement, so the ? placeholders and args work unchanged) and records a
// QuerySample carrying the real access type/index MySQL chose — the "verified"
// query-education sample, sampled at Profile.ExplainRate because explaining a query
// is itself extra server work and shouldn't distort the throughput being measured.
// This is Hotel Sim's shard-targeted-vs-scatter-gather explain panel, reframed
// around indexing: a targeted lookup should show type=range/ref against
// idx_fi_route_date; the deliberate region-only browse shows a much wider range (or
// a full scan) against idx_fi_region_date across every route in that region.
func (e *Engine) verifyExplain(ctx context.Context, query string, args []any, kind string, ms float64) {
	rows, err := e.Store.DB.QueryContext(ctx, "EXPLAIN "+query, args...)
	if err != nil {
		log.Printf("airlinesim: explain: %v", err)
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms})
		return
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || !rows.Next() {
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms})
		return
	}
	vals := make([]sql.NullString, len(cols))
	scanArgs := make([]any, len(cols))
	for i := range vals {
		scanArgs[i] = &vals[i]
	}
	if rows.Scan(scanArgs...) != nil {
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms})
		return
	}
	byName := map[string]string{}
	for i, c := range cols {
		if vals[i].Valid {
			byName[c] = vals[i].String
		}
	}
	var rowsExamined int64
	if r, ok := byName["rows"]; ok {
		if n, perr := strconv.ParseInt(r, 10, 64); perr == nil {
			rowsExamined = n
		}
	}
	indexUsed := byName["key"]
	if indexUsed == "" {
		indexUsed = "(none: " + byName["type"] + " scan)"
	}
	e.Store.RecordQuerySample(ctx, store.QuerySample{
		Kind: kind, SQLText: query, DurationMs: ms, RowsExamined: rowsExamined, IndexUsed: indexUsed,
	})
}
